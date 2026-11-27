// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"

	admv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type fakeNodeLister struct {
	nodes map[string]*corev1.Node
}

func (f *fakeNodeLister) List(labels.Selector) (ret []*corev1.Node, err error) {
	panic("implement me")
}

func (f *fakeNodeLister) Get(name string) (*corev1.Node, error) {
	node, ok := f.nodes[name]
	if !ok {
		return nil, fmt.Errorf("nodr %q not found", name)
	}
	return node, nil
}

var _ listersv1.NodeLister = &fakeNodeLister{}

const podName = "testpod"

func TestPodAdmission_ValidateUpdate(t *testing.T) {
	tests := []struct {
		name        string
		node        *corev1.Node
		ctx         context.Context
		oldObj      *corev1.Pod
		newObj      *corev1.Pod
		expectedErr error
	}{
		{
			name: "allow if user is not ovnkube-node and doesn't modify ovnkube-node annotations",
			node: &corev1.Node{},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: "system:nodes:node",
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   podName,
					Labels: map[string]string{"key": "old"},
				},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:   podName,
					Labels: map[string]string{"key": "new"},
				},
			},
			expectedErr: nil,
		},
		{
			name: "error out if different user tries to set ovnkube-node annotations",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: "system:nodes:node",
				}},
			}),
			node: &corev1.Node{},
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: "old"},
				},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: "new"},
				},
			},
			expectedErr: fmt.Errorf("user %q is not allowed to set the following annotations on pod: %q: %v", "system:nodes:node", podName, []string{types.OvnPodAnnotationName}),
		},
		{
			name: "error out if the request is not in context",
			node: &corev1.Node{},
			ctx:  context.TODO(),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: podName,
				},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{"new": "value"},
				},
			},
			expectedErr: errors.New("admission.Request not found in context"),
		},
		{
			name: "ovnkube-node cannot modify annotations on pods running on different nodes",
			node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name:        nodeName + "_rougeOne",
				Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"192.168.0.0/24"}`},
			}},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName + "_rougeOne",
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:0a:80:00:05"}}`},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["192.168.0.10/24"],"mac_address":"0a:58:0a:80:00:05"}}`},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			expectedErr: fmt.Errorf("ovnkube-node on node: %q is not allowed to modify pods %q annotations", nodeName+"_rougeOne", podName),
		},
		{
			name: "ovnkube-node cannot modify pod annotations that do not belong to it",
			node: &corev1.Node{},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName + "bad": "old"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName + "bad": "new"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			expectedErr: fmt.Errorf("ovnkube-node on node: %q is not allowed to set the following annotations on pod: %q: %v", nodeName, podName, []string{types.OvnPodAnnotationName + "bad"}),
		},
		{
			name: "ovnkube-node can modify OvnPodAnnotationName annotation on a pod",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"192.168.0.0/24"}`},
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:0a:80:00:05"}}`},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["192.168.0.10/24"],"mac_address":"0a:58:0a:80:00:05"}}`},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
		},
		{
			name: "ovnkube-node cannot modify an IP in the OvnPodAnnotationName annotation to an invalid value",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"192.168.0.0/24"}`},
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:0a:80:00:05"}}`},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["10.10.10.10/24"],"mac_address":"0a:58:0a:80:00:05"}}`},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			expectedErr: fmt.Errorf("user: %q is not allowed to set %s on pod %q: 10.10.10.10/24 does not belong to %s node", userName, types.OvnPodAnnotationName, podName, nodeName),
		},
		{
			name: "ovnkube-node cannot set an IP in the OvnPodAnnotationName annotation on host-networked pods",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"192.168.0.0/24"}`},
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: podName,
				},
				Spec: corev1.PodSpec{NodeName: nodeName, HostNetwork: true},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:0a:80:00:05"}}`},
				},
				Spec: corev1.PodSpec{NodeName: nodeName, HostNetwork: true},
			},
			expectedErr: fmt.Errorf("user: %q is not allowed to set %s on pod %q: the annotation is not allowed on host networked pods", userName, types.OvnPodAnnotationName, podName),
		},
		{
			name: "ovnkube-node can use an IP in OvnPodAnnotationName annotation that belongs to a different node in kubevirt live-migration",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"192.168.0.0/24"}`},
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: ""},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["10.10.10.10/24"],"mac_address":"0a:58:0a:80:00:05"}}`,
						kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: ""},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
		},
		{
			name: "ovnkube-node can modify DPUConnectionDetailsAnnot annotation on a pod",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"192.168.0.0/24"}`},
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{util.DPUConnectionDetailsAnnot: "old"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{util.DPUConnectionDetailsAnnot: "new"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
		},
		{
			name: "ovnkube-node can modify DPUConnetionStatusAnnot annotation on a pod",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"192.168.0.0/24"}`},
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{util.DPUConnectionStatusAnnot: "old"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{util.DPUConnectionStatusAnnot: "new"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
		},
		{
			name: "ovnkube-node cannot modify anything other than pods annotations",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"192.168.0.0/24"}`},
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:0a:80:00:05"}}`},
					Labels:      map[string]string{"key": "old"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["192.168.0.10/24"],"mac_address":"0a:58:0a:80:00:05"}}`},
					Labels:      map[string]string{"key": "new"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			expectedErr: fmt.Errorf("ovnkube-node on node: %q is not allowed to modify anything other than annotations", nodeName),
		},
		// additional acceptance conditions
		{
			name: "additonal acceptance conditions valid",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: additionalUserName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{"pod-annotation-valid1": "old"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{"pod-annotation-valid1": "new"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
		},
		{
			name: "additonal acceptance conditions invalid",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: additionalUserName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{"pod-annotation-invalid1": "old"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{"pod-annotation-invalid1": "new"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			expectedErr: fmt.Errorf("%s node: %q is not allowed to set the following annotations on pod: \"testpod\": [pod-annotation-invalid1]", additionalNamePrefix, nodeName),
		},
		{
			name: "additional acceptance conditions cannot modify ovnkube-node annotations",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: additionalUserName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: "old"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: "new"},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			expectedErr: fmt.Errorf("%s node: %q is not allowed to set the following annotations on pod: %q: [%s]", additionalNamePrefix, nodeName, podName, types.OvnPodAnnotationName),
		},
		{
			name: "additional acceptance conditions cannot modify allowed annotations on pods on other nodes",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: additionalUserName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{"pod-annotation-valid1": "old"},
				},
				Spec: corev1.PodSpec{NodeName: "other-node"},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{"pod-annotation-valid1": "new"},
				},
				Spec: corev1.PodSpec{NodeName: "other-node"},
			},
			expectedErr: fmt.Errorf("%s on node: %q is not allowed to modify pods %q annotations", additionalNamePrefix, nodeName, podName),
		},
		{
			name: "additional acceptance conditions cannot modify anything other than annotations",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: additionalUserName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Labels: map[string]string{"key": "old"}},
				Spec:       corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Labels: map[string]string{"key": "new"}},
				Spec:       corev1.PodSpec{NodeName: nodeName},
			},
			expectedErr: fmt.Errorf("%s on node: %q is not allowed to modify anything other than annotations", additionalNamePrefix, nodeName),
		},
		{
			name: "additional acceptance conditions cannot modify pod status",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: additionalUserName,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName},
				Spec:       corev1.PodSpec{NodeName: nodeName},
				Status:     corev1.PodStatus{Phase: corev1.PodPending},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName},
				Spec:       corev1.PodSpec{NodeName: nodeName},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			},
			expectedErr: fmt.Errorf("%s on node: %q is not allowed to modify anything other than annotations", additionalNamePrefix, nodeName),
		},
		{
			name: "additional acceptance conditions annotations are not protected from other users",
			node: &corev1.Node{},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: "system:nodes:node",
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{"pod-annotation-valid1": "old"},
				},
				Spec: corev1.PodSpec{NodeName: "other-node"},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{"pod-annotation-valid1": "new"},
				},
				Spec: corev1.PodSpec{NodeName: "other-node"},
			},
			expectedErr: nil,
		},
	}

	allowedPodAnnotations := []string{"pod-annotation-valid1"}
	additionalPodAdmissions := PodAdmissionConditionOption{
		CommonNamePrefix:         additionalNamePrefix,
		AllowedPodAnnotations:    allowedPodAnnotations,
		AllowedPodAnnotationKeys: sets.New(allowedPodAnnotations...),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			padm := NewPodAdmissionWebhook(&fakeNodeLister{
				nodes: map[string]*corev1.Node{tt.node.Name: tt.node},
			}, nil, []PodAdmissionConditionOption{
				additionalPodAdmissions,
			})
			_, err := padm.ValidateUpdate(tt.ctx, tt.oldObj, tt.newObj)
			if err != tt.expectedErr && err.Error() != tt.expectedErr.Error() {
				t.Errorf("ValidateUpdate() error = %v, expectedErr %v", err, tt.expectedErr)
				return
			}
		})
	}
}

func TestPodAdmission_ValidateUpdateExtraUsers(t *testing.T) {
	extraUser := "system:serviceaccount:ovnkube-cluster-manager"
	tests := []struct {
		name        string
		node        *corev1.Node
		ctx         context.Context
		oldObj      *corev1.Pod
		newObj      *corev1.Pod
		expectedErr error
	}{
		{
			name: "extra-user can modify OvnPodAnnotationName annotation on a pod",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"192.168.0.0/24"}`},
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: extraUser,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:0a:80:00:05"}}`},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["192.168.0.10/24"],"mac_address":"0a:58:0a:80:00:05"}}`},
				},
				Spec: corev1.PodSpec{NodeName: nodeName},
			},
		},
		{
			name: "extra-user cannot set an IP in the OvnPodAnnotationName annotation on host-networked pods",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"192.168.0.0/24"}`},
				},
			},
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: extraUser,
				}},
			}),
			oldObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: podName,
				},
				Spec: corev1.PodSpec{NodeName: nodeName, HostNetwork: true},
			},
			newObj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        podName,
					Annotations: map[string]string{types.OvnPodAnnotationName: `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:0a:80:00:05"}}`},
				},
				Spec: corev1.PodSpec{NodeName: nodeName, HostNetwork: true},
			},
			expectedErr: fmt.Errorf("user: %q is not allowed to set %s on pod %q: the annotation is not allowed on host networked pods", extraUser, types.OvnPodAnnotationName, podName),
		},
	}

	allowedPodAnnotations := []string{"pod-annotation-valid1"}
	additionalPodAdmissions := PodAdmissionConditionOption{
		CommonNamePrefix:         additionalNamePrefix,
		AllowedPodAnnotations:    allowedPodAnnotations,
		AllowedPodAnnotationKeys: sets.New(allowedPodAnnotations...),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			padm := NewPodAdmissionWebhook(&fakeNodeLister{
				nodes: map[string]*corev1.Node{tt.node.Name: tt.node},
			}, nil, []PodAdmissionConditionOption{
				additionalPodAdmissions,
			}, extraUser)
			_, err := padm.ValidateUpdate(tt.ctx, tt.oldObj, tt.newObj)
			if err != tt.expectedErr && err.Error() != tt.expectedErr.Error() {
				t.Errorf("ValidateUpdate() error = %v, expectedErr %v", err, tt.expectedErr)
				return
			}
		})
	}
}

// TestPodNetworkAnnotationCreate checks allocation output is rejected while network requests remain valid.
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

// TestProtectedPodAnnotationsCreate checks creation restrictions also apply to newly registered keys.
func TestProtectedPodAnnotationsCreate(t *testing.T) {
	validator := NewPodAdmissionWebhook(nil, nil, nil)
	// A newly registered key must inherit creation protection automatically.
	validator.annotationKeys.Insert("k8s.ovn.org/future-controller-state")
	for key := range validator.annotationKeys {
		for _, value := range []string{"", "controller-state"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				_, err := validator.ValidateCreate(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{key: value},
				}})
				if err == nil {
					t.Fatalf("creation accepted protected annotation %s", key)
				}
			})
		}
	}
}

// TestPodNetworkAnnotationUpdate checks allocation protection on both pod update endpoints.
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

func TestPodAdmission_ValidateUpdateUserDefinedNetworks(t *testing.T) {
	const (
		clusterManager = "system:serviceaccount:ovnkube-cluster-manager"
		nadNamespace   = "tenant"
		defaultEntry   = `"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:0a:80:00:05"}`
	)
	nad := func(name, topology, extraConfig string) *nadv1.NetworkAttachmentDefinition {
		return &nadv1.NetworkAttachmentDefinition{
			ObjectMeta: metav1.ObjectMeta{Namespace: nadNamespace, Name: name},
			Spec: nadv1.NetworkAttachmentDefinitionSpec{Config: fmt.Sprintf(
				`{"cniVersion":"1.0.0","name":%q,"type":"ovn-k8s-cni-overlay","topology":%q,"netAttachDefName":%q%s}`,
				"net-"+name, topology, nadNamespace+"/"+name, extraConfig)},
		}
	}
	nadIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, n := range []*nadv1.NetworkAttachmentDefinition{
		nad("l3", types.Layer3Topology, `,"subnets":"10.30.0.0/16/24","role":"primary"`),
		nad("l2", types.Layer2Topology, `,"subnets":"10.20.0.0/16","role":"primary"`),
		nad("localnet-ipam", types.LocalnetTopology, `,"subnets":"10.40.0.0/16"`),
		nad("localnet-no-ipam", types.LocalnetTopology, ``),
		nad("localnet-dhcp", types.LocalnetTopology, `,"ipam":{"type":"dhcp"}`),
	} {
		if err := nadIndexer.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	nadLister := nadlisters.NewNetworkAttachmentDefinitionLister(nadIndexer)

	// entry returns the pod network annotation entry of a NAD from nadNamespace.
	entry := func(nadName, ips, extra string) string {
		return fmt.Sprintf(`"%s/%s":{"ip_addresses":[%s],"mac_address":"0a:58:0a:80:00:06"%s}`, nadNamespace, nadName, ips, extra)
	}
	pod := func(annotations map[string]string, entries ...string) *corev1.Pod {
		if len(entries) > 0 {
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[types.OvnPodAnnotationName] = "{" + strings.Join(entries, ",") + "}"
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Annotations: annotations},
			Spec:       corev1.PodSpec{NodeName: nodeName},
		}
	}
	liveMigratable := func() map[string]string {
		return map[string]string{kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: ""}
	}
	notAllowed := func(format string, args ...any) error {
		return fmt.Errorf("user: %q is not allowed to set %s on pod %q: %s",
			userName, types.OvnPodAnnotationName, podName, fmt.Sprintf(format, args...))
	}
	clusterManagerAllocated := func(nadName, topology string) error {
		return notAllowed("the %s/%s entry of %s network %q is allocated by cluster manager", nadNamespace, nadName, topology, "net-"+nadName)
	}

	tests := []struct {
		name string
		// user defaults to the ovnkube-node identity of the pod's node
		user string
		// multiNetworkDisabled runs the webhook without a NAD lister
		multiNetworkDisabled bool
		oldObj               *corev1.Pod
		newObj               *corev1.Pod
		expectedErr          error
	}{
		{
			name:   "ovnkube-node can set a layer3 network IP from its node subnet",
			oldObj: pod(nil, defaultEntry),
			newObj: pod(nil, defaultEntry, entry("l3", `"10.30.1.7/24"`, "")),
		},
		{
			name:        "ovnkube-node cannot set a layer3 network IP from another node subnet",
			oldObj:      pod(nil, defaultEntry),
			newObj:      pod(nil, defaultEntry, entry("l3", `"10.30.2.7/24"`, "")),
			expectedErr: notAllowed("10.30.2.7/24 of network %q does not belong to %s node", "net-l3", nodeName),
		},
		{
			name:        "ovnkube-node cannot modify a layer3 network IP to an invalid value",
			oldObj:      pod(nil, defaultEntry, entry("l3", `"10.30.1.7/24"`, "")),
			newObj:      pod(nil, defaultEntry, entry("l3", `"10.30.2.7/24"`, "")),
			expectedErr: notAllowed("10.30.2.7/24 of network %q does not belong to %s node", "net-l3", nodeName),
		},
		{
			name:   "ovnkube-node can use a layer3 network IP that belongs to a different node in kubevirt live-migration",
			oldObj: pod(liveMigratable(), defaultEntry),
			newObj: pod(liveMigratable(), defaultEntry, entry("l3", `"10.30.2.7/24"`, "")),
		},
		{
			name:   "ovnkube-node can set layer3 network IPs for several attachments to the same network",
			oldObj: pod(nil, defaultEntry),
			newObj: pod(nil, defaultEntry, entry("l3", `"10.30.1.7/24"`, ""), entry("l3/1", `"10.30.1.8/24"`, "")),
		},
		{
			name:        "ovnkube-node cannot set a layer3 network IP from another node subnet for a second attachment",
			oldObj:      pod(nil, defaultEntry, entry("l3", `"10.30.1.7/24"`, "")),
			newObj:      pod(nil, defaultEntry, entry("l3", `"10.30.1.7/24"`, ""), entry("l3/1", `"10.30.2.8/24"`, "")),
			expectedErr: notAllowed("10.30.2.8/24 of network %q does not belong to %s node", "net-l3", nodeName),
		},
		{
			name:        "ovnkube-node cannot add a second attachment entry of a layer2 network",
			oldObj:      pod(nil, defaultEntry, entry("l2", `"10.20.0.5/16"`, `,"tunnel_id":3`)),
			newObj:      pod(nil, defaultEntry, entry("l2", `"10.20.0.5/16"`, `,"tunnel_id":3`), entry("l2/1", `"10.20.0.6/16"`, `,"tunnel_id":4`)),
			expectedErr: notAllowed("the %s/l2/1 entry of %s network %q is allocated by cluster manager", nadNamespace, types.Layer2Topology, "net-l2"),
		},
		{
			name:   "ovnkube-node can remove a layer3 network entry",
			oldObj: pod(nil, defaultEntry, entry("l3", `"10.30.1.7/24"`, "")),
			newObj: pod(nil, defaultEntry),
		},
		{
			name:        "ovnkube-node cannot add a layer2 network entry",
			oldObj:      pod(nil, defaultEntry),
			newObj:      pod(nil, defaultEntry, entry("l2", `"10.20.0.5/16"`, `,"tunnel_id":3`)),
			expectedErr: clusterManagerAllocated("l2", types.Layer2Topology),
		},
		{
			name:        "ovnkube-node cannot add a layer2 network entry without a default network entry",
			oldObj:      pod(nil),
			newObj:      pod(nil, entry("l2", `"10.20.0.5/16"`, `,"tunnel_id":3`)),
			expectedErr: clusterManagerAllocated("l2", types.Layer2Topology),
		},
		{
			name:        "ovnkube-node cannot modify a layer2 network entry",
			oldObj:      pod(nil, defaultEntry, entry("l2", `"10.20.0.5/16"`, `,"tunnel_id":3`)),
			newObj:      pod(nil, defaultEntry, entry("l2", `"10.20.0.9/16"`, `,"tunnel_id":4`)),
			expectedErr: clusterManagerAllocated("l2", types.Layer2Topology),
		},
		{
			name:        "ovnkube-node cannot remove a layer2 network entry",
			oldObj:      pod(nil, defaultEntry, entry("l2", `"10.20.0.5/16"`, `,"tunnel_id":3`)),
			newObj:      pod(nil, defaultEntry),
			expectedErr: clusterManagerAllocated("l2", types.Layer2Topology),
		},
		{
			name:        "ovnkube-node cannot modify a layer2 network entry in kubevirt live-migration",
			oldObj:      pod(liveMigratable(), defaultEntry, entry("l2", `"10.20.0.5/16"`, `,"tunnel_id":3`)),
			newObj:      pod(liveMigratable(), defaultEntry, entry("l2", `"10.20.0.9/16"`, `,"tunnel_id":3`)),
			expectedErr: clusterManagerAllocated("l2", types.Layer2Topology),
		},
		{
			name:   "ovnkube-node can set the default network entry next to an unchanged layer2 network entry",
			oldObj: pod(nil, entry("l2", `"10.20.0.5/16"`, `,"tunnel_id":3`)),
			newObj: pod(nil, entry("l2", `"10.20.0.5/16"`, `,"tunnel_id":3`), defaultEntry),
		},
		{
			name:   "cluster manager can add a layer2 network entry",
			user:   clusterManager,
			oldObj: pod(nil, defaultEntry),
			newObj: pod(nil, defaultEntry, entry("l2", `"10.20.0.5/16"`, `,"tunnel_id":3`)),
		},
		{
			name:        "ovnkube-node cannot add an entry of a localnet network with IPAM",
			oldObj:      pod(nil, defaultEntry),
			newObj:      pod(nil, defaultEntry, entry("localnet-ipam", `"10.40.0.7/16"`, "")),
			expectedErr: clusterManagerAllocated("localnet-ipam", types.LocalnetTopology),
		},
		{
			name:   "ovnkube-node can add an entry of a localnet network without IPAM",
			oldObj: pod(nil, defaultEntry),
			newObj: pod(nil, defaultEntry, entry("localnet-no-ipam", ``, "")),
		},
		{
			name:   "ovnkube-node can add an entry of a localnet network with DHCP IPAM",
			oldObj: pod(nil, defaultEntry),
			newObj: pod(nil, defaultEntry, entry("localnet-dhcp", `"172.16.0.9/24"`, "")),
		},
		{
			name:   "ovnkube-node cannot add an entry of an unknown network",
			oldObj: pod(nil, defaultEntry),
			newObj: pod(nil, defaultEntry, entry("unknown", `"10.1.1.1/24"`, "")),
			expectedErr: notAllowed("failed to get NetworkAttachmentDefinition %s/unknown: networkattachmentdefinition.k8s.cni.cncf.io %q not found",
				nadNamespace, "unknown"),
		},
		{
			name:                 "user defined network entries are not validated when multi-network is disabled",
			multiNetworkDisabled: true,
			oldObj:               pod(nil, defaultEntry),
			newObj:               pod(nil, defaultEntry, entry("l2", `"10.20.0.5/16"`, `,"tunnel_id":3`)),
		},
	}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        nodeName,
		Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"192.168.0.0/24","net-l3":"10.30.1.0/24"}`},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lister nadlisters.NetworkAttachmentDefinitionLister
			if !tt.multiNetworkDisabled {
				lister = nadLister
			}
			user := tt.user
			if user == "" {
				user = userName
			}
			ctx := admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: user}},
			})
			padm := NewPodAdmissionWebhook(&fakeNodeLister{nodes: map[string]*corev1.Node{nodeName: node}}, lister, nil, clusterManager)
			_, err := padm.ValidateUpdate(ctx, tt.oldObj, tt.newObj)
			if err != tt.expectedErr && (err == nil || tt.expectedErr == nil || err.Error() != tt.expectedErr.Error()) {
				t.Errorf("ValidateUpdate() error = %v, expectedErr %v", err, tt.expectedErr)
			}
		})
	}
}
