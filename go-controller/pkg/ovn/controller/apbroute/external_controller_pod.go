package apbroute

import (
	"encoding/json"
	"fmt"
	"net"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	v1pod "k8s.io/kubernetes/pkg/api/v1/pod"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func (m *externalPolicyManager) syncPod(pod *corev1.Pod, routeQueue workqueue.TypedRateLimitingInterface[string]) error {
	policyKeys, err := m.getPoliciesForPodChange(pod)
	if err != nil {
		return err
	}
	klog.V(5).Infof("APB queuing policies: %v for pod: %s/%s", policyKeys, pod.Namespace, pod.Name)
	for policyName := range policyKeys {
		routeQueue.Add(policyName)
	}

	return nil
}

func getExGwPodIPs(gatewayPod *corev1.Pod, networkName string) (sets.Set[string], error) {
	// If an external gateway pod is in terminating or not ready state then don't return the
	// IPs for the external gateway pod
	if util.PodTerminating(gatewayPod) || !v1pod.IsPodReadyConditionTrue(gatewayPod.Status) {
		klog.Warningf("External gateway pod cannot serve traffic; it's in terminating or not ready state: %s/%s", gatewayPod.Namespace, gatewayPod.Name)
		return nil, nil
	}

	if networkName != "" {
		return getMultusIPsFromNetworkName(gatewayPod, networkName)
	}
	if gatewayPod.Spec.HostNetwork {
		return getPodIPs(gatewayPod), nil
	}
	return nil, fmt.Errorf("ignoring pod %s as an external gateway candidate. Invalid combination "+
		"of host network: %t and routing-network annotation: %s", gatewayPod.Name, gatewayPod.Spec.HostNetwork,
		networkName)
}

func getPodIPs(pod *corev1.Pod) sets.Set[string] {
	foundGws := sets.New[string]()
	for _, podIP := range pod.Status.PodIPs {
		ip := utilnet.ParseIPSloppy(podIP.IP)
		if ip != nil {
			foundGws.Insert(ip.String())
		}
	}
	return foundGws
}

func getMultusIPsFromNetworkName(pod *corev1.Pod, networkName string) (sets.Set[string], error) {
	foundGws := sets.New[string]()
	var multusNetworks []nettypes.NetworkStatus
	err := json.Unmarshal([]byte(pod.ObjectMeta.Annotations[nettypes.NetworkStatusAnnot]), &multusNetworks)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshall annotation on pod %s %s '%s': %v",
			pod.Name, nettypes.NetworkStatusAnnot, pod.ObjectMeta.Annotations[nettypes.NetworkStatusAnnot], err)
	}
	for _, multusNetwork := range multusNetworks {
		if multusNetwork.Name == networkName {
			for _, gwIP := range multusNetwork.IPs {
				ip := net.ParseIP(gwIP)
				if ip != nil {
					foundGws.Insert(ip.String())
				}
			}
			return foundGws, nil
		}
	}
	return nil, fmt.Errorf("unable to find multus network %s in pod %s/%s", networkName, pod.Namespace, pod.Name)
}
