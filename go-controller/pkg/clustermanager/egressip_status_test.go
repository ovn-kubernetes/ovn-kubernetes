// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
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
