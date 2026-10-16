// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"context"
	"testing"

	admv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestPodNetworkAnnotationCreate(t *testing.T) {
	validator := NewPodAdmissionWebhook(nil, nil, nil)
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		deny        bool
	}{
		{"ordinary pod", nil, false},
		{"allocation result", map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["10.128.0.5/23"]}}`}, true},
		{"empty allocation result", map[string]string{types.OvnPodAnnotationName: ""}, true},
		{"static IP and claim request", map[string]string{"k8s.v1.cni.cncf.io/networks": `[{"name":"net","ips":["10.128.0.5/23"],"ipam-claim-reference":"claim"}]`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validator.ValidateCreate(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}})
			if (err != nil) != tc.deny {
				t.Fatalf("error = %v, want denied = %v", err, tc.deny)
			}
		})
	}
}

func TestPodNetworkAnnotationUpdate(t *testing.T) {
	validator := NewPodAdmissionWebhook(nil, nil, nil)
	for _, subresource := range []string{"", "status"} {
		for _, tc := range []struct {
			name     string
			old, new map[string]string
			deny     bool
		}{
			{"add", nil, map[string]string{types.OvnPodAnnotationName: "forged"}, true},
			{"change", map[string]string{types.OvnPodAnnotationName: "allocated"}, map[string]string{types.OvnPodAnnotationName: "forged"}, true},
			{"remove", map[string]string{types.OvnPodAnnotationName: "allocated"}, nil, true},
			{"unrelated update", map[string]string{types.OvnPodAnnotationName: "allocated"}, map[string]string{types.OvnPodAnnotationName: "allocated", "example.com/note": "note"}, false},
		} {
			t.Run(subresource+"/"+tc.name, func(t *testing.T) {
				ctx := admission.NewContextWithRequest(context.Background(), admission.Request{AdmissionRequest: admv1.AdmissionRequest{
					Operation: admv1.Update, SubResource: subresource, UserInfo: authenticationv1.UserInfo{Username: "pod-creator"},
				}})
				_, err := validator.ValidateUpdate(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: tc.old}}, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: tc.new}})
				if (err != nil) != tc.deny {
					t.Fatalf("error = %v, want denied = %v", err, tc.deny)
				}
			})
		}
	}
}
