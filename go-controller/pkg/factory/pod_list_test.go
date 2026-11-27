// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

type podListRoundTripper func(*http.Request) (*http.Response, error)

func (f podListRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newPodListTestClient(t *testing.T, transport podListRoundTripper) *kubernetes.Clientset {
	t.Helper()
	client, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "http://kubernetes.invalid"}, &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func encodePodListTestResponse(t *testing.T, obj runtime.Object) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := protobuf.NewSerializer(scheme.Scheme, scheme.Scheme).Encode(obj, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testPodList() *corev1.PodList {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "one",
			Namespace:   "example",
			Labels:      map[string]string{"network": "blue"},
			Annotations: map[string]string{"k8s.ovn.org/pod-networks": "{}"},
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager: "test", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{}}`),
			}},
			OwnerReferences: []metav1.OwnerReference{{Name: "drop", Kind: "ReplicaSet"}},
		},
		Spec: corev1.PodSpec{
			NodeName: "worker",
			Containers: []corev1.Container{{
				Name: "app",
				Ports: []corev1.ContainerPort{{
					Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP,
				}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIPs: []corev1.PodIP{{IP: "192.0.2.10"}},
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue, Message: "drop this",
			}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true}},
		},
	}
	second := pod.DeepCopy()
	second.Name = "two"
	second.Status.PodIPs[0].IP = "192.0.2.11"
	return &corev1.PodList{
		TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"},
		ListMeta: metav1.ListMeta{ResourceVersion: "42", Continue: "continue-next", RemainingItemCount: ptr.To(int64(3))},
		Items:    []corev1.Pod{pod, *second},
	}
}

func TestTrimmedPodListMatchesTrimmedStandardList(t *testing.T) {
	data, err := testPodList().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Unknown fields can appear anywhere in a protobuf response.
	data = append(protowire.AppendString(protowire.AppendTag(nil, 15, protowire.BytesType), "ignored"), data...)
	var expected corev1.PodList
	if err := expected.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	for i := range expected.Items {
		expected.Items[i].ManagedFields = nil
		trimPod(&expected.Items[i])
	}
	want := expected.DeepCopy()
	var actual trimmingPodList
	if err := actual.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	// The decoded list should be independent of the response buffer.
	clear(data)
	if !reflect.DeepEqual(&actual.PodList, want) {
		t.Fatalf("decoded and trimmed list differs from standard decoding: actual %#v, want %#v", &actual.PodList, want)
	}
}

func TestPodListWatch(t *testing.T) {
	fixture := testPodList()
	podListBody := encodePodListTestResponse(t, fixture)
	client := newPodListTestClient(t, func(req *http.Request) (*http.Response, error) {
		query := req.URL.Query()
		if req.Method != http.MethodGet || req.URL.Path != "/api/v1/namespaces/example/pods" ||
			query.Get("resourceVersion") != "17" || query.Get("continue") != "continue-start" ||
			query.Get("limit") != "25" || query.Get("fieldSelector") != "spec.nodeName=worker" ||
			query.Get("labelSelector") != "network=blue" || query.Get("timeoutSeconds") != "10" ||
			req.Header.Get("Accept") != runtime.ContentTypeProtobuf+","+runtime.ContentTypeJSON {
			t.Errorf("unexpected Pod LIST request: %s %s, Accept %q", req.Method, req.URL, req.Header.Get("Accept"))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {runtime.ContentTypeProtobuf}},
			Body:       io.NopCloser(bytes.NewReader(podListBody)),
		}, nil
	})
	opts := metav1.ListOptions{
		ResourceVersion: "17", Continue: "continue-start", Limit: 25,
		FieldSelector: "spec.nodeName=worker", LabelSelector: "network=blue", TimeoutSeconds: ptr.To(int64(10)),
	}

	decoded, err := client.CoreV1().Pods("example").List(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items[0].ManagedFields) == 0 {
		t.Fatal("ordinary Pod LIST must retain full objects")
	}
	for i := range decoded.Items {
		decoded.Items[i].ManagedFields = nil
		trimPod(&decoded.Items[i])
	}
	got, err := newPodListWatch(client, "example").ListWithContext(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, decoded) {
		t.Fatalf("optimized Pod LIST differs from standard LIST after trimming: actual %#v, want %#v", got, decoded)
	}
}

func TestPodListWatchPreservesFallbackAndErrors(t *testing.T) {
	status := &metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure, Reason: metav1.StatusReasonExpired, Code: http.StatusGone,
	}
	for _, tc := range []struct {
		name        string
		contentType string
		statusCode  int
		body        []byte
		expired     bool
	}{
		{
			name:        "JSON",
			contentType: runtime.ContentTypeJSON,
			statusCode:  http.StatusOK,
			body:        []byte(`{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"42"},"items":[{"metadata":{"name":"one","namespace":"example","managedFields":[{"manager":"test"}]}}]}`),
		},
		{
			name: "HTTP error", contentType: runtime.ContentTypeProtobuf, statusCode: http.StatusGone,
			body: encodePodListTestResponse(t, status), expired: true,
		},
		{
			name: "status envelope", contentType: runtime.ContentTypeProtobuf, statusCode: http.StatusOK,
			body: encodePodListTestResponse(t, status), expired: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newPodListTestClient(t, func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tc.statusCode,
					Header:     http.Header{"Content-Type": {tc.contentType}},
					Body:       io.NopCloser(bytes.NewReader(tc.body)),
				}, nil
			})
			obj, err := newPodListWatch(client, "example").ListWithContext(t.Context(), metav1.ListOptions{})
			if tc.expired {
				if !apierrors.IsResourceExpired(err) {
					t.Fatalf("wanted resource expired error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			list, ok := obj.(*corev1.PodList)
			if !ok || list.ResourceVersion != "42" || len(list.Items) != 1 || len(list.Items[0].ManagedFields) != 1 {
				t.Fatalf("unexpected JSON LIST: %#v", obj)
			}
		})
	}
}

func TestPodListTrimmingFailedDecodeRetainsReflectorStore(t *testing.T) {
	fixture := testPodList()
	data, err := fixture.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	data = append(protowire.AppendTag(data, 2, protowire.BytesType), 1)
	body := encodePodListTestResponse(t, &runtime.Unknown{
		TypeMeta: runtime.TypeMeta{Kind: "PodList", APIVersion: "v1"},
		Raw:      data,
	})
	client := newPodListTestClient(t, func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {runtime.ContentTypeProtobuf}},
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	})
	existing := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: "example"}}
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := store.Add(existing); err != nil {
		t.Fatal(err)
	}
	listWatch := newPodListWatch(client, "example")
	listWatch.WatchFuncWithContext = func(context.Context, metav1.ListOptions) (watch.Interface, error) {
		return nil, errors.New("watch unavailable in test")
	}
	reflector := cache.NewReflector(listWatch, &corev1.Pod{}, store, 0)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = reflector.ListAndWatchWithContext(ctx)
	if err == nil || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("wanted unexpected EOF for incomplete LIST, got %v", err)
	}
	if entries := store.List(); len(entries) != 1 || entries[0] != existing {
		t.Fatalf("incomplete LIST modified the reflector store: %#v", entries)
	}
}
