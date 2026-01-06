package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// This handles the annotations related to subnets assigned to a node. The annotations are
// created by the master, and then read by the node. In a single-stack cluster, they look
// like:
//
//   annotations:
//     k8s.ovn.org/node-subnets: |
//       {
//         "default": "10.130.0.0/23"
//       }
//
// (This allows for specifying multiple network attachments, but currently only "default"
// is used.)
//
// In a dual-stack cluster, the values are lists:
//
//   annotations:
//     k8s.ovn.org/node-subnets: |
//       {
//         "default": ["10.130.0.0/23", "fd01:0:0:2::/64"]
//       }

const (
	// ovnNodeSubnets is the constant string representing the node subnets annotation key
	ovnNodeSubnets = "k8s.ovn.org/node-subnets"
)

// updateSubnetAnnotation add the hostSubnets of the given network to the input node annotations;
// input annotations is not nil
// if hostSubnets is empty, deletes the existing subnet annotation for given network from the input node annotations.
func updateSubnetAnnotation(annotations map[string]string, annotationName, netName string, hostSubnets []*net.IPNet) error {
	var bytes []byte

	// First get the all host subnets for all existing networks
	subnetsMap, err := parseSubnetAnnotation(annotations, annotationName)
	if err != nil {
		if !IsAnnotationNotSetError(err) {
			return fmt.Errorf("failed to parse node subnet annotation %q: %v",
				annotations, err)
		}
		// in the case that the annotation does not exist
		subnetsMap = map[string][]*net.IPNet{}
	}

	// add or delete host subnet of the specified network
	if len(hostSubnets) != 0 {
		subnetsMap[netName] = hostSubnets
	} else {
		delete(subnetsMap, netName)
	}

	// if no host subnet left, just delete the host subnet annotation from node annotations.
	if len(subnetsMap) == 0 {
		delete(annotations, annotationName)
		return nil
	}

	// Marshal all host subnets of all networks back to annotations.
	subnetsStrMap := make(map[string][]string)
	for n, subnets := range subnetsMap {
		subnetsStr := make([]string, len(subnets))
		for i, subnet := range subnets {
			subnetsStr[i] = subnet.String()
		}
		subnetsStrMap[n] = subnetsStr
	}
	bytes, err = json.Marshal(subnetsStrMap)
	if err != nil {
		return err
	}
	annotations[annotationName] = string(bytes)
	return nil
}

func setSubnetAnnotation(nodeAnnotator kube.Annotator, annotationName string, defaultSubnets []*net.IPNet) error {
	annotation := map[string]string{}
	err := updateSubnetAnnotation(annotation, annotationName, types.DefaultNetworkName, defaultSubnets)
	if err != nil {
		return err
	}
	return nodeAnnotator.Set(annotationName, annotation[annotationName])
}

func parseSubnetAnnotation(nodeAnnotations map[string]string, annotationName string) (map[string][]*net.IPNet, error) {
	annotation, ok := nodeAnnotations[annotationName]
	if !ok {
		return nil, newAnnotationNotSetError("could not find %q annotation", annotationName)
	}
	return parseSubnetAnnotationValue(annotationName, annotation)
}

func parseSubnetAnnotationWithCache(node *corev1.Node, annotationName string, cache NodeAnnotationCache) (map[string][]*net.IPNet, error) {
	subnetMap, err := parseSubnetAnnotationWithCacheNoCopy(node, annotationName, cache)
	if err != nil {
		return nil, err
	}
	return copySubnetMap(subnetMap), nil
}

func parseSubnetAnnotationWithCacheNoCopy(node *corev1.Node, annotationName string, cache NodeAnnotationCache) (map[string][]*net.IPNet, error) {
	annotation, ok := node.Annotations[annotationName]
	if !ok {
		return nil, newAnnotationNotSetError("could not find %q annotation", annotationName)
	}
	if cache != nil {
		if cached, ok := cache.GetSubnetMap(node.Name, annotationName, annotation); ok {
			return cached, nil
		}
	}
	subnetMap, err := parseSubnetAnnotationValue(annotationName, annotation)
	if err != nil {
		return nil, err
	}
	if cache != nil {
		cache.SetSubnetMap(node.Name, annotationName, annotation, subnetMap)
	}
	return subnetMap, nil
}

func parseSubnetAnnotationValue(annotationName, annotation string) (map[string][]*net.IPNet, error) {
	subnetsStrMap := map[string][]string{}
	subnetsDual := make(map[string][]string)
	if err := json.Unmarshal([]byte(annotation), &subnetsDual); err == nil {
		subnetsStrMap = subnetsDual
	} else {
		subnetsSingle := make(map[string]string)
		if err := json.Unmarshal([]byte(annotation), &subnetsSingle); err != nil {
			return nil, fmt.Errorf("could not parse %q annotation %q as either single-stack or dual-stack: %v",
				annotationName, annotation, err)
		}
		for netName, v := range subnetsSingle {
			subnetsStrMap[netName] = make([]string, 1)
			subnetsStrMap[netName][0] = v
		}
	}

	if len(subnetsStrMap) == 0 {
		return nil, fmt.Errorf("unexpected empty %s annotation", annotationName)
	}

	subnetMap := make(map[string][]*net.IPNet)
	for netName, subnetsStr := range subnetsStrMap {
		var ipnets []*net.IPNet
		for _, subnet := range subnetsStr {
			_, ipnet, err := net.ParseCIDR(subnet)
			if err != nil {
				return nil, fmt.Errorf("error parsing %q value: %v", annotationName, err)
			}
			ipnets = append(ipnets, ipnet)
		}
		subnetMap[netName] = ipnets
	}
	return subnetMap, nil
}

func copySubnetMap(in map[string][]*net.IPNet) map[string][]*net.IPNet {
	out := make(map[string][]*net.IPNet, len(in))
	for netName, ipnets := range in {
		out[netName] = CopyIPNets(ipnets)
	}
	return out
}

func NodeSubnetAnnotationChanged(oldNode, newNode *corev1.Node) bool {
	return oldNode.Annotations[ovnNodeSubnets] != newNode.Annotations[ovnNodeSubnets]
}

func NodeSubnetAnnotationChangedForNetwork(oldNode, newNode *corev1.Node, netName string) bool {
	var oldSubnets, newSubnets map[string]json.RawMessage

	if err := json.Unmarshal([]byte(oldNode.Annotations[ovnNodeSubnets]), &oldSubnets); err != nil {
		klog.Errorf("Failed to unmarshal old node %s annotation: %v", oldNode.Name, err)
		return false
	}
	if err := json.Unmarshal([]byte(newNode.Annotations[ovnNodeSubnets]), &newSubnets); err != nil {
		klog.Errorf("Failed to unmarshal new node %s annotation: %v", newNode.Name, err)
		return false
	}
	return !bytes.Equal(oldSubnets[netName], newSubnets[netName])
}

// NodeSubnetAnnotationChangedForNetworkWithCache returns true if the node subnet annotation
// for the given network changed, using the provided cache if available.
func NodeSubnetAnnotationChangedForNetworkWithCache(oldNode, newNode *corev1.Node, netName string, cache NodeAnnotationCache) bool {
	if cache == nil {
		return NodeSubnetAnnotationChangedForNetwork(oldNode, newNode, netName)
	}
	oldSubnets, err := parseSubnetAnnotationWithCache(oldNode, ovnNodeSubnets, cache)
	if err != nil {
		if !IsAnnotationNotSetError(err) {
			klog.Errorf("Failed to parse old node %s annotation: %v", oldNode.Name, err)
		}
		return false
	}
	newSubnets, err := parseSubnetAnnotationWithCache(newNode, ovnNodeSubnets, cache)
	if err != nil {
		if !IsAnnotationNotSetError(err) {
			klog.Errorf("Failed to parse new node %s annotation: %v", newNode.Name, err)
		}
		return false
	}
	return !equalIPNetSlices(oldSubnets[netName], newSubnets[netName])
}

// NodeSubnetAnnotationChangedForNetworkWithState returns true if the node subnet annotation
// for the given network changed using pre-parsed annotation state.
func NodeSubnetAnnotationChangedForNetworkWithState(oldState, newState *NodeAnnotationState, netName string) bool {
	if oldState == nil || newState == nil {
		return false
	}
	if oldState.subnetsErr != nil {
		if !IsAnnotationNotSetError(oldState.subnetsErr) {
			klog.Errorf("Failed to parse old node %s annotation: %v", oldState.nodeName, oldState.subnetsErr)
		}
		return false
	}
	if newState.subnetsErr != nil {
		if !IsAnnotationNotSetError(newState.subnetsErr) {
			klog.Errorf("Failed to parse new node %s annotation: %v", newState.nodeName, newState.subnetsErr)
		}
		return false
	}
	return !equalIPNetSlices(oldState.subnets[netName], newState.subnets[netName])
}

func equalIPNetSlices(a, b []*net.IPNet) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		switch {
		case a[i] == nil && b[i] == nil:
			continue
		case a[i] == nil || b[i] == nil:
			return false
		case a[i].String() != b[i].String():
			return false
		}
	}
	return true
}

// UpdateNodeHostSubnetAnnotation updates a "k8s.ovn.org/node-subnets" annotation for network "netName",
// with the specified network, suitable for passing to kube.SetAnnotationsOnNode. If hostSubnets is empty,
// it deleted the "k8s.ovn.org/node-subnets" annotation for network "netName"
func UpdateNodeHostSubnetAnnotation(annotations map[string]string, hostSubnets []*net.IPNet, netName string) (map[string]string, error) {
	if annotations == nil {
		annotations = map[string]string{}
	}
	err := updateSubnetAnnotation(annotations, ovnNodeSubnets, netName, hostSubnets)
	if err != nil {
		return nil, err
	}
	return annotations, nil
}

// SetNodeHostSubnetAnnotation sets a "k8s.ovn.org/node-subnets" annotation
// using a kube.Annotator
func SetNodeHostSubnetAnnotation(nodeAnnotator kube.Annotator, defaultSubnets []*net.IPNet) error {
	return setSubnetAnnotation(nodeAnnotator, ovnNodeSubnets, defaultSubnets)
}

// DeleteNodeHostSubnetAnnotation removes a "k8s.ovn.org/node-subnets" annotation
// using a kube.Annotator
func DeleteNodeHostSubnetAnnotation(nodeAnnotator kube.Annotator) {
	nodeAnnotator.Delete(ovnNodeSubnets)
}

func HasNodeHostSubnetAnnotation(node *corev1.Node, netName string) bool {
	var nodeSubnetMap map[string]json.RawMessage
	annotation, ok := node.Annotations[ovnNodeSubnets]
	if !ok {
		return false
	}
	if err := json.Unmarshal([]byte(annotation), &nodeSubnetMap); err != nil {
		return false
	}
	if _, ok := nodeSubnetMap[netName]; ok {
		return true
	}
	return false
}

// ParseNodeHostSubnetAnnotation parses the "k8s.ovn.org/node-subnets" annotation
// on a node and returns the host subnet for the given network.
func ParseNodeHostSubnetAnnotation(node *corev1.Node, netName string) ([]*net.IPNet, error) {
	var nodeSubnetMap map[string]json.RawMessage
	var ret []*net.IPNet
	annotation, ok := node.Annotations[ovnNodeSubnets]
	if !ok {
		return nil, newAnnotationNotSetError("could not find %q annotation", ovnNodeSubnets)
	}
	if err := json.Unmarshal([]byte(annotation), &nodeSubnetMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal %q annotation on node %s: %v", ovnNodeSubnets, node.Name, err)
	}
	val, ok := nodeSubnetMap[netName]
	if !ok {
		return nil, newAnnotationNotSetError("node %q has no %q annotation for network %s", node.Name, ovnNodeSubnets, netName)
	}

	var subnets, subnetsDual []string
	if err := json.Unmarshal(val, &subnetsDual); err == nil {
		subnets = subnetsDual
	} else {
		subnetsSingle := ""
		if err := json.Unmarshal(val, &subnetsSingle); err != nil {
			return nil, fmt.Errorf("could not parse %q annotation %q as either single-stack or dual-stack: %v",
				ovnNodeSubnets, val, err)
		}
		subnets = append(subnets, subnetsSingle)
	}

	if len(subnets) == 0 {
		return nil, fmt.Errorf("unexpected empty %s annotation for %s network", ovnNodeSubnets, netName)
	}

	for _, subnet := range subnets {
		_, ipnet, err := net.ParseCIDR(subnet)
		if err != nil {
			return nil, fmt.Errorf("error parsing %q value: %v", subnet, err)
		}
		ret = append(ret, ipnet)
	}

	return ret, nil
}

// GetNodeSubnetAnnotationNetworkNames parses the "k8s.ovn.org/node-subnets" annotation
// on a node and returns the list of network names set.
func GetNodeSubnetAnnotationNetworkNames(node *corev1.Node) ([]string, error) {
	nodeNetworks := []string{}
	subnetsMap, err := parseSubnetAnnotation(node.Annotations, ovnNodeSubnets)
	if err != nil {
		return nodeNetworks, err
	}

	for network := range subnetsMap {
		nodeNetworks = append(nodeNetworks, network)
	}

	return nodeNetworks, nil
}

// ParseNodesHostSubnetAnnotation parses parses the "k8s.ovn.org/node-subnets" annotation
// for all the provided nodes
func ParseNodesHostSubnetAnnotation(nodes []*corev1.Node, netName string) ([]*net.IPNet, error) {
	allSubnets := []*net.IPNet{}
	for _, node := range nodes {
		subnets, err := ParseNodeHostSubnetAnnotation(node, netName)
		if err != nil {
			return nil, err
		}
		allSubnets = append(allSubnets, subnets...)
	}
	return allSubnets, nil
}

// ParseNodeHostSubnetsAnnotation parses parses the "k8s.ovn.org/node-subnets" annotation
// for all the networks
func ParseNodeHostSubnetsAnnotation(node *corev1.Node) (map[string][]*net.IPNet, error) {
	return parseSubnetAnnotation(node.Annotations, ovnNodeSubnets)
}
