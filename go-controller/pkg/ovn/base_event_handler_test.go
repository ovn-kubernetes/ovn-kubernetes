// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestNamespaceAreResourcesEqual(t *testing.T) {
	handler := &baseNetworkControllerEventHandler{}

	tests := []struct {
		name     string
		old      *corev1.Namespace
		new      *corev1.Namespace
		expected bool
	}{
		{
			name: "identical namespaces",
			old: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-ns",
					Annotations: map[string]string{util.AclLoggingAnnotation: `{"deny":"alert"}`},
					Labels:      map[string]string{"env": "prod"},
				},
			},
			new: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-ns",
					Annotations: map[string]string{util.AclLoggingAnnotation: `{"deny":"alert"}`},
					Labels:      map[string]string{"env": "prod"},
				},
			},
			expected: true,
		},
		{
			name: "different ACL logging annotation",
			old: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-ns",
					Annotations: map[string]string{util.AclLoggingAnnotation: `{"deny":"alert"}`},
				},
			},
			new: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-ns",
					Annotations: map[string]string{util.AclLoggingAnnotation: `{"deny":"warning"}`},
				},
			},
			expected: false,
		},
		{
			name: "different multicast annotation",
			old: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-ns",
					Annotations: map[string]string{util.NsMulticastAnnotation: "false"},
				},
			},
			new: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-ns",
					Annotations: map[string]string{util.NsMulticastAnnotation: "true"},
				},
			},
			expected: false,
		},
		{
			name: "different labels",
			old: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-ns",
					Labels: map[string]string{"env": "prod"},
				},
			},
			new: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-ns",
					Labels: map[string]string{"env": "staging"},
				},
			},
			expected: false,
		},
		{
			name: "different non-OVN annotations are equal",
			old: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-ns",
					Annotations: map[string]string{"argocd.argoproj.io/managed-by": "v1"},
				},
			},
			new: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-ns",
					Annotations: map[string]string{"argocd.argoproj.io/managed-by": "v2"},
				},
			},
			expected: true,
		},
		{
			name: "nil vs empty annotations are equal",
			old: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "test-ns"},
			},
			new: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-ns",
					Annotations: map[string]string{},
				},
			},
			expected: true,
		},
		{
			name: "added label is not equal",
			old: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-ns",
					Labels: map[string]string{"env": "prod"},
				},
			},
			new: &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-ns",
					Labels: map[string]string{"env": "prod", "team": "networking"},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := handler.areResourcesEqual(factory.NamespaceType, tt.old, tt.new)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("got %v, want %v", result, tt.expected)
			}
		})
	}
}
