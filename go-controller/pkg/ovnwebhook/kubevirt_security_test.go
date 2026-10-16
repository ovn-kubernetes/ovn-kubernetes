// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"context"
	"testing"

	kubevirtv1 "kubevirt.io/api/core/v1"

	admv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const kubeVirtControllerUser = "system:serviceaccount:kubevirt:kubevirt-controller"

func kubeVirtAdmissionContext(user string) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{AdmissionRequest: admv1.AdmissionRequest{
		UserInfo: authenticationv1.UserInfo{Username: user},
	}})
}

func launcherPodForAdmission() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "launcher", Namespace: "tenant",
		Labels:          map[string]string{kubevirtv1.AppLabel: "virt-launcher", kubevirtv1.VirtualMachineNameLabel: "vm"},
		Annotations:     map[string]string{kubevirtv1.DomainAnnotation: "vm", kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: ""},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: kubevirtv1.SchemeGroupVersion.String(), Kind: "VirtualMachineInstance", Name: "vm", UID: "vmi-uid", Controller: ptr.To(true)}},
	}}
}

func TestKubeVirtNetworkIdentityCreate(t *testing.T) {
	validator := NewPodAdmissionWebhook(nil, nil, []string{kubeVirtControllerUser}, "network-controller")
	for _, tc := range []struct {
		name, user string
		mutate     func(*corev1.Pod)
		deny       bool
	}{
		{"forged launcher and owner", "pod-creator", nil, true},
		{"network writer cannot authorize migration", "network-controller", nil, true},
		{"selected service account is not requester identity", "pod-creator", func(p *corev1.Pod) { p.Spec.ServiceAccountName = "kubevirt-controller" }, true},
		{"KubeVirt source", kubeVirtControllerUser, nil, false},
		{"KubeVirt migration target", kubeVirtControllerUser, func(p *corev1.Pod) { p.Name = "target" }, false},
		{"template overrides domain", kubeVirtControllerUser, func(p *corev1.Pod) { p.Annotations[kubevirtv1.DomainAnnotation] = "victim" }, true},
		{"missing VMI owner", kubeVirtControllerUser, func(p *corev1.Pod) { p.OwnerReferences = nil }, true},
		{"wrong owner kind", kubeVirtControllerUser, func(p *corev1.Pod) { p.OwnerReferences[0].Kind = "VirtualMachine" }, true},
		{"missing owner UID", kubeVirtControllerUser, func(p *corev1.Pod) { p.OwnerReferences[0].UID = "" }, true},
		{"KubeVirt cannot inject allocation", kubeVirtControllerUser, func(p *corev1.Pod) { p.Annotations[types.OvnPodAnnotationName] = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := launcherPodForAdmission()
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			_, err := validator.ValidateCreate(kubeVirtAdmissionContext(tc.user), pod)
			if (err != nil) != tc.deny {
				t.Fatalf("error = %v, want denied = %v", err, tc.deny)
			}
		})
	}
	// An empty allowlist must fail closed rather than trust a username's shape.
	_, err := NewPodAdmissionWebhook(nil, nil, nil).ValidateCreate(kubeVirtAdmissionContext(kubeVirtControllerUser), launcherPodForAdmission())
	if err == nil {
		t.Fatal("unconfigured KubeVirt identity accepted")
	}
}

func TestKubeVirtProtectedFields(t *testing.T) {
	validator := NewPodAdmissionWebhook(nil, nil, []string{kubeVirtControllerUser})
	for _, field := range []struct {
		key   string
		label bool
	}{
		{kubevirtv1.DomainAnnotation, false}, {kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation, false},
		{kubevirtv1.MigrationTargetReadyTimestamp, false}, {kubevirtv1.VirtualMachineNameLabel, true},
		{kubevirtv1.NodeNameLabel, true}, {kubevirtv1.AppLabel, true},
	} {
		t.Run(field.key, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}, Annotations: map[string]string{}}}
			values := pod.Annotations
			if field.label {
				values = pod.Labels
			}
			values[field.key] = "virt-launcher"
			ctx := kubeVirtAdmissionContext("pod-creator")
			if _, err := validator.ValidateCreate(ctx, pod); err == nil {
				t.Fatal("forged field accepted on create")
			}

			changed := pod.DeepCopy()
			if field.label {
				changed.Labels[field.key] = "other"
			} else {
				changed.Annotations[field.key] = "other"
			}
			for _, pair := range [][2]*corev1.Pod{{{}, pod}, {pod, {}}, {pod, changed}} {
				if _, err := validator.ValidateUpdate(ctx, pair[0], pair[1]); err == nil {
					t.Fatal("forged field accepted on update")
				}
			}
		})
	}
}

func TestKubeVirtNetworkIdentityUpdate(t *testing.T) {
	validator := NewPodAdmissionWebhook(nil, nil, []string{kubeVirtControllerUser})
	for _, tc := range []struct {
		name, user string
		mutate     func(*corev1.Pod)
		deny       bool
	}{
		{"unrelated user update", "pod-creator", func(p *corev1.Pod) { p.Annotations["example.com/note"] = "ok" }, false},
		{"kubelet status", "system:node:node-a", func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }, false},
		{"garbage collector orphaning", "garbage-collector", func(p *corev1.Pod) { p.OwnerReferences = nil }, false},
		{"garbage collector unblocks deletion", "garbage-collector", func(p *corev1.Pod) { p.OwnerReferences[0].BlockOwnerDeletion = ptr.To(false) }, false},
		{"forged owner", "pod-creator", func(p *corev1.Pod) { p.OwnerReferences[0].UID = "victim" }, true},
		{"forged readiness", "pod-creator", func(p *corev1.Pod) { p.Annotations[kubevirtv1.MigrationTargetReadyTimestamp] = "ready" }, true},
		{"controller readiness", kubeVirtControllerUser, func(p *corev1.Pod) { p.Annotations[kubevirtv1.MigrationTargetReadyTimestamp] = "ready" }, false},
		{"controller node label", kubeVirtControllerUser, func(p *corev1.Pod) { p.Labels[kubevirtv1.NodeNameLabel] = "node-b" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldPod := launcherPodForAdmission()
			newPod := oldPod.DeepCopy()
			tc.mutate(newPod)
			_, err := validator.ValidateUpdate(kubeVirtAdmissionContext(tc.user), oldPod, newPod)
			if (err != nil) != tc.deny {
				t.Fatalf("error = %v, want denied = %v", err, tc.deny)
			}
		})
	}
}
