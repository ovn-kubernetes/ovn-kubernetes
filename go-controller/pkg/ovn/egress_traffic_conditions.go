// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
)

const (
	trafficSelectorResolved = "TrafficSelectorResolved"
	trafficConflict         = "TrafficConflict"
	noMatchingTrafficClass  = "NoMatchingTrafficClass"
	invalidTrafficSelection = "InvalidTrafficSelection"
)

type trafficConditionHandler struct {
	informer     cache.SharedIndexInformer
	registration cache.ResourceEventHandlerRegistration
}

// StartTrafficConditions reports global selection results independently of local OVN state.
// Every controller uses cluster-wide informer caches so one zone cannot clear another's conflict.
func (e *EgressIPController) StartTrafficConditions(stopCh <-chan struct{}) (err error) {
	e.trafficConditions = controller.NewReconciler("egress-traffic-conditions", &controller.ReconcilerConfig{
		Threadiness: 1, MaxAttempts: controller.InfiniteAttempts,
		Reconcile: func(ctx context.Context, _ string) error { return e.reconcileTrafficConditions(ctx) },
	})
	defer func() {
		if err != nil {
			e.StopTrafficConditions()
		}
	}()
	informers := []cache.SharedIndexInformer{
		e.watchFactory.EgressIPInformer().Informer(),
	}
	if config.OVNKubernetesFeature.EnableTrafficClass {
		informers = append(informers, e.watchFactory.TrafficClassInformer().Informer(),
			e.watchFactory.PodCoreInformer().Informer(), e.watchFactory.NamespaceCoreInformer().Informer())
	}
	for _, informer := range informers {
		registration, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    e.enqueueTrafficConditions,
			DeleteFunc: e.enqueueTrafficConditions,
			UpdateFunc: func(old, new interface{}) {
				if trafficConditionNeedsUpdate(old, new) {
					e.enqueueTrafficConditions(old)
					e.enqueueTrafficConditions(new)
				}
			},
		})
		if err != nil {
			return err
		}
		e.trafficConditionHandlers = append(e.trafficConditionHandlers, trafficConditionHandler{informer, registration})
		if !util.WaitForInformerCacheSyncWithTimeout("egress-traffic-conditions", stopCh, informer.HasSynced) {
			return fmt.Errorf("timed out syncing traffic condition informer")
		}
	}
	if err := controller.Start(e.trafficConditions); err != nil {
		return err
	}
	e.trafficConditions.Reconcile("")
	return nil
}

// Ignore workload events that cannot change any selective owner's pod set.
func (e *EgressIPController) enqueueTrafficConditions(obj interface{}) {
	affected, err := e.affectsTrafficConditions(obj)
	if err != nil || affected {
		e.trafficConditions.Reconcile("")
	}
}

func (e *EgressIPController) affectsTrafficConditions(obj interface{}) (bool, error) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	var pod *corev1.Pod
	var ns *corev1.Namespace
	switch value := obj.(type) {
	case *corev1.Pod:
		if !util.PodNeedsSNAT(value) {
			return false, nil
		}
		pod = value
		var err error
		ns, err = e.watchFactory.NamespaceCoreInformer().Lister().Get(pod.Namespace)
		if err != nil {
			return false, err
		}
	case *corev1.Namespace:
		ns = value
	default:
		return true, nil
	}
	eips, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return false, err
	}
	for _, eip := range eips {
		if !egressip.HasTrafficSelector(eip.Spec) {
			continue
		}
		nsSelector, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
		if err != nil {
			return false, err
		}
		if !nsSelector.Matches(labels.Set(ns.Labels)) {
			continue
		}
		if pod == nil {
			return true, nil
		}
		podSelector, err := metav1.LabelSelectorAsSelector(&eip.Spec.PodSelector)
		if err != nil {
			return false, err
		}
		if podSelector.Matches(labels.Set(pod.Labels)) {
			return true, nil
		}
	}
	return false, nil
}

// StopTrafficConditions removes handlers and waits for condition reconciliation to stop.
func (e *EgressIPController) StopTrafficConditions() {
	for _, handler := range e.trafficConditionHandlers {
		_ = handler.informer.RemoveEventHandler(handler.registration)
	}
	e.trafficConditionHandlers = nil
	if e.trafficConditions != nil {
		controller.Stop(e.trafficConditions)
	}
}

func trafficConditionNeedsUpdate(old, new interface{}) bool {
	switch before := old.(type) {
	case *eipv1.EgressIP:
		after := new.(*eipv1.EgressIP)
		if !reflect.DeepEqual(before.Spec, after.Spec) {
			return true
		}
		// A delayed status event must revisit a no-op based on an older informer snapshot.
		for _, name := range []string{trafficSelectorResolved, trafficConflict} {
			if !equality.Semantic.DeepEqual(meta.FindStatusCondition(before.Status.Conditions, name),
				meta.FindStatusCondition(after.Status.Conditions, name)) {
				return true
			}
		}
		return false
	case *trafficv1.TrafficClass:
		after := new.(*trafficv1.TrafficClass)
		return !reflect.DeepEqual(before.Labels, after.Labels) || !reflect.DeepEqual(before.Spec, after.Spec)
	case *corev1.Pod:
		after := new.(*corev1.Pod)
		return !reflect.DeepEqual(before.Labels, after.Labels) || util.PodNeedsSNAT(before) != util.PodNeedsSNAT(after)
	case *corev1.Namespace:
		return !reflect.DeepEqual(before.Labels, new.(*corev1.Namespace).Labels)
	}
	return false
}

type selectedEgressTraffic struct {
	eip          *eipv1.EgressIP
	destinations []egressip.Destination
	pods         sets.Set[string]
	resolved     metav1.Condition
	conflict     metav1.Condition
	unsupported  []egressip.UnsupportedProtocol
}

func (e *EgressIPController) reconcileTrafficConditions(ctx context.Context) error {
	eips, err := e.watchFactory.GetEgressIPs()
	if err != nil {
		return err
	}
	if !config.OVNKubernetesFeature.EnableTrafficClass {
		for _, eip := range eips {
			var conditions []metav1.Condition
			if egressip.HasTrafficSelector(eip.Spec) {
				conditions = []metav1.Condition{{Type: trafficSelectorResolved, Status: metav1.ConditionFalse, Reason: "FeatureGateDisabled", Message: "TrafficClass is disabled; this EgressIP handles no traffic"}}
			}
			if err = e.patchTrafficConditions(ctx, eip, conditions); err != nil {
				return err
			}
		}
		clear(e.trafficProtocolWarnings)
		return nil
	}
	classes, err := e.watchFactory.TrafficClassInformer().Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("list TrafficClasses for EgressIP traffic conditions: %w", err)
	}
	pods, err := e.watchFactory.PodCoreInformer().Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("list pods for EgressIP traffic conditions: %w", err)
	}
	namespaces, err := e.watchFactory.NamespaceCoreInformer().Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("list namespaces for EgressIP traffic conditions: %w", err)
	}
	namespaceLabels := map[string]labels.Set{}
	for _, ns := range namespaces {
		namespaceLabels[ns.Name] = labels.Set(ns.Labels)
	}
	slices.SortFunc(eips, func(a, b *eipv1.EgressIP) int { return strings.Compare(a.Name, b.Name) })
	selected := make([]selectedEgressTraffic, 0, len(eips))
	for _, eip := range eips {
		if !egressip.HasTrafficSelector(eip.Spec) {
			if err = e.patchTrafficConditions(ctx, eip, nil); err != nil {
				return err
			}
			continue
		}
		state, err := selectEgressTraffic(eip, classes, pods, namespaceLabels)
		if err != nil {
			state = selectedEgressTraffic{
				eip:      eip,
				resolved: metav1.Condition{Type: trafficSelectorResolved, Status: metav1.ConditionFalse, Reason: invalidTrafficSelection, Message: err.Error()},
				conflict: metav1.Condition{Type: trafficConflict, Status: metav1.ConditionUnknown, Reason: invalidTrafficSelection, Message: "Cannot determine conflicts for invalid traffic selection"},
			}
		}
		selected = append(selected, state)
	}
	podKeys := sets.New[string]()
	for _, state := range selected {
		podKeys.Insert(state.pods.UnsortedList()...)
	}
	for _, podKey := range sets.List(podKeys) {
		var candidates []egressip.TrafficCandidate
		for _, state := range selected {
			if state.pods.Has(podKey) && len(state.destinations) != 0 {
				candidates = append(candidates, egressip.TrafficCandidate{EgressIP: state.eip, Destinations: state.destinations})
			}
		}
		results := egressip.SelectTrafficOwners(candidates)
		for i := range selected {
			if conflict := results[selected[i].eip.Name]; conflict != nil {
				setTrafficConflict(&selected[i], conflict.Owner, podKey, conflict.Destination, conflict.Other)
			}
		}
	}
	activeWarnings := sets.New[string]()
	for _, state := range selected {
		if err = e.patchTrafficConditions(ctx, state.eip, []metav1.Condition{state.resolved, state.conflict}); err != nil {
			return err
		}
		key := string(state.eip.UID) + "/" + state.eip.Name
		activeWarnings.Insert(key)
		previous := e.trafficProtocolWarnings[key]
		for _, unsupported := range state.unsupported {
			if previous.Has(unsupported) {
				continue
			}
			message := fmt.Sprintf("TrafficClass %s match %d protocol entry %d is unsupported and was skipped", unsupported.Class, unsupported.Match, unsupported.Protocol)
			klog.Warningf("EgressIP %s: %s", state.eip.Name, message)
			e.recorder.Event(state.eip, corev1.EventTypeWarning, "UnsupportedProtocol", message)
		}
		e.trafficProtocolWarnings[key] = sets.New(state.unsupported...)
	}
	for key := range e.trafficProtocolWarnings {
		if !activeWarnings.Has(key) {
			delete(e.trafficProtocolWarnings, key)
		}
	}
	return nil
}

func selectEgressTraffic(eip *eipv1.EgressIP, classes []*trafficv1.TrafficClass, pods []*corev1.Pod, namespaceLabels map[string]labels.Set) (selectedEgressTraffic, error) {
	state := selectedEgressTraffic{eip: eip, pods: sets.New[string]()}
	resolution, err := egressip.ResolveTraffic(eip, classes)
	if err != nil {
		return state, err
	}
	state.destinations = resolution.Destinations
	resolved := metav1.Condition{Type: trafficSelectorResolved, Status: metav1.ConditionFalse,
		Reason: noMatchingTrafficClass, Message: "No TrafficClass resources match trafficSelector"}
	if len(state.destinations) > 0 {
		resolved.Status, resolved.Reason = metav1.ConditionTrue, "MatchesFound"
		resolved.Message = fmt.Sprintf("Resolved %d matches from %d TrafficClass resource(s)", len(state.destinations), resolution.ClassCount)
	}
	if len(state.destinations) == 0 && resolution.ClassCount > 0 && len(resolution.Unsupported) > 0 {
		resolved.Reason, resolved.Message = "AllMatchesUnsupported", "Selected TrafficClass resources contain no supported matches"
	}
	state.unsupported = resolution.Unsupported
	state.resolved = resolved
	state.conflict = metav1.Condition{Type: trafficConflict, Status: metav1.ConditionFalse,
		Reason: "NoOverlappingTraffic", Message: "No overlapping traffic with another EgressIP targeting the same pod"}
	nsSelector, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
	if err != nil {
		return state, err
	}
	podSelector, err := metav1.LabelSelectorAsSelector(&eip.Spec.PodSelector)
	if err != nil {
		return state, err
	}
	for _, pod := range pods {
		nsLabels, exists := namespaceLabels[pod.Namespace]
		if exists && util.PodNeedsSNAT(pod) && nsSelector.Matches(nsLabels) && podSelector.Matches(labels.Set(pod.Labels)) {
			state.pods.Insert(pod.Namespace + "/" + pod.Name)
		}
	}
	return state, nil
}

func setTrafficConflict(state *selectedEgressTraffic, peer, pod string, own, other egressip.Destination) {
	condition := &state.conflict
	if condition.Status == metav1.ConditionTrue {
		return
	}
	condition.Status, condition.Reason = metav1.ConditionTrue, "OverlappingTraffic"
	condition.Message = fmt.Sprintf("Destination %s overlaps %s with EgressIP %s for pod %s; not programmed", own, other, peer, pod)
}

func (e *EgressIPController) patchTrafficConditions(ctx context.Context, snapshot *eipv1.EgressIP, desired []metav1.Condition) error {
	// Managed-condition events requeue stale snapshots; unchanged results need no API read.
	if equality.Semantic.DeepEqual(mergeTrafficConditions(snapshot.Status.Conditions, desired), snapshot.Status.Conditions) {
		return nil
	}
	client := e.kube.EIPClient.K8sV1().EgressIPs()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := client.Get(ctx, snapshot.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.UID != snapshot.UID || !reflect.DeepEqual(current.Spec, snapshot.Spec) {
			return fmt.Errorf("EgressIP %s changed during traffic condition reconciliation", snapshot.Name)
		}
		conditions := mergeTrafficConditions(current.Status.Conditions, desired)
		if reflect.DeepEqual(conditions, current.Status.Conditions) {
			return nil
		}
		// Admission defaults the required items array when conditions are the first status write.
		patch, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{"resourceVersion": current.ResourceVersion},
			"status":   map[string]interface{}{"conditions": conditions},
		})
		if err != nil {
			return err
		}
		_, err = client.Patch(ctx, current.Name, ktypes.MergePatchType, patch, metav1.PatchOptions{})
		if err != nil {
			return err
		}
		before := meta.FindStatusCondition(current.Status.Conditions, trafficSelectorResolved)
		after := meta.FindStatusCondition(conditions, trafficSelectorResolved)
		if after != nil && after.Status == metav1.ConditionFalse &&
			(before == nil || before.Status != after.Status || before.Reason != after.Reason) {
			reason := after.Reason
			if reason == noMatchingTrafficClass {
				reason = "NoTrafficClass"
			}
			if reason != invalidTrafficSelection {
				klog.Warningf("EgressIP %s: %s", current.Name, after.Message)
				e.recorder.Event(current, corev1.EventTypeWarning, reason, after.Message)
			}
		}
		return nil
	})
}

func mergeTrafficConditions(current, desired []metav1.Condition) []metav1.Condition {
	conditions := slices.Clone(current)
	for _, name := range []string{trafficSelectorResolved, trafficConflict} {
		condition := meta.FindStatusCondition(desired, name)
		if condition == nil {
			meta.RemoveStatusCondition(&conditions, name)
		} else {
			// Omit observedGeneration: legacy status writes also advance generation.
			meta.SetStatusCondition(&conditions, *condition)
		}
	}
	return conditions
}
