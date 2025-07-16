package pod

import (
	"fmt"
	"net"

	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
)

// macOwner compose the owner identifier reserved for MAC addresses management.
// Returns "<ns>/<pod-name>" for regular pods and "<ns>/<vm-name>" for VMs.
func macOwner(pod *corev1.Pod) string {
	// Check if this is a VM pod and persistent IPs are enabled
	if vmName, ok := pod.Labels[kubevirtv1.VirtualMachineNameLabel]; ok {
		return fmt.Sprintf("%s/%s", pod.Namespace, vmName)
	}

	// Default to pod-based identifier
	return fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
}

// ReleasePodReservedMacAddress releases pod's reserved MAC address, if exists.
// It removes the used MAC address, from pod network annotation, and remove it from the MAC manager store.
func (allocator *PodAnnotationAllocator) ReleasePodReservedMacAddress(pod *corev1.Pod, mac net.HardwareAddr) error {
	networkName := allocator.netInfo.GetNetworkName()
	if allocator.macManager == nil {
		klog.V(5).Infof("No mac manager for network %q, skipping mac address release", networkName)
		return nil
	}

	if vmKey := kubevirt.ExtractVMNameFromPod(pod); vmKey != nil {
		allVMPodsCompleted, err := kubevirt.AllVMPodsAreCompleted(allocator.podLister, pod)
		if err != nil {
			return fmt.Errorf("failed checking all VM %q pods are completed: %v", vmKey, err)
		}
		if !allVMPodsCompleted {
			klog.V(5).Infof(`Retaining MAC address %q of pod "%s/%s" on network %q because its in use by another VM pod`,
				mac, pod.Namespace, pod.Name, networkName)
			return nil
		}
	}

	owner := macOwner(pod)
	allocator.macManager.Release(owner, mac)

	klog.V(5).Infof("Released MAC %q owned by %q on network %q", mac, owner, networkName)

	return nil
}
