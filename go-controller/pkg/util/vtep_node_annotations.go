package util

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
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
