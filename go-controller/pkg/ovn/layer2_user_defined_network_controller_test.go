package ovn

import (
	"context"
	"fmt"
	"net"
	"time"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/urfave/cli/v2"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	knet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	testnm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/networkmanager"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type lspEnableValue *bool

var (
	lspEnableNotSpecified    lspEnableValue = nil
	lspEnableExplicitlyTrue  lspEnableValue = ptr.To(true)
	lspEnableExplicitlyFalse lspEnableValue = ptr.To(false)
)

type liveMigrationPodInfo struct {
	podPhase           corev1.PodPhase
	annotation         map[string]string
	creationTimestamp  metav1.Time
	expectedLspEnabled lspEnableValue
}

type liveMigrationInfo struct {
	vmName        string
	sourcePodInfo liveMigrationPodInfo
	targetPodInfo liveMigrationPodInfo
}

var _ = Describe("OVN Multi-Homed pod operations for layer 2 network", func() {
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
			NBData: []libovsdbtest.TestData{},
		}

		config.OVNKubernetesFeature = *minimalFeatureConfig()
		config.Gateway.V4MasqueradeSubnet = dummyMasqueradeSubnet().String()
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	DescribeTable(
		"reconciles a new",
		func(netInfo userDefinedNetInfo, testConfig testConfiguration, gatewayMode config.GatewayMode) {
			const podIdx = 0
			podInfo := dummyL2TestPod(ns, netInfo, podIdx, podIdx)
			setupConfig(netInfo, testConfig, gatewayMode)
			app.Action = func(*cli.Context) error {
				pod := newMultiHomedPod(podInfo, netInfo)

				const nodeIPv4CIDR = "192.168.126.202/24"
				By(fmt.Sprintf("Creating a node named %q, with IP: %s", nodeName, nodeIPv4CIDR))
				testNode, err := newNodeWithUserDefinedNetworks(nodeName, nodeIPv4CIDR)
				Expect(err).NotTo(HaveOccurred())

				Expect(setupFakeOvnForLayer2Topology(fakeOvn, initialDB, netInfo, testNode, podInfo, pod)).To(Succeed())
				defer fakeOvn.networkManager.Stop()

				// for layer2 on interconnect, it is the cluster manager that
				// allocates the OVN annotation; on unit tests, this just
				// doesn't happen, and we create the pod with these annotations
				// set. Hence, no point checking they're the expected ones.
				// TODO: align the mocked annotations with the production code
				//   - currently missing setting the routes.
				if !config.OVNKubernetesFeature.EnableInterconnect {
					By("asserting the pod OVN pod networks annotation are the expected ones")
					// check that after start networks annotations and nbdb will be updated
					Eventually(func() string {
						return getPodAnnotations(fakeOvn.fakeClient.KubeClient, podInfo.namespace, podInfo.podName)
					}).WithTimeout(2 * time.Second).Should(MatchJSON(podInfo.getAnnotationsJson()))
				}

				expectationOptions := testConfig.expectationOptions
				if netInfo.isPrimary {
					By("configuring the expectation machine with the GW related configuration")
					gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
					Expect(err).NotTo(HaveOccurred())
					Expect(gwConfig.NextHops).NotTo(BeEmpty())
					expectationOptions = append(expectationOptions, withGatewayConfig(gwConfig))
					expectationOptions = append(expectationOptions, withClusterPortGroup())
				}
				By("asserting the OVN entities provisioned in the NBDB are the expected ones")
				Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(
						newUserDefinedNetworkExpectationMachine(
							fakeOvn,
							[]testPod{podInfo},
							expectationOptions...,
						).expectedLogicalSwitchesAndPorts(netInfo.isPrimary)...))

				return nil
			}

			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		Entry("pod on a user defined secondary network",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
			config.GatewayModeShared,
		),

		Entry("pod on a user defined primary network",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
			config.GatewayModeShared,
		),

		Entry("pod on a user defined secondary network on an IC cluster",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			config.GatewayModeShared,
		),

		Entry("pod on a user defined primary network on an IC cluster",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			config.GatewayModeShared,
		),

		Entry("pod on a user defined primary network on an IC cluster; LGW",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			config.GatewayModeLocal,
		),

		Entry("pod on a user defined primary network on an IC cluster with per-pod SNATs enabled",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(func(testConfig *testConfiguration) {
				testConfig.gatewayConfig = &config.GatewayConfig{DisableSNATMultipleGWs: true}
			}),
			config.GatewayModeShared,
		),
		/** FIXME: tests do not support ipv6 yet
		Entry("pod on a IPv6 user defined primary network on an IC cluster with per-pod SNATs enabled",
			dummyPrimaryLayer2UserDefinedNetwork("2001:db8:abcd:0012::/64"),
			icClusterWithDisableSNATTestConfiguration(),
			config.GatewayModeShared,
		),
		*/
	)

	DescribeTable(
		"reconciles a new kubevirt-related pod during its live-migration phases",
		func(netInfo userDefinedNetInfo, testConfig testConfiguration, migrationInfo *liveMigrationInfo) {
			ipamClaim := ipamclaimsapi.IPAMClaim{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: ns,
					Name:      netInfo.netName + "-" + migrationInfo.vmName,
				},
				Spec: ipamclaimsapi.IPAMClaimSpec{
					Network:   netInfo.netName,
					Interface: "net1",
				},
			}
			netInfo.allowPersistentIPs = true
			netInfo.ipamClaimReference = ipamClaim.Name

			const (
				sourcePodInfoIdx      = 0
				targetPodInfoIdx      = 1
				userDefinedNetworkIdx = 0
			)
			sourcePodInfo := dummyL2TestPod(ns, netInfo, sourcePodInfoIdx, userDefinedNetworkIdx)
			setupConfig(netInfo, testConfig, config.GatewayModeShared)
			app.Action = func(*cli.Context) error {
				sourcePod := newMultiHomedKubevirtPod(
					migrationInfo.vmName,
					migrationInfo.sourcePodInfo,
					sourcePodInfo,
					netInfo)

				const nodeIPv4CIDR = "192.168.126.202/24"
				By(fmt.Sprintf("Creating a node named %q, with IP: %s", nodeName, nodeIPv4CIDR))
				testNode, err := newNodeWithUserDefinedNetworks(nodeName, nodeIPv4CIDR)
				Expect(err).NotTo(HaveOccurred())

				Expect(setupFakeOvnForLayer2Topology(fakeOvn, initialDB, netInfo, testNode, sourcePodInfo, sourcePod,
					&ipamclaimsapi.IPAMClaimList{Items: []ipamclaimsapi.IPAMClaim{ipamClaim}}),
				).To(Succeed())
				defer fakeOvn.networkManager.Stop()

				// for layer2 on interconnect, it is the cluster manager that
				// allocates the OVN annotation; on unit tests, this just
				// doesn't happen, and we create the pod with these annotations
				// set. Hence, no point checking they're the expected ones.
				// TODO: align the mocked annotations with the production code
				//   - currently missing setting the routes.
				if !config.OVNKubernetesFeature.EnableInterconnect {
					By("asserting the pod OVN pod networks annotation are the expected ones")
					// check that after start networks annotations and nbdb will be updated
					Eventually(func() string {
						return getPodAnnotations(fakeOvn.fakeClient.KubeClient, sourcePodInfo.namespace, sourcePodInfo.podName)
					}).WithTimeout(2 * time.Second).Should(MatchJSON(sourcePodInfo.getAnnotationsJson()))
				}

				expectationOptions := testConfig.expectationOptions
				if netInfo.isPrimary {
					By("configuring the expectation machine with the GW related configuration")
					gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
					Expect(err).NotTo(HaveOccurred())
					Expect(gwConfig.NextHops).NotTo(BeEmpty())
					expectationOptions = append(expectationOptions, withGatewayConfig(gwConfig))
					expectationOptions = append(expectationOptions, withClusterPortGroup())
				}
				By("asserting the OVN entities provisioned in the NBDB are the expected ones before migration started")
				Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(
						newUserDefinedNetworkExpectationMachine(
							fakeOvn,
							[]testPod{sourcePodInfo},
							expectationOptions...,
						).expectedLogicalSwitchesAndPorts(netInfo.isPrimary)...))

				targetPodInfo := dummyL2TestPod(ns, netInfo, targetPodInfoIdx, userDefinedNetworkIdx)
				targetKvPod := newMultiHomedKubevirtPod(
					migrationInfo.vmName,
					migrationInfo.targetPodInfo,
					targetPodInfo,
					netInfo)

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(targetKvPod.Namespace).Create(context.Background(), targetKvPod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("asserting the OVN entities provisioned in the NBDB are the expected ones after migration")
				expectedPodLspEnabled := map[string]*bool{}
				expectedPodLspEnabled[sourcePodInfo.podName] = migrationInfo.sourcePodInfo.expectedLspEnabled

				testPods := []testPod{sourcePodInfo}
				if !util.PodCompleted(targetKvPod) {
					testPods = append(testPods, targetPodInfo)
					expectedPodLspEnabled[targetPodInfo.podName] = migrationInfo.targetPodInfo.expectedLspEnabled
				}
				Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(
						newUserDefinedNetworkExpectationMachine(
							fakeOvn,
							testPods,
							expectationOptions...,
						).expectedLogicalSwitchesAndPortsWithLspEnabled(netInfo.isPrimary, expectedPodLspEnabled)...))
				return nil
			}

			Expect(app.Run([]string{app.Name})).To(Succeed())
		},

		Entry("on a layer2 topology with user defined secondary network, when target pod is not yet ready",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
			notReadyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined secondary network, when target pod is ready",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
			readyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined secondary network and an IC cluster, when target pod is not yet ready",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			notReadyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined secondary network and an IC cluster, when target pod is ready",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			readyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined secondary network and an IC cluster, when target pod failed",
			dummySecondaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			failedMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined primary network, when target pod is not yet ready",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
			notReadyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined primary network, when target pod is ready",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			nonICClusterTestConfiguration(),
			readyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined primary network and an IC cluster, when target pod is not yet ready",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			notReadyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined primary network and an IC cluster, when target pod is ready",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			readyMigrationInfo(),
		),

		Entry("on a layer2 topology with user defined primary network and an IC cluster, when target pod failed",
			dummyPrimaryLayer2UserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(),
			failedMigrationInfo(),
		),
	)

	DescribeTable(
		"user-defined network controller DB entities are properly cleaned up",
		func(netInfo userDefinedNetInfo, testConfig testConfiguration) {
			podInfo := dummyTestPod(ns, netInfo)
			if testConfig.configToOverride != nil {
				config.OVNKubernetesFeature = *testConfig.configToOverride
				if testConfig.gatewayConfig != nil {
					config.Gateway.DisableSNATMultipleGWs = testConfig.gatewayConfig.DisableSNATMultipleGWs
				}
				config.OVNKubernetesFeature.EnableMultiNetwork = true
			}
			app.Action = func(*cli.Context) error {
				netConf := netInfo.netconf()
				networkConfig, err := util.NewNetInfo(netConf)
				Expect(err).NotTo(HaveOccurred())

				fakeNetworkManager := &testnm.FakeNetworkManager{
					PrimaryNetworks: map[string]util.NetInfo{},
				}
				fakeNetworkManager.PrimaryNetworks[ns] = networkConfig
				nad, err := newNetworkAttachmentDefinition(
					ns,
					nadName,
					*netConf,
				)
				Expect(err).NotTo(HaveOccurred())

				const nodeIPv4CIDR = "192.168.126.202/24"
				testNode, err := newNodeWithUserDefinedNetworks(nodeName, nodeIPv4CIDR)
				Expect(err).NotTo(HaveOccurred())

				gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
				Expect(err).NotTo(HaveOccurred())
				Expect(gwConfig.NextHops).NotTo(BeEmpty())
				nbZone := &nbdb.NBGlobal{Name: ovntypes.OvnDefaultZone, UUID: ovntypes.OvnDefaultZone}

				n := newNamespace(ns)
				if netInfo.isPrimary {
					n = newUDNNamespace(ns)
					gwConfig, err := util.ParseNodeL3GatewayAnnotation(testNode)
					Expect(err).NotTo(HaveOccurred())
					initialDB.NBData = append(
						initialDB.NBData,
						expectedGWEntitiesLayer2(nodeName, networkConfig, *gwConfig)...)
					initialDB.NBData = append(
						initialDB.NBData,
						expectedLayer2EgressEntities(networkConfig, *gwConfig, networkConfig.Subnets()[0].CIDR)...)
				}
				initialDB.NBData = append(initialDB.NBData, nbZone)

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
				)

				Expect(netInfo.setupOVNDependencies(&initialDB)).To(Succeed())

				podInfo.populateLogicalSwitchCache(fakeOvn)

				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.Background(), podInfo.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				// on IC, the test itself spits out the pod with the
				// annotations set, since on production it would be the
				// clustermanager to annotate the pod.
				if !config.OVNKubernetesFeature.EnableInterconnect {
					// pod exists, networks annotations don't
					_, ok := pod.Annotations[util.OvnPodAnnotationName]
					Expect(ok).To(BeFalse())
				}

				Expect(fakeOvn.networkManager.Start()).To(Succeed())
				defer fakeOvn.networkManager.Stop()
				udnNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
				Expect(ok).To(BeTrue())

				fullUDNController, ok := fakeOvn.fullL2UDNControllers[userDefinedNetworkName]
				Expect(ok).To(BeTrue())
				err = fullUDNController.init()
				Expect(err).NotTo(HaveOccurred())

				udnNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
				podInfo.populateUserDefinedNetworkLogicalSwitchCache(udnNetController)
				Expect(udnNetController.bnc.WatchNodes()).To(Succeed())
				Expect(udnNetController.bnc.WatchPods()).To(Succeed())

				Expect(fakeOvn.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
				Expect(fakeOvn.fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Delete(context.Background(), nad.Name, metav1.DeleteOptions{})).To(Succeed())

				err = fullUDNController.Cleanup()
				Expect(err).NotTo(HaveOccurred())
				Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(generateUDNPostInitDB([]libovsdbtest.TestData{nbZone})))

				return nil
			}
			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		Entry("pod on a user defined primary network",
			dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16"),
			nonICClusterTestConfiguration(),
		),
		Entry("pod on a user defined primary network on an IC cluster",
			dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16"),
			icClusterTestConfiguration(),
		),
		Entry("pod on a user defined primary network on an IC cluster with per-pod SNATs enabled",
			dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16"),
			icClusterTestConfiguration(func(testConfig *testConfiguration) {
				testConfig.gatewayConfig = &config.GatewayConfig{DisableSNATMultipleGWs: true}
			}),
		),
	)

})

func dummySecondaryLayer2UserDefinedNetwork(subnets string) userDefinedNetInfo {
	return userDefinedNetInfo{
		netName:        userDefinedNetworkName,
		nadName:        namespacedName(ns, nadName),
		topology:       ovntypes.Layer2Topology,
		clustersubnets: subnets,
	}
}

func dummyPrimaryLayer2UserDefinedNetwork(subnets string) userDefinedNetInfo {
	udnNetInfo := dummySecondaryLayer2UserDefinedNetwork(subnets)
	udnNetInfo.isPrimary = true
	return udnNetInfo
}

func dummyL2TestPod(nsName string, info userDefinedNetInfo, podIdx, udnNetIdx int) testPod {
	const nodeSubnet = "10.128.1.0/24"

	if info.isPrimary {
		pod := newTPod(nodeName, nodeSubnet, "10.128.1.2", "", fmt.Sprintf("myPod-%d", podIdx), fmt.Sprintf("10.128.1.%d", podIdx+3), fmt.Sprintf("0a:58:0a:80:01:%0.2d", podIdx+3), nsName)
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
		pod.addNetwork(
			info.netName,
			info.nadName,
			info.clustersubnets,
			"",
			"100.200.0.1",
			fmt.Sprintf("100.200.0.%d/16", udnNetIdx+3),
			fmt.Sprintf("0a:58:64:c8:00:%0.2d", udnNetIdx+3),
			"primary",
			0,
			[]util.PodRoute{
				{
					Dest:    testing.MustParseIPNet("172.16.1.0/24"),
					NextHop: testing.MustParseIP("100.200.0.1"),
				},
				{
					Dest:    testing.MustParseIPNet("100.65.0.0/16"),
					NextHop: testing.MustParseIP("100.200.0.1"),
				},
			},
		)
		return pod
	}
	pod := newTPod(nodeName, nodeSubnet, "10.128.1.2", "10.128.1.1", fmt.Sprintf("%s-%d", podName, podIdx), fmt.Sprintf("10.128.1.%d", podIdx+3), fmt.Sprintf("0a:58:0a:80:01:%0.2d", podIdx+3), nsName)
	pod.addNetwork(
		info.netName,
		info.nadName,
		info.clustersubnets,
		"",
		"",
		fmt.Sprintf("100.200.0.%d/16", udnNetIdx+1),
		fmt.Sprintf("0a:58:64:c8:00:%0.2d", udnNetIdx+1),
		"secondary",
		0,
		[]util.PodRoute{},
	)
	return pod
}

func getTestTransitRouterInfo(netInfo util.NetInfo) *transitRouterInfo {
	transitRouterInfo, err := getTransitRouterInfo(netInfo, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				// this is hardcoded in newNodeWithSecondaryNets
				ovnNodeID: "4",
			},
		},
	})
	Expect(err).NotTo(HaveOccurred())
	return transitRouterInfo
}

func expectedGWEntitiesLayer2(nodeName string, netInfo util.NetInfo, gwConfig util.L3GatewayConfig) []libovsdbtest.TestData {
	gwRouterName := fmt.Sprintf("GR_%s_%s", netInfo.GetNetworkName(), nodeName)
	trInfo := getTestTransitRouterInfo(netInfo)
	expectedEntities := append(
		expectedGWRouterPlusNATAndStaticRoutes(nodeName, gwRouterName, netInfo, gwConfig),
		expectedGRToTransitRouterLRPLayer2(gwRouterName, gwRouterJoinIPAddress(), netInfo, trInfo),
		expectedGRToExternalSwitchLRP(gwRouterName, netInfo, nodePhysicalIPAddress(), udnGWSNATAddress()),
	)
	expectedEntities = append(expectedEntities, expectedStaticMACBindings(gwRouterName, staticMACBindingIPs())...)
	expectedEntities = append(expectedEntities, expectedExternalSwitchAndLSPs(netInfo, gwConfig, nodeName)...)
	return expectedEntities
}

func expectedGRToTransitRouterLRPLayer2(gatewayRouterName string, gwRouterLRPIP *net.IPNet, netInfo util.NetInfo,
	transitRouterInfo *transitRouterInfo) *nbdb.LogicalRouterPort {
	lrpName := fmt.Sprintf("%s%s", ovntypes.RouterToTransitRouterPrefix, gatewayRouterName)
	options := map[string]string{libovsdbops.GatewayMTU: fmt.Sprintf("%d", 1400)}

	var ips []string
	ips = append(ips, gwRouterLRPIP.String())
	ips = append(ips, transitRouterInfo.gatewayRouterNets[0].String())
	mac := util.IPAddrToHWAddr(gwRouterLRPIP.IP).String()
	return &nbdb.LogicalRouterPort{
		UUID:     lrpName + "-UUID",
		Name:     lrpName,
		Networks: ips,
		MAC:      mac,
		Options:  options,
		ExternalIDs: map[string]string{
			ovntypes.TopologyExternalID: netInfo.TopologyType(),
			ovntypes.NetworkExternalID:  netInfo.GetNetworkName(),
		},
		Peer: ptr.To(ovntypes.TransitRouterToRouterPrefix + gatewayRouterName),
	}
}

func expectedLayer2EgressEntities(netInfo util.NetInfo, gwConfig util.L3GatewayConfig, nodeSubnet *net.IPNet) []libovsdbtest.TestData {
	const (
		routerPolicyUUID1 = "lrpol1-UUID"
		staticRouteUUID1  = "sr1-UUID"
		staticRouteUUID2  = "sr2-UUID"
		masqSNATUUID1     = "masq-snat1-UUID"
	)
	trInfo := getTestTransitRouterInfo(netInfo)
	transitRouterName := fmt.Sprintf("%s_transit_router", netInfo.GetNetworkName())

	rtosLRPName := fmt.Sprintf("%s%s", ovntypes.TransitRouterToSwitchPrefix, netInfo.GetNetworkScopedName(ovntypes.OVNLayer2Switch))
	rtosLRPUUID := rtosLRPName + "-UUID"
	gwRouterName := fmt.Sprintf("GR_%s_%s", netInfo.GetNetworkName(), nodeName)

	rtorLRPName := fmt.Sprintf("%s%s", ovntypes.TransitRouterToRouterPrefix, gwRouterName)
	rtorLRPUUID := rtorLRPName + "-UUID"
	nodeIP := gwConfig.IPAddresses[0].IP.String()
	masqSNAT := newNATEntry(masqSNATUUID1, "169.254.169.14", nodeSubnet.String(), standardNonDefaultNetworkExtIDs(netInfo), "")
	masqSNAT.Match = getMasqueradeManagementIPSNATMatch(util.IPAddrToHWAddr(managementPortIP(nodeSubnet)).String())
	masqSNAT.LogicalPort = ptr.To(fmt.Sprintf("trtos-%s", netInfo.GetNetworkScopedName(ovntypes.OVNLayer2Switch)))
	if !config.OVNKubernetesFeature.EnableInterconnect {
		masqSNAT.GatewayPort = nil
	}
	gwChassisName := fmt.Sprintf("%s-%s", rtosLRPName, gwConfig.ChassisID)
	gatewayChassisUUID := gwChassisName + "-UUID"
	lrsrNextHop := trInfo.gatewayRouterNets[0].IP.String()
	if config.Gateway.Mode == config.GatewayModeLocal {
		lrsrNextHop = managementPortIP(nodeSubnet).String()
	}
	expectedEntities := []libovsdbtest.TestData{
		&nbdb.LogicalRouter{
			Name:         transitRouterName,
			UUID:         transitRouterName + "-UUID",
			Ports:        []string{rtosLRPUUID, rtorLRPUUID},
			StaticRoutes: []string{staticRouteUUID1, staticRouteUUID2},
			Policies:     []string{routerPolicyUUID1},
			ExternalIDs:  standardNonDefaultNetworkExtIDs(netInfo),
			Nat:          []string{masqSNATUUID1},
		},
		&nbdb.LogicalRouterPort{
			UUID:           rtosLRPUUID,
			Name:           rtosLRPName,
			Networks:       []string{"100.200.0.1/16"},
			MAC:            "0a:58:64:c8:00:01",
			GatewayChassis: []string{gatewayChassisUUID},
			Options:        map[string]string{libovsdbops.GatewayMTU: "1400"},
		},
		&nbdb.LogicalRouterPort{
			UUID:        rtorLRPUUID,
			Name:        rtorLRPName,
			Networks:    []string{trInfo.transitRouterNets[0].String()},
			MAC:         util.IPAddrToHWAddr(trInfo.transitRouterNets[0].IP).String(),
			Options:     map[string]string{libovsdbops.RequestedTnlKey: "4"},
			Peer:        ptr.To(fmt.Sprintf("%s%s", ovntypes.RouterToTransitRouterPrefix, gwRouterName)),
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
		},
		expectedGRStaticRoute(staticRouteUUID1, nodeSubnet.String(), lrsrNextHop, &nbdb.LogicalRouterStaticRoutePolicySrcIP, nil, netInfo),
		expectedGRStaticRoute(staticRouteUUID2, gwRouterJoinIPAddress().IP.String(), trInfo.gatewayRouterNets[0].IP.String(), nil, nil, netInfo),
		expectedLogicalRouterPolicy(routerPolicyUUID1, netInfo, nodeName, nodeIP, managementPortIP(nodeSubnet).String()),
		masqSNAT,
		&nbdb.GatewayChassis{UUID: gatewayChassisUUID, Name: gwChassisName, Priority: 1, ChassisName: gwConfig.ChassisID},
	}
	return expectedEntities
}

func dummyLayer2SecondaryUserDefinedNetwork(subnets string) userDefinedNetInfo {
	return userDefinedNetInfo{
		netName:        userDefinedNetworkName,
		nadName:        namespacedName(ns, nadName),
		topology:       ovntypes.Layer2Topology,
		clustersubnets: subnets,
	}
}

func dummyLayer2PrimaryUserDefinedNetwork(subnets string) userDefinedNetInfo {
	secondaryNet := dummyLayer2SecondaryUserDefinedNetwork(subnets)
	secondaryNet.isPrimary = true
	return secondaryNet
}

func setupFakeOvnForLayer2Topology(fakeOvn *FakeOVN, initialDB libovsdbtest.TestSetup, netInfo userDefinedNetInfo, testNode *corev1.Node, podInfo testPod, pod *corev1.Pod, extraObjects ...runtime.Object) error {
	By(fmt.Sprintf("creating a network attachment definition for network: %s", netInfo.netName))
	nad, err := newNetworkAttachmentDefinition(
		ns,
		nadName,
		*netInfo.netconf(),
	)
	Expect(err).NotTo(HaveOccurred())
	By("setting up the OVN DB without any entities in it")
	Expect(netInfo.setupOVNDependencies(&initialDB)).To(Succeed())

	n := newNamespace(ns)
	if netInfo.isPrimary {
		n = newUDNNamespace(ns)
		networkConfig, err := util.NewNetInfo(netInfo.netconf())
		Expect(err).NotTo(HaveOccurred())

		initialDB.NBData = append(
			initialDB.NBData,
			&nbdb.LogicalRouter{
				Name:        fmt.Sprintf("GR_%s_%s", networkConfig.GetNetworkName(), nodeName),
				ExternalIDs: standardNonDefaultNetworkExtIDs(networkConfig),
			},
			&nbdb.LogicalRouter{
				Name:        fmt.Sprintf("%s_transit_router", netInfo.netName),
				ExternalIDs: standardNonDefaultNetworkExtIDs(networkConfig),
			},
			newNetworkClusterPortGroup(networkConfig),
		)
	}

	objects := []runtime.Object{
		&corev1.NamespaceList{
			Items: []corev1.Namespace{
				*n,
			},
		},
		&corev1.NodeList{Items: []corev1.Node{*testNode}},
		&corev1.PodList{
			Items: []corev1.Pod{
				*pod,
			},
		},
		&nadapi.NetworkAttachmentDefinitionList{
			Items: []nadapi.NetworkAttachmentDefinition{*nad},
		},
	}

	objects = append(objects, extraObjects...)

	fakeOvn.startWithDBSetup(initialDB, objects...)
	podInfo.populateLogicalSwitchCache(fakeOvn)

	// on IC, the test itself spits out the pod with the
	// annotations set, since on production it would be the
	// clustermanager to annotate the pod.
	if !config.OVNKubernetesFeature.EnableInterconnect {
		By("asserting the pod originally does *not* feature the OVN pod networks annotation")
		// pod exists, networks annotations don't
		pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.Background(), podInfo.podName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		_, ok := pod.Annotations[util.OvnPodAnnotationName]
		if ok {
			return fmt.Errorf("expected pod annotation %q", util.OvnPodAnnotationName)
		}
	}
	if err = fakeOvn.networkManager.Start(); err != nil {
		return err
	}

	if err = fakeOvn.controller.WatchNamespaces(); err != nil {
		return err
	}
	if err = fakeOvn.controller.WatchPods(); err != nil {
		return err
	}
	By("asserting the pod (once reconciled) *features* the OVN pod networks annotation")
	userDefinedNetController, doesControllerExist := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
	if !doesControllerExist {
		return fmt.Errorf("expected user-defined network controller to exist")
	}

	userDefinedNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
	podInfo.populateUserDefinedNetworkLogicalSwitchCache(userDefinedNetController)
	if err = userDefinedNetController.bnc.WatchNodes(); err != nil {
		return err
	}
	if err = userDefinedNetController.bnc.WatchPods(); err != nil {
		return err
	}

	return nil
}

func setupConfig(netInfo userDefinedNetInfo, testConfig testConfiguration, gatewayMode config.GatewayMode) {
	if testConfig.configToOverride != nil {
		config.OVNKubernetesFeature = *testConfig.configToOverride
		if testConfig.gatewayConfig != nil {
			config.Gateway.DisableSNATMultipleGWs = testConfig.gatewayConfig.DisableSNATMultipleGWs
		}
	}
	config.Gateway.Mode = gatewayMode
	if knet.IsIPv6CIDRString(netInfo.clustersubnets) {
		config.IPv6Mode = true
		// tests dont support dualstack yet
		config.IPv4Mode = false
	}
}

func notReadyMigrationInfo() *liveMigrationInfo {
	const vmName = "my-vm"
	return &liveMigrationInfo{
		vmName: vmName,
		sourcePodInfo: liveMigrationPodInfo{
			podPhase:           corev1.PodRunning,
			creationTimestamp:  metav1.NewTime(time.Now().Add(-time.Hour)),
			expectedLspEnabled: lspEnableNotSpecified,
		},
		targetPodInfo: liveMigrationPodInfo{
			podPhase:           corev1.PodRunning,
			creationTimestamp:  metav1.NewTime(time.Now()),
			expectedLspEnabled: lspEnableExplicitlyFalse,
		},
	}
}

func readyMigrationInfo() *liveMigrationInfo {
	const vmName = "my-vm"
	return &liveMigrationInfo{
		vmName: vmName,
		sourcePodInfo: liveMigrationPodInfo{
			podPhase:           corev1.PodRunning,
			creationTimestamp:  metav1.NewTime(time.Now().Add(-time.Hour)),
			expectedLspEnabled: lspEnableExplicitlyFalse,
		},
		targetPodInfo: liveMigrationPodInfo{
			podPhase:           corev1.PodRunning,
			creationTimestamp:  metav1.NewTime(time.Now()),
			annotation:         map[string]string{kubevirtv1.MigrationTargetReadyTimestamp: "some-timestamp"},
			expectedLspEnabled: lspEnableExplicitlyTrue,
		},
	}
}

func failedMigrationInfo() *liveMigrationInfo {
	const vmName = "my-vm"
	return &liveMigrationInfo{
		vmName: vmName,
		sourcePodInfo: liveMigrationPodInfo{
			podPhase:           corev1.PodRunning,
			creationTimestamp:  metav1.NewTime(time.Now().Add(-time.Hour)),
			expectedLspEnabled: lspEnableExplicitlyTrue,
		},
		targetPodInfo: liveMigrationPodInfo{
			podPhase:          corev1.PodFailed,
			creationTimestamp: metav1.NewTime(time.Now()),
		},
	}
}
