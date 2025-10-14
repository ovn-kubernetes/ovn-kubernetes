package clustermanager

import (
	"context"
	"fmt"
	"net"

	networkattchmentdefclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/dnsnameresolver"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/egressservice"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/endpointslicemirror"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/routeadvertisements"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/status_manager"
	udncontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork"
	udntemplate "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/virtualprivatenetworkconnect"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/unidling"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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

	vpncController *virtualprivatenetworkconnect.Controller
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
		// tunnelKeysAllocator is now only used for NAD tunnel keys allocation, but will be reused
		// for Connecting UDNs. So we initialize it here and pass it to the networkManager.
		// The same instance should be initialized only once and passed to all the
		// users of tunnel-keys.
		tunnelKeysAllocator, err := initTunnelKeysAllocator(ovnClient.NetworkAttchDefClient)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize tunnel keys allocator: %w", err)
		}

		cm.networkManager, err = networkmanager.NewForCluster(cm, wf, ovnClient, recorder, tunnelKeysAllocator)
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

	if util.IsVirtualPrivateNetworkConnectEnabled() {
		cm.vpncController = virtualprivatenetworkconnect.NewController(cm.networkManager.Interface(), wf, ovnClient)
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

	if cm.vpncController != nil {
		err := cm.vpncController.Start()
		if err != nil {
			return err
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
	if cm.vpncController != nil {
		cm.vpncController.Stop()
		cm.vpncController = nil
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

// initTunnelKeysAllocator reserves any existing tunnel keys to avoid re-allocation.
// It will be shared across multiple controllers and should account for different object types.
// Good news is that we don't care about missing events, because we only need to reserve ids that are already
// annotated, and no one else can annotate them except ClusterManager.
func initTunnelKeysAllocator(nadClient networkattchmentdefclientset.Interface) (*id.TunnelKeysAllocator, error) {
	tunnelKeysAllocator := id.NewTunnelKeyAllocator("TunnelKeys")

	existingNADs, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list existing NADs: %w", err)
	}
	for _, nad := range existingNADs.Items {
		// reserve tunnel keys that are already allocated to make sure they are
		if nad.Annotations[types.OvnNetworkTunnelKeysAnnotation] != "" {
			netconf, err := util.ParseNetConf(&nad)
			if err != nil {
				// ignore non-OVN NADs; otherwise log and continue
				if err.Error() == util.ErrorAttachDefNotOvnManaged.Error() {
					continue
				}
				klog.Warningf("Failed to parse NAD annotation %s: %v", nad.Name, err)
				continue
			}
			networkName := netconf.Name
			tunnelKeys, err := util.ParseTunnelKeysAnnotation(nad.Annotations[types.OvnNetworkTunnelKeysAnnotation])
			if err != nil {
				return nil, fmt.Errorf("failed to parse annotated tunnel keys: %w", err)
			}
			if err = tunnelKeysAllocator.ReserveKeys(networkName, tunnelKeys); err != nil {
				return nil, fmt.Errorf("failed to reserve tunnel keys %v for network %s: %w", tunnelKeys, networkName, err)
			}
		}
	}
	return tunnelKeysAllocator, nil
}
