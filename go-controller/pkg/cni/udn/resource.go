// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package udn

import (
	"fmt"

	"gopkg.in/k8snetworkplumbingwg/multus-cni.v4/pkg/kubeletclient"
	"gopkg.in/k8snetworkplumbingwg/multus-cni.v4/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

func getPodResourceInfo(pod *corev1.Pod, resourceName string) (*types.ResourceInfo, error) {
	podDesc := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	ck, err := kubeletclient.GetResourceClient("")
	if err != nil {
		return nil, fmt.Errorf("failed to get a ResourceClient instance get resource info for pod %s: %v", podDesc, err)
	}
	resourceMap, err := ck.GetPodResourceMap(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to get resources allocated for pod %s from ResourceClient: %v", podDesc, err)
	}
	entry, ok := resourceMap[resourceName]
	if !ok {
		return nil, fmt.Errorf("failed to get resources allocated for pod %s: no resources for resource %s", podDesc, resourceName)
	}
	klog.V(5).Infof("ResourceMap for pod %s resource %s: %+v", podDesc, resourceName, entry)
	return entry, nil
}

// GetPodPrimaryUDNDeviceID gets the last deviceId of the specified resources
// allocated for the given Pod, skipping excludedDeviceID: the device already
// consumed by another attachment of the same pod (the default network in DPU
// mode), whose position in the allocation list is not deterministic. Devices
// that multus handed to other NADs from the same pool are not visible here,
// so only collisions with the default network are prevented.
func GetPodPrimaryUDNDeviceID(pod *corev1.Pod, resourceName, excludedDeviceID string) (string, error) {
	entry, err := getPodResourceInfo(pod, resourceName)
	if err != nil {
		return "", err
	}
	deviceID := pickDeviceID(entry.DeviceIDs, excludedDeviceID)
	if deviceID == "" {
		return "", fmt.Errorf("no available device IDs found for pod %s/%s, resource %s (allocated: %v, excluded: %q)",
			pod.Namespace, pod.Name, resourceName, entry.DeviceIDs, excludedDeviceID)
	}
	klog.V(4).Infof("Picked device ID %s for the primary UDN of pod %s/%s (allocated: %v, excluded: %q)",
		deviceID, pod.Namespace, pod.Name, entry.DeviceIDs, excludedDeviceID)
	return deviceID, nil
}

// pickDeviceID returns the last device ID that differs from excludedDeviceID,
// or "" if there is none.
func pickDeviceID(deviceIDs []string, excludedDeviceID string) string {
	for i := len(deviceIDs) - 1; i >= 0; i-- {
		if deviceIDs[i] != excludedDeviceID {
			return deviceIDs[i]
		}
	}
	return ""
}
