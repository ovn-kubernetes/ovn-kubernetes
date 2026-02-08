//go:build linux
// +build linux

package managementport

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// The legacy iptables management port chain
	iptableMgmPortChain = "OVN-KUBE-SNAT-MGMTPORT"

	// The "mgmtport-snat" chain contains the rules to SNAT traffic sent to the
	// management port (except for `externalTrafficPolicy: Local` traffic, where
	// the source IP must be preserved).
	nftMgmtPortChain = "mgmtport-snat"

	// ovnkubeSvcViaMgmPortRT is the number of the custom routing table used to steer host->service
	// traffic packets into OVN via ovn-k8s-mp0. Currently only used for ITP=local traffic.
	ovnkubeSvcViaMgmPortRT = "7"

	ovsPort         = "ovs"
	netdevPort      = "netdev"
	representorPort = "representor"
)

type managementPortController struct {
	ports map[string]managementPort
	cfg   *managementPortConfig

	nodeName string

	reconcile func()
}

// NewManagementPortController creates a new ManagementPorts
func NewManagementPortController(
	node *corev1.Node,
	hostSubnets []*net.IPNet,
	netdevDevName string,
	repDevName string,
	routeManager *routemanager.Controller,
	netInfo util.NetInfo,
) (Controller, error) {
	cfg, err := newManagementPortConfig(node, hostSubnets, netInfo)
	if err != nil {
		return nil, err
	}

	c := &managementPortController{
		cfg:       cfg,
		nodeName:  node.Name,
		ports:     map[string]managementPort{},
		reconcile: func() {},
	}

	var hasOVS, hasNetdev, hasRepresentor bool
	switch {
	case config.OvnKubeNode.Mode == types.NodeModeDPU:
		hasRepresentor = true
	case config.OvnKubeNode.Mode == types.NodeModeDPUHost:
		hasNetdev = true
	case config.OvnKubeNode.MgmtPortNetdev != "":
		hasRepresentor = true
		hasNetdev = true
	default:
		hasOVS = true
	}

	if hasOVS {
		c.ports[ovsPort] = newManagementPortOVS(cfg, routeManager)
	}
	if hasNetdev {
		// Get device ID for fallback lookup after DPU reboot.
		deviceID, err := util.GetDeviceIDFromNetdevice(netdevDevName)
		if err != nil {
			return nil, fmt.Errorf("failed to get device ID for %s: %v", netdevDevName, err)
		}
		klog.Infof("Management port device ID: %s (from device %s)", deviceID, netdevDevName)
		c.ports[netdevPort] = newManagementPortNetdev(netdevDevName, deviceID, cfg, routeManager)
	}
	if hasRepresentor {
		ifName := types.K8sMgmtIntfName
		if hasNetdev {
			ifName += "_0"
		}
		c.ports[representorPort] = newManagementPortRepresentor(ifName, repDevName, cfg)
	}

	// setup NFT sets early as gateway initialization depends on it
	err = SetupManagementPortNFTSets()
	if err != nil {
		return nil, err
	}

	return c, nil
}

// GetInterfaceName of the management port
func (c *managementPortController) GetInterfaceName() string {
	if c.ports[representorPort] != nil && c.ports[netdevPort] != nil {
		return types.K8sMgmtIntfName + "_0"
	}
	return types.K8sMgmtIntfName
}

func (c *managementPortController) start(stopChan <-chan struct{}) error {
	// the differenet management port devices are aggregated in 'c.ports' map;
	// start them all and aggregate the respective reconciliations in a single
	// function for later use
	var reconciles []func()
	for _, port := range c.ports {
		reconcile, err := start(port, stopChan)
		if err != nil {
			return err
		}
		reconciles = append(reconciles, reconcile)
	}
	c.reconcile = func() {
		for _, reconcile := range reconciles {
			reconcile()
		}
	}

	if config.Gateway.NodeportEnable {
		if config.OvnKubeNode.Mode == types.NodeModeFull {
			// (TODO): Internal Traffic Policy is not supported in DPU mode
			if err := initMgmPortRoutingRules(c.cfg); err != nil {
				return err
			}
		}
	}

	return nil
}

type managementPortOVS struct {
	cfg          *managementPortConfig
	routeManager *routemanager.Controller
}

// newManagementPort creates a new newManagementPort
func newManagementPortOVS(cfg *managementPortConfig, routeManager *routemanager.Controller) *managementPortOVS {
	return &managementPortOVS{
		cfg:          cfg,
		routeManager: routeManager,
	}
}

func (mp *managementPortOVS) create() error {
	for _, mgmtPortName := range []string{types.K8sMgmtIntfName, types.K8sMgmtIntfName + "_0"} {
		if err := syncMgmtPortInterface(mgmtPortName, true); err != nil {
			return fmt.Errorf("failed to sync management port: %v", err)
		}
	}

	// Create a OVS internal interface.
	legacyMgmtIntfName := util.GetLegacyK8sMgmtIntfName(mp.cfg.nodeName)
	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--if-exists", "del-port", "br-int", legacyMgmtIntfName,
		"--", "--may-exist", "add-port", "br-int", types.K8sMgmtIntfName,
		"--", "set", "interface", types.K8sMgmtIntfName, fmt.Sprintf("mac=\"%s\"", mp.cfg.mpMAC.String()),
		"type=internal", "mtu_request="+fmt.Sprintf("%d", config.Default.MTU),
		"external-ids:iface-id="+types.K8sPrefix+mp.cfg.nodeName)
	if err != nil {
		return fmt.Errorf("failed to add port to br-int: stdout %q, stderr %q, error: %w", stdout, stderr, err)
	}

	return createPlatformManagementPort(types.K8sMgmtIntfName, mp.cfg, mp.routeManager)
}

func (mp *managementPortOVS) reconcilePeriod() time.Duration {
	return 30 * time.Second
}

func (mp *managementPortOVS) doReconcile() error {
	return createPlatformManagementPort(types.K8sMgmtIntfName, mp.cfg, mp.routeManager)
}

func tearDownManagementPortConfig(link netlink.Link) error {
	if err := util.LinkAddrFlush(link); err != nil {
		return err
	}

	if err := util.LinkRoutesDel(link, nil); err != nil {
		return err
	}

	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		return nil
	}
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables: %v", err)
	}

	tx := nft.NewTransaction()
	// Delete would return an error if we tried to delete a chain that didn't exist, so
	// we do an Add first (which is a no-op if the chain already exists) and then Delete.
	tx.Add(&knftables.Chain{
		Name: nftMgmtPortChain,
	})
	tx.Delete(&knftables.Chain{
		Name: nftMgmtPortChain,
	})
	err = nft.Run(context.TODO(), tx)
	if err != nil && !knftables.IsNotFound(err) {
		return fmt.Errorf("could not clear the nftables chain for management port: %v", err)
	}

	return nil
}

func setupManagementPortIPFamilyConfig(link netlink.Link, mpcfg *managementPortConfig, cfg *managementPortIPFamilyConfig, routeManager *routemanager.Controller) error {
	var err error
	var exists bool

	ifName := link.Attrs().Name

	// synchronize IP addresses, removing undesired addresses
	// should also remove routes specifying those undesired addresses
	err = util.SyncAddresses(link, []*net.IPNet{cfg.ifAddr})
	if err != nil {
		return err
	}

	// now check for addition of any missing routes
	for _, subnet := range cfg.clusterSubnets {
		route, err := util.LinkRouteGetByDstAndGw(link, cfg.gwIP, subnet)
		if err != nil || route == nil {
			// we need to warn so that it can be debugged as to why routes are incorrect
			klog.Warningf("Missing or unable to find route entry for subnet %s via gateway %s on link %v with MTU: %d", subnet, cfg.gwIP, ifName, config.Default.RoutableMTU)
		}

		subnetCopy := *subnet
		err = routeManager.Add(netlink.Route{LinkIndex: link.Attrs().Index, Gw: cfg.gwIP, Dst: &subnetCopy, MTU: config.Default.RoutableMTU})
		if err != nil {
			klog.Warningf("Could not add route entry for subnet %s via gateway %s: %v", subnet, cfg.gwIP, err)
		}
	}

	// Add a neighbour entry on the K8s node to map routerIP with routerMAC. This is
	// required because in certain cases ARP requests from the K8s Node to the routerIP
	// arrives on OVN Logical Router pipeline with ARP source protocol address set to
	// K8s Node IP. OVN Logical Router pipeline drops such packets since it expects
	// source protocol address to be in the Logical Switch's subnet.
	if exists, err = util.LinkNeighExists(link, cfg.gwIP, mpcfg.gwMAC); err == nil && !exists {
		klog.Warningf("Missing arp entry for MAC/IP binding (%s/%s) on link %s", mpcfg.gwMAC.String(), cfg.gwIP, types.K8sMgmtIntfName)
		// LinkNeighExists checks if the mac also matches, but it is possible there is a stale entry
		// still in the neighbor cache which would prevent add. Therefore execute a delete first if an IP entry exists.
		if exists, err = util.LinkNeighIPExists(link, cfg.gwIP); err != nil {
			klog.Warningf("Could not detect if stale IP neighbor entry exists for IP %s, on iface %s: %v", cfg.gwIP.String(), types.K8sMgmtIntfName, err)
		} else if exists {
			klog.Warningf("Found stale neighbor entry IP binding (%s) on link %s", cfg.gwIP.String(), types.K8sMgmtIntfName)
			if err = util.LinkNeighDel(link, cfg.gwIP); err != nil {
				klog.Warningf("Could not remove remove stale IP neighbor entry for IP %s, on iface %s: %v", cfg.gwIP.String(), types.K8sMgmtIntfName, err)
			}
		}
		err = util.LinkNeighAdd(link, cfg.gwIP, mpcfg.gwMAC)
	}
	if err != nil {
		return err
	}

	protocol := iptables.ProtocolIPv4
	if mpcfg.ipv6 != nil && cfg == mpcfg.ipv6 {
		protocol = iptables.ProtocolIPv6
	}

	// IPv6 forwarding is enabled globally
	if protocol == iptables.ProtocolIPv4 {
		err := util.SetforwardingModeForInterface(types.K8sMgmtIntfName)
		if err != nil {
			klog.Warning(err)
		}
	}

	return nil
}

func setupManagementPortConfig(link netlink.Link, cfg *managementPortConfig, routeManager *routemanager.Controller) error {
	var err error

	if cfg.ipv4 != nil {
		err = setupManagementPortIPFamilyConfig(link, cfg, cfg.ipv4, routeManager)
	}
	if cfg.ipv6 != nil && err == nil {
		err = setupManagementPortIPFamilyConfig(link, cfg, cfg.ipv6, routeManager)
	}

	return err
}

// setupManagementPortNFTSets sets up the NFT sets that the management port SNAR
// rules rely on. These sets are written to by other componets so they are setup
// independantly and as early as possible.
func SetupManagementPortNFTSets() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()
	tx.Add(&knftables.Set{
		Name:    types.NFTMgmtPortNoSNATNodePorts,
		Comment: knftables.PtrTo("NodePorts not subject to management port SNAT"),
		Type:    "inet_proto . inet_service",
	})
	tx.Add(&knftables.Set{
		Name:    types.NFTMgmtPortNoSNATServicesV4,
		Comment: knftables.PtrTo("eTP:Local short-circuit not subject to management port SNAT (IPv4)"),
		Type:    "ipv4_addr . inet_proto . inet_service",
	})
	tx.Add(&knftables.Set{
		Name:    types.NFTMgmtPortNoSNATServicesV6,
		Comment: knftables.PtrTo("eTP:Local short-circuit not subject to management port SNAT (IPv6)"),
		Type:    "ipv6_addr . inet_proto . inet_service",
	})
	tx.Add(&knftables.Set{
		Name:    types.NFTMgmtPortNoSNATSubnetsV4,
		Comment: knftables.PtrTo("subnets not subject to management port SNAT (IPv4)"),
		Type:    "ipv4_addr",
		Flags:   []knftables.SetFlag{knftables.IntervalFlag},
	})
	tx.Add(&knftables.Set{
		Name:    types.NFTMgmtPortNoSNATSubnetsV6,
		Comment: knftables.PtrTo("subnets not subject to management port SNAT (IPv6)"),
		Type:    "ipv6_addr",
		Flags:   []knftables.SetFlag{knftables.IntervalFlag},
	})

	err = nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not add nftables sets for management port: %v", err)
	}
	return nil
}

// setupManagementPortNFTChain sets up the management port SNAT chain and rules.
// Relies on the sets from setupManagementPortNFTSets.
func setupManagementPortNFTChain(interfaceName string, cfg *managementPortConfig) error {
	counterIfDebug := ""
	if config.Logging.Level > 4 {
		counterIfDebug = "counter"
	}

	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()
	tx.Add(&knftables.Chain{
		Name:     nftMgmtPortChain,
		Comment:  knftables.PtrTo("OVN SNAT to Management Port"),
		Type:     knftables.PtrTo(knftables.NATType),
		Hook:     knftables.PtrTo(knftables.PostroutingHook),
		Priority: knftables.PtrTo(knftables.SNATPriority),
	})

	tx.Flush(&knftables.Chain{
		Name: nftMgmtPortChain,
	})
	tx.Add(&knftables.Rule{
		Chain: nftMgmtPortChain,
		Rule: knftables.Concat(
			"oifname", "!=", interfaceName,
			"return",
		),
	})
	tx.Add(&knftables.Rule{
		Chain: nftMgmtPortChain,
		Rule: knftables.Concat(
			"meta l4proto", ".", "th dport", "@", types.NFTMgmtPortNoSNATNodePorts,
			counterIfDebug,
			"return",
		),
	})

	isPodNetworkAdvertised := util.IsPodNetworkAdvertisedAtNode(cfg.netInfo, cfg.nodeName)

	if cfg.ipv4 != nil {
		if isPodNetworkAdvertised {
			tx.Add(&knftables.Rule{
				Chain: nftMgmtPortChain,
				Rule: knftables.Concat(
					"meta nfproto ipv4",
					"fib saddr type != local",
					counterIfDebug,
					"return",
				),
			})
		}
		// don't SNAT if the source IP is already the Mgmt port IP
		tx.Add(&knftables.Rule{
			Chain: nftMgmtPortChain,
			Rule: knftables.Concat(
				"meta nfproto ipv4",
				"ip saddr", cfg.ipv4.ifAddr.IP,
				counterIfDebug,
				"return",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: nftMgmtPortChain,
			Rule: knftables.Concat(
				"ip daddr . meta l4proto . th dport", "@", types.NFTMgmtPortNoSNATServicesV4,
				counterIfDebug,
				"return",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: nftMgmtPortChain,
			Rule: knftables.Concat(
				"ip saddr", "@", types.NFTMgmtPortNoSNATSubnetsV4,
				counterIfDebug,
				"return",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: nftMgmtPortChain,
			Rule: knftables.Concat(
				counterIfDebug,
				"snat ip to", cfg.ipv4.ifAddr.IP,
			),
		})
	}

	if cfg.ipv6 != nil {
		if isPodNetworkAdvertised {
			tx.Add(&knftables.Rule{
				Chain: nftMgmtPortChain,
				Rule: knftables.Concat(
					"meta nfproto ipv6",
					"fib saddr type != local",
					counterIfDebug,
					"return",
				),
			})
		}
		// don't SNAT if the source IP is already the Mgmt port IP
		tx.Add(&knftables.Rule{
			Chain: nftMgmtPortChain,
			Rule: knftables.Concat(
				"meta nfproto ipv6",
				"ip6 saddr", cfg.ipv6.ifAddr.IP,
				counterIfDebug,
				"return",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: nftMgmtPortChain,
			Rule: knftables.Concat(
				"ip6 daddr . meta l4proto . th dport", "@", types.NFTMgmtPortNoSNATServicesV6,
				counterIfDebug,
				"return",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: nftMgmtPortChain,
			Rule: knftables.Concat(
				"ip6 saddr", "@", types.NFTMgmtPortNoSNATSubnetsV6,
				counterIfDebug,
				"return",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: nftMgmtPortChain,
			Rule: knftables.Concat(
				counterIfDebug,
				"snat ip6 to", cfg.ipv6.ifAddr.IP,
			),
		})
	}

	err = nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not update nftables rule for management port: %v", err)
	}
	return nil
}

func UpdateNoSNATSubnetsSets(node *corev1.Node, getSubnetsFn func(*corev1.Node) ([]string, error)) error {
	subnetsList, err := getSubnetsFn(node)
	if err != nil {
		return fmt.Errorf("error retrieving subnets list: %w", err)
	}

	subNetV4 := make([]*knftables.Element, 0)
	subNetV6 := make([]*knftables.Element, 0)

	for _, subnet := range subnetsList {
		if utilnet.IPFamilyOfCIDRString(subnet) == utilnet.IPv4 {
			subNetV4 = append(subNetV4,
				&knftables.Element{
					Set: types.NFTMgmtPortNoSNATSubnetsV4,
					Key: []string{subnet},
				},
			)
		}
		if utilnet.IPFamilyOfCIDRString(subnet) == utilnet.IPv6 {
			subNetV6 = append(subNetV6,
				&knftables.Element{
					Set: types.NFTMgmtPortNoSNATSubnetsV6,
					Key: []string{subnet},
				},
			)
		}

	}
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables: %v", err)
	}

	tx := nft.NewTransaction()
	tx.Flush(&knftables.Set{
		Name: types.NFTMgmtPortNoSNATSubnetsV4,
	})
	tx.Flush(&knftables.Set{
		Name: types.NFTMgmtPortNoSNATSubnetsV6,
	})

	for _, elem := range subNetV4 {
		tx.Add(elem)
	}
	for _, elem := range subNetV6 {
		tx.Add(elem)
	}

	return nft.Run(context.TODO(), tx)
}

// createPlatformManagementPort creates a management port attached to the node switch
// that lets the node access its pods via their private IP address. This is used
// for health checking and other management tasks.
func createPlatformManagementPort(interfaceName string, cfg *managementPortConfig, routeManager *routemanager.Controller) error {
	var link netlink.Link
	link, err := util.LinkSetUp(interfaceName)
	if err != nil {
		return err
	}
	if err := setupManagementPortConfig(link, cfg, routeManager); err != nil {
		return err
	}
	if err := setupManagementPortNFTChain(interfaceName, cfg); err != nil {
		return err
	}
	DelLegacyMgtPortIptRules()

	return nil
}

// syncMgmtPortInterface verifies if no other interface configured as management port. This may happen if another
// interface had been used as management port or Node was running in different mode.
// If old management port is found, its IP configuration is flushed and interface renamed.
func syncMgmtPortInterface(mgmtPortName string, isExpectedToBeInternal bool) error {
	// Query both type and name, because with type only stdout will be empty for both non-existing port and representor netdevice
	stdout, _, _ := util.RunOVSVsctl("--no-headings",
		"--data", "bare",
		"--format", "csv",
		"--columns", "type,name",
		"find", "Interface", "name="+mgmtPortName)
	if stdout == "" {
		// Not found on the bridge. But could be that interface with the same name exists
		return unconfigureMgmtNetdevicePort(mgmtPortName)
	}

	// Found existing port. Check its type
	if stdout == "internal,"+mgmtPortName {
		if isExpectedToBeInternal {
			// Do nothing
			return nil
		}

		klog.Infof("Found OVS internal port %s. Removing it", mgmtPortName)
		err := DeleteManagementPortInternalOVSInterface(types.DefaultNetworkName, mgmtPortName)
		if err != nil {
			return err
		}
		return nil
	}

	// It is representor which was used as management port.
	// Remove it from the bridge and rename.
	klog.Infof("Found existing representor management port. Removing it")
	return unconfigureMgmtRepresentorPort(mgmtPortName)
}

func unconfigureMgmtRepresentorPort(mgmtPortName string) error {
	// Get saved port name
	savedName, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Interface", mgmtPortName, "external-ids:ovn-orig-mgmt-port-rep-name")
	if err != nil {
		klog.Warningf("Failed to get external-ds:ovn-orig-mgmt-port-rep-name: %s", stderr)
	}

	return DeleteManagementPortRepInterface(types.DefaultNetworkName, mgmtPortName, savedName)
}

func unconfigureMgmtNetdevicePort(mgmtPortName string) error {
	link, err := util.GetNetLinkOps().LinkByName(mgmtPortName)
	if err != nil {
		if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return fmt.Errorf("failed to lookup %s link: %v", mgmtPortName, err)
		}
		// Nothing to unconfigure. Return.
		return nil
	}

	klog.Infof("Found existing management interface %s. Unconfiguring it", mgmtPortName)
	if err = tearDownManagementPortConfig(link); err != nil {
		return fmt.Errorf("teardown failed: %v", err)
	}

	savedName := ""
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		// Get original interface name saved at OVS database
		stdout, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".", "external-ids:ovn-orig-mgmt-port-netdev-name")
		if err != nil {
			klog.Warningf("Failed to get external-ds:ovn-orig-mgmt-port-netdev-name: %s", stderr)
		} else {
			savedName = stdout
		}
	}

	return TearDownManagementPortLink(types.DefaultNetworkName, link, savedName)
}

// DelLegacyMgtPortIptRules deletes legacy iptables rules for the management port; this is
// only used for cleaning up stale rules when upgrading, and can eventually be removed.
func DelLegacyMgtPortIptRules() {
	// Clean up all iptables and ip6tables remnants that may be left around
	ipt, err := util.GetIPTablesHelper(iptables.ProtocolIPv4)
	if err != nil {
		return
	}
	ipt6, err := util.GetIPTablesHelper(iptables.ProtocolIPv6)
	if err != nil {
		return
	}
	rule := []string{"-o", types.K8sMgmtIntfName, "-j", iptableMgmPortChain}
	_ = ipt.Delete("nat", "POSTROUTING", rule...)
	_ = ipt6.Delete("nat", "POSTROUTING", rule...)
	_ = ipt.ClearChain("nat", iptableMgmPortChain)
	_ = ipt6.ClearChain("nat", iptableMgmPortChain)
	_ = ipt.DeleteChain("nat", iptableMgmPortChain)
	_ = ipt6.DeleteChain("nat", iptableMgmPortChain)
}

// initMgmPortRoutingRules creates the routing table, routes and rules that
// let's us forward service traffic to ovn-k8s-mp0 as opposed to the default
// route towards breth0
func initMgmPortRoutingRules(mgmtCfg *managementPortConfig) error {
	// create ovnkubeSvcViaMgmPortRT and service route towards ovn-k8s-mp0
	for _, hostSubnet := range mgmtCfg.hostSubnets {
		isIPv6 := utilnet.IsIPv6CIDR(hostSubnet)
		gatewayIP := mgmtCfg.netInfo.GetNodeGatewayIP(hostSubnet).IP.String()
		for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
			if isIPv6 == utilnet.IsIPv6CIDR(svcCIDR) {
				if stdout, stderr, err := util.RunIP("route", "replace", "table", ovnkubeSvcViaMgmPortRT, svcCIDR.String(), "via", gatewayIP, "dev", types.K8sMgmtIntfName); err != nil {
					return fmt.Errorf("error adding routing table entry into custom routing table: %s: stdout: %s, stderr: %s, err: %v", ovnkubeSvcViaMgmPortRT, stdout, stderr, err)
				}
				klog.V(5).Infof("Successfully added route into custom routing table: %s", ovnkubeSvcViaMgmPortRT)
			}
		}
	}

	createRule := func(family string) error {
		stdout, stderr, err := util.RunIP(family, "rule")
		if err != nil {
			return fmt.Errorf("error listing routing rules, stdout: %s, stderr: %s, err: %v", stdout, stderr, err)
		}
		if !strings.Contains(stdout, fmt.Sprintf("from all fwmark %s lookup %s", types.OVNKubeITPMark, ovnkubeSvcViaMgmPortRT)) {
			if stdout, stderr, err := util.RunIP(family, "rule", "add", "fwmark", types.OVNKubeITPMark, "lookup", ovnkubeSvcViaMgmPortRT, "prio", "30"); err != nil {
				return fmt.Errorf("error adding routing rule for service via management table (%s): stdout: %s, stderr: %s, err: %v", ovnkubeSvcViaMgmPortRT, stdout, stderr, err)
			}
		}
		return nil
	}

	// create ip rule that will forward ovnkubeITPMark marked packets to ovnkubeITPRoutingTable
	if config.IPv4Mode {
		if err := createRule("-4"); err != nil {
			return fmt.Errorf("could not add IPv4 rule: %v", err)
		}
	}
	if config.IPv6Mode {
		if err := createRule("-6"); err != nil {
			return fmt.Errorf("could not add IPv6 rule: %v", err)
		}
	}

	// lastly update the reverse path filtering options for ovn-k8s-mp0 interface to avoid dropping return packets
	// NOTE: v6 doesn't have rp_filter strict mode block
	if config.IPv4Mode {
		return util.SetRPFilterLooseModeForInterface(types.K8sMgmtIntfName)
	}
	return nil
}
