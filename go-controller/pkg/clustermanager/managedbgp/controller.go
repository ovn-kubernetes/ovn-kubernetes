package managedbgp

import (
	"context"
	"fmt"
	"reflect"

	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	frrclientset "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	frrlisters "github.com/metallb/frr-k8s/pkg/client/listers/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// ControllerName is the name of the managed BGP controller
	ControllerName = "managed-bgp-controller"
	// FRRConfigManagedLabel is the label used to identify FRRConfigurations managed by this controller
	FRRConfigManagedLabel = "k8s.ovn.org/managed-internal-fabric"
	// FRRConfigManagedValue is the value used for the FRRConfigManagedLabel
	FRRConfigManagedValue = "bgp"
	// BaseFRRConfigName is the name of the base FRRConfiguration
	BaseFRRConfigName = "ovnk-managed-base"
)

// Controller manages the BGP topology for no-overlay networks with managed routing
type Controller struct {
	frrClient      frrclientset.Interface
	frrLister      frrlisters.FRRConfigurationLister
	nodeController controllerutil.Controller
	wf             *factory.WatchFactory
	recorder       record.EventRecorder
}

// NewController creates a new managed BGP controller
func NewController(
	wf *factory.WatchFactory,
	frrClient frrclientset.Interface,
	recorder record.EventRecorder,
) *Controller {
	c := &Controller{
		frrClient: frrClient,
		frrLister: wf.FRRConfigurationsInformer().Lister(),
		wf:        wf,
		recorder:  recorder,
	}

	nodeConfig := &controllerutil.ControllerConfig[corev1.Node]{
		RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:      c.reconcileNode,
		Threadiness:    1,
		Informer:       wf.NodeCoreInformer().Informer(),
		Lister:         wf.NodeCoreInformer().Lister().List,
		ObjNeedsUpdate: c.nodeNeedsUpdate,
	}
	c.nodeController = controllerutil.NewController(ControllerName, nodeConfig)

	return c
}

// Start starts the managed BGP controller
func (c *Controller) Start() error {
	klog.Infof("Starting managed BGP controller")
	return controllerutil.Start(c.nodeController)
}

// Stop stops the managed BGP controller
func (c *Controller) Stop() {
	klog.Infof("Stopping managed BGP controller")
	controllerutil.Stop(c.nodeController)
}

func (c *Controller) nodeNeedsUpdate(oldNode, newNode *corev1.Node) bool {
	if oldNode == nil || newNode == nil {
		return true
	}
	// We care about node IP changes
	oldV4, oldV6 := util.GetNodeInternalAddrs(oldNode)
	newV4, newV6 := util.GetNodeInternalAddrs(newNode)
	return !reflect.DeepEqual(oldV4, newV4) || !reflect.DeepEqual(oldV6, newV6)
}

func (c *Controller) reconcileNode(_ string) error {
	if config.ManagedBGP.Topology != config.ManagedBGPTopologyFullMesh {
		return nil
	}

	nodes, err := c.wf.GetNodes()
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	// For full-mesh, we ensure there is a single base FRRConfiguration peering with all nodes.
	// The RouteAdvertisements controller will then generate per-node configs based on this,
	// excluding self-peering.
	if err := c.ensureBaseFRRConfiguration(nodes); err != nil {
		klog.Errorf("Failed to ensure base FRRConfiguration: %v", err)
		return err
	}

	// Cleanup old per-node FRRConfigurations if they exist (from previous implementation)
	// and any other stale managed configs.
	frrConfigs, err := c.frrLister.FRRConfigurations(config.ManagedBGP.FRRNamespace).List(labels.SelectorFromSet(map[string]string{
		FRRConfigManagedLabel: FRRConfigManagedValue,
	}))
	if err != nil {
		return fmt.Errorf("failed to list FRRConfigurations: %w", err)
	}

	for _, frrConfig := range frrConfigs {
		if frrConfig.Name == BaseFRRConfigName {
			continue
		}
		klog.Infof("Deleting stale managed FRRConfiguration %s", frrConfig.Name)
		if err := c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Delete(context.TODO(), frrConfig.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			klog.Errorf("Failed to delete stale FRRConfiguration %s: %v", frrConfig.Name, err)
		}
	}

	return nil
}

func (c *Controller) ensureBaseFRRConfiguration(allNodes []*corev1.Node) error {
	neighbors := []frrtypes.Neighbor{}
	for _, node := range allNodes {
		v4, v6 := util.GetNodeInternalAddrs(node)
		if v4 != nil {
			neighbors = append(neighbors, frrtypes.Neighbor{
				Address:   v4.String(),
				ASN:       config.ManagedBGP.ASNumber,
				DisableMP: true,
			})
		}
		if v6 != nil {
			neighbors = append(neighbors, frrtypes.Neighbor{
				Address:   v6.String(),
				ASN:       config.ManagedBGP.ASNumber,
				DisableMP: true,
			})
		}
	}

	frrConfig := &frrtypes.FRRConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BaseFRRConfigName,
			Namespace: config.ManagedBGP.FRRNamespace,
			Labels: map[string]string{
				FRRConfigManagedLabel: FRRConfigManagedValue,
			},
		},
		Spec: frrtypes.FRRConfigurationSpec{
			// Empty NodeSelector means it applies as a base for all nodes by RouteAdvertisements controller
			NodeSelector: metav1.LabelSelector{},
			BGP: frrtypes.BGPConfig{
				Routers: []frrtypes.Router{
					{
						ASN:       config.ManagedBGP.ASNumber,
						Neighbors: neighbors,
					},
				},
			},
		},
	}

	existing, err := c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Get(context.TODO(), BaseFRRConfigName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("Creating base FRRConfiguration %s", BaseFRRConfigName)
			_, err = c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Create(context.TODO(), frrConfig, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
			return nil
		}
		return err
	}

	if !reflect.DeepEqual(existing.Spec, frrConfig.Spec) {
		klog.Infof("Updating base FRRConfiguration %s", BaseFRRConfigName)
		updated := existing.DeepCopy()
		updated.Spec = frrConfig.Spec
		_, err = c.frrClient.ApiV1beta1().FRRConfigurations(config.ManagedBGP.FRRNamespace).Update(context.TODO(), updated, metav1.UpdateOptions{})
		return err
	}

	return nil
}
