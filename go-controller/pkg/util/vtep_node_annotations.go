// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
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

// SetNodeVTEPEntry sets or updates a VTEP entry in the k8s.ovn.org/vteps
// annotation on a node using retry-on-conflict. The annotation is a shared
// JSON blob written by both the cluster-manager (managed VTEPs) and
// ovnkube-node (unmanaged VTEPs), so concurrent writes are expected.
func SetNodeVTEPEntry(nodeName, vtepName string, ips []string, getNode func(string) (*corev1.Node, error), updateNode func(*corev1.Node) error) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		node, err := getNode(nodeName)
		if err != nil {
			return fmt.Errorf("failed to get node %s: %w", nodeName, err)
		}
		cnode := node.DeepCopy()

		vteps, err := ParseNodeVTEPs(cnode)
		if err != nil {
			if !IsAnnotationNotSetError(err) {
				return fmt.Errorf("failed to parse VTEP annotation: %w", err)
			}
			vteps = map[string]VTEPNodeAnnotation{}
		}

		vteps[vtepName] = VTEPNodeAnnotation{IPs: ips}
		return applyVTEPAnnotation(cnode, vteps, updateNode)
	})
}

// RemoveNodeVTEPEntry removes a VTEP entry from the k8s.ovn.org/vteps
// annotation on a node using retry-on-conflict. If the entry does not exist
// or the annotation is not set, this is a no-op.
func RemoveNodeVTEPEntry(nodeName, vtepName string, getNode func(string) (*corev1.Node, error), updateNode func(*corev1.Node) error) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		node, err := getNode(nodeName)
		if err != nil {
			return fmt.Errorf("failed to get node %s: %w", nodeName, err)
		}

		vteps, err := ParseNodeVTEPs(node)
		if err != nil {
			if IsAnnotationNotSetError(err) {
				return nil
			}
			return fmt.Errorf("failed to parse VTEP annotation: %w", err)
		}

		if _, ok := vteps[vtepName]; !ok {
			return nil
		}

		cnode := node.DeepCopy()
		delete(vteps, vtepName)
		return applyVTEPAnnotation(cnode, vteps, updateNode)
	})
}

func applyVTEPAnnotation(node *corev1.Node, vteps map[string]VTEPNodeAnnotation, updateNode func(*corev1.Node) error) error {
	annotations, err := MarshalNodeVTEPs(vteps)
	if err != nil {
		return fmt.Errorf("failed to marshal VTEP annotation: %w", err)
	}
	for k, v := range annotations {
		if v == nil {
			delete(node.Annotations, k)
		} else {
			if node.Annotations == nil {
				node.Annotations = map[string]string{}
			}
			node.Annotations[k] = v.(string)
		}
	}
	return updateNode(node)
}
