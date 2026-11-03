// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/uplink"
)

func TestUplinkStateAdmission(t *testing.T) {
	validator := NewUplinkStateAdmissionWebhook()
	stateFor := func(uplinkName, nodeName string) *uplinkv1alpha1.UplinkState {
		t.Helper()
		return &uplinkv1alpha1.UplinkState{
			ObjectMeta: metav1.ObjectMeta{Name: uplinkutil.StateName(uplinkName, nodeName)},
			Spec:       uplinkv1alpha1.UplinkStateSpec{UplinkName: uplinkName, NodeName: nodeName},
		}
	}
	ctxFor := func(username string) context.Context {
		return admission.NewContextWithRequest(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			UserInfo: authenticationv1.UserInfo{Username: username},
		}})
	}
	own := stateFor("blue", "node-a")
	for _, tc := range []struct {
		name, user string
		allowWrite bool
	}{
		{"owner", "system:ovn-node:node-a", true},
		{"other node", "system:ovn-node:node-b", false},
		{"malformed prefix", "system:ovn-node-evil:node-a", false},
		{"administrator", "admin", false},
		{"cluster manager cleanup", "system:serviceaccount:ovn-kubernetes:ovnkube-cluster-manager", false},
		{"garbage collection", "system:serviceaccount:kube-system:generic-garbage-collector", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := ctxFor(tc.user)
			if _, err := validator.ValidateCreate(ctx, own); (err == nil) != tc.allowWrite {
				t.Fatalf("create: %v", err)
			}
		})
	}
	ctx := ctxFor("system:ovn-node:node-a")
	for _, tc := range []struct {
		name   string
		mutate func(*uplinkv1alpha1.UplinkState)
	}{
		{"victim key", func(s *uplinkv1alpha1.UplinkState) { s.Name = "blue.node-b" }},
		{"forged hashed key", func(s *uplinkv1alpha1.UplinkState) { s.Name = "uplinkstate-a1234567" }},
		{"other node identity", func(s *uplinkv1alpha1.UplinkState) { s.Spec.NodeName = "node-b" }},
		{"finalizer", func(s *uplinkv1alpha1.UplinkState) { s.Finalizers = []string{"example.com/block"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forged := own.DeepCopy()
			tc.mutate(forged)
			if _, err := validator.ValidateCreate(ctx, forged); err == nil {
				t.Fatal("accepted forged creation")
			}
		})
	}
	t.Run("canonical hashed key", func(t *testing.T) {
		longName := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 60)
		state := stateFor(longName, "node-a")
		if !strings.HasPrefix(state.Name, "uplinkstate-") {
			t.Fatal("fixture is not hashed")
		}
		if _, err := validator.ValidateCreate(ctx, state); err != nil {
			t.Fatal(err)
		}
	})
}
