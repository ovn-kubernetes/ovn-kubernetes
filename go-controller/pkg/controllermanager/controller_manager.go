package controllermanager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics/recorders"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/observability"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/routeimport"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// ControllerManager structure is the object manages all controllers
type ControllerManager struct {
	client       clientset.Interface
	kube         *kube.KubeOVN
	watchFactory *factory.WatchFactory
	podRecorder  *metrics.PodRecorder
	// event recorder used to post events to k8s
	recorder record.EventRecorder
	// libovsdb northbound client interface
	nbClient libovsdbclient.Client
	// libovsdb southbound client interface
	sbClient libovsdbclient.Client
	// has SCTP support
	SCTPSupport bool
	// Supports multicast?
	multicastSupport bool
	// Supports OVN Template Load Balancers?
	svcTemplateSupport bool

	stopChan                 chan struct{}
	wg                       *sync.WaitGroup
	portCache                *ovn.PortCache
	defaultNetworkController networkmanager.BaseNetworkController

	// networkManager creates and deletes network controllers
	networkManager     networkmanager.Controller
	routeImportManager routeimport.Controller

	// eIPController programs OVN to support EgressIP
	eIPController *ovn.EgressIPController

	podTracker      networkmanager.TrackerController
	egressIPTracker networkmanager.TrackerController
}

func (cm *ControllerManager) NewNetworkController(nInfo util.NetInfo) (networkmanager.NetworkController, error) {
	// Pass a shallow clone of the watch factory, this allows multiplexing
	// informers for user-defined networks.
	cnci, err := cm.newCommonNetworkControllerInfo(cm.watchFactory.ShallowClone())
	if err != nil {
		return nil, fmt.Errorf("failed to create network controller info %w", err)
	}
	topoType := nInfo.TopologyType()
	switch topoType {
	case ovntypes.Layer3Topology:
		return ovn.NewLayer3UserDefinedNetworkController(cnci, nInfo, cm.networkManager.Interface(), cm.routeImportManager, cm.eIPController, cm.portCache, cm)
	case ovntypes.Layer2Topology:
		return ovn.NewLayer2UserDefinedNetworkController(cnci, nInfo, cm.networkManager.Interface(), cm.routeImportManager, cm.portCache, cm.eIPController)
	case ovntypes.LocalnetTopology:
		return ovn.NewLocalnetUserDefinedNetworkController(cnci, nInfo, cm.networkManager.Interface()), nil
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// newDummyNetworkController creates a dummy network controller used to clean up specific network
func (cm *ControllerManager) newDummyNetworkController(topoType, netName string) (networkmanager.NetworkController, error) {
	// Pass a shallow clone of the watch factory, this allows multiplexing
	// informers for user-defined Networks.
	cnci, err := cm.newCommonNetworkControllerInfo(cm.watchFactory.ShallowClone())
	if err != nil {
		return nil, fmt.Errorf("failed to create network controller info %w", err)
	}
	netInfo, _ := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: types.NetConf{Name: netName}, Topology: topoType})
	switch topoType {
	case ovntypes.Layer3Topology:
		return ovn.NewLayer3UserDefinedNetworkController(cnci, netInfo, cm.networkManager.Interface(), cm.routeImportManager, cm.eIPController, cm.portCache, cm)
	case ovntypes.Layer2Topology:
		return ovn.NewLayer2UserDefinedNetworkController(cnci, netInfo, cm.networkManager.Interface(), cm.routeImportManager, cm.portCache, cm.eIPController)
	case ovntypes.LocalnetTopology:
		return ovn.NewLocalnetUserDefinedNetworkController(cnci, netInfo, cm.networkManager.Interface()), nil
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// Find all the OVN logical switches/routers for the secondary networks
func findAllSecondaryNetworkLogicalEntities(nbClient libovsdbclient.Client) ([]*nbdb.LogicalSwitch,
	[]*nbdb.LogicalRouter, error) {

	belongsToSecondaryNetwork := func(externalIDs map[string]string) bool {
		_, hasNetworkExternalID := externalIDs[ovntypes.NetworkExternalID]
		networkRole, hasNetworkRoleExternalID := externalIDs[ovntypes.NetworkRoleExternalID]
		return hasNetworkExternalID && hasNetworkRoleExternalID && networkRole == ovntypes.NetworkRoleSecondary
	}

	p1 := func(item *nbdb.LogicalSwitch) bool {
		return belongsToSecondaryNetwork(item.ExternalIDs)
	}
	nodeSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(nbClient, p1)
	if err != nil {
		klog.Errorf("Failed to get all logical switches of secondary network error: %v", err)
		return nil, nil, err
	}
	p2 := func(item *nbdb.LogicalRouter) bool {
		return belongsToSecondaryNetwork(item.ExternalIDs)
	}
	clusterRouters, err := libovsdbops.FindLogicalRoutersWithPredicate(nbClient, p2)
	if err != nil {
		klog.Errorf("Failed to get all distributed logical routers: %v", err)
		return nil, nil, err
	}
	return nodeSwitches, clusterRouters, nil
}

func (cm *ControllerManager) GetDefaultNetworkController() networkmanager.ReconcilableNetworkController {
	return cm.defaultNetworkController
}

func (cm *ControllerManager) CleanupStaleNetworks(validNetworks ...util.NetInfo) error {
	existingNetworksMap := map[string]string{}
	validNetworksSubnets := sets.New[string]()
	for _, network := range validNetworks {
		existingNetworksMap[network.GetNetworkName()] = network.TopologyType()
		for _, subnet := range network.Subnets() {
			validNetworksSubnets.Insert(subnet.CIDR.String())
		}
	}

	// Get all the existing secondary networks and its logical entities
	switches, routers, err := findAllSecondaryNetworkLogicalEntities(cm.nbClient)
	if err != nil {
		return err
	}

	staleNetworkControllers := map[string]networkmanager.NetworkController{}
	for _, ls := range switches {
		netName := ls.ExternalIDs[ovntypes.NetworkExternalID]
		// TopologyExternalID always co-exists with NetworkExternalID
		topoType := ls.ExternalIDs[ovntypes.TopologyExternalID]
		if existingNetworksMap[netName] == topoType {
			// network still exists, no cleanup to do
			continue
		}
		// Create dummy network controllers to clean up logical entities
		klog.V(5).Infof("Found stale %s network %s", topoType, netName)
		if oc, err := cm.newDummyNetworkController(topoType, netName); err == nil {
			staleNetworkControllers[netName] = oc
			continue
		}
	}
	for _, lr := range routers {
		netName := lr.ExternalIDs[ovntypes.NetworkExternalID]
		// TopologyExternalID always co-exists with NetworkExternalID
		topoType := lr.ExternalIDs[ovntypes.TopologyExternalID]
		if existingNetworksMap[netName] == topoType {
			// network still exists, no cleanup to do
			continue
		}
		// Create dummy network controllers to clean up logical entities
		klog.V(5).Infof("Found stale %s network %s", topoType, netName)
		if oc, err := cm.newDummyNetworkController(topoType, netName); err == nil {
			staleNetworkControllers[netName] = oc
			continue
		}
	}

	for netName, oc := range staleNetworkControllers {
		klog.Infof("Cleanup entities for stale network %s", netName)
		err = oc.Cleanup()
		if err != nil {
			klog.Errorf("Failed to delete stale OVN logical entities for network %s: %v", netName, err)
		}
	}

	// Remove stale subnets from the advertised networks address set used for isolation
	// NOTE: network reconciliation will take care of removing the subnets for existing networks that are no longer
	// advertised.
	addressSetFactory := addressset.NewOvnAddressSetFactory(cm.nbClient, config.IPv4Mode, config.IPv6Mode)
	advertisedSubnets, err := addressSetFactory.GetAddressSet(ovn.GetAdvertisedNetworkSubnetsAddressSetDBIDs())
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return fmt.Errorf("failed to get advertised subnets addresset %s: %w", ovn.GetAdvertisedNetworkSubnetsAddressSetDBIDs(), err)
	}
	if advertisedSubnets != nil {
		v4AdvertisedSubnets, v6AdvertisedSubnets := advertisedSubnets.GetAddresses()
		var invalidSubnets []string
		for _, subnet := range append(v4AdvertisedSubnets, v6AdvertisedSubnets...) {
			if !validNetworksSubnets.Has(subnet) {
				klog.Infof("Cleanup stale advertised subnet: %q", subnet)
				invalidSubnets = append(invalidSubnets, subnet)
			}
		}

		if err := advertisedSubnets.DeleteAddresses(invalidSubnets); err != nil {
			klog.Errorf("Failed to delete stale advertised subnets: %v", invalidSubnets)
		}
	}
	return nil
}

// NewControllerManager creates a new ovnkube controller manager to manage all the controller for all networks
func NewControllerManager(ovnClient *util.OVNClientset, wf *factory.WatchFactory,
	libovsdbOvnNBClient libovsdbclient.Client, libovsdbOvnSBClient libovsdbclient.Client,
	recorder record.EventRecorder, wg *sync.WaitGroup) (*ControllerManager, error) {
	podRecorder := metrics.NewPodRecorder()

	stopCh := make(chan struct{})
	cm := &ControllerManager{
		client: ovnClient.KubeClient,
		kube: &kube.KubeOVN{
			Kube:                 kube.Kube{KClient: ovnClient.KubeClient},
			ANPClient:            ovnClient.ANPClient,
			EIPClient:            ovnClient.EgressIPClient,
			EgressFirewallClient: ovnClient.EgressFirewallClient,
			CloudNetworkClient:   ovnClient.CloudNetworkClient,
			EgressServiceClient:  ovnClient.EgressServiceClient,
			APBRouteClient:       ovnClient.AdminPolicyRouteClient,
			EgressQoSClient:      ovnClient.EgressQoSClient,
			IPAMClaimsClient:     ovnClient.IPAMClaimsClient,
			NetworkQoSClient:     ovnClient.NetworkQoSClient,
		},
		stopChan:         stopCh,
		watchFactory:     wf,
		recorder:         recorder,
		nbClient:         libovsdbOvnNBClient,
		sbClient:         libovsdbOvnSBClient,
		podRecorder:      &podRecorder,
		portCache:        ovn.NewPortCache(stopCh),
		wg:               wg,
		multicastSupport: config.EnableMulticast,
	}

	var err error
	cm.networkManager = networkmanager.Default()
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.networkManager, err = networkmanager.NewForZone(config.Default.Zone, cm, wf)
		if err != nil {
			return nil, err
		}
	}

	if util.IsRouteAdvertisementsEnabled() {
		if !config.OVNKubernetesFeature.EnableInterconnect {
			return nil, fmt.Errorf("RouteAdvertisements can only be used if Interconnect is enabled")
		}
		cm.routeImportManager = routeimport.New(config.Default.Zone, cm.nbClient)
	}

	if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		cm.podTracker = networkmanager.NewPodTrackerController("zone-pod-tracker", wf, cm.networkManager.Interface(), cm.OnNetworkRefChange)
		if config.OVNKubernetesFeature.EnableEgressIP {
			cm.egressIPTracker = networkmanager.NewEgressIPTrackerController(wf, cm.networkManager.Interface(), cm.OnNetworkRefChange)
		}
	}

	return cm, nil
}

func (cm *ControllerManager) configureSCTPSupport() error {
	hasSCTPSupport, err := util.DetectSCTPSupport()
	if err != nil {
		return err
	}

	if !hasSCTPSupport {
		klog.Warningf("SCTP unsupported by this version of OVN. Kubernetes service creation with SCTP will not work ")
	} else {
		klog.Info("SCTP support detected in OVN")
	}
	cm.SCTPSupport = hasSCTPSupport
	return nil
}

func (cm *ControllerManager) configureSvcTemplateSupport() {
	if !config.OVNKubernetesFeature.EnableServiceTemplateSupport {
		cm.svcTemplateSupport = false
	} else if _, _, err := util.RunOVNNbctl("--columns=_uuid", "list", "Chassis_Template_Var"); err != nil {
		klog.Warningf("Version of OVN in use does not support Chassis_Template_Var. " +
			"Disabling Templates Support")
		cm.svcTemplateSupport = false
	} else {
		cm.svcTemplateSupport = true
	}
}

func (cm *ControllerManager) configureMetrics(stopChan <-chan struct{}) {
	metrics.RegisterOVNKubeControllerPerformance(cm.nbClient)
	metrics.RegisterOVNKubeControllerFunctional(stopChan)
	metrics.RunTimestamp(stopChan, cm.sbClient, cm.nbClient)
	metrics.MonitorIPSec(cm.nbClient)
}

func (cm *ControllerManager) createACLLoggingMeter() error {
	band := &nbdb.MeterBand{
		Action: ovntypes.MeterAction,
		Rate:   config.Logging.ACLLoggingRateLimit,
	}
	ops, err := libovsdbops.CreateMeterBandOps(cm.nbClient, nil, band)
	if err != nil {
		return fmt.Errorf("can't create meter band %v: %v", band, err)
	}

	meterFairness := true
	meter := &nbdb.Meter{
		Name: ovntypes.OvnACLLoggingMeter,
		Fair: &meterFairness,
		Unit: ovntypes.PacketsPerSecond,
	}
	ops, err = libovsdbops.CreateOrUpdateMeterOps(cm.nbClient, ops, meter, []*nbdb.MeterBand{band},
		&meter.Bands, &meter.Fair, &meter.Unit)
	if err != nil {
		return fmt.Errorf("can't create meter %v: %v", meter, err)
	}

	_, err = libovsdbops.TransactAndCheck(cm.nbClient, ops)
	if err != nil {
		return fmt.Errorf("can't transact ACL logging meter: %v", err)
	}

	return nil
}

// newCommonNetworkControllerInfo creates and returns the common networkController info
func (cm *ControllerManager) newCommonNetworkControllerInfo(wf *factory.WatchFactory) (*ovn.CommonNetworkControllerInfo, error) {
	return ovn.NewCommonNetworkControllerInfo(cm.client, cm.kube, wf, cm.recorder, cm.nbClient,
		cm.sbClient, cm.podRecorder, cm.SCTPSupport, cm.multicastSupport, cm.svcTemplateSupport)
}

// initDefaultNetworkController creates the controller for default network
func (cm *ControllerManager) initDefaultNetworkController(observManager *observability.Manager) error {
	cnci, err := cm.newCommonNetworkControllerInfo(cm.watchFactory)
	if err != nil {
		return fmt.Errorf("failed to create common network controller info: %w", err)
	}
	defaultController, err := ovn.NewDefaultNetworkController(cnci, observManager, cm.networkManager.Interface(), cm.routeImportManager, cm.eIPController, cm.portCache)
	if err != nil {
		return err
	}
	// Make sure we only set defaultNetworkController in case of no error,
	// otherwise we would initialize the interface with a nil implementation
	// which is not the same as nil interface.
	cm.defaultNetworkController = defaultController
	return nil
}

// Start the ovnkube controller
func (cm *ControllerManager) Start(ctx context.Context) error {
	klog.Info("Starting the ovnkube controller")

	// Make sure that the ovnkube-controller zone matches with the Northbound db zone.
	// Wait for 300s before giving up
	maxTimeout := 300 * time.Second
	klog.Infof("Waiting up to %s for NBDB zone to match: %s", maxTimeout, config.Default.Zone)
	start := time.Now()
	var zone string
	var err1 error
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, maxTimeout, true, func(_ context.Context) (bool, error) {
		zone, err1 = libovsdbutil.GetNBZone(cm.nbClient)
		if err1 != nil {
			return false, nil
		}
		if config.Default.Zone != zone {
			err1 = fmt.Errorf("config zone %s different from NBDB zone %s", config.Default.Zone, zone)
			return false, nil
		}
		return true, nil
	})

	if err != nil {
		return fmt.Errorf("failed to start default ovnkube-controller - OVN NBDB zone %s does not match the configured zone %q: errors: %v, %v",
			zone, config.Default.Zone, err, err1)
	}
	klog.Infof("NBDB zone sync took: %s", time.Since(start))

	err = cm.watchFactory.Start()
	if err != nil {
		return err
	}

	// Wait for one node to have the zone we want to manage, otherwise there is no point in configuring NBDB.
	// Really this covers a use case where a node is going from local -> remote, but has not yet annotated itself.
	// In this case ovnkube-controller on this remote node will treat the node as remote, and then once the annotation
	// appears will convert it to local, which may or may not clean up DB resources correctly.
	klog.Infof("Waiting up to %s for a node to have %q zone", maxTimeout, config.Default.Zone)
	start = time.Now()
	err = wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, maxTimeout, true, func(_ context.Context) (bool, error) {
		nodes, err := cm.watchFactory.GetNodes()
		if err != nil {
			klog.Errorf("Unable to get nodes from informer while waiting for node zone sync")
			return false, nil
		}
		if len(nodes) == 0 {
			klog.Infof("No nodes in cluster: waiting for a node to have %q zone is not needed", config.Default.Zone)
			return true, nil
		}
		for _, node := range nodes {
			if util.GetNodeZone(node) == config.Default.Zone {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("failed to start default network controller - while waiting for any node to have zone: %q, error: %v",
			config.Default.Zone, err)
	}
	klog.Infof("Waiting for node in zone sync took: %s", time.Since(start))

	cm.configureMetrics(cm.stopChan)

	err = cm.configureSCTPSupport()
	if err != nil {
		return err
	}

	cm.configureSvcTemplateSupport()

	err = cm.createACLLoggingMeter()
	if err != nil {
		return nil
	}

	if config.Metrics.EnableConfigDuration {
		// with k=10,
		//  for a cluster with 10 nodes, measurement of 1 in every 100 requests
		//  for a cluster with 100 nodes, measurement of 1 in every 1000 requests
		recorders.GetConfigDurationRecorder().Run(cm.nbClient, cm.watchFactory, 10, time.Second*5, cm.stopChan)
	}
	cm.podRecorder.Run(cm.sbClient, cm.stopChan)

	if config.OVNKubernetesFeature.EnableEgressIP {
		cm.eIPController = ovn.NewEIPController(cm.nbClient, cm.kube, cm.watchFactory, cm.recorder, cm.portCache, cm.networkManager.Interface(),
			addressset.NewOvnAddressSetFactory(cm.nbClient, config.IPv4Mode, config.IPv6Mode), config.IPv4Mode, config.IPv6Mode, zone, ovn.DefaultNetworkControllerName)
		// FIXME(martinkennelly): remove when EIP controller is fully extracted from from DNC and started here. Ensure SyncLocalNodeZonesCache is re-enabled in EIP controller.
		if err = cm.eIPController.SyncLocalNodeZonesCache(); err != nil {
			klog.Warningf("Failed to sync EgressIP controllers local node node cache: %v", err)
		}
	}

	var observabilityManager *observability.Manager
	if config.OVNKubernetesFeature.EnableObservability {
		observabilityManager = observability.NewManager(cm.nbClient)
		if err = observabilityManager.Init(); err != nil {
			return fmt.Errorf("failed to init observability manager: %w", err)
		}
	} else {
		err = observability.Cleanup(cm.nbClient)
		if err != nil {
			klog.Warningf("Observability cleanup failed, expected if not all Samples ware deleted yet: %v", err)
		}
	}

	if util.IsNetworkSegmentationSupportEnabled() {
		addressSetFactory := addressset.NewOvnAddressSetFactory(cm.nbClient, config.IPv4Mode, config.IPv6Mode)
		go func() {
			if err := udnenabledsvc.NewController(cm.nbClient, addressSetFactory, cm.watchFactory.ServiceCoreInformer(),
				config.Default.UDNAllowedDefaultServices).Run(cm.stopChan); err != nil {
				klog.Errorf("UDN enabled service controller failed: %v", err)
			}
		}()
	}

	err = cm.initDefaultNetworkController(observabilityManager)
	if err != nil {
		return fmt.Errorf("failed to init default network controller: %v", err)
	}

	if util.IsRouteAdvertisementsEnabled() {
		if err := cm.configureAdvertisedNetworkIsolation(); err != nil {
			return fmt.Errorf("failed to initialize advertised network isolation: %w", err)
		}
	}

	if cm.podTracker != nil {
		if err = cm.podTracker.Start(); err != nil {
			return fmt.Errorf("failed to start pod tracker: %w", err)
		}
	}

	if cm.egressIPTracker != nil {
		if err = cm.egressIPTracker.Start(); err != nil {
			return fmt.Errorf("failed to start egress ip tracker: %w", err)
		}
	}

	if cm.networkManager != nil {
		if err = cm.networkManager.Start(); err != nil {
			return fmt.Errorf("failed to start NAD Controller :%v", err)
		}
	}

	if cm.routeImportManager != nil {
		err = cm.routeImportManager.Start()
		if err != nil {
			return fmt.Errorf("failed to start route import: %v", err)
		}
	}

	err = cm.defaultNetworkController.Start(ctx)
	if err != nil {
		return fmt.Errorf("failed to start default network controller: %v", err)
	}

	return nil
}

// Stop gracefully stops all managed controllers
func (cm *ControllerManager) Stop() {
	// stop metric recorders
	close(cm.stopChan)

	// stop the default network controller
	if cm.defaultNetworkController != nil {
		cm.defaultNetworkController.Stop()
	}

	// stop the NAD controller
	if cm.networkManager != nil {
		cm.networkManager.Stop()
	}

	// stop pod tracker
	if cm.podTracker != nil {
		cm.podTracker.Stop()
	}

	// stop egressIP tracker
	if cm.egressIPTracker != nil {
		cm.egressIPTracker.Stop()
	}

	if cm.routeImportManager != nil {
		cm.routeImportManager.Stop()
	}
}

func (cm *ControllerManager) Reconcile(_ string, _, _ util.NetInfo) error {
	return nil
}

func (cm *ControllerManager) configureAdvertisedNetworkIsolation() error {
	addressSetFactory := addressset.NewOvnAddressSetFactory(cm.nbClient, config.IPv4Mode, config.IPv6Mode)
	_, err := addressSetFactory.EnsureAddressSet(ovn.GetAdvertisedNetworkSubnetsAddressSetDBIDs())
	return err
}

// Filter out NADs that are not part of a UDN/CUDN running on our node
func (cm *ControllerManager) Filter(nad *nettypes.NetworkAttachmentDefinition) (bool, error) {
	ownerRef := metav1.GetControllerOf(nad)
	if ownerRef == nil {
		return false, nil
	}

	// if this isn't per-node zone controller in IC, don't filter
	if config.Default.Zone == ovntypes.OvnDefaultZone {
		return false, nil
	}

	if ownerRef.Kind != "ClusterUserDefinedNetwork" && ownerRef.Kind != "UserDefinedNetwork" {
		return false, nil
	}

	nodes, err := cm.watchFactory.GetNodes()
	if err != nil {
		return false, fmt.Errorf("failed to list nodes for controller manager filtering: %w", err)
	}

	for _, node := range nodes {
		if util.GetNodeZone(node) != config.Default.Zone {
			continue
		}
		if cm.NodeHasNAD(node.Name, util.GetNADName(nad.Namespace, nad.Name)) {
			return false, nil
		}
	}

	return true, nil
}

// OnNetworkRefChange is a callback function used to signal an action to this controller when
// a network needs to be added or removed or just updated
func (cm *ControllerManager) OnNetworkRefChange(node, nadNamespacedName string, active bool) {
	klog.V(4).Infof("Network change for zone controller triggered by pod/egress IP events "+
		"on node: %s, NAD: %s, active: %t", node, nadNamespacedName, active)

	namespace, name, err := cache.SplitMetaNamespaceKey(nadNamespacedName)
	if err != nil {
		klog.Errorf("Failed splitting key %q, falling back to normal network reconcile: %v", name, err)
		// fallback to regular reconcile
		cm.networkManager.Interface().Reconcile(nadNamespacedName)
		return
	}

	nad, err := cm.watchFactory.NADInformer().Lister().NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil {
		klog.Errorf("Failed to find NAD %q in informer, falling back to normal network reconcile: %v", nadNamespacedName, err)
		// fallback to regular reconcile
		cm.networkManager.Interface().Reconcile(nadNamespacedName)
		return
	}

	nadNetwork, err := util.ParseNADInfo(nad)
	if err != nil || nadNetwork == nil {
		klog.Errorf("Failed to parse NAD %q info, falling back to normal network reconcile: %v", nadNamespacedName, err)
		// fallback to regular reconcile
		cm.networkManager.Interface().Reconcile(nadNamespacedName)
		return
	}

	n, err := cm.watchFactory.GetNode(node)
	if err != nil {
		klog.Errorf("Failed to find node %q in informer, falling back to normal network reconcile: %v", node, err)
	}
	// remove only networks local on our node
	isActive := active || util.GetNodeZone(n) != config.Default.Zone
	cm.networkManager.Interface().ForceReconcile(nadNamespacedName, nadNetwork.GetNetworkName(), isActive)
}

func (cm *ControllerManager) NodeHasNAD(node, nad string) bool {
	if cm.podTracker != nil && cm.podTracker.NodeHasNAD(node, nad) {
		return true
	}
	if cm.egressIPTracker != nil && cm.egressIPTracker.NodeHasNAD(node, nad) {
		return true
	}
	return false
}
