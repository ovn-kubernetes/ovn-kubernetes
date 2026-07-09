// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	kubemocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	v1mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
)

func TestIsPodAnnotationUpdateRetryable(t *testing.T) {
	if !IsPodAnnotationUpdateRetryable(apierrors.NewConflict(
		schema.GroupResource{Group: "", Resource: "pods"},
		"test-pod",
		errors.New("conflict"),
	)) {
		t.Fatal("expected conflict error to be retryable")
	}

	if !IsPodAnnotationUpdateRetryable(apierrors.NewInvalid(
		schema.GroupKind{Group: "", Kind: "Pod"},
		"test-pod",
		field.ErrorList{field.Invalid(field.NewPath("metadata", "annotations"), "value", "test failed")},
	)) {
		t.Fatal("expected invalid error to be retryable")
	}

	if IsPodAnnotationUpdateRetryable(errors.New("plain error")) {
		t.Fatal("expected plain error to not be retryable")
	}
}

func TestUpdatePodWithAllocationOrRollback(t *testing.T) {
	tests := []struct {
		name             string
		allocateRollback bool
		getPodErr        bool
		allocateErr      bool
		updatePodErr     bool
		expectAllocation bool
		expectRollback   bool
		expectUpdate     bool
		expectErr        bool
	}{
		{
			name:             "normal operation",
			allocateRollback: true,
			expectAllocation: true,
			expectUpdate:     true,
		},
		{
			name:             "pod get fails",
			allocateRollback: true,
			getPodErr:        true,
			expectErr:        true,
		},
		{
			name:             "allocate fails",
			expectAllocation: true,
			allocateRollback: true,
			allocateErr:      true,
			expectErr:        true,
		},
		{
			name:             "update pod fails",
			expectAllocation: true,
			allocateRollback: true,
			updatePodErr:     true,
			expectRollback:   true,
			expectErr:        true,
		},
		{
			name:             "update pod fails no rollback",
			expectAllocation: true,
			updatePodErr:     true,
			expectErr:        true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			podListerMock := &v1mocks.PodLister{}
			kubeMock := &kubemocks.Interface{}
			podNamespaceLister := &v1mocks.PodNamespaceLister{}

			podListerMock.On("Pods", mock.AnythingOfType("string")).Return(podNamespaceLister)

			var rollbackDone bool
			rollback := func() {
				rollbackDone = true
			}

			pod := &corev1.Pod{}

			var allocated bool
			allocate := func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
				allocated = true
				if tt.allocateErr {
					return pod, rollback, errors.New("Allocate error")
				}
				if tt.allocateRollback {
					return pod, rollback, nil
				}
				return pod, nil, nil
			}

			if tt.getPodErr {
				podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(nil, errors.New("Get pod error"))
			} else {
				podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)
			}

			if tt.updatePodErr {
				kubeMock.On("PatchPodStatusAnnotations", pod, mock.AnythingOfType("*v1.Pod")).Return(errors.New("Update pod error"))
			} else if tt.expectUpdate {
				kubeMock.On("PatchPodStatusAnnotations", pod, mock.AnythingOfType("*v1.Pod")).Return(nil)
			}

			err := UpdatePodWithRetryOrRollback(podListerMock, kubeMock, &corev1.Pod{}, allocate)

			if (err != nil) != tt.expectErr {
				t.Errorf("UpdatePodWithAllocationOrRollback() error = %v, expectErr %v", err, tt.expectErr)
			}

			if allocated != tt.expectAllocation {
				t.Errorf("UpdatePodWithAllocationOrRollback() allocated = %v, expectAllocation %v", allocated, tt.expectAllocation)
			}

			if rollbackDone != tt.expectRollback {
				t.Errorf("UpdatePodWithAllocationOrRollback() rollbackDone = %v, expectRollback %v", rollbackDone, tt.expectRollback)
			}
		})
	}
}

// TestUpdatePodWithRetryOrRollbackLiveGetOnRetry verifies that when the
// annotation patch fails in a retryable way (e.g. the resourceVersion test op
// in the patch fails with Invalid because it was built from a stale informer
// copy), the retry rebuilds the patch from a pod fetched directly from the
// apiserver instead of the (possibly still stale) lister.
func TestUpdatePodWithRetryOrRollbackLiveGetOnRetry(t *testing.T) {
	stalePod := &corev1.Pod{}
	stalePod.Name = "test-pod"
	stalePod.ResourceVersion = "1000"
	freshPod := &corev1.Pod{}
	freshPod.Name = "test-pod"
	freshPod.ResourceVersion = "1001"

	invalidErr := apierrors.NewInvalid(
		schema.GroupKind{Group: "", Kind: "Pod"},
		"test-pod",
		field.ErrorList{field.Invalid(field.NewPath("metadata", "resourceVersion"), "1000", "test failed")},
	)

	podListerMock := &v1mocks.PodLister{}
	podNamespaceLister := &v1mocks.PodNamespaceLister{}
	kubeMock := &kubemocks.Interface{}

	podListerMock.On("Pods", mock.AnythingOfType("string")).Return(podNamespaceLister)
	// the lister keeps returning the stale pod for the whole retry window
	podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(stalePod, nil)
	// the apiserver has the fresh pod
	kubeMock.On("GetPod", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return(freshPod, nil)

	// a patch built from the stale pod fails its resourceVersion test op,
	// a patch built from the fresh pod succeeds
	kubeMock.On("PatchPodStatusAnnotations", stalePod, mock.AnythingOfType("*v1.Pod")).Return(invalidErr)
	kubeMock.On("PatchPodStatusAnnotations", freshPod, mock.AnythingOfType("*v1.Pod")).Return(nil)

	allocate := func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
		return pod, nil, nil
	}

	err := UpdatePodWithRetryOrRollback(podListerMock, kubeMock, stalePod, allocate)
	if err != nil {
		t.Fatalf("UpdatePodWithRetryOrRollback() unexpected error: %v", err)
	}

	kubeMock.AssertCalled(t, "GetPod", mock.AnythingOfType("string"), mock.AnythingOfType("string"))
	kubeMock.AssertCalled(t, "PatchPodStatusAnnotations", freshPod, mock.AnythingOfType("*v1.Pod"))
	// only the first attempt may use the lister
	podNamespaceLister.AssertNumberOfCalls(t, "Get", 1)
}
