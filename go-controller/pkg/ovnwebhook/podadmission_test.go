// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"context"
	"errors"
	"fmt"
	"testing"

	kubevirtv1 "kubevirt.io/api/core/v1"

	admv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
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

func newPodLister(t *testing.T, pods ...*corev1.Pod) listersv1.PodLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, pod := range pods {
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("failed to add pod %s/%s to the lister: %v", pod.Namespace, pod.Name, err)
		}
	}
	return listersv1.NewPodLister(indexer)
}

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
			}, newPodLister(t), []PodAdmissionConditionOption{
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
			}, newPodLister(t), []PodAdmissionConditionOption{
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

// TestPodAdmission_ValidateUpdateLiveMigratable checks which default network IPs
// a node may set on a live-migratable pod.
func TestPodAdmission_ValidateUpdateLiveMigratable(t *testing.T) {
	const (
		namespace      = "vms"
		vmName         = "vm1"
		localAnnot     = `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:c0:a8:00:05"}}`
		migratedAnnot  = `{"default":{"ip_addresses":["10.10.10.10/24"],"mac_address":"0a:58:0a:0a:0a:0a"}}`
		dualStackAnnot = `{"default":{"ip_addresses":["10.10.10.10/24","fd00:10:10::10/64"],"mac_address":"0a:58:0a:0a:0a:0a"}}`
		otherAnnot     = `{"default":{"ip_addresses":["10.10.10.11/24"],"mac_address":"0a:58:0a:0a:0a:0b"}}`
		withSecondary  = `{"default":{"ip_addresses":["10.10.10.10/24"],"mac_address":"0a:58:0a:0a:0a:0a"},"vms/net":{"mac_address":"0a:58:0a:0a:0a:0c"}}`
		extraUser      = "system:serviceaccount:ovnkube-cluster-manager"
	)
	deniedMigrated := "10.10.10.10/24 does not belong to " + nodeName + " node and is not held by another pod of the same virtual machine"
	vmPod := func(name, namespace, vmName, nodeName string, annotations map[string]string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				UID:       ktypes.UID(namespace + "/" + name),
				Labels: map[string]string{
					kubevirtv1.AppLabel:                "virt-launcher",
					kubevirtv1.VirtualMachineNameLabel: vmName,
				},
				Annotations: map[string]string{
					kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation: "",
					kubevirtv1.DomainAnnotation:                             vmName,
				},
			},
			Spec: corev1.PodSpec{NodeName: nodeName},
		}
		for k, v := range annotations {
			pod.Annotations[k] = v
		}
		return pod
	}
	target := func(annotations map[string]string) *corev1.Pod {
		return vmPod(podName, namespace, vmName, nodeName, annotations)
	}
	withPhase := func(pod *corev1.Pod, phase corev1.PodPhase) *corev1.Pod {
		pod.Status.Phase = phase
		return pod
	}
	podNetworks := func(value string) map[string]string {
		return map[string]string{types.OvnPodAnnotationName: value}
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        nodeName,
		Annotations: map[string]string{"k8s.ovn.org/node-subnets": `{"default":"192.168.0.0/24"}`},
	}}

	tests := []struct {
		name        string
		user        string
		oldObj      *corev1.Pod
		newObj      *corev1.Pod
		pods        []*corev1.Pod
		noNode      bool
		expectedErr string
	}{
		{
			name:   "IP in the node subnet is allowed",
			oldObj: target(nil),
			newObj: target(podNetworks(localAnnot)),
		},
		{
			name:   "IP of the source pod of the same virtual machine is allowed on the migration target",
			oldObj: target(nil),
			newObj: target(podNetworks(migratedAnnot)),
			pods:   []*corev1.Pod{vmPod("source", namespace, vmName, "other-node", podNetworks(migratedAnnot))},
		},
		{
			name:   "IP of the source pod is allowed when cluster manager updates the migration target",
			user:   extraUser,
			oldObj: target(nil),
			newObj: target(podNetworks(migratedAnnot)),
			pods:   []*corev1.Pod{vmPod("source", namespace, vmName, "other-node", podNetworks(migratedAnnot))},
		},
		{
			name:        "IP outside the node subnet without a related pod is denied",
			oldObj:      target(nil),
			newObj:      target(podNetworks(migratedAnnot)),
			expectedErr: deniedMigrated,
		},
		{
			name:        "IP held by a pod of another virtual machine in the same namespace is denied",
			oldObj:      target(nil),
			newObj:      target(podNetworks(migratedAnnot)),
			pods:        []*corev1.Pod{vmPod("victim", namespace, "vm2", "other-node", podNetworks(migratedAnnot))},
			expectedErr: deniedMigrated,
		},
		{
			name:        "IP held by a pod of a virtual machine with the same name in another namespace is denied",
			oldObj:      target(nil),
			newObj:      target(podNetworks(migratedAnnot)),
			pods:        []*corev1.Pod{vmPod("source", "other-namespace", vmName, "other-node", podNetworks(migratedAnnot))},
			expectedErr: deniedMigrated,
		},
		{
			name:        "IP other than the one held by the source pod is denied",
			oldObj:      target(nil),
			newObj:      target(podNetworks(migratedAnnot)),
			pods:        []*corev1.Pod{vmPod("source", namespace, vmName, "other-node", podNetworks(otherAnnot))},
			expectedErr: deniedMigrated,
		},
		{
			name:   "both IPs of a dual-stack source pod are allowed on the migration target",
			oldObj: target(nil),
			newObj: target(podNetworks(dualStackAnnot)),
			pods:   []*corev1.Pod{vmPod("source", namespace, vmName, "other-node", podNetworks(dualStackAnnot))},
		},
		{
			name:        "IP that the source pod does not hold is denied when it holds the other one",
			oldObj:      target(nil),
			newObj:      target(podNetworks(dualStackAnnot)),
			pods:        []*corev1.Pod{vmPod("source", namespace, vmName, "other-node", podNetworks(migratedAnnot))},
			expectedErr: "fd00:10:10::10/64 does not belong to " + nodeName + " node and is not held by another pod of the same virtual machine",
		},
		{
			name:   "IP of a completed pod of the same virtual machine is denied while another one is running",
			oldObj: target(nil),
			newObj: target(podNetworks(migratedAnnot)),
			pods: []*corev1.Pod{
				withPhase(vmPod("old-source", namespace, vmName, "other-node", podNetworks(migratedAnnot)), corev1.PodSucceeded),
				withPhase(vmPod("source", namespace, vmName, "another-node", nil), corev1.PodRunning),
			},
			expectedErr: deniedMigrated,
		},
		{
			name:   "IP of a running pod of the same virtual machine is allowed while another one is completed",
			oldObj: target(nil),
			newObj: target(podNetworks(migratedAnnot)),
			pods: []*corev1.Pod{
				withPhase(vmPod("old-source", namespace, vmName, "other-node", nil), corev1.PodSucceeded),
				withPhase(vmPod("source", namespace, vmName, "another-node", podNetworks(migratedAnnot)), corev1.PodRunning),
			},
		},
		{
			name:   "IP held only by completed pods of the same virtual machine is denied",
			oldObj: target(nil),
			newObj: target(podNetworks(migratedAnnot)),
			pods: []*corev1.Pod{
				withPhase(vmPod("succeeded", namespace, vmName, "other-node", podNetworks(migratedAnnot)), corev1.PodSucceeded),
				withPhase(vmPod("failed", namespace, vmName, "another-node", podNetworks(migratedAnnot)), corev1.PodFailed),
			},
			expectedErr: deniedMigrated,
		},
		{
			name:        "IP held only by the pod itself is denied",
			oldObj:      target(nil),
			newObj:      target(podNetworks(migratedAnnot)),
			pods:        []*corev1.Pod{target(podNetworks(migratedAnnot))},
			expectedErr: deniedMigrated,
		},
		{
			name:   "IP of a related pod is denied on a mirror pod",
			oldObj: target(map[string]string{corev1.MirrorPodAnnotationKey: "hash"}),
			newObj: target(map[string]string{
				corev1.MirrorPodAnnotationKey: "hash",
				types.OvnPodAnnotationName:    migratedAnnot,
			}),
			pods:        []*corev1.Pod{vmPod("source", namespace, vmName, "other-node", podNetworks(migratedAnnot))},
			expectedErr: deniedMigrated,
		},
		{
			name:   "IP of a mirror pod of the same virtual machine is denied",
			oldObj: target(nil),
			newObj: target(podNetworks(migratedAnnot)),
			pods: []*corev1.Pod{vmPod("source", namespace, vmName, "other-node", map[string]string{
				corev1.MirrorPodAnnotationKey: "hash",
				types.OvnPodAnnotationName:    migratedAnnot,
			})},
			expectedErr: deniedMigrated,
		},
		{
			name:   "already set IP outside the node subnet is kept when another network is added",
			oldObj: target(podNetworks(migratedAnnot)),
			newObj: target(podNetworks(withSecondary)),
		},
		{
			name:   "already set IP is kept when another network is added and the node is not known",
			oldObj: target(podNetworks(migratedAnnot)),
			newObj: target(podNetworks(withSecondary)),
			noNode: true,
		},
		{
			name:        "new IP is denied when the node is not known",
			oldObj:      target(nil),
			newObj:      target(podNetworks(localAnnot)),
			noNode:      true,
			expectedErr: "could not get info on node " + nodeName + ` from client: nodr "` + nodeName + `" not found`,
		},
		{
			name:   "already set IP outside the node subnet is kept when a pod in the namespace has no virtual machine name",
			oldObj: target(podNetworks(migratedAnnot)),
			newObj: target(podNetworks(withSecondary)),
			pods: func() []*corev1.Pod {
				pod := vmPod("unnamed", namespace, vmName, "other-node", nil)
				delete(pod.Annotations, kubevirtv1.DomainAnnotation)
				return []*corev1.Pod{pod}
			}(),
		},
		{
			name:        "already set IP cannot change to another IP outside the node subnet",
			oldObj:      target(podNetworks(otherAnnot)),
			newObj:      target(podNetworks(migratedAnnot)),
			expectedErr: deniedMigrated,
		},
		{
			name:   "already set IP can change to an IP in the node subnet",
			oldObj: target(podNetworks(migratedAnnot)),
			newObj: target(podNetworks(localAnnot)),
		},
		{
			name: "host networked pod is denied",
			oldObj: func() *corev1.Pod {
				pod := target(nil)
				pod.Spec.HostNetwork = true
				return pod
			}(),
			newObj: func() *corev1.Pod {
				pod := target(podNetworks(localAnnot))
				pod.Spec.HostNetwork = true
				return pod
			}(),
			expectedErr: "the annotation is not allowed on host networked pods",
		},
		{
			name: "pod that is not live-migratable is denied an IP of a related pod",
			oldObj: func() *corev1.Pod {
				pod := target(nil)
				delete(pod.Annotations, kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation)
				return pod
			}(),
			newObj: func() *corev1.Pod {
				pod := target(podNetworks(migratedAnnot))
				delete(pod.Annotations, kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation)
				return pod
			}(),
			pods:        []*corev1.Pod{vmPod("source", namespace, vmName, "other-node", podNetworks(migratedAnnot))},
			expectedErr: "10.10.10.10/24 does not belong to " + nodeName + " node",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := tt.user
			if user == "" {
				user = userName
			}
			ctx := admission.NewContextWithRequest(context.Background(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: user}},
			})
			nodes := map[string]*corev1.Node{nodeName: node}
			if tt.noNode {
				nodes = nil
			}
			padm := NewPodAdmissionWebhook(&fakeNodeLister{nodes: nodes},
				newPodLister(t, tt.pods...), nil, extraUser)
			_, err := padm.ValidateUpdate(ctx, tt.oldObj, tt.newObj)
			if tt.expectedErr == "" {
				if err != nil {
					t.Fatalf("ValidateUpdate() error = %v, expected none", err)
				}
				return
			}
			expectedErr := fmt.Sprintf("user: %q is not allowed to set %s on pod %q: %s", user, types.OvnPodAnnotationName, podName, tt.expectedErr)
			if err == nil || err.Error() != expectedErr {
				t.Fatalf("ValidateUpdate() error = %v, expected %q", err, expectedErr)
			}
		})
	}
}

// TestPodAdmission_ValidateUpdateVirtualMachineName checks that no user can
// move a pod to another virtual machine.
func TestPodAdmission_ValidateUpdateVirtualMachineName(t *testing.T) {
	pod := func(annotations map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Annotations: annotations}}
	}
	tests := []struct {
		name   string
		oldObj *corev1.Pod
		newObj *corev1.Pod
	}{
		{
			name:   "changed",
			oldObj: pod(map[string]string{kubevirtv1.DomainAnnotation: "vm1"}),
			newObj: pod(map[string]string{kubevirtv1.DomainAnnotation: "vm2"}),
		},
		{
			name:   "added",
			oldObj: pod(nil),
			newObj: pod(map[string]string{kubevirtv1.DomainAnnotation: "vm2"}),
		},
		{
			name:   "removed",
			oldObj: pod(map[string]string{kubevirtv1.DomainAnnotation: "vm1"}),
			newObj: pod(nil),
		},
	}
	expectedErr := fmt.Sprintf("annotation %s must not change on pod %q", kubevirtv1.DomainAnnotation, podName)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := admission.NewContextWithRequest(context.Background(), admission.Request{
				AdmissionRequest: admv1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: "system:node:" + nodeName}},
			})
			_, err := NewPodAdmissionWebhook(nil, nil, nil).ValidateUpdate(ctx, tt.oldObj, tt.newObj)
			if err == nil || err.Error() != expectedErr {
				t.Fatalf("ValidateUpdate() error = %v, expected %q", err, expectedErr)
			}
		})
	}
}
