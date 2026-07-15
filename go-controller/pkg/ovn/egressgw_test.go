// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"net"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("OVN Egress Gateway Operations", func() {
	const (
		namespaceName = "namespace1"
	)
	var (
		app     *cli.App
		fakeOvn *FakeOVN

		logicalRouterPort = "rtoe-GR_node1"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.Zone = "node1"
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true)
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("hybrid route policy operations in lgw mode", func() {
		ginkgo.It("delete hybrid route policy for pods", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				asIndex := apbroute.GetHybridRouteAddrSetDbIDs("node1", ovntypes.DefaultNetworkControllerName)
				asv4, _ := addressset.GetHashNamesForAS(asIndex)
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPolicy{
								UUID:     "2a7a61cb-fb13-4266-a3f0-9ac5c4471123 [u2596996164]",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.0.4"},
								Match:    "inport == \"rtos-node1\" && ip4.src == $" + asv4 + " && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouter{
								Name:     ovntypes.OVNClusterRouter,
								UUID:     ovntypes.OVNClusterRouter + "-UUID",
								Policies: []string{"2a7a61cb-fb13-4266-a3f0-9ac5c4471123 [u2596996164]"},
							},
							&nbdb.LogicalRouter{
								UUID:  "GR_node1-UUID",
								Name:  "GR_node1",
								Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
								Networks: []string{"100.64.0.4/32"},
							},
						},
					},
				)
				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{},
					},
					&nbdb.LogicalRouter{
						UUID:  "GR_node1-UUID",
						Name:  "GR_node1",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				injectNode(fakeOvn)
				err := fakeOvn.controller.delHybridRoutePolicyForPod(net.ParseIP("10.128.1.3"), "node1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				dbIDs := apbroute.GetHybridRouteAddrSetDbIDs("node1", ovntypes.DefaultNetworkControllerName)
				fakeOvn.asf.EventuallyExpectNoAddressSet(dbIDs)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("delete hybrid route policy for pods with force", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				asIndex1 := apbroute.GetHybridRouteAddrSetDbIDs("node1", ovntypes.DefaultNetworkControllerName)
				as1v4, _ := addressset.GetHashNamesForAS(asIndex1)
				asIndex2 := apbroute.GetHybridRouteAddrSetDbIDs("node2", ovntypes.DefaultNetworkControllerName)
				as2v4, _ := addressset.GetHashNamesForAS(asIndex2)
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-1st-UUID",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.0.4"},
								Match:    "inport == \"rtos-node1\" && ip4.src == $" + as1v4 + " && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-2nd-UUID",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.1.4"},
								Match:    "inport == \"rtos-node2\" && ip4.src == $" + as2v4 + " && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouter{
								Name:     ovntypes.OVNClusterRouter,
								UUID:     ovntypes.OVNClusterRouter + "-UUID",
								Policies: []string{"501-1st-UUID", "501-2nd-UUID"},
							},
							&nbdb.LogicalRouter{
								UUID:  "GR_node1-UUID",
								Name:  "GR_node1",
								Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
								Networks: []string{"100.64.0.4/32"},
							},
						},
					},
				)
				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{},
					},
					&nbdb.LogicalRouter{
						UUID:  "GR_node1-UUID",
						Name:  "GR_node1",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				err := fakeOvn.controller.delAllHybridRoutePolicies()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				dbIDs := apbroute.GetHybridRouteAddrSetDbIDs("node1", ovntypes.DefaultNetworkControllerName)
				fakeOvn.asf.EventuallyExpectNoAddressSet(dbIDs)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("delete legacy hybrid route policies", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				asIndex := apbroute.GetHybridRouteAddrSetDbIDs("node1", ovntypes.DefaultNetworkControllerName)
				asv4, _ := addressset.GetHashNamesForAS(asIndex)
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-1st-UUID",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.0.4"},
								Match:    "inport == \"rtos-node1\" && ip4.src == 1.3.3.7 && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-2nd-UUID",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.1.4"},
								Match:    "inport == \"rtos-node2\" && ip4.src == 1.3.3.8 && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-new-UUID",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.1.4"},
								Match:    "inport == \"rtos-node2\" && ip4.src == $" + asv4 + " && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouter{
								Name:     ovntypes.OVNClusterRouter,
								UUID:     ovntypes.OVNClusterRouter + "-UUID",
								Policies: []string{"501-1st-UUID", "501-2nd-UUID", "501-new-UUID"},
							},
							&nbdb.LogicalRouter{
								UUID:  "GR_node1-UUID",
								Name:  "GR_node1",
								Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
								Networks: []string{"100.64.0.4/32"},
							},
						},
					},
				)
				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						UUID:     "501-new-UUID",
						Priority: ovntypes.HybridOverlayReroutePriority,
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.1.4"},
						Match:    "inport == \"rtos-node2\" && ip4.src == $" + asv4 + " && ip4.dst != 10.128.0.0/14",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"501-new-UUID"},
					},
					&nbdb.LogicalRouter{
						UUID:  "GR_node1-UUID",
						Name:  "GR_node1",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				err := fakeOvn.controller.delAllLegacyHybridRoutePolicies()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("delete stale addresses from legacy hybrid route policies on startup", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				asIndex := apbroute.GetHybridRouteAddrSetDbIDs("node1", ovntypes.DefaultNetworkControllerName)
				asv4, _ := addressset.GetHashNamesForAS(asIndex)

				node1 := tNode{
					Name:                 "node1",
					NodeIP:               "1.2.3.4",
					NodeLRPMAC:           "0a:58:0a:01:01:01",
					LrpIP:                "100.64.0.2",
					LrpIPv6:              "fd98::2",
					DrLrpIP:              "100.64.0.1",
					PhysicalBridgeMAC:    "11:22:33:44:55:66",
					SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
					NodeSubnet:           "10.1.1.0/24",
					GWRouter:             ovntypes.GWRouterPrefix + "node1",
					GatewayRouterIPMask:  "172.16.16.2/24",
					GatewayRouterIP:      "172.16.16.2",
					GatewayRouterNextHop: "172.16.16.1",
					PhysicalBridgeName:   "br-eth0",
					NodeGWIP:             "10.1.1.1/24",
					NodeMgmtPortIP:       "10.1.1.2",
					NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
					DnatSnatIP:           "169.254.0.1",
				}
				// create a test node and annotate it with host subnet
				testNode := node1.k8sNode("2")

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterStaticRoute{
								UUID:     "static-route-1-UUID",
								IPPrefix: "10.128.1.3/32",
								Nexthop:  "9.0.0.1",
								Options: map[string]string{
									"ecmp_symmetric_reply": "true",
								},
								OutputPort: &logicalRouterPort,
								Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
							},
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-new-UUID",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.0.4"},
								Match:    "inport == \"rtos-node1\" && ip4.src == $" + asv4 + " && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouter{
								Name:     ovntypes.OVNClusterRouter,
								UUID:     ovntypes.OVNClusterRouter + "-UUID",
								Policies: []string{"501-new-UUID"},
							},
							&nbdb.LogicalRouter{
								UUID:         "GR_node1-UUID",
								Name:         "GR_node1",
								Ports:        []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
								StaticRoutes: []string{"static-route-1-UUID"},
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
								Networks: []string{"100.64.0.4/32"},
							},
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							testNode,
						},
					},
				)

				nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: fakeOvn.fakeClient.KubeClient},
					testNode.Name)

				vlanID := uint(1024)
				l3Config := node1.gatewayConfig(config.GatewayModeLocal, vlanID)
				err := util.SetL3GatewayConfig(nodeAnnotator, l3Config)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = nodeAnnotator.Run()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// add address set with one legit IP that exists in a ecmp route, and one that doesn't
				_, err = fakeOvn.asf.NewAddressSet(asIndex, []string{"10.128.1.3", "1.1.1.1"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
					},
					&nbdb.LogicalRouter{
						UUID:  "GR_node1-UUID",
						Name:  "GR_node1",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				err = fakeOvn.controller.apbExternalRouteController.Repair()
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				fakeOvn.asf.EventuallyExpectNoAddressSet(asIndex)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
	ginkgo.Context("external gateway route cleanup on pod and namespace deletion", func() {
		seedRoute := func() ktypes.NamespacedName {
			podNsName := ktypes.NamespacedName{Namespace: namespaceName, Name: "myPod"}
			fakeOvn.startWithDBSetup(
				libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalRouterStaticRoute{
							UUID:       "static-route-1-UUID",
							IPPrefix:   "10.128.1.3/32",
							Nexthop:    "9.0.0.1",
							Options:    map[string]string{"ecmp_symmetric_reply": "true"},
							OutputPort: &logicalRouterPort,
							Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						},
						&nbdb.LogicalRouter{
							UUID:         "GR_node1-UUID",
							Name:         "GR_node1",
							StaticRoutes: []string{"static-route-1-UUID"},
						},
					},
				},
			)
			injectNode(fakeOvn)
			// Seed the shared route cache as if the APB controller had programmed an
			// external-gateway ECMP route for this pod.
			err := fakeOvn.controller.externalGatewayRouteInfo.CreateOrLoad(podNsName, func(routeInfo *apbroute.RouteInfo) error {
				routeInfo.PodExternalRoutes["10.128.1.3"] = map[string]string{"9.0.0.1": "GR_node1"}
				return nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return podNsName
		}
		routeDeletedNB := []libovsdbtest.TestData{
			&nbdb.LogicalRouter{
				UUID:         "GR_node1-UUID",
				Name:         "GR_node1",
				StaticRoutes: []string{},
			},
		}
		ginkgo.It("deletes external gateway ECMP routes for a pod with no matching policy", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				podNsName := seedRoute()
				err := fakeOvn.controller.deleteGWRoutesForPod(podNsName, []*net.IPNet{{IP: net.ParseIP("10.128.1.3")}})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(routeDeletedNB))
				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})
		ginkgo.It("deletes external gateway ECMP routes when the namespace is removed", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				seedRoute()
				err := fakeOvn.controller.deleteGWRoutesForNamespace(namespaceName, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(routeDeletedNB))
				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})
		ginkgo.It("deletes the BFD entry along with the external gateway route on pod deletion", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				podNsName := ktypes.NamespacedName{Namespace: namespaceName, Name: "myPod"}
				bfdUUID := "bfd-1-UUID"
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.BFD{
								UUID:        bfdUUID,
								LogicalPort: "rtoe-GR_node1",
								DstIP:       "9.0.0.1",
							},
							&nbdb.LogicalRouterStaticRoute{
								UUID:       "static-route-1-UUID",
								IPPrefix:   "10.128.1.3/32",
								Nexthop:    "9.0.0.1",
								BFD:        &bfdUUID,
								Options:    map[string]string{"ecmp_symmetric_reply": "true"},
								OutputPort: &logicalRouterPort,
								Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
							},
							&nbdb.LogicalRouter{
								UUID:         "GR_node1-UUID",
								Name:         "GR_node1",
								StaticRoutes: []string{"static-route-1-UUID"},
							},
						},
					},
				)
				injectNode(fakeOvn)
				err := fakeOvn.controller.externalGatewayRouteInfo.CreateOrLoad(podNsName, func(routeInfo *apbroute.RouteInfo) error {
					routeInfo.PodExternalRoutes["10.128.1.3"] = map[string]string{"9.0.0.1": "GR_node1"}
					return nil
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// Both the ECMP route and its now-dangling BFD row must be removed.
				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{},
					},
				}
				err = fakeOvn.controller.deleteGWRoutesForPod(podNsName, []*net.IPNet{{IP: net.ParseIP("10.128.1.3")}})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

	})

	ginkgo.Context("SNAT on gateway router operations", func() {
		ginkgo.It("add/delete SNAT per pod on gateway router", func() {
			app.Action = func(*cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				config.Gateway.DisableSNATMultipleGWs = true

				nodeName := "node1"
				namespaceT := *ovntest.NewNamespace(namespaceName)
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				pod := []corev1.Pod{
					*ovntest.NewPod(t.namespace, t.podName, t.nodeName, t.podIP),
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName,
								Networks: []string{"100.64.0.4/32"},
							},
							&nbdb.LogicalRouter{
								Name:  ovntypes.GWRouterPrefix + nodeName,
								UUID:  ovntypes.GWRouterPrefix + nodeName + "-UUID",
								Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID: "node1",
								Name: "node1",
							},
						},
					},
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							namespaceT,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{
							*newNode("node1", "192.168.126.202/24"),
						},
					},
					&corev1.PodList{
						Items: pod,
					},
				)
				finalNB := []libovsdbtest.TestData{
					&nbdb.NAT{
						UUID:       "nat-UUID",
						ExternalIP: "169.254.33.2",
						LogicalIP:  "10.128.1.3",
						Options:    map[string]string{"stateless": "false"},
						Type:       nbdb.NATTypeSNAT,
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + nodeName,
						UUID:  ovntypes.GWRouterPrefix + nodeName + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName + "-UUID"},
						Nat:   []string{"nat-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName,
						Networks: []string{"100.64.0.4/32"},
					},
					&nbdb.LogicalSwitch{
						UUID: "node1",
						Name: "node1",
					},
				}
				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				extIPs, err := getExternalIPsGR(fakeOvn.controller.watchFactory, pod[0].Spec.NodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, fullMaskPodNet, _ := net.ParseCIDR("10.128.1.3/32")
				gomega.Expect(
					addOrUpdatePodSNAT(fakeOvn.controller.nbClient, util.GetGatewayRouterFromNode(pod[0].Spec.NodeName), extIPs, []*net.IPNet{fullMaskPodNet}),
				).To(gomega.Succeed())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				finalNB = []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + nodeName,
						UUID:  ovntypes.GWRouterPrefix + nodeName + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName + "-UUID"},
						Nat:   []string{},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName,
						Networks: []string{"100.64.0.4/32"},
					},
					&nbdb.LogicalSwitch{
						UUID: "node1",
						Name: "node1",
					},
				}
				err = fakeOvn.controller.deletePodSNAT(nodeName, extIPs, []*net.IPNet{fullMaskPodNet})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})

// injectNode adds a valid node to the nodeinformer so the get
// to understand if there are two bridged won't fail
func injectNode(fakeOvn *FakeOVN) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.254.33.2/24", "next-hop":"169.254.33.1"}}`,
				"k8s.ovn.org/node-chassis-id": chassisIDForNode("node1"),
				"k8s.ovn.org/node-subnets":    `{"default":"10.128.1.0/24"}`,
				util.OvnNodeZoneName:          "node1",
			},
		},
	}
	gomega.ExpectWithOffset(1, fakeOvn.controller.watchFactory.NodeInformer().GetStore().Add(node)).To(gomega.Succeed())
	fakeOvn.controller.localNodes.Store(node.Name, true)
}
