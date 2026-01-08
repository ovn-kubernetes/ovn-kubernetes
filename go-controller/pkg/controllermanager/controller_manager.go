package controllermanager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
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
	topologycontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/topology"
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
	udnNodeController  *topologycontroller.NodeController

	// eIPController programs OVN to support EgressIP
	eIPController *ovn.EgressIPController
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
		oc, err := ovn.NewLayer3UserDefinedNetworkController(cnci, nInfo, cm.networkManager.Interface(), cm.routeImportManager, cm.eIPController, cm.portCache)
		if err != nil {
			return nil, err
		}
		if cm.udnNodeController != nil {
			oc.SetNodeReconciler(cm.udnNodeController)
			oc.SetNodeAnnotationCache(cm.udnNodeController.AnnotationCache())
			oc.SetNodeHandlerRegistrar(func() {
				cm.udnNodeController.RegisterNetworkController(oc)
			})
		}
		return oc, nil
	case ovntypes.Layer2Topology:
		oc, err := ovn.NewLayer2UserDefinedNetworkController(cnci, nInfo, cm.networkManager.Interface(), cm.routeImportManager, cm.portCache, cm.eIPController)
		if err != nil {
			return nil, err
		}
		if cm.udnNodeController != nil {
			oc.SetNodeReconciler(cm.udnNodeController)
			oc.SetNodeAnnotationCache(cm.udnNodeController.AnnotationCache())
			oc.SetNodeHandlerRegistrar(func() {
				cm.udnNodeController.RegisterNetworkController(oc)
			})
		}
		return oc, nil
	case ovntypes.LocalnetTopology:
		oc := ovn.NewLocalnetUserDefinedNetworkController(cnci, nInfo, cm.networkManager.Interface())
		if cm.udnNodeController != nil {
			oc.SetNodeReconciler(cm.udnNodeController)
			oc.SetNodeAnnotationCache(cm.udnNodeController.AnnotationCache())
			oc.SetNodeHandlerRegistrar(func() {
				cm.udnNodeController.RegisterNetworkController(oc)
			})
		}
		return oc, nil
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// newDummyNetworkController creates a dummy network controller used to clean up specific network.
// role is the NetworkRoleExternalID from stale OVN entities (e.g. "primary" or "secondary") so that
// the dummy's netInfo.IsPrimaryNetwork() is correct for Layer2 gateway cleanup.
func (cm *ControllerManager) newDummyNetworkController(topoType, netName, role string) (networkmanager.NetworkController, error) {
	// Pass a shallow clone of the watch factory, this allows multiplexing
	// informers for user-defined Networks.
	cnci, err := cm.newCommonNetworkControllerInfo(cm.watchFactory.ShallowClone())
	if err != nil {
		return nil, fmt.Errorf("failed to create network controller info %w", err)
	}
	netInfo, _ := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: types.NetConf{Name: netName}, Topology: topoType, Role: role})
	switch topoType {
	case ovntypes.Layer3Topology:
		return ovn.NewLayer3UserDefinedNetworkController(cnci, netInfo, cm.networkManager.Interface(), cm.routeImportManager, cm.eIPController, cm.portCache)
	case ovntypes.Layer2Topology:
		return ovn.NewLayer2UserDefinedNetworkController(cnci, netInfo, cm.networkManager.Interface(), cm.routeImportManager, cm.portCache, cm.eIPController)
	case ovntypes.LocalnetTopology:
		return ovn.NewLocalnetUserDefinedNetworkController(cnci, netInfo, cm.networkManager.Interface()), nil
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// findAllUserDefinedNetworkLogicalEntities returns all OVN logical switches and
// routers that belong to user-defined networks (primary or secondary). Same
// predicate as original: entities have NetworkExternalID and NetworkRoleExternalID
// (TopologyExternalID always co-exists with NetworkExternalID per CleanupStaleNetworks).
// Caller reads role and topoType from entity ExternalIDs for dummy controller creation.
// Used on controller restart to remove stale entities for deleted UDNs.
func findAllUserDefinedNetworkLogicalEntities(nbClient libovsdbclient.Client) ([]*nbdb.LogicalSwitch,
	[]*nbdb.LogicalRouter, error) {

	belongsToUserDefinedNetwork := func(externalIDs map[string]string) bool {
		_, hasNetworkExternalID := externalIDs[ovntypes.NetworkExternalID]
		_, hasNetworkRoleExternalID := externalIDs[ovntypes.NetworkRoleExternalID]
		return hasNetworkExternalID && hasNetworkRoleExternalID
	}

	p1 := func(item *nbdb.LogicalSwitch) bool {
		return belongsToUserDefinedNetwork(item.ExternalIDs)
	}
	switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(nbClient, p1)
	if err != nil {
		klog.Errorf("Failed to get all logical switches of user-defined networks: %v", err)
		return nil, nil, err
	}
	p2 := func(item *nbdb.LogicalRouter) bool {
		return belongsToUserDefinedNetwork(item.ExternalIDs)
	}
	routers, err := libovsdbops.FindLogicalRoutersWithPredicate(nbClient, p2)
	if err != nil {
		klog.Errorf("Failed to get all logical routers of user-defined networks: %v", err)
		return nil, nil, err
	}
	return switches, routers, nil
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

	// Get all the existing user-defined network logical entities (primary and secondary).
	// For a given network, all switches/routers have the same role external ID (primary or secondary).
	switches, routers, err := findAllUserDefinedNetworkLogicalEntities(cm.nbClient)
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
		role := ls.ExternalIDs[ovntypes.NetworkRoleExternalID]
		if _, ok := staleNetworkControllers[netName]; ok {
			// already have a dummy controller for this network (from an earlier entity)
			continue
		}
		// Create dummy network controllers to clean up logical entities
		klog.V(5).Infof("Found stale %s network %s", topoType, netName)
		if oc, err := cm.newDummyNetworkController(topoType, netName, role); err == nil {
			staleNetworkControllers[netName] = oc
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
		role := lr.ExternalIDs[ovntypes.NetworkRoleExternalID]
		if _, ok := staleNetworkControllers[netName]; ok {
			// already have a dummy controller for this network (from an earlier entity)
			continue
		}
		// Create dummy network controllers to clean up logical entities
		klog.V(5).Infof("Found stale %s network %s", topoType, netName)
		if oc, err := cm.newDummyNetworkController(topoType, netName, role); err == nil {
			staleNetworkControllers[netName] = oc
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
			NADClient:            ovnClient.NetworkAttchDefClient,
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
		cm.udnNodeController = topologycontroller.NewNodeController(cm.watchFactory, cm.networkManager.Interface())
	}

	if util.IsRouteAdvertisementsEnabled() {
		if !config.OVNKubernetesFeature.EnableInterconnect {
			return nil, fmt.Errorf("RouteAdvertisements can only be used if Interconnect is enabled")
		}
		cm.routeImportManager = routeimport.New(config.Default.Zone, cm.nbClient)
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

	if err = cm.setTopologyType(); err != nil {
		return fmt.Errorf("failed to set layer2 topology type: %w", err)
	}

	cm.configureMetrics(cm.stopChan)

	err = cm.configureSCTPSupport()
	if err != nil {
		return err
	}

	cm.configureSvcTemplateSupport()

	err = cm.createACLLoggingMeter()
	if err != nil {
		return fmt.Errorf("failed to create acl logging meter: %w", err)
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

	if cm.networkManager != nil {
		if cm.udnNodeController != nil {
			if err = cm.udnNodeController.Start(); err != nil {
				return fmt.Errorf("failed to start UDN node topology controller: %v", err)
			}
		}
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
		if cm.udnNodeController != nil {
			cm.udnNodeController.Stop()
		}
		cm.networkManager.Stop()
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

func (cm *ControllerManager) setTopologyType() error {
	nodes, err := cm.kube.KClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("unable to get nodes from informer while setting topology type for layer2: %w", err)
	}
	// set it to true and check if all the nodes in the zone already have annotation
	config.Layer2UsesTransitRouter = true
	for _, node := range nodes.Items {
		if util.GetNodeZone(&node) == config.Default.Zone && node.Annotations[util.Layer2TopologyVersion] != util.TransitRouterTopoVersion {
			// at least one node doesn't have the annotation
			config.Layer2UsesTransitRouter = false
			break
		}
	}
	if config.Layer2UsesTransitRouter {
		// all nodes are already using new topology, no need to do anything extra
		return nil
	}

	// Transit router is not used yet, check if we can switch to the new topology now.
	// Find all primary layer2 switches and check if they have any running pods.
	layer2Switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(cm.nbClient, func(ls *nbdb.LogicalSwitch) bool {
		return ls.ExternalIDs[ovntypes.TopologyExternalID] == ovntypes.Layer2Topology &&
			ls.ExternalIDs[ovntypes.NetworkRoleExternalID] == ovntypes.NetworkRolePrimary
	})
	if err != nil {
		return fmt.Errorf("failed to find layer2 switches: %w", err)
	}
	for _, sw := range layer2Switches {
		hasRunningPods, err := cm.hasLocalPodsOnSwitch(sw)
		if err != nil {
			return fmt.Errorf("failed to check if there are running pods on switch %s: %w", sw.Name, err)
		}
		if hasRunningPods {
			klog.Infof("Network %s has running pods, not switching to transit router topology yet", sw.Name)
			return nil
		}
	}
	// we checked all layer2 switches and none of them has running pods
	// now make sure that cluster manager has upgraded and is assigning tunnel keys, otherwise new topology won't work

	// no layer2 switches means there are no layer2 networks (already handled, new ones are fine), so we won't find tunnel-keys annotations
	if len(layer2Switches) != 0 {
		existingNADs, err := cm.kube.NADClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("").List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to list existing NADs: %w", err)
		}
		clusterManagerReady := false
		for _, nad := range existingNADs.Items {
			if nad.Annotations[ovntypes.OvnNetworkTunnelKeysAnnotation] != "" {
				clusterManagerReady = true
				break
			}
		}
		if !clusterManagerReady {
			klog.Infof("Cluster manager is not ready to assign tunnel keys yet, not switching to transit router topology yet")
			return nil
		}
	}

	klog.Infof("Switching to transit router for layer2 networks")
	config.Layer2UsesTransitRouter = true
	return cm.setUDNLayer2NodeUsesTransitRouter(nodes)
}

func (cm *ControllerManager) hasLocalPodsOnSwitch(sw *nbdb.LogicalSwitch) (bool, error) {
	if len(sw.Ports) == 0 {
		return false, nil
	}

	ports, err := libovsdbops.FindLogicalSwitchPortWithPredicate(
		cm.nbClient,
		func(lsp *nbdb.LogicalSwitchPort) bool {
			return lsp.Type == "" &&
				lsp.ExternalIDs["pod"] == "true" &&
				slices.Contains(sw.Ports, lsp.UUID)
		})
	if err != nil {
		return false, err
	}
	if len(ports) > 0 {
		return true, nil
	}
	return false, nil
}

func (cm *ControllerManager) setUDNLayer2NodeUsesTransitRouter(nodeList *corev1.NodeList) error {
	for _, node := range nodeList.Items {
		if util.GetNodeZone(&node) == config.Default.Zone {
			if err := cm.kube.SetAnnotationsOnNode(node.Name, map[string]interface{}{
				util.Layer2TopologyVersion: util.TransitRouterTopoVersion}); err != nil {
				return fmt.Errorf("failed to set annotation %s on node %s: %w", util.Layer2TopologyVersion, node.Name, err)
			}
		}
	}
	return nil
}
