package pod

import (
	"fmt"
	"net"

	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	k8snet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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

	pods, err := allocator.fetchNetworkPods()
	if err != nil {
		return err
	}
	podMACs, err := indexMACAddrByPodPrimaryUDN(pods)
	if err != nil {
		return err
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
	for owner, mac := range podMACs {
		if rerr := allocator.macManager.Reserve(owner, mac); rerr != nil {
			return fmt.Errorf("failed to reserve pod MAC %q for owner %q on network %q: %w",
				mac, owner, networkName, rerr)
		}
		klog.V(5).Infof("Reserved MAC %q on initialization, for pod %q on network %q", mac, owner, networkName)
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

// fetchNetworkPods fetch running pods in to the network NAD namespaces.
func (allocator *PodAnnotationAllocator) fetchNetworkPods() ([]*corev1.Pod, error) {
	var netPods []*corev1.Pod
	for _, ns := range allocator.netInfo.GetNADNamespaces() {
		pods, err := allocator.podLister.Pods(ns).List(labels.Everything())
		if err != nil {
			return nil, fmt.Errorf("failed to list pods for namespace %q: %v", ns, err)
		}
		for _, pod := range pods {
			if pod == nil {
				continue
			}
			if pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() && len(pod.Finalizers) == 0 {
				// skip non-running or about to dispose pods
				continue
			}
			netPods = append(netPods, pod)
		}
	}
	return netPods, nil
}

// indexMACAddrByPodPrimaryUDN indexes the MAC address of the primary UDN for each pod.
// It returns a map where keys are the owner ID (composed by macOwner, e.g., "namespace/pod-name")
// and the values are the corresponding MAC addresses.
func indexMACAddrByPodPrimaryUDN(pods []*corev1.Pod) (map[string]net.HardwareAddr, error) {
	indexedMACs := map[string]net.HardwareAddr{}
	for _, pod := range pods {
		podNetworks, err := util.UnmarshalPodAnnotationAllNetworks(pod.Annotations)
		if err != nil {
			return nil, fmt.Errorf(`failed to unmarshal pod-network annotation "%s/%s": %v`, pod.Namespace, pod.Name, err)
		}
		for _, network := range podNetworks {
			if network.Role != types.NetworkRolePrimary {
				// filter out default network and secondary user-defined networks
				continue
			}
			mac, perr := net.ParseMAC(network.MAC)
			if perr != nil {
				return nil, fmt.Errorf(`failed to parse mac address "%s/%s": %v`, pod.Namespace, pod.Name, perr)
			}
			indexedMACs[macOwner(pod)] = mac
		}
	}

	return indexedMACs, nil
}
