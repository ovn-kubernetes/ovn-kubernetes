package util

import (
	"encoding/json"
	"fmt"
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
