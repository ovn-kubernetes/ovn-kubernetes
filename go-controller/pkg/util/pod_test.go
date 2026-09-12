// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestUpdatePodWaitsForInformerBeforeRetry(t *testing.T) {
	podListerMock := &v1mocks.PodLister{}
	podNamespaceLister := &v1mocks.PodNamespaceLister{}
	kubeMock := &kubemocks.Interface{}
	podListerMock.On("Pods", "test-ns").Return(podNamespaceLister)

	podV1 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace:       "test-ns",
		Name:            "test-pod",
		ResourceVersion: "1",
	}}
	podV2 := podV1.DeepCopy()
	podV2.ResourceVersion = "2"
	podNamespaceLister.On("Get", "test-pod").Return(podV1, nil).Once()
	podNamespaceLister.On("Get", "test-pod").Return(podV2, nil).Once()

	conflictErr := apierrors.NewConflict(
		schema.GroupResource{Resource: "pods"},
		podV1.Name,
		errors.New("annotation changed"),
	)
	kubeMock.On("PatchPodStatusAnnotations", podV1, mock.AnythingOfType("*v1.Pod")).
		Return(conflictErr).Once()
	kubeMock.On("PatchPodStatusAnnotations", podV2, mock.AnythingOfType("*v1.Pod")).
		Return(nil).Once()

	allocate := func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
		return pod, nil, nil
	}
	err := UpdatePodWithRetryOrRollback(podListerMock, kubeMock, podV1, allocate)
	if err != nil {
		t.Fatalf("expected cache-advanced retry to succeed: %v", err)
	}
	kubeMock.AssertExpectations(t)
}

func TestUpdatePodDoesNotRepeatPatchFromStaleInformer(t *testing.T) {
	podListerMock := &v1mocks.PodLister{}
	podNamespaceLister := &v1mocks.PodNamespaceLister{}
	kubeMock := &kubemocks.Interface{}
	podListerMock.On("Pods", "test-ns").Return(podNamespaceLister)

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace:       "test-ns",
		Name:            "test-pod",
		ResourceVersion: "1",
	}}
	podNamespaceLister.On("Get", "test-pod").Return(pod, nil)
	conflictErr := apierrors.NewConflict(
		schema.GroupResource{Resource: "pods"},
		pod.Name,
		errors.New("annotation changed"),
	)
	kubeMock.On("PatchPodStatusAnnotations", pod, mock.AnythingOfType("*v1.Pod")).
		Return(conflictErr).Once()

	allocate := func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
		return pod, nil, nil
	}
	err := UpdatePodWithRetryOrRollback(podListerMock, kubeMock, pod, allocate)
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected the original conflict, got %v", err)
	}
	kubeMock.AssertNumberOfCalls(t, "PatchPodStatusAnnotations", 1)
}
