// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onsi/gomega"
	ocpconfigapi "github.com/openshift/api/config/v1"
	ocpcloudnetworkfake "github.com/openshift/client-go/cloudnetwork/clientset/versioned/fake"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	objretry "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestEgressIPAssignmentPatchPreservesConditions(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("prepare test config: %v", err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "one", Annotations: map[string]string{"other-writer": "keep"}}, Status: eipv1.EgressIPStatus{
		Items:      []eipv1.EgressIPStatusItem{{Node: "old", EgressIP: "192.0.2.1"}},
		Conditions: []metav1.Condition{{Type: "TrafficConflict", Status: metav1.ConditionTrue, Reason: "OverlappingTraffic", Message: "keep", LastTransitionTime: metav1.NewTime(time.Unix(1, 0))}},
	}}
	clients := util.GetOVNClientset(owner)
	wf, err := factory.NewClusterManagerWatchFactory(clients.GetClusterManagerClientset())
	if err != nil {
		t.Fatalf("create cluster-manager watch factory: %v", err)
	}
	t.Cleanup(wf.Stop)
	e := newEgressIPController(clients.GetClusterManagerClientset(), wf, record.NewFakeRecorder(10))
	ctx := context.Background()
	for _, items := range [][]eipv1.EgressIPStatusItem{
		{{Node: "replacement", EgressIP: "192.0.2.2"}}, nil,
	} {
		if err = e.patchEgressIP(ctx, owner.Name, egressIPPatch{Status: &egressIPItemsPatch{Items: items}}); err != nil {
			t.Fatalf("patch EgressIP assignments: %v", err)
		}
		got, err := clients.EgressIPClient.K8sV1().EgressIPs().Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get patched EgressIP: %v", err)
		}
		if !reflect.DeepEqual(got.Status.Conditions, owner.Status.Conditions) || !reflect.DeepEqual(got.Annotations, owner.Annotations) {
			t.Fatalf("assignment patch changed another writer's fields: %+v", got)
		}
		if got.Status.Items == nil || len(got.Status.Items) != len(items) {
			t.Fatalf("incorrect assignment array: %+v", got.Status.Items)
		}
		if len(items) > 0 && !reflect.DeepEqual(got.Status.Items, items) {
			t.Fatalf("assignment patch lost desired items: %+v", got.Status.Items)
		}
	}
}

func TestEgressIPAssignmentEventEquality(t *testing.T) {
	original := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "owner", UID: "owner-uid"}, Spec: eipv1.EgressIPSpec{EgressIPs: []string{"192.0.2.1"}}}
	for _, tc := range []struct {
		name   string
		edit   func(*eipv1.EgressIP)
		ignore bool
	}{
		{"conditions", func(eip *eipv1.EgressIP) {
			eip.Status.Conditions = []metav1.Condition{{Type: "TrafficConflict", Status: metav1.ConditionFalse}}
		}, true},
		{"spec", func(eip *eipv1.EgressIP) { eip.Spec.EgressIPs = []string{"192.0.2.2"} }, false},
		{"assignments", func(eip *eipv1.EgressIP) {
			eip.Status.Items = []eipv1.EgressIPStatusItem{{Node: "node", EgressIP: "192.0.2.1"}}
		}, false},
		{"annotations", func(eip *eipv1.EgressIP) { eip.Annotations = map[string]string{"key": "value"} }, false},
		{"labels", func(eip *eipv1.EgressIP) { eip.Labels = map[string]string{"key": "value"} }, false},
		{"replacement", func(eip *eipv1.EgressIP) { eip.UID = "replacement-uid" }, false},
		{"deletion", func(eip *eipv1.EgressIP) { eip.DeletionTimestamp = &metav1.Time{Time: time.Unix(1, 0)} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated := original.DeepCopy()
			tc.edit(updated)
			handler := &egressIPClusterControllerEventHandler{objType: factory.EgressIPType}
			equal, err := handler.AreResourcesEqual(original, updated)
			if err != nil || equal != tc.ignore {
				t.Fatalf("ignore %s update = %t, want %t (error %v)", tc.name, equal, tc.ignore, err)
			}
		})
	}
}

type observedAssignmentEvents struct {
	objretry.EventHandler
	compared chan bool
}

func (h *observedAssignmentEvents) AreResourcesEqual(old, new interface{}) (bool, error) {
	equal, err := h.EventHandler.AreResourcesEqual(old, new)
	h.compared <- equal
	return equal, err
}

func TestConditionUpdateDoesNotRepeatPendingCloudCreate(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "prepare cloud configuration")
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.Kubernetes.PlatformType = string(ocpconfigapi.AWSPlatformType)
	owner := &eipv1.EgressIP{ObjectMeta: metav1.ObjectMeta{Name: "owner", Annotations: createAnnotWithMark(util.EgressIPMarkBase)}, Spec: eipv1.EgressIPSpec{EgressIPs: []string{"192.0.2.1"}}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Annotations: map[string]string{
		util.OvnNodeIfAddr: `{"ipv4":"192.0.2.10/24"}`, util.OVNNodeHostCIDRs: `["192.0.2.10/24"]`,
	}}}
	clients := util.GetOVNClientset(owner, node)
	cloudClient := clients.CloudNetworkClient.(*ocpcloudnetworkfake.Clientset)
	cloudEvents := watch.NewRaceFreeFake()
	t.Cleanup(cloudEvents.Stop)
	cloudClient.PrependWatchReactor("cloudprivateipconfigs", clienttesting.DefaultWatchReactor(cloudEvents, nil))
	var creates atomic.Int32
	cloudClient.PrependReactor("create", "cloudprivateipconfigs", func(clienttesting.Action) (bool, runtime.Object, error) {
		creates.Add(1)
		return false, nil, nil
	})
	wf, err := factory.NewClusterManagerWatchFactory(clients.GetClusterManagerClientset())
	g.Expect(err).NotTo(gomega.HaveOccurred(), "create the cluster-manager watch factory")
	t.Cleanup(wf.Shutdown)
	recorder := record.NewFakeRecorder(10)
	e := newEgressIPController(clients.GetClusterManagerClientset(), wf, recorder)
	t.Cleanup(e.Stop)
	available := setupNode(node.Name, []string{"192.0.2.10/24"}, nil)
	e.nodeAllocator.cache[node.Name] = &available
	g.Expect(wf.Start()).To(gomega.Succeed(), "start the assignment and delayed cloud informers")
	observed := &observedAssignmentEvents{EventHandler: e.retryEgressIPs.ResourceHandler.EventHandler, compared: make(chan bool, 10)}
	e.retryEgressIPs.ResourceHandler.EventHandler = observed
	_, err = e.WatchEgressIP()
	g.Expect(err).NotTo(gomega.HaveOccurred(), "start the real assignment retry handler")
	g.Eventually(creates.Load, "5s").Should(gomega.Equal(int32(1)), "submit the initial cloud assignment")
	_, err = cloudClient.CloudV1().CloudPrivateIPConfigs().Get(context.Background(), owner.Spec.EgressIPs[0], metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "accept creation in the API")
	_, err = wf.GetCloudPrivateIPConfig(owner.Spec.EgressIPs[0])
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue(), "hold cloud informer delivery after API creation")
	updated := owner.DeepCopy()
	updated.Status.Conditions = []metav1.Condition{{Type: "TrafficConflict", Status: metav1.ConditionFalse, Reason: "NoOverlappingTraffic", LastTransitionTime: metav1.Now()}}
	_, err = clients.EgressIPClient.K8sV1().EgressIPs().Update(context.Background(), updated, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "publish a condition while assignment confirmation is pending")
	g.Eventually(observed.compared, "5s").Should(gomega.Receive(gomega.BeTrue()), "filter the delivered update before retry processing")
	g.Expect(creates.Load()).To(gomega.Equal(int32(1)), "do not repeat the accepted cloud create")
	g.Expect(objretry.CheckRetryObj(owner.Name, e.retryEgressIPs)).To(gomega.BeFalse(), "do not enqueue an AlreadyExists retry")
	g.Expect(recorder.Events).NotTo(gomega.Receive(), "do not report a spurious CloudAssignmentFailed warning")
}
