package multihoming

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
)

func AllowedClient(podName string) string {
	return "allowed-" + podName
}

func BlockedClient(podName string) string {
	return "blocked-" + podName
}

func MultiNetPolicyPort(port int) mnpapi.MultiNetworkPolicyPort {
	tcp := v1.ProtocolTCP
	p := intstr.FromInt32(int32(port))
	return mnpapi.MultiNetworkPolicyPort{
		Protocol: &tcp,
		Port:     &p,
	}
}

func MultiNetPolicyPortRange(port, endPort int) mnpapi.MultiNetworkPolicyPort {
	netpolPort := MultiNetPolicyPort(port)
	endPort32 := int32(endPort)
	netpolPort.EndPort = &endPort32
	return netpolPort
}

func MultiNetIngressLimitingPolicy(policyFor string, appliesFor metav1.LabelSelector, allowForSelector metav1.LabelSelector, allowPorts ...mnpapi.MultiNetworkPolicyPort) *mnpapi.MultiNetworkPolicy {
	var (
		portAllowlist []mnpapi.MultiNetworkPolicyPort
	)
	portAllowlist = append(portAllowlist, allowPorts...)
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			PolicyForAnnotation: policyFor,
		},
			Name: "allow-ports-via-pod-selector",
		},

		Spec: mnpapi.MultiNetworkPolicySpec{
			PodSelector: appliesFor,
			Ingress: []mnpapi.MultiNetworkPolicyIngressRule{
				{
					Ports: portAllowlist,
					From: []mnpapi.MultiNetworkPolicyPeer{
						{
							PodSelector: &allowForSelector,
						},
					},
				},
			},
			PolicyTypes: []mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
		},
	}
}

func MultiNetIngressLimitingIPBlockPolicy(
	policyFor string,
	appliesFor metav1.LabelSelector,
	allowForIPBlock mnpapi.IPBlock,
	allowPorts ...int,
) *mnpapi.MultiNetworkPolicy {
	var (
		portAllowlist []mnpapi.MultiNetworkPolicyPort
	)
	tcp := v1.ProtocolTCP
	for _, port := range allowPorts {
		p := intstr.FromInt(port)
		portAllowlist = append(portAllowlist, mnpapi.MultiNetworkPolicyPort{
			Protocol: &tcp,
			Port:     &p,
		})
	}
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			PolicyForAnnotation: policyFor,
		},
			Name: "allow-ports-ip-block",
		},

		Spec: mnpapi.MultiNetworkPolicySpec{
			PodSelector: appliesFor,
			Ingress: []mnpapi.MultiNetworkPolicyIngressRule{
				{
					Ports: portAllowlist,
					From: []mnpapi.MultiNetworkPolicyPeer{
						{
							IPBlock: &allowForIPBlock,
						},
					},
				},
			},
			PolicyTypes: []mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
		},
	}
}

func MultiNetEgressLimitingIPBlockPolicy(
	policyName string,
	policyFor string,
	appliesFor metav1.LabelSelector,
	allowForIPBlock mnpapi.IPBlock,
) *mnpapi.MultiNetworkPolicy {
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
			Annotations: map[string]string{
				PolicyForAnnotation: policyFor,
			},
		},

		Spec: mnpapi.MultiNetworkPolicySpec{
			PodSelector: appliesFor,
			Egress: []mnpapi.MultiNetworkPolicyEgressRule{
				{
					To: []mnpapi.MultiNetworkPolicyPeer{
						{
							IPBlock: &allowForIPBlock,
						},
					},
				},
			},
			PolicyTypes: []mnpapi.MultiPolicyType{mnpapi.PolicyTypeEgress},
		},
	}
}

func MultiNetPolicy(
	policyName string,
	policyFor string,
	appliesFor metav1.LabelSelector,
	policyTypes []mnpapi.MultiPolicyType,
	ingress []mnpapi.MultiNetworkPolicyIngressRule,
	egress []mnpapi.MultiNetworkPolicyEgressRule,
) *mnpapi.MultiNetworkPolicy {
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
			Annotations: map[string]string{
				PolicyForAnnotation: policyFor,
			},
		},
		Spec: mnpapi.MultiNetworkPolicySpec{
			PodSelector: appliesFor,
			PolicyTypes: policyTypes,
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}

func DoesPolicyFeatAnIPBlock(policy *mnpapi.MultiNetworkPolicy) bool {
	for _, rule := range policy.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.IPBlock != nil {
				return true
			}
		}
	}
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				return true
			}
		}
	}
	return false
}

func SetBlockedClientIPInPolicyIPBlockExcludedRanges(policy *mnpapi.MultiNetworkPolicy, blockedIP string) {
	if policy.Spec.Ingress != nil {
		for _, rule := range policy.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.IPBlock != nil {
					peer.IPBlock.Except = []string{blockedIP}
				}
			}
		}
	}
	if policy.Spec.Egress != nil {
		for _, rule := range policy.Spec.Egress {
			for _, peer := range rule.To {
				if peer.IPBlock != nil {
					peer.IPBlock.Except = []string{blockedIP}
				}
			}
		}
	}
}

func MultiNetIngressLimitingPolicyAllowFromNamespace(
	policyFor string, appliesFor metav1.LabelSelector, allowForSelector metav1.LabelSelector, allowPorts ...int,
) *mnpapi.MultiNetworkPolicy {
	return &mnpapi.MultiNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			PolicyForAnnotation: policyFor,
		},
			Name: "allow-ports-same-ns",
		},

		Spec: mnpapi.MultiNetworkPolicySpec{
			PodSelector: appliesFor,
			Ingress: []mnpapi.MultiNetworkPolicyIngressRule{
				{
					Ports: allowedTCPPortsForPolicy(allowPorts...),
					From: []mnpapi.MultiNetworkPolicyPeer{
						{
							NamespaceSelector: &allowForSelector,
						},
					},
				},
			},
			PolicyTypes: []mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
		},
	}
}

func allowedTCPPortsForPolicy(allowPorts ...int) []mnpapi.MultiNetworkPolicyPort {
	var (
		portAllowlist []mnpapi.MultiNetworkPolicyPort
	)
	tcp := v1.ProtocolTCP
	for _, port := range allowPorts {
		p := intstr.FromInt(port)
		portAllowlist = append(portAllowlist, mnpapi.MultiNetworkPolicyPort{
			Protocol: &tcp,
			Port:     &p,
		})
	}
	return portAllowlist
}
