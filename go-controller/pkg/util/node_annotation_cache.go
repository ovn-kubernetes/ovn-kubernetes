package util

import "net"

// NodeAnnotationCache stores parsed node annotation maps keyed by node and raw value.
// Implementations should treat returned maps as read-only.
type NodeAnnotationCache interface {
	// GetNetworkMap returns parsed data for nodeName and annotationName if raw
	// matches the exact annotation value currently cached for that annotation.
	// annotationName is the node annotation key (for example,
	// "k8s.ovn.org/network-ids"). raw is the raw annotation string read from the
	// node object before parsing.
	GetNetworkMap(nodeName, annotationName, raw string) (map[string]string, bool)
	// SetNetworkMap stores parsed data for nodeName and annotationName under the
	// exact raw annotation string read from the node object.
	// parsed is the decoded map form of raw (network name -> string value).
	SetNetworkMap(nodeName, annotationName, raw string, parsed map[string]string)
	// GetSubnetMap returns parsed data for nodeName and annotationName if raw
	// matches the exact annotation value currently cached for that annotation.
	// annotationName is the node annotation key and raw is the exact annotation
	// string read from the node object before parsing.
	GetSubnetMap(nodeName, annotationName, raw string) (map[string][]*net.IPNet, bool)
	// SetSubnetMap stores parsed data for nodeName and annotationName under the
	// exact raw annotation string read from the node object.
	// parsed is the decoded map form of raw (network name -> list of CIDRs).
	SetSubnetMap(nodeName, annotationName, raw string, parsed map[string][]*net.IPNet)
}
