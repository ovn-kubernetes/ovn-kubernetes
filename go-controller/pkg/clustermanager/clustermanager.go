package clustermanager

import (
	"context"
	"fmt"
	"net"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/dnsnameresolver"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/egressservice"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/endpointslicemirror"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/routeadvertisements"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/status_manager"
	udncontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork"
	udntemplate "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/unidling"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// ClusterManager structure is the object which manages the cluster nodes.
// It creates a default network controller for the default network and a
// user-defined network cluster controller manager to manage the multi networks.
type ClusterManager struct {
	client                      clientset.Interface
	defaultNetClusterController *networkClusterController
	zoneClusterController       *zoneClusterController
	wf                          *factory.WatchFactory
	udnClusterManager           *userDefinedNetworkClusterManager
	// Controller used for programming node allocation for egress IP
	// The OVN DB setup is handled by egressIPZoneController that runs in ovnkube-controller
	eIPC                          *egressIPClusterController
	egressServiceController       *egressservice.Controller
	endpointSliceMirrorController *endpointslicemirror.Controller
	// Controller used for maintaining dns name resolver objects
	dnsNameResolverController *dnsnameresolver.Controller
	// Controller for managing user-defined-network CRD
	userDefinedNetworkController *udncontroller.Controller
	// event recorder used to post events to k8s
	recorder record.EventRecorder

	// unique identity for clusterManager running on different ovnkube-cluster-manager instance,
	// used for leader election
	identity      string
	statusManager *status_manager.StatusManager

	// networkManager creates and deletes network controllers
	networkManager networkmanager.Controller

	raController *routeadvertisements.Controller

	podTracker      networkmanager.TrackerController
	egressIPTracker networkmanager.TrackerController
}

// NewClusterManager creates a new cluster manager to manage the cluster nodes.
func NewClusterManager(
	ovnClient *util.OVNClusterManagerClientset,
	wf *factory.WatchFactory,
	identity string,
	recorder record.EventRecorder,
) (*ClusterManager, error) {

	wf = wf.ShallowClone()
	defaultNetClusterController := newDefaultNetworkClusterController(&util.DefaultNetInfo{}, ovnClient, wf, recorder)

	zoneClusterController, err := newZoneClusterController(ovnClient, wf)
	if err != nil {
		return nil, fmt.Errorf("failed to create zone cluster controller, err : %w", err)
	}

	cm := &ClusterManager{
		client:                      ovnClient.KubeClient,
		defaultNetClusterController: defaultNetClusterController,
		zoneClusterController:       zoneClusterController,
		wf:                          wf,
		recorder:                    recorder,
		identity:                    identity,
		statusManager:               status_manager.NewStatusManager(wf, ovnClient),
	}

	cm.networkManager = networkmanager.Default()
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.networkManager, err = networkmanager.NewForCluster(cm, wf, ovnClient, recorder)
		if err != nil {
			return nil, err
		}

		cm.udnClusterManager, err = newUserDefinedNetworkClusterManager(ovnClient, wf, cm.networkManager.Interface(), recorder)
		if err != nil {
			return nil, err
		}
	}

	if config.OVNKubernetesFeature.EnableEgressIP {
		cm.eIPC = newEgressIPController(ovnClient, wf, recorder)
	}

	if config.OVNKubernetesFeature.EnableEgressService {
		// TODO: currently an ugly hack to pass the (copied) isReachable func to the egress service controller
		// without touching the egressIP controller code too much before the Controller object is created.
		// This will be removed once we consolidate all of the healthchecks to a different place and have
		// the controllers query a universal cache instead of creating multiple goroutines that do the same thing.
		timeout := config.OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout
		hcPort := config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort
		isReachable := func(nodeName string, mgmtIPs []net.IP, healthClient healthcheck.EgressIPHealthClient) bool {
			// Check if we need to do node reachability check
			if timeout == 0 {
				return true
			}

			if hcPort == 0 {
				return isReachableLegacy(nodeName, mgmtIPs, timeout)
			}

			return isReachableViaGRPC(mgmtIPs, healthClient, hcPort, timeout)
		}

		cm.egressServiceController, err = egressservice.NewController(ovnClient, wf, isReachable)
		if err != nil {
			return nil, err
		}
	}
	if util.IsNetworkSegmentationSupportEnabled() {
		cm.endpointSliceMirrorController, err = endpointslicemirror.NewController(ovnClient, wf, cm.networkManager.Interface())
		if err != nil {
			return nil, err
		}
	}
	if config.Kubernetes.OVNEmptyLbEvents {
		if _, err := unidling.NewUnidledAtController(&kube.Kube{KClient: ovnClient.KubeClient}, wf.ServiceInformer()); err != nil {
			return nil, err
		}
	}
	if util.IsDNSNameResolverEnabled() {
		cm.dnsNameResolverController = dnsnameresolver.NewController(ovnClient, wf)
	}

	if util.IsNetworkSegmentationSupportEnabled() {
		udnController := udncontroller.New(
			ovnClient.NetworkAttchDefClient, wf.NADInformer(),
			ovnClient.UserDefinedNetworkClient,
			wf.UserDefinedNetworkInformer(), wf.ClusterUserDefinedNetworkInformer(),
			udntemplate.RenderNetAttachDefManifest,
			cm.networkManager.Interface(),
			wf.PodCoreInformer(),
			wf.NamespaceInformer(),
			cm.recorder,
		)
		cm.userDefinedNetworkController = udnController
		if cm.udnClusterManager != nil {
			cm.udnClusterManager.SetNetworkStatusReporter(udnController.UpdateSubsystemCondition)
		}
	}

	if util.IsRouteAdvertisementsEnabled() {
		cm.raController = routeadvertisements.NewController(cm.networkManager.Interface(), wf, ovnClient)
	}

	if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		cm.podTracker = networkmanager.NewPodTrackerController("cluster-manager-pod-tracker", wf, cm.OnNetworkRefChange)
		if config.OVNKubernetesFeature.EnableEgressIP {
			cm.egressIPTracker = networkmanager.NewEgressIPTrackerController("cluster-manager-egress-ip-tracker", wf, cm.OnNetworkRefChange)
		}
	}

	return cm, nil
}

// Start the cluster manager.
func (cm *ClusterManager) Start(ctx context.Context) error {
	klog.Info("Starting the cluster manager")

	// Start and sync the watch factory to begin listening for events
	if err := cm.wf.Start(); err != nil {
		return err
	}

	// Start networkManager before other controllers
	if err := cm.networkManager.Start(); err != nil {
		return err
	}

	if err := cm.defaultNetClusterController.Start(ctx); err != nil {
		return err
	}

	if err := cm.zoneClusterController.Start(ctx); err != nil {
		return fmt.Errorf("could not start zone controller, err: %w", err)
	}

	if config.OVNKubernetesFeature.EnableEgressIP {
		if err := cm.eIPC.Start(); err != nil {
			return err
		}
	}

	if config.OVNKubernetesFeature.EnableEgressService {
		if err := cm.egressServiceController.Start(1); err != nil {
			return err
		}
	}

	if util.IsNetworkSegmentationSupportEnabled() {
		if err := cm.endpointSliceMirrorController.Start(ctx, 1); err != nil {
			return err
		}
	}
	if err := cm.statusManager.Start(); err != nil {
		return err
	}

	if util.IsDNSNameResolverEnabled() {
		if err := cm.dnsNameResolverController.Start(); err != nil {
			return err
		}
	}

	if util.IsNetworkSegmentationSupportEnabled() {
		if err := cm.userDefinedNetworkController.Run(); err != nil {
			return err
		}
	}

	if cm.raController != nil {
		err := cm.raController.Start()
		if err != nil {
			return err
		}
	}

	if cm.podTracker != nil {
		if err := cm.podTracker.Start(); err != nil {
			return fmt.Errorf("failed to start pod tracker: %w", err)
		}
	}

	if cm.egressIPTracker != nil {
		if err := cm.egressIPTracker.Start(); err != nil {
			return fmt.Errorf("failed to start egress ip tracker: %w", err)
		}
	}
	return nil
}

// Stop the cluster manager.
func (cm *ClusterManager) Stop() {
	klog.Info("Stopping the cluster manager")
	cm.defaultNetClusterController.Stop()
	cm.zoneClusterController.Stop()

	if config.OVNKubernetesFeature.EnableEgressIP {
		cm.eIPC.Stop()
	}
	if config.OVNKubernetesFeature.EnableEgressService {
		cm.egressServiceController.Stop()
	}
	if util.IsNetworkSegmentationSupportEnabled() {
		cm.endpointSliceMirrorController.Stop()
	}
	if cm.podTracker != nil {
		cm.podTracker.Stop()
	}
	if cm.egressIPTracker != nil {
		cm.egressIPTracker.Stop()
	}
	cm.statusManager.Stop()
	if util.IsDNSNameResolverEnabled() {
		cm.dnsNameResolverController.Stop()
	}
	if util.IsNetworkSegmentationSupportEnabled() {
		cm.userDefinedNetworkController.Shutdown()
	}
	if cm.raController != nil {
		cm.raController.Stop()
		cm.raController = nil
	}
}

func (cm *ClusterManager) NewNetworkController(netInfo util.NetInfo) (networkmanager.NetworkController, error) {
	return cm.udnClusterManager.NewNetworkController(netInfo)
}

func (cm *ClusterManager) GetDefaultNetworkController() networkmanager.ReconcilableNetworkController {
	return cm.defaultNetClusterController
}

func (cm *ClusterManager) CleanupStaleNetworks(validNetworks ...util.NetInfo) error {
	return cm.udnClusterManager.CleanupStaleNetworks(validNetworks...)
}

func (cm *ClusterManager) Reconcile(name string, old, new util.NetInfo) error {
	if cm.raController != nil {
		cm.raController.ReconcileNetwork(name, old, new)
	}
	return nil
}

func (cm *ClusterManager) Filter(_ *nettypes.NetworkAttachmentDefinition) (bool, error) {
	return false, nil
}

// OnNetworkRefChange is a callback function used to signal an action to this controller when
// a network needs to be added or removed or just updated
func (cm *ClusterManager) OnNetworkRefChange(node, nadNamespacedName string, active bool) {
	klog.V(4).Infof("Network change for cluster manager triggered by pod/egress IP events "+
		"on node: %s, NAD: %s, active: %t", node, nadNamespacedName, active)

	// determine if NAD belongs to a UDN if we need to update status
	namespace, name, err := cache.SplitMetaNamespaceKey(nadNamespacedName)
	if err != nil {
		klog.Errorf("Failed splitting key %q during network change update: %v", nadNamespacedName, err)
		return
	}

	nad, err := cm.wf.NADInformer().Lister().NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.Errorf("Failed retrieving network attachment definition %q: %v", name, err)
		}
		return
	}

	ownerRef := metav1.GetControllerOf(nad)
	if ownerRef == nil || (ownerRef.Kind != "ClusterUserDefinedNetwork" && ownerRef.Kind != "UserDefinedNetwork") {
		return // not managed by (C)UDN, won't update status
	}

	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		klog.Errorf("Failed parsing network attachment definition %q: %v", name, err)
		return
	}

	networkName := netInfo.GetNetworkName()
	if len(networkName) == 0 {
		return
	}

	allNodes, err := cm.wf.GetNodes()
	if err != nil {
		klog.Errorf("Failed getting nodes for UDN status update %q: %v", name, err)
		return
	}

	uniqueNodes := sets.New[string]()
	for _, node := range allNodes {
		if cm.NodeHasNAD(node.Name, util.GetNADName(nad.Namespace, nad.Name)) {
			uniqueNodes.Insert(node.Name)
		}
	}

	var cond *metav1.Condition
	if uniqueNodes.Len() == 0 {
		msg := "no nodes currently rendered with network"
		cond = &metav1.Condition{
			Type:               "NodesSelected",
			Status:             metav1.ConditionFalse,
			Reason:             "DynamicAllocation",
			Message:            msg,
			LastTransitionTime: metav1.Now(),
		}
	} else {
		msg := fmt.Sprintf("%d node(s) rendered with network", uniqueNodes.Len())
		cond = &metav1.Condition{
			Type:               "NodesSelected",
			Status:             metav1.ConditionTrue,
			Reason:             "DynamicAllocation",
			Message:            msg,
			LastTransitionTime: metav1.Now(),
		}
	}

	if err := cm.userDefinedNetworkController.UpdateSubsystemCondition(
		networkName,
		"ClusterManager", // FieldManager – must be unique per subsystem
		cond,
	); err != nil {
		klog.Errorf("Failed to update NodesSelected condition for %s: %v", networkName, err)
	} else {
		klog.Infof("Updated Dynamic Allocation NodesSelected condition for %s: %s", networkName, cond.Message)
	}
}

func (cm *ClusterManager) NodeHasNAD(node, nad string) bool {
	if cm.podTracker != nil && cm.podTracker.NodeHasNAD(node, nad) {
		return true
	}
	if cm.egressIPTracker != nil && cm.egressIPTracker.NodeHasNAD(node, nad) {
		return true
	}
	return false
}
