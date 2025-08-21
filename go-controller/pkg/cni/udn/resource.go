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

// GetPodPrimaryUDNDeviceID gets last deviceId of the specified resources allocated for the given Pod
func GetPodPrimaryUDNDeviceID(pod *corev1.Pod, resourceName string) (string, error) {
	entry, err := getPodResourceInfo(pod, resourceName)
	if err != nil {
		return "", err
	}
	if len(entry.DeviceIDs) == 0 {
		return "", fmt.Errorf("no device IDs found for pod %s/%s, resource %s", pod.Namespace, pod.Name, resourceName)
	}
	return entry.DeviceIDs[len(entry.DeviceIDs)-1], nil
}
