// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package adminnetworkpolicy

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

func TestDeleteHandlers(t *testing.T) {
	tests := []struct {
		name    string
		object  interface{}
		key     string
		handler func(*Controller, interface{})
	}{
		{"ANP", &anpapi.AdminNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "policy"}}, "policy", (*Controller).onANPDelete},
		{"BANP", &anpapi.BaselineAdminNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, "default", (*Controller).onBANPDelete},
		{"Namespace", &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "namespace"}}, "namespace", (*Controller).onANPNamespaceDelete},
		{"Pod", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "namespace"}}, "namespace/pod", (*Controller).onANPPodDelete},
		{"Node", &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}, "node", (*Controller).onANPNodeDelete},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, event := range []struct {
				name   string
				object interface{}
			}{
				{"object", tt.object},
				{"tombstone", cache.DeletedFinalStateUnknown{Key: tt.key, Obj: tt.object}},
				{"tombstone without object", cache.DeletedFinalStateUnknown{Key: tt.key}},
			} {
				t.Run(event.name, func(t *testing.T) {
					queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
					defer queue.ShutDown()
					controller := &Controller{
						anpQueue:          queue,
						banpQueue:         queue,
						anpNamespaceQueue: queue,
						anpPodQueue:       queue,
						anpNodeQueue:      queue,
					}
					tt.handler(controller, event.object)
					if queue.Len() != 1 {
						t.Fatalf("expected deletion to queue %q for reconciliation, got %d items", tt.key, queue.Len())
					}
					key, _ := queue.Get()
					defer queue.Done(key)
					if key != tt.key {
						t.Errorf("expected key %q, got %q", tt.key, key)
					}
				})
			}
		})
	}
}
