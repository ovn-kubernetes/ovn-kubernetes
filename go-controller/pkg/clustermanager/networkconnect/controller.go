package networkconnect

import (
	"reflect"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned"
	networkconnectlisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/listers/clusternetworkconnect/v1"
	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var (
	cudnGVK = userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork")
	udnGVK  = userdefinednetworkv1.SchemeGroupVersion.WithKind("UserDefinedNetwork")
)

type Controller struct {
	// wf is the watch factory for accessing informers
	wf *factory.WatchFactory
	// listers
	cncLister       networkconnectlisters.ClusterNetworkConnectLister
	namespaceLister corelisters.NamespaceLister
	nadLister       nadlisters.NetworkAttachmentDefinitionLister
	//clientset
	cncClient networkconnectclientset.Interface
	nadClient nadclientset.Interface
	// Controller for managing cluster-network-connect events
	cncController controllerutil.Controller
	// Controller for managing NetworkAttachmentDefinition events
	nadController  controllerutil.Controller
	networkManager networkmanager.Interface
}

func NewController(
	wf *factory.WatchFactory,
	ovnClient *util.OVNClusterManagerClientset,
	networkManager networkmanager.Interface,
) *Controller {
	cncLister := wf.ClusterNetworkConnectInformer().Lister()
	nadLister := wf.NADInformer().Lister()

	c := &Controller{
		wf:              wf,
		cncClient:       ovnClient.NetworkConnectClient,
		nadClient:       ovnClient.NetworkAttchDefClient,
		cncLister:       cncLister,
		nadLister:       nadLister,
		namespaceLister: wf.NamespaceInformer().Lister(),
		networkManager:  networkManager,
	}

	cncCfg := &controllerutil.ControllerConfig[networkconnectv1.ClusterNetworkConnect]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.ClusterNetworkConnectInformer().Informer(),
		Lister:         cncLister.List,
		Reconcile:      c.reconcileClusterNetworkConnect,
		ObjNeedsUpdate: cncNeedsUpdate,
		Threadiness:    1,
	}
	c.cncController = controllerutil.NewController(
		"clustermanager-network-connect-controller",
		cncCfg,
	)

	nadCfg := &controllerutil.ControllerConfig[nadv1.NetworkAttachmentDefinition]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:       wf.NADInformer().Informer(),
		Lister:         nadLister.List,
		Reconcile:      c.reconcileNAD,
		ObjNeedsUpdate: nadNeedsUpdate,
		Threadiness:    1,
	}
	c.nadController = controllerutil.NewController(
		"clustermanager-network-connect-network-attachment-definition-controller",
		nadCfg,
	)

	return c
}

func (c *Controller) Start() error {
	defer klog.Infof("Cluster manager network connect controllers started")
	return controllerutil.Start(
		c.cncController,
		c.nadController,
	)
}

func (c *Controller) Stop() {
	controllerutil.Stop(
		c.cncController,
		c.nadController,
	)
	klog.Infof("Cluster manager network connect controllers stopped")
}

func cncNeedsUpdate(oldObj, newObj *networkconnectv1.ClusterNetworkConnect) bool {
	// Case 1: CNC is being deleted
	// Case 2: CNC is being created
	if oldObj == nil || newObj == nil {
		return true
	}
	// Case 3: CNC is being updated
	// Only trigger updates when the Spec.NetworkSelectors changes
	// We only need to check for selector changes
	// and don't need to react on connectivity enabled field changes
	// from cluster manager.
	// connectSubnet is immutable so that can't change after creation.
	return !reflect.DeepEqual(oldObj.Spec.NetworkSelectors, newObj.Spec.NetworkSelectors)
}

func nadNeedsUpdate(oldObj, newObj *nadv1.NetworkAttachmentDefinition) bool {
	nadSupported := func(nad *nadv1.NetworkAttachmentDefinition) bool {
		if nad == nil {
			return false
		}
		// we don't support direct NADs anymore. CNC is only supported for CUDNs and UDNs
		controller := metav1.GetControllerOfNoCopy(nad)
		isCUDN := controller != nil && controller.Kind == cudnGVK.Kind && controller.APIVersion == cudnGVK.GroupVersion().String()
		isUDN := controller != nil && controller.Kind == udnGVK.Kind && controller.APIVersion == udnGVK.GroupVersion().String()
		if !isCUDN && !isUDN {
			return false
		}
		network, err := util.ParseNADInfo(nad)
		if err != nil {
			// cannot parse NAD info, so we take this as unsupported NAD error
			return false
		}
		if network.IsPrimaryNetwork() {
			// only layer3 and layer2 topology are supported
			// but since primary network is always layer3 or layer2,
			// we can ignored the need to check the topology
			return true
		}
		return false // we don't support secondary networks, so we can ignore it
	}
	// ignore if we don't support this NAD
	if !nadSupported(oldObj) && !nadSupported(newObj) {
		return false
	}
	// CASE1: NAD is being deleted (UDN or CUDN is being deleted)
	// CASE2: NAD is being created (UDN or CUDN is being created)
	if oldObj == nil || newObj == nil {
		return true
	}
	oldNADLabels := labels.Set(oldObj.Labels)
	newNADLabels := labels.Set(newObj.Labels)
	labelsChanged := !labels.Equals(oldNADLabels, newNADLabels)
	// safe spot check for ovn network id annotation add happening as update to NADs
	annotationsChanged := oldObj.Annotations[ovntypes.OvnNetworkIDAnnotation] != newObj.Annotations[ovntypes.OvnNetworkIDAnnotation]

	// CASE3: NAD is being updated (UDN or CUDN is being updated)
	//  3.1: NAD network id annotation changed
	//  3.2: NAD labels changed (only relevant for CUDNs->NADs)
	return labelsChanged || annotationsChanged
}

func (c *Controller) reconcileClusterNetworkConnect(_ string) error {
	// STEP1: Validate the CNC
	// STEP2: Generate a tunnelID for the connect router corresponding to this CNC
	// STEP3: Discover the selected UDNs and CUDNs
	// STEP4: Generate subnets of size CNC.Spec.ConnectSubnets.NetworkPrefix for each layer3 network
	//  and /31 or /127 subnet for each layer2 networks

	return nil
}

func (c *Controller) reconcileNAD(_ string) error {
	return nil
}
