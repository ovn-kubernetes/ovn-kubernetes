// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	ovnkutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func joinMetaNamespaceAndName(namespace, name string, separator ...string) string {
	if namespace == "" {
		return name
	}
	sep := "/"
	if len(separator) > 0 {
		sep = separator[0]
	}
	return namespace + sep + name
}

func GetNetworkQoSAddrSetDbIDs(nqosNamespace, nqosName, ruleIndex, ipBlockIndex, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNetworkQoS, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: joinMetaNamespaceAndName(nqosNamespace, nqosName, ":"),
			// rule index is the unique id for address set within given objectName
			libovsdbops.RuleIndex:       ruleIndex,
			libovsdbops.IpBlockIndexKey: ipBlockIndex,
		})
}

// GetNetworkQoSPortGroupDbIDs returns the DbObjectIDs of the per-NetworkQoS
// source port group. On ipamless localnet networks the source pods have no
// OVN-managed IP, so they are matched via inport == @pg instead of a src-IP
// address set. There is a single source port group per NetworkQoS object,
// keyed by namespace:name (mirroring GetNetworkQoSAddrSetDbIDs' ObjectNameKey).
func GetNetworkQoSPortGroupDbIDs(nqosNamespace, nqosName, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetworkQoS, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: joinMetaNamespaceAndName(nqosNamespace, nqosName, ":"),
		})
}

// isIPAMlessLocalnet reports whether this controller manages an ipamless
// (no OVN-managed pod IPs) secondary localnet network. On such networks source
// pods carry MAC-only logical switch ports, so NetworkQoS must match source pods
// via a port group (inport == @pg) rather than a src-IP address set.
func (c *Controller) isIPAMlessLocalnet() bool {
	return c.TopologyType() == types.LocalnetTopology && !ovnkutil.DoesNetworkRequireIPAM(c.NetInfo)
}

func getPodAddresses(pod *corev1.Pod, networkInfo ovnkutil.NetInfo, resolver func(nadKey string) string) ([]string, error) {
	// check annotation "k8s.ovn.org/pod-networks" before calling GetPodIPsOfNetwork,
	// as it's no easy to check if the error is caused by missing annotation, while
	// we don't want to return error for such case as it will trigger retry
	_, ok := pod.Annotations[types.OvnPodAnnotationName]
	if !ok {
		// pod hasn't been annotated yet, return nil to avoid retry
		return nil, nil
	}
	ips, err := ovnkutil.GetPodIPsOfNetwork(pod, networkInfo, resolver)
	if err != nil {
		return nil, err
	}
	addresses := []string{}
	for _, ip := range ips {
		addresses = append(addresses, ip.String())
	}
	return addresses, nil
}

func generateNetworkQoSMatch(qosState *networkQoSState, rule *GressRule, ipv4Enabled, ipv6Enabled, ipamless bool) string {
	var match string
	if ipamless {
		// Ipamless localnet source pods have no OVN-managed IP, so scope to the
		// selected source pods by their logical switch port membership in the
		// per-NetworkQoS source port group. IPMode() is (false,false) on ipamless
		// networks, so an explicit (ip4 || ip6) qualifier is required to avoid
		// matching ARP/ND/DHCP and breaking address acquisition.
		match = fmt.Sprintf("inport == @%s && (ip4 || ip6)", qosState.SrcPortGroupName)
	} else {
		match = addressSetToMatchString(qosState.SrcAddrSet, trafficDirSource, ipv4Enabled, ipv6Enabled)
	}

	classiferMatchString := rule.Classifier.ToQosMatchString(ipv4Enabled, ipv6Enabled)
	if classiferMatchString != "" {
		match = match + " && " + classiferMatchString
	}

	return match
}

func addressSetToMatchString(addrset addressset.AddressSet, dir trafficDirection, ipv4Enabled, ipv6Enabled bool) string {
	ipv4AddrSetHashName, ipv6AddrSetHashName := addrset.GetASHashNames()
	output := ""
	switch {
	case ipv4Enabled && ipv6Enabled:
		output = fmt.Sprintf("(ip4.%s == {$%s} || ip6.%s == {$%s})", dir, ipv4AddrSetHashName, dir, ipv6AddrSetHashName)
	case ipv4Enabled:
		output = fmt.Sprintf("ip4.%s == {$%s}", dir, ipv4AddrSetHashName)
	case ipv6Enabled:
		output = fmt.Sprintf("ip6.%s == {$%s}", dir, ipv6AddrSetHashName)
	}
	return output
}
