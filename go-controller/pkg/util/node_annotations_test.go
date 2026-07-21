package util

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeCloudEgressIPConfigAnnotationChanged(t *testing.T) {
	tests := []struct {
		name           string
		oldAnnotations map[string]string
		newAnnotations map[string]string
		expected       bool
	}{
		{
			name: "annotation changed - capacity increased",
			oldAnnotations: map[string]string{
				"cloud.network.openshift.io/egress-ipconfig": `[{"interface":"eth0","ifaddr":{"ipv4":"10.0.0.0/24"},"capacity":{"ipv4":0}}]`,
			},
			newAnnotations: map[string]string{
				"cloud.network.openshift.io/egress-ipconfig": `[{"interface":"eth0","ifaddr":{"ipv4":"10.0.0.0/24"},"capacity":{"ipv4":1}}]`,
			},
			expected: true,
		},
		{
			name: "annotation unchanged",
			oldAnnotations: map[string]string{
				"cloud.network.openshift.io/egress-ipconfig": `[{"interface":"eth0","ifaddr":{"ipv4":"10.0.0.0/24"},"capacity":{"ipv4":1}}]`,
			},
			newAnnotations: map[string]string{
				"cloud.network.openshift.io/egress-ipconfig": `[{"interface":"eth0","ifaddr":{"ipv4":"10.0.0.0/24"},"capacity":{"ipv4":1}}]`,
			},
			expected: false,
		},
		{
			name:           "annotation added",
			oldAnnotations: map[string]string{},
			newAnnotations: map[string]string{
				"cloud.network.openshift.io/egress-ipconfig": `[{"interface":"eth0","ifaddr":{"ipv4":"10.0.0.0/24"},"capacity":{"ipv4":1}}]`,
			},
			expected: true,
		},
		{
			name: "annotation removed",
			oldAnnotations: map[string]string{
				"cloud.network.openshift.io/egress-ipconfig": `[{"interface":"eth0","ifaddr":{"ipv4":"10.0.0.0/24"},"capacity":{"ipv4":1}}]`,
			},
			newAnnotations: map[string]string{},
			expected:       true,
		},
		{
			name:           "both nodes missing annotation",
			oldAnnotations: map[string]string{},
			newAnnotations: map[string]string{},
			expected:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: tt.oldAnnotations,
				},
			}
			newNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: tt.newAnnotations,
				},
			}

			result := NodeCloudEgressIPConfigAnnotationChanged(oldNode, newNode)
			if result != tt.expected {
				t.Errorf("NodeCloudEgressIPConfigAnnotationChanged() = %v, expected %v", result, tt.expected)
			}
		})
	}
}
