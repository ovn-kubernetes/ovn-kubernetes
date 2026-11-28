// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPodAnnotationPatchRejectsRecreatedPod(t *testing.T) {
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod", UID: "old", Annotations: map[string]string{"network": "before"}}}
	next := old.DeepCopy()
	next.Annotations["network"] = "after"
	replacement := old.DeepCopy()
	replacement.UID = "new"
	client := fake.NewSimpleClientset(replacement)
	k := &Kube{KClient: client}
	if _, err := k.PatchPodStatusAnnotationsWithContext(context.Background(), old, next); err == nil {
		t.Fatal("stale writer patched replacement pod")
	}
	got, err := client.CoreV1().Pods("ns").Get(context.Background(), "pod", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations["network"] != "before" {
		t.Fatal("replacement annotations changed")
	}
}
