// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktypes "k8s.io/apimachinery/pkg/types"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	eipfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
)

func TestTrafficConditionLifecycle(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("prepare test config: %v", err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableTrafficClass = true
	ctx := context.Background()
	eip := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "one", Generation: 1}, Spec: eipv1.EgressIPSpec{
		TrafficSelector:   &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "one"}},
		NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tenant": "one"}},
		PodSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "sip"}},
	}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: "node", EgressIP: "192.0.2.10"}},
		Conditions: []metav1.Condition{{Type: "OtherWriter", Status: metav1.ConditionTrue, Reason: "Healthy", Message: "keep", LastTransitionTime: metav1.Now()}}}}
	peer := eip.DeepCopy()
	peer.Name = "two"
	peer.Spec.TrafficSelector.MatchLabels["traffic"] = "two"
	class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "one", Labels: map[string]string{"traffic": "one"}},
		Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "198.51.100.0/24", Protocols: []trafficv1.ProtocolMatch{
			{UDP: &trafficv1.PortProtocol{DestinationPort: &trafficv1.Port{Number: ptr.To[int32](5060)}}},
		}}}}}
	peerClass := class.DeepCopy()
	peerClass.Name = "two"
	peerClass.Labels["traffic"] = "two"
	peerClass.Spec.Matches[0].Protocols[0].UDP = &trafficv1.PortProtocol{SourcePort: &trafficv1.Port{Range: &trafficv1.PortRange{Start: 16384, End: 32767}}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workload", Labels: map[string]string{"tenant": "one"}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sip", Namespace: ns.Name, Labels: map[string]string{"app": "sip"}}, Spec: corev1.PodSpec{NodeName: "node"}}
	catchAll := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "catch-all"}}
	clients := util.GetOVNClientset(eip, peer, catchAll, class, peerClass, ns, pod)
	wf, err := factory.NewOVNKubeControllerWatchFactory(clients.GetOVNKubeControllerClientset(), "node")
	if err != nil {
		t.Fatalf("create controller watch factory: %v", err)
	}
	t.Cleanup(wf.Stop)
	for _, pair := range []struct {
		obj runtime.Object
		add func(interface{}) error
	}{
		{eip, wf.EgressIPInformer().Informer().GetStore().Add},
		{peer, wf.EgressIPInformer().Informer().GetStore().Add},
		{catchAll, wf.EgressIPInformer().Informer().GetStore().Add},
		{class, wf.TrafficClassInformer().Informer().GetStore().Add},
		{peerClass, wf.TrafficClassInformer().Informer().GetStore().Add},
		{ns, wf.NamespaceCoreInformer().Informer().GetStore().Add},
		{pod, wf.PodCoreInformer().Informer().GetStore().Add},
	} {
		if err = pair.add(pair.obj); err != nil {
			t.Fatalf("populate informer store: %v", err)
		}
	}
	recorder := record.NewFakeRecorder(10)
	e := newTrafficConditionTestController(t, clients, wf, recorder, "node")
	get := func(name string) *eipv1.EgressIP {
		t.Helper()
		got, err := clients.EgressIPClient.K8sV1().EgressIPs().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get EgressIP: %v", err)
		}
		// Deliver status writes to the informer without sharing mutable test objects.
		if err = wf.EgressIPInformer().Informer().GetStore().Update(got.DeepCopy()); err != nil {
			t.Fatalf("deliver EgressIP status to informer: %v", err)
		}
		return got
	}
	check := func(name, typ string, status metav1.ConditionStatus) *metav1.Condition {
		t.Helper()
		c := meta.FindStatusCondition(get(name).Status.Conditions, typ)
		if c == nil || c.Status != status {
			t.Fatalf("%s %s = %+v, want %s", name, typ, c, status)
		}
		return c
	}
	if err = e.reconcileTrafficConditions(context.Background()); err != nil {
		t.Fatalf("reconcile traffic conditions: %v", err)
	}
	check("one", trafficSelectorResolved, metav1.ConditionTrue)
	check("one", trafficConflict, metav1.ConditionFalse)
	if c := check("two", trafficConflict, metav1.ConditionTrue); !strings.Contains(c.Message, "workload/sip") {
		t.Fatal("conflict does not identify shared pod")
	}
	before := get("one").Status.Conditions
	unrelated := pod.DeepCopy()
	unrelated.Name, unrelated.Labels = "unrelated", map[string]string{"app": "other"}
	if affected, err := e.affectsTrafficConditions(unrelated); err != nil || affected {
		t.Fatalf("unrelated pod queued a global scan: affected=%t, err=%v", affected, err)
	}
	if affected, err := e.affectsTrafficConditions(pod); err != nil || !affected {
		t.Fatalf("selected pod did not queue a scan: affected=%t, err=%v", affected, err)
	}
	if err = wf.PodCoreInformer().Informer().GetStore().Add(unrelated); err != nil {
		t.Fatalf("add unrelated pod to informer: %v", err)
	}
	fakeClient := clients.EgressIPClient.(*eipfake.Clientset)
	fakeClient.ClearActions()
	if err = e.reconcileTrafficConditions(context.Background()); err != nil {
		t.Fatalf("reconcile after unrelated pod: %v", err)
	}
	if actions := fakeClient.Actions(); len(actions) != 0 {
		t.Fatalf("unchanged selection made API calls: %+v", actions)
	}
	patchCount := func() int {
		count := 0
		for _, action := range clients.EgressIPClient.(*eipfake.Clientset).Actions() {
			if action.GetVerb() == "patch" {
				count++
			}
		}
		return count
	}
	actions := patchCount()
	if err = e.reconcileTrafficConditions(context.Background()); err != nil {
		t.Fatalf("reconcile unchanged conditions: %v", err)
	}
	if patchCount() != actions {
		t.Fatal("unchanged conditions were written again")
	}
	if !reflect.DeepEqual(before, get("one").Status.Conditions) {
		t.Fatal("unchanged reconciliation moved transition times")
	}
	// Previously admitted malformed selectors must not block healthy owners.
	invalid := eip.DeepCopy()
	invalid.Name, invalid.Status.Conditions = "invalid", nil
	invalid.Spec.TrafficSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "class", Operator: "Invalid"}}}
	if _, err = clients.EgressIPClient.K8sV1().EgressIPs().Create(ctx, invalid, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create invalid EgressIP: %v", err)
	}
	if err = wf.EgressIPInformer().Informer().GetStore().Add(invalid); err != nil {
		t.Fatalf("add invalid EgressIP to informer: %v", err)
	}
	if err = e.reconcileTrafficConditions(context.Background()); err != nil {
		t.Fatalf("reconcile invalid selection: %v", err)
	}
	if c := check("invalid", trafficSelectorResolved, metav1.ConditionFalse); c.Reason != "InvalidTrafficSelection" {
		t.Fatalf("invalid selector not diagnosed: %+v", c)
	}
	check("invalid", trafficConflict, metav1.ConditionUnknown)
	check("one", trafficConflict, metav1.ConditionFalse)
	select {
	case event := <-recorder.Events:
		t.Fatalf("invalid selector emitted a misleading no-class warning: %s", event)
	default:
	}
	// Assignment and condition updates own separate fields on the legacy endpoint.
	if _, err = clients.EgressIPClient.K8sV1().EgressIPs().Patch(ctx, "one", ktypes.MergePatchType,
		[]byte(`{"status":{"items":[{"node":"replacement","egressIP":"192.0.2.11"}]}}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("patch assignment items: %v", err)
	}
	if !reflect.DeepEqual(before, get("one").Status.Conditions) {
		t.Fatal("assignment patch erased conditions")
	}
	if err = wf.TrafficClassInformer().Informer().GetStore().Delete(peerClass); err != nil {
		t.Fatalf("delete peer TrafficClass from informer: %v", err)
	}
	if err = e.reconcileTrafficConditions(context.Background()); err != nil {
		t.Fatalf("reconcile deleted TrafficClass: %v", err)
	}
	check("one", trafficConflict, metav1.ConditionFalse)
	check("two", trafficSelectorResolved, metav1.ConditionFalse)
	check("two", trafficConflict, metav1.ConditionFalse)
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "NoTrafficClass") {
			t.Fatal(event)
		}
	default:
		t.Fatal("missing warning")
	}
	if meta.FindStatusCondition(get("one").Status.Conditions, "OtherWriter") == nil || get("one").Status.Items[0].Node != "replacement" {
		t.Fatal("condition patch lost another writer's state")
	}
	// A peer selecting a different actual pod is not a conflict.
	if err = wf.TrafficClassInformer().Informer().GetStore().Add(peerClass); err != nil {
		t.Fatalf("restore peer TrafficClass in informer: %v", err)
	}
	peer = get("two")
	peer.Spec.PodSelector.MatchLabels["app"] = "another-app"
	if _, err = clients.EgressIPClient.K8sV1().EgressIPs().Update(ctx, peer, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update peer pod selector: %v", err)
	}
	if err = wf.EgressIPInformer().Informer().GetStore().Update(peer); err != nil {
		t.Fatalf("deliver peer selector to informer: %v", err)
	}
	if err = e.reconcileTrafficConditions(context.Background()); err != nil {
		t.Fatalf("reconcile peer selector change: %v", err)
	}
	check("one", trafficConflict, metav1.ConditionFalse)
	// Removing a selector removes only our conditions.
	eip = get("one")
	eip.Spec.TrafficSelector = nil
	if _, err = clients.EgressIPClient.K8sV1().EgressIPs().Update(ctx, eip, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("remove traffic selector: %v", err)
	}
	if err = wf.EgressIPInformer().Informer().GetStore().Update(eip); err != nil {
		t.Fatalf("deliver removed selector to informer: %v", err)
	}
	if err = e.reconcileTrafficConditions(context.Background()); err != nil {
		t.Fatalf("reconcile removed selector: %v", err)
	}
	if len(get("one").Status.Conditions) != 1 {
		t.Fatal("selector removal left traffic conditions")
	}
	if _, err = clients.EgressIPClient.K8sV1().EgressIPs().Patch(ctx, "one", ktypes.MergePatchType,
		[]byte(`{"status":{"items":[]}}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("clear assignment items: %v", err)
	}
	if items := get("one").Status.Items; items == nil || len(items) != 0 {
		t.Fatal("empty assignment patch must retain the required items array")
	}
}

func TestTrafficConditionRelevantEvents(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "one"}}, Spec: corev1.PodSpec{NodeName: "node"}}
	updated := pod.DeepCopy()
	updated.Status.PodIP = "10.128.0.2"
	if trafficConditionNeedsUpdate(pod, updated) {
		t.Fatal("unrelated pod status change triggered a global overlap scan")
	}
	updated.Labels["app"] = "two"
	if !trafficConditionNeedsUpdate(pod, updated) {
		t.Fatal("workload selector change was missed")
	}
	updated = pod.DeepCopy()
	updated.Status.Phase = corev1.PodSucceeded
	if !trafficConditionNeedsUpdate(pod, updated) {
		t.Fatal("completed pod could leave a stale conflict")
	}
	eip := &eipv1.EgressIP{}
	changed := eip.DeepCopy()
	changed.Status.Conditions = []metav1.Condition{{Type: trafficConflict, Status: metav1.ConditionTrue}}
	if !trafficConditionNeedsUpdate(eip, changed) {
		t.Fatal("managed condition change must requeue a potentially stale selection")
	}
	if trafficConditionNeedsUpdate(changed, changed.DeepCopy()) {
		t.Fatal("unchanged managed conditions triggered another scan")
	}
	if !trafficConditionNeedsUpdate(changed, eip) {
		t.Fatal("managed condition removal was missed")
	}
	unrelatedStatus := eip.DeepCopy()
	unrelatedStatus.Status.Items = []eipv1.EgressIPStatusItem{{Node: "other", EgressIP: "192.0.2.1"}}
	unrelatedStatus.Status.Conditions = []metav1.Condition{{Type: "OtherWriter", Status: metav1.ConditionTrue}}
	if trafficConditionNeedsUpdate(eip, unrelatedStatus) {
		t.Fatal("assignment or unrelated condition change triggered a scan")
	}
	changed.Spec.TrafficSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "one"}}
	if !trafficConditionNeedsUpdate(eip, changed) {
		t.Fatal("traffic selector change was missed")
	}
}

func TestDisabledTrafficConditionsWithoutClassInformer(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("prepare test config: %v", err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "disabled"}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": "one"}}}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: "node", EgressIP: "192.0.2.1"}}}}
	clients := util.GetOVNClientset(owner)
	wf, err := factory.NewOVNKubeControllerWatchFactory(clients.GetOVNKubeControllerClientset(), "node")
	if err != nil {
		t.Fatalf("create controller watch factory: %v", err)
	}
	t.Cleanup(wf.Stop)
	if err = wf.EgressIPInformer().Informer().GetStore().Add(owner); err != nil {
		t.Fatalf("add EgressIP to informer: %v", err)
	}
	recorder := record.NewFakeRecorder(10)
	controller := newTrafficConditionTestController(t, clients, wf, recorder, "node")
	if err = controller.reconcileTrafficConditions(context.Background()); err != nil {
		t.Fatalf("reconcile disabled gate: %v", err)
	}
	got, err := clients.EgressIPClient.K8sV1().EgressIPs().Get(context.Background(), owner.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get disabled-gate EgressIP: %v", err)
	}
	condition := meta.FindStatusCondition(got.Status.Conditions, trafficSelectorResolved)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "FeatureGateDisabled" || !reflect.DeepEqual(got.Status.Items, owner.Status.Items) {
		t.Fatalf("disabled result: %+v", got.Status)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "FeatureGateDisabled") {
			t.Fatal(event)
		}
	default:
		t.Fatal("missing disabled-gate event")
	}
}

func TestTrafficResolutionWithoutPodsAndWithUnsupportedEntries(t *testing.T) {
	owner := &eipv1.EgressIP{Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": "one"}}}}
	class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"class": "one"}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "192.0.2.0/24"}}}}
	result, err := selectEgressTraffic(owner, []*trafficv1.TrafficClass{class}, nil, nil)
	if err != nil || result.resolved.Status != metav1.ConditionTrue || result.resolved.Reason != "MatchesFound" || len(result.pods) != 0 {
		t.Fatalf("no-pod resolution: %+v %v", result, err)
	}
	class.Spec.Matches[0].Protocols = []trafficv1.ProtocolMatch{{}}
	result, err = selectEgressTraffic(owner, []*trafficv1.TrafficClass{class}, nil, nil)
	if err != nil || result.resolved.Status != metav1.ConditionFalse || result.resolved.Reason != "AllMatchesUnsupported" {
		t.Fatalf("unsupported resolution: %+v %v", result, err)
	}
}

func TestOverlappingTrafficClassesForOneOwner(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableTrafficClass = true
	owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "owner"}, Spec: eipv1.EgressIPSpec{
		TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "shared"}},
	}}
	class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "broad", Labels: map[string]string{"traffic": "shared"}},
		Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "192.168.250.0/24"}}}}
	nested := class.DeepCopy()
	nested.Name = "nested"
	nested.Spec.Matches[0].DestinationCIDR = "192.168.250.128/25"
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: namespace.Name}, Spec: corev1.PodSpec{NodeName: "node"}}
	clients := util.GetOVNClientset(owner, class, nested, namespace, pod)
	wf, err := factory.NewOVNKubeControllerWatchFactory(clients.GetOVNKubeControllerClientset(), "node")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(wf.Stop)
	for _, pair := range []struct {
		obj runtime.Object
		add func(interface{}) error
	}{
		{owner, wf.EgressIPInformer().Informer().GetStore().Add},
		{class, wf.TrafficClassInformer().Informer().GetStore().Add},
		{nested, wf.TrafficClassInformer().Informer().GetStore().Add},
		{namespace, wf.NamespaceCoreInformer().Informer().GetStore().Add},
		{pod, wf.PodCoreInformer().Informer().GetStore().Add},
	} {
		g.Expect(pair.add(pair.obj)).To(gomega.Succeed(), "populate informer with %T", pair.obj)
	}
	selection, err := egressip.ResolvePodTraffic([]*eipv1.EgressIP{owner}, []*trafficv1.TrafficClass{class, nested})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(selection.DestinationsFor(owner.Name)).To(gomega.ConsistOf(
		egressip.Destination{CIDR: netip.MustParsePrefix("192.168.250.0/24")},
		egressip.Destination{CIDR: netip.MustParsePrefix("192.168.250.128/25")},
	))
	g.Expect(selection.SelectPod(pod, namespace.Labels)).To(gomega.HaveKeyWithValue(owner.Name, gomega.BeNil()))
	controller := newTrafficConditionTestController(t, clients, wf, record.NewFakeRecorder(10), "node")
	g.Expect(controller.reconcileTrafficConditions(context.Background())).To(gomega.Succeed())
	current, err := clients.EgressIPClient.K8sV1().EgressIPs().Get(context.Background(), owner.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	resolved := meta.FindStatusCondition(current.Status.Conditions, trafficSelectorResolved)
	g.Expect(resolved).NotTo(gomega.BeNil())
	g.Expect(resolved.Status).To(gomega.Equal(metav1.ConditionTrue))
	conflict := meta.FindStatusCondition(current.Status.Conditions, trafficConflict)
	g.Expect(conflict).NotTo(gomega.BeNil())
	g.Expect(conflict.Status).To(gomega.Equal(metav1.ConditionFalse))
}

func TestUnsupportedProtocolWarningsOnChange(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("supported entries=%t", mixed), func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			t.Cleanup(func() { _ = config.PrepareTestConfig() })
			config.OVNKubernetesFeature.EnableEgressIP = true
			config.OVNKubernetesFeature.EnableTrafficClass = true
			owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "owner"}, Spec: eipv1.EgressIPSpec{
				TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"traffic": "one"}},
			}}
			class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "one", UID: "class-one", Generation: 1, Labels: map[string]string{"traffic": "one"}},
				Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "192.0.2.0/24", Protocols: []trafficv1.ProtocolMatch{{}}}}}}
			if mixed {
				class.Spec.Matches[0].Protocols = append(class.Spec.Matches[0].Protocols, trafficv1.ProtocolMatch{TCP: &trafficv1.PortProtocol{}})
			}
			clients := util.GetOVNClientset(owner, class)
			wf, err := factory.NewOVNKubeControllerWatchFactory(clients.GetOVNKubeControllerClientset(), "node")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(wf.Stop)
			g.Expect(wf.EgressIPInformer().Informer().GetStore().Add(owner)).To(gomega.Succeed())
			g.Expect(wf.TrafficClassInformer().Informer().GetStore().Add(class)).To(gomega.Succeed())
			recorder := record.NewFakeRecorder(10)
			e := newTrafficConditionTestController(t, clients, wf, recorder, "node")
			checkWarnings := func(want int) {
				t.Helper()
				g.Expect(e.reconcileTrafficConditions(context.Background())).To(gomega.Succeed())
				count := 0
				for len(recorder.Events) > 0 {
					if event := <-recorder.Events; strings.Contains(event, "UnsupportedProtocol") {
						g.Expect(event).To(gomega.ContainSubstring("TrafficClass one match 0 protocol entry"))
						count++
					}
				}
				g.Expect(count).To(gomega.Equal(want))
			}
			checkWarnings(1)
			checkWarnings(0)
			// Metadata updates do not repeat warnings; changed unsupported entries do.
			class = class.DeepCopy()
			class.Annotations = map[string]string{"unrelated": "change"}
			g.Expect(wf.TrafficClassInformer().Informer().GetStore().Update(class)).To(gomega.Succeed())
			checkWarnings(0)
			class = class.DeepCopy()
			class.Generation++
			class.Spec.Matches[0].Protocols = append(class.Spec.Matches[0].Protocols, trafficv1.ProtocolMatch{})
			g.Expect(wf.TrafficClassInformer().Informer().GetStore().Update(class)).To(gomega.Succeed())
			checkWarnings(2)
			checkWarnings(0)
		})
	}
}

func newTrafficConditionTestController(t *testing.T, clients *util.OVNClientset, wf *factory.WatchFactory, recorder record.EventRecorder, nodeName string) *EgressIPController {
	t.Helper()
	nb, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	if err != nil {
		t.Fatalf("create NB test harness: %v", err)
	}
	t.Cleanup(cleanup.Cleanup)
	return NewEIPController(nb, &kube.KubeOVN{EIPClient: clients.EgressIPClient}, wf, recorder, nil,
		&networkmanager.FakeNetworkManager{}, addressset.NewOvnAddressSetFactory(nb, true, false), nil,
		true, false, nodeName, types.DefaultNetworkControllerName)
}

func TestTrafficConditionsAcrossControllers(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("prepare test config: %v", err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableTrafficClass = true
	owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "one"}, Spec: eipv1.EgressIPSpec{
		TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": "shared"}},
		PodSelector:     metav1.LabelSelector{MatchLabels: map[string]string{"app": "shared"}},
	}}
	peer := owner.DeepCopy()
	peer.Name = "two"
	peer.Spec.PodSelector = metav1.LabelSelector{}
	class := &trafficv1.TrafficClass{ObjectMeta: metav1.ObjectMeta{Name: "shared", Labels: map[string]string{"class": "shared"}}, Spec: trafficv1.TrafficClassSpec{Matches: []trafficv1.TrafficMatch{{DestinationCIDR: "192.0.2.0/24"}}}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "work"}}
	local := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: ns.Name, Labels: map[string]string{"app": "local"}}, Spec: corev1.PodSpec{NodeName: "local"}}
	remote := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "remote", Namespace: ns.Name, Labels: map[string]string{"app": "shared"}}, Spec: corev1.PodSpec{NodeName: "remote"}}
	clients := util.GetOVNClientset(owner, peer, class, ns, local, remote)
	var controllers []*EgressIPController
	for _, name := range []string{"local", "remote"} {
		wf, err := factory.NewOVNKubeControllerWatchFactory(clients.GetOVNKubeControllerClientset(), name)
		if err != nil {
			t.Fatalf("create controller watch factory: %v", err)
		}
		t.Cleanup(wf.Stop)
		for _, obj := range []*eipv1.EgressIP{owner, peer} {
			if err = wf.EgressIPInformer().Informer().GetStore().Add(obj.DeepCopy()); err != nil {
				t.Fatalf("add EgressIP to informer: %v", err)
			}
		}
		if err = wf.TrafficClassInformer().Informer().GetStore().Add(class.DeepCopy()); err != nil {
			t.Fatalf("add TrafficClass to informer: %v", err)
		}
		if err = wf.NamespaceCoreInformer().Informer().GetStore().Add(ns.DeepCopy()); err != nil {
			t.Fatalf("add namespace to informer: %v", err)
		}
		for _, pod := range []*corev1.Pod{local, remote} {
			if err = wf.PodCoreInformer().Informer().GetStore().Add(pod.DeepCopy()); err != nil {
				t.Fatalf("add pod to informer: %v", err)
			}
		}
		controllers = append(controllers, newTrafficConditionTestController(t, clients, wf, record.NewFakeRecorder(10), name))
	}
	checkConflict := func(status metav1.ConditionStatus) {
		t.Helper()
		got, err := clients.EgressIPClient.K8sV1().EgressIPs().Get(context.Background(), peer.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get peer EgressIP: %v", err)
		}
		condition := meta.FindStatusCondition(got.Status.Conditions, trafficConflict)
		if condition == nil || condition.Status != status {
			t.Fatalf("global conflict = %+v, want %s", condition, status)
		}
		if status == metav1.ConditionTrue && !strings.Contains(condition.Message, "work/remote") {
			t.Fatalf("remote conflict missing: %+v", condition)
		}
	}
	if err := controllers[0].reconcileTrafficConditions(context.Background()); err != nil {
		t.Fatalf("reconcile first controller: %v", err)
	}
	checkConflict(metav1.ConditionTrue)
	fakeClient := clients.EgressIPClient.(*eipfake.Clientset)
	fakeClient.ClearActions()
	if err := controllers[1].reconcileTrafficConditions(context.Background()); err != nil {
		t.Fatalf("reconcile second controller: %v", err)
	}
	checkConflict(metav1.ConditionTrue)
	for _, action := range fakeClient.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatal("another controller rewrote an equivalent global result")
		}
	}
	for _, controller := range controllers {
		if err := controller.watchFactory.PodCoreInformer().Informer().GetStore().Delete(remote); err != nil {
			t.Fatalf("delete remote pod from informer: %v", err)
		}
	}
	if err := controllers[1].reconcileTrafficConditions(context.Background()); err != nil {
		t.Fatalf("reconcile remote pod deletion: %v", err)
	}
	checkConflict(metav1.ConditionFalse)
	if err := controllers[0].reconcileTrafficConditions(context.Background()); err != nil {
		t.Fatalf("reconcile peer controller after deletion: %v", err)
	}
	checkConflict(metav1.ConditionFalse)
}

func TestTrafficConditionPatchRetriesConcurrentAssignment(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("prepare test config: %v", err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "one", UID: "one", ResourceVersion: "1"}, Status: eipv1.EgressIPStatus{Items: []eipv1.EgressIPStatusItem{{Node: "old", EgressIP: "192.0.2.1"}}}}
	clients := util.GetOVNClientset(owner)
	wf, err := factory.NewOVNKubeControllerWatchFactory(clients.GetOVNKubeControllerClientset(), "node")
	if err != nil {
		t.Fatalf("create controller watch factory: %v", err)
	}
	t.Cleanup(wf.Stop)
	recorder := record.NewFakeRecorder(10)
	e := newTrafficConditionTestController(t, clients, wf, recorder, "node")
	client := clients.EgressIPClient.(*eipfake.Clientset)
	patches := 0
	client.PrependReactor("patch", "egressips", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patches++
		var patch struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
			Status map[string]json.RawMessage `json:"status"`
		}
		if err := json.Unmarshal(action.(clienttesting.PatchAction).GetPatch(), &patch); err != nil {
			t.Fatalf("decode condition patch: %v", err)
		}
		if len(patch.Status) != 1 || patch.Status["conditions"] == nil {
			t.Fatal("condition patch owns fields outside conditions")
		}
		if patches == 1 {
			updated := owner.DeepCopy()
			updated.ResourceVersion = "2"
			updated.Status.Items[0].Node = "replacement"
			if err := client.Tracker().Update(eipv1.SchemeGroupVersion.WithResource("egressips"), updated, ""); err != nil {
				t.Fatalf("simulate concurrent assignment write: %v", err)
			}
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "k8s.ovn.org", Resource: "egressips"}, owner.Name, fmt.Errorf("assignment changed"))
		}
		if patch.Metadata.ResourceVersion != "2" {
			t.Fatalf("retry used stale resourceVersion %q", patch.Metadata.ResourceVersion)
		}
		return false, nil, nil
	})
	desired := []metav1.Condition{{Type: trafficSelectorResolved, Status: metav1.ConditionFalse, Reason: "FeatureGateDisabled", Message: "disabled"}}
	if err = e.patchTrafficConditions(context.Background(), owner, desired); err != nil {
		t.Fatalf("patch traffic conditions: %v", err)
	}
	got, err := clients.EgressIPClient.K8sV1().EgressIPs().Get(context.Background(), owner.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get patched EgressIP: %v", err)
	}
	if patches != 2 || got.Status.Items[0].Node != "replacement" || meta.FindStatusCondition(got.Status.Conditions, trafficSelectorResolved) == nil {
		t.Fatalf("concurrent assignment was lost: patches=%d status=%+v", patches, got.Status)
	}
	// A second writer with an older informer snapshot must observe the equivalent live result.
	if err = e.patchTrafficConditions(context.Background(), owner, desired); err != nil {
		t.Fatalf("patch with stale informer snapshot: %v", err)
	}
	if patches != 2 {
		t.Fatal("stale informer snapshot caused a redundant condition write")
	}
	if len(recorder.Events) != 1 {
		t.Fatal("conflict retries or equivalent writes duplicated the transition event")
	}
	// A write can reach the API before its corresponding informer event.
	snapshot := got.DeepCopy()
	current := got.DeepCopy()
	current.Status.Conditions = nil
	if err = client.Tracker().Update(eipv1.SchemeGroupVersion.WithResource("egressips"), current, ""); err != nil {
		t.Fatalf("simulate condition change before informer delivery: %v", err)
	}
	client.ClearActions()
	if err = e.patchTrafficConditions(context.Background(), snapshot, desired); err != nil {
		t.Fatalf("reconcile matching stale snapshot: %v", err)
	}
	if actions := client.Actions(); len(actions) != 0 {
		t.Fatalf("matching snapshot made API calls: %+v", actions)
	}
	// Delivery must schedule another pass; the stale cached no-op is not the last word.
	if !trafficConditionNeedsUpdate(snapshot, current) {
		t.Fatal("delayed managed-condition event did not request another reconciliation")
	}
	if err = wf.EgressIPInformer().Informer().GetStore().Update(current); err != nil {
		t.Fatalf("deliver delayed condition change: %v", err)
	}
	fresh, err := wf.GetEgressIP(owner.Name)
	if err != nil {
		t.Fatalf("read delivered condition snapshot: %v", err)
	}
	if err = e.patchTrafficConditions(context.Background(), fresh, desired); err != nil {
		t.Fatalf("restore conditions after informer delivery: %v", err)
	}
	got, err = clients.EgressIPClient.K8sV1().EgressIPs().Get(context.Background(), owner.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get restored conditions: %v", err)
	}
	if meta.FindStatusCondition(got.Status.Conditions, trafficSelectorResolved) == nil || patches != 3 || got.Status.Items[0].Node != "replacement" {
		t.Fatalf("delayed event did not restore conditions while preserving assignment: patches=%d status=%+v", patches, got.Status)
	}

}

func TestTrafficConditionControllerLifecycle(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			t.Cleanup(func() { _ = config.PrepareTestConfig() })
			config.OVNKubernetesFeature.EnableEgressIP = true
			config.OVNKubernetesFeature.EnableTrafficClass = enabled
			owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "one"}, Spec: eipv1.EgressIPSpec{TrafficSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"class": "missing"}}}}
			clients := util.GetOVNClientset(owner)
			wf, err := factory.NewOVNKubeControllerWatchFactory(clients.GetOVNKubeControllerClientset(), "node")
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(wf.Shutdown)
			e := newTrafficConditionTestController(t, clients, wf, record.NewFakeRecorder(10), "node")
			g.Expect(wf.Start()).To(gomega.Succeed())
			stop := make(chan struct{})
			defer close(stop)
			g.Expect(e.StartTrafficConditions(stop)).To(gomega.Succeed())
			defer e.StopTrafficConditions()
			reason := "FeatureGateDisabled"
			if enabled {
				reason = "NoMatchingTrafficClass"
			}
			g.Eventually(func() string {
				got, err := clients.EgressIPClient.K8sV1().EgressIPs().Get(context.Background(), owner.Name, metav1.GetOptions{})
				if err != nil {
					return ""
				}
				condition := meta.FindStatusCondition(got.Status.Conditions, trafficSelectorResolved)
				if condition == nil {
					return ""
				}
				return condition.Reason
			}, "5s").Should(gomega.Equal(reason), "traffic-condition worker did not publish the expected reason")
			other := metav1.Condition{Type: "OtherWriter", Status: metav1.ConditionTrue, Reason: "Healthy", Message: "keep", LastTransitionTime: metav1.Now()}
			patch, err := json.Marshal(map[string]interface{}{"status": map[string]interface{}{"conditions": []metav1.Condition{other}}})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = clients.EgressIPClient.K8sV1().EgressIPs().Patch(context.Background(), owner.Name, ktypes.MergePatchType, patch, metav1.PatchOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Eventually(func() bool {
				got, err := clients.EgressIPClient.K8sV1().EgressIPs().Get(context.Background(), owner.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}
				condition := meta.FindStatusCondition(got.Status.Conditions, trafficSelectorResolved)
				return condition != nil && condition.Reason == reason && meta.FindStatusCondition(got.Status.Conditions, "OtherWriter") != nil
			}, "5s").Should(gomega.BeTrue(), "managed-condition events must restore selection without removing another writer's condition")
			e.StopTrafficConditions()
			g.Expect(e.trafficConditionHandlers).To(gomega.BeEmpty())
		})
	}
}

func TestTrafficConditionStartupCancellation(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	clients := util.GetOVNClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(clients.GetOVNKubeControllerClientset(), "node")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(wf.Stop)
	e := newTrafficConditionTestController(t, clients, wf, record.NewFakeRecorder(10), "node")
	stop := make(chan struct{})
	close(stop)
	g.Expect(e.StartTrafficConditions(stop)).NotTo(gomega.Succeed())
	g.Expect(e.trafficConditionHandlers).To(gomega.BeEmpty())
	e.StopTrafficConditions()
}
