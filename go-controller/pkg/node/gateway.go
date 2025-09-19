package node

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/safchain/ethtool"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/egressip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

// Gateway responds to Service and Endpoint K8s events
// and programs OVN gateway functionality.
// It may also spawn threads to ensure the flow tables
// are kept in sync
type Gateway interface {
	informer.ServiceAndEndpointsEventHandler
	Init(<-chan struct{}, *sync.WaitGroup) error
	Start() error
	GetGatewayBridgeIface() string
	GetGatewayIface() string
	SetDefaultGatewayBridgeMAC(addr net.HardwareAddr)
	SetDefaultPodNetworkAdvertised(bool)
	SetDefaultBridgeGARPDropFlows(bool)
	Reconcile() error
}

type gateway struct {
	// loadBalancerHealthChecker is a health check server for load-balancer type services
	loadBalancerHealthChecker informer.ServiceAndEndpointsEventHandler
	// portClaimWatcher is for reserving ports for virtual IPs allocated by the cluster on the host
	portClaimWatcher informer.ServiceEventHandler
	// nodePortWatcherIptables is used in Shared GW mode to handle nodePort IPTable rules
	nodePortWatcherIptables informer.ServiceEventHandler
	// nodePortWatcher is used in Local+Shared GW modes to handle nodePort flows in shared OVS bridge
	nodePortWatcher      informer.ServiceAndEndpointsEventHandler
	openflowManager      *openflowManager
	nodeIPManager        *addressManager
	bridgeEIPAddrManager *egressip.BridgeEIPAddrManager
	initFunc             func() error
	readyFunc            func() (bool, error)

	servicesRetryFramework *retry.RetryFramework

	watchFactory *factory.WatchFactory // used for retry
	stopChan     <-chan struct{}
	wg           *sync.WaitGroup

	nextHops []net.IP
}

func (g *gateway) AddService(svc *corev1.Service) error {
	var err error
	var errors []error

	if g.portClaimWatcher != nil {
		if err = g.portClaimWatcher.AddService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	if g.loadBalancerHealthChecker != nil {
		if err = g.loadBalancerHealthChecker.AddService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcher != nil {
		if err = g.nodePortWatcher.AddService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcherIptables != nil {
		if err = g.nodePortWatcherIptables.AddService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.Join(errors...)
}

func (g *gateway) UpdateService(old, new *corev1.Service) error {
	var err error
	var errors []error

	if g.portClaimWatcher != nil {
		if err = g.portClaimWatcher.UpdateService(old, new); err != nil {
			errors = append(errors, err)
		}
	}
	if g.loadBalancerHealthChecker != nil {
		if err = g.loadBalancerHealthChecker.UpdateService(old, new); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcher != nil {
		if err = g.nodePortWatcher.UpdateService(old, new); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcherIptables != nil {
		if err = g.nodePortWatcherIptables.UpdateService(old, new); err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.Join(errors...)
}

func (g *gateway) DeleteService(svc *corev1.Service) error {
	var err error
	var errors []error

	if g.portClaimWatcher != nil {
		if err = g.portClaimWatcher.DeleteService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	if g.loadBalancerHealthChecker != nil {
		if err = g.loadBalancerHealthChecker.DeleteService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcher != nil {
		if err = g.nodePortWatcher.DeleteService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcherIptables != nil {
		if err = g.nodePortWatcherIptables.DeleteService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.Join(errors...)
}

func (g *gateway) SyncServices(objs []interface{}) error {
	var err error
	klog.Infof("Starting gateway service sync")
	start := time.Now()
	if g.portClaimWatcher != nil {
		err = g.portClaimWatcher.SyncServices(objs)
	}
	if err == nil && g.loadBalancerHealthChecker != nil {
		err = g.loadBalancerHealthChecker.SyncServices(objs)
	}
	if err == nil && g.nodePortWatcher != nil {
		err = g.nodePortWatcher.SyncServices(objs)
	}
	if err == nil && g.nodePortWatcherIptables != nil {
		err = g.nodePortWatcherIptables.SyncServices(objs)
	}
	if err != nil {
		return fmt.Errorf("gateway sync services failed: %v", err)
	}
	klog.Infof("Gateway service sync done. Time taken: %s", time.Since(start))
	return nil
}

func (g *gateway) AddEndpointSlice(epSlice *discovery.EndpointSlice) error {
	var err error
	var errors []error

	if g.loadBalancerHealthChecker != nil {
		// Filter out objects without the default serviceName label to exclude mirrored EndpointSlices
		// Only default EndpointSlices contain the discovery.LabelServiceName label
		if !util.IsNetworkSegmentationSupportEnabled() || epSlice.Labels[discovery.LabelServiceName] != "" {
			if err = g.loadBalancerHealthChecker.AddEndpointSlice(epSlice); err != nil {
				errors = append(errors, err)
			}
		}
	}
	if g.nodePortWatcher != nil {
		if err = g.nodePortWatcher.AddEndpointSlice(epSlice); err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.Join(errors...)

}

func (g *gateway) UpdateEndpointSlice(oldEpSlice, newEpSlice *discovery.EndpointSlice) error {
	var err error
	var errors []error

	if g.loadBalancerHealthChecker != nil {
		// Filter out objects without the default serviceName label to exclude mirrored EndpointSlices
		// Only default EndpointSlices contain the discovery.LabelServiceName label
		if !util.IsNetworkSegmentationSupportEnabled() || newEpSlice.Labels[discovery.LabelServiceName] != "" {
			if err = g.loadBalancerHealthChecker.UpdateEndpointSlice(oldEpSlice, newEpSlice); err != nil {
				errors = append(errors, err)
			}
		}
	}
	if g.nodePortWatcher != nil {
		if err = g.nodePortWatcher.UpdateEndpointSlice(oldEpSlice, newEpSlice); err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.Join(errors...)

}

func (g *gateway) DeleteEndpointSlice(epSlice *discovery.EndpointSlice) error {
	var err error
	var errors []error

	// Check if the namespace still exists before proceeding
	_, err = g.watchFactory.GetNamespace(epSlice.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(5).Infof("Namespace %s not found, skipping endpointslice %s deletion (namespace was deleted)", epSlice.Namespace, epSlice.Name)
			return nil // Return success to skip retry
		}
		// For other errors, continue with the deletion process
		klog.Warningf("Failed to check namespace %s existence: %v, continuing with endpointslice deletion", epSlice.Namespace, err)
	}

	if g.loadBalancerHealthChecker != nil {
		// Filter out objects without the default serviceName label to exclude mirrored EndpointSlices
		// Only default EndpointSlices contain the discovery.LabelServiceName label
		if !util.IsNetworkSegmentationSupportEnabled() || epSlice.Labels[discovery.LabelServiceName] != "" {
			if err = g.loadBalancerHealthChecker.DeleteEndpointSlice(epSlice); err != nil {
				errors = append(errors, err)
			}
		}
	}
	if g.nodePortWatcher != nil {
		if err = g.nodePortWatcher.DeleteEndpointSlice(epSlice); err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.Join(errors...)
}

func (g *gateway) AddEgressIP(eip *egressipv1.EgressIP) error {
	if !util.IsNetworkSegmentationSupportEnabled() || !config.OVNKubernetesFeature.EnableInterconnect || config.Gateway.Mode == config.GatewayModeDisabled {
		return nil
	}
	isSyncRequired, err := g.bridgeEIPAddrManager.AddEgressIP(eip)
	if err != nil {
		return err
	}
	if isSyncRequired {
		if err = g.Reconcile(); err != nil {
			return fmt.Errorf("failed to sync gateway: %v", err)
		}
	}
	return nil
}

func (g *gateway) UpdateEgressIP(oldEIP, newEIP *egressipv1.EgressIP) error {
	if !util.IsNetworkSegmentationSupportEnabled() || !config.OVNKubernetesFeature.EnableInterconnect || config.Gateway.Mode == config.GatewayModeDisabled {
		return nil
	}
	isSyncRequired, err := g.bridgeEIPAddrManager.UpdateEgressIP(oldEIP, newEIP)
	if err != nil {
		return err
	}
	if isSyncRequired {
		if err = g.Reconcile(); err != nil {
			return fmt.Errorf("failed to sync gateway: %v", err)
		}
	}
	return nil
}

func (g *gateway) DeleteEgressIP(eip *egressipv1.EgressIP) error {
	if !util.IsNetworkSegmentationSupportEnabled() || !config.OVNKubernetesFeature.EnableInterconnect || config.Gateway.Mode == config.GatewayModeDisabled {
		return nil
	}
	isSyncRequired, err := g.bridgeEIPAddrManager.DeleteEgressIP(eip)
	if err != nil {
		return err
	}
	if isSyncRequired {
		if err = g.Reconcile(); err != nil {
			return fmt.Errorf("failed to sync gateway: %v", err)
		}
	}
	return nil
}

func (g *gateway) SyncEgressIP(eips []interface{}) error {
	if !util.IsNetworkSegmentationSupportEnabled() || !config.OVNKubernetesFeature.EnableInterconnect || config.Gateway.Mode == config.GatewayModeDisabled {
		return nil
	}
	if err := g.bridgeEIPAddrManager.SyncEgressIP(eips); err != nil {
		return err
	}
	if err := g.Reconcile(); err != nil {
		return fmt.Errorf("failed to sync gateway: %v", err)
	}
	return nil
}

func (g *gateway) Init(stopChan <-chan struct{}, wg *sync.WaitGroup) error {
	g.stopChan = stopChan
	g.wg = wg

	var err error

	g.servicesRetryFramework = g.newRetryFrameworkNode(factory.ServiceForGatewayType)
	if _, err = g.servicesRetryFramework.WatchResource(); err != nil {
		return fmt.Errorf("gateway init failed to start watching services: %v", err)
	}

	endpointSlicesRetryFramework := g.newRetryFrameworkNode(factory.EndpointSliceForGatewayType)
	if _, err = endpointSlicesRetryFramework.WatchResource(); err != nil {
		return fmt.Errorf("gateway init failed to start watching endpointslices: %v", err)
	}

	if config.OVNKubernetesFeature.EnableEgressIP {
		eipRetryFramework := g.newRetryFrameworkNode(factory.EgressIPType)
		if _, err = eipRetryFramework.WatchResource(); err != nil {
			return fmt.Errorf("gateway init failed to start watching EgressIPs: %v", err)
		}
	}

	return nil
}

func (g *gateway) Start() error {
	if g.openflowManager != nil {
		klog.Info("Spawning Conntrack Rule Check Thread")
		err := g.openflowManager.updateBridgeFlowCache(g.nodeIPManager.ListAddresses())
		if err != nil {
			return fmt.Errorf("failed to update bridge flow cache: %w", err)
		}
		g.openflowManager.Run(g.stopChan, g.wg)
	}

	if g.nodeIPManager != nil {
		g.nodeIPManager.Run(g.stopChan, g.wg)
	}

	return nil
}

// sets up an uplink interface for UDP Generic Receive Offload forwarding as part of
// the EnableUDPAggregation feature.
func setupUDPAggregationUplink(ifname string) error {
	e, err := ethtool.NewEthtool()
	if err != nil {
		return fmt.Errorf("failed to initialize ethtool: %v", err)
	}
	defer e.Close()

	err = e.Change(ifname, map[string]bool{
		"rx-udp-gro-forwarding": true,
	})
	if err != nil {
		return fmt.Errorf("could not enable UDP offload features on %q: %v", ifname, err)
	}

	return nil
}

func gatewayInitInternal(nodeName, gwIntf, egressGatewayIntf string, gwNextHops []net.IP, nodeSubnets, gwIPs []*net.IPNet,
	advertised bool, nodeAnnotator kube.Annotator) (
	*bridgeconfig.BridgeConfiguration, *bridgeconfig.BridgeConfiguration, error) {
	gatewayBridge, err := bridgeconfig.NewBridgeConfiguration(gwIntf, nodeName, types.PhysicalNetworkName, nodeSubnets, gwIPs, advertised)
	if err != nil {
		return nil, nil, fmt.Errorf("bridge for interface failed for %s: %w", gwIntf, err)
	}
	var egressGWBridge *bridgeconfig.BridgeConfiguration
	if egressGatewayIntf != "" {
		egressGWBridge, err = bridgeconfig.NewBridgeConfiguration(egressGatewayIntf, nodeName, types.PhysicalNetworkExGwName, nodeSubnets, nil, false)
		if err != nil {
			return nil, nil, fmt.Errorf("bridge for interface failed for %s: %w", egressGatewayIntf, err)
		}
	}

	chassisID, err := util.GetNodeChassisID()
	if err != nil {
		return nil, nil, err
	}

	// Set annotation that determines if options:gateway_mtu shall be set for this node.
	enableGatewayMTU := true
	if config.Gateway.DisablePacketMTUCheck {
		klog.Warningf("Config option disable-pkt-mtu-check is set to true. " +
			"options:gateway_mtu will be disabled on gateway routers. " +
			"IP fragmentation or large TCP/UDP payloads may not be forwarded correctly.")
		enableGatewayMTU = false
	} else {
		chkPktLengthSupported, err := util.DetectCheckPktLengthSupport(gatewayBridge.GetBridgeName())
		if err != nil {
			return nil, nil, err
		}
		if !chkPktLengthSupported {
			klog.Warningf("OVS does not support check_packet_length action. " +
				"options:gateway_mtu will be disabled on gateway routers. " +
				"IP fragmentation or large TCP/UDP payloads may not be forwarded correctly.")
			enableGatewayMTU = false
		} else {
			/* This is a work around. In order to have the most optimal performance, the packet MTU check should be
			 * disabled when OVS HW Offload is enabled on the node. The reason is that OVS HW Offload does not support
			 * packet MTU checks properly without the offload support for sFlow.
			 * The patches for sFlow in OvS: https://patchwork.ozlabs.org/project/openvswitch/list/?series=290804
			 * As of writing these offload support patches for sFlow are in review.
			 * TODO: This workaround should be removed once the offload support for sFlow patches are merged upstream OvS.
			 */
			ovsHardwareOffloadEnabled, err := util.IsOvsHwOffloadEnabled()
			if err != nil {
				return nil, nil, err
			}
			if ovsHardwareOffloadEnabled {
				klog.Warningf("OVS hardware offloading is enabled. " +
					"options:gateway_mtu will be disabled on gateway routers for performance reasons. " +
					"IP fragmentation or large TCP/UDP payloads may not be forwarded correctly.")
				enableGatewayMTU = false
			}
		}
	}
	if err := util.SetGatewayMTUSupport(nodeAnnotator, enableGatewayMTU); err != nil {
		return nil, nil, err
	}

	if config.Default.EnableUDPAggregation {
		err = setupUDPAggregationUplink(gatewayBridge.GetUplinkName())
		if err == nil && egressGWBridge != nil {
			err = setupUDPAggregationUplink(egressGWBridge.GetUplinkName())
		}
		if err != nil {
			klog.Warningf("Could not enable UDP packet aggregation on uplink interface (aggregation will be disabled): %v", err)
			config.Default.EnableUDPAggregation = false
		}
	}

	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		// Set static FDB entry for sharedGW MAC.
		// If `GatewayIfaceRep` port is present, use it instead of LOCAL (bridge name).
		gwport := gatewayBridge.GetBridgeName()                           // Default is LOCAL port for the bridge.
		if repPort := gatewayBridge.GetGatewayIfaceRep(); repPort != "" { // We have an accelerated switchdev device for GW.
			gwport = repPort
		}

		if err := util.SetStaticFDBEntry(gatewayBridge.GetBridgeName(), gwport, gatewayBridge.GetMAC()); err != nil {
			return nil, nil, err
		}
	}

	l3GwConfig := util.L3GatewayConfig{
		Mode:           config.Gateway.Mode,
		ChassisID:      chassisID,
		BridgeID:       gatewayBridge.GetBridgeName(),
		InterfaceID:    gatewayBridge.GetInterfaceID(),
		MACAddress:     gatewayBridge.GetMAC(),
		IPAddresses:    gatewayBridge.GetIPs(),
		NextHops:       gwNextHops,
		NodePortEnable: config.Gateway.NodeportEnable,
		VLANID:         &config.Gateway.VLANID,
	}
	if egressGWBridge != nil {
		l3GwConfig.EgressGWInterfaceID = egressGWBridge.GetInterfaceID()
		l3GwConfig.EgressGWMACAddress = egressGWBridge.GetMAC()
		l3GwConfig.EgressGWIPAddresses = egressGWBridge.GetIPs()
	}

	err = util.SetL3GatewayConfig(nodeAnnotator, &l3GwConfig)
	return gatewayBridge, egressGWBridge, err
}

func (g *gateway) GetGatewayBridgeIface() string {
	return g.openflowManager.getDefaultBridgeName()
}

func (g *gateway) GetGatewayIface() string {
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		if g.openflowManager == nil {
			return ""
		}
		return g.openflowManager.defaultBridge.GetGatewayIface()
	} else {
		return config.Gateway.Interface
	}
}

// SetDefaultGatewayBridgeMAC updates the mac address for the OFM used to render flows with
func (g *gateway) SetDefaultGatewayBridgeMAC(macAddr net.HardwareAddr) {
	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		return
	}
	g.openflowManager.setDefaultBridgeMAC(macAddr)
	klog.Infof("Default gateway bridge MAC address updated to %s", macAddr)
}

func (g *gateway) SetDefaultPodNetworkAdvertised(isPodNetworkAdvertised bool) {
	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		return
	}
	g.openflowManager.defaultBridge.GetNetworkConfig(types.DefaultNetworkName).Advertised.Store(isPodNetworkAdvertised)
}

func (g *gateway) GetDefaultPodNetworkAdvertised() bool {
	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		return false
	}
	return g.openflowManager.defaultBridge.GetNetworkConfig(types.DefaultNetworkName).Advertised.Load()
}

// SetDefaultBridgeGARPDropFlows will enable flows to drop GARPs if the openflow
// manager has been initialized.
func (g *gateway) SetDefaultBridgeGARPDropFlows(isDropped bool) {
	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		return
	}

	if g.openflowManager == nil {
		return
	}
	g.openflowManager.setDefaultBridgeGARPDrop(isDropped)
}

// Reconcile handles triggering updates to different components of a gateway, like OFM, Services
func (g *gateway) Reconcile() error {
	klog.Info("Reconciling gateway with updates")
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		if g.openflowManager != nil {
			if err := g.openflowManager.updateBridgeFlowCache(g.nodeIPManager.ListAddresses()); err != nil {
				return err
			}
			// let's sync these flows immediately
			g.openflowManager.requestFlowSync()
		}
	}
	// TBD updateSNATRules() gets node host-cidr by accessing gateway.nodeIPManager, which does not
	// exist in dpu-host mode.
	if config.OvnKubeNode.Mode == types.NodeModeFull {
		err := g.updateSNATRules()
		if err != nil {
			return err
		}
	}
	// Services create OpenFlow flows as well, need to update them all
	if g.servicesRetryFramework != nil {
		if errs := g.addAllServices(); errs != nil {
			err := utilerrors.Join(errs...)
			return err
		}
	}
	return nil
}

func (g *gateway) addAllServices() []error {
	errs := []error{}
	svcs, err := g.watchFactory.GetServices()
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, svc := range svcs {
			svc := *svc
			klog.V(5).Infof("Adding service %s/%s to retryServices", svc.Namespace, svc.Name)
			err = g.servicesRetryFramework.AddRetryObjWithAddNoBackoff(&svc)
			if err != nil {
				err = fmt.Errorf("failed to add service %s/%s to retry framework: %w", svc.Namespace, svc.Name, err)
				errs = append(errs, err)
			}
		}
	}
	g.servicesRetryFramework.RequestRetryObjs()
	return errs
}

func (g *gateway) updateSNATRules() error {
	subnets := util.IPsToNetworkIPs(g.nodeIPManager.mgmtPort.GetAddresses()...)

	if config.Gateway.Mode != config.GatewayModeLocal {
		return delLocalGatewayPodSubnetNFTRules()
	}

	return addOrUpdateLocalGatewayPodSubnetNFTRules(g.GetDefaultPodNetworkAdvertised(), subnets...)
}
