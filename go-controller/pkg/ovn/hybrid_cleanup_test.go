// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

type hybridCleanupFailureClient struct {
	libovsdbclient.Client
}

func (c *hybridCleanupFailureClient) Transact(_ context.Context, _ ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	return nil, fmt.Errorf("injected hybrid overlay cleanup failure")
}

var _ = ginkgo.Describe("Hybrid overlay cleanup without node metadata", func() {
	var fakeOvn *FakeOVN
	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.HybridOverlay.Enabled = true
		var err error
		config.Kubernetes.NoHostSubnetNodes, err = metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
			MatchLabels: map[string]string{corev1.LabelOSStable: "windows"},
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		fakeOvn = NewFakeOVN(false, "node1")
	})
	ginkgo.AfterEach(func() { fakeOvn.shutdown() })

	ginkgo.DescribeTable("does not transact when the cluster router has no hybrid overlay state", func(routerExists bool) {
		data := []libovsdbtest.TestData{}
		if routerExists {
			data = append(data, &nbdb.LogicalRouter{UUID: "cluster-UUID", Name: types.OVNClusterRouter})
		}
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{NBData: data})
		fakeOvn.controller.nbClient = &hybridCleanupFailureClient{Client: fakeOvn.nbClient}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "windows"}}
		for range 2 {
			gomega.Expect(fakeOvn.controller.deleteHoNodeEvent(node)).To(gomega.Succeed())
		}
		gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(data))
	},
		ginkgo.Entry("missing cluster router", false),
		ginkgo.Entry("empty cluster router", true),
	)

	ginkgo.DescribeTable("removes destination routes atomically and preserves other nodes",
		func(ipFamily, subnet, otherSubnet string, policyOnly bool) {
			cluster := &nbdb.LogicalRouter{UUID: "cluster-UUID", Name: types.OVNClusterRouter}
			gateway := &nbdb.LogicalRouter{UUID: "gateway-UUID", Name: "GR_node1"}
			data := []libovsdbtest.TestData{cluster, gateway}
			expected := []libovsdbtest.TestData{}
			for _, target := range []string{"windows", "windows-extra"} {
				prefix := subnet
				if target != "windows" {
					prefix = otherSubnet
				}
				policy := &nbdb.LogicalRouterPolicy{
					UUID: target + "-policy-UUID", Priority: types.HybridOverlaySubnetPriority, Action: "reroute",
					Match:    fmt.Sprintf(`inport == "rtos-node1" && %s.dst == %s`, ipFamily, prefix),
					Nexthops: []string{"10.1.1.3"}, ExternalIDs: map[string]string{"name": "hybrid-subnet-node1:" + target},
				}
				gatewayPolicy := policy.DeepCopy()
				gatewayPolicy.UUID = target + "-gateway-policy-UUID"
				gatewayPolicy.Match = fmt.Sprintf("%s.src == 100.64.0.2 && %s.dst == %s", ipFamily, ipFamily, prefix)
				gatewayPolicy.ExternalIDs = map[string]string{"name": "hybrid-subnet-node1-gr"}
				cluster.Policies = append(cluster.Policies, policy.UUID, gatewayPolicy.UUID)
				data = append(data, policy, gatewayPolicy)
				if target != "windows" {
					expected = append(expected, policy, gatewayPolicy)
				}
				if policyOnly && target == "windows" {
					continue
				}
				route := &nbdb.LogicalRouterStaticRoute{
					UUID: target + "-route-UUID", IPPrefix: prefix, Nexthop: "10.1.1.3",
					ExternalIDs: map[string]string{"name": "hybrid-subnet-node1:" + target},
				}
				gatewayRoute := route.DeepCopy()
				gatewayRoute.UUID = target + "-gateway-route-UUID"
				gatewayRoute.Nexthop = "100.64.0.1"
				gatewayRoute.ExternalIDs = map[string]string{"name": "hybrid-subnet-node1-gr"}
				cluster.StaticRoutes = append(cluster.StaticRoutes, route.UUID)
				gateway.StaticRoutes = append(gateway.StaticRoutes, gatewayRoute.UUID)
				data = append(data, route, gatewayRoute)
				if target != "windows" {
					expected = append(expected, route, gatewayRoute)
				}
			}
			// A different feature's policy to the same subnet must not be removed.
			unrelated := &nbdb.LogicalRouterPolicy{
				UUID: "unrelated-policy-UUID", Priority: types.HybridOverlaySubnetPriority, Action: "allow",
				Match: fmt.Sprintf("%s.dst == %s", ipFamily, subnet), ExternalIDs: map[string]string{"name": "unrelated"},
			}
			cluster.Policies = append(cluster.Policies, unrelated.UUID)
			data = append(data, unrelated)
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{NBData: data})
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "windows"}}

			ginkgo.By("retaining discovery records when the cleanup transaction fails")
			fakeOvn.controller.nbClient = &hybridCleanupFailureClient{Client: fakeOvn.nbClient}
			gomega.Expect(fakeOvn.controller.deleteHoNodeEvent(node)).To(gomega.MatchError(gomega.ContainSubstring("injected hybrid overlay cleanup failure")))
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(data))
			fakeOvn.controller.nbClient = fakeOvn.nbClient

			expectedCluster := cluster.DeepCopy()
			expectedCluster.Policies = []string{"windows-extra-policy-UUID", "windows-extra-gateway-policy-UUID", unrelated.UUID}
			expectedCluster.StaticRoutes = []string{"windows-extra-route-UUID"}
			expectedGateway := gateway.DeepCopy()
			expectedGateway.StaticRoutes = []string{"windows-extra-gateway-route-UUID"}
			expected = append(expected, unrelated, expectedCluster, expectedGateway)
			ginkgo.By("deleting from a name-only node even when neither node exists in the informer")
			for range 2 {
				gomega.Expect(fakeOvn.controller.ReconcileNode(context.Background(), node, nil, nil, nil)).To(gomega.Succeed())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expected))
			}
		},
		ginkgo.Entry("IPv4", "ip4", "11.1.0.0/24", "11.1.1.0/24", false),
		ginkgo.Entry("IPv6", "ip6", "fd11::/64", "fd11:0:0:1::/64", false),
		ginkgo.Entry("partial setup before routes exist", "ip4", "11.1.0.0/24", "11.1.1.0/24", true),
	)

	ginkgo.DescribeTable("removes source-node bindings without a DRIP and preserves similarly named nodes", func(deletedSource, ipFamily, subnet string) {
		drIP, gatewayIP, clusterIP := "10.1.1.3", "100.64.0.2", "100.64.0.1"
		if ipFamily == "ip6" {
			drIP, gatewayIP, clusterIP = "fd10::3", "fd98::2", "fd98::1"
		}
		cluster := &nbdb.LogicalRouter{UUID: "cluster-UUID", Name: types.OVNClusterRouter}
		expectedCluster := cluster.DeepCopy()
		data := []libovsdbtest.TestData{cluster}
		expected := []libovsdbtest.TestData{expectedCluster}
		for _, source := range []string{"node1", "node10", "node1-extra", "node1-gr"} {
			policy := &nbdb.LogicalRouterPolicy{
				UUID: source + "-policy-UUID", Priority: types.HybridOverlaySubnetPriority, Action: "reroute",
				Match:    fmt.Sprintf(`inport == "rtos-%s" && %s.dst == %s`, source, ipFamily, subnet),
				Nexthops: []string{drIP}, ExternalIDs: map[string]string{"name": "hybrid-subnet-" + source},
			}
			gatewayPolicy := policy.DeepCopy()
			gatewayPolicy.UUID = source + "-gateway-policy-UUID"
			gatewayPolicy.Match = fmt.Sprintf("%s.src == %s && %s.dst == %s", ipFamily, gatewayIP, ipFamily, subnet)
			gatewayPolicy.ExternalIDs = map[string]string{"name": "hybrid-subnet-" + source + "-gr"}
			route := &nbdb.LogicalRouterStaticRoute{
				UUID: source + "-route-UUID", IPPrefix: subnet, Nexthop: drIP,
				ExternalIDs: map[string]string{"name": "hybrid-subnet-" + source},
			}
			gatewayRoute := route.DeepCopy()
			gatewayRoute.UUID = source + "-gateway-route-UUID"
			gatewayRoute.Nexthop = clusterIP
			gatewayRoute.ExternalIDs = map[string]string{"name": "hybrid-subnet-" + source + "-gr"}
			gateway := &nbdb.LogicalRouter{UUID: source + "-gateway-UUID", Name: "GR_" + source, StaticRoutes: []string{gatewayRoute.UUID}}
			cluster.Policies = append(cluster.Policies, policy.UUID, gatewayPolicy.UUID)
			cluster.StaticRoutes = append(cluster.StaticRoutes, route.UUID)
			data = append(data, policy, gatewayPolicy, route, gatewayRoute, gateway)
			if source != deletedSource {
				expected = append(expected, policy, gatewayPolicy, route, gatewayRoute, gateway)
				expectedCluster.Policies = append(expectedCluster.Policies, policy.UUID, gatewayPolicy.UUID)
				expectedCluster.StaticRoutes = append(expectedCluster.StaticRoutes, route.UUID)
			} else {
				expectedGateway := gateway.DeepCopy()
				expectedGateway.StaticRoutes = nil
				expected = append(expected, expectedGateway)
			}
			for i, ip := range []string{"10.1.1.3", "fd10::3"} {
				binding := &nbdb.StaticMACBinding{UUID: fmt.Sprintf("%s-binding-%d-UUID", source, i),
					LogicalPort: "rtos-" + source, IP: ip, MAC: "0a:58:0a:01:01:03", OverrideDynamicMAC: true}
				data = append(data, binding)
				if source != deletedSource {
					expected = append(expected, binding)
				}
			}
		}
		fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{NBData: data})
		for range 2 {
			gomega.Expect(fakeOvn.controller.deleteHybridOverlayPort(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: deletedSource}})).To(gomega.Succeed())
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expected))
		}
	},
		ginkgo.Entry("IPv4 base-name collision", "node1", "ip4", "11.1.0.0/16"),
		ginkgo.Entry("IPv4 gateway-name collision", "node1-gr", "ip4", "11.1.0.0/16"),
		ginkgo.Entry("IPv6 base-name collision", "node1", "ip6", "fd11::/64"),
		ginkgo.Entry("IPv6 gateway-name collision", "node1-gr", "ip6", "fd11::/64"),
	)
})
