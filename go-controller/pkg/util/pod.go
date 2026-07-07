// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
)

// AllocateToPodWithRollbackFunc is a function used to allocate a resource to a
// pod that depends on the current state of the pod, and possibly updating it.
// To be used with UpdatePodWithAllocationOrRollback. Implementations can return
// a nil pod if no update is warranted. Implementations can also return a
// rollback function that will be invoked if the pod update fails.
type AllocateToPodWithRollbackFunc func(pod *corev1.Pod) (*corev1.Pod, func(), error)

// IsPodAnnotationUpdateRetryable returns true for errors that should trigger a
// relist-and-retry when updating pod annotations.
//
// JSON Patch "test" failures from the apiserver are surfaced as Invalid rather
// than Conflict, so this retry path needs to handle both.
func IsPodAnnotationUpdateRetryable(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsInvalid(err)
}

// UpdatePodWithRetryOrRollback updates pod annotations with the result of the
// allocate function. If the pod update fails, it applies the rollback provided by
// the allocate function.
func UpdatePodWithRetryOrRollback(podLister listers.PodLister, kube kube.Interface, pod *corev1.Pod, allocate AllocateToPodWithRollbackFunc) error {
	start := time.Now()
	defer func() {
		klog.V(5).Infof("[%s/%s] pod update took %v", pod.Namespace, pod.Name, time.Since(start))
	}()
	// The patch built by PatchPodStatusAnnotations may be guarded by a
	// resourceVersion test that fails if the patch was built from a stale pod.
	// The informer cache can stay stale for the whole retry window when the
	// apiserver is under load (e.g. a concurrent writer just bumped the pod's
	// resourceVersion and the watch event has not been delivered yet), in
	// which case retrying from the lister would keep failing with the same
	// error. So once a patch attempt fails in a retryable way, fetch the pod
	// directly from the apiserver on the next attempt.
	fetchLive := false
	err := retry.OnError(OvnConflictBackoff, IsPodAnnotationUpdateRetryable, func() error {
		var oldPod *corev1.Pod
		var err error
		if fetchLive {
			oldPod, err = kube.GetPod(pod.Namespace, pod.Name)
		} else {
			oldPod, err = podLister.Pods(pod.Namespace).Get(pod.Name)
		}
		if err != nil {
			return err
		}

		// Informer cache should not be mutated, so copy the object
		currentPod := oldPod.DeepCopy()
		updatedPod, rollback, err := allocate(currentPod)
		if err != nil {
			return err
		}

		if updatedPod == nil {
			return nil
		}

		err = kube.PatchPodStatusAnnotations(oldPod, updatedPod)
		if err != nil {
			if IsPodAnnotationUpdateRetryable(err) {
				fetchLive = true
			}
			if rollback != nil {
				rollback()
			}
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to update pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}
