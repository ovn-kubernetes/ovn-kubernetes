package ovn

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	zoneinterconnect "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

// method/structure shared by all layer 2 network controller, including localnet and layer2 network controllres.

// BaseLayer2UserDefinedNetworkController structure holds per-network fields and network specific
// configuration for secondary layer2/localnet network controller
type BaseLayer2UserDefinedNetworkController struct {
	BaseUserDefinedNetworkController
}

// stop gracefully stops the controller, and delete all logical entities for this network if requested
func (oc *BaseLayer2UserDefinedNetworkController) stop() {
	if oc.stopChan == nil {
		klog.Infof("Secondary %s network controller of network %s is already stopped", oc.TopologyType(), oc.GetNetworkName())
		return
	}
	klog.Infof("Stop secondary %s network controller of network %s", oc.TopologyType(), oc.GetNetworkName())
	close(oc.stopChan)
	oc.stopChan = nil
	oc.cancelableCtx.Cancel()
	oc.wg.Wait()

	if oc.ipamClaimsHandler != nil {
		oc.watchFactory.RemoveIPAMClaimsHandler(oc.ipamClaimsHandler)
	}
	if oc.netPolicyHandler != nil {
		oc.watchFactory.RemovePolicyHandler(oc.netPolicyHandler)
	}
	if oc.multiNetPolicyHandler != nil {
		oc.watchFactory.RemoveMultiNetworkPolicyHandler(oc.multiNetPolicyHandler)
	}
	if oc.podHandler != nil {
		oc.watchFactory.RemovePodHandler(oc.podHandler)
	}
	if oc.nodeHandler != nil {
		oc.watchFactory.RemoveNodeHandler(oc.nodeHandler)
	}
	if oc.namespaceHandler != nil {
		oc.watchFactory.RemoveNamespaceHandler(oc.namespaceHandler)
	}
	if oc.routeImportManager != nil && config.Gateway.Mode == config.GatewayModeShared {
		oc.routeImportManager.ForgetNetwork(oc.GetNetworkName())
	}
}

// cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *BaseLayer2UserDefinedNetworkController) cleanup() error {
	netName := oc.GetNetworkName()
	klog.Infof("Delete OVN logical entities for network %s", netName)
	// delete layer 2 logical switches
	ops, err := libovsdbops.DeleteLogicalSwitchesWithPredicateOps(oc.nbClient, nil,
		func(item *nbdb.LogicalSwitch) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting switches of network %s: %v", netName, err)
	}

	ops, err = cleanupPolicyLogicalEntities(oc.nbClient, ops, oc.controllerName)
	if err != nil {
		return err
	}

	ops, err = libovsdbops.DeleteQoSesWithPredicateOps(oc.nbClient, ops,
		func(item *nbdb.QoS) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting QoSes of network %s: %v", netName, err)
	}

	ops, err = libovsdbops.DeleteAddressSetsWithPredicateOps(oc.nbClient, ops,
		func(item *nbdb.AddressSet) bool {
			return item.ExternalIDs[types.NetworkExternalID] == netName
		})
	if err != nil {
		return fmt.Errorf("failed to get ops for deleting address sets of network %s: %v", netName, err)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to deleting switches of network %s: %v", netName, err)
	}

	return nil
}

func (oc *BaseLayer2UserDefinedNetworkController) run() error {
	// WatchNamespaces() should be started first because it has no other
	// dependencies, and WatchNodes() depends on it
	if err := oc.WatchNamespaces(); err != nil {
		return err
	}

	if err := oc.WatchNodes(); err != nil {
		return err
	}

	// when on IC, it will be the NetworkController that returns the IPAMClaims
	// IPs back to the pool
	if oc.allocatesPodAnnotation() && oc.allowPersistentIPs() {
		// WatchIPAMClaims should be started before WatchPods to prevent OVN-K
		// master assigning IPs to pods without taking into account the persistent
		// IPs set aside for the IPAMClaims
		if err := oc.WatchIPAMClaims(); err != nil {
			return err
		}
	}

	if err := oc.WatchPods(); err != nil {
		return err
	}

	if util.IsMultiNetworkPoliciesSupportEnabled() && !oc.IsPrimaryNetwork() {
		// WatchMultiNetworkPolicy depends on WatchPods and WatchNamespaces
		if err := oc.WatchMultiNetworkPolicy(); err != nil {
			return err
		}
	}

	if oc.IsPrimaryNetwork() {
		// WatchNetworkPolicy depends on WatchPods and WatchNamespaces
		if err := oc.WatchNetworkPolicy(); err != nil {
			return err
		}
	}

	// start NetworkQoS controller if feature is enabled
	if config.OVNKubernetesFeature.EnableNetworkQoS {
		err := oc.newNetworkQoSController()
		if err != nil {
			return fmt.Errorf("unable to create network qos controller, err: %w", err)
		}
		oc.wg.Add(1)
		go func() {
			defer oc.wg.Done()
			// Until we have scale issues in future let's spawn only one thread
			oc.nqosController.Run(1, oc.stopChan)
		}()
	}

	// Add ourselves to the route import manager
	if oc.routeImportManager != nil && config.Gateway.Mode == config.GatewayModeShared {
		err := oc.routeImportManager.AddNetwork(oc.GetNetInfo())
		if err != nil {
			return fmt.Errorf("failed to add network %s to the route import manager: %v", oc.GetNetworkName(), err)
		}
	}
	return nil
}

func (oc *BaseLayer2UserDefinedNetworkController) initializeLogicalSwitch(switchName string, clusterSubnets []config.CIDRNetworkEntry, excludeSubnets, reservedSubnets []*net.IPNet, clusterLoadBalancerGroupUUID, switchLoadBalancerGroupUUID string) (*nbdb.LogicalSwitch, error) {
	logicalSwitch := nbdb.LogicalSwitch{
		Name:        switchName,
		ExternalIDs: util.GenerateExternalIDsForSwitchOrRouter(oc.GetNetInfo()),
	}

	hostSubnets := make([]*net.IPNet, 0, len(clusterSubnets))
	for _, clusterSubnet := range clusterSubnets {
		subnet := clusterSubnet.CIDR
		hostSubnets = append(hostSubnets, subnet)
		if utilnet.IsIPv6CIDR(subnet) {
			logicalSwitch.OtherConfig = map[string]string{"ipv6_prefix": subnet.IP.String()}
		} else {
			logicalSwitch.OtherConfig = map[string]string{"subnet": subnet.String()}
		}
	}

	if oc.isLayer2Interconnect() {
		tunnelKey := zoneinterconnect.BaseTransitSwitchTunnelKey + oc.GetNetworkID()
		if config.Layer2UsesTransitRouter && oc.IsPrimaryNetwork() {
			if len(oc.GetTunnelKeys()) != 2 {
				return nil, fmt.Errorf("layer2 network %s with transit router enabled requires exactly 2 tunnel keys, got: %v", oc.GetNetworkName(), oc.GetTunnelKeys())
			}
			tunnelKey = oc.GetTunnelKeys()[0]
		}
		err := oc.zoneICHandler.AddTransitSwitchConfig(&logicalSwitch, tunnelKey)
		if err != nil {
			return nil, err
		}
	}

	if clusterLoadBalancerGroupUUID != "" && switchLoadBalancerGroupUUID != "" {
		logicalSwitch.LoadBalancerGroup = []string{clusterLoadBalancerGroupUUID, switchLoadBalancerGroupUUID}
	}

	err := libovsdbops.CreateOrUpdateLogicalSwitch(oc.nbClient, &logicalSwitch)
	if err != nil {
		return nil, fmt.Errorf("failed to create logical switch %+v: %v", logicalSwitch, err)
	}

	if err = oc.lsManager.AddOrUpdateSwitch(switchName, hostSubnets, reservedSubnets, excludeSubnets...); err != nil {
		return nil, err
	}

	return &logicalSwitch, nil
}

func (oc *BaseLayer2UserDefinedNetworkController) addUpdateNodeEvent(node *corev1.Node) error {
	if oc.isLocalZoneNode(node) {
		return oc.addUpdateLocalNodeEvent(node)
	}
	return oc.addUpdateRemoteNodeEvent(node)
}

func (oc *BaseLayer2UserDefinedNetworkController) addUpdateLocalNodeEvent(node *corev1.Node) error {
	_, present := oc.localZoneNodes.LoadOrStore(node.Name, true)

	if !present {
		// process all pods so they are reconfigured as local
		errs := oc.addAllPodsOnNode(node.Name)
		if errs != nil {
			err := utilerrors.Join(errs...)
			return err
		}
	}

	return nil
}

func (oc *BaseLayer2UserDefinedNetworkController) addUpdateRemoteNodeEvent(node *corev1.Node) error {
	_, present := oc.localZoneNodes.Load(node.Name)

	if present {
		err := oc.deleteNodeEvent(node)
		if err != nil {
			return err
		}

		// process all pods so they are reconfigured as remote
		errs := oc.addAllPodsOnNode(node.Name)
		if errs != nil {
			err = utilerrors.Join(errs...)
			return err
		}
	}

	return nil
}

func (oc *BaseLayer2UserDefinedNetworkController) deleteNodeEvent(node *corev1.Node) error {
	oc.localZoneNodes.Delete(node.Name)
	return nil
}

func (oc *BaseLayer2UserDefinedNetworkController) syncNodes(nodes []interface{}) error {
	for _, tmp := range nodes {
		node, ok := tmp.(*corev1.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", tmp)
		}

		// Add the node to the foundNodes only if it belongs to the local zone.
		if oc.isLocalZoneNode(node) {
			oc.localZoneNodes.Store(node.Name, true)
		}
	}

	return nil
}

func (oc *BaseLayer2UserDefinedNetworkController) syncIPAMClaims(ipamClaims []interface{}) error {
	switchName, err := oc.getExpectedSwitchName(dummyPod())
	if err != nil {
		return err
	}
	return oc.ipamClaimsReconciler.Sync(ipamClaims, oc.lsManager.ForSwitch(switchName))
}

func dummyPod() *corev1.Pod {
	return &corev1.Pod{Spec: corev1.PodSpec{NodeName: ""}}
}
