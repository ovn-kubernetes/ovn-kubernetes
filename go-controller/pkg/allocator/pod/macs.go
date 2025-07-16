package pod

import (
	"errors"
	"fmt"
	"net"

	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	allocmac "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/mac"
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
	if allocator.macRegistry == nil {
		klog.V(5).Infof("No MAC registry defined for network %q, skipping MAC address release", networkName)
		return nil
	}

	macOwnerID := macOwner(pod)
	if vmKey := kubevirt.ExtractVMNameFromPod(pod); vmKey != nil {
		allVMPodsCompleted, err := kubevirt.AllVMPodsAreCompleted(allocator.podLister, pod)
		if err != nil {
			return fmt.Errorf("failed checking all VM %q pods are completed: %v", vmKey, err)
		}
		if !allVMPodsCompleted {
			klog.V(5).Infof(`Retaining MAC address %q for owner %q on network %q because its in use by another VM pod`,
				mac, macOwnerID, networkName)
			return nil
		}
	}

	if err := allocator.macRegistry.Release(macOwnerID, mac); err != nil {
		if errors.Is(err, allocmac.ErrReleaseMismatchOwner) {
			// the given pod is not the original MAC owner thus there is no point to retry. avoid retries by not returning an error.
			klog.Errorf(`Failed to release MAC %q for owner %q on network %q, because its originally reserved for different owner`, mac, macOwnerID, networkName)
		} else {
			return fmt.Errorf("failed to release MAC address %q for owner %q on network %q: %v", mac, macOwnerID, networkName, err)
		}
	} else {
		klog.V(5).Infof("Released MAC %q owned by %q on network %q", mac, macOwnerID, networkName)
	}

	return nil
}
