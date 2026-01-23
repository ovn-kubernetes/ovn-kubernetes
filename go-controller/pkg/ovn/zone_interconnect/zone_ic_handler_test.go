package zoneinterconnect

import (
	"fmt"
	"net"
	"sort"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// ovnNodeIDAnnotaton is the node annotation name used to store the node id.
	ovnNodeIDAnnotaton = "k8s.ovn.org/node-id"

	// ovnTransitSwitchPortAddrAnnotation is the node annotation name to store the transit switch port ips.
	ovnTransitSwitchPortAddrAnnotation = "k8s.ovn.org/node-transit-switch-port-ifaddr"

	// ovnNodeZoneNameAnnotation is the node annotation name to store the node zone name.
	ovnNodeZoneNameAnnotation = "k8s.ovn.org/zone-name"

	// ovnNodeChassisIDAnnotatin is the node annotation name to store the node chassis id.
	ovnNodeChassisIDAnnotatin = "k8s.ovn.org/node-chassis-id"

	// ovnNodeSubnetsAnnotation is the node annotation name to store the node subnets.
	ovnNodeSubnetsAnnotation = "k8s.ovn.org/node-subnets"

	// ovnNodeNetworkIDsAnnotation is the node annotation name to store the network ids.
	ovnNodeNetworkIDsAnnotation = "k8s.ovn.org/network-ids"
)

func newClusterJoinSwitch() *nbdb.LogicalSwitch {
	return &nbdb.LogicalSwitch{
		UUID: types.OVNJoinSwitch + "-UUID",
		Name: types.OVNJoinSwitch,
	}
}

func newOVNClusterRouter(netName string) *nbdb.LogicalRouter {
	return &nbdb.LogicalRouter{
		UUID: getNetworkScopedName(netName, types.OVNClusterRouter) + "-UUID",
		Name: getNetworkScopedName(netName, types.OVNClusterRouter),
	}
}

func createTransitSwitchPortBindings(sbClient libovsdbclient.Client, netName string, nodes ...*corev1.Node) error {
	var ports []string
	for _, node := range nodes {
		ports = append(ports, getNetworkScopedName(netName, types.TransitSwitchToRouterPrefix+node.Name))
	}

	return libovsdbtest.CreateTransitSwitchPortBindings(sbClient, netName, ports...)
}

func getNetworkScopedName(netName, name string) string {
	if netName == types.DefaultNetworkName {
		return name
	}
	return fmt.Sprintf("%s%s", util.GetUserDefinedNetworkPrefix(netName), name)
}

func invokeICHandlerAddNodeFunction(zone string, icHandler *ZoneInterconnectHandler, nodes ...*corev1.Node) error {
	for _, node := range nodes {
		if util.GetNodeZone(node) == zone {
			err := icHandler.AddLocalZoneNode(node)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		} else {
			err := icHandler.AddRemoteZoneNode(node)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
	}

	return nil
}

func invokeICHandlerDeleteNodeFunction(icHandler *ZoneInterconnectHandler, nodes ...*corev1.Node) error {
	for _, node := range nodes {
		err := icHandler.DeleteNode(node)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	return nil
}

func checkInterconnectResources(zone string, netName string, nbClient libovsdbclient.Client, testNodesRouteInfo map[string]map[string]string, nodes ...*corev1.Node) error {
	localZoneNodes := []*corev1.Node{}
	remoteZoneNodes := []*corev1.Node{}
	localZoneNodeNames := []string{}
	remoteZoneNodeNames := []string{}
	for _, node := range nodes {
		nodeZone := util.GetNodeZone(node)
		if nodeZone == zone {
			localZoneNodes = append(localZoneNodes, node)
			localZoneNodeNames = append(localZoneNodeNames, node.Name)
		} else {
			remoteZoneNodes = append(remoteZoneNodes, node)
			remoteZoneNodeNames = append(remoteZoneNodeNames, node.Name)
		}

	}

	sort.Strings(localZoneNodeNames)
	sort.Strings(remoteZoneNodeNames)
	// First check if transit switch exists or not
	s := nbdb.LogicalSwitch{
		Name: getNetworkScopedName(netName, types.TransitSwitch),
	}

	ts, err := libovsdbops.GetLogicalSwitch(nbClient, &s)

	if err != nil {
		return fmt.Errorf("could not find transit switch %s in the nb db for network %s : err - %v", s.Name, netName, err)
	}

	noOfTSPorts := len(localZoneNodes) + len(remoteZoneNodes)

	if len(ts.Ports) != noOfTSPorts {
		return fmt.Errorf("transit switch %s doesn't have expected logical ports.  Found %d : Expected %d ports",
			getNetworkScopedName(netName, types.TransitSwitch), len(ts.Ports), noOfTSPorts)
	}
	// Checking just to be sure that the returned switch is infact transit switch.
	if ts.Name != getNetworkScopedName(netName, types.TransitSwitch) {
		return fmt.Errorf("transit switch %s not found in NB DB. Instead found %s", getNetworkScopedName(netName, types.TransitSwitch), ts.Name)
	}

	tsPorts := make([]string, noOfTSPorts)
	i := 0
	for _, p := range ts.Ports {
		lp := nbdb.LogicalSwitchPort{
			UUID: p,
		}

		lsp, err := libovsdbops.GetLogicalSwitchPort(nbClient, &lp)
		if err != nil {
			return fmt.Errorf("could not find logical switch port with uuid %s in the nb db for network %s : err - %v", p, netName, err)
		}
		tsPorts[i] = lsp.Name + ":" + lsp.Type
		i++
	}

	sort.Strings(tsPorts)

	// Verify Transit switch ports.
	// For local nodes, the transit switch port should be of type 'router'
	// and for remote zone nodes, it should be of type 'remote'.
	expectedTsPorts := make([]string, noOfTSPorts)
	i = 0
	for _, node := range localZoneNodes {
		// The logical port for the local zone nodes should be of type patch.
		nodeTSPortName := getNetworkScopedName(netName, types.TransitSwitchToRouterPrefix+node.Name)
		expectedTsPorts[i] = nodeTSPortName + ":router"
		i++
	}

	for _, node := range remoteZoneNodes {
		// The logical port for the local zone nodes should be of type patch.
		nodeTSPortName := getNetworkScopedName(netName, types.TransitSwitchToRouterPrefix+node.Name)
		expectedTsPorts[i] = nodeTSPortName + ":remote"
		i++
	}

	sort.Strings(expectedTsPorts)
	gomega.Expect(tsPorts).To(gomega.Equal(expectedTsPorts))

	r := nbdb.LogicalRouter{
		Name: getNetworkScopedName(netName, types.OVNClusterRouter),
	}

	clusterRouter, err := libovsdbops.GetLogicalRouter(nbClient, &r)
	if err != nil {
		return fmt.Errorf("could not find cluster router %s in the nb db for network %s : err - %v", r.Name, netName, err)
	}

	// Verify that the OVN cluster router ports for each local node
	// connects to the Transit switch.
	icClusterRouterPorts := []string{}
	lrpPrefixName := getNetworkScopedName(netName, types.RouterToTransitSwitchPrefix)
	for _, p := range clusterRouter.Ports {
		lp := nbdb.LogicalRouterPort{
			UUID: p,
		}

		lrp, err := libovsdbops.GetLogicalRouterPort(nbClient, &lp)
		if err != nil {
			return fmt.Errorf("could not find logical router port with uuid %s in the nb db for network %s : err - %v", p, netName, err)
		}

		if lrp.Name[:len(lrpPrefixName)] == lrpPrefixName {
			icClusterRouterPorts = append(icClusterRouterPorts, lrp.Name)
		}
	}

	sort.Strings(icClusterRouterPorts)

	expectedICClusterRouterPorts := []string{}
	for _, node := range localZoneNodes {
		expectedICClusterRouterPorts = append(expectedICClusterRouterPorts, getNetworkScopedName(netName, types.RouterToTransitSwitchPrefix+node.Name))
	}
	sort.Strings(expectedICClusterRouterPorts)

	gomega.Expect(icClusterRouterPorts).To(gomega.Equal(expectedICClusterRouterPorts))

	// Verify the static routes
	expectedStaticRoutes := []string{}

	for _, node := range remoteZoneNodeNames {
		nodeRouteInfo := testNodesRouteInfo[node]
		expectedStaticRoutes = append(expectedStaticRoutes, nodeRouteInfo["node-subnets"]+"-"+nodeRouteInfo["ts-ip"])
		if netName == types.DefaultNetworkName {
			expectedStaticRoutes = append(expectedStaticRoutes, nodeRouteInfo["host-route"]+"-"+nodeRouteInfo["ts-ip"])
		}
	}
	sort.Strings(expectedStaticRoutes)

	clusterRouterStaticRoutes := []string{}
	for _, srUUID := range clusterRouter.StaticRoutes {
		newPredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
			return item.UUID == srUUID
		}
		sr, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(nbClient, newPredicate)
		if err != nil {
			return err
		}

		clusterRouterStaticRoutes = append(clusterRouterStaticRoutes, sr[0].IPPrefix+"-"+sr[0].Nexthop)
	}
	sort.Strings(clusterRouterStaticRoutes)
	gomega.Expect(clusterRouterStaticRoutes).To(gomega.Equal(expectedStaticRoutes))

	return nil
}

var _ = ginkgo.Describe("Zone Interconnect Operations", func() {
	var (
		app                *cli.App
		libovsdbCleanup    *libovsdbtest.Context
		testNode1          corev1.Node
		testNode2          corev1.Node
		testNode3          corev1.Node
		node1Chassis       sbdb.Chassis
		node2Chassis       sbdb.Chassis
		node3Chassis       sbdb.Chassis
		initialNBDB        []libovsdbtest.TestData
		initialSBDB        []libovsdbtest.TestData
		testNodesRouteInfo map[string]map[string]string
		nodeRouteInfoMap   map[string]map[string]map[string]string
	)

	const (
		clusterIPNet   string = "10.1.0.0"
		clusterCIDR    string = clusterIPNet + "/16"
		clusterv6CIDR  string = "aef0::/48"
		joinSubnetCIDR string = "100.64.0.0/16/19"
		vlanID                = 1024
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		libovsdbCleanup = nil

		node1Chassis = sbdb.Chassis{Name: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6", Hostname: "node1", UUID: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"}
		node2Chassis = sbdb.Chassis{Name: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac7", Hostname: "node2", UUID: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac7"}
		node3Chassis = sbdb.Chassis{Name: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac8", Hostname: "node3", UUID: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac8"}

	})

	ginkgo.AfterEach(func() {
		if libovsdbCleanup != nil {
			libovsdbCleanup.Cleanup()
		}
	})

	ginkgo.Context("Default network", func() {
		ginkgo.BeforeEach(func() {
			// node1 is a local zone node
			testNode1 = corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						ovnNodeChassisIDAnnotatin:          "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
						ovnNodeZoneNameAnnotation:          "global",
						ovnNodeIDAnnotaton:                 "2",
						ovnNodeSubnetsAnnotation:           "{\"default\":[\"10.244.2.0/24\"]}",
						ovnTransitSwitchPortAddrAnnotation: "{\"ipv4\":\"100.88.0.2/16\"}",
						ovnNodeNetworkIDsAnnotation:        "{\"default\":\"0\"}",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}},
				},
			}
			// node2 is a local zone node
			testNode2 = corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node2",
					Annotations: map[string]string{
						ovnNodeChassisIDAnnotatin:          "cb9ec8fa-b409-4ef3-9f42-d9283c47aac7",
						ovnNodeZoneNameAnnotation:          "global",
						ovnNodeIDAnnotaton:                 "3",
						ovnNodeSubnetsAnnotation:           "{\"default\":[\"10.244.3.0/24\"]}",
						ovnTransitSwitchPortAddrAnnotation: "{\"ipv4\":\"100.88.0.3/16\"}",
						ovnNodeNetworkIDsAnnotation:        "{\"default\":\"0\"}",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}},
				},
			}
			// node3 is a remote zone node
			testNode3 = corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node3",
					Annotations: map[string]string{
						ovnNodeChassisIDAnnotatin:          "cb9ec8fa-b409-4ef3-9f42-d9283c47aac8",
						ovnNodeZoneNameAnnotation:          "foo",
						ovnNodeIDAnnotaton:                 "4",
						ovnNodeSubnetsAnnotation:           "{\"default\":[\"10.244.4.0/24\"]}",
						ovnTransitSwitchPortAddrAnnotation: "{\"ipv4\":\"100.88.0.4/16\"}",
						ovnNodeNetworkIDsAnnotation:        "{\"default\":\"0\"}",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.12"}},
				},
			}

			testNodesRouteInfo = map[string]map[string]string{
				"node1": {"node-subnets": "10.244.2.0/24", "ts-ip": "100.88.0.2", "host-route": "100.64.0.2/32"},
				"node2": {"node-subnets": "10.244.3.0/24", "ts-ip": "100.88.0.3", "host-route": "100.64.0.3/32"},
				"node3": {"node-subnets": "10.244.4.0/24", "ts-ip": "100.88.0.4", "host-route": "100.64.0.4/32"},
			}
			initialNBDB = []libovsdbtest.TestData{
				newClusterJoinSwitch(),
				newOVNClusterRouter(types.DefaultNetworkName),
			}

			initialSBDB = []libovsdbtest.TestData{
				&node1Chassis, &node2Chassis, &node3Chassis}
		})

		ginkgo.It("Basic checks", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: initialNBDB,
					SBData: initialSBDB,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
				libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = createTransitSwitchPortBindings(libovsdbOvnSBClient, types.DefaultNetworkName, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				zoneICHandler := NewZoneInterconnectHandler(&util.DefaultNetInfo{}, libovsdbOvnNBClient, libovsdbOvnSBClient, nil)
				err = zoneICHandler.createOrUpdateTransitSwitch(0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = invokeICHandlerAddNodeFunction("global", zoneICHandler, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = checkInterconnectResources("global", types.DefaultNetworkName, libovsdbOvnNBClient, testNodesRouteInfo, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"-init-cluster-manager",
				"-zone-join-switch-subnets=" + joinSubnetCIDR,
				"-enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Basic checks in dual-stack", func() {
			app.Action = func(ctx *cli.Context) error {

				dbSetup := libovsdbtest.TestSetup{
					NBData: initialNBDB,
					SBData: initialSBDB,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(config.IPv4Mode).To(gomega.BeTrue())
				gomega.Expect(config.IPv6Mode).To(gomega.BeTrue())
				config.Kubernetes.HostNetworkNamespace = ""

				var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
				libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(libovsdbOvnSBClient.Connected()).To(gomega.BeTrue())
				gomega.Expect(libovsdbOvnNBClient.Connected()).To(gomega.BeTrue())

				err = createTransitSwitchPortBindings(libovsdbOvnSBClient, types.DefaultNetworkName, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				zoneICHandler := NewZoneInterconnectHandler(&util.DefaultNetInfo{}, libovsdbOvnNBClient, libovsdbOvnSBClient, nil)
				err = zoneICHandler.createOrUpdateTransitSwitch(0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = invokeICHandlerAddNodeFunction("global", zoneICHandler, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = checkInterconnectResources("global", types.DefaultNetworkName, libovsdbOvnNBClient, testNodesRouteInfo, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Set annotations to include ipv6 to node3 (remote zone)
				node3Ipv4Subnet := "10.244.4.0/24"
				_, node3Ipv4SubnetNet, err := net.ParseCIDR(node3Ipv4Subnet)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				node3Ipv4SubnetPrefix := node3Ipv4SubnetNet.IP.String() + "/24"
				node3TransitIpv4 := "100.88.0.4"
				node3Ipv6Subnet := "aef0:0:0:4::2895/64"
				_, node3Ipv6SubnetNet, err := net.ParseCIDR(node3Ipv6Subnet)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				node3Ipv6SubnetPrefix := node3Ipv6SubnetNet.IP.String() + "/64"
				node3TransitIpv6 := "fd97::4"

				testNode3.Annotations[ovnNodeSubnetsAnnotation] = "{\"default\":[\"" + node3Ipv4Subnet + "\", \"" + node3Ipv6Subnet + "\"]}"
				testNode3.Annotations[ovnTransitSwitchPortAddrAnnotation] = "{\"ipv4\":\"" + node3TransitIpv4 + "/16\", \"ipv6\":\"" + node3TransitIpv6 + "/64\"}"

				err = invokeICHandlerAddNodeFunction("global", zoneICHandler, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				r := nbdb.LogicalRouter{
					Name: getNetworkScopedName(types.DefaultNetworkName, types.OVNClusterRouter),
				}
				clusterRouter, err := libovsdbops.GetLogicalRouter(libovsdbOvnNBClient, &r)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Make sure cluster router now has an static route for the remote node's ipv4 and ipv6
				staticIpv4RouteFound := false
				staticIpv6RouteFound := false
				for _, srUUID := range clusterRouter.StaticRoutes {
					newPredicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
						return item.UUID == srUUID
					}
					sr, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(libovsdbOvnNBClient, newPredicate)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(sr).Should(gomega.HaveLen(1), "The static route should have been found by uuid")
					if sr[0].IPPrefix == node3Ipv4SubnetPrefix && sr[0].Nexthop == node3TransitIpv4 {
						staticIpv4RouteFound = true
					} else if sr[0].IPPrefix == node3Ipv6SubnetPrefix && sr[0].Nexthop == node3TransitIpv6 {
						staticIpv6RouteFound = true
					}
				}
				gomega.Expect(staticIpv4RouteFound).To(gomega.BeTrue())
				gomega.Expect(staticIpv6RouteFound).To(gomega.BeTrue())
				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR + "," + clusterv6CIDR,
				"-k8s-service-cidr=10.96.0.0/16,fd00:10:96::/112",
				"-init-cluster-manager",
				"-zone-join-switch-subnets=" + joinSubnetCIDR,
				"-enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Change node zones", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: initialNBDB,
					SBData: initialSBDB,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
				libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = createTransitSwitchPortBindings(libovsdbOvnSBClient, types.DefaultNetworkName, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				zoneICHandler := NewZoneInterconnectHandler(&util.DefaultNetInfo{}, libovsdbOvnNBClient, libovsdbOvnSBClient, nil)
				err = zoneICHandler.createOrUpdateTransitSwitch(0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = invokeICHandlerAddNodeFunction("global", zoneICHandler, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = checkInterconnectResources("global", types.DefaultNetworkName, libovsdbOvnNBClient, testNodesRouteInfo, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Change the zone of node2 to a remote zone
				testNode2.Annotations[ovnNodeZoneNameAnnotation] = "bar"
				err = invokeICHandlerAddNodeFunction("global", zoneICHandler, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = checkInterconnectResources("global", types.DefaultNetworkName, libovsdbOvnNBClient, testNodesRouteInfo, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Change the zone of node2 and node3 to global  (no remote zone nodes)
				err = invokeICHandlerAddNodeFunction("global", zoneICHandler, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = checkInterconnectResources("global", types.DefaultNetworkName, libovsdbOvnNBClient, testNodesRouteInfo, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"-init-cluster-manager",
				"-zone-join-switch-subnets=" + joinSubnetCIDR,
				"-enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Sync nodes", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: initialNBDB,
					SBData: initialSBDB,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
				libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = createTransitSwitchPortBindings(libovsdbOvnSBClient, types.DefaultNetworkName, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				zoneICHandler := NewZoneInterconnectHandler(&util.DefaultNetInfo{}, libovsdbOvnNBClient, libovsdbOvnSBClient, nil)
				err = zoneICHandler.createOrUpdateTransitSwitch(0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = invokeICHandlerAddNodeFunction("global", zoneICHandler, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = checkInterconnectResources("global", types.DefaultNetworkName, libovsdbOvnNBClient, testNodesRouteInfo, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Call ICHandler CleanupStaleNodes function removing the testNode3 from the list of nodes
				var kNodes []interface{}
				kNodes = append(kNodes, &testNode1)
				kNodes = append(kNodes, &testNode2)
				err = zoneICHandler.CleanupStaleNodes(kNodes)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = checkInterconnectResources("global", types.DefaultNetworkName, libovsdbOvnNBClient, testNodesRouteInfo, &testNode1, &testNode2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"-init-cluster-manager",
				"-zone-join-switch-subnets=" + joinSubnetCIDR,
				"-enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("CleanupStaleNodes with nil should cleanup all transit switch ports for no-overlay migration", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: initialNBDB,
					SBData: initialSBDB,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
				libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = createTransitSwitchPortBindings(libovsdbOvnSBClient, types.DefaultNetworkName, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				zoneICHandler := NewZoneInterconnectHandler(&util.DefaultNetInfo{}, libovsdbOvnNBClient, libovsdbOvnSBClient, nil)
				gomega.Expect(zoneICHandler).NotTo(gomega.BeNil())

				// Create transit switch and add nodes (simulating previous overlay configuration)
				err = zoneICHandler.createOrUpdateTransitSwitch(0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Set up nodes: testNode1 as local zone, testNode2 and testNode3 as remote zones
				testNode2.Annotations[ovnNodeZoneNameAnnotation] = "remote-zone-1"
				testNode3.Annotations[ovnNodeZoneNameAnnotation] = "remote-zone-2"
				err = invokeICHandlerAddNodeFunction("global", zoneICHandler, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Verify transit switch exists with ports
				ts, err := libovsdbops.GetLogicalSwitch(libovsdbOvnNBClient, &nbdb.LogicalSwitch{Name: types.TransitSwitch})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ts.Ports).NotTo(gomega.BeEmpty(), "Transit switch should have ports before cleanup")

				// Verify IC router ports exist (for local zone node)
				clusterRouter, err := libovsdbops.GetLogicalRouter(libovsdbOvnNBClient, &nbdb.LogicalRouter{Name: types.OVNClusterRouter})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				icRouterPorts := 0
				for _, p := range clusterRouter.Ports {
					lrp, err := libovsdbops.GetLogicalRouterPort(libovsdbOvnNBClient, &nbdb.LogicalRouterPort{UUID: p})
					if err != nil {
						continue
					}
					if len(lrp.Name) >= len(types.RouterToTransitSwitchPrefix) && lrp.Name[:len(types.RouterToTransitSwitchPrefix)] == types.RouterToTransitSwitchPrefix {
						icRouterPorts++
					}
				}
				gomega.Expect(icRouterPorts).To(gomega.Equal(1), "Should have router port for local zone node before cleanup")

				// Verify IC static routes exist (for remote zone nodes)
				p := func(route *nbdb.LogicalRouterStaticRoute) bool {
					return route.ExternalIDs != nil && route.ExternalIDs["ic-node"] != ""
				}
				routes, err := libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(libovsdbOvnNBClient, clusterRouter, p)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(routes).NotTo(gomega.BeEmpty(), "Should have IC static routes for remote zone nodes before cleanup")

				// Call CleanupStaleNodes with nil to simulate no-overlay migration
				// nil means "no current IC nodes", so all nodes become stale and should be cleaned up
				err = zoneICHandler.CleanupStaleNodes(nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Verify all transit switch ports are cleaned up
				ts, err = libovsdbops.GetLogicalSwitch(libovsdbOvnNBClient, &nbdb.LogicalSwitch{Name: types.TransitSwitch})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ts.Ports).To(gomega.BeEmpty(), "Transit switch ports should be cleaned up")

				// Verify all IC router ports are cleaned up (local zone node resources)
				clusterRouter, err = libovsdbops.GetLogicalRouter(libovsdbOvnNBClient, &nbdb.LogicalRouter{Name: types.OVNClusterRouter})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				icRouterPorts = 0
				for _, p := range clusterRouter.Ports {
					lrp, err := libovsdbops.GetLogicalRouterPort(libovsdbOvnNBClient, &nbdb.LogicalRouterPort{UUID: p})
					if err != nil {
						continue
					}
					if len(lrp.Name) >= len(types.RouterToTransitSwitchPrefix) && lrp.Name[:len(types.RouterToTransitSwitchPrefix)] == types.RouterToTransitSwitchPrefix {
						icRouterPorts++
					}
				}
				gomega.Expect(icRouterPorts).To(gomega.Equal(0), "All IC router ports should be cleaned up")

				// Verify all IC static routes are cleaned up (remote zone node resources)
				routes, err = libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(libovsdbOvnNBClient, clusterRouter, p)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(routes).To(gomega.BeEmpty(), "All IC static routes should be cleaned up")

				// Now call Cleanup to remove all interconnect resources (transit switch and any remaining nodes)
				err = zoneICHandler.Cleanup()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Verify transit switch is deleted
				_, err = libovsdbops.GetLogicalSwitch(libovsdbOvnNBClient, &nbdb.LogicalSwitch{Name: types.TransitSwitch})
				gomega.Expect(err).To(gomega.MatchError(libovsdbclient.ErrNotFound))

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"-init-cluster-manager",
				"-zone-join-switch-subnets=" + joinSubnetCIDR,
				"-enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("CleanupStaleNodes with nil should cleanup orphaned IC resources when transit switch doesn't exist", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: initialNBDB,
					SBData: initialSBDB,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
				libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = createTransitSwitchPortBindings(libovsdbOvnSBClient, types.DefaultNetworkName, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				zoneICHandler := NewZoneInterconnectHandler(&util.DefaultNetInfo{}, libovsdbOvnNBClient, libovsdbOvnSBClient, nil)
				gomega.Expect(zoneICHandler).NotTo(gomega.BeNil())

				// Create transit switch and add nodes (simulating previous IC configuration)
				err = zoneICHandler.createOrUpdateTransitSwitch(0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Add testNode1 as local zone, testNode2 and testNode3 as remote zone
				testNode2.Annotations[ovnNodeZoneNameAnnotation] = "remote"
				testNode3.Annotations[ovnNodeZoneNameAnnotation] = "remote"
				err = invokeICHandlerAddNodeFunction("global", zoneICHandler, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Verify IC resources exist
				clusterRouter, err := libovsdbops.GetLogicalRouter(libovsdbOvnNBClient, &nbdb.LogicalRouter{Name: types.OVNClusterRouter})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Count IC router ports (for local zone nodes)
				icRouterPorts := 0
				for _, p := range clusterRouter.Ports {
					lrp, err := libovsdbops.GetLogicalRouterPort(libovsdbOvnNBClient, &nbdb.LogicalRouterPort{UUID: p})
					if err != nil {
						continue
					}
					if len(lrp.Name) >= len(types.RouterToTransitSwitchPrefix) && lrp.Name[:len(types.RouterToTransitSwitchPrefix)] == types.RouterToTransitSwitchPrefix {
						icRouterPorts++
					}
				}
				gomega.Expect(icRouterPorts).To(gomega.Equal(1), "Should have router port for local zone node (node1)")

				// Count IC static routes (for remote zone nodes)
				p := func(route *nbdb.LogicalRouterStaticRoute) bool {
					return route.ExternalIDs != nil && route.ExternalIDs["ic-node"] != ""
				}
				routes, err := libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(libovsdbOvnNBClient, clusterRouter, p)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(routes).ToNot(gomega.BeEmpty(), "Should have IC static routes for remote zone nodes (node2, node3)")

				// Manually delete the transit switch to simulate the resource leak scenario
				// This leaves orphaned router ports and static routes
				err = libovsdbops.DeleteLogicalSwitch(libovsdbOvnNBClient, types.TransitSwitch)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Verify transit switch is gone
				_, err = libovsdbops.GetLogicalSwitch(libovsdbOvnNBClient, &nbdb.LogicalSwitch{Name: types.TransitSwitch})
				gomega.Expect(err).To(gomega.MatchError(libovsdbclient.ErrNotFound))

				// Verify orphaned resources still exist
				clusterRouter, err = libovsdbops.GetLogicalRouter(libovsdbOvnNBClient, &nbdb.LogicalRouter{Name: types.OVNClusterRouter})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				icRouterPorts = 0
				for _, p := range clusterRouter.Ports {
					lrp, err := libovsdbops.GetLogicalRouterPort(libovsdbOvnNBClient, &nbdb.LogicalRouterPort{UUID: p})
					if err != nil {
						continue
					}
					if len(lrp.Name) >= len(types.RouterToTransitSwitchPrefix) && lrp.Name[:len(types.RouterToTransitSwitchPrefix)] == types.RouterToTransitSwitchPrefix {
						icRouterPorts++
					}
				}
				gomega.Expect(icRouterPorts).To(gomega.Equal(1), "Router port should still exist before cleanup (the leak)")

				routes, err = libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(libovsdbOvnNBClient, clusterRouter, p)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(routes).ToNot(gomega.BeEmpty(), "IC static routes should still exist before cleanup (the leak)")

				// Call CleanupStaleNodes with nil - should discover all nodes and clean them
				err = zoneICHandler.CleanupStaleNodes(nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Verify all router ports are cleaned up
				clusterRouter, err = libovsdbops.GetLogicalRouter(libovsdbOvnNBClient, &nbdb.LogicalRouter{Name: types.OVNClusterRouter})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				icRouterPorts = 0
				for _, p := range clusterRouter.Ports {
					lrp, err := libovsdbops.GetLogicalRouterPort(libovsdbOvnNBClient, &nbdb.LogicalRouterPort{UUID: p})
					if err != nil {
						continue
					}
					if len(lrp.Name) >= len(types.RouterToTransitSwitchPrefix) && lrp.Name[:len(types.RouterToTransitSwitchPrefix)] == types.RouterToTransitSwitchPrefix {
						icRouterPorts++
					}
				}
				gomega.Expect(icRouterPorts).To(gomega.Equal(0), "All router ports should be cleaned up")

				// Verify all IC static routes are cleaned up
				routes, err = libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(libovsdbOvnNBClient, clusterRouter, p)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(routes).To(gomega.BeEmpty(), "All IC static routes should be cleaned up")

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"-init-cluster-manager",
				"-zone-join-switch-subnets=" + joinSubnetCIDR,
				"-enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Secondary networks", func() {
		ginkgo.BeforeEach(func() {
			testNode1 = corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						ovnNodeChassisIDAnnotatin:          "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
						ovnNodeZoneNameAnnotation:          "global",
						ovnNodeIDAnnotaton:                 "2",
						ovnNodeSubnetsAnnotation:           "{\"blue\":[\"10.244.2.0/24\"]}",
						ovnTransitSwitchPortAddrAnnotation: "{\"ipv4\":\"100.88.0.2/16\"}",
						ovnNodeNetworkIDsAnnotation:        "{\"blue\":\"1\"}",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}},
				},
			}
			// node2 is a local zone node
			testNode2 = corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node2",
					Annotations: map[string]string{
						ovnNodeChassisIDAnnotatin:          "cb9ec8fa-b409-4ef3-9f42-d9283c47aac7",
						ovnNodeZoneNameAnnotation:          "global",
						ovnNodeIDAnnotaton:                 "3",
						ovnNodeSubnetsAnnotation:           "{\"blue\":[\"10.244.3.0/24\"]}",
						ovnTransitSwitchPortAddrAnnotation: "{\"ipv4\":\"100.88.0.3/16\"}",
						ovnNodeNetworkIDsAnnotation:        "{\"blue\":\"1\"}",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}},
				},
			}
			// node3 is a remote zone node
			testNode3 = corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node3",
					Annotations: map[string]string{
						ovnNodeChassisIDAnnotatin:          "cb9ec8fa-b409-4ef3-9f42-d9283c47aac8",
						ovnNodeZoneNameAnnotation:          "foo",
						ovnNodeIDAnnotaton:                 "4",
						ovnNodeSubnetsAnnotation:           "{\"blue\":[\"10.244.4.0/24\"]}",
						ovnTransitSwitchPortAddrAnnotation: "{\"ipv4\":\"100.88.0.4/16\"}",
						ovnNodeNetworkIDsAnnotation:        "{\"blue\":\"1\"}",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.12"}},
				},
			}
			testNodesRouteInfo = map[string]map[string]string{
				"node1": {"node-subnets": "10.244.2.0/24", "ts-ip": "100.88.0.2", "host-route": "100.64.0.2/32"},
				"node2": {"node-subnets": "10.244.3.0/24", "ts-ip": "100.88.0.3", "host-route": "100.64.0.3/32"},
				"node3": {"node-subnets": "10.244.4.0/24", "ts-ip": "100.88.0.4", "host-route": "100.64.0.4/32"},
			}
			initialNBDB = []libovsdbtest.TestData{
				newOVNClusterRouter("blue"),
			}

			initialSBDB = []libovsdbtest.TestData{
				&node1Chassis, &node2Chassis, &node3Chassis}
		})

		ginkgo.It("Basic checks", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: initialNBDB,
					SBData: initialSBDB,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
				libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = createTransitSwitchPortBindings(libovsdbOvnSBClient, "blue", &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: cnitypes.NetConf{Name: "blue"}, Topology: types.Layer3Topology})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				zoneICHandler := NewZoneInterconnectHandler(netInfo, libovsdbOvnNBClient, libovsdbOvnSBClient, nil)
				err = zoneICHandler.createOrUpdateTransitSwitch(1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = invokeICHandlerAddNodeFunction("global", zoneICHandler, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = checkInterconnectResources("global", "blue", libovsdbOvnNBClient, testNodesRouteInfo, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"-init-cluster-manager",
				"-zone-join-switch-subnets=" + joinSubnetCIDR,
				"-enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Sync nodes", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: initialNBDB,
					SBData: initialSBDB,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
				libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = createTransitSwitchPortBindings(libovsdbOvnSBClient, "blue", &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: cnitypes.NetConf{Name: "blue"}, Topology: types.Layer3Topology})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				zoneICHandler := NewZoneInterconnectHandler(netInfo, libovsdbOvnNBClient, libovsdbOvnSBClient, nil)
				err = zoneICHandler.createOrUpdateTransitSwitch(1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = invokeICHandlerAddNodeFunction("global", zoneICHandler, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = checkInterconnectResources("global", "blue", libovsdbOvnNBClient, testNodesRouteInfo, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Call ICHandler CleanupStaleNodes function removing the testNode3 from the list of nodes
				var kNodes []interface{}
				kNodes = append(kNodes, &testNode1)
				kNodes = append(kNodes, &testNode2)
				err = zoneICHandler.CleanupStaleNodes(kNodes)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = checkInterconnectResources("global", "blue", libovsdbOvnNBClient, testNodesRouteInfo, &testNode1, &testNode2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"-init-cluster-manager",
				"-zone-join-switch-subnets=" + joinSubnetCIDR,
				"-enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Two secondary networks", func() {
		ginkgo.BeforeEach(func() {
			testNode1 = corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						ovnNodeChassisIDAnnotatin:          "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
						ovnNodeZoneNameAnnotation:          "global",
						ovnNodeIDAnnotaton:                 "2",
						ovnNodeSubnetsAnnotation:           "{\"red\":[\"10.244.2.0/24\"], \"blue\":[\"11.244.2.0/24\"]}",
						ovnTransitSwitchPortAddrAnnotation: "{\"ipv4\":\"100.88.0.2/16\"}",
						ovnNodeNetworkIDsAnnotation:        "{\"red\":\"2\", \"blue\":\"1\"}",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}},
				},
			}
			// node2 is a remote zone node
			testNode2 = corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node2",
					Annotations: map[string]string{
						ovnNodeChassisIDAnnotatin:          "cb9ec8fa-b409-4ef3-9f42-d9283c47aac7",
						ovnNodeZoneNameAnnotation:          "foo",
						ovnNodeIDAnnotaton:                 "3",
						ovnNodeSubnetsAnnotation:           "{\"red\":[\"10.244.3.0/24\"], \"blue\":[\"11.244.3.0/24\"]}",
						ovnTransitSwitchPortAddrAnnotation: "{\"ipv4\":\"100.88.0.3/16\"}",
						ovnNodeNetworkIDsAnnotation:        "{\"red\":\"2\", \"blue\":\"1\"}",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}},
				},
			}
			// node3 is a remote zone node
			testNode3 = corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node3",
					Annotations: map[string]string{
						ovnNodeChassisIDAnnotatin:          "cb9ec8fa-b409-4ef3-9f42-d9283c47aac8",
						ovnNodeZoneNameAnnotation:          "foo",
						ovnNodeIDAnnotaton:                 "4",
						ovnNodeSubnetsAnnotation:           "{\"red\":[\"10.244.4.0/24\"], \"blue\":[\"11.244.4.0/24\"]}",
						ovnTransitSwitchPortAddrAnnotation: "{\"ipv4\":\"100.88.0.4/16\"}",
						ovnNodeNetworkIDsAnnotation:        "{\"red\":\"2\", \"blue\":\"1\"}",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.12"}},
				},
			}

			nodeRouteInfoMap = map[string]map[string]map[string]string{
				"red": {
					"node1": {"node-subnets": "10.244.2.0/24", "ts-ip": "100.88.0.2", "host-route": "100.64.0.2/32"},
					"node2": {"node-subnets": "10.244.3.0/24", "ts-ip": "100.88.0.3", "host-route": "100.64.0.3/32"},
					"node3": {"node-subnets": "10.244.4.0/24", "ts-ip": "100.88.0.4", "host-route": "100.64.0.4/32"},
				},
				"blue": {
					"node1": {"node-subnets": "11.244.2.0/24", "ts-ip": "100.88.0.2", "host-route": "100.64.0.2/32"},
					"node2": {"node-subnets": "11.244.3.0/24", "ts-ip": "100.88.0.3", "host-route": "100.64.0.3/32"},
					"node3": {"node-subnets": "11.244.4.0/24", "ts-ip": "100.88.0.4", "host-route": "100.64.0.4/32"},
				},
			}
			initialNBDB = []libovsdbtest.TestData{
				newOVNClusterRouter("blue"),
				newOVNClusterRouter("red"),
			}

			initialSBDB = []libovsdbtest.TestData{
				&node1Chassis, &node2Chassis, &node3Chassis}
		})

		ginkgo.It("Delete remote node", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: initialNBDB,
					SBData: initialSBDB,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
				libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				zoneICHandler := map[string]*ZoneInterconnectHandler{}
				for _, netName := range []string{"red", "blue"} {
					err = createTransitSwitchPortBindings(libovsdbOvnSBClient, netName, &testNode1, &testNode2, &testNode3)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: cnitypes.NetConf{Name: netName}, Topology: types.Layer3Topology})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					zoneICHandler[netName] = NewZoneInterconnectHandler(netInfo, libovsdbOvnNBClient, libovsdbOvnSBClient, nil)
					err = zoneICHandler[netName].createOrUpdateTransitSwitch(1)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = invokeICHandlerAddNodeFunction("global", zoneICHandler[netName], &testNode1, &testNode2, &testNode3)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = checkInterconnectResources("global", netName, libovsdbOvnNBClient, nodeRouteInfoMap[netName], &testNode1, &testNode2, &testNode3)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}

				// Check the logical entities are as expected when a remote node is deleted
				ginkgo.By("Delete remote node \"red\"")
				delete(nodeRouteInfoMap["red"], "node3")
				err = invokeICHandlerDeleteNodeFunction(zoneICHandler["red"], &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = checkInterconnectResources("global", "red", libovsdbOvnNBClient, nodeRouteInfoMap["red"], &testNode1, &testNode2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = checkInterconnectResources("global", "blue", libovsdbOvnNBClient, nodeRouteInfoMap["blue"], &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"-init-cluster-manager",
				"-zone-join-switch-subnets=" + joinSubnetCIDR,
				"-enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Error scenarios", func() {
		ginkgo.BeforeEach(func() {
			initialNBDB = []libovsdbtest.TestData{}
			initialSBDB = []libovsdbtest.TestData{}
		})

		ginkgo.It("Missing annotations and error scenarios for local node", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: initialNBDB,
					SBData: initialSBDB,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
				libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				testNode4 := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node4",
					},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}},
					},
				}

				err = createTransitSwitchPortBindings(libovsdbOvnSBClient, types.DefaultNetworkName, &testNode1, &testNode2, &testNode3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				zoneICHandler := NewZoneInterconnectHandler(&util.DefaultNetInfo{}, libovsdbOvnNBClient, libovsdbOvnSBClient, nil)
				gomega.Expect(zoneICHandler).NotTo(gomega.BeNil())

				err = zoneICHandler.createOrUpdateTransitSwitch(0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = zoneICHandler.AddLocalZoneNode(&testNode4)
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("failed to get node id for node - node4")))

				// Set the node id
				testNode4.Annotations = map[string]string{ovnNodeIDAnnotaton: "5"}
				err = zoneICHandler.AddLocalZoneNode(&testNode4)
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("failed to get the node transit switch port ips for node node4")))

				// Set the node transit switch port ips
				testNode4.Annotations[ovnTransitSwitchPortAddrAnnotation] = "{\"ipv4\":\"100.88.0.5/16\"}"
				err = zoneICHandler.AddLocalZoneNode(&testNode4)
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("failed to create/update cluster router ovn_cluster_router to add transit switch port rtots-node4 for the node node4")))

				// Create the cluster router
				r := newOVNClusterRouter(types.DefaultNetworkName)
				err = libovsdbops.CreateOrUpdateLogicalRouter(libovsdbOvnNBClient, r)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = zoneICHandler.AddLocalZoneNode(&testNode4)
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("failed to parse node node4 subnets annotation")))

				// Set node subnet annotation
				testNode4.Annotations[ovnNodeSubnetsAnnotation] = "{\"default\":[\"10.244.5.0/24\"]}"

				err = zoneICHandler.AddLocalZoneNode(&testNode4)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				testNodesRouteInfo = map[string]map[string]string{
					"node4": {"node-subnets": "10.244.5.0/24", "ts-ip": "100.88.0.5", "host-route": "100.64.0.5/32"},
				}
				err = checkInterconnectResources("global", types.DefaultNetworkName, libovsdbOvnNBClient, testNodesRouteInfo, &testNode4)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"-init-cluster-manager",
				"-zone-join-switch-subnets=" + joinSubnetCIDR,
				"-enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Missing annotations and error scenarios for remote node", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: initialNBDB,
					SBData: initialSBDB,
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.Kubernetes.HostNetworkNamespace = ""

				var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client
				libovsdbOvnNBClient, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				testNode4 := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node4",
						Annotations: map[string]string{
							ovnNodeZoneNameAnnotation: "foo",
						},
					},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}},
					},
				}

				err = createTransitSwitchPortBindings(libovsdbOvnSBClient, types.DefaultNetworkName, &testNode1, &testNode2, &testNode3, &testNode4)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				zoneICHandler := NewZoneInterconnectHandler(&util.DefaultNetInfo{}, libovsdbOvnNBClient, libovsdbOvnSBClient, nil)
				gomega.Expect(zoneICHandler).NotTo(gomega.BeNil())

				err = zoneICHandler.createOrUpdateTransitSwitch(0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = zoneICHandler.AddRemoteZoneNode(&testNode4)
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("failed to get node id for node - node4")))

				// Set the node id
				testNode4.Annotations[ovnNodeIDAnnotaton] = "5"
				err = zoneICHandler.AddRemoteZoneNode(&testNode4)
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("failed to parse node node4 subnets annotation")))

				// Set node subnet annotation
				testNode4.Annotations[ovnNodeSubnetsAnnotation] = "{\"default\":[\"10.244.5.0/24\"]}"
				err = zoneICHandler.AddRemoteZoneNode(&testNode4)
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("failed to get the node transit switch port IP addresses")))

				// Set the node transit switch port ips
				testNode4.Annotations[ovnTransitSwitchPortAddrAnnotation] = "{\"ipv4\":\"100.88.0.5/16\"}"
				err = zoneICHandler.AddRemoteZoneNode(&testNode4)
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("failed to create static route ops: unable to get logical router static routes with predicate on router ovn_cluster_router")))

				// Create the cluster router
				r := newOVNClusterRouter(types.DefaultNetworkName)
				err = libovsdbops.CreateOrUpdateLogicalRouter(libovsdbOvnNBClient, r)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = zoneICHandler.AddRemoteZoneNode(&testNode4)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				testNodesRouteInfo = map[string]map[string]string{
					"node4": {"node-subnets": "10.244.5.0/24", "ts-ip": "100.88.0.5", "host-route": "100.64.0.5/32"},
				}
				err = checkInterconnectResources("global", types.DefaultNetworkName, libovsdbOvnNBClient, testNodesRouteInfo, &testNode4)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-cluster-subnets=" + clusterCIDR,
				"-init-cluster-manager",
				"-zone-join-switch-subnets=" + joinSubnetCIDR,
				"-enable-interconnect",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})
})
