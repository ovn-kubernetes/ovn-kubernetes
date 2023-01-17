package ovn

import (
	"fmt"
	"net"
	"reflect"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/dhcp"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

func (bsnc *BaseSecondaryNetworkController) getPortInfoForSecondaryNetwork(pod *kapi.Pod) map[string]*lpInfo {
	if !util.PodWantsNetwork(pod) {
		return nil
	}
	portInfoMap, _ := bsnc.logicalPortCache.getAll(pod)
	return portInfoMap
}

// GetInternalCacheEntryForSecondaryNetwork returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (bsnc *BaseSecondaryNetworkController) GetInternalCacheEntryForSecondaryNetwork(objType reflect.Type, obj interface{}) interface{} {
	switch objType {
	case factory.PodType:
		pod := obj.(*kapi.Pod)
		return bsnc.getPortInfoForSecondaryNetwork(pod)
	default:
		return nil
	}
}

// AddSecondaryNetworkResourceCommon adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation. This function is called for secondary network only.
func (bsnc *BaseSecondaryNetworkController) AddSecondaryNetworkResourceCommon(objType reflect.Type, obj interface{}) error {
	switch objType {
	case factory.PodType:
		pod, ok := obj.(*kapi.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T object to *knet.Pod", obj)
		}
		return bsnc.ensurePodForSecondaryNetwork(pod, true)

	default:
		return fmt.Errorf("object type %s not supported", objType)
	}
}

// UpdateSecondaryNetworkResourceCommon updates the specified object in the cluster to its version in newObj
// according to its type and returns the error, if any, yielded during the object update. This function is
// called for secondary network only.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (bsnc *BaseSecondaryNetworkController) UpdateSecondaryNetworkResourceCommon(objType reflect.Type, oldObj, newObj interface{}, inRetryCache bool) error {
	switch objType {
	case factory.PodType:
		oldPod := oldObj.(*kapi.Pod)
		newPod := newObj.(*kapi.Pod)

		return bsnc.ensurePodForSecondaryNetwork(newPod, inRetryCache || util.PodScheduled(oldPod) != util.PodScheduled(newPod))

	default:
		return fmt.Errorf("object type %s not supported", objType)
	}
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods.
// This function is called for secondary network only.
func (bsnc *BaseSecondaryNetworkController) DeleteSecondaryNetworkResourceCommon(objType reflect.Type, obj, cachedObj interface{}) error {
	switch objType {
	case factory.PodType:
		var portInfoMap map[string]*lpInfo
		pod := obj.(*kapi.Pod)

		if cachedObj != nil {
			portInfoMap = cachedObj.(map[string]*lpInfo)
		}
		return bsnc.removePodForSecondaryNetwork(pod, portInfoMap)

	default:
		return fmt.Errorf("object type %s not supported", objType)
	}
}

// ensurePodForSecondaryNetwork tries to set up secondary network for a pod. It returns nil on success and error
// on failure; failure indicates the pod set up should be retried later.
func (bsnc *BaseSecondaryNetworkController) ensurePodForSecondaryNetwork(pod *kapi.Pod, addPort bool) error {

	// Try unscheduled pods later
	if !util.PodScheduled(pod) {
		return nil
	}

	if !util.PodWantsNetwork(pod) && !addPort {
		return nil
	}

	// If a node does not have an assigned hostsubnet don't wait for the logical switch to appear
	switchName, err := bsnc.getExpectedSwitchName(pod)
	if err != nil {
		return err
	}

	on, network, err := util.PodWantsMultiNetwork(pod, bsnc.NetInfo)
	if err != nil {
		// configuration error, no need to retry, do not return error
		klog.Errorf("Error getting network-attachment for pod %s/%s network %s: %v",
			pod.Namespace, pod.Name, bsnc.GetNetworkName(), err)
		return nil
	}

	if !on {
		// the pod is not attached to this specific network
		klog.V(5).Infof("Pod %s/%s is not attached on this network controller %s",
			pod.Namespace, pod.Name, bsnc.GetNetworkName())
		return nil
	}

	if bsnc.lsManager.IsNonHostSubnetSwitch(switchName) {
		return nil
	}

	nadName := util.GetNADName(network.Namespace, network.Name)

	var libovsdbExecuteTime time.Duration

	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort for NAD %s took %v, libovsdb time %v",
			pod.Namespace, pod.Name, nadName, time.Since(start), libovsdbExecuteTime)
	}()

	ops, lsp, podAnnotation, newlyCreated, err := bsnc.addLogicalPortToNetwork(pod, nadName, network)
	if err != nil {
		return err
	}

	recordOps, txOkCallBack, _, err := bsnc.AddConfigDurationRecord("pod", pod.Namespace, pod.Name)
	if err != nil {
		klog.Errorf("Config duration recorder: %v", err)
	}
	ops = append(ops, recordOps...)

	transactStart := time.Now()
	_, err = libovsdbops.TransactAndCheckAndSetUUIDs(bsnc.nbClient, lsp, ops)
	libovsdbExecuteTime = time.Since(transactStart)
	if err != nil {
		return fmt.Errorf("error transacting operations %+v: %v", ops, err)
	}
	txOkCallBack()
	bsnc.podRecorder.AddLSP(pod.UID, bsnc.NetInfo)

	// if somehow lspUUID is empty, there is a bug here with interpreting OVSDB results
	if len(lsp.UUID) == 0 {
		return fmt.Errorf("UUID is empty from LSP: %+v", *lsp)
	}

	_ = bsnc.logicalPortCache.add(pod, switchName, nadName, lsp.UUID, podAnnotation.MAC, podAnnotation.IPs)

	if newlyCreated {
		metrics.RecordPodCreated(pod, bsnc.NetInfo)
	}

	if bsnc.IsDHCP() {
		var switchSubnets []*net.IPNet
		if switchSubnets = bsnc.lsManager.GetSwitchSubnets(switchName); switchSubnets == nil {
			return fmt.Errorf("cannot retrieve subnet for assigning gateway routes switch: %s", switchName)
		}
		router := util.GetNodeGatewayIfAddr(switchSubnets[0]).IP.String()
		cidr := switchSubnets[0].String()
		dhcpOptions, err := dhcp.ComposeOptionsWithKubeDNS(bsnc.client, cidr, router)
		if err != nil {
			return fmt.Errorf("failed composing DHCP options: %v", err)
		}
		err = libovsdbops.CreateOrUpdateDhcpv4Options(bsnc.nbClient, lsp, dhcpOptions)
		if err != nil {
			return fmt.Errorf("failed adding ovn operations to add DHCP v4 options: %v", err)
		}
	}
	return nil
}

// removePodForSecondaryNetwork tried to tear down a for on a secondary network. It returns nil on success
// and error on failure; failure indicates the pod tear down should be retried later.
func (bsnc *BaseSecondaryNetworkController) removePodForSecondaryNetwork(pod *kapi.Pod, portInfoMap map[string]*lpInfo) error {
	var nadName string
	var portInfo *lpInfo

	podDesc := pod.Namespace + "/" + pod.Name
	klog.Infof("Deleting pod: %s for network %s", podDesc, bsnc.GetNetworkName())

	if portInfoMap != nil {
		if len(portInfoMap) == 0 {
			return nil
		}
		for nadName, portInfo = range portInfoMap {
			break
		}
	} else {
		if !util.PodWantsNetwork(pod) || !util.PodScheduled(pod) {
			return nil
		}

		// podInfoMap does not exist, it means the pod failed to be created or this is called when ovnkube-master
		// restarts and logicalPortCache is not filled in. In this case, we'd still need to delete resources (IPs,
		// logical switch port) created for this Pod.

		// get all nadNames from pod annotation, and see if any NAD is of this network
		podNetworks, err := util.UnmarshalPodAnnotationAllNetworks(pod.Annotations)
		if err != nil {
			return err
		}

		for nadName = range podNetworks {
			if bsnc.HasNAD(nadName) {
				break
			}
		}
		if nadName == "" {
			return nil
		}
	}
	bsnc.logicalPortCache.remove(pod, nadName)

	// TBD namespaceInfo needed when multi-network policy support is added
	pInfo, err := bsnc.deletePodLogicalPort(pod, portInfo, nadName)
	if err != nil {
		return err
	}

	// do not release IP address unless we have validated no other pod is using it
	if pInfo == nil {
		return nil
	}

	// Releasing IPs needs to happen last so that we can deterministically know that if delete failed that
	// the IP of the pod needs to be released. Otherwise we could have a completed pod failed to be removed
	// and we dont know if the IP was released or not, and subsequently could accidentally release the IP
	// while it is now on another pod
	klog.Infof("Attempting to release IPs for pod: %s/%s, ips: %s network %s", pod.Namespace, pod.Name,
		util.JoinIPNetIPs(pInfo.ips, " "), bsnc.GetNetworkName())
	return bsnc.releasePodIPs(pInfo)
}

func (bsnc *BaseSecondaryNetworkController) syncPodsForSecondaryNetwork(pods []interface{}) error {
	// get the list of logical switch ports (equivalent to pods). Reserve all existing Pod IPs to
	// avoid subsequent new Pods getting the same duplicate Pod IP.
	expectedLogicalPorts := make(map[string]bool)
	for _, podInterface := range pods {
		pod, ok := podInterface.(*kapi.Pod)
		if !ok {
			return fmt.Errorf("spurious object in syncPods: %v", podInterface)
		}
		on, network, err := util.PodWantsMultiNetwork(pod, bsnc.NetInfo)
		if err != nil || !on {
			if err != nil {
				klog.Warningf("Failed to determine if pod %s/%s needs to be plumb interface on network %s: %v",
					pod.Namespace, pod.Name, bsnc.GetNetworkName(), err)
			}
			continue
		}
		nadName := util.GetNADName(network.Namespace, network.Name)
		annotations, err := util.UnmarshalPodAnnotation(pod.Annotations, nadName)
		if err != nil {
			if !util.IsAnnotationNotSetError(err) {
				klog.Errorf("Failed to get pod annotation of pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadName)
			}
			continue
		}
		expectedLogicalPortName, err := bsnc.allocatePodIPs(pod, annotations, nadName)
		if err != nil {
			return err
		}
		if expectedLogicalPortName != "" {
			expectedLogicalPorts[expectedLogicalPortName] = true
		}
	}
	return bsnc.deleteStaleLogicalSwitchPorts(expectedLogicalPorts)
}
