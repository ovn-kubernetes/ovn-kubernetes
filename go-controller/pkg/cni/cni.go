// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"fmt"
	"net"
	"strings"

	current "github.com/containernetworking/cni/pkg/types/100"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	ovs "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var (
	minRsrc           = resource.MustParse("1k")
	maxRsrc           = resource.MustParse("1P")
	BandwidthNotFound = &notFoundError{}
)

const dpuNotReadyMsg = "DPU Not Ready"

type direction int

func (d direction) String() string {
	if d == Egress {
		return "egress"
	}
	return "ingress"
}

const (
	Egress direction = iota
	Ingress
)

type notFoundError struct{}

func (*notFoundError) Error() string {
	return "not found"
}

func validateBandwidthIsReasonable(rsrc *resource.Quantity) error {
	if rsrc.Value() < minRsrc.Value() {
		return fmt.Errorf("resource is unreasonably small (< 1kbit)")
	}
	if rsrc.Value() > maxRsrc.Value() {
		return fmt.Errorf("resoruce is unreasonably large (> 1Pbit)")
	}
	return nil
}

func extractPodBandwidth(podAnnotations map[string]string, dir direction) (int64, error) {
	annotation := "kubernetes.io/ingress-bandwidth"
	if dir == Egress {
		annotation = "kubernetes.io/egress-bandwidth"
	}

	str, found := podAnnotations[annotation]
	if !found {
		return 0, BandwidthNotFound
	}
	bwVal, err := resource.ParseQuantity(str)
	if err != nil {
		return 0, err
	}
	if err := validateBandwidthIsReasonable(&bwVal); err != nil {
		return 0, err
	}
	return bwVal.Value(), nil
}

func (pr *PodRequest) String() string {
	return fmt.Sprintf("[%s/%s %s network %s NAD %s NAD key %s]", pr.PodNamespace, pr.PodName, pr.SandboxID, pr.netName, pr.nadName, pr.nadKey)
}

// checkOrUpdatePodUID validates the given pod UID against the request's existing
// pod UID. If the existing UID is empty the runtime did not support passing UIDs
// and the best we can do is use the given UID for the duration of the request.
// But if the existing UID is valid and does not match the given UID then the
// sandbox request is for a different pod instance and should be terminated.
// Static pod UID is a hash of the pod itself that does not match
// the UID of the mirror kubelet creates on the api /server.
// We will use the UID of the mirror.
// The hash is annotated in the mirror pod (kubernetes.io/config.hash)
// and we could match against it, but let's avoid that for now as it is not
// a published standard.
func (pr *PodRequest) checkOrUpdatePodUID(pod *corev1.Pod) error {
	if pr.PodUID == "" || IsStaticPod(pod) {
		// Runtime didn't pass UID, or the pod is a static pod, use the one we got from the pod object
		pr.PodUID = string(pod.UID)
	} else if string(pod.UID) != pr.PodUID {
		// Exit early if the pod was deleted and recreated already
		return fmt.Errorf("pod deleted before sandbox %v operation began. Request Pod UID %s is different from "+
			"the Pod UID (%s) retrieved from the informer/API", pr.Command, pr.PodUID, pod.UID)
	}
	return nil
}

func (pr *PodRequest) cmdAdd(kubeAuth *KubeAPIAuth, clientset *ClientSet, ovsClient client.Client) (*Response, error) {
	namespace := pr.PodNamespace
	podName := pr.PodName
	if namespace == "" || podName == "" {
		return nil, fmt.Errorf("required CNI variable missing")
	}

	kubecli := &kube.Kube{KClient: clientset.kclient}

	pod, _, _, err := GetPodWithAnnotations(pr.ctx, clientset, namespace, podName, "",
		func(*corev1.Pod, string) (*util.PodAnnotation, bool, error) {
			return nil, true, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod %s/%s: %v", namespace, podName, err)
	}

	// nadKey is only set for default network and primary UDN
	if pr.nadKey == "" {
		nadKey, err := GetCNINADKey(pod, pr.IfName, pr.nadName)
		if err != nil {
			return nil, fmt.Errorf("failed to get NAD key for CNI Add request %v: %v", pr, err)
		}
		pr.nadKey = nadKey
	}

	annotCondFn := isOvnReady
	netdevName := ""
	if pr.CNIConf.DeviceID != "" {
		var err error

		if !pr.IsVFIO {
			netdevName, err = util.GetNetdevNameFromDeviceId(pr.CNIConf.DeviceID, pr.deviceInfo)
			if err != nil {
				return nil, fmt.Errorf("failed in cmdAdd while getting Netdevice name: %w", err)
			}
		}
		if config.IsModeDPUHost() {
			// Add DPU connection-details annotation so ovnkube-node running on DPU
			// performs the needed network plumbing.
			if err = pr.addDPUConnectionDetailsAnnot(kubecli, clientset.podLister, netdevName); err != nil {
				return nil, err
			}
			// Defer default-network DPU readiness gating so the primary UDN annotation/DPU readiness can progress in parallel when present.
		}
		// In the case of SmartNIC (CX5), we store the netdevname in the representor's
		// OVS interface's external_id column. This is done in ConfigureInterface().
	}

	// now checks for default network's DPU connection status
	if config.IsModeDPUHost() {
		if pr.CNIConf.DeviceID != "" {
			annotCondFn = isDPUReady(annotCondFn, pr.nadKey)
		}
	}

	// Get the IP address and MAC address of the pod
	// for DPU, ensure connection-details is present
	pod, annotations, podNADAnnotation, err := GetPodWithAnnotations(pr.ctx, clientset, namespace, podName, pr.nadKey, annotCondFn)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod annotation: %v", err)
	}

	if err = pr.checkOrUpdatePodUID(pod); err != nil {
		return nil, err
	}

	podInterfaceInfo, err := pr.buildPodInterfaceInfo(annotations, podNADAnnotation, netdevName)
	if err != nil {
		return nil, err
	}
	// get all the Pod interface names of the same nadName. See if this is a pod with multiple secondary UDN of nadName
	podIfNamesOfSameNAD, _ := GetPodIfNamesForNAD(pod, pr.nadName)
	if len(podIfNamesOfSameNAD) > 1 {
		podInterfaceInfo.PodIfNamesOfSameNAD = podIfNamesOfSameNAD
	}

	podInterfaceInfo.SkipIPConfig = kubevirt.IsPodLiveMigratable(pod)

	// On a DHCP IPAM network the annotation's L3 config is a report of the
	// PREVIOUS sandbox's externally-owned lease, not an OVN-K reservation:
	// that lease died with its sandbox (released on DEL, keyed to its
	// container ID), so on a repeat ADD re-applying it would program a stale
	// IP on the fresh interface and a stale default route that collides
	// with the primary network's default route — failing the ADD outright
	// ("failed to add gateway route to link: file exists") and looping the
	// sandbox forever. Clear the L3 fields so the DHCP exchange below is
	// the only source of addressing, making every repeat ADD identical to
	// the first ADD. The MAC is OVN-K-allocated and must stay.
	if podNADAnnotation.IPAMMode == types.IPAMTypeDHCP {
		podInterfaceInfo.IPs = nil
		podInterfaceInfo.Gateways = nil
		podInterfaceInfo.Routes = nil
	}

	response := &Response{KubeAuth: kubeAuth}
	if !config.UnprivilegedMode {
		if ovsClient == nil && !config.IsModeDPUHost() {
			return nil, fmt.Errorf("OVS client is required in privileged mode")
		}

		netName := pr.netName
		if pr.CNIConf.PhysicalNetworkName != "" {
			netName = pr.CNIConf.PhysicalNetworkName
		}

		// Skip checking bridge mapping on DPU hosts as OVS is not present
		if config.IsModeDPU() || config.IsModeFull() {
			if err := checkBridgeMapping(ovsClient, pr.CNIConf.Topology, netName); err != nil {
				return nil, fmt.Errorf("failed bridge mapping validation: %w", err)
			}
		}

		response.Result, err = getCNIResult(pr, ovsClient, clientset, podInterfaceInfo)
		if err != nil {
			return nil, err
		}

		// If IPAM mode is DHCP, obtain IP configuration from an external DHCP
		// server. The lease-handling behavior is selected by workload type:
		//   - KubeVirt VMs: perform a one-shot DHCP discovery to learn the IP and
		//     report it via the pod annotation only. The IP is never applied to the
		//     interface (a VFIO VF loses it on rebind; a non-VFIO VM would start
		//     KubeVirt's in-pod dnsmasq). The guest runs its own DHCP client.
		//   - Regular pods: delegate to the DHCP CNI plugin daemon, which applies
		//     the IP and maintains the lease for the pod's lifetime.
		//
		// In both cases the learned IPs are merged into the CNI result (multus
		// network-status) and patched into the k8s.ovn.org/pod-networks
		// annotation so ovnkube-controller programs the logical switch port and
		// IP-based features (MultiNetworkPolicy, NetworkQoS) see the address.
		if pr.CNIConf.IPAM.Type == types.IPAMTypeDHCP {
			// DHCP IPAM runs on the node itself: full mode owns the host OVS
			// datapath and the lease marker; DPU-host mode delegates OVS to
			// the DPU (representor plumbed there, gated by connection-status
			// Ready) and records the marker in a host-local file.
			if !config.IsModeFull() && !config.IsModeDPUHost() {
				return nil, fmt.Errorf("DHCP IPAM is not supported in ovnkube-node mode %q for pod %s/%s",
					config.OvnKubeNode.Mode, pr.PodNamespace, pr.PodName)
			}
			var dhcpResult *current.Result
			if kubevirt.IsPodOwnedByVirtualMachine(pod) {
				dhcpResult, err = dhcpOps.DoOneShot(pr)
				if err != nil {
					return nil, fmt.Errorf("VM DHCP discovery failed for pod %s/%s: %v",
						pr.PodNamespace, pr.PodName, err)
				}
			} else {
				// DHCP delegation needs a netdev inside the pod network
				// namespace to perform the DORA exchange and apply the lease;
				// a VFIO device exposes none (the VF is consumed via
				// /dev/vfio/*) and, unlike a KubeVirt VM, no DHCP client will
				// run inside the workload to own the lease. Reject explicitly
				// instead of failing obscurely in the DHCP CNI plugin.
				if pr.IsVFIO {
					return nil, fmt.Errorf("dhcp IPAM mode is not supported for VFIO device %s on regular pod %s/%s: "+
						"DHCP with VFIO is only supported for KubeVirt VM pods",
						pr.CNIConf.DeviceID, pr.PodNamespace, pr.PodName)
				}
				// Record the lease marker BEFORE asking the daemon for a
				// lease: cmdDel keys the release off this marker, so a lease
				// must never exist without it (an unreleased lease leaks a
				// renewal goroutine in the daemon until lease expiry). The
				// inverse — marker without lease, when the acquisition below
				// fails — only costs the failed-ADD's teardown one no-op
				// release. Fatal on failure, like every other write that
				// establishes this pod's state.
				if err := pr.recordDHCPLeaseMarker(response.Result); err != nil {
					return nil, fmt.Errorf("failed to record DHCP lease marker for pod %s/%s: %w",
						pr.PodNamespace, pr.PodName, err)
				}
				dhcpResult, err = dhcpOps.ExecAdd(pr)
				if err != nil {
					return nil, fmt.Errorf("DHCP IPAM ADD failed for pod %s/%s: %v",
						pr.PodNamespace, pr.PodName, err)
				}
			}
			mergeDHCPResultIntoCNIResult(dhcpResult, response.Result)
			if err := pr.updatePodNetworksAnnotationWithDHCPResult(clientset, dhcpResult); err != nil {
				return nil, fmt.Errorf("failed to report DHCP IPs in pod-networks annotation for pod %s/%s: %v",
					pr.PodNamespace, pr.PodName, err)
			}
		}
	} else {
		if pr.CNIConf.IPAM.Type == types.IPAMTypeDHCP {
			return nil, fmt.Errorf("dhcp IPAM mode for localnet topology is not supported in unprivileged mode")
		}
		response.PodIFInfo = podInterfaceInfo
	}
	return response, nil
}

func (pr *PodRequest) cmdDel(clientset *ClientSet) (*Response, error) {
	// assume success case, return an empty Result
	response := &Response{}
	response.Result = &current.Result{}

	namespace := pr.PodNamespace
	podName := pr.PodName
	if namespace == "" || podName == "" {
		return nil, fmt.Errorf("required CNI variable missing")
	}

	pod, err := clientset.getPod(pr.PodNamespace, pr.PodName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get pod %s/%s: %w", pr.PodNamespace, pr.PodName, err)
		}
	}

	if pod != nil && pr.netName != types.DefaultNetworkName {
		nadKey, err := GetCNINADKey(pod, pr.IfName, pr.nadName)
		if err != nil {
			return nil, err
		}
		pr.nadKey = nadKey
	} else {
		pr.nadKey = pr.nadName
	}

	// Release the DHCP lease before anything tears down the pod's datapath.
	// In full mode that teardown is UnconfigureInterface below; placing the
	// release here — above the DeviceID block — keeps it ahead of that (and,
	// for a future DPU-host backend, ahead of the connection-details deletion
	// that makes the DPU unplug the representor). Only the pod delegation path
	// registers a lease with the DHCP CNI daemon, recorded on the OVS
	// interface at ADD time (KubeVirt VMs run their own DHCP client, so no
	// marker and no RELEASE). Keying off node-local state keeps CNI DEL
	// independent of the apiserver. A failed marker lookup or a failed release
	// fails the whole DEL: the marker (in full mode, the OVS port carrying it)
	// must survive so the retried DEL re-attempts the release — otherwise the
	// daemon keeps renewing a lease for a pod that no longer exists until the
	// server-side TTL expires. The cost is that pod deletion blocks while the
	// dhcp daemon is unreachable. Skipped in unprivileged mode, where DHCP
	// IPAM is rejected at ADD and no lease is ever recorded.
	if !config.UnprivilegedMode && pr.CNIConf.IPAM.Type == types.IPAMTypeDHCP {
		markerExists, markerErr := pr.dhcpLeaseMarkerExists()
		if markerErr != nil {
			return nil, markerErr
		}
		if markerExists {
			klog.Infof("DHCP: releasing lease for pod %s/%s iface %s netns %s container %s",
				pr.PodNamespace, pr.PodName, pr.IfName, pr.Netns, pr.SandboxID)
			if delErr := dhcpOps.ExecDel(pr); delErr != nil {
				return nil, fmt.Errorf("failed to release the DHCP lease of pod %s/%s (sandbox %s, iface %s): %w",
					pr.PodNamespace, pr.PodName, pr.SandboxID, pr.IfName, delErr)
			}
			// No drain wait is needed before the port teardown below: the
			// daemon transmits the RELEASE synchronously before its RPC
			// returns, and releases are best-effort by protocol (RFC 2131
			// §4.4.4) — a lost one simply expires at the server's TTL.
			klog.Infof("DHCP: released lease for pod %s/%s", pr.PodNamespace, pr.PodName)
			pr.removeDHCPLeaseMarker()
		}
	}

	netdevName := ""
	if pr.CNIConf.DeviceID != "" {
		if config.IsModeDPUHost() {
			var dpuCD *util.DPUConnectionDetails
			if pod == nil {
				// no need to update DPU connection-details annotation if pod is already removed
				klog.Warningf("Failed to get pod %s/%s: %v", pr.PodNamespace, pr.PodName, err)
			} else {
				dpuCD, err = util.UnmarshalPodDPUConnDetails(pod.Annotations, pr.nadKey)
				if err != nil {
					klog.Warningf("Failed to get DPU connection details annotation for pod %s/%s NAD key %s: %v", pr.PodNamespace,
						pr.PodName, pr.nadKey, err)
				}
			}
			if dpuCD == nil {
				if !util.IsSimulatedDPU() {
					return response, nil
				}
				// A simulated device is a veth and is destroyed with the pod namespace unless it is moved back first.
				netdevName = pr.CNIConf.DeviceID
			} else {
				// check if this cmdDel is meant for the current sandbox, if not, directly return
				if dpuCD.SandboxId != pr.SandboxID {
					klog.Infof("The cmdDel request for sandbox %s is not meant for the currently configured "+
						"pod %s/%s on NAD key %s with sandbox %s. Ignoring this request.",
						pr.SandboxID, namespace, podName, pr.nadKey, dpuCD.SandboxId)
					return response, nil
				}

				netdevName = dpuCD.VfNetdevName
				if pr.netName == types.DefaultNetworkName {
					// if this is the default network name, remove the whole DPU connection-details annotation,
					// including the primary UDN connection-details if any
					updatePodAnnotationNoRollback := func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
						delete(pod.Annotations, util.DPUConnectionDetailsAnnot)
						return pod, nil, nil
					}

					err = util.UpdatePodWithRetryOrRollback(
						clientset.podLister,
						&kube.Kube{KClient: clientset.kclient},
						pod,
						updatePodAnnotationNoRollback,
					)
				} else {
					// Delete the DPU connection-details annotation for this NAD
					err = pr.updatePodDPUConnDetailsWithRetry(&kube.Kube{KClient: clientset.kclient}, clientset.podLister, nil)
				}
				// not an error if pod has already been deleted
				if err != nil && !apierrors.IsNotFound(err) {
					return nil, fmt.Errorf("failed to cleanup the DPU connection details annotation for NAD key %s: %v", pr.nadKey, err)
				}
			}
		} else {
			// Find the hostInterface name
			condString := []string{"external-ids:sandbox=" + pr.SandboxID}
			condString = append(condString, fmt.Sprintf("external_ids:pod-if-name=%s", pr.IfName))
			ovsIfNames, err := ovsFind("Interface", "name", condString...)
			if err != nil || len(ovsIfNames) != 1 {
				// the pod was added before "external_ids:pod-if-name" was introduced, fall back to the old way to find
				// out the OVS interface associated with this CNIDel request
				condString = []string{"external-ids:sandbox=" + pr.SandboxID}
				if pr.netName != types.DefaultNetworkName {
					condString = append(condString, fmt.Sprintf("external_ids:%s=%s", types.NADExternalID, pr.nadKey))
				} else {
					condString = append(condString, fmt.Sprintf("external_ids:%s{=}[]", types.NADExternalID))
				}
				ovsIfNames, err = ovsFind("Interface", "name", condString...)
			}

			if err != nil || len(ovsIfNames) != 1 {
				klog.Warningf("Couldn't find the OVS interface for pod %s/%s NAD key %s: %v",
					pr.PodNamespace, pr.PodName, pr.nadKey, err)
			} else {
				out, err := ovsGet("interface", ovsIfNames[0], "external_ids", "vf-netdev-name")
				if err != nil {
					klog.Warningf("Couldn't find the original Netdev name from OVS interface %s for pod %s/%s: %v",
						ovsIfNames[0], pr.PodNamespace, pr.PodName, err)
				} else {
					netdevName = out
				}
			}
		}
	}

	podInterfaceInfo := &PodInterfaceInfo{
		IsDPUHostMode: config.IsModeDPUHost(),
		NetdevName:    netdevName,
	}
	if !config.UnprivilegedMode {
		err := podRequestInterfaceOps.UnconfigureInterface(pr, podInterfaceInfo)
		if err != nil {
			return nil, err
		}
	} else {
		// pass the isDPU flag and vfNetdevName back to cniShim
		if pr.CNIConf.IPAM.Type == types.IPAMTypeDHCP {
			klog.Warningf("DHCP lease release skipped for pod %s/%s: not supported in unprivileged mode",
				pr.PodNamespace, pr.PodName)
		}
		response.Result = nil
		response.PodIFInfo = podInterfaceInfo
	}
	return response, nil
}

// getCNIResult get result from pod interface info.
// PodInfoGetter is used to check if sandbox is still valid for the current
// instance of the pod in the apiserver, see checkCancelSandbox for more info.
// If kube api is not available from the CNI, pass nil to skip this check.
func getCNIResult(pr *PodRequest, ovsClient client.Client, getter PodInfoGetter, podInterfaceInfo *PodInterfaceInfo) (*current.Result, error) {
	interfacesArray, err := podRequestInterfaceOps.ConfigureInterface(pr, ovsClient, getter, podInterfaceInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to configure pod interface: %v", err)
	}

	gateways := map[string]net.IP{}
	for _, gw := range podInterfaceInfo.Gateways {
		if gw.To4() != nil && gateways["4"] == nil {
			gateways["4"] = gw
		} else if gw.To4() == nil && gateways["6"] == nil {
			gateways["6"] = gw
		}
	}

	// Build the result structure to pass back to the runtime
	ips := []*current.IPConfig{}
	for _, ipcidr := range podInterfaceInfo.IPs {
		ip := &current.IPConfig{
			Interface: current.Int(1),
			Address:   *ipcidr,
		}
		var ipVersion string
		if utilnet.IsIPv6CIDR(ipcidr) {
			ipVersion = "6"
		} else {
			ipVersion = "4"
		}
		ip.Gateway = gateways[ipVersion]
		ips = append(ips, ip)
	}

	return &current.Result{
		Interfaces: interfacesArray,
		IPs:        ips,
	}, nil
}

func (pr *PodRequest) buildPodInterfaceInfo(annotations map[string]string, podAnnotation *util.PodAnnotation, netDevice string) (*PodInterfaceInfo, error) {
	return PodAnnotation2PodInfo(
		annotations,
		podAnnotation,
		pr.PodUID,
		netDevice,
		pr.nadKey,
		pr.netName,
		pr.CNIConf.MTU,
	)
}

func checkBridgeMapping(ovsClient client.Client, topology string, networkName string) error {
	if topology != types.LocalnetTopology || networkName == types.DefaultNetworkName {
		return nil
	}

	openvSwitch, err := ovs.GetOpenvSwitch(ovsClient)
	if err != nil {
		return fmt.Errorf("failed getting openvswitch: %w", err)
	}

	ovnBridgeMappings := openvSwitch.ExternalIDs["ovn-bridge-mappings"]

	bridgeMappings := strings.Split(ovnBridgeMappings, ",")
	for _, bridgeMapping := range bridgeMappings {
		networkBridgeAssociation := strings.Split(bridgeMapping, ":")
		if len(networkBridgeAssociation) == 2 && networkBridgeAssociation[0] == networkName {
			return nil
		}
	}
	klog.V(5).Infof("Failed to find bridge mapping for network: %q, current OVN bridge-mappings: (%s)", networkName, ovnBridgeMappings)
	return fmt.Errorf("failed to find OVN bridge-mapping for network: %q", networkName)
}
