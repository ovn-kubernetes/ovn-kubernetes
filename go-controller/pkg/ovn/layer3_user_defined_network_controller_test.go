package ovn

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	knet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	zoneic "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/zone_interconnect"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type userDefinedNetInfo struct {
	netName            string
	nadName            string
	clustersubnets     string
	hostsubnets        string // not used in layer2 tests
	topology           string
	isPrimary          bool
	allowPersistentIPs bool
	ipamClaimReference string
}

const (
	nadName                = "blue-net"
	ns                     = "namespace1"
	userDefinedNetworkName = "isolatednet"
	userDefinedNetworkID   = "2"
	denyPolicyName         = "deny-all-policy"
	denyPG                 = "deny-port-group"
)

type testConfiguration struct {
	configToOverride   *config.OVNKubernetesFeatureConfig
	gatewayConfig      *config.GatewayConfig
	expectationOptions []option
}

var _ = Describe("OVN Multi-Homed pod operations for layer 3 network", func() {
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdbtest.TestSetup
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed()) // reset defaults

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true)
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: nodeName,
				},
			},
		}

		config.OVNKubernetesFeature = *minimalFeatureConfig()
		config.Gateway.V4MasqueradeSubnet = dummyMasqueradeSubnet().String()
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	DescribeTable(
		"reconciles a new",
		func(netInfo userDefinedNetInfo, testConfig testConfiguration, gwMode config.GatewayMode) {
			podInfo := dummyTestPod(ns, netInfo)
			if testConfig.configToOverride != nil {
				config.OVNKubernetesFeature = *testConfig.configToOverride
				if testConfig.gatewayConfig != nil {
					config.Gateway.DisableSNATMultipleGWs = testConfig.gatewayConfig.DisableSNATMultipleGWs
				}
			}
			config.Gateway.Mode = gwMode
			if knet.IsIPv6CIDRString(netInfo.clustersubnets) {
				config.IPv6Mode = true
				// tests dont support dualstack yet
				config.IPv4Mode = false
			}
			app.Action = func(*cli.Context) error {
				nad, err := newNetworkAttachmentDefinition(
					ns,
					nadName,
					*netInfo.netconf(),
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(netInfo.setupOVNDependencies(&initialDB)).To(Succeed())
				n := newNamespace(ns)
				if netInfo.isPrimary {
					n = newUDNNamespace(ns)
					networkConfig, err := util.NewNetInfo(netInfo.netconf())
					Expect(err).NotTo(HaveOccurred())
					initialDB.NBData = append(
						initialDB.NBData,
						&nbdb.LogicalSwitch{
							Name:        fmt.Sprintf("%s_join", netInfo.netName),
							ExternalIDs: standardNonDefaultNetworkExtIDs(networkConfig),
						},
						&nbdb.LogicalRouter{
							Name:        fmt.Sprintf("%s_ovn_cluster_router", netInfo.netName),
							ExternalIDs: standardNonDefaultNetworkExtIDs(networkConfig),
						},
						&nbdb.LogicalRouterPort{
							Name: fmt.Sprintf("rtos-%s_%s", netInfo.netName, nodeName),
						},
					)
					initialDB.NBData = append(initialDB.NBData, getHairpinningACLsV4AndPortGroup()...)
					initialDB.NBData = append(initialDB.NBData, getHairpinningACLsV4AndPortGroupForNetwork(networkConfig, nil)...)
				}

				const nodeIPv4CIDR = "192.168.126.202/24"
				testNode, err := newNodeWithUserDefinedNetworks(nodeName, nodeIPv4CIDR, netInfo)
				Expect(err).NotTo(HaveOccurred())
				networkPolicy := getMatchLabelsNetworkPolicy(denyPolicyName, ns, "", "", false, false)
				fakeOvn.startWithDBSetup(
					initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							*n,
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{*testNode},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*newMultiHomedPod(podInfo, netInfo),
						},
					},
					&nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					},
					&networkingv1.NetworkPolicyList{
						Items: []networkingv1.NetworkPolicy{*networkPolicy},
					},
				)
				podInfo.populateLogicalSwitchCache(fakeOvn)

				// pod exists, networks annotations don't
				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.Background(), podInfo.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				Expect(fakeOvn.networkManager.Start()).NotTo(HaveOccurred())
				defer fakeOvn.networkManager.Stop()

				Expect(fakeOvn.controller.WatchNamespaces()).NotTo(HaveOccurred())
				Expect(fakeOvn.controller.WatchPods()).NotTo(HaveOccurred())
				if netInfo.isPrimary {
					Expect(fakeOvn.controller.WatchNetworkPolicy()).NotTo(HaveOccurred())
				}
				userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
				Expect(ok).To(BeTrue())

				userDefinedNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
				podInfo.populateUserDefinedNetworkLogicalSwitchCache(userDefinedNetController)
				Expect(userDefinedNetController.bnc.WatchNodes()).To(Succeed())
				Expect(userDefinedNetController.bnc.WatchPods()).To(Succeed())

				if netInfo.isPrimary {
					Expect(userDefinedNetController.bnc.WatchNetworkPolicy()).To(Succeed())
					ninfo, err := fakeOvn.networkManager.Interface().GetActiveNetworkForNamespace(ns)
					Expect(err).NotTo(HaveOccurred())
					Expect(ninfo.GetNetworkName()).To(Equal(netInfo.netName))
				}

				// check that after start networks annotations and nbdb will be updated
				Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, podInfo.namespace, podInfo.podName)
				}).WithTimeout(2 * time.Second).Should(MatchJSON(podInfo.getAnnotationsJson()))

				defaultNetExpectations := getDefaultNetExpectedPodsAndSwitches([]testPod{podInfo}, []string{nodeName})
				expectationOptions := testConfig.expectationOptions
				if netInfo.isPrimary {
					defaultNetExpectations = emptyDefaultClusterNetworkNodeSwitch(podInfo.nodeName)
					gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
					Expect(err).NotTo(HaveOccurred())
					Expect(gwConfig.NextHops).NotTo(BeEmpty())
					expectationOptions = append(expectationOptions, withGatewayConfig(gwConfig))
					if testConfig.configToOverride != nil && testConfig.configToOverride.EnableEgressFirewall {
						defaultNetExpectations = append(defaultNetExpectations,
							buildNamespacedPortGroup(podInfo.namespace, DefaultNetworkControllerName))
						secNetPG := buildNamespacedPortGroup(podInfo.namespace, userDefinedNetController.bnc.controllerName)
						portName := util.GetUserDefinedNetworkLogicalPortName(podInfo.namespace, podInfo.podName, netInfo.nadName) + "-UUID"
						secNetPG.Ports = []string{portName}
						defaultNetExpectations = append(defaultNetExpectations, secNetPG)
					}
					networkConfig, err := util.NewNetInfo(netInfo.netconf())
					Expect(err).NotTo(HaveOccurred())
					// Add NetPol hairpin ACLs and PGs for the validation.
					mgmtPortName := managementPortName(userDefinedNetController.bnc.GetNetworkScopedName(nodeName))
					mgmtPortUUID := mgmtPortName + "-UUID"
					defaultNetExpectations = append(defaultNetExpectations, getHairpinningACLsV4AndPortGroup()...)
					defaultNetExpectations = append(defaultNetExpectations, getHairpinningACLsV4AndPortGroupForNetwork(networkConfig,
						[]string{mgmtPortUUID})...)
					// Add Netpol deny policy ACLs and PGs for the validation.
					podLPortName := util.GetUserDefinedNetworkLogicalPortName(podInfo.namespace, podInfo.podName, netInfo.nadName) + "-UUID"
					dataParams := newNetpolDataParams(networkPolicy).withLocalPortUUIDs(podLPortName).withNetInfo(networkConfig)
					defaultDenyExpectedData := getDefaultDenyData(dataParams)
					pgDbIDs := getNetworkPolicyPortGroupDbIDs(ns, userDefinedNetController.bnc.controllerName, denyPolicyName)
					ingressPG := libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil)
					ingressPG.UUID = denyPG
					ingressPG.Ports = []string{podLPortName}
					defaultNetExpectations = append(defaultNetExpectations, ingressPG)
					defaultNetExpectations = append(defaultNetExpectations, defaultDenyExpectedData...)
				}
				Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(
						append(
							defaultNetExpectations,
							newUserDefinedNetworkExpectationMachine(
								fakeOvn,
								[]testPod{podInfo},
								expectationOptions...,
							).expectedLogicalSwitchesAndPorts(netInfo.isPrimary)...)))

				return nil
			}

			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		Entry("pod on a user defined secondary network",
			dummySecondaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			nonICClusterTestConfiguration(),
			config.GatewayModeShared,
		),
		Entry("pod on a user defined primary network",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			nonICClusterTestConfiguration(),
			config.GatewayModeShared,
		),
		Entry("pod on a user defined secondary network on an IC cluster",
			dummySecondaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(),
			config.GatewayModeShared,
		),
		Entry("pod on a user defined primary network on an IC cluster",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(),
			config.GatewayModeShared,
		),
		Entry("pod on a user defined primary network on an IC cluster; LGW",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(),
			config.GatewayModeLocal,
		),
		Entry("pod on a user defined primary network on an IC cluster with per-pod SNATs enabled",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(func(testConfig *testConfiguration) {
				testConfig.gatewayConfig = &config.GatewayConfig{DisableSNATMultipleGWs: true}
			}),
			config.GatewayModeShared,
		),
		Entry("pod on a user defined primary network on an IC cluster with EgressFirewall enabled",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(func(config *testConfiguration) {
				config.configToOverride.EnableEgressFirewall = true
			}),
			config.GatewayModeShared,
		),
	)

	DescribeTable(
		"the gateway is properly cleaned up",
		func(netInfo userDefinedNetInfo, testConfig testConfiguration) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			podInfo := dummyTestPod(ns, netInfo)
			if testConfig.configToOverride != nil {
				config.OVNKubernetesFeature = *testConfig.configToOverride
				if testConfig.gatewayConfig != nil {
					config.Gateway.DisableSNATMultipleGWs = testConfig.gatewayConfig.DisableSNATMultipleGWs
				}
			}
			app.Action = func(ctx *cli.Context) error {
				netConf := netInfo.netconf()
				networkConfig, err := util.NewNetInfo(netConf)
				Expect(err).NotTo(HaveOccurred())

				nad, err := newNetworkAttachmentDefinition(
					ns,
					nadName,
					*netConf,
				)
				Expect(err).NotTo(HaveOccurred())

				mutableNetworkConfig := util.NewMutableNetInfo(networkConfig)
				mutableNetworkConfig.SetNADs(util.GetNADName(nad.Namespace, nad.Name))
				networkConfig = mutableNetworkConfig

				fakeNetworkManager := &networkmanager.FakeNetworkManager{
					PrimaryNetworks: make(map[string]util.NetInfo),
				}
				fakeNetworkManager.PrimaryNetworks[ns] = networkConfig

				const nodeIPv4CIDR = "192.168.126.202/24"
				testNode, err := newNodeWithUserDefinedNetworks(nodeName, nodeIPv4CIDR, netInfo)
				Expect(err).NotTo(HaveOccurred())

				nbZone := &nbdb.NBGlobal{Name: types.OvnDefaultZone, UUID: types.OvnDefaultZone}
				defaultNetExpectations := emptyDefaultClusterNetworkNodeSwitch(podInfo.nodeName)
				defaultNetExpectations = append(defaultNetExpectations, nbZone)
				gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
				Expect(err).NotTo(HaveOccurred())
				Expect(gwConfig.NextHops).NotTo(BeEmpty())

				if netInfo.isPrimary {
					gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
					Expect(err).NotTo(HaveOccurred())
					initialDB.NBData = append(
						initialDB.NBData,
						expectedGWEntities(podInfo.nodeName, networkConfig, *gwConfig)...)
					initialDB.NBData = append(
						initialDB.NBData,
						expectedLayer3EgressEntities(networkConfig, *gwConfig, testing.MustParseIPNet(netInfo.hostsubnets))...)
					initialDB.NBData = append(initialDB.NBData,
						newNetworkClusterPortGroup(networkConfig),
					)
					if testConfig.configToOverride != nil && testConfig.configToOverride.EnableEgressFirewall {
						defaultNetExpectations = append(defaultNetExpectations,
							buildNamespacedPortGroup(podInfo.namespace, DefaultNetworkControllerName))
					}
				}
				initialDB.NBData = append(initialDB.NBData, nbZone)

				fakeOvn.startWithDBSetup(
					initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							*newUDNNamespace(ns),
						},
					},
					&corev1.NodeList{
						Items: []corev1.Node{*testNode},
					},
					&corev1.PodList{
						Items: []corev1.Pod{
							*newMultiHomedPod(podInfo, netInfo),
						},
					},
					&nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					},
				)

				Expect(netInfo.setupOVNDependencies(&initialDB)).To(Succeed())

				podInfo.populateLogicalSwitchCache(fakeOvn)

				// pod exists, networks annotations don't
				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.Background(), podInfo.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				Expect(fakeOvn.networkManager.Start()).NotTo(HaveOccurred())
				defer fakeOvn.networkManager.Stop()

				Expect(fakeOvn.controller.WatchNamespaces()).To(Succeed())
				Expect(fakeOvn.controller.WatchPods()).To(Succeed())
				userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
				Expect(ok).To(BeTrue())

				userDefinedNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
				podInfo.populateUserDefinedNetworkLogicalSwitchCache(userDefinedNetController)
				Expect(userDefinedNetController.bnc.WatchNodes()).To(Succeed())
				Expect(userDefinedNetController.bnc.WatchPods()).To(Succeed())

				if netInfo.isPrimary {
					Expect(userDefinedNetController.bnc.WatchNetworkPolicy()).To(Succeed())
				}

				Expect(fakeOvn.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
				Expect(fakeOvn.fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Delete(context.Background(), nad.Name, metav1.DeleteOptions{})).To(Succeed())

				// we must access the layer3 controller to be able to issue its cleanup function (to remove the GW related stuff).
				Expect(
					newLayer3UserDefinedNetworkController(
						&userDefinedNetController.bnc.CommonNetworkControllerInfo,
						networkConfig,
						nodeName,
						fakeNetworkManager,
						nil,
						NewPortCache(ctx.Done()),
					).Cleanup()).To(Succeed())
				Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(defaultNetExpectations))

				return nil
			}
			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		Entry("pod on a user defined primary network",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			nonICClusterTestConfiguration(),
		),
		Entry("pod on a user defined primary network on an IC cluster",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(),
		),
		Entry("pod on a user defined primary network on an IC cluster with per-pod SNATs enabled",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(func(testConfig *testConfiguration) {
				testConfig.gatewayConfig = &config.GatewayConfig{DisableSNATMultipleGWs: true}
			}),
		),
		Entry("pod on a user defined primary network on an IC cluster with EgressFirewall enabled",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(func(config *testConfiguration) {
				config.configToOverride.EnableEgressFirewall = true
			}),
		),
	)
})

var _ = Describe("Layer3 UDN Transport Mode - Interconnect", func() {
	const (
		node1Name  = "node1"
		node2Name  = "node2"
		udnName    = "test-udn"
		udnNadName = "default/test-nad"
		zone1      = "global"
		zone2      = "remote"
	)

	var (
		app       *cli.App
		initialDB libovsdbtest.TestSetup
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableInterconnect = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{},
			SBData: []libovsdbtest.TestData{},
		}
	})

	createUDNNetConf := func(transport string) *ovncnitypes.NetConf {
		return &ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				Name: udnName,
				Type: "ovn-k8s-cni-overlay",
			},
			Topology:  types.Layer3Topology,
			NADName:   udnNadName,
			Subnets:   "192.168.0.0/16",
			Role:      types.NetworkRolePrimary,
			Transport: transport,
		}
	}

	createTestNode := func(nodeName, zone string, nodeID string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets":                    `{"default":"10.128.1.0/24","` + udnName + `":"192.168.1.0/24"}`,
					"k8s.ovn.org/zone-name":                       zone,
					"k8s.ovn.org/node-id":                         nodeID,
					"k8s.ovn.org/node-chassis-id":                 "test-chassis-" + nodeName,
					"k8s.ovn.org/node-transit-switch-port-ifaddr": `{"ipv4":"100.88.0.1/16"}`,
				},
			},
		}
	}

	Context("with no-overlay transport", func() {
		It("should not create interconnect resources when addUpdateLocalNodeEvent is called", func() {
			app.Action = func(ctx *cli.Context) error {
				netConf := createUDNNetConf(types.NetworkTransportNoOverlay)
				netInfo, err := util.NewNetInfo(netConf)
				Expect(err).NotTo(HaveOccurred())
				Expect(netInfo.GetNetworkTransport()).To(Equal(config.TransportNoOverlay))

				node1 := createTestNode(node1Name, zone1, "1")
				clusterRouterName := netInfo.GetNetworkScopedClusterRouterName()
				transitSwitchName := netInfo.GetNetworkScopedName(types.TransitSwitch)

				// Setup NBDB with cluster router
				initialDB.NBData = []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name: clusterRouterName,
						UUID: clusterRouterName + "-UUID",
						ExternalIDs: map[string]string{
							types.NetworkExternalID:  udnName,
							types.TopologyExternalID: types.Layer3Topology,
						},
					},
					&nbdb.LogicalSwitch{
						Name: netInfo.GetNetworkScopedName(node1Name),
						UUID: netInfo.GetNetworkScopedName(node1Name) + "-UUID",
						ExternalIDs: map[string]string{
							types.NetworkExternalID:     udnName,
							types.NetworkRoleExternalID: types.NetworkRolePrimary,
						},
						OtherConfig: map[string]string{"subnet": "192.168.1.0/24"},
					},
				}

				nbClient, sbClient, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(initialDB)
				Expect(err).NotTo(HaveOccurred())
				defer libovsdbCleanup.Cleanup()

				// Create the controller
				watcher := &factory.WatchFactory{}
				zoneICHandler := zoneic.NewZoneInterconnectHandler(netInfo, nbClient, sbClient, watcher)

				cnci := &CommonNetworkControllerInfo{
					nbClient:     nbClient,
					sbClient:     sbClient,
					watchFactory: watcher,
				}

				controller := &Layer3UserDefinedNetworkController{
					BaseUserDefinedNetworkController: BaseUserDefinedNetworkController{
						BaseNetworkController: BaseNetworkController{
							CommonNetworkControllerInfo: *cnci,
							ReconcilableNetInfo:         util.NewReconcilableNetInfo(netInfo),
							zoneICHandler:               zoneICHandler,
						localZoneNodes:              &sync.Map{},
						},
					},
				}

				// Call addUpdateLocalNodeEvent with syncZoneIC=true
				nSyncs := &nodeSyncs{syncZoneIC: true}
				err = controller.addUpdateLocalNodeEvent(node1, nSyncs)
				// Error is expected because we don't have full node setup, but the IC check runs first
				// The important part is verifying no IC resources were created

				// Verify no transit switch was created
				ts := &nbdb.LogicalSwitch{Name: transitSwitchName}
				_, err = libovsdbops.GetLogicalSwitch(nbClient, ts)
				Expect(err).To(HaveOccurred(), "Transit switch should not exist for no-overlay transport")

				// Verify no transit switch port was created
				transitSwitchPortName := netInfo.GetNetworkScopedName(types.TransitSwitchToRouterPrefix + node1Name)
				ports, err := libovsdbops.FindLogicalSwitchPortWithPredicate(nbClient, func(lsp *nbdb.LogicalSwitchPort) bool {
					return lsp.Name == transitSwitchPortName
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(ports).To(BeEmpty(), "No transit switch port should exist for no-overlay transport")

				// Verify no router port was created
				routerPortName := netInfo.GetNetworkScopedName(types.RouterToTransitSwitchPrefix + node1Name)
				rports, err := libovsdbops.FindLogicalRouterPortWithPredicate(nbClient, func(lrp *nbdb.LogicalRouterPort) bool {
					return lrp.Name == routerPortName
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(rports).To(BeEmpty(), "No router port should exist for no-overlay transport")

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not create interconnect resources when addUpdateRemoteNodeEvent is called", func() {
			app.Action = func(ctx *cli.Context) error {
				netConf := createUDNNetConf(types.NetworkTransportNoOverlay)
				netInfo, err := util.NewNetInfo(netConf)
				Expect(err).NotTo(HaveOccurred())
				Expect(netInfo.GetNetworkTransport()).To(Equal(config.TransportNoOverlay))

				node2 := createTestNode(node2Name, zone2, "2")
				clusterRouterName := netInfo.GetNetworkScopedClusterRouterName()
				transitSwitchName := netInfo.GetNetworkScopedName(types.TransitSwitch)

				// Setup NBDB with cluster router
				initialDB.NBData = []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name: clusterRouterName,
						UUID: clusterRouterName + "-UUID",
						ExternalIDs: map[string]string{
							types.NetworkExternalID:  udnName,
							types.TopologyExternalID: types.Layer3Topology,
						},
					},
				}

				nbClient, sbClient, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(initialDB)
				Expect(err).NotTo(HaveOccurred())
				defer libovsdbCleanup.Cleanup()

				// Create the controller
				watcher := &factory.WatchFactory{}
				zoneICHandler := zoneic.NewZoneInterconnectHandler(netInfo, nbClient, sbClient, watcher)

				cnci := &CommonNetworkControllerInfo{
					nbClient:     nbClient,
					sbClient:     sbClient,
					watchFactory: watcher,
				}

				controller := &Layer3UserDefinedNetworkController{
					BaseUserDefinedNetworkController: BaseUserDefinedNetworkController{
						BaseNetworkController: BaseNetworkController{
							CommonNetworkControllerInfo: *cnci,
							ReconcilableNetInfo:         util.NewReconcilableNetInfo(netInfo),
							zoneICHandler:               zoneICHandler,
						localZoneNodes:              &sync.Map{},
						},
					},
				}

				// Call addUpdateRemoteNodeEvent with syncZoneIc=true
				err = controller.addUpdateRemoteNodeEvent(node2, true)
				Expect(err).NotTo(HaveOccurred(), "addUpdateRemoteNodeEvent should succeed for no-overlay (skips IC operations)")

				// Verify no transit switch was created
				ts := &nbdb.LogicalSwitch{Name: transitSwitchName}
				_, err = libovsdbops.GetLogicalSwitch(nbClient, ts)
				Expect(err).To(HaveOccurred(), "Transit switch should not exist for no-overlay transport")

				// Verify no remote ports were created
				remotePortName := netInfo.GetNetworkScopedName(types.TransitSwitchToRouterPrefix + node2Name)
				ports, err := libovsdbops.FindLogicalSwitchPortWithPredicate(nbClient, func(lsp *nbdb.LogicalSwitchPort) bool {
					return lsp.Name == remotePortName
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(ports).To(BeEmpty(), "No remote port should exist for no-overlay transport")

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("with overlay transport", func() {
		It("should create interconnect resources when addUpdateLocalNodeEvent is called", func() {
			app.Action = func(ctx *cli.Context) error {
				// Empty transport = overlay/geneve
				netConf := createUDNNetConf("")
				netInfo, err := util.NewNetInfo(netConf)
				Expect(err).NotTo(HaveOccurred())
				Expect(netInfo.GetNetworkTransport()).NotTo(Equal(config.TransportNoOverlay))

				node1 := createTestNode(node1Name, zone1, "1")
				clusterRouterName := netInfo.GetNetworkScopedClusterRouterName()
				transitSwitchName := netInfo.GetNetworkScopedName(types.TransitSwitch)

				// Setup NBDB with cluster router and transit switch
				initialDB.NBData = []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name: clusterRouterName,
						UUID: clusterRouterName + "-UUID",
						ExternalIDs: map[string]string{
							types.NetworkExternalID:  udnName,
							types.TopologyExternalID: types.Layer3Topology,
						},
					},
					&nbdb.LogicalSwitch{
						Name: netInfo.GetNetworkScopedName(node1Name),
						UUID: netInfo.GetNetworkScopedName(node1Name) + "-UUID",
						ExternalIDs: map[string]string{
							types.NetworkExternalID:     udnName,
							types.NetworkRoleExternalID: types.NetworkRolePrimary,
						},
						OtherConfig: map[string]string{"subnet": "192.168.1.0/24"},
					},
					// Pre-create transit switch for overlay
					&nbdb.LogicalSwitch{
						Name: transitSwitchName,
						UUID: transitSwitchName + "-UUID",
						ExternalIDs: map[string]string{
							types.NetworkExternalID: udnName,
						},
					},
				}

				nbClient, sbClient, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(initialDB)
				Expect(err).NotTo(HaveOccurred())
				defer libovsdbCleanup.Cleanup()

				// Create the controller
				watcher := &factory.WatchFactory{}
				zoneICHandler := zoneic.NewZoneInterconnectHandler(netInfo, nbClient, sbClient, watcher)

				cnci := &CommonNetworkControllerInfo{
					nbClient:     nbClient,
					sbClient:     sbClient,
					watchFactory: watcher,
				}

				controller := &Layer3UserDefinedNetworkController{
					BaseUserDefinedNetworkController: BaseUserDefinedNetworkController{
						BaseNetworkController: BaseNetworkController{
							CommonNetworkControllerInfo: *cnci,
							ReconcilableNetInfo:         util.NewReconcilableNetInfo(netInfo),
							zoneICHandler:               zoneICHandler,
						localZoneNodes:              &sync.Map{},
						},
					},
				}

				// Call addUpdateLocalNodeEvent with syncZoneIC=true
				nSyncs := &nodeSyncs{syncZoneIC: true}
				err = controller.addUpdateLocalNodeEvent(node1, nSyncs)
				// Error may occur due to incomplete setup, but IC operations should have been attempted

				// Verify transit switch still exists
				ts := &nbdb.LogicalSwitch{Name: transitSwitchName}
				ts, err = libovsdbops.GetLogicalSwitch(nbClient, ts)
				Expect(err).NotTo(HaveOccurred(), "Transit switch should exist for overlay transport")

				// Verify transit switch port was created
				transitSwitchPortName := netInfo.GetNetworkScopedName(types.TransitSwitchToRouterPrefix + node1Name)
				ports, err := libovsdbops.FindLogicalSwitchPortWithPredicate(nbClient, func(lsp *nbdb.LogicalSwitchPort) bool {
					return lsp.Name == transitSwitchPortName
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(ports).To(HaveLen(1), "Transit switch port should be created for overlay transport")

				// Verify router port was created
				routerPortName := netInfo.GetNetworkScopedName(types.RouterToTransitSwitchPrefix + node1Name)
				rports, err := libovsdbops.FindLogicalRouterPortWithPredicate(nbClient, func(lrp *nbdb.LogicalRouterPort) bool {
					return lrp.Name == routerPortName
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(rports).To(HaveLen(1), "Router port should be created for overlay transport")

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

func newPodWithPrimaryUDN(
	nodeName, nodeSubnet, nodeMgtIP, nodeGWIP, podName, podIPs, podMAC, namespace string,
	primaryUDNConfig userDefinedNetInfo,
) testPod {
	pod := newTPod(nodeName, nodeSubnet, nodeMgtIP, "", podName, podIPs, podMAC, namespace)
	if primaryUDNConfig.isPrimary {
		pod.networkRole = "infrastructure-locked"
		pod.routes = append(
			pod.routes,
			util.PodRoute{
				Dest:    testing.MustParseIPNet("10.128.0.0/14"),
				NextHop: testing.MustParseIP("10.128.1.1"),
			},
			util.PodRoute{
				Dest:    testing.MustParseIPNet("100.64.0.0/16"),
				NextHop: testing.MustParseIP("10.128.1.1"),
			},
		)
	}
	pod.addNetwork(
		primaryUDNConfig.netName,
		primaryUDNConfig.nadName,
		primaryUDNConfig.hostsubnets,
		"",
		nodeGWIP,
		"192.168.1.3/24",
		"0a:58:c0:a8:01:03",
		"primary",
		0,
		[]util.PodRoute{
			{
				Dest:    testing.MustParseIPNet("192.168.0.0/16"),
				NextHop: testing.MustParseIP("192.168.1.1"),
			},
			{
				Dest:    testing.MustParseIPNet("172.16.1.0/24"),
				NextHop: testing.MustParseIP("192.168.1.1"),
			},
			{
				Dest:    testing.MustParseIPNet("100.65.0.0/16"),
				NextHop: testing.MustParseIP("192.168.1.1"),
			},
		},
	)
	return pod
}

func namespacedName(ns, name string) string { return fmt.Sprintf("%s/%s", ns, name) }

func (sni *userDefinedNetInfo) getNetworkRole() string {
	return util.GetUserDefinedNetworkRole(sni.isPrimary)
}

func getNetworkRole(netInfo util.NetInfo) string {
	return util.GetUserDefinedNetworkRole(netInfo.IsPrimaryNetwork())
}

func (sni *userDefinedNetInfo) setupOVNDependencies(dbData *libovsdbtest.TestSetup) error {
	netInfo, err := util.NewNetInfo(sni.netconf())
	if err != nil {
		return err
	}

	externalIDs := map[string]string{
		types.NetworkExternalID:     sni.netName,
		types.NetworkRoleExternalID: sni.getNetworkRole(),
	}
	switch sni.topology {
	case types.Layer2Topology:
		dbData.NBData = append(dbData.NBData, &nbdb.LogicalSwitch{
			Name:        netInfo.GetNetworkScopedName(types.OVNLayer2Switch),
			UUID:        netInfo.GetNetworkScopedName(types.OVNLayer2Switch) + "_UUID",
			ExternalIDs: externalIDs,
		})
	case types.Layer3Topology:
		dbData.NBData = append(dbData.NBData, &nbdb.LogicalSwitch{
			Name:        netInfo.GetNetworkScopedName(nodeName),
			UUID:        netInfo.GetNetworkScopedName(nodeName) + "_UUID",
			ExternalIDs: externalIDs,
		})
	case types.LocalnetTopology:
		dbData.NBData = append(dbData.NBData, &nbdb.LogicalSwitch{
			Name:        netInfo.GetNetworkScopedName(types.OVNLocalnetSwitch),
			UUID:        netInfo.GetNetworkScopedName(types.OVNLocalnetSwitch) + "_UUID",
			ExternalIDs: externalIDs,
		})
	default:
		return fmt.Errorf("missing topology in the network configuration: %v", sni)
	}
	return nil
}

func (sni *userDefinedNetInfo) netconf() *ovncnitypes.NetConf {
	const plugin = "ovn-k8s-cni-overlay"

	role := types.NetworkRoleSecondary
	transitSubnet := ""
	if sni.isPrimary {
		role = types.NetworkRolePrimary
		if sni.topology == types.Layer2Topology {
			transitSubnets := []string{}
			for _, clusterSubnet := range strings.Split(sni.clustersubnets, ",") {
				_, cidr, err := net.ParseCIDR(clusterSubnet)
				Expect(err).NotTo(HaveOccurred())
				if knet.IsIPv4CIDR(cidr) {
					transitSubnets = append(transitSubnets, config.ClusterManager.V4TransitSubnet)
				} else {
					transitSubnets = append(transitSubnets, config.ClusterManager.V6TransitSubnet)
				}
			}
			transitSubnet = strings.Join(transitSubnets, ",")
		}
	}

	return &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: sni.netName,
			Type: plugin,
		},
		Topology:           sni.topology,
		NADName:            sni.nadName,
		Subnets:            sni.clustersubnets,
		Role:               role,
		AllowPersistentIPs: sni.allowPersistentIPs,
		TransitSubnet:      transitSubnet,
	}
}

func dummyTestPod(nsName string, info userDefinedNetInfo) testPod {
	const nodeSubnet = "10.128.1.0/24"
	if info.isPrimary {
		return newPodWithPrimaryUDN(
			nodeName,
			nodeSubnet,
			"10.128.1.2",
			"192.168.1.1",
			"myPod",
			"10.128.1.3",
			"0a:58:0a:80:01:03",
			nsName,
			info,
		)
	}
	pod := newTPod(nodeName, nodeSubnet, "10.128.1.2", "10.128.1.1", podName, "10.128.1.3", "0a:58:0a:80:01:03", nsName)
	pod.addNetwork(
		info.netName,
		info.nadName,
		info.hostsubnets,
		"",
		"",
		"192.168.1.3/24",
		"0a:58:c0:a8:01:03",
		"secondary",
		0,
		[]util.PodRoute{
			{
				Dest:    testing.MustParseIPNet(info.clustersubnets),
				NextHop: testing.MustParseIP("192.168.1.1"),
			},
		},
	)
	return pod
}

func dummySecondaryLayer3UserDefinedNetwork(clustersubnets, hostsubnets string) userDefinedNetInfo {
	return userDefinedNetInfo{
		netName:        userDefinedNetworkName,
		nadName:        namespacedName(ns, nadName),
		topology:       types.Layer3Topology,
		clustersubnets: clustersubnets,
		hostsubnets:    hostsubnets,
	}
}

func dummyPrimaryLayer3UserDefinedNetwork(clustersubnets, hostsubnets string) userDefinedNetInfo {
	secondaryNet := dummySecondaryLayer3UserDefinedNetwork(clustersubnets, hostsubnets)
	secondaryNet.isPrimary = true
	return secondaryNet
}

// This util is returning a network-name/hostSubnet for the node's node-subnets annotation
func (sni *userDefinedNetInfo) String() string {
	return fmt.Sprintf("%q: %q", sni.netName, sni.hostsubnets)
}

func newNodeWithUserDefinedNetworks(nodeName string, nodeIPv4CIDR string, netInfos ...userDefinedNetInfo) (*corev1.Node, error) {
	var nodeSubnetInfo []string
	for _, info := range netInfos {
		nodeSubnetInfo = append(nodeSubnetInfo, info.String())
	}

	parsedNodeSubnets := fmt.Sprintf("{\"default\":\"%s\"}", v4Node1Subnet)
	if len(nodeSubnetInfo) > 0 {
		parsedNodeSubnets = fmt.Sprintf("{\"default\":\"%s\", %s}", v4Node1Subnet, strings.Join(nodeSubnetInfo, ","))
	}

	nodeIP, nodeCIDR, err := net.ParseCIDR(nodeIPv4CIDR)
	if err != nil {
		return nil, err
	}
	nextHopIP := util.GetNodeGatewayIfAddr(nodeCIDR).IP
	nodeCIDR.IP = nodeIP

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4CIDR, ""),
				"k8s.ovn.org/node-subnets":        parsedNodeSubnets,
				util.OVNNodeHostCIDRs:             fmt.Sprintf("[\"%s\"]", nodeIPv4CIDR),
				"k8s.ovn.org/zone-name":           "global",
				"k8s.ovn.org/l3-gateway-config":   fmt.Sprintf("{\"default\":{\"mode\":\"shared\",\"bridge-id\":\"breth0\",\"interface-id\":\"breth0_ovn-worker\",\"mac-address\":%q,\"ip-addresses\":[%[2]q],\"ip-address\":%[2]q,\"next-hops\":[%[3]q],\"next-hop\":%[3]q,\"node-port-enable\":\"true\",\"vlan-id\":\"0\"}}", util.IPAddrToHWAddr(nodeIP), nodeCIDR, nextHopIP),
				util.OvnNodeChassisID:             "abdcef",
				"k8s.ovn.org/network-ids":         fmt.Sprintf("{\"default\":\"0\",\"isolatednet\":\"%s\"}", userDefinedNetworkID),
				util.OvnNodeID:                    "4",
				"k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids": "{\"isolatednet\":\"25\"}",
			},
			Labels: map[string]string{
				"k8s.ovn.org/egress-assignable": "",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}, nil
}

func dummyJoinIPs() []*net.IPNet {
	return []*net.IPNet{dummyMasqueradeIP()}
}

func dummyMasqueradeIP() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("169.254.169.13"),
		Mask: net.CIDRMask(24, 32),
	}
}
func dummyMasqueradeSubnet() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("169.254.169.0"),
		Mask: net.CIDRMask(24, 32),
	}
}

func emptyDefaultClusterNetworkNodeSwitch(nodeName string) []libovsdbtest.TestData {
	switchUUID := nodeName + "-UUID"
	return []libovsdbtest.TestData{&nbdb.LogicalSwitch{UUID: switchUUID, Name: nodeName}}
}

func expectedGWEntities(nodeName string, netInfo util.NetInfo, gwConfig util.L3GatewayConfig) []libovsdbtest.TestData {
	gwRouterName := fmt.Sprintf("GR_%s_%s", netInfo.GetNetworkName(), nodeName)

	expectedEntities := append(
		expectedGWRouterPlusNATAndStaticRoutes(nodeName, gwRouterName, netInfo, gwConfig),
		expectedGRToJoinSwitchLRP(gwRouterName, gwRouterJoinIPAddress(), netInfo),
		expectedGRToExternalSwitchLRP(gwRouterName, netInfo, nodePhysicalIPAddress(), udnGWSNATAddress()),
		expectedGatewayChassis(nodeName, netInfo, gwConfig),
	)
	expectedEntities = append(expectedEntities, expectedStaticMACBindings(gwRouterName, staticMACBindingIPs())...)
	expectedEntities = append(expectedEntities, expectedExternalSwitchAndLSPs(netInfo, gwConfig, nodeName)...)
	expectedEntities = append(expectedEntities, expectedJoinSwitchAndLSPs(netInfo, nodeName)...)
	return expectedEntities
}

func expectedGWRouterPlusNATAndStaticRoutes(
	nodeName, gwRouterName string,
	netInfo util.NetInfo,
	gwConfig util.L3GatewayConfig,
) []libovsdbtest.TestData {
	gwRouterToExtLRPUUID := fmt.Sprintf("%s%s-UUID", types.GWRouterToExtSwitchPrefix, gwRouterName)

	const (
		nat1             = "abc-UUID"
		nat2             = "cba-UUID"
		staticRoute1     = "srA-UUID"
		staticRoute2     = "srB-UUID"
		staticRoute3     = "srC-UUID"
		ipv4DefaultRoute = "0.0.0.0/0"
	)

	staticRouteOutputPort := types.GWRouterToExtSwitchPrefix + netInfo.GetNetworkScopedGWRouterName(nodeName)
	gwRouterLRPUUID := fmt.Sprintf("%s%s-UUID", types.GWRouterToJoinSwitchPrefix, gwRouterName)
	grOptions := gwRouterOptions(gwConfig)
	sr1 := expectedGRStaticRoute(staticRoute1, netInfo.Subnets()[0].CIDR.String(), dummyMasqueradeIP().IP.String(), nil, nil, netInfo)
	if netInfo.TopologyType() == types.Layer2Topology {
		gwRouterLRPUUID = fmt.Sprintf("%s%s-UUID", types.RouterToTransitRouterPrefix, gwRouterName)
		grOptions["lb_force_snat_ip"] = gwRouterJoinIPAddress().IP.String()
		transitRouteOutputPort := types.RouterToTransitRouterPrefix + netInfo.GetNetworkScopedGWRouterName(nodeName)
		trInfo := getTestTransitRouterInfo(netInfo)
		sr1 = expectedGRStaticRoute(staticRoute1, netInfo.Subnets()[0].CIDR.String(), trInfo.transitRouterNets[0].IP.String(), nil, &transitRouteOutputPort, netInfo)
	}
	nextHopIP := gwConfig.NextHops[0].String()
	nextHopMasqIP := nextHopMasqueradeIP().String()
	masqSubnet := config.Gateway.V4MasqueradeSubnet
	var nat []string
	nat = append(nat, nat1, nat2)
	expectedEntities := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			Name:         gwRouterName,
			UUID:         gwRouterName + "-UUID",
			ExternalIDs:  gwRouterExternalIDs(netInfo, gwConfig),
			Options:      grOptions,
			Ports:        []string{gwRouterLRPUUID, gwRouterToExtLRPUUID},
			Nat:          nat,
			StaticRoutes: []string{staticRoute1, staticRoute2, staticRoute3},
		},
		sr1,
		expectedGRStaticRoute(staticRoute2, ipv4DefaultRoute, nextHopIP, nil, &staticRouteOutputPort, netInfo),
		expectedGRStaticRoute(staticRoute3, masqSubnet, nextHopMasqIP, nil, &staticRouteOutputPort, netInfo),
	}
	expectedEntities = append(expectedEntities, newNATEntry(nat1, dummyMasqueradeIP().IP.String(), gwRouterJoinIPAddress().IP.String(), standardNonDefaultNetworkExtIDs(netInfo), ""))
	expectedEntities = append(expectedEntities, newNATEntry(nat2, dummyMasqueradeIP().IP.String(), netInfo.Subnets()[0].CIDR.String(), standardNonDefaultNetworkExtIDs(netInfo), ""))
	return expectedEntities
}

func expectedStaticMACBindings(gwRouterName string, ips []net.IP) []libovsdbtest.TestData {
	lrpName := fmt.Sprintf("%s%s", types.GWRouterToExtSwitchPrefix, gwRouterName)
	var bindings []libovsdbtest.TestData
	for _, ip := range ips {
		bindings = append(bindings, &nbdb.StaticMACBinding{
			UUID:               fmt.Sprintf("%sstatic-mac-binding-UUID(%s)", lrpName, ip.String()),
			IP:                 ip.String(),
			LogicalPort:        lrpName,
			MAC:                util.IPAddrToHWAddr(ip).String(),
			OverrideDynamicMAC: true,
		})
	}
	return bindings
}

func expectedGatewayChassis(nodeName string, netInfo util.NetInfo, gwConfig util.L3GatewayConfig) *nbdb.GatewayChassis {
	gwChassisName := fmt.Sprintf("%s%s_%s-%s", types.RouterToSwitchPrefix, netInfo.GetNetworkName(), nodeName, gwConfig.ChassisID)
	return &nbdb.GatewayChassis{UUID: gwChassisName + "-UUID", Name: gwChassisName, Priority: 1, ChassisName: gwConfig.ChassisID}
}

func expectedGRToJoinSwitchLRP(gatewayRouterName string, gwRouterLRPIP *net.IPNet, netInfo util.NetInfo) *nbdb.LogicalRouterPort {
	lrpName := fmt.Sprintf("%s%s", types.GWRouterToJoinSwitchPrefix, gatewayRouterName)
	options := map[string]string{libovsdbops.GatewayMTU: fmt.Sprintf("%d", 1400)}
	return expectedLogicalRouterPort(lrpName, netInfo, options, gwRouterLRPIP)
}

func expectedGRToExternalSwitchLRP(gatewayRouterName string, netInfo util.NetInfo, joinSwitchIPs ...*net.IPNet) *nbdb.LogicalRouterPort {
	lrpName := fmt.Sprintf("%s%s", types.GWRouterToExtSwitchPrefix, gatewayRouterName)
	return expectedLogicalRouterPort(lrpName, netInfo, nil, joinSwitchIPs...)
}

func expectedLogicalRouterPort(lrpName string, netInfo util.NetInfo, options map[string]string, routerNetworks ...*net.IPNet) *nbdb.LogicalRouterPort {
	var ips []string
	for _, ip := range routerNetworks {
		ips = append(ips, ip.String())
	}
	var mac string
	if len(routerNetworks) > 0 {
		ipToGenMacFrom := routerNetworks[0]
		mac = util.IPAddrToHWAddr(ipToGenMacFrom.IP).String()
	}
	return &nbdb.LogicalRouterPort{
		UUID:     lrpName + "-UUID",
		Name:     lrpName,
		Networks: ips,
		MAC:      mac,
		Options:  options,
		ExternalIDs: map[string]string{
			types.TopologyExternalID: netInfo.TopologyType(),
			types.NetworkExternalID:  netInfo.GetNetworkName(),
		},
	}
}

func expectedLayer3EgressEntities(netInfo util.NetInfo, gwConfig util.L3GatewayConfig, nodeSubnet *net.IPNet) []libovsdbtest.TestData {
	const (
		routerPolicyUUID1 = "lrpol1-UUID"
		routerPolicyUUID2 = "lrpol2-UUID"
		staticRouteUUID1  = "sr1-UUID"
		staticRouteUUID2  = "sr2-UUID"
		masqSNATUUID1     = "masq-snat1-UUID"
	)
	masqIPAddr := dummyMasqueradeIP().IP.String()
	clusterRouterName := fmt.Sprintf("%s_ovn_cluster_router", netInfo.GetNetworkName())
	rtosLRPName := fmt.Sprintf("%s%s", types.RouterToSwitchPrefix, netInfo.GetNetworkScopedName(nodeName))
	rtosLRPUUID := rtosLRPName + "-UUID"
	nodeIP := gwConfig.IPAddresses[0].IP.String()
	masqSNAT := newNATEntry(masqSNATUUID1, "169.254.169.14", nodeSubnet.String(), standardNonDefaultNetworkExtIDs(netInfo), "")
	masqSNAT.Match = getMasqueradeManagementIPSNATMatch(util.IPAddrToHWAddr(managementPortIP(nodeSubnet)).String())
	masqSNAT.LogicalPort = ptr.To(fmt.Sprintf("rtos-%s_%s", netInfo.GetNetworkName(), nodeName))
	if !config.OVNKubernetesFeature.EnableInterconnect {
		masqSNAT.GatewayPort = ptr.To(fmt.Sprintf("rtos-%s_%s", netInfo.GetNetworkName(), nodeName) + "-UUID")
	}

	gatewayChassisUUID := fmt.Sprintf("%s-%s-UUID", rtosLRPName, gwConfig.ChassisID)
	lrsrNextHop := gwRouterJoinIPAddress().IP.String()
	if config.Gateway.Mode == config.GatewayModeLocal {
		lrsrNextHop = managementPortIP(nodeSubnet).String()
	}
	expectedEntities := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			Name:         clusterRouterName,
			UUID:         clusterRouterName + "-UUID",
			Ports:        []string{rtosLRPUUID},
			StaticRoutes: []string{staticRouteUUID1, staticRouteUUID2},
			Policies:     []string{routerPolicyUUID1, routerPolicyUUID2},
			ExternalIDs:  standardNonDefaultNetworkExtIDs(netInfo),
			Nat:          []string{masqSNATUUID1},
		},
		&nbdb.LogicalRouterPort{
			UUID:           rtosLRPUUID,
			Name:           rtosLRPName,
			Networks:       []string{"192.168.1.1/24"},
			MAC:            "0a:58:c0:a8:01:01",
			GatewayChassis: []string{gatewayChassisUUID},
			Options:        map[string]string{libovsdbops.GatewayMTU: "1400"},
		},
		expectedGRStaticRoute(staticRouteUUID1, nodeSubnet.String(), lrsrNextHop, &nbdb.LogicalRouterStaticRoutePolicySrcIP, nil, netInfo),
		expectedGRStaticRoute(staticRouteUUID2, gwRouterJoinIPAddress().IP.String(), gwRouterJoinIPAddress().IP.String(), nil, nil, netInfo),
		expectedLogicalRouterPolicy(routerPolicyUUID1, netInfo, nodeName, nodeIP, managementPortIP(nodeSubnet).String()),
		expectedLogicalRouterPolicy(routerPolicyUUID2, netInfo, nodeName, masqIPAddr, managementPortIP(nodeSubnet).String()),
		masqSNAT,
	}
	return expectedEntities
}

func expectedLogicalRouterPolicy(routerPolicyUUID1 string, netInfo util.NetInfo, nodeName, destIP, nextHop string) *nbdb.LogicalRouterPolicy {
	const (
		priority      = 1004
		rerouteAction = "reroute"
	)
	networkScopedSwitchName := netInfo.GetNetworkScopedSwitchName(nodeName)
	lrpName := fmt.Sprintf("%s%s", types.RouterToSwitchPrefix, networkScopedSwitchName)

	return &nbdb.LogicalRouterPolicy{
		UUID:        routerPolicyUUID1,
		Action:      rerouteAction,
		ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
		Match:       fmt.Sprintf("inport == %q && ip4.dst == %s /* %s */", lrpName, destIP, networkScopedSwitchName),
		Nexthops:    []string{nextHop},
		Priority:    priority,
	}
}

func expectedGRStaticRoute(uuid, ipPrefix, nextHop string, policy *nbdb.LogicalRouterStaticRoutePolicy, outputPort *string, netInfo util.NetInfo) *nbdb.LogicalRouterStaticRoute {
	return &nbdb.LogicalRouterStaticRoute{
		UUID:       uuid,
		IPPrefix:   ipPrefix,
		OutputPort: outputPort,
		Nexthop:    nextHop,
		Policy:     policy,
		ExternalIDs: map[string]string{
			types.NetworkExternalID:  "isolatednet",
			types.TopologyExternalID: netInfo.TopologyType(),
		},
	}
}

func allowAllFromMgmtPort(aclUUID string, mgmtPortIP string, switchName string) *nbdb.ACL {
	meterName := "acl-logging"
	return &nbdb.ACL{
		UUID:      aclUUID,
		Action:    "allow-related",
		Direction: "to-lport",
		ExternalIDs: map[string]string{
			"k8s.ovn.org/name":             switchName,
			"ip":                           mgmtPortIP,
			"k8s.ovn.org/id":               fmt.Sprintf("isolatednet-network-controller:NetpolNode:%s:%s", switchName, mgmtPortIP),
			"k8s.ovn.org/owner-controller": "isolatednet-network-controller",
			"k8s.ovn.org/owner-type":       "NetpolNode",
		},
		Match:    fmt.Sprintf("ip4.src==%s", mgmtPortIP),
		Meter:    &meterName,
		Priority: 1001,
		Tier:     2,
	}
}

func nodePhysicalIPAddress() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("192.168.126.202"),
		Mask: net.CIDRMask(24, 32),
	}
}

func udnGWSNATAddress() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("169.254.169.13"),
		Mask: net.CIDRMask(24, 32),
	}
}

func newNATEntry(uuid string, externalIP string, logicalIP string, extIDs map[string]string, match string) *nbdb.NAT {
	return &nbdb.NAT{
		UUID:        uuid,
		ExternalIP:  externalIP,
		LogicalIP:   logicalIP,
		Match:       match,
		Type:        "snat",
		Options:     map[string]string{"stateless": "false"},
		ExternalIDs: extIDs,
	}
}

func expectedExternalSwitchAndLSPs(netInfo util.NetInfo, gwConfig util.L3GatewayConfig, nodeName string) []libovsdbtest.TestData {
	const (
		port1UUID = "port1-UUID"
		port2UUID = "port2-UUID"
	)
	gwRouterName := netInfo.GetNetworkScopedGWRouterName(nodeName)
	return []libovsdbtest.TestData{
		&nbdb.LogicalSwitch{
			UUID:        "ext-UUID",
			Name:        netInfo.GetNetworkScopedExtSwitchName(nodeName),
			ExternalIDs: standardNonDefaultNetworkExtIDsForLogicalSwitch(netInfo),
			Ports:       []string{port1UUID, port2UUID},
		},
		&nbdb.LogicalSwitchPort{
			UUID:        port1UUID,
			Name:        netInfo.GetNetworkScopedExtPortName(gwConfig.BridgeID, nodeName),
			Addresses:   []string{"unknown"},
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
			Options:     map[string]string{"network_name": "physnet"},
			Type:        types.LocalnetTopology,
		},
		&nbdb.LogicalSwitchPort{
			UUID:        port2UUID,
			Name:        types.EXTSwitchToGWRouterPrefix + gwRouterName,
			Addresses:   []string{gwConfig.MACAddress.String()},
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
			Options:     externalSwitchRouterPortOptions(gwRouterName),
			Type:        "router",
		},
	}
}

func externalSwitchRouterPortOptions(gatewayRouterName string) map[string]string {
	return map[string]string{
		"nat-addresses":             "router",
		"exclude-lb-vips-from-garp": "true",
		libovsdbops.RouterPort:      types.GWRouterToExtSwitchPrefix + gatewayRouterName,
	}
}

func expectedJoinSwitchAndLSPs(netInfo util.NetInfo, nodeName string) []libovsdbtest.TestData {
	const joinToGRLSPUUID = "port3-UUID"
	gwRouterName := netInfo.GetNetworkScopedGWRouterName(nodeName)
	expectedData := []libovsdbtest.TestData{
		&nbdb.LogicalSwitch{
			UUID:        "join-UUID",
			Name:        netInfo.GetNetworkScopedJoinSwitchName(),
			Ports:       []string{joinToGRLSPUUID},
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
		},
		&nbdb.LogicalSwitchPort{
			UUID:        joinToGRLSPUUID,
			Name:        types.JoinSwitchToGWRouterPrefix + gwRouterName,
			Addresses:   []string{"router"},
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
			Options:     map[string]string{libovsdbops.RouterPort: types.GWRouterToJoinSwitchPrefix + gwRouterName},
			Type:        "router",
		},
	}
	return expectedData
}

func nextHopMasqueradeIP() net.IP {
	return net.ParseIP("169.254.169.4")
}

func staticMACBindingIPs() []net.IP {
	return []net.IP{net.ParseIP("169.254.169.4"), net.ParseIP("169.254.169.2")}
}

func gwRouterJoinIPAddress() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("100.65.0.4"),
		Mask: net.CIDRMask(16, 32),
	}
}

func gwRouterOptions(gwConfig util.L3GatewayConfig) map[string]string {

	dynamicNeighRouters := "true"
	if config.OVNKubernetesFeature.EnableInterconnect {
		dynamicNeighRouters = "false"
	}

	return map[string]string{
		"lb_force_snat_ip":              "router_ip",
		"mac_binding_age_threshold":     "300",
		"chassis":                       gwConfig.ChassisID,
		"always_learn_from_arp_request": "false",
		"dynamic_neigh_routers":         dynamicNeighRouters,
	}
}

func standardNonDefaultNetworkExtIDs(netInfo util.NetInfo) map[string]string {
	return map[string]string{
		types.TopologyExternalID: netInfo.TopologyType(),
		types.NetworkExternalID:  netInfo.GetNetworkName(),
	}
}

func standardNonDefaultNetworkExtIDsForLogicalSwitch(netInfo util.NetInfo) map[string]string {
	externalIDs := standardNonDefaultNetworkExtIDs(netInfo)
	externalIDs[types.NetworkRoleExternalID] = getNetworkRole(netInfo)
	return externalIDs
}

func newLayer3UserDefinedNetworkController(
	cnci *CommonNetworkControllerInfo,
	netInfo util.NetInfo,
	nodeName string,
	networkManager networkmanager.Interface,
	eIPController *EgressIPController,
	portCache *PortCache,
) *Layer3UserDefinedNetworkController {
	layer3NetworkController, err := NewLayer3UserDefinedNetworkController(cnci, netInfo, networkManager, nil, eIPController, portCache)
	Expect(err).NotTo(HaveOccurred())
	layer3NetworkController.gatewayManagers.Store(
		nodeName,
		newDummyGatewayManager(cnci.kube, cnci.nbClient, netInfo, cnci.watchFactory, nodeName),
	)
	return layer3NetworkController
}

func buildNamespacedPortGroup(namespace, controller string) *nbdb.PortGroup {
	pgIDs := getNamespacePortGroupDbIDs(namespace, controller)
	pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
	pg.UUID = pg.Name + "-UUID"
	return pg
}

func getNetworkPolicyPortGroupDbIDs(namespace, controllerName, name string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetworkPolicy, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: libovsdbops.BuildNamespaceNameKey(namespace, name),
		})
}
