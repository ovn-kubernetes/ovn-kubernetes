package util

import "net"

// NodeAnnotationCache stores parsed node annotation maps keyed by node and raw value.
// Implementations should treat returned maps as read-only.
type NodeAnnotationCache interface {
	GetNetworkMap(nodeName, annotationName, raw string) (map[string]string, bool)
	SetNetworkMap(nodeName, annotationName, raw string, parsed map[string]string)
	GetSubnetMap(nodeName, annotationName, raw string) (map[string][]*net.IPNet, bool)
	SetSubnetMap(nodeName, annotationName, raw string, parsed map[string][]*net.IPNet)
}
