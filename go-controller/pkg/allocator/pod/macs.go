package pod

import (
	"fmt"
	"net"

	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	k8snet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
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

// InitializeMACManager initializes MAC reservation tracker with MAC addresses in use in the network.
func (allocator *PodAnnotationAllocator) InitializeMACManager() error {
	networkName := allocator.netInfo.GetNetworkName()
	if allocator.macManager == nil {
		klog.V(5).Infof("No mac manager for network %s, skipping mac address release", networkName)
		return nil
	}

	// reserve MCAs used by infra first, to prevent network disruptions to connected pods in case of conflict.
	infraMACs := calculateSubnetsInfraMACAddresses(allocator.netInfo)
	for owner, mac := range infraMACs {
		if err := allocator.macManager.Reserve(owner, mac); err != nil {
			return fmt.Errorf("failed to reserve infra MAC %q for owner %q on network %q: %w",
				mac, owner, networkName, err)
		}
		klog.V(5).Infof("Reserved MAC %q on initialization, for infra %q on network %q", mac, owner, networkName)
	}

	return nil
}

// calculateSubnetsInfraMACAddresses return map of the network infrastructure mac addresses and owner name.
// It calculates the gateway and management ports MAC addresses from their IP address.
func calculateSubnetsInfraMACAddresses(netInfo util.NetInfo) map[string]net.HardwareAddr {
	reservedMACs := map[string]net.HardwareAddr{}
	for _, subnet := range netInfo.Subnets() {
		if subnet.CIDR == nil {
			continue
		}

		gwIP := netInfo.GetNodeGatewayIP(subnet.CIDR)
		gwMAC := util.IPAddrToHWAddr(gwIP.IP)
		gwKey := fmt.Sprintf("gw-v%s", k8snet.IPFamilyOf(gwIP.IP))
		reservedMACs[gwKey] = gwMAC

		mgmtIP := netInfo.GetNodeManagementIP(subnet.CIDR)
		mgmtMAC := util.IPAddrToHWAddr(mgmtIP.IP)
		mgmtKey := fmt.Sprintf("mgmt-v%s", k8snet.IPFamilyOf(mgmtIP.IP))
		reservedMACs[mgmtKey] = mgmtMAC
	}

	return reservedMACs
}
