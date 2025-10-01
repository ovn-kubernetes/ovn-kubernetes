package node

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/knftables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovnretry "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// ctMarkUDNBase is the conntrack mark base value for user defined networks to use
	// Each network gets its own mark == base + network-id
	ctMarkUDNBase = 3
	// pktMarkBase is the base value for packet mark assigned to user defined networks
	// Each network has a packet mark equal to base + network-id
	pktMarkBase = 4096
	// waitForPatchPortTimeout is the maximum time we wait for a UDN's patch
	// port to be created by OVN.
	waitForPatchPortTimeout = 30 * time.Second
	// UDNMasqueradeIPRulePriority the priority of the ip routing rules created for masquerade IP address
	// allocated for every user defined network.
	UDNMasqueradeIPRulePriority = 2000
)

// UserDefinedNetworkGateway contains information
// required to program a UDN at each node's
// gateway.
// NOTE: Currently invoked only for primary networks.
type UserDefinedNetworkGateway struct {
	// network information
	util.NetInfo
	// node that its programming things on
	node          *corev1.Node
	nodeLister    listers.NodeLister
	kubeInterface kube.Interface
	// vrf manager that creates and manages vrfs for all UDNs
	// used with a lock since its shared between all network controllers
	vrfManager *vrfmanager.Controller
	// masqCTMark holds the mark value for this network
	// which is used for egress traffic in shared gateway mode
	masqCTMark uint
	// pktMark hold the packets mark value for this network
	// which is used for directing traffic towards the UDN
	pktMark uint
	// v4MasqIPs holds the IPv4 masquerade IPs for this network
	v4MasqIPs *udn.MasqueradeIPs
	// v6MasqIPs holds the IPv6 masquerade IPs for this network
	v6MasqIPs *udn.MasqueradeIPs
	// stores the pointer to default network's gateway so that
	// we can leverage it from here to program UDN flows on breth0
	// Currently we use the openflowmanager and nodeIPManager from
	// gateway, but maybe we could invoke our own instance of these
	// for UDNs in the future. For now default network and UDNs will
	// use the same gateway struct instance
	*gateway
	// iprules manager that creates and manages iprules for
	// all UDNs. Must be accessed with a lock
	ruleManager *iprulemanager.Controller

	// reconcile channel to signal reconciliation of the gateway on network
	// configuration changes
	reconcile chan struct{}
	// ensure run() is only started once per instance
	runOnce sync.Once

	// vrfTableId holds the route table ID corresponding to management port interface of the network
	vrfTableId int

	// gwInterfaceIndex holds the link index of gateway interface
	gwInterfaceIndex int

	// save BGP state at the start of reconciliation loop run to handle it consistently throughout the run
	isNetworkAdvertisedToDefaultVRF bool
	isNetworkAdvertised             bool

	// retry framework for nodes
	retryNodes *ovnretry.RetryFramework
}

func NewUserDefinedNetworkGateway(netInfo util.NetInfo, node *corev1.Node, nodeLister listers.NodeLister,
	kubeInterface kube.Interface, vrfManager *vrfmanager.Controller, ruleManager *iprulemanager.Controller,
	defaultNetworkGateway Gateway) (*UserDefinedNetworkGateway, error) {
	// Generate a per network conntrack mark and masquerade IPs to be used for egress traffic.
	var (
		v4MasqIPs *udn.MasqueradeIPs
		v6MasqIPs *udn.MasqueradeIPs
		err       error
	)
	networkID := netInfo.GetNetworkID()
	masqCTMark := ctMarkUDNBase + uint(networkID)
	pktMark := pktMarkBase + uint(networkID)
	if config.IPv4Mode {
		v4MasqIPs, err = udn.AllocateV4MasqueradeIPs(networkID)
		if err != nil {
			return nil, fmt.Errorf("failed to get v4 masquerade IP, network %s (%d): %v", netInfo.GetNetworkName(), networkID, err)
		}
	}
	if config.IPv6Mode {
		v6MasqIPs, err = udn.AllocateV6MasqueradeIPs(networkID)
		if err != nil {
			return nil, fmt.Errorf("failed to get v6 masquerade IP, network %s (%d): %v", netInfo.GetNetworkName(), networkID, err)
		}
	}

	gw, ok := defaultNetworkGateway.(*gateway)
	if !ok {
		return nil, fmt.Errorf("unable to dereference default node network controller gateway object")
	}

	if gw.openflowManager == nil {
		return nil, fmt.Errorf("openflow manager has not been provided for network: %s", netInfo.GetNetworkName())
	}
	intfName := gw.openflowManager.defaultBridge.GetGatewayIface()
	link, err := util.GetNetLinkOps().LinkByName(intfName)
	if err != nil {
		return nil, fmt.Errorf("unable to get link for %s, error: %v", intfName, err)
	}

	return &UserDefinedNetworkGateway{
		NetInfo:          netInfo,
		node:             node,
		nodeLister:       nodeLister,
		kubeInterface:    kubeInterface,
		vrfManager:       vrfManager,
		masqCTMark:       masqCTMark,
		pktMark:          pktMark,
		v4MasqIPs:        v4MasqIPs,
		v6MasqIPs:        v6MasqIPs,
		gateway:          gw,
		ruleManager:      ruleManager,
		reconcile:        make(chan struct{}, 1),
		gwInterfaceIndex: link.Attrs().Index,
	}, nil
}

func (udng *UserDefinedNetworkGateway) Init() error {
	syncFunc := func([]any) error {
		return udng.syncNetwork()
	}
	udng.retryNodes = udng.newRetryFrameworkNodeWithParameters(factory.NodeType, syncFunc)
	if _, err := udng.retryNodes.WatchResource(); err != nil {
		return fmt.Errorf("UDN gateway init failed to start watching nodes: %v", err)
	}
	return nil
}

func (udng *UserDefinedNetworkGateway) AreResourcesEqual(objType reflect.Type, obj1, obj2 any) (bool, error) {
	switch objType {
	case factory.NodeType:
		oldNode, newNode := obj1.(*corev1.Node), obj2.(*corev1.Node)
		// For Layer2 topology, node host subnet annotations are irrelevant to the gateway programming.
		// Skip all node updates to avoid unnecessary reconciliations.
		if udng.TopologyType() == types.Layer2Topology {
			return true, nil
		}
		// Do not handle local node add (or) delete events.
		if (newNode != nil && newNode.Name != udng.node.Name) && oldNode == nil {
			return true, nil
		}
		if (oldNode != nil && oldNode.Name != udng.node.Name) && newNode == nil {
			return true, nil
		}
		// If this is an update for a non-local node, ignore it.
		if oldNode != nil && newNode != nil && oldNode.Name != udng.node.Name && newNode.Name != udng.node.Name {
			return true, nil
		}
		// Now let's compare host subnets associated with node object.
		var old, new []*net.IPNet
		if oldNode != nil && oldNode.Name == udng.node.Name {
			old, _ = util.ParseNodeHostSubnetAnnotation(oldNode, udng.GetNetworkName())
		}
		if newNode != nil && newNode.Name == udng.node.Name {
			new, _ = util.ParseNodeHostSubnetAnnotation(newNode, udng.GetNetworkName())
		}
		AreHostSubnetsSame := func(old, new []*net.IPNet) bool {
			if old == nil && new == nil {
				return true
			}
			if old == nil || new == nil {
				return false
			}
			if len(old) != len(new) {
				return false
			}
			if len(old) == 0 {
				return true
			}
			sort.Slice(old, func(i, j int) bool {
				return old[i].String() < old[j].String()
			})
			sort.Slice(new, func(i, j int) bool {
				return new[i].String() < new[j].String()
			})
			for i := range old {
				if old[i].String() != new[i].String() {
					return false
				}
			}
			return true
		}
		return AreHostSubnetsSame(old, new), nil
	default:
		return false, fmt.Errorf("no object comparison for type %s", objType)
	}
}

func (udng *UserDefinedNetworkGateway) AddResource(objType reflect.Type, _ any, _ bool) error {
	switch objType {
	case factory.NodeType:
		return udng.syncNetwork()
	default:
		return fmt.Errorf("no add function for object type %s", objType)
	}
}

func (udng *UserDefinedNetworkGateway) UpdateResource(objType reflect.Type, _ any, _ any, _ bool) error {
	switch objType {
	case factory.NodeType:
		return udng.syncNetwork()
	default:
		return fmt.Errorf("no update function for object type %s", objType)
	}
}

func (udng *UserDefinedNetworkGateway) DeleteResource(objType reflect.Type, _ any) error {
	switch objType {
	case factory.NodeType:
		return udng.DelNetwork()
	default:
		return fmt.Errorf("no delete function for object type %s", objType)
	}
}

// GetUDNMarkChain returns the UDN mark chain name
func GetUDNMarkChain(pktMark string) string {
	return "udn-mark-" + pktMark
}

// delMarkChain removes the UDN packet mark nftables chain
func (udng *UserDefinedNetworkGateway) delMarkChain() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()
	chain := &knftables.Chain{
		Name: GetUDNMarkChain(fmt.Sprintf("0x%x", udng.pktMark)),
	}
	// Delete would return an error if we tried to delete a chain that didn't exist, so
	// we do an Add first (which is a no-op if the chain already exists) and then Delete.
	tx.Add(chain)
	tx.Delete(chain)
	return nft.Run(context.TODO(), tx)
}

// addMarkChain adds the UDN nftables chain containing a rule that marks packets
// with the network specific value
func (udng *UserDefinedNetworkGateway) addMarkChain() error {
	counterIfDebug := ""
	if config.Logging.Level > 4 {
		counterIfDebug = "counter"
	}

	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()
	chain := &knftables.Chain{
		Name:    GetUDNMarkChain(fmt.Sprintf("0x%x", udng.pktMark)),
		Comment: ptr.To(fmt.Sprintf("%s: UDN packet marking", udng.GetNetworkName())),
	}
	tx.Add(chain)
	tx.Flush(chain)

	tx.Add(&knftables.Rule{
		Chain: chain.Name,
		Rule:  knftables.Concat("meta mark set", fmt.Sprintf("0x%x", udng.pktMark), counterIfDebug),
	})

	return nft.Run(context.TODO(), tx)
}

// SyncNetwork will be responsible to create all plumbings
// required by this UDN on the gateway side
func (udng *UserDefinedNetworkGateway) AddNetwork() error {
	err := udng.syncNetwork()
	// run gateway reconciliation loop on network configuration changes
	udng.run()
	return err
}

func (udng *UserDefinedNetworkGateway) syncNetwork() error {
	if udng.openflowManager == nil {
		return fmt.Errorf("openflow manager has not been provided for network: %s", udng.NetInfo.GetNetworkName())
	}
	// port is created first and its MAC address configured. The IP(s) on that link are added after enslaving to a VRF device (addUDNManagementPortIPs)
	// because IPv6 addresses are removed by the kernel (if not link local) when enslaved to a VRF device.
	// Add the routes(AddVRFRoutes) after setting the IP(s) to ensure that the default subnet route towards the mgmt network exists.
	mplink, err := udng.addUDNManagementPort()
	if err != nil {
		return fmt.Errorf("could not create management port netdevice for network %s: %w", udng.GetNetworkName(), err)
	}
	vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
	routes, err := udng.computeRoutesForUDN(mplink)
	if err != nil {
		return fmt.Errorf("failed to compute routes for network %s, err: %v", udng.GetNetworkName(), err)
	}
	if err = udng.vrfManager.AddVRF(vrfDeviceName, mplink.Attrs().Name, uint32(udng.vrfTableId), nil); err != nil {
		return fmt.Errorf("could not add VRF %d for network %s, err: %v", udng.vrfTableId, udng.GetNetworkName(), err)
	}
	if err = udng.addUDNManagementPortIPs(mplink); err != nil {
		return fmt.Errorf("unable to add management port IP(s) for link %s, for network %s: %w", mplink.Attrs().Name, udng.GetNetworkName(), err)
	}
	if err = udng.vrfManager.AddVRFRoutes(vrfDeviceName, routes); err != nil {
		return fmt.Errorf("could not add VRF %s routes for network %s, err: %v", vrfDeviceName, udng.GetNetworkName(), err)
	}

	udng.updateAdvertisementStatus()

	// create the iprules for this network
	if err = udng.updateUDNVRFIPRules(); err != nil {
		return fmt.Errorf("failed to update IP rules for network %s: %w", udng.GetNetworkName(), err)
	}

	if err = udng.updateAdvertisedUDNIsolationRules(); err != nil {
		return fmt.Errorf("failed to update isolation rules for network %s: %w", udng.GetNetworkName(), err)
	}

	if err := udng.updateUDNVRFIPRoute(); err != nil {
		return fmt.Errorf("failed to update ip routes for network %s: %w", udng.GetNetworkName(), err)
	}

	// add loose mode for rp filter on management port
	mgmtPortName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.GetNetworkID()))
	if err := addRPFilterLooseModeForManagementPort(mgmtPortName); err != nil {
		return fmt.Errorf("could not set loose mode for reverse path filtering on management port %s: %v", mgmtPortName, err)
	}

	nodeSubnets, err := udng.getLocalSubnets()
	var mgmtIPs []*net.IPNet
	for _, subnet := range nodeSubnets {
		mgmtIPs = append(mgmtIPs, udng.GetNodeManagementIP(subnet))
	}
	if err != nil {
		return fmt.Errorf("failed to get node subnets for network %s: %w", udng.GetNetworkName(), err)
	}
	if err = udng.openflowManager.addNetwork(udng.NetInfo, nodeSubnets, mgmtIPs, udng.masqCTMark, udng.pktMark, udng.v6MasqIPs, udng.v4MasqIPs); err != nil {
		return fmt.Errorf("could not add network %s: %v", udng.GetNetworkName(), err)
	}

	waiter := newStartupWaiterWithTimeout(waitForPatchPortTimeout)
	readyFunc := func() (bool, error) {
		if err := udng.openflowManager.defaultBridge.SetNetworkOfPatchPort(udng.GetNetworkName()); err != nil {
			klog.V(3).Infof("Failed to set network %s's openflow ports for default bridge; error: %v", udng.GetNetworkName(), err)
			return false, nil
		}
		if udng.openflowManager.externalGatewayBridge != nil {
			if err := udng.openflowManager.externalGatewayBridge.SetNetworkOfPatchPort(udng.GetNetworkName()); err != nil {
				klog.V(3).Infof("Failed to set network %s's openflow ports for secondary bridge; error: %v", udng.GetNetworkName(), err)
				return false, nil
			}
		}
		return true, nil
	}
	postFunc := func() error {
		if err := udng.gateway.Reconcile(); err != nil {
			return fmt.Errorf("failed to reconcile flows on bridge for network %s; error: %v", udng.GetNetworkName(), err)
		}
		return nil
	}
	waiter.AddWait(readyFunc, postFunc)
	if err := waiter.Wait(); err != nil {
		return err
	}

	if err := udng.addMarkChain(); err != nil {
		return fmt.Errorf("failed to add the service masquerade chain: %w", err)
	}

	return nil
}

func (udng *UserDefinedNetworkGateway) GetNetworkRuleMetadata() string {
	return fmt.Sprintf("%s-%d", udng.GetNetworkName(), udng.GetNetworkID())
}

// DelNetwork will be responsible to remove all plumbings used by this UDN on
// the gateway side. It's considered invalid to call this instance after
// DelNetwork has returned succesfully.
func (udng *UserDefinedNetworkGateway) DelNetwork() error {
	vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
	// delete the iprules for this network
	if err := udng.ruleManager.DeleteWithMetadata(udng.GetNetworkRuleMetadata()); err != nil {
		return fmt.Errorf("unable to delete iprules for network %s, err: %v", udng.GetNetworkName(), err)
	}
	// delete the VRF device for this network
	if err := udng.vrfManager.DeleteVRF(vrfDeviceName); err != nil {
		return err
	}
	// delete the openflows for this network
	if udng.openflowManager != nil {
		udng.openflowManager.delNetwork(udng.NetInfo)
		if err := udng.gateway.Reconcile(); err != nil {
			return fmt.Errorf("failed to reconcile default gateway for network %s, err: %v", udng.GetNetworkName(), err)
		}
	}

	err := udng.deleteAdvertisedUDNIsolationRules()
	if err != nil {
		return fmt.Errorf("failed to remove advertised UDN isolation rules for network %s: %w", udng.GetNetworkName(), err)
	}

	if err := udng.delMarkChain(); err != nil {
		return err
	}
	// delete the management port interface for this network
	err = udng.deleteUDNManagementPort()
	if err != nil {
		return err
	}

	// close channel only when succesful since we can be called multiple times
	// on failure
	close(udng.reconcile)
	return nil
}

// addUDNManagementPort does the following:
// STEP1: creates the (netdevice) OVS interface on br-int for the UDN's management port
// STEP2: sets up the management port link on the host
// STEP3: enables IPv4 forwarding on the interface if the network has a v4 subnet
// Returns a netlink Link which is the UDN management port interface along with its MAC address
func (udng *UserDefinedNetworkGateway) addUDNManagementPort() (netlink.Link, error) {
	var err error
	interfaceName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.GetNetworkID()))
	networkLocalSubnets, err := udng.getLocalSubnets()
	if err != nil {
		return nil, err
	}
	if len(networkLocalSubnets) == 0 {
		return nil, fmt.Errorf("cannot determine subnets while configuring management port for network: %s", udng.GetNetworkName())
	}
	macAddr := util.IPAddrToHWAddr(udng.GetNodeManagementIP(networkLocalSubnets[0]).IP)

	// STEP1
	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--may-exist", "add-port", "br-int", interfaceName,
		"--", "set", "interface", interfaceName, fmt.Sprintf("mac=\"%s\"", macAddr.String()),
		"type=internal", "mtu_request="+fmt.Sprintf("%d", udng.NetInfo.MTU()),
		"external-ids:iface-id="+udng.GetNetworkScopedK8sMgmtIntfName(udng.node.Name),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to add port to br-int for network %s, stdout: %q, stderr: %q, error: %w",
			udng.GetNetworkName(), stdout, stderr, err)
	}
	klog.V(3).Infof("Added OVS management port interface %s for network %s", interfaceName, udng.GetNetworkName())

	// STEP2
	mplink, err := util.LinkSetUp(interfaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to set the link up for interface %s while plumbing network %s, err: %v",
			interfaceName, udng.GetNetworkName(), err)
	}
	vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
	udng.vrfTableId = vrfTableId
	klog.V(3).Infof("Setup management port link %s for network %s succeeded", interfaceName, udng.GetNetworkName())

	// STEP3
	// IPv6 forwarding is enabled globally
	if ipv4, _ := udng.IPMode(); ipv4 {
		// we use forward slash as path separator to allow dotted interfaceName e.g. foo.200
		stdout, stderr, err := util.RunSysctl("-w", fmt.Sprintf("net/ipv4/conf/%s/forwarding=1", interfaceName))
		// systctl output enforces dot as path separator
		if err != nil || stdout != fmt.Sprintf("net.ipv4.conf.%s.forwarding = 1", strings.ReplaceAll(interfaceName, ".", "/")) {
			return nil, fmt.Errorf("could not set the correct forwarding value for interface %s: stdout: %v, stderr: %v, err: %v",
				interfaceName, stdout, stderr, err)
		}
	}
	return mplink, nil
}

// getLocalSubnets returns pod subnets used by the current node.
// For L3 networks it parses the ovnNodeSubnets annotation, for L2 networks it returns the network subnets.
func (udng *UserDefinedNetworkGateway) getLocalSubnets() ([]*net.IPNet, error) {
	var networkLocalSubnets []*net.IPNet
	var err error

	// fetch subnets which we will use to get management port IP(s)
	if udng.TopologyType() == types.Layer3Topology {
		networkLocalSubnets, err = util.ParseNodeHostSubnetAnnotation(udng.node, udng.GetNetworkName())
		if err != nil {
			return nil, fmt.Errorf("waiting for node %s to start, no annotation found on node for network %s: %w",
				udng.node.Name, udng.GetNetworkName(), err)
		}
	} else if udng.TopologyType() == types.Layer2Topology {
		// NOTE: We don't support L2 networks without subnets as primary UDNs
		globalFlatL2Networks := udng.Subnets()
		for _, globalFlatL2Network := range globalFlatL2Networks {
			networkLocalSubnets = append(networkLocalSubnets, globalFlatL2Network.CIDR)
		}
	}
	return networkLocalSubnets, nil
}

func (udng *UserDefinedNetworkGateway) addUDNManagementPortIPs(mpLink netlink.Link) error {
	networkLocalSubnets, err := udng.getLocalSubnets()
	if err != nil {
		return err
	}

	// extract management port IP from subnets and add it to link
	for _, subnet := range networkLocalSubnets {
		if config.IPv6Mode && utilnet.IsIPv6CIDR(subnet) || config.IPv4Mode && utilnet.IsIPv4CIDR(subnet) {
			ip := udng.GetNodeManagementIP(subnet)
			var err error
			var exists bool
			if exists, err = util.LinkAddrExist(mpLink, ip); err == nil && !exists {
				err = util.LinkAddrAdd(mpLink, ip, 0, 0, 0)
			}
			if err != nil {
				return fmt.Errorf("failed to add management port IP from subnet %s to netdevice %s for network %s, err: %v",
					subnet, mpLink.Attrs().Name, udng.GetNetworkName(), err)
			}
		}
	}
	return nil
}

// deleteUDNManagementPort does the following:
// STEP1: deletes the OVS interface on br-int for the UDN's management port interface
// STEP2: deletes the mac address from the annotation
func (udng *UserDefinedNetworkGateway) deleteUDNManagementPort() error {
	var err error
	interfaceName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.GetNetworkID()))
	// STEP1
	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--if-exists", "del-port", "br-int", interfaceName,
	)
	if err != nil {
		return fmt.Errorf("failed to delete port from br-int for network %s, stdout: %q, stderr: %q, error: %v",
			udng.GetNetworkName(), stdout, stderr, err)
	}
	klog.V(3).Infof("Removed OVS management port interface %s for network %s", interfaceName, udng.GetNetworkName())
	return nil
}

// computeRoutesForUDN returns a list of routes programmed into a given UDN's VRF
// when adding new routes please leave a sample comment on how that route looks like
func (udng *UserDefinedNetworkGateway) computeRoutesForUDN(mpLink netlink.Link) ([]netlink.Route, error) {
	networkMTU := udng.NetInfo.MTU()
	if networkMTU == 0 {
		networkMTU = config.Default.MTU
	}
	var retVal []netlink.Route
	// Route1: Add serviceCIDR route: 10.96.0.0/16 via 169.254.169.4 dev breth0 mtu 1400
	// necessary for UDN CNI and host-networked pods to talk to services
	for _, serviceSubnet := range config.Kubernetes.ServiceCIDRs {
		serviceSubnet := serviceSubnet
		isV6 := utilnet.IsIPv6CIDR(serviceSubnet)
		gwIP := config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP
		if isV6 {
			gwIP = config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP
		}
		retVal = append(retVal, netlink.Route{
			LinkIndex: udng.gwInterfaceIndex,
			Dst:       serviceSubnet,
			MTU:       networkMTU,
			Gw:        gwIP,
			Table:     udng.vrfTableId,
		})
	}

	// Route2: Add default route: default via 172.18.0.1 dev breth0 mtu 1400
	// necessary for UDN CNI and host-networked pods default traffic to go to node's gatewayIP
	defaultRoute, err := udng.getDefaultRouteExceptIfVRFLite()
	if err != nil {
		return nil, fmt.Errorf("unable to add default route for network %s, err: %v", udng.GetNetworkName(), err)
	}
	retVal = append(retVal, defaultRoute...)

	// Route3: Add MasqueradeRoute for reply traffic route: 169.254.169.12 dev ovn-k8s-mpX mtu 1400
	// necessary for reply traffic towards UDN CNI pods to go into OVN
	masqIPv4, err := udng.getV4MasqueradeIP()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch masqueradeV4 IP for network %s, err: %v", udng.GetNetworkName(), err)
	}
	if masqIPv4 != nil {
		retVal = append(retVal, netlink.Route{
			LinkIndex: mpLink.Attrs().Index,
			Dst:       masqIPv4,
			MTU:       networkMTU,
			Table:     udng.vrfTableId,
		})
	}

	masqIPv6, err := udng.getV6MasqueradeIP()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch masqueradeV6 IP for network %s, err: %v", udng.GetNetworkName(), err)
	}
	if masqIPv6 != nil {
		retVal = append(retVal, netlink.Route{
			LinkIndex: mpLink.Attrs().Index,
			Dst:       masqIPv6,
			MTU:       networkMTU,
			Table:     udng.vrfTableId,
		})
	}

	// Add routes for V[4|6]HostETPLocalMasqueradeIP:
	//   169.254.0.3 via 100.100.1.1 dev ovn-k8s-mp1
	// For Layer3 networks add the cluster subnet route
	//   100.100.0.0/16 via 100.100.1.1 dev ovn-k8s-mp1
	networkLocalSubnets, err := udng.getLocalSubnets()
	if err != nil {
		return nil, err
	}
	for _, localSubnet := range networkLocalSubnets {
		gwIP := udng.GetNodeGatewayIP(localSubnet)
		if gwIP == nil {
			return nil, fmt.Errorf("unable to find gateway IP for network %s, subnet: %s", udng.GetNetworkName(), localSubnet)
		}
		etpLocalMasqueradeIP := config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP
		if utilnet.IsIPv6CIDR(localSubnet) {
			etpLocalMasqueradeIP = config.Gateway.MasqueradeIPs.V6HostETPLocalMasqueradeIP
		}
		retVal = append(retVal, netlink.Route{
			LinkIndex: mpLink.Attrs().Index,
			Dst: &net.IPNet{
				IP:   etpLocalMasqueradeIP,
				Mask: util.GetIPFullMask(etpLocalMasqueradeIP),
			},
			Gw:    gwIP.IP,
			Table: udng.vrfTableId,
		})
		if udng.NetInfo.TopologyType() == types.Layer3Topology {
			for _, clusterSubnet := range udng.Subnets() {
				if clusterSubnet.CIDR.Contains(gwIP.IP) {
					retVal = append(retVal, netlink.Route{
						LinkIndex: mpLink.Attrs().Index,
						Dst:       clusterSubnet.CIDR,
						Gw:        gwIP.IP,
						Table:     udng.vrfTableId,
					})
				}
			}
		}
	}
	// Add unreachable route to enure that kernel always finds a match to the VRF table rather than
	// referring to default VRF table and send traffic via unwanted interfaces and to unwanted gateway.
	// non 0 link index for an unreachable or blackhole IPv4 route returns 'invalid argument'
	hasV4Subnet, hasV6Subnet := udng.IPMode()
	if hasV4Subnet {
		_, v4AnyCIDR, _ := net.ParseCIDR("0.0.0.0/0")
		retVal = append(retVal, netlink.Route{
			Dst:      v4AnyCIDR,
			Table:    udng.vrfTableId,
			Priority: 4278198272,
			Type:     unix.RTN_UNREACHABLE,
		})
	}
	// link index for an unreachable IPv6 route always get set to 1. Link index 1 refers to default loopback
	// device at all time. Reference: https://docs.kernel.org/networking/vrf.html#using-iproute2-for-vrfs
	if hasV6Subnet {
		_, v6AnyCIDR, _ := net.ParseCIDR("::/0")
		retVal = append(retVal, netlink.Route{
			LinkIndex: types.LoopbackInterfaceIndex,
			Dst:       v6AnyCIDR,
			Table:     udng.vrfTableId,
			Priority:  4278198272,
			Type:      unix.RTN_UNREACHABLE,
		})
	}
	return retVal, nil
}

func (udng *UserDefinedNetworkGateway) getDefaultRoute() ([]netlink.Route, error) {
	networkMTU := udng.NetInfo.MTU()
	if networkMTU == 0 {
		networkMTU = config.Default.MTU
	}

	var retVal []netlink.Route
	var defaultAnyCIDR *net.IPNet
	for _, nextHop := range udng.gateway.nextHops {
		isV6 := utilnet.IsIPv6(nextHop)
		_, defaultAnyCIDR, _ = net.ParseCIDR("0.0.0.0/0")
		if isV6 {
			_, defaultAnyCIDR, _ = net.ParseCIDR("::/0")
		}
		retVal = append(retVal, netlink.Route{
			LinkIndex: udng.gwInterfaceIndex,
			Dst:       defaultAnyCIDR,
			MTU:       networkMTU,
			Gw:        nextHop,
			Table:     udng.vrfTableId,
		})
	}
	return retVal, nil
}

func (udng *UserDefinedNetworkGateway) getDefaultRouteExceptIfVRFLite() ([]netlink.Route, error) {
	// If the network is advertised on a non default VRF then we should only consider routes received from external BGP
	// device and not send any traffic based on default route similar to one present in default VRF. This is more important
	// for VRF-Lite usecase where we need traffic to leave from vlan device instead of default gateway interface.
	if udng.isNetworkAdvertised && !udng.isNetworkAdvertisedToDefaultVRF {
		return nil, nil
	}
	return udng.getDefaultRoute()
}

// getV4MasqueradeIP returns the V4 management port masqueradeIP for this network
func (udng *UserDefinedNetworkGateway) getV4MasqueradeIP() (*net.IPNet, error) {
	if !config.IPv4Mode {
		return nil, nil
	}
	masqIPs, err := udn.AllocateV4MasqueradeIPs(udng.GetNetworkID())
	if err != nil {
		return nil, fmt.Errorf("failed to allocate masquerade IPs for v4 stack for network %s: %w", udng.GetNetworkName(), err)
	}
	return util.GetIPNetFullMaskFromIP(masqIPs.ManagementPort.IP), nil
}

// getV6MasqueradeIP returns the V6 management port masqueradeIP for this network
func (udng *UserDefinedNetworkGateway) getV6MasqueradeIP() (*net.IPNet, error) {
	if !config.IPv6Mode {
		return nil, nil
	}
	masqIPs, err := udn.AllocateV6MasqueradeIPs(udng.GetNetworkID())
	if err != nil {
		return nil, fmt.Errorf("failed to allocate masquerade IPs for v6 stack for network %s: %w", udng.GetNetworkName(), err)
	}
	return util.GetIPNetFullMaskFromIP(masqIPs.ManagementPort.IP), nil
}

// constructUDNVRFIPRules constructs rules that redirect matching packets
// into the corresponding UDN VRF routing table.
//
// When a network is not advertised on the default VRF, an example of the rules
// we set for it is:
// 2000:	from all fwmark 0x1001 lookup 1007
// 2000:	from all to 169.254.0.12 lookup 1007
//
// When a network is advertised on the default VRF, an example of the rules
// we set for it is:
// 2000:	from all fwmark 0x1001 lookup 1007
// 2000:	from all to 10.132.0.0/14 lookup 1007
// 2000:	from all to 169.254.0.12 lookup 1007
func (udng *UserDefinedNetworkGateway) constructUDNVRFIPRules() ([]netlink.Rule, []netlink.Rule, error) {
	var addIPRules []netlink.Rule
	var delIPRules []netlink.Rule
	var masqIPRules []netlink.Rule
	var subnetIPRules []netlink.Rule
	masqIPv4, err := udng.getV4MasqueradeIP()
	if err != nil {
		return nil, nil, err
	}
	masqIPv6, err := udng.getV6MasqueradeIP()
	if err != nil {
		return nil, nil, err
	}

	if masqIPv4 != nil {
		addIPRules = append(addIPRules, generateIPRuleForPacketMark(udng.pktMark, false, uint(udng.vrfTableId)))
		masqIPRules = append(masqIPRules, generateIPRuleForMasqIP(masqIPv4.IP, false, uint(udng.vrfTableId)))
		for _, subnet := range udng.Subnets() {
			if utilnet.IsIPv4CIDR(subnet.CIDR) {
				subnetIPRules = append(subnetIPRules, generateIPRuleForUDNSubnet(subnet.CIDR, false, uint(udng.vrfTableId)))
			}
		}
	}
	if masqIPv6 != nil {
		addIPRules = append(addIPRules, generateIPRuleForPacketMark(udng.pktMark, true, uint(udng.vrfTableId)))
		masqIPRules = append(masqIPRules, generateIPRuleForMasqIP(masqIPv6.IP, true, uint(udng.vrfTableId)))
		for _, subnet := range udng.Subnets() {
			if utilnet.IsIPv6CIDR(subnet.CIDR) {
				subnetIPRules = append(subnetIPRules, generateIPRuleForUDNSubnet(subnet.CIDR, true, uint(udng.vrfTableId)))
			}
		}
	}
	switch {
	case udng.isNetworkAdvertisedToDefaultVRF:
		// the network is advertised to the default VRF
		addIPRules = append(addIPRules, masqIPRules...)
		addIPRules = append(addIPRules, subnetIPRules...)
	default:
		// network is not advertised on the default VRF
		addIPRules = append(addIPRules, masqIPRules...)
		delIPRules = append(delIPRules, subnetIPRules...)
	}
	return addIPRules, delIPRules, nil
}

func generateIPRuleForPacketMark(mark uint, isIPv6 bool, vrfTableId uint) netlink.Rule {
	r := *netlink.NewRule()
	r.Table = int(vrfTableId)
	r.Priority = UDNMasqueradeIPRulePriority
	r.Family = netlink.FAMILY_V4
	if isIPv6 {
		r.Family = netlink.FAMILY_V6
	}
	r.Mark = uint32(mark)
	return r
}
func generateIPRuleForMasqIP(masqIP net.IP, isIPv6 bool, vrfTableId uint) netlink.Rule {
	r := *netlink.NewRule()
	r.Table = int(vrfTableId)
	r.Priority = UDNMasqueradeIPRulePriority
	r.Family = netlink.FAMILY_V4
	if isIPv6 {
		r.Family = netlink.FAMILY_V6
	}
	r.Dst = util.GetIPNetFullMaskFromIP(masqIP)
	return r
}

func generateIPRuleForUDNSubnet(udnIP *net.IPNet, isIPv6 bool, vrfTableId uint) netlink.Rule {
	r := *netlink.NewRule()
	r.Table = int(vrfTableId)
	r.Priority = UDNMasqueradeIPRulePriority
	r.Family = netlink.FAMILY_V4
	if isIPv6 {
		r.Family = netlink.FAMILY_V6
	}
	r.Dst = udnIP
	return r
}

func addRPFilterLooseModeForManagementPort(mgmtPortName string) error {
	// update the reverse path filtering options for ovn-k8s-mpX interface to avoid dropping packets with masqueradeIP
	// coming out of managementport interface
	// NOTE: v6 doesn't have rp_filter strict mode block
	rpFilterLooseMode := "2"
	// TODO: Convert testing framework to mock golang module utilities. Example:
	// result, err := sysctl.Sysctl(fmt.Sprintf("net/ipv4/conf/%s/rp_filter", types.K8sMgmtIntfName), rpFilterLooseMode)
	// we use forward slash as path separator to allow dotted mgmtPortName e.g. foo.200
	stdout, stderr, err := util.RunSysctl("-w", fmt.Sprintf("net/ipv4/conf/%s/rp_filter=%s", mgmtPortName, rpFilterLooseMode))
	// systctl output enforces dot as path separator
	if err != nil || stdout != fmt.Sprintf("net.ipv4.conf.%s.rp_filter = %s", strings.ReplaceAll(mgmtPortName, ".", "/"), rpFilterLooseMode) {
		return fmt.Errorf("could not set the correct rp_filter value for interface %s: stdout: %v, stderr: %v, err: %v",
			mgmtPortName, stdout, stderr, err)
	}
	return nil
}

func (udng *UserDefinedNetworkGateway) run() {
	udng.runOnce.Do(func() {
		go func() {
			for range udng.reconcile {
				err := retry.OnError(
					wait.Backoff{
						Duration: 10 * time.Millisecond,
						Steps:    4,
						Factor:   5.0,
					},
					func(error) bool {
						select {
						case _, open := <-udng.reconcile:
							return open
						default:
							return true
						}
					},
					udng.syncNetwork,
				)
				if err != nil {
					klog.Errorf("Failed to reconcile gateway for network %s: %v", udng.GetNetworkName(), err)
				}
			}
		}()
	})
}

func (udng *UserDefinedNetworkGateway) Reconcile() {
	select {
	case udng.reconcile <- struct{}{}:
	default:
	}
}

// updateUDNVRFIPRules updates IP rules for a network depending on whether the
// network is advertised to the default VRF or not
func (udng *UserDefinedNetworkGateway) updateUDNVRFIPRules() error {
	addIPRules, deleteIPRules, err := udng.constructUDNVRFIPRules()
	if err != nil {
		return fmt.Errorf("unable to get iprules for network %s, err: %v", udng.GetNetworkName(), err)
	}

	for _, rule := range addIPRules {
		if err = udng.ruleManager.AddWithMetadata(rule, udng.GetNetworkRuleMetadata()); err != nil {
			return fmt.Errorf("unable to create iprule %v for network %s, err: %v", rule, udng.GetNetworkName(), err)
		}
	}
	for _, rule := range deleteIPRules {
		if err = udng.ruleManager.Delete(rule); err != nil {
			return fmt.Errorf("unable to delete iprule for network %s, err: %v", udng.GetNetworkName(), err)
		}
	}
	return nil
}

// Add or remove default route from a vrf device based on the network is
// advertised on its own network or the default network
func (udng *UserDefinedNetworkGateway) updateUDNVRFIPRoute() error {
	vrfName := util.GetNetworkVRFName(udng.NetInfo)

	switch {
	case udng.isNetworkAdvertised && !udng.isNetworkAdvertisedToDefaultVRF:
		// Remove default route for networks advertised to non-default VRF
		if err := udng.removeDefaultRouteFromVRF(); err != nil {
			return fmt.Errorf("failed to remove default route from VRF %s for network %s: %v",
				vrfName, udng.GetNetworkName(), err)
		}

	default:
		// Add default route for networks that are either:
		// - not advertised
		// - advertised to default VRF
		defaultRoute, err := udng.getDefaultRouteExceptIfVRFLite()
		if err != nil {
			return fmt.Errorf("failed to get default route for network %s: %v",
				udng.GetNetworkName(), err)
		}

		if err = udng.vrfManager.AddVRFRoutes(vrfName, defaultRoute); err != nil {
			return fmt.Errorf("failed to add default route to VRF %s for network %s: %v",
				vrfName, udng.GetNetworkName(), err)
		}
	}

	return nil
}

func (udng *UserDefinedNetworkGateway) removeDefaultRouteFromVRF() error {
	vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
	defaultRoute, err := udng.getDefaultRoute()
	if err != nil {
		return fmt.Errorf("unable to get default route for network %s, err: %v", udng.GetNetworkName(), err)
	}
	if err = udng.vrfManager.DeleteVRFRoutes(vrfDeviceName, defaultRoute); err != nil {
		return fmt.Errorf("unable to delete routes for network %s, err: %v", udng.GetNetworkName(), err)
	}
	return nil
}

// updateAdvertisedUDNIsolationRules adds the full UDN subnets to nftablesAdvertisedUDNsSetV[4|6] nft set that is used
// in the following chain/rules to drop locally generated traffic towards a UDN network:
//
//	chain udn-bgp-drop {
//	  comment "Drop traffic generated locally towards advertised UDN subnets"
//	  type filter hook output priority filter; policy accept;
//	  ip daddr @advertised-udn-subnets-v4 counter packets 0 bytes 0 drop
//	  ip6 daddr @advertised-udn-subnets-v6 counter packets 0 bytes 0 drop
//	}
//
// It blocks access to the full UDN subnet to handle a case in L3 when a node tries to access
// a host subnet available on a different node. Example set entries:
//
//	 set advertised-udn-subnets-v4 {
//	   type ipv4_addr
//	   flags interval
//	   comment "advertised UDNs V4 subnets"
//	   elements = { 10.10.0.0/16 comment "cluster_udn_l3network" }
//	}
func (udng *UserDefinedNetworkGateway) updateAdvertisedUDNIsolationRules() error {
	switch {
	case udng.isNetworkAdvertised:
		return udng.addAdvertisedUDNIsolationRules()
	default:
		return udng.deleteAdvertisedUDNIsolationRules()
	}
}

func (udng *UserDefinedNetworkGateway) addAdvertisedUDNIsolationRules() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %v", err)
	}
	tx := nft.NewTransaction()

	for _, udnNet := range udng.Subnets() {
		set := nftablesAdvertisedUDNsSetV4
		if utilnet.IsIPv6CIDR(udnNet.CIDR) {
			set = nftablesAdvertisedUDNsSetV6
		}
		tx.Add(&knftables.Element{
			Set:     set,
			Key:     []string{udnNet.CIDR.String()},
			Comment: knftables.PtrTo(udng.GetNetworkName()),
		})

	}

	if tx.NumOperations() == 0 {
		return nil
	}
	return nft.Run(context.TODO(), tx)
}

func (udng *UserDefinedNetworkGateway) deleteAdvertisedUDNIsolationRules() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed to get nftables helper: %v", err)
	}
	tx := nft.NewTransaction()

	existingV4, err := nft.ListElements(context.TODO(), "set", nftablesAdvertisedUDNsSetV4)
	if err != nil {
		if !knftables.IsNotFound(err) {
			return fmt.Errorf("could not list existing items in %s set: %w", nftablesAdvertisedUDNsSetV4, err)
		}
	}
	existingV6, err := nft.ListElements(context.TODO(), "set", nftablesAdvertisedUDNsSetV6)
	if err != nil {
		if !knftables.IsNotFound(err) {
			return fmt.Errorf("could not list existing items in %s set: %w", nftablesAdvertisedUDNsSetV6, err)
		}
	}

	for _, elem := range append(existingV4, existingV6...) {
		if elem.Comment != nil && *elem.Comment == udng.GetNetworkName() {
			tx.Delete(elem)
		}
	}

	if tx.NumOperations() == 0 {
		return nil
	}
	return nft.Run(context.TODO(), tx)
}

func (udng *UserDefinedNetworkGateway) updateAdvertisementStatus() {
	vrfs := udng.GetPodNetworkAdvertisedOnNodeVRFs(udng.node.Name)
	udng.isNetworkAdvertised = len(vrfs) > 0
	udng.isNetworkAdvertisedToDefaultVRF = slices.Contains(vrfs, types.DefaultNetworkName)
}
