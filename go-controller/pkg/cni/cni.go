package cni

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	current "github.com/containernetworking/cni/pkg/types/100"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/libovsdb/client"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/udn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops/ovs"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var (
	minRsrc           = resource.MustParse("1k")
	maxRsrc           = resource.MustParse("1P")
	BandwidthNotFound = &notFoundError{}
)

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

func (pr *PodRequest) cmdAdd(kubeAuth *KubeAPIAuth, clientset *ClientSet, networkManager networkmanager.Interface, ovsClient client.Client) (*Response, error) {
	return pr.cmdAddWithGetCNIResultFunc(kubeAuth, clientset, getCNIResult, networkManager, ovsClient)
}

// primaryDPUReady makes sure previous annotation condition is ready, then if primary UDN interface is needed and it is
// in the DPU-HOST/DPU setup, checks if DPU connection annotations for primary UDN interface are ready.
func (pr *PodRequest) primaryDPUReady(primaryUDN *udn.UserDefinedPrimaryNetwork, k kube.Interface, podLister corev1listers.PodLister, annotCondFn podAnnotWaitCond) podAnnotWaitCond {
	return func(pod *corev1.Pod, nadKey string) (*util.PodAnnotation, bool, error) {
		// First, check the original annotation condition
		annotation, isReady, err := annotCondFn(pod, nadKey)
		if err != nil || !isReady {
			return annotation, isReady, err
		}
		// primaryUDNPodRequest would be nil if no primary UDN interface is needed
		primaryUDNPodRequest := pr.buildPrimaryUDNPodRequest(primaryUDN)
		// DPU-Host: add DPU connection-details annotation to allow DPU performs the needed primary UDN interface plumbing.
		if config.OvnKubeNode.Mode == types.NodeModeDPUHost &&
			primaryUDNPodRequest != nil && primaryUDNPodRequest.CNIConf.DeviceID != "" {
			netdevName := primaryUDN.NetworkDevice()
			if err := primaryUDNPodRequest.addDPUConnectionDetailsAnnot(k, podLister, netdevName); err != nil {
				return annotation, false, err
			}
			// Check if DPU status annotation is ready (passing nil as we've already checked annotation)
			return isDPUReady(nil, primaryUDN.NADName())(pod, nadKey)
		}
		// Non-DPU case: proceed normally
		return annotation, true, nil
	}
}

func (pr *PodRequest) cmdAddWithGetCNIResultFunc(
	kubeAuth *KubeAPIAuth,
	clientset *ClientSet,
	getCNIResultFn getCNIResultFunc,
	networkManager networkmanager.Interface,
	ovsClient client.Client,
) (*Response, error) {
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

	if pr.netName != types.DefaultNetworkName {
		nadKey, err := GetCNINADKey(pod, pr.IfName, pr.nadName)
		if err != nil {
			return nil, fmt.Errorf("failed to get NAD key for CNI Add request %v: %v", pr, err)
		}
		pr.nadKey = nadKey
	} else {
		pr.nadKey = pr.nadName
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
		if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
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
	// Get the IP address and MAC address of the pod
	// for DPU, ensure connection-details is present

	primaryUDN := udn.NewPrimaryNetwork(networkManager, clientset.nadLister)
	if util.IsNetworkSegmentationSupportEnabled() {
		annotCondFn = primaryUDN.WaitForPrimaryAnnotationFn(annotCondFn)
		// checks for primary UDN network's DPU connections status
		annotCondFn = pr.primaryDPUReady(primaryUDN, kubecli, clientset.podLister, annotCondFn)
	}

	// now checks for default network's DPU connection status
	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		if pr.CNIConf.DeviceID != "" {
			annotCondFn = isDPUReady(annotCondFn, pr.nadKey)
		}
	}
	pod, annotations, podNADAnnotation, err := GetPodWithAnnotations(pr.ctx, clientset, namespace, podName, pr.nadKey, annotCondFn)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod annotation: %v", err)
	}

	var primaryUDNPodInfo *PodInterfaceInfo
	primaryUDNPodRequest := pr.buildPrimaryUDNPodRequest(primaryUDN)
	if primaryUDNPodRequest != nil {
		primaryUDNPodInfo, err = primaryUDNPodRequest.buildPodInterfaceInfo(annotations, primaryUDN.Annotation(), primaryUDN.NetworkDevice())
		if err != nil {
			return nil, err
		}
		klog.V(4).Infof("Pod %s/%s primaryUDN podRequest %v podInfo %v", namespace, podName, primaryUDNPodRequest, primaryUDNPodInfo)
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

	response := &Response{KubeAuth: kubeAuth}
	if !config.UnprivilegedMode {
		netName := pr.netName
		if pr.CNIConf.PhysicalNetworkName != "" {
			netName = pr.CNIConf.PhysicalNetworkName
		}

		// Skip checking bridge mapping on DPU hosts as OVS is not present
		if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
			if err := checkBridgeMapping(ovsClient, pr.CNIConf.Topology, netName); err != nil {
				return nil, fmt.Errorf("failed bridge mapping validation: %w", err)
			}
		}

		response.Result, err = getCNIResultFn(pr, clientset, podInterfaceInfo)
		if err != nil {
			return nil, err
		}
		if primaryUDNPodRequest != nil {
			err = primaryUDNCmdAddGetCNIResultFunc(response.Result, getCNIResultFn, primaryUDNPodRequest, clientset, primaryUDNPodInfo)
			if err != nil {
				return nil, err
			}
		}
	} else {
		response.PodIFInfo = podInterfaceInfo
		if primaryUDNPodRequest != nil {
			response.PrimaryUDNPodInfo = primaryUDNPodInfo
			response.PrimaryUDNPodReq = primaryUDNPodRequest
		}
	}

	return response, nil
}

func primaryUDNCmdAddGetCNIResultFunc(result *current.Result, getCNIResultFn getCNIResultFunc, primaryUDNPodRequest *PodRequest,
	clientset PodInfoGetter, primaryUDNPodInfo *PodInterfaceInfo) error {
	primaryUDNResult, err := getCNIResultFn(primaryUDNPodRequest, clientset, primaryUDNPodInfo)
	if err != nil {
		return err
	}

	result.Routes = append(result.Routes, primaryUDNResult.Routes...)
	numOfInitialIPs := len(result.IPs)
	numOfInitialIfaces := len(result.Interfaces)
	result.Interfaces = append(result.Interfaces, primaryUDNResult.Interfaces...)
	result.IPs = append(result.IPs, primaryUDNResult.IPs...)

	// Offset the index of the default network IPs to correctly point to the default network interfaces
	for i := numOfInitialIPs; i < len(result.IPs); i++ {
		ifaceIPConfig := result.IPs[i].Copy()
		if result.IPs[i].Interface != nil {
			result.IPs[i].Interface = current.Int(*ifaceIPConfig.Interface + numOfInitialIfaces)
		}
	}
	return nil
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

	netdevName := ""
	if pr.CNIConf.DeviceID != "" {
		if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
			if pod == nil {
				// no need to update DPU connection-details annotation if pod is already removed
				klog.Warningf("Failed to get pod %s/%s: %v", pr.PodNamespace, pr.PodName, err)
				return response, nil
			}
			dpuCD, err := util.UnmarshalPodDPUConnDetails(pod.Annotations, pr.nadKey)
			if err != nil {
				klog.Warningf("Failed to get DPU connection details annotation for pod %s/%s NAD key %s: %v", pr.PodNamespace,
					pr.PodName, pr.nadKey, err)
				return response, nil
			}

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
		IsDPUHostMode: config.OvnKubeNode.Mode == types.NodeModeDPUHost,
		NetdevName:    netdevName,
	}
	if !config.UnprivilegedMode {
		err := podRequestInterfaceOps.UnconfigureInterface(pr, podInterfaceInfo)
		if err != nil {
			return nil, err
		}
	} else {
		// pass the isDPU flag and vfNetdevName back to cniShim
		response.Result = nil
		response.PodIFInfo = podInterfaceInfo
	}
	return response, nil
}

func (pr *PodRequest) cmdCheck() error {
	// noop...CMD check is not considered useful, and has a considerable performance impact
	// to pod bring up times with CRIO. This is due to the fact that CRIO currently calls check
	// after CNI ADD before it finishes bringing the container up
	return nil
}

// HandlePodRequest is the callback for all the requests
// coming to the cniserver after being processed into PodRequest objects
// Argument '*PodRequest' encapsulates all the necessary information
// kclient is passed in so that clientset can be reused from the server
// Return value is the actual bytes to be sent back without further processing.
func HandlePodRequest(
	request *PodRequest,
	clientset *ClientSet,
	kubeAuth *KubeAPIAuth,
	networkManager networkmanager.Interface,
	ovsClient client.Client,
) ([]byte, error) {
	var result, resultForLogging []byte
	var response *Response
	var err, err1 error

	klog.Infof("%s %s starting CNI request %+v", request, request.Command, request)
	switch request.Command {
	case CNIAdd:
		response, err = request.cmdAdd(kubeAuth, clientset, networkManager, ovsClient)
	case CNIDel:
		response, err = request.cmdDel(clientset)
	case CNICheck:
		err = request.cmdCheck()
	default:
	}

	if response != nil {
		if result, err1 = response.Marshal(); err1 != nil {
			return nil, fmt.Errorf("%s %s CNI request %+v failed to marshal result: %v",
				request, request.Command, request, err1)
		}
		if resultForLogging, err1 = response.MarshalForLogging(); err1 != nil {
			klog.Errorf("%s %s CNI request %+v, %v", request, request.Command, request, err1)
		}
	}

	klog.Infof("%s %s finished CNI request %+v, result %q, err %v",
		request, request.Command, request, string(resultForLogging), err)

	if err != nil {
		// Prefix errors with request info for easier failure debugging
		return nil, fmt.Errorf("%s %v", request, err)
	}
	return result, nil
}

// getCNIResult get result from pod interface info.
// PodInfoGetter is used to check if sandbox is still valid for the current
// instance of the pod in the apiserver, see checkCancelSandbox for more info.
// If kube api is not available from the CNI, pass nil to skip this check.
func getCNIResult(pr *PodRequest, getter PodInfoGetter, podInterfaceInfo *PodInterfaceInfo) (*current.Result, error) {
	interfacesArray, err := podRequestInterfaceOps.ConfigureInterface(pr, getter, podInterfaceInfo)
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

// buildPrimaryUDNPodRequest returns PodRequest for primary UDN interface,
// it returns nil if primary UDN is not requested on the Pod
func (pr *PodRequest) buildPrimaryUDNPodRequest(
	primaryUDN *udn.UserDefinedPrimaryNetwork,
) *PodRequest {
	if !primaryUDN.Found() {
		// if primary UDN interface is not needed, return nil
		return nil
	}
	deviceID, deviceInfo, isVFIO := primaryUDN.NetworkDeviceInfo()
	req := &PodRequest{
		Command:      pr.Command,
		PodNamespace: pr.PodNamespace,
		PodName:      pr.PodName,
		PodUID:       pr.PodUID,
		SandboxID:    pr.SandboxID,
		Netns:        pr.Netns,
		IfName:       primaryUDN.InterfaceName(),
		CNIConf: &ovncnitypes.NetConf{
			// primary UDN MTU will be taken from config.Default.MTU
			// if not specified at the NAD
			MTU:      primaryUDN.MTU(),
			DeviceID: deviceID,
		},
		timestamp:  time.Now(),
		IsVFIO:     isVFIO,
		netName:    primaryUDN.NetworkName(),
		nadName:    primaryUDN.NADName(),
		nadKey:     primaryUDN.NADName(),
		deviceInfo: *deviceInfo,
	}

	req.ctx, req.cancel = context.WithCancel(pr.ctx)
	return req
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
