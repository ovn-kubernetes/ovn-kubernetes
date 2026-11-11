// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func mustParseCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("failed to parse CIDR %q: %v", cidr, err)
	}
	return ipNet
}

func TestNodeAnnotationCacheNetworkMapHitMissAndStaleRaw(t *testing.T) {
	cache := NewNodeAnnotationCache()
	nodeName := "node-a"
	annotationName := "k8s.ovn.org/network-ids"
	raw := `{"default":"0","blue":"42"}`
	parsed := map[string]string{"default": "0", "blue": "42"}

	if _, ok := cache.getParsedNetworkMap(nodeName, annotationName, raw); ok {
		t.Fatal("expected cache miss before SetNetworkMap")
	}

	cache.setNetworkMap(nodeName, annotationName, raw, parsed)

	got, ok := cache.getParsedNetworkMap(nodeName, annotationName, raw)
	if !ok {
		t.Fatal("expected cache hit for matching node/annotation/raw")
	}
	if len(got) != len(parsed) || got["default"] != "0" || got["blue"] != "42" {
		t.Fatalf("unexpected parsed map: got=%v want=%v", got, parsed)
	}

	if _, ok := cache.getParsedNetworkMap(nodeName, annotationName, `{"default":"0"}`); ok {
		t.Fatal("expected cache miss for stale raw value")
	}
	if _, ok := cache.getParsedNetworkMap(nodeName, "k8s.ovn.org/other", raw); ok {
		t.Fatal("expected cache miss for different annotation name")
	}
	if _, ok := cache.getParsedNetworkMap("node-b", annotationName, raw); ok {
		t.Fatal("expected cache miss for different node")
	}

	newRaw := `{"default":"0","blue":"43"}`
	newParsed := map[string]string{"default": "0", "blue": "43"}
	cache.setNetworkMap(nodeName, annotationName, newRaw, newParsed)

	if _, ok := cache.getParsedNetworkMap(nodeName, annotationName, raw); ok {
		t.Fatal("expected old raw value to miss after cache update")
	}
	got, ok = cache.getParsedNetworkMap(nodeName, annotationName, newRaw)
	if !ok {
		t.Fatal("expected cache hit for updated raw value")
	}
	if got["blue"] != "43" {
		t.Fatalf("expected updated cached value, got=%v", got["blue"])
	}
}

func TestNodeAnnotationCacheSubnetMapHitMissAndDeleteNode(t *testing.T) {
	cache := NewNodeAnnotationCache()
	nodeName := "node-a"
	annotationName := "k8s.ovn.org/node-subnets"
	raw := `{"default":["10.128.0.0/23"]}`
	parsed := map[string][]*net.IPNet{
		"default": {mustParseCIDR(t, "10.128.0.0/23")},
	}

	if _, ok := cache.getParsedSubnetMap(nodeName, annotationName, raw); ok {
		t.Fatal("expected cache miss before SetSubnetMap")
	}

	cache.setSubnetMap(nodeName, annotationName, raw, parsed)

	got, ok := cache.getParsedSubnetMap(nodeName, annotationName, raw)
	if !ok {
		t.Fatal("expected cache hit for matching node/annotation/raw")
	}
	if len(got["default"]) != 1 || got["default"][0].String() != "10.128.0.0/23" {
		t.Fatalf("unexpected subnet map: got=%v", got)
	}

	cache.deleteNode(nodeName)

	if _, ok := cache.getParsedSubnetMap(nodeName, annotationName, raw); ok {
		t.Fatal("expected subnet cache miss after DeleteNode")
	}
}

func TestBuildNodeAnnotationStateDoesNotOverwriteLatestCacheWithOldSnapshot(t *testing.T) {
	cache := NewNodeAnnotationCache()
	nodeName := "node-a"
	oldRaw := `{"default":"0","blue":"42"}`
	newRaw := `{"default":"0","blue":"43"}`

	newNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				util.OvnNetworkIDs:          newRaw,
				types.NodeSubnetsAnnotation: `{"default":["10.128.1.0/24"]}`,
			},
		},
	}
	oldNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				util.OvnNetworkIDs:          oldRaw,
				types.NodeSubnetsAnnotation: `{"default":["10.128.0.0/24"]}`,
			},
		},
	}

	if state := cache.updateNodeAnnotationState(newNode, true); state == nil {
		t.Fatal("expected new node state to be built")
	}
	if state := cache.updateNodeAnnotationState(oldNode, false); state == nil {
		t.Fatal("expected old node state to be built")
	}

	if _, ok := cache.getParsedNetworkMap(nodeName, util.OvnNetworkIDs, oldRaw); ok {
		t.Fatal("expected old raw network annotation not to replace the latest cached value")
	}
	got, ok := cache.getParsedNetworkMap(nodeName, util.OvnNetworkIDs, newRaw)
	if !ok {
		t.Fatal("expected new raw network annotation to remain cached")
	}
	if got["blue"] != "43" {
		t.Fatalf("expected latest cached value to be preserved, got=%v", got["blue"])
	}
}

func TestParseSubnetMapValueRejectsEmptySubnetEntry(t *testing.T) {
	_, err := parseSubnetMapValue(types.NodeSubnetsAnnotation, `{"isolatednet":[]}`)
	if err == nil {
		t.Fatal("expected empty subnet entry to be rejected")
	}
}

func TestSubnetComparisonAcrossMalformedAttempts(t *testing.T) {
	for _, malformed := range []string{`{invalid`, `{"default":["invalid-cidr"]}`} {
		t.Run(malformed, func(t *testing.T) {
			cache := NewNodeAnnotationCache()
			oldNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: "node-a", Annotations: map[string]string{
					types.NodeSubnetsAnnotation: `{"default":["10.1.0.0/24"]}`,
				},
			}}
			for i, raw := range []string{malformed, malformed + " ", `{"default":["10.2.0.0/24"]}`, `{"default":["10.2.0.0/24"]}`} {
				newNode := oldNode.DeepCopy()
				newNode.Annotations[types.NodeSubnetsAnnotation] = raw
				oldState := cache.UpdateNodeAnnotationState(oldNode, false)
				newState := cache.UpdateNodeAnnotationState(newNode, true)
				wantChanged := i == 2
				if got := NodeSubnetAnnotationChangedForNetworkWithState(oldState, newState, types.DefaultNetworkName); got != wantChanged {
					t.Errorf("attempt %d: parsed comparison = %t, want %t", i, got, wantChanged)
				}
				// The raw comparator validates JSON only; invalid CIDRs remain
				// valid JSON and are compared as before.
				wantChanged = wantChanged || (i == 0 && malformed != `{invalid`)
				if got := util.NodeSubnetAnnotationChangedForNetwork(oldNode, newNode, types.DefaultNetworkName); got != wantChanged {
					t.Errorf("attempt %d: raw comparison = %t, want %t", i, got, wantChanged)
				}
				oldNode = newNode
			}
		})
	}
}

func TestGatewayAnnotationChangedForNetworkWithState(t *testing.T) {
	cache := NewNodeAnnotationCache()

	oldNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
			Annotations: map[string]string{
				util.OvnNodeL3GatewayConfig: `{"default":{"mode":"shared","mac-address":"0a:58:0a:01:01:01","ip-address":"192.168.1.2/24","next-hop":"192.168.1.1"},"blue":{"mode":"shared","mac-address":"0a:58:0a:01:01:02","ip-address":"10.0.0.2/24","next-hop":"10.0.0.1"}}`,
			},
		},
	}
	newNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
			Annotations: map[string]string{
				util.OvnNodeL3GatewayConfig: `{"default":{"mode":"shared","mac-address":"0a:58:0a:01:01:01","ip-address":"192.168.1.2/24","next-hop":"192.168.1.1"},"blue":{"mode":"shared","mac-address":"0a:58:0a:01:01:02","ip-address":"10.0.1.2/24","next-hop":"10.0.1.1"}}`,
			},
		},
	}

	oldState := cache.UpdateNodeAnnotationState(oldNode, false)
	newState := cache.UpdateNodeAnnotationState(newNode, true)

	if GatewayAnnotationChangedForNetworkWithState(oldState, newState, types.DefaultNetworkName) {
		t.Fatal("expected default network gateway state to remain unchanged when only another network changed")
	}
	if !GatewayAnnotationChangedForNetworkWithState(oldState, newState, "blue") {
		t.Fatal("expected blue network gateway state change to be detected")
	}
}
