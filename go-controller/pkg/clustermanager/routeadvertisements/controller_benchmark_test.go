// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"context"
	"fmt"
	"testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// BenchmarkGenerateFRRConfigurations measures complete generation using cached
// Nodes and source configurations, without API writes or informer startup.
func BenchmarkGenerateFRRConfigurations(b *testing.B) {
	for _, nodeCount := range []int{100, 3094} {
		for _, configCount := range []int{1, 32} {
			b.Run(fmt.Sprintf("nodes=%d/configs=%d", nodeCount, configCount), func(b *testing.B) {
				oldDefault, oldFeatures, oldGateway := config.Default, config.OVNKubernetesFeature, config.Gateway
				b.Cleanup(func() {
					config.Default, config.OVNKubernetesFeature, config.Gateway = oldDefault, oldFeatures, oldGateway
				})
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: ovntest.MustParseIPNet("10.0.0.0/8"), HostSubnetLength: 24}}
				config.Default.Transport = ""
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableRouteAdvertisements = true
				config.OVNKubernetesFeature.EnableDynamicUDNAllocation = false
				config.Gateway.Mode = config.GatewayModeLocal
				clients := util.GetOVNClientset().GetClusterManagerClientset()
				wf, err := factory.NewClusterManagerWatchFactory(clients)
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(wf.Shutdown)
				c := NewController(&networkmanager.FakeNetworkManager{}, wf, clients)
				nad := (&testNAD{Name: config.Default.ClusterDefaultNetworkNAD.Name, Namespace: config.Default.ClusterDefaultNetworkNAD.Namespace, Network: types.DefaultNetworkName}).NAD()
				nad.Annotations = map[string]string{types.OvnNetworkNameAnnotation: types.DefaultNetworkName}
				if err := wf.NADInformer().Informer().GetIndexer().Add(nad); err != nil {
					b.Fatal(err)
				}
				for i := 0; i < nodeCount; i++ {
					node := (&testNode{
						Name:                     fmt.Sprintf("node-%04d", i),
						Labels:                   map[string]string{"rack": fmt.Sprint(i % configCount)},
						PrimaryAddressAnnotation: fmt.Sprintf(`{"ipv4":"172.16.%d.%d/24"}`, i/250, i%250+1),
						SubnetsAnnotation:        fmt.Sprintf(`{"default":"10.%d.%d.0/24"}`, i/256, i%256),
					}).Node()
					if err := wf.NodeCoreInformer().Informer().GetIndexer().Add(node); err != nil {
						b.Fatal(err)
					}
				}
				for i := 0; i < configCount; i++ {
					source := (&testFRRConfig{
						Name: fmt.Sprintf("source-%02d", i), Namespace: "frr",
						NodeSelector: map[string]string{"rack": fmt.Sprint(i)},
						Routers:      []*testRouter{{ASN: 64512, Neighbors: []*testNeighbor{{ASN: 64513, Address: "192.0.2.1"}}}},
					}).FRRConfiguration()
					if err := wf.FRRConfigurationsInformer().Informer().GetIndexer().Add(source); err != nil {
						b.Fatal(err)
					}
				}
				ra := (&testRA{Name: "benchmark", AdvertisePods: true, SelectsDefault: true}).RouteAdvertisements()
				ctx := context.Background()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					generated, _, err := c.generateFRRConfigurations(ctx, ra)
					if err != nil {
						b.Fatal(err)
					}
					if len(generated) != nodeCount {
						b.Fatalf("generated %d configurations, want %d", len(generated), nodeCount)
					}
				}
			})
		}
	}
}
