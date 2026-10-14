// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	OVNNodeVTEPs = "k8s.ovn.org/vteps"
)

// VTEPNodeAnnotation holds the VTEP IPs discovered or allocated on a node.
// The annotation on the node is a JSON map keyed by VTEP name:
//
//	{"vtep-name1": {"ips": ["192.168.1.1", "fd00::1"]}, "vtep-name2": {"ips": ["10.0.0.1"]}}
type VTEPNodeAnnotation struct {
	IPs []string `json:"ips"`
}

// ParseNodeVTEPs parses the k8s.ovn.org/vteps annotation from a node and
// returns a map of VTEP name to its annotation data. Returns an
// AnnotationNotSetError if the annotation is not present.
func ParseNodeVTEPs(node *corev1.Node) (map[string]VTEPNodeAnnotation, error) {
	raw, ok := node.Annotations[OVNNodeVTEPs]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", OVNNodeVTEPs, node.Name)
	}
	var vteps map[string]VTEPNodeAnnotation
	if err := json.Unmarshal([]byte(raw), &vteps); err != nil {
		return nil, fmt.Errorf("failed to parse %s annotation on node %q: %w", OVNNodeVTEPs, node.Name, err)
	}
	return vteps, nil
}

// GetNodeVTEPIPOwners indexes node addresses and VTEP claims by canonical IP.
// Invalid annotations do not prevent indexing addresses from other nodes.
func GetNodeVTEPIPOwners(nodes []*corev1.Node) map[string]sets.Set[string] {
	owners := map[string]sets.Set[string]{}
	add := func(ip, nodeName string) {
		addr, err := netip.ParseAddr(ip)
		if err != nil || addr.Zone() != "" {
			return
		}
		ip = addr.Unmap().String()
		if owners[ip] == nil {
			owners[ip] = sets.New[string]()
		}
		owners[ip].Insert(nodeName)
	}
	for _, node := range nodes {
		for _, address := range node.Status.Addresses {
			if address.Type == corev1.NodeInternalIP || address.Type == corev1.NodeExternalIP {
				add(address.Address, node.Name)
			}
		}
		vteps, err := ParseNodeVTEPs(node)
		if err != nil {
			continue
		}
		for _, entry := range vteps {
			for _, ip := range entry.IPs {
				add(ip, node.Name)
			}
		}
	}
	return owners
}

// ValidateNodeVTEPs parses the annotation, rejects invalid or conflicting IPs,
// and returns canonical addresses suitable for constructing host prefixes.
func ValidateNodeVTEPs(node *corev1.Node, ipOwners map[string]sets.Set[string]) (map[string]VTEPNodeAnnotation, error) {
	return ValidateSelectedNodeVTEPs(node, ipOwners, nil)
}

// ValidateSelectedNodeVTEPs validates and returns only the named VTEP entries.
// A nil selection validates all entries; an empty selection validates none.
func ValidateSelectedNodeVTEPs(node *corev1.Node, ipOwners map[string]sets.Set[string], selected sets.Set[string]) (map[string]VTEPNodeAnnotation, error) {
	vteps, err := ParseNodeVTEPs(node)
	if err != nil {
		return nil, err
	}
	if selected != nil {
		maps.DeleteFunc(vteps, func(name string, _ VTEPNodeAnnotation) bool {
			return !selected.Has(name)
		})
	}
	for _, name := range slices.Sorted(maps.Keys(vteps)) {
		entry := vteps[name]
		families := sets.New[bool]()
		for i, ip := range entry.IPs {
			addr, parseErr := netip.ParseAddr(ip)
			if parseErr != nil || addr.Zone() != "" || !addr.Unmap().IsGlobalUnicast() {
				return nil, fmt.Errorf("node %q VTEP %q has invalid IP %q", node.Name, name, ip)
			}
			addr = addr.Unmap()
			if families.Has(addr.Is6()) {
				return nil, fmt.Errorf("node %q VTEP %q has multiple IPs of the same family", node.Name, name)
			}
			families.Insert(addr.Is6())
			ip = addr.String()
			for _, owner := range sets.List(ipOwners[ip]) {
				if owner != node.Name {
					return nil, fmt.Errorf("node %q VTEP %q IP %q is already claimed by node %q", node.Name, name, ip, owner)
				}
			}
			entry.IPs[i] = ip
		}
		vteps[name] = entry
	}
	return vteps, nil
}

// MarshalNodeVTEPs serializes the VTEPs annotation for use with SetAnnotationsOnNode.
// If the map is empty, the annotation value is set to nil so that a strategic
// merge patch removes the key from the node.
func MarshalNodeVTEPs(vteps map[string]VTEPNodeAnnotation) (map[string]interface{}, error) {
	if len(vteps) == 0 {
		return map[string]interface{}{OVNNodeVTEPs: nil}, nil
	}
	data, err := json.Marshal(vteps)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		OVNNodeVTEPs: string(data),
	}, nil
}

// NodeVTEPsAnnotationChanged returns true if the k8s.ovn.org/vteps annotation
// differs between the old and new node objects. Both oldNode and newNode must
// be non-nil;
func NodeVTEPsAnnotationChanged(oldNode, newNode *corev1.Node) bool {
	return oldNode.Annotations[OVNNodeVTEPs] != newNode.Annotations[OVNNodeVTEPs]
}
