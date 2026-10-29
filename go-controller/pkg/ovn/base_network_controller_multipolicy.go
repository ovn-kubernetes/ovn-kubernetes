// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"net"
	"strings"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"

	knet "k8s.io/api/networking/v1"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const PolicyForAnnotation = "k8s.v1.cni.cncf.io/policy-for"

func (bsnc *BaseUserDefinedNetworkController) syncMultiNetworkPolicies(_ context.Context, multiPolicies []interface{}) error {
	expectedPolicies := make(map[string]map[string]bool)
	for _, npInterface := range multiPolicies {
		policy, ok := npInterface.(*mnpapi.MultiNetworkPolicy)
		if !ok {
			return fmt.Errorf("spurious object in syncMultiNetworkPolicies: %v", npInterface)
		}
		if !bsnc.shouldApplyMultiPolicy(policy) {
			klog.V(5).Infof("[network (%s)] skipping syncing policy %s/%s",
				bsnc.GetNetworkName(), policy.Namespace, policy.Name)
			continue
		}
		if nsMap, ok := expectedPolicies[policy.Namespace]; ok {
			nsMap[policy.Name] = true
		} else {
			expectedPolicies[policy.Namespace] = map[string]bool{
				policy.Name: true,
			}
		}
	}

	return bsnc.syncNetworkPoliciesCommon(expectedPolicies)
}

func (bsnc *BaseUserDefinedNetworkController) shouldApplyMultiPolicy(mpolicy *mnpapi.MultiNetworkPolicy) bool {
	policyForAnnot, ok := mpolicy.Annotations[PolicyForAnnotation]
	if !ok {
		klog.V(5).Infof("%s annotation not defined in multi-policy %s/%s", PolicyForAnnotation,
			mpolicy.Namespace, mpolicy.Name)
		return false
	}
	policyForAnnot = strings.ReplaceAll(policyForAnnot, " ", "")
	policyForNetworks := strings.Split(policyForAnnot, ",")
	for _, networkName := range policyForNetworks {
		networkNamespace := mpolicy.Namespace
		substrings := strings.Split(networkName, "/")
		if len(substrings) > 1 {
			networkName = substrings[1]
			networkNamespace = substrings[0]
		}
		nadKey := util.GetNADName(networkNamespace, networkName)
		if bsnc.networkManager.GetNetworkNameForNADKey(nadKey) == bsnc.GetNetworkName() {
			return true
		}
	}
	return false
}

// convertMultiNetPolicyIPBlock validates the ipBlock the same way the API server
// validates NetworkPolicy ipBlocks. The MultiNetworkPolicy CRD does not validate
// these fields and they are interpolated verbatim into ACL matches.
func convertMultiNetPolicyIPBlock(ipb *mnpapi.IPBlock) (*knet.IPBlock, error) {
	_, cidrIPNet, err := net.ParseCIDR(ipb.CIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid ipBlock cidr %q: %w", ipb.CIDR, err)
	}
	for _, exceptCIDRStr := range ipb.Except {
		_, exceptCIDR, err := net.ParseCIDR(exceptCIDRStr)
		if err != nil {
			return nil, fmt.Errorf("invalid ipBlock except %q: %w", exceptCIDRStr, err)
		}
		cidrMaskLen, _ := cidrIPNet.Mask.Size()
		exceptMaskLen, _ := exceptCIDR.Mask.Size()
		if !cidrIPNet.Contains(exceptCIDR.IP) || cidrMaskLen >= exceptMaskLen {
			return nil, fmt.Errorf("ipBlock except %q must be a strict subset of cidr %q", exceptCIDRStr, ipb.CIDR)
		}
	}
	return &knet.IPBlock{CIDR: ipb.CIDR, Except: ipb.Except}, nil
}

func convertMultiNetPolicyToNetPolicy(mpolicy *mnpapi.MultiNetworkPolicy, allowPeerSelectors bool) (*knet.NetworkPolicy, error) {
	var policy knet.NetworkPolicy
	var ipb *knet.IPBlock
	var err error

	policy.Name = mpolicy.Name
	policy.Namespace = mpolicy.Namespace
	policy.Spec.PodSelector = mpolicy.Spec.PodSelector
	policy.Annotations = mpolicy.Annotations
	policy.Spec.Ingress = make([]knet.NetworkPolicyIngressRule, 0, len(mpolicy.Spec.Ingress))
	for i, mingress := range mpolicy.Spec.Ingress {
		var ingress knet.NetworkPolicyIngressRule
		ingress.Ports = make([]knet.NetworkPolicyPort, len(mingress.Ports))
		for j, mport := range mingress.Ports {
			ingress.Ports[j] = knet.NetworkPolicyPort{
				Protocol: mport.Protocol,
				Port:     mport.Port,
				EndPort:  mport.EndPort,
			}
		}
		ingress.From = make([]knet.NetworkPolicyPeer, 0, len(mingress.From))
		for _, mfrom := range mingress.From {
			if !allowPeerSelectors && isPeerSelector(mfrom) {
				return nil, fmt.Errorf("invalid ingress peer %v in multi-network policy %s; IPAM-less networks can only have `ipBlock` peers", mfrom, mpolicy.Name)
			}
			ipb = nil
			if mfrom.IPBlock != nil {
				ipb, err = convertMultiNetPolicyIPBlock(mfrom.IPBlock)
				if err != nil {
					klog.Warningf("Ignoring ingress peer in multi-network policy %s/%s: %v", mpolicy.Namespace, mpolicy.Name, err)
					continue
				}
			}
			ingress.From = append(ingress.From, knet.NetworkPolicyPeer{
				PodSelector:       mfrom.PodSelector,
				NamespaceSelector: mfrom.NamespaceSelector,
				IPBlock:           ipb,
			})
		}
		// an empty peer list allows all sources, so a rule whose peers were all ignored must be dropped
		if len(mingress.From) > 0 && len(ingress.From) == 0 {
			klog.Warningf("Ignoring ingress rule %d in multi-network policy %s/%s: all peers are invalid", i, mpolicy.Namespace, mpolicy.Name)
			continue
		}
		policy.Spec.Ingress = append(policy.Spec.Ingress, ingress)
	}
	policy.Spec.Egress = make([]knet.NetworkPolicyEgressRule, 0, len(mpolicy.Spec.Egress))
	for i, megress := range mpolicy.Spec.Egress {
		var egress knet.NetworkPolicyEgressRule
		egress.Ports = make([]knet.NetworkPolicyPort, len(megress.Ports))
		for j, mport := range megress.Ports {
			egress.Ports[j] = knet.NetworkPolicyPort{
				Protocol: mport.Protocol,
				Port:     mport.Port,
				EndPort:  mport.EndPort,
			}
		}
		egress.To = make([]knet.NetworkPolicyPeer, 0, len(megress.To))
		for _, mto := range megress.To {
			if !allowPeerSelectors && isPeerSelector(mto) {
				return nil, fmt.Errorf("invalid egress peer %v in multi-network policy %s; IPAM-less networks can only have `ipBlock` peers", mto, mpolicy.Name)
			}
			ipb = nil
			if mto.IPBlock != nil {
				ipb, err = convertMultiNetPolicyIPBlock(mto.IPBlock)
				if err != nil {
					klog.Warningf("Ignoring egress peer in multi-network policy %s/%s: %v", mpolicy.Namespace, mpolicy.Name, err)
					continue
				}
			}
			egress.To = append(egress.To, knet.NetworkPolicyPeer{
				PodSelector:       mto.PodSelector,
				NamespaceSelector: mto.NamespaceSelector,
				IPBlock:           ipb,
			})
		}
		if len(megress.To) > 0 && len(egress.To) == 0 {
			klog.Warningf("Ignoring egress rule %d in multi-network policy %s/%s: all peers are invalid", i, mpolicy.Namespace, mpolicy.Name)
			continue
		}
		policy.Spec.Egress = append(policy.Spec.Egress, egress)
	}
	policy.Spec.PolicyTypes = make([]knet.PolicyType, len(mpolicy.Spec.PolicyTypes))
	for i, mpolicytype := range mpolicy.Spec.PolicyTypes {
		policy.Spec.PolicyTypes[i] = knet.PolicyType(mpolicytype)
	}
	return &policy, nil
}

func (bnc *BaseNetworkController) convertMultiNetPolicyToNetPolicy(mpolicy *mnpapi.MultiNetworkPolicy) (*knet.NetworkPolicy, error) {
	// selector peers only need discoverable pod IPs (own IPAM or DHCP-learned)
	allowPeerSelectors := bnc.doesNetworkHaveDiscoverablePodIPs()
	return convertMultiNetPolicyToNetPolicy(mpolicy, allowPeerSelectors)
}

func isPeerSelector(peer mnpapi.MultiNetworkPolicyPeer) bool {
	return peer.PodSelector != nil || peer.NamespaceSelector != nil
}
