// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"mime"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

var (
	podListKind    = corev1.SchemeGroupVersion.WithKind("PodList")
	podListDecoder = func() runtime.Decoder {
		podListScheme := runtime.NewScheme()
		podListScheme.AddKnownTypeWithName(podListKind, &trimmingPodList{})
		return runtime.WithoutVersionDecoder{Decoder: protobuf.NewSerializer(podListScheme, podListScheme)}
	}()
)

type trimmingPodList struct {
	corev1.PodList
}

// Unmarshal uses the generated decoder for each top-level field. Trimming each
// new item before decoding the next one bounds the lifetime of discarded fields.
func (list *trimmingPodList) Unmarshal(data []byte) error {
	for len(data) > 0 {
		_, _, n := protowire.ConsumeField(data)
		if n < 0 {
			return protowire.ParseError(n)
		}
		previousItems := len(list.Items)
		if err := list.PodList.Unmarshal(data[:n]); err != nil {
			return err
		}
		if len(list.Items) > previousItems {
			pod := &list.Items[previousItems]
			pod.ManagedFields = nil
			trimPod(pod)
		}
		data = data[n:]
	}
	return nil
}

func trimPod(pod *corev1.Pod) {
	// OVN-K does not consume pod volumes from informer cache.
	pod.Spec.Volumes = []corev1.Volume{}
	// Scheduling-only pod fields are not read from the informer cache.
	pod.Spec.Tolerations = nil
	pod.Spec.Affinity = nil
	pod.Spec.NodeSelector = nil
	pod.OwnerReferences = nil
	// OVN-K only walks pod containers for named ports, so trim the per-container
	// runtime and resource payload that is not read from the informer cache.
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].Command = nil
		pod.Spec.Containers[i].Args = nil
		pod.Spec.Containers[i].Env = nil
		pod.Spec.Containers[i].EnvFrom = nil
		pod.Spec.Containers[i].VolumeMounts = nil
		pod.Spec.Containers[i].Resources = corev1.ResourceRequirements{}
		pod.Spec.Containers[i].LivenessProbe = nil
		pod.Spec.Containers[i].ReadinessProbe = nil
		pod.Spec.Containers[i].StartupProbe = nil
		pod.Spec.Containers[i].Lifecycle = nil
		pod.Spec.Containers[i].SecurityContext = nil
	}
	// Init and ephemeral containers are not read from the informer cache.
	pod.Spec.InitContainers = nil
	pod.Spec.EphemeralContainers = nil
	// OVN-K only needs pod phase/IPs/host IP and a small subset of conditions.
	// Container status arrays are retained heavily in heap profiles and are not read.
	pod.Status.ContainerStatuses = nil
	pod.Status.InitContainerStatuses = nil
	pod.Status.EphemeralContainerStatuses = nil
	// Current informer consumers only read condition type/status/transition time.
	for i := range pod.Status.Conditions {
		pod.Status.Conditions[i].LastProbeTime = metav1.Time{}
		pod.Status.Conditions[i].Reason = ""
		pod.Status.Conditions[i].Message = ""
		pod.Status.Conditions[i].ObservedGeneration = 0
	}
}

func newPodListWatch(client kubernetes.Interface, namespace string) *cache.ListWatch {
	core := client.CoreV1()
	pods := core.Pods(namespace)
	listWatch := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			return pods.List(ctx, opts)
		},
		WatchFuncWithContext: pods.Watch,
	}
	// Use the typed List method for clients with their own implementation.
	if _, ok := client.(*kubernetes.Clientset); ok {
		restClient := core.RESTClient()
		listWatch.ListWithContextFunc = func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			return listPodsWithTrimming(ctx, restClient, namespace, opts)
		}
	}
	return listWatch
}

func listPodsWithTrimming(ctx context.Context, client rest.Interface, namespace string, opts metav1.ListOptions) (*corev1.PodList, error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	response := client.Get().
		UseProtobufAsDefaultIfPreferred(true).
		NamespaceIfScoped(namespace, namespace != "").
		Resource("pods").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Do(ctx)

	var list trimmingPodList
	var contentType string
	data, err := response.ContentType(&contentType).Raw()
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if err == nil && len(data) != 0 && mediaType == runtime.ContentTypeProtobuf {
		_, kind, decodeErr := podListDecoder.Decode(data, nil, &list)
		if kind != nil && *kind == podListKind {
			return &list.PodList, decodeErr
		}
	}
	// Use the usual decoder for JSON and unexpected object types, including API Status.
	return &list.PodList, response.Into(&list.PodList)
}
