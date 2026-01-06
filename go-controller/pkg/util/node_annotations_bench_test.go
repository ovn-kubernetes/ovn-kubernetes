package util

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var benchSinkInt int
var benchSinkErr error

// Benchmarks:
// - BenchmarkParseNetworkAnnotationsPerNetwork models current UDN behavior:
//   each per-network controller unmarshals annotations and parses ints.
// - BenchmarkParseNetworkAnnotationsParseOnce models the refactor target:
//   unmarshal each annotation once, then parse per-network values.
// - BenchmarkParseNetworkMapAnnotationOnce isolates raw unmarshal cost.
// - BenchmarkParseNetworkAnnotationsPerNetworkWithCache models per-network
//   controllers using a shared node annotation cache.
// - BenchmarkParseNetworkAnnotationsStatePerNetwork models parse-once state
//   reused across per-network handlers.

func makeNodeWithNetworkAnnotations(networkCount int) (*corev1.Node, []string) {
	netNames := make([]string, 0, networkCount)
	networkIDs := make(map[string]string, networkCount)
	tunnelIDs := make(map[string]string, networkCount)
	for i := 0; i < networkCount; i++ {
		netName := fmt.Sprintf("net-%d", i)
		netNames = append(netNames, netName)
		networkIDs[netName] = strconv.Itoa(i + 1)
		tunnelIDs[netName] = strconv.Itoa(i + 1)
	}

	networkIDsJSON, _ := json.Marshal(networkIDs)
	tunnelIDsJSON, _ := json.Marshal(tunnelIDs)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
			Annotations: map[string]string{
				OvnNetworkIDs:                  string(networkIDsJSON),
				ovnUDNLayer2NodeGRLRPTunnelIDs: string(tunnelIDsJSON),
			},
		},
	}

	return node, netNames
}

type benchCacheKey struct {
	nodeName       string
	annotationName string
	raw            string
}

type benchAnnotationCache struct {
	network map[benchCacheKey]map[string]string
	subnet  map[benchCacheKey]map[string][]*net.IPNet
}

func newBenchAnnotationCache() *benchAnnotationCache {
	return &benchAnnotationCache{
		network: map[benchCacheKey]map[string]string{},
		subnet:  map[benchCacheKey]map[string][]*net.IPNet{},
	}
}

func (c *benchAnnotationCache) GetNetworkMap(nodeName, annotationName, raw string) (map[string]string, bool) {
	m, ok := c.network[benchCacheKey{nodeName: nodeName, annotationName: annotationName, raw: raw}]
	return m, ok
}

func (c *benchAnnotationCache) SetNetworkMap(nodeName, annotationName, raw string, parsed map[string]string) {
	c.network[benchCacheKey{nodeName: nodeName, annotationName: annotationName, raw: raw}] = parsed
}

func (c *benchAnnotationCache) GetSubnetMap(nodeName, annotationName, raw string) (map[string][]*net.IPNet, bool) {
	m, ok := c.subnet[benchCacheKey{nodeName: nodeName, annotationName: annotationName, raw: raw}]
	return m, ok
}

func (c *benchAnnotationCache) SetSubnetMap(nodeName, annotationName, raw string, parsed map[string][]*net.IPNet) {
	c.subnet[benchCacheKey{nodeName: nodeName, annotationName: annotationName, raw: raw}] = parsed
}

func BenchmarkParseNetworkMapAnnotationOnce(b *testing.B) {
	for _, count := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("networks=%d", count), func(b *testing.B) {
			node, _ := makeNodeWithNetworkAnnotations(count)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, benchSinkErr = parseNetworkMapAnnotation(node.Annotations, OvnNetworkIDs)
			}
		})
	}
}

func BenchmarkParseNetworkAnnotationsPerNetwork(b *testing.B) {
	for _, count := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("networks=%d", count), func(b *testing.B) {
			node, netNames := makeNodeWithNetworkAnnotations(count)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, netName := range netNames {
					benchSinkInt, benchSinkErr = ParseNetworkIDAnnotation(node, netName)
					benchSinkInt, benchSinkErr = ParseUDNLayer2NodeGRLRPTunnelIDs(node, netName)
				}
			}
		})
	}
}

func BenchmarkParseNetworkAnnotationsPerNetworkWithCache(b *testing.B) {
	for _, count := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("networks=%d", count), func(b *testing.B) {
			node, netNames := makeNodeWithNetworkAnnotations(count)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cache := newBenchAnnotationCache()
				for _, netName := range netNames {
					networkIDsMap, err := parseNetworkMapAnnotationWithCache(node, OvnNetworkIDs, cache)
					if err != nil {
						b.Fatal(err)
					}
					tunnelIDsMap, err := parseNetworkMapAnnotationWithCache(node, ovnUDNLayer2NodeGRLRPTunnelIDs, cache)
					if err != nil {
						b.Fatal(err)
					}
					benchSinkInt, benchSinkErr = strconv.Atoi(networkIDsMap[netName])
					benchSinkInt, benchSinkErr = strconv.Atoi(tunnelIDsMap[netName])
				}
			}
		})
	}
}

func BenchmarkParseNetworkAnnotationsParseOnce(b *testing.B) {
	for _, count := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("networks=%d", count), func(b *testing.B) {
			node, netNames := makeNodeWithNetworkAnnotations(count)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				networkIDsMap, err := parseNetworkMapAnnotation(node.Annotations, OvnNetworkIDs)
				if err != nil {
					b.Fatal(err)
				}
				tunnelIDsMap, err := parseNetworkMapAnnotation(node.Annotations, ovnUDNLayer2NodeGRLRPTunnelIDs)
				if err != nil {
					b.Fatal(err)
				}
				for _, netName := range netNames {
					benchSinkInt, benchSinkErr = strconv.Atoi(networkIDsMap[netName])
					benchSinkInt, benchSinkErr = strconv.Atoi(tunnelIDsMap[netName])
				}
			}
		})
	}
}

func BenchmarkParseNetworkAnnotationsStatePerNetwork(b *testing.B) {
	for _, count := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("networks=%d", count), func(b *testing.B) {
			node, netNames := makeNodeWithNetworkAnnotations(count)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cache := newBenchAnnotationCache()
				state := BuildNodeAnnotationState(node, cache)
				for _, netName := range netNames {
					benchSinkInt, benchSinkErr = state.NetworkID(netName)
					benchSinkInt, benchSinkErr = state.TunnelID(netName)
				}
			}
		})
	}
}
