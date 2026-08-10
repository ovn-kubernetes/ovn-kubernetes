// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	observabilityconfigv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1"
)

// reconcileKey is the single key used for the ObservabilityConfig reconciler. All CR
// events enqueue this same key: reconcile always recomputes the full applicable set
// from the informer store, so per-object keys are unnecessary and a single key lets the
// workqueue coalesce bursts of events into one reconcile.
const reconcileKey = "observabilityconfig"

// ObservabilityConfigInformer is the minimal interface needed to watch ObservabilityConfig CRs.
// Implemented by the generated informer from the observabilityconfig clientset.
type ObservabilityConfigInformer interface {
	Informer() cache.SharedIndexInformer
}

// NodeGetter resolves a node by name; used to look up the local node's labels for
// Filter.NodeSelector matching. Implemented by factory.WatchFactory.
type NodeGetter interface {
	GetNode(name string) (*corev1.Node, error)
}

// configApplier is the subset of Manager that the reconciler drives. It is the sole
// boundary between k8s watching (this file) and the collector-state engine (Manager).
type configApplier interface {
	// applyConfigs applies the set of ObservabilityConfigs that apply to this node.
	applyConfigs(configs []*observabilityconfigv1alpha1.ObservabilityConfig) error
	// clearConfig clears all applicable configs and cleans up their collectors.
	clearConfig()
}

// configReconciler watches ObservabilityConfig CRs and, on every change, recomputes the
// set that applies to the local node and drives a configApplier with it. All CR events are
// funneled through a single-worker reconciler keyed by reconcileKey, so applies never
// overlap and transient failures are requeued with backoff.
type configReconciler struct {
	applier    configApplier
	informer   ObservabilityConfigInformer
	nodeGetter NodeGetter
	nodeName   string

	reconciler      controller.Reconciler
	eventHandlerReg cache.ResourceEventHandlerRegistration
}

func newConfigReconciler(applier configApplier, informer ObservabilityConfigInformer, nodeGetter NodeGetter, nodeName string) *configReconciler {
	return &configReconciler{
		applier:    applier,
		informer:   informer,
		nodeGetter: nodeGetter,
		nodeName:   nodeName,
	}
}

// start creates the single-worker reconciler, wires the informer event handler and triggers
// an initial reconcile. On any failure it rolls back and returns the error.
func (r *configReconciler) start() error {
	r.reconciler = controller.NewReconciler("observability-config", &controller.ReconcilerConfig{
		Reconcile:   r.reconcile,
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	})
	if err := controller.Start(r.reconciler); err != nil {
		r.reconciler = nil
		return fmt.Errorf("failed to start reconciler: %w", err)
	}

	reg, err := r.informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { r.reconciler.Reconcile(reconcileKey) },
		UpdateFunc: func(_, _ interface{}) { r.reconciler.Reconcile(reconcileKey) },
		DeleteFunc: func(_ interface{}) { r.reconciler.Reconcile(reconcileKey) },
	})
	if err != nil {
		controller.Stop(r.reconciler)
		r.reconciler = nil
		return fmt.Errorf("failed to add event handler: %w", err)
	}
	r.eventHandlerReg = reg

	// The informer may already be synced, so trigger an explicit initial reconcile.
	r.reconciler.Reconcile(reconcileKey)
	return nil
}

// stop removes the event handler and stops the reconciler worker.
func (r *configReconciler) stop() {
	if r.eventHandlerReg != nil {
		if err := r.informer.Informer().RemoveEventHandler(r.eventHandlerReg); err != nil {
			klog.Errorf("Observability: failed to remove ObservabilityConfig event handler: %v", err)
		}
		r.eventHandlerReg = nil
	}
	if r.reconciler != nil {
		controller.Stop(r.reconciler)
	}
}

// reconcile recomputes the set of applicable ObservabilityConfigs from the informer store
// and applies it. It is the single entry point driven by the reconciler worker, so it is
// never run concurrently with itself. Returning an error requeues (with backoff) so
// transient OVSDB failures are retried.
func (r *configReconciler) reconcile(_ string) error {
	objs := r.informer.Informer().GetStore().List()
	nodeLabelsMap := localNodeLabels(r.nodeGetter, r.nodeName)
	configs := allApplicableConfigs(objs, nodeLabelsMap)
	if len(configs) == 0 {
		r.applier.clearConfig()
		return nil
	}
	return r.applier.applyConfigs(configs)
}

// allApplicableConfigs returns all ObservabilityConfigs that apply to this node, ordered
// for resolution: "default" first, then by name. Node labels nil means only configs
// with no NodeSelector apply (cluster-wide).
func allApplicableConfigs(objs []interface{}, nodeLabels map[string]string) []*observabilityconfigv1alpha1.ObservabilityConfig {
	var candidates []*observabilityconfigv1alpha1.ObservabilityConfig
	for _, obj := range objs {
		cfg, ok := obj.(*observabilityconfigv1alpha1.ObservabilityConfig)
		if !ok {
			continue
		}
		if configAppliesToNode(cfg, nodeLabels) {
			candidates = append(candidates, cfg)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// Prefer "default" first, then stable order
	slices.SortFunc(candidates, func(a, b *observabilityconfigv1alpha1.ObservabilityConfig) int {
		if a.Name == "default" && b.Name != "default" {
			return -1
		}
		if a.Name != "default" && b.Name == "default" {
			return 1
		}
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return candidates
}

// localNodeLabels returns the labels of the local node (by name) for Filter.NodeSelector
// matching, or nil if the node cannot be resolved.
func localNodeLabels(nodeGetter NodeGetter, nodeName string) map[string]string {
	if nodeGetter == nil {
		return nil
	}
	node, err := nodeGetter.GetNode(nodeName)
	if err != nil || node == nil {
		return nil
	}
	return node.Labels
}

// configAppliesToNode returns true if the ObservabilityConfig applies to this node.
// When nodeLabels is nil (local node labels could not be resolved), only configs with no
// Filter.NodeSelector apply.
// NodeSelector evaluation matches the pattern used in the Admin Network Policy controller
// (pkg/ovn/controller/admin_network_policy/admin_network_policy_node.go setNodeForANP):
// selector.Matches(labels.Set(node.Labels)).
func configAppliesToNode(cfg *observabilityconfigv1alpha1.ObservabilityConfig, nodeLabels map[string]string) bool {
	if cfg.Spec.Filter == nil || cfg.Spec.Filter.NodeSelector == nil {
		return true
	}
	if nodeLabels == nil {
		return false
	}
	selector, err := metav1.LabelSelectorAsSelector(cfg.Spec.Filter.NodeSelector)
	if err != nil {
		klog.Warningf("Observability: invalid NodeSelector on ObservabilityConfig %s: %v", cfg.Name, err)
		return false
	}
	return selector.Matches(labels.Set(nodeLabels))
}
