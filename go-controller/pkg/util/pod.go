package util

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
)

// annotationNADKeys returns the NAD keys present in the pod-networks annotation
// for diagnostic logging.
func annotationNADKeys(annotations map[string]string) []string {
	raw, ok := annotations[OvnPodAnnotationName]
	if !ok {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return []string{"<parse-error>"}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

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
	attempt := 0
	err := retry.OnError(OvnConflictBackoff, IsPodAnnotationUpdateRetryable, func() error {
		attempt++
		oldPod, err := podLister.Pods(pod.Namespace).Get(pod.Name)
		if err != nil {
			return err
		}

		// Informer cache should not be mutated, so copy the object
		pod = oldPod.DeepCopy()
		pod, rollback, err := allocate(pod)
		if err != nil {
			return err
		}

		if pod == nil {
			klog.V(5).Infof("[%s/%s] pod update attempt %d: allocate returned nil (no update needed), rv=%s",
				oldPod.Namespace, oldPod.Name, attempt, oldPod.ResourceVersion)
			return nil
		}

		klog.Infof("[%s/%s] pod update attempt %d: patching annotation, lister rv=%s, annot keys=%v",
			oldPod.Namespace, oldPod.Name, attempt, oldPod.ResourceVersion,
			annotationNADKeys(oldPod.Annotations))

		err = kube.PatchPodStatusAnnotations(oldPod, pod)
		if err != nil && rollback != nil {
			rollback()
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to update pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}
