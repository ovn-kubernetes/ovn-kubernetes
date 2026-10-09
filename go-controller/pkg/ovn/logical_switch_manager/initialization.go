// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package logicalswitchmanager

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// SortPodsForIPAM orders bootstrap reservations so live pods claim their IPs
// before completed pods whose annotations may refer to released addresses.
// Among completed pods, the most recently initialized pod claims first.
func SortPodsForIPAM(objects []interface{}) ([]*corev1.Pod, error) {
	pods := make([]*corev1.Pod, 0, len(objects))
	for _, object := range objects {
		pod, ok := object.(*corev1.Pod)
		if !ok {
			return nil, fmt.Errorf("spurious object in pod IPAM initialization: %v", object)
		}
		pods = append(pods, pod)
	}
	initialized := func(pod *corev1.Pod) time.Time {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodInitialized {
				return condition.LastTransitionTime.Time
			}
		}
		return time.Time{}
	}
	sort.SliceStable(pods, func(i, j int) bool {
		if util.PodCompleted(pods[i]) != util.PodCompleted(pods[j]) {
			return !util.PodCompleted(pods[i])
		}
		return initialized(pods[i]).After(initialized(pods[j]))
	})
	return pods, nil
}
