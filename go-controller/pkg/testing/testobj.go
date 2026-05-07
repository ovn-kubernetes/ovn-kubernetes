// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testing

import (
	"crypto/sha256"
	"fmt"
	"net"
	"strings"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func NewPodMeta(namespace, name string, additionalLabels map[string]string) metav1.ObjectMeta {
	labels := map[string]string{
		"name": name,
	}
	for k, v := range additionalLabels {
		labels[k] = v
	}
	return metav1.ObjectMeta{
		Name:      name,
		UID:       apimachinerytypes.UID(name),
		Namespace: namespace,
		Labels:    labels,
	}
}

func NewPodWithLabelsAllIPFamilies(namespace, name, node string, podIPs []string, additionalLabels map[string]string) *corev1.Pod {
	podIPList := []corev1.PodIP{}
	for _, podIP := range podIPs {
		podIPList = append(podIPList, corev1.PodIP{IP: podIP})
	}
	return &corev1.Pod{
		ObjectMeta: NewPodMeta(namespace, name, additionalLabels),
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "containerName",
					Image: "containerImage",
				},
			},
			NodeName: node,
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIP:  podIPList[0].IP,
			PodIPs: podIPList,
		},
	}
}

func NewPodWithLabels(namespace, name, node, podIP string, additionalLabels map[string]string) *corev1.Pod {
	podIPs := []corev1.PodIP{}
	if podIP != "" {
		podIPs = append(podIPs, corev1.PodIP{IP: podIP})
	}
	return &corev1.Pod{
		ObjectMeta: NewPodMeta(namespace, name, additionalLabels),
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "containerName",
					Image: "containerImage",
				},
			},
			NodeName: node,
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIP:  podIP,
			PodIPs: podIPs,
		},
	}
}

func NewPod(namespace, name, node, podIP string) *corev1.Pod {
	podIPs := []corev1.PodIP{}
	ips := strings.Split(podIP, " ")
	if len(ips) > 0 {
		podIP = ips[0]
		for _, ip := range ips {
			podIPs = append(podIPs, corev1.PodIP{IP: ip})
		}
	}
	return &corev1.Pod{
		ObjectMeta: NewPodMeta(namespace, name, nil),
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "containerName",
					Image: "containerImage",
				},
			},
			NodeName: node,
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIP:  podIP,
			PodIPs: podIPs,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}

// ipAddrToHWAddr takes the four octets of IPv4 address (aa.bb.cc.dd, for example) and uses them in creating
// a MAC address (0A:58:AA:BB:CC:DD).  For IPv6, create a hash from the IPv6 string and use that for MAC Address.
// Assumption: the caller will ensure that an empty net.IP{} will NOT be passed.
func ipAddrToHWAddr(ip net.IP) net.HardwareAddr {
	// Ensure that for IPv4, we are always working with the IP in 4-byte form.
	ip4 := ip.To4()
	if ip4 != nil {
		// safe to use private MAC prefix: 0A:58
		return net.HardwareAddr{0x0A, 0x58, ip4[0], ip4[1], ip4[2], ip4[3]}
	}

	hash := sha256.Sum256([]byte(ip.String()))
	return net.HardwareAddr{0x0A, 0x58, hash[0], hash[1], hash[2], hash[3]}
}

func getPodAnnotationDefault(ip string, hasPrimaryUDN bool) string {
	role := "primary"
	if hasPrimaryUDN {
		role = "infrastructure-locked"
	}
	netip := net.ParseIP(ip)
	mac := ipAddrToHWAddr(netip)
	return fmt.Sprintf(`"default":{"mac_address":"%s","ip_address":"%s/24","role":"%s"}`, mac.String(), ip, role)
}

func getPodAnnotationPrimary(namespace, nadName, ip string) string {
	netip := net.ParseIP(ip)
	mac := ipAddrToHWAddr(netip)
	return fmt.Sprintf(`"%s/%s":{"mac_address":"%s","ip_address":"%s/24","role":"primary"}`,
		namespace, nadName, mac, ip)
}

func getPodAnnotationSecondary(namespace, nadName, ip string) string {
	netip := net.ParseIP(ip)
	mac := ipAddrToHWAddr(netip)
	return fmt.Sprintf(`"%s/%s":{"mac_address":"%s","ip_address":"%s/24","role":"secondary"}`,
		namespace, nadName, mac, ip)
}

func NewPodWithPrimaryNADIP(namespace, name, node, defaultNetworkIP, nadName, nadIP string) *corev1.Pod {
	pod := NewPod(namespace, name, node, defaultNetworkIP)
	pod.Annotations = map[string]string{types.OvnPodAnnotationName: fmt.Sprintf(`{%s,%s}`, getPodAnnotationDefault(defaultNetworkIP, true), getPodAnnotationPrimary(namespace, nadName, nadIP))}
	return pod
}

func NewPodWithSecondaryNADIP(namespace, name, node, defaultNetworkIP, nadAndNetworkName, nadIP string) *corev1.Pod {
	pod := NewPod(namespace, name, node, defaultNetworkIP)
	pod.Annotations = map[string]string{
		types.OvnPodAnnotationName:   fmt.Sprintf(`{%s,%s}`, getPodAnnotationDefault(defaultNetworkIP, false), getPodAnnotationSecondary(namespace, nadAndNetworkName, nadIP)),
		nadv1.NetworkAttachmentAnnot: nadAndNetworkName,
	}
	return pod
}

func NewNamespaceMeta(namespace string, additionalLabels map[string]string) metav1.ObjectMeta {
	labels := map[string]string{
		"name": namespace,
	}
	for k, v := range additionalLabels {
		labels[k] = v
	}
	return metav1.ObjectMeta{
		UID:         apimachinerytypes.UID(namespace),
		Name:        namespace,
		Labels:      labels,
		Annotations: map[string]string{},
	}
}

func NewNamespaceWithLabels(namespace string, additionalLabels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: NewNamespaceMeta(namespace, additionalLabels),
		Spec:       corev1.NamespaceSpec{},
		Status:     corev1.NamespaceStatus{},
	}
}

func NewNamespace(namespace string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: NewNamespaceMeta(namespace, nil),
		Spec:       corev1.NamespaceSpec{},
		Status:     corev1.NamespaceStatus{},
	}
}

func NewTestNetworkPolicy(name, namespace string, podSelector metav1.LabelSelector, ingress []networkingv1.NetworkPolicyIngressRule,
	egress []networkingv1.NetworkPolicyEgressRule, policyTypes ...networkingv1.PolicyType) *networkingv1.NetworkPolicy {
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			UID:       apimachinerytypes.UID(namespace),
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"name": name,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: podSelector,
			PolicyTypes: policyTypes,
			Ingress:     ingress,
			Egress:      egress,
		},
	}
	if policyTypes == nil {
		if len(ingress) > 0 {
			policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, networkingv1.PolicyTypeIngress)
		}
		if len(egress) > 0 {
			policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
		}
	}
	return policy
}

func NewMatchLabelsNetworkPolicy(policyName, netpolNamespace, peerNamespace, peerPodName string, ingress, egress bool) *networkingv1.NetworkPolicy {
	netPolPeer := networkingv1.NetworkPolicyPeer{}
	if peerPodName != "" {
		netPolPeer.PodSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name": peerPodName,
			},
		}
	}
	if peerNamespace != "" {
		netPolPeer.NamespaceSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name": peerNamespace,
			},
		}
	}
	var ingressRules []networkingv1.NetworkPolicyIngressRule
	if ingress {
		ingressRules = []networkingv1.NetworkPolicyIngressRule{
			{
				From: []networkingv1.NetworkPolicyPeer{netPolPeer},
			},
		}
	}
	var egressRules []networkingv1.NetworkPolicyEgressRule
	if egress {
		egressRules = []networkingv1.NetworkPolicyEgressRule{
			{
				To: []networkingv1.NetworkPolicyPeer{netPolPeer},
			},
		}
	}
	return NewTestNetworkPolicy(policyName, netpolNamespace, metav1.LabelSelector{}, ingressRules, egressRules)
}
