package topology

import (
	"net"
	"testing"
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
	cache := newNodeAnnotationCache()
	nodeName := "node-a"
	annotationName := "k8s.ovn.org/network-ids"
	raw := `{"default":"0","blue":"42"}`
	parsed := map[string]string{"default": "0", "blue": "42"}

	if _, ok := cache.GetNetworkMap(nodeName, annotationName, raw); ok {
		t.Fatal("expected cache miss before SetNetworkMap")
	}

	cache.SetNetworkMap(nodeName, annotationName, raw, parsed)

	got, ok := cache.GetNetworkMap(nodeName, annotationName, raw)
	if !ok {
		t.Fatal("expected cache hit for matching node/annotation/raw")
	}
	if len(got) != len(parsed) || got["default"] != "0" || got["blue"] != "42" {
		t.Fatalf("unexpected parsed map: got=%v want=%v", got, parsed)
	}

	if _, ok := cache.GetNetworkMap(nodeName, annotationName, `{"default":"0"}`); ok {
		t.Fatal("expected cache miss for stale raw value")
	}
	if _, ok := cache.GetNetworkMap(nodeName, "k8s.ovn.org/other", raw); ok {
		t.Fatal("expected cache miss for different annotation name")
	}
	if _, ok := cache.GetNetworkMap("node-b", annotationName, raw); ok {
		t.Fatal("expected cache miss for different node")
	}

	newRaw := `{"default":"0","blue":"43"}`
	newParsed := map[string]string{"default": "0", "blue": "43"}
	cache.SetNetworkMap(nodeName, annotationName, newRaw, newParsed)

	if _, ok := cache.GetNetworkMap(nodeName, annotationName, raw); ok {
		t.Fatal("expected old raw value to miss after cache update")
	}
	got, ok = cache.GetNetworkMap(nodeName, annotationName, newRaw)
	if !ok {
		t.Fatal("expected cache hit for updated raw value")
	}
	if got["blue"] != "43" {
		t.Fatalf("expected updated cached value, got=%v", got["blue"])
	}
}

func TestNodeAnnotationCacheSubnetMapHitMissAndDeleteNode(t *testing.T) {
	cache := newNodeAnnotationCache()
	nodeName := "node-a"
	annotationName := "k8s.ovn.org/node-subnets"
	raw := `{"default":["10.128.0.0/23"]}`
	parsed := map[string][]*net.IPNet{
		"default": {mustParseCIDR(t, "10.128.0.0/23")},
	}

	if _, ok := cache.GetSubnetMap(nodeName, annotationName, raw); ok {
		t.Fatal("expected cache miss before SetSubnetMap")
	}

	cache.SetSubnetMap(nodeName, annotationName, raw, parsed)

	got, ok := cache.GetSubnetMap(nodeName, annotationName, raw)
	if !ok {
		t.Fatal("expected cache hit for matching node/annotation/raw")
	}
	if len(got["default"]) != 1 || got["default"][0].String() != "10.128.0.0/23" {
		t.Fatalf("unexpected subnet map: got=%v", got)
	}

	cache.DeleteNode(nodeName)

	if _, ok := cache.GetSubnetMap(nodeName, annotationName, raw); ok {
		t.Fatal("expected subnet cache miss after DeleteNode")
	}
}
