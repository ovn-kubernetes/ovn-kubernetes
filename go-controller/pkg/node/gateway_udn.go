// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/knftables"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
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
	// dpuUDNVRFRouteTableIDStart is a distinct table range for UDN VRFs
	// created on DPUs. DPU mode has no host management interface to derive a
	// table from, so this avoids colliding with link-index based tables.
	dpuUDNVRFRouteTableIDStart = 100000
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
	ruleManager iprulemanager.Interface

	// management port controller
	mgmtPortController *managementport.UDNManagementPortController

	ovsClient               libovsdbclient.Client
	uplinkStateLister       uplinklisters.UplinkStateLister
	uplinkGatewayController *UplinkGatewayController
	// openflowBridgeName selects the openflow manager target for this gateway.
	// defaultOpenFlowBridgeSetName means the default bridge plus external gateway bridge.
	openflowBridgeName string
	// nextHops holds the default route next hops for this UDN gateway.
	nextHops []net.IP

	// reconcile channel to signal reconciliation of the gateway on network
	// configuration changes
	reconcile chan struct{}

	// vrfTableId holds the route table ID corresponding to management port interface of the network
	vrfTableId int

	// gwInterfaceName holds the gateway interface name for this network.
	gwInterfaceName string
	// gwInterfaceIndex holds the link index of the gateway interface for this network.
	gwInterfaceIndex int

	// save BGP state at the start of reconciliation loop run to handle it consistently throughout the run
	isNetworkAdvertisedToDefaultVRF bool
	isNetworkAdvertised             bool
}

func NewUserDefinedNetworkGateway(netInfo util.NetInfo, node *corev1.Node, nodeLister listers.NodeLister,
	kubeInterface kube.Interface, vrfManager *vrfmanager.Controller, ruleManager iprulemanager.Interface,
	defaultNetworkGateway Gateway, ovsClient libovsdbclient.Client, uplinkStateLister uplinklisters.UplinkStateLister,
	uplinkGatewayController *UplinkGatewayController) (*UserDefinedNetworkGateway, error) {
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

	if (config.IsModeDPU() || config.IsModeFull()) && gw.openflowManager == nil {
		return nil, fmt.Errorf("openflow manager has not been provided for network: %s", netInfo.GetNetworkName())
	}

	intfName := gw.GetGatewayIface()
	if intfName == "" {
		return nil, fmt.Errorf("failed to get gateway interface for network: %s", netInfo.GetNetworkName())
	}
	link, err := util.GetNetLinkOps().LinkByName(intfName)
	if err != nil {
		return nil, fmt.Errorf("unable to get link for gateway interface %s, error: %v", intfName, err)
	}
	gwInterfaceName := intfName
	gwInterfaceIndex := link.Attrs().Index
	if netInfo.Uplink() != "" {
		gwInterfaceName = ""
		gwInterfaceIndex = 0
	}

	return &UserDefinedNetworkGateway{
		NetInfo:                 netInfo,
		node:                    node,
		nodeLister:              nodeLister,
		kubeInterface:           kubeInterface,
		vrfManager:              vrfManager,
		masqCTMark:              masqCTMark,
		pktMark:                 pktMark,
		v4MasqIPs:               v4MasqIPs,
		v6MasqIPs:               v6MasqIPs,
		gateway:                 gw,
		ruleManager:             ruleManager,
		ovsClient:               ovsClient,
		uplinkStateLister:       uplinkStateLister,
		uplinkGatewayController: uplinkGatewayController,
		openflowBridgeName:      defaultOpenFlowBridgeSetName,
		nextHops:                gw.nextHops,
		reconcile:               make(chan struct{}, 1),
		gwInterfaceName:         gwInterfaceName,
		gwInterfaceIndex:        gwInterfaceIndex,
	}, nil
}

type resolvedUplinkGateway struct {
	hostInterfaceName string
	bridgeName        string
	macAddress        net.HardwareAddr
	ipAddresses       []*net.IPNet
	defaultGateways   []net.IP
}

func (udng *UserDefinedNetworkGateway) ensureUplinkGateway() (*bridgeconfig.BridgeConfiguration, error) {
	if udng.Uplink() == "" {
		return nil, nil
	}
	resolved, err := udng.resolveUplinkGateway(config.IsModeDPU() || config.IsModeFull())
	if err != nil {
		return nil, err
	}
	udng.nextHops = resolved.defaultGateways

	gwInterfaceName := uplinkGatewayInterfaceName(resolved)
	if gwInterfaceName == "" {
		return nil, fmt.Errorf("uplink gateway interface is unset for network %s", udng.GetNetworkName())
	}
	link, err := util.GetNetLinkOps().LinkByName(gwInterfaceName)
	if err != nil {
		return nil, fmt.Errorf("unable to get link for Uplink gateway interface %s: %w",
			gwInterfaceName, err)
	}
	if link.Attrs().Index == 0 {
		return nil, fmt.Errorf("uplink gateway interface %s has invalid link index for network %s",
			gwInterfaceName, udng.GetNetworkName())
	}
	udng.gwInterfaceName = gwInterfaceName
	udng.gwInterfaceIndex = link.Attrs().Index
	ipv4Mode, _ := udng.IPMode()
	if err := configureUplinkGatewayRPFilter(resolved, ipv4Mode); err != nil {
		return nil, fmt.Errorf("failed to configure reverse path filtering for Uplink %s on network %s: %w",
			udng.Uplink(), udng.GetNetworkName(), err)
	}

	if config.IsModeDPUHost() {
		return nil, nil
	}
	if resolved.bridgeName == "" {
		return nil, fmt.Errorf("uplink state for uplink %s on node %s has no OVS bridge",
			udng.Uplink(), udng.node.Name)
	}

	bridge, err := bridgeconfig.NewUnmanagedBridgeConfiguration(
		udng.ovsClient,
		resolved.bridgeName,
		resolved.hostInterfaceName,
		udng.node.Name,
		physicalNetworkName(udng.NetInfo),
		resolved.ipAddresses,
		resolved.macAddress,
	)
	if err != nil {
		return nil, newUplinkGatewayError(
			uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed,
			err,
		)
	}
	if err := bridge.ConfigureBridgePorts(); err != nil {
		return nil, newUplinkGatewayError(
			uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed,
			err,
		)
	}
	if err := configureUplinkStaticFDBEntry(bridge); err != nil {
		return nil, newUplinkGatewayError(
			uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed,
			err,
		)
	}
	return bridge, nil
}

func configureUplinkStaticFDBEntry(bridge *bridgeconfig.BridgeConfiguration) error {
	if config.IsModeDPUHost() {
		return nil
	}
	return util.SetStaticFDBEntry(
		bridge.GetBridgeName(),
		bridge.GetStaticFDBPort(),
		bridge.GetMAC(),
		config.Gateway.VLANID)
}

func (udng *UserDefinedNetworkGateway) resolveUplinkGateway(requireResolvedBridge bool) (*resolvedUplinkGateway, error) {
	stateName := uplinkutil.StateName(udng.Uplink(), udng.node.Name)
	state, err := uplinkutil.GetState(udng.uplinkStateLister, udng.Uplink(), udng.node.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("waiting for UplinkState %s for uplink %q on node %q",
				stateName, udng.Uplink(), udng.node.Name)
		}
		return nil, fmt.Errorf("failed to get UplinkState %s: %w", stateName, err)
	}
	return resolvedUplinkGatewayFromState(state, udng.Uplink(), udng.node.Name, requireResolvedBridge)
}

func configureUplinkGatewayRPFilter(resolved *resolvedUplinkGateway, ipv4Mode bool) error {
	if !ipv4Mode {
		return nil
	}

	interfaceName := uplinkGatewayInterfaceName(resolved)
	if interfaceName == "" {
		return nil
	}

	// Default-VRF Uplink networks keep the Uplink interface out of the UDN VRF,
	// while host service traffic can be marked and routed through the UDN table
	// to that interface. Replies return on the Uplink interface, but strict
	// rp_filter may validate them against a reverse route that does not use the
	// same interface. Loose mode matches the existing management port behavior.
	return util.SetRPFilterLooseModeForInterface(interfaceName)
}

// uplinkGatewayInterfaceName returns the Linux device used for host-side Uplink
// gateway configuration. In Full/DPU mode that device is the resolved OVS bridge;
// resolveUplinkGateway requires it to be ready before this helper is called. In
// DPU-host and non-OVS programming paths, the host interface is the local device.
// An empty return means no interface was resolved, so callers skip optional
// per-interface setup.
func uplinkGatewayInterfaceName(resolved *resolvedUplinkGateway) string {
	if config.IsModeDPU() || config.IsModeFull() {
		return resolved.bridgeName
	}
	return resolved.hostInterfaceName
}

func resolvedUplinkGatewayFromState(
	state *uplinkv1alpha1.UplinkState,
	uplinkName, nodeName string,
	requireResolvedBridge bool,
) (*resolvedUplinkGateway, error) {
	if err := uplinkutil.ValidateOVSBridgeState(state, uplinkName, nodeName, requireResolvedBridge); err != nil {
		return nil, err
	}
	return parseResolvedUplinkGateway(state)
}

func parseResolvedUplinkGateway(state *uplinkv1alpha1.UplinkState) (*resolvedUplinkGateway, error) {
	macAddress, err := net.ParseMAC(string(state.Status.MACAddress))
	if err != nil {
		return nil, fmt.Errorf("failed to parse UplinkState MAC address %q: %w",
			state.Status.MACAddress, err)
	}

	ipAddresses := make([]*net.IPNet, 0, len(state.Status.IPAddresses))
	for _, ipAddress := range state.Status.IPAddresses {
		ip, cidr, err := net.ParseCIDR(string(ipAddress))
		if err != nil {
			return nil, fmt.Errorf("failed to parse UplinkState IP address %q: %w",
				ipAddress, err)
		}
		cidr.IP = ip
		ipAddresses = append(ipAddresses, cidr)
	}
	if len(ipAddresses) == 0 {
		return nil, fmt.Errorf("uplink state has no gateway IP addresses")
	}

	defaultGateways := make([]net.IP, 0, len(state.Status.DefaultGateways))
	for _, defaultGateway := range state.Status.DefaultGateways {
		ip := net.ParseIP(string(defaultGateway))
		if ip == nil {
			return nil, fmt.Errorf("failed to parse UplinkState default gateway %q",
				defaultGateway)
		}
		defaultGateways = append(defaultGateways, ip)
	}

	bridgeName := ""
	if state.Status.OVSBridge != nil {
		bridgeName = state.Status.OVSBridge.Name
	}
	return &resolvedUplinkGateway{
		hostInterfaceName: string(state.Status.HostInterfaceName),
		bridgeName:        bridgeName,
		macAddress:        macAddress,
		ipAddresses:       ipAddresses,
		defaultGateways:   defaultGateways,
	}, nil
}

func physicalNetworkName(netInfo util.NetInfo) string {
	if netInfo.IsDefault() || (netInfo.IsPrimaryNetwork() && netInfo.Uplink() == "") {
		return types.PhysicalNetworkName
	}
	return netInfo.GetNetworkName()
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

// AddNetwork will be responsible to create all plumbings
// required by this UDN on the gateway side
func (udng *UserDefinedNetworkGateway) AddNetwork() error {
	var err error
	if udng.Uplink() == "" {
		err = udng.addNetwork()
	} else {
		err = udng.uplinkGatewayController.ReconcileNetwork(udng.NetInfo, udng.addNetwork)
	}
	if err != nil {
		return err
	}

	// Run gateway reconciliation only after initial programming and its
	// aggregate readiness update have both succeeded.
	udng.run()
	return nil
}

func (udng *UserDefinedNetworkGateway) addNetwork() error {
	if (config.IsModeDPU() || config.IsModeFull()) && udng.openflowManager == nil {
		return fmt.Errorf("openflow manager has not been provided for network: %s", udng.NetInfo.GetNetworkName())
	}
	udng.updateAdvertisementStatus()

	nodeSubnets, err := udng.getLocalSubnets()
	if err != nil {
		return fmt.Errorf("could not create management port for network %s, cannot determine subnets: %v",
			udng.GetNetworkName(), err)
	}

	// TBD-merge udng.node.Name, needs lower case?
	udng.mgmtPortController, err = managementport.NewUDNManagementPortController(udng.nodeLister, udng.node.Name, nodeSubnets, udng.NetInfo)
	if err != nil {
		return fmt.Errorf("could not create management port for network %s, UDN management port controller init failure: %v",
			udng.GetNetworkName(), err)
	}

	err = udng.mgmtPortController.Create()
	if err != nil {
		klog.Errorf("Create management port for network %s failed: %v", udng.GetNetworkName(), err)
		return fmt.Errorf("could not create management port for network %s, management port creation failure: %v",
			udng.GetNetworkName(), err)
	}

	uplinkBridge, err := udng.ensureUplinkGateway()
	if err != nil {
		return fmt.Errorf("failed to resolve Uplink gateway for network %s: %w",
			udng.GetNetworkName(), err)
	}

	if config.IsModeDPU() && udng.Uplink() != "" {
		vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
		if err = udng.ensureDPUVRF(); err != nil {
			return err
		}
		if err = udng.reconcileUplinkGatewayVRFSlave(vrfDeviceName); err != nil {
			return err
		}
	} else if config.IsModeDPUHost() || config.IsModeFull() {
		mgmtPortName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(udng.GetNetworkID()))
		mplink, err := util.LinkByName(mgmtPortName)
		if err != nil {
			return err
		}

		vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)
		udng.vrfTableId = vrfTableId

		vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
		routes, err := udng.computeRoutesForUDN(mplink)
		if err != nil {
			return fmt.Errorf("failed to compute routes for network %s, err: %v", udng.GetNetworkName(), err)
		}
		if err = udng.vrfManager.AddVRF(vrfDeviceName, mplink.Attrs().Name, uint32(udng.vrfTableId), nil); err != nil {
			return fmt.Errorf("could not add VRF %d for network %s, err: %v", udng.vrfTableId, udng.GetNetworkName(), err)
		}
		if err = udng.reconcileUplinkGatewayVRFSlave(vrfDeviceName); err != nil {
			return err
		}
		if udng.Uplink() != "" && udng.gwInterfaceName != "" && udng.gwInterfaceName != mplink.Attrs().Name {
			if err = setNodeMasqueradeIPOnExtBridge(udng.gwInterfaceName); err != nil {
				return fmt.Errorf("failed to configure service route source addresses on Uplink gateway interface %s for network %s: %w",
					udng.gwInterfaceName, udng.GetNetworkName(), err)
			}
			if err = addHostMACBindings(udng.gwInterfaceName); err != nil {
				return fmt.Errorf("failed to configure service next-hop neighbors on Uplink gateway interface %s for network %s: %w",
					udng.gwInterfaceName, udng.GetNetworkName(), err)
			}
		}
		if err = udng.addUDNManagementPortIPs(mplink); err != nil {
			return fmt.Errorf("unable to add management port IP(s) for link %s, for network %s: %w", mplink.Attrs().Name, udng.GetNetworkName(), err)
		}
		if err = udng.vrfManager.AddVRFRoutes(vrfDeviceName, routes); err != nil {
			return fmt.Errorf("could not add VRF %s routes for network %s, err: %v", vrfDeviceName, udng.GetNetworkName(), err)
		}
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		// create the iprules for this network
		if err = udng.updateUDNVRFIPRules(); err != nil {
			return fmt.Errorf("failed to update IP rules for network %s: %w", udng.GetNetworkName(), err)
		}

		if err = udng.updateAdvertisedUDNIsolationRules(); err != nil {
			return fmt.Errorf("failed to update isolation rules for network %s: %w", udng.GetNetworkName(), err)
		}

		if err = udng.updateUDNVRFIPRoute(); err != nil {
			return fmt.Errorf("failed to update ip routes for network %s: %w", udng.GetNetworkName(), err)
		}
	}

	if config.IsModeDPU() || config.IsModeFull() {
		var mgmtIPs []*net.IPNet
		for _, subnet := range nodeSubnets {
			mgmtIPs = append(mgmtIPs, udng.GetNodeManagementIP(subnet))
		}
		udng.openflowBridgeName = defaultOpenFlowBridgeSetName
		if uplinkBridge != nil {
			udng.openflowBridgeName = uplinkBridge.GetBridgeName()
		}
		err = udng.openflowManager.addNetwork(udng.openflowBridgeName, uplinkBridge, udng.NetInfo, nodeSubnets, mgmtIPs,
			udng.masqCTMark, udng.pktMark, udng.v6MasqIPs, udng.v4MasqIPs)
		if err != nil {
			return fmt.Errorf("could not add network %s: %v", udng.GetNetworkName(), err)
		}

		waiter := newStartupWaiterWithTimeout(waitForPatchPortTimeout)
		readyFunc := func() (bool, error) {
			if err := udng.openflowManager.setNetworkOfPatchPort(udng.openflowBridgeName, udng.GetNetworkName()); err != nil {
				klog.V(3).Infof("Failed to set network %s's openflow ports for bridge target %s; error: %v",
					udng.GetNetworkName(), openflowBridgeTargetDisplayName(udng.openflowBridgeName), err)
				return false, nil
			}
			return true, nil
		}
		postFunc := func() error {
			if err := udng.gateway.Reconcile(); err != nil {
				return fmt.Errorf("failed to reconcile flows on bridge for network %s; error: %v", udng.GetNetworkName(), err)
			}
			if udng.Uplink() != "" {
				if err := udng.syncUplinkBridgeFlows(); err != nil {
					return err
				}
			}
			return nil
		}
		waiter.AddWait(readyFunc, postFunc)
		if err := waiter.Wait(); err != nil {
			return newUplinkGatewayError(uplinkv1alpha1.UplinkStateReasonBridgeMappingFailed, err)
		}
	} else {
		if err := udng.gateway.Reconcile(); err != nil {
			return fmt.Errorf("failed to reconcile flows on bridge for network %s; error: %v", udng.GetNetworkName(), err)
		}
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		if err := udng.addMarkChain(); err != nil {
			return fmt.Errorf("failed to add the service masquerade chain: %w", err)
		}
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
	var err error
	if udng.Uplink() == "" {
		err = udng.delNetwork()
	} else {
		err = udng.uplinkGatewayController.DeleteNetwork(udng.NetInfo, udng.delNetwork)
	}
	if err != nil {
		return err
	}

	// Close only after cleanup and the aggregate readiness update succeed so a
	// status-update retry cannot close the channel twice.
	close(udng.reconcile)
	return nil
}

func (udng *UserDefinedNetworkGateway) delNetwork() error {
	var errs []error
	vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
	if config.IsModeDPUHost() || config.IsModeFull() {
		// delete the iprules for this network
		if err := udng.ruleManager.DeleteWithMetadata(udng.GetNetworkRuleMetadata()); err != nil {
			errs = append(errs, fmt.Errorf("unable to delete iprules for network %s, err: %v", udng.GetNetworkName(), err))
		}
	}
	// delete the VRF device for this network
	if err := udng.vrfManager.DeleteVRF(vrfDeviceName); err != nil {
		errs = append(errs, fmt.Errorf("unable to delete VRF device %s for network %s, err: %v", vrfDeviceName, udng.GetNetworkName(), err))
	}
	if config.IsModeDPU() || config.IsModeFull() {
		// delete the openflows for this network
		if udng.openflowManager != nil {
			if err := udng.openflowManager.delNetwork(udng.NetInfo, udng.openflowBridgeName); err != nil {
				errs = append(errs, fmt.Errorf("unable to delete openflows for network %s from bridge target %s, err: %w",
					udng.GetNetworkName(), openflowBridgeTargetDisplayName(udng.openflowBridgeName), err))
			}
		}
	}
	if udng.openflowManager != nil || config.IsModeDPUHost() {
		if err := udng.gateway.Reconcile(); err != nil {
			errs = append(errs, fmt.Errorf("failed to reconcile default gateway for network %s, err: %v", udng.GetNetworkName(), err))
		}
	}
	if udng.Uplink() != "" && (config.IsModeDPU() || config.IsModeFull()) {
		if _, err := udng.openflowManager.syncUplinkBridgeFlows(udng.openflowBridgeName); err != nil {
			errs = append(errs, newUplinkGatewayError(uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed, err))
		}
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		err := udng.deleteAdvertisedUDNIsolationRules()
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to remove advertised UDN isolation rules for network %s: %w", udng.GetNetworkName(), err))
		}

		if err := udng.delMarkChain(); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove mark chain for network %s, err: %v", udng.GetNetworkName(), err))
		}
	}
	// delete the management port interface for this network
	if udng.mgmtPortController != nil {
		err := udng.mgmtPortController.Delete()
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete UDN management port for network %s, err: %w", udng.GetNetworkName(), err))
		}
	}
	if len(errs) > 0 {
		return utilerrors.Join(errs...)
	}

	return nil
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

	klog.V(5).Infof("Add management port IPs on interface %s for network %s subnets %+v",
		mpLink.Attrs().Name, udng.GetNetworkName(), networkLocalSubnets)

	// extract management port IP from subnets and add it to link
	for _, subnet := range networkLocalSubnets {
		if config.IPv6Mode && utilnet.IsIPv6CIDR(subnet) || config.IPv4Mode && utilnet.IsIPv4CIDR(subnet) {
			ip := udng.GetNodeManagementIP(subnet)
			addrFlags := 0
			if udng.useNoPrefixRouteManagementPortIPs() {
				// In local gateway no-overlay mode, ingress to local UDN pods
				// must go through OVN, so prevent Linux from creating a
				// connected route that bypasses the UDN cluster router.
				addrFlags = unix.IFA_F_NOPREFIXROUTE
			}
			existingIP, err := util.LinkAddrGetIPNet(mpLink, ip.IP)
			if err != nil {
				return fmt.Errorf("failed to find management port IP from subnet %s on netdevice %s for network %s, err: %v",
					subnet, mpLink.Attrs().Name, udng.GetNetworkName(), err)
			}
			if existingIP != nil && existingIP.String() != ip.String() {
				err = util.LinkAddrDel(mpLink, existingIP)
				if err != nil {
					return fmt.Errorf("failed to delete stale management port IP %s from netdevice %s for network %s, err: %v",
						existingIP, mpLink.Attrs().Name, udng.GetNetworkName(), err)
				}
				existingIP = nil
			}
			if existingIP == nil {
				err = util.LinkAddrAdd(mpLink, ip, addrFlags, 0, 0)
			}
			if err != nil {
				return fmt.Errorf("failed to add management port IP from subnet %s to netdevice %s for network %s, err: %v",
					subnet, mpLink.Attrs().Name, udng.GetNetworkName(), err)
			}
		}
	}
	return nil
}

func (udng *UserDefinedNetworkGateway) useNoPrefixRouteManagementPortIPs() bool {
	return config.Gateway.Mode == config.GatewayModeLocal &&
		udng.TopologyType() == types.Layer3Topology &&
		udng.Transport() == types.NetworkTransportNoOverlay
}

// computeRoutesForUDN returns a list of routes programmed into a given UDN's VRF
// when adding new routes please leave a sample comment on how that route looks like
func (udng *UserDefinedNetworkGateway) computeRoutesForUDN(mpLink netlink.Link) ([]netlink.Route, error) {
	networkMTU := udng.NetInfo.MTU()
	if networkMTU == 0 {
		networkMTU = config.Default.MTU
	}
	var retVal []netlink.Route
	serviceRouteLinkIndex := udng.gwInterfaceIndex
	serviceRouteUsesUplink := udng.Uplink() != ""
	// Route1: Add serviceCIDR route: 10.96.0.0/16 via 169.254.169.4 dev breth0 mtu 1400
	// or via the selected Uplink gateway interface when one is configured.
	// This is necessary for UDN CNI and host-networked pods to talk to services.
	addedUplinkV6DummyNextHopRoute := false
	for _, serviceSubnet := range config.Kubernetes.ServiceCIDRs {
		serviceSubnet := serviceSubnet
		isV6 := utilnet.IsIPv6CIDR(serviceSubnet)
		gwIP := config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP
		if isV6 {
			gwIP = config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP
		}
		if serviceRouteUsesUplink && isV6 && !addedUplinkV6DummyNextHopRoute {
			// Uplink bridges do not carry the shared gateway bridge's
			// masquerade address. Add a per-VRF connected route so Linux
			// accepts the IPv6 dummy next hop on the selected Uplink link.
			retVal = append(retVal, netlink.Route{
				LinkIndex: serviceRouteLinkIndex,
				Dst:       util.GetIPNetFullMaskFromIP(gwIP),
				Scope:     netlink.SCOPE_LINK,
				Table:     udng.vrfTableId,
			})
			addedUplinkV6DummyNextHopRoute = true
		}
		serviceRoute := netlink.Route{
			LinkIndex: serviceRouteLinkIndex,
			Dst:       serviceSubnet,
			MTU:       networkMTU,
			Gw:        gwIP,
			Src:       config.Gateway.MasqueradeIPs.V4HostMasqueradeIP,
			Table:     udng.vrfTableId,
		}
		if isV6 {
			serviceRoute.Src = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP
		}
		if serviceRouteUsesUplink {
			serviceRoute.Flags = unix.RTNH_F_ONLINK
		}
		retVal = append(retVal, serviceRoute)
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
	// Local-gateway no-overlay Layer3 uses the node-local subnet instead:
	//   100.100.1.0/24 via 100.100.1.1 dev ovn-k8s-mp1
	networkLocalSubnets, err := udng.getLocalSubnets()
	if err != nil {
		return nil, err
	}
	for _, localSubnet := range networkLocalSubnets {
		gwIP := udng.GetNodeGatewayIP(localSubnet)
		if gwIP == nil {
			return nil, fmt.Errorf("unable to find gateway IP for network %s, subnet: %s", udng.GetNetworkName(), localSubnet)
		}
		if udng.useNoPrefixRouteManagementPortIPs() {
			// The management port address uses noprefixroute in local gateway
			// no-overlay mode. Add a host route to the UDN gateway before
			// installing routes through it.
			retVal = append(retVal, netlink.Route{
				LinkIndex: mpLink.Attrs().Index,
				Dst:       util.GetIPNetFullMaskFromIP(gwIP.IP),
				Scope:     netlink.SCOPE_LINK,
				Table:     udng.vrfTableId,
			})
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
			if udng.useNoPrefixRouteManagementPortIPs() {
				retVal = append(retVal, netlink.Route{
					LinkIndex: mpLink.Attrs().Index,
					Dst:       localSubnet,
					Gw:        gwIP.IP,
					Table:     udng.vrfTableId,
				})
				continue
			}
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
	hasV4Subnet, hasV6Subnet := udng.IPMode()
	if udng.Uplink() != "" {
		if len(udng.nextHops) == 0 {
			return nil, nil
		}
	}
	for _, nextHop := range udng.nextHops {
		isV6 := utilnet.IsIPv6(nextHop)
		if (isV6 && !hasV6Subnet) || (!isV6 && !hasV4Subnet) {
			continue
		}
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

func (udng *UserDefinedNetworkGateway) run() {
	go func() {
		for range udng.reconcile {
			reconcile := udng.doReconcile
			if udng.Uplink() != "" {
				reconcile = func() error {
					return udng.uplinkGatewayController.ReconcileNetwork(udng.NetInfo, udng.doReconcile)
				}
			}
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
				reconcile,
			)
			if err != nil {
				klog.Errorf("Failed to reconcile gateway for network %s: %v", udng.GetNetworkName(), err)
			}
		}
	}()
}

func (udng *UserDefinedNetworkGateway) Reconcile() {
	select {
	case udng.reconcile <- struct{}{}:
	default:
	}
}

func (udng *UserDefinedNetworkGateway) doReconcile() error {
	klog.Infof("Reconciling gateway with updates for UDN %s", udng.GetNetworkName())

	if config.IsModeDPU() || config.IsModeFull() {
		// shouldn't happen
		if udng.openflowManager == nil || udng.openflowManager.defaultBridge == nil {
			return fmt.Errorf("openflow manager with default bridge configuration has not been provided for network %s", udng.GetNetworkName())
		}
	}

	udng.updateAdvertisementStatus()

	if config.IsModeDPU() || config.IsModeFull() {
		// update bridge configuration
		netConfig := udng.openflowManager.getNetworkConfig(udng.NetInfo)
		if netConfig == nil {
			return fmt.Errorf("missing bridge configuration for network %s", udng.GetNetworkName())
		}
		netConfig.Advertised.Store(udng.isNetworkAdvertised)
	}

	if config.IsModeDPU() && udng.Uplink() != "" {
		vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
		if err := udng.reconcileUplinkGatewayVRFSlave(vrfDeviceName); err != nil {
			return err
		}
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
		if err := udng.reconcileUplinkGatewayVRFSlave(vrfDeviceName); err != nil {
			return err
		}

		if err := udng.updateUDNVRFIPRules(); err != nil {
			return fmt.Errorf("error while updating ip rule for UDN %s: %s", udng.GetNetworkName(), err)
		}

		if err := udng.updateUDNVRFIPRoute(); err != nil {
			return fmt.Errorf("error while updating ip route for UDN %s: %s", udng.GetNetworkName(), err)
		}
	}

	if config.IsModeDPU() || config.IsModeFull() {
		// add below OpenFlows based on the gateway mode and whether the network is advertised or not:
		// table=1, n_packets=0, n_bytes=0, priority=16,ip,nw_dst=128.192.0.2 actions=LOCAL (Both gateway modes)
		// table=1, n_packets=0, n_bytes=0, priority=15,ip,nw_dst=128.192.0.0/14 actions=output:3 (shared gateway mode)
		// necessary service isolation flows based on whether network is advertised or not
		if err := udng.openflowManager.updateBridgeFlowCache(udng.nodeIPManager.ListAddresses()); err != nil {
			return fmt.Errorf("error while updating logical flow for UDN %s: %s", udng.GetNetworkName(), err)
		}
		// let's sync these flows immediately
		udng.openflowManager.requestFlowSync()
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		if err := udng.updateAdvertisedUDNIsolationRules(); err != nil {
			return fmt.Errorf("error while updating advertised UDN isolation rules for network %s: %w", udng.GetNetworkName(), err)
		}
	}
	if udng.Uplink() != "" && (config.IsModeDPU() || config.IsModeFull()) {
		if err := udng.syncUplinkBridgeFlows(); err != nil {
			return err
		}
	}
	return nil
}

func (udng *UserDefinedNetworkGateway) syncUplinkBridgeFlows() error {
	found, err := udng.openflowManager.syncUplinkBridgeFlows(udng.openflowBridgeName)
	if err != nil {
		return newUplinkGatewayError(uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed, err)
	}
	if !found {
		return newUplinkGatewayError(uplinkv1alpha1.UplinkStateReasonGatewayProgrammingFailed,
			fmt.Errorf("uplink bridge %s not found", udng.openflowBridgeName))
	}
	return nil
}

func dpuUDNVRFRouteTableID(networkID int) int {
	return dpuUDNVRFRouteTableIDStart + networkID
}

func (udng *UserDefinedNetworkGateway) ensureDPUVRF() error {
	vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)
	vrfTableID := dpuUDNVRFRouteTableID(udng.GetNetworkID())
	udng.vrfTableId = vrfTableID
	if err := udng.vrfManager.AddVRF(vrfDeviceName, "", uint32(vrfTableID), nil); err != nil {
		return fmt.Errorf("could not add DPU VRF %d for network %s, err: %v",
			vrfTableID, udng.GetNetworkName(), err)
	}
	return nil
}

func (udng *UserDefinedNetworkGateway) shouldEnslaveUplinkGatewayToVRF() bool {
	return udng.Uplink() != "" &&
		udng.isNetworkAdvertised &&
		!udng.isNetworkAdvertisedToDefaultVRF
}

func (udng *UserDefinedNetworkGateway) reconcileUplinkGatewayVRFSlave(vrfDeviceName string) error {
	if udng.Uplink() == "" || udng.gwInterfaceName == "" {
		return nil
	}

	if udng.shouldEnslaveUplinkGatewayToVRF() {
		if config.IPv6Mode {
			if err := util.SetIPv6KeepAddrOnDownForInterface(udng.gwInterfaceName); err != nil {
				return newUplinkGatewayError(
					uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed,
					fmt.Errorf("could not preserve IPv6 addresses on Uplink gateway interface %s for network %s: %w",
						udng.gwInterfaceName, udng.GetNetworkName(), err),
				)
			}
		}
		if err := udng.vrfManager.AddVRFSlave(vrfDeviceName, udng.gwInterfaceName); err != nil {
			return newUplinkGatewayError(
				uplinkVRFAttachmentFailureReason(err),
				fmt.Errorf("could not add Uplink gateway interface %s to VRF %s for network %s: %w",
					udng.gwInterfaceName, vrfDeviceName, udng.GetNetworkName(), err),
			)
		}
		return nil
	}

	if err := udng.vrfManager.DeleteVRFSlave(vrfDeviceName, udng.gwInterfaceName); err != nil {
		return newUplinkGatewayError(
			uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed,
			fmt.Errorf("could not remove Uplink gateway interface %s from VRF %s for network %s: %w",
				udng.gwInterfaceName, vrfDeviceName, udng.GetNetworkName(), err),
		)
	}
	return nil
}

func uplinkVRFAttachmentFailureReason(err error) string {
	var conflict *vrfmanager.VRFSlaveConflictError
	if errors.As(err, &conflict) {
		return uplinkv1alpha1.UplinkStateReasonConfigurationConflict
	}
	return uplinkv1alpha1.UplinkStateReasonVRFAttachmentFailed
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
	udng.isNetworkAdvertised = util.IsPodNetworkAdvertisedAtNode(udng.NetInfo, udng.node.Name)
	udng.isNetworkAdvertisedToDefaultVRF = util.IsPodNetworkAdvertisedAtNodeDefaultVRF(udng.NetInfo, udng.node.Name)
}
