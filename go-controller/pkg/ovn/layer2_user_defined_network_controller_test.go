package ovn

import (
	"context"
	"fmt"
	"net"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/urfave/cli/v2"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	knet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	ovnkcnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	testnm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
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
				nodes := []corev1.Node{*testNode}
				if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
					testNode2, err := newNodeWithUserDefinedNetworks("test-node2", "192.168.127.202/24", netInfo)
					Expect(err).NotTo(HaveOccurred())
					testNode2.Annotations["k8s.ovn.org/zone-name"] = "blah"
					By("adding an extra node that should be ignored by Dynamic UDN Allocation")
					nodes = append(nodes, *testNode2)
				}
				Expect(setupFakeOvnForLayer2Topology(fakeOvn, initialDB, netInfo, nodes, podInfo, pod)).To(Succeed())
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
						).expectedLogicalSwitchesAndPorts()...))

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
		Entry("with dynamic UDN allocation, a remote node with no NAD is ignored",
			dummyLayer2PrimaryUserDefinedNetwork("100.200.0.0/16"),
			icClusterTestConfiguration(func(config *testConfiguration) {
				config.configToOverride.EnableDynamicUDNAllocation = true
				config.configToOverride.EnableNetworkSegmentation = true
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

				Expect(setupFakeOvnForLayer2Topology(fakeOvn, initialDB, netInfo, []corev1.Node{*testNode}, sourcePodInfo, sourcePod,
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
						).expectedLogicalSwitchesAndPorts()...))

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
						).expectedLogicalSwitchesAndPortsWithLspEnabled(expectedPodLspEnabled)...))
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
			setupConfig(netInfo, testConfig, config.GatewayModeShared)
			config.OVNKubernetesFeature.EnableMultiNetwork = true
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
				nbZone := &nbdb.NBGlobal{Name: config.Default.Zone, UUID: config.Default.Zone}

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
						expectedLayer2EgressEntities(networkConfig, *gwConfig, networkConfig.Subnets()[0].CIDR, false)...)
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

	It("primary layer 2 UDN: controller creates entities via init/watchers, then dummy Cleanup() removes them", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		setupConfig(dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16"), testConfiguration{}, config.GatewayModeShared)
		app.Action = func(ctx *cli.Context) error {
			netInfo := dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16")
			netConf := netInfo.netconf()
			networkConfig, err := util.NewNetInfo(netConf)
			Expect(err).NotTo(HaveOccurred())
			mutableNetInfo := util.NewMutableNetInfo(networkConfig)
			mutableNetInfoCleanup := util.NewMutableNetInfo(networkConfig)
			mutableNetInfoCleanup.SetNetworkID(ovntypes.InvalidID)

			nad, err := newNetworkAttachmentDefinition(ns, nadName, *netConf)
			Expect(err).NotTo(HaveOccurred())
			fakeNetworkManager := &testnm.FakeNetworkManager{
				PrimaryNetworks: map[string]util.NetInfo{},
			}
			fakeNetworkManager.PrimaryNetworks[ns] = mutableNetInfo

			const nodeIPv4CIDR = "192.168.126.202/24"
			testNode, err := newNodeWithUserDefinedNetworks(nodeName, nodeIPv4CIDR, netInfo)
			Expect(err).NotTo(HaveOccurred())
			nbZone := &nbdb.NBGlobal{Name: config.Default.Zone, UUID: config.Default.Zone}

			// Minimal initialDB: no UDN entities. init() + watchers create them.
			initialDB.NBData = append(initialDB.NBData, nbZone)
			Expect(netInfo.setupOVNDependencies(&initialDB)).To(Succeed())

			fakeOvn.startWithDBSetup(
				initialDB,
				&corev1.NamespaceList{Items: []corev1.Namespace{*newUDNNamespace(ns)}},
				&corev1.NodeList{Items: []corev1.Node{*testNode}},
				&corev1.PodList{Items: []corev1.Pod{}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)

			Expect(fakeOvn.networkManager.Start()).To(Succeed())
			defer fakeOvn.networkManager.Stop()
			Expect(fakeOvn.controller.WatchNamespaces()).To(Succeed())
			Expect(fakeOvn.controller.WatchPods()).To(Succeed())

			// Run init() to create cluster-level entities, then watchers so node sync creates per-node entities.
			l2Controller, ok := fakeOvn.fullL2UDNControllers[userDefinedNetworkName]
			Expect(ok).To(BeTrue())
			Expect(l2Controller.init()).To(Succeed())
			udnNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
			Expect(ok).To(BeTrue())
			udnNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
			Expect(l2Controller.WatchNodes()).To(Succeed())
			Expect(l2Controller.WatchPods()).To(Succeed())
			Expect(l2Controller.WatchNetworkPolicy()).To(Succeed())

			// Wait for the controller to create the Layer2 switch.
			udnLSName := l2Controller.GetNetworkScopedSwitchName(ovntypes.OVNLayer2Switch)
			Eventually(func(g Gomega) {
				switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(fakeOvn.nbClient, func(ls *nbdb.LogicalSwitch) bool {
					return ls.Name == udnLSName
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(switches).NotTo(BeEmpty())
			}).WithTimeout(10 * time.Second).Should(Succeed())

			// Assert gateway router was created before cleanup.
			udnGWRouterName := l2Controller.GetNetworkScopedGWRouterName(nodeName)
			Eventually(func(g Gomega) {
				routers, err := libovsdbops.FindLogicalRoutersWithPredicate(fakeOvn.nbClient, func(lr *nbdb.LogicalRouter) bool {
					return lr.Name == udnGWRouterName
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(routers).NotTo(BeEmpty())
			}).WithTimeout(10 * time.Second).Should(Succeed())

			// Dummy controller with InvalidID runs Cleanup() to remove all entities for this network.
			dummyController, err := NewLayer2UserDefinedNetworkController(
				&l2Controller.CommonNetworkControllerInfo,
				mutableNetInfoCleanup,
				fakeOvn.networkManager.Interface(),
				nil,
				NewPortCache(ctx.Done()),
				nil,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(dummyController.Cleanup()).To(Succeed())
			Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(generateUDNPostInitDB([]libovsdbtest.TestData{nbZone})))
			return nil
		}
		Expect(app.Run([]string{app.Name})).To(Succeed())
	})

	It("controller should cleanup stale nodes on startup", func() {
		app.Action = func(*cli.Context) error {
			netInfo := dummyLayer2PrimaryUserDefinedNetwork("192.168.0.0/16")
			netConf := netInfo.netconf()
			networkConfig, err := util.NewNetInfo(netConf)
			Expect(err).NotTo(HaveOccurred())

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

			n := newUDNNamespace(ns)
			initialDB.NBData = append(
				initialDB.NBData,
				expectedGWEntitiesLayer2(nodeName, networkConfig, *gwConfig)...)
			initialDB.NBData = append(initialDB.NBData, nbZone)
			// save current state of DB, it will be preserved through the test
			finalDB := append([]libovsdbtest.TestData{}, initialDB.NBData...)
			// add stale node to the initial db
			initialDB.NBData = append(
				initialDB.NBData,
				expectedLayer2EgressEntities(networkConfig, *gwConfig, networkConfig.Subnets()[0].CIDR, true)...)
			// final db should not have the stale node
			finalDB = append(finalDB, expectedLayer2EgressEntities(networkConfig, *gwConfig, networkConfig.Subnets()[0].CIDR, false)...)

			// the db setup doesn't have layer2 switch, which makes nodeAdd fail, but simplifies the test
			// we only care about the initial Sync for nodes
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
				&nadapi.NetworkAttachmentDefinitionList{
					Items: []nadapi.NetworkAttachmentDefinition{*nad},
				},
			)

			udnNetController, ok := fakeOvn.fullL2UDNControllers[userDefinedNetworkName]
			Expect(ok).To(BeTrue())
			// start watching nodes to trigger initial node cleanup
			Expect(udnNetController.WatchNodes()).To(Succeed())
			Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalDB))
			// check if the remoteNodesNoRouter map is empty
			isEmpty := true
			udnNetController.remoteNodesNoRouter.Range(func(_, _ interface{}) bool {
				isEmpty = false // A key was found, so it's not empty
				return false    // Stop iterating immediately
			})
			Expect(isEmpty).To(BeTrue())
			return nil
		}
		Expect(app.Run([]string{app.Name})).To(Succeed())
	})

	It("controller should correctly assigns dummy joinSubnet IPs", func() {
		config.IPv6Mode = true
		// add a fake node with last-joinIP nodeID to make sure that large subnets don't check for nodeIDs at all
		testNode := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test",
				Annotations: map[string]string{
					ovnNodeID: "65534",
				},
			},
		}
		fakeOvn.startWithDBSetup(initialDB, &corev1.NodeList{Items: []corev1.Node{*testNode}})
		controller := &Layer2UserDefinedNetworkController{}
		controller.watchFactory = fakeOvn.watcher
		// this network won't invoke nodeID check, so it should pass
		netInfo, err := util.NewNetInfo(&ovnkcnitypes.NetConf{
			NetConf:    cnitypes.NetConf{Name: "test"},
			Topology:   ovntypes.Layer2Topology,
			JoinSubnet: "100.65.0.0/16,fd99::/64",
		})
		Expect(err).NotTo(HaveOccurred())
		controller.ReconcilableNetInfo = util.NewReconcilableNetInfo(netInfo)
		res, err := controller.getLastJoinIPs()
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(HaveLen(2))
		Expect(res).To(Equal([]*net.IPNet{
			{IP: net.ParseIP("100.65.255.254"), Mask: net.CIDRMask(16, 32)},
			{IP: net.ParseIP("fd99::ffff:ffff:ffff:fffe"), Mask: net.CIDRMask(64, 128)},
		}))
		// this network has a small subnet, it will do the nodeID check
		// it will fail if there is a node with nodeID 1022, which doesn't exist for now
		netInfo, err = util.NewNetInfo(&ovnkcnitypes.NetConf{
			NetConf:    cnitypes.NetConf{Name: "test"},
			Topology:   ovntypes.Layer2Topology,
			JoinSubnet: "100.65.0.0/22",
		})
		Expect(err).NotTo(HaveOccurred())
		controller.ReconcilableNetInfo = util.NewReconcilableNetInfo(netInfo)
		res, err = controller.getLastJoinIPs()
		Expect(err).ToNot(HaveOccurred())
		Expect(res).To(Equal([]*net.IPNet{
			{IP: net.ParseIP("100.65.3.254"), Mask: net.CIDRMask(22, 32)},
			{IP: net.ParseIP("fd99::ffff:ffff:ffff:fffe"), Mask: net.CIDRMask(64, 128)},
		}))
		// now update the node to have a last-IP nodeID
		testNode.Annotations[ovnNodeID] = "1022"
		_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), testNode, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		// wait for node update to be propagated to the watchFactory
		time.Sleep(10 * time.Millisecond)
		_, err = controller.getLastJoinIPs()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cannot use the last IP of the join subnet"))
	})

	Describe("Dynamic UDN allocation with remote node", func() {
		It("activates a remote node when a NAD becomes active and cleans it up when inactive", func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
			config.OVNKubernetesFeature.EnableInterconnect = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.Default.Zone = testICZone
			config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"

			// Basic UDN setup
			netInfo := dummyLayer2PrimaryUserDefinedNetwork("100.200.0.0/16")
			n := newUDNNamespace(ns)
			nad, err := newNetworkAttachmentDefinition(ns, nadName, *netInfo.netconf())
			Expect(err).NotTo(HaveOccurred())

			// Local node and remote node with NAD
			localNode, err := newNodeWithUserDefinedNetworks(nodeName, "192.168.126.202/24", netInfo)
			Expect(err).NotTo(HaveOccurred())
			localNode.Annotations[util.OvnTransitSwitchPortAddr] = `{"ipv4":"100.88.0.3/16"}`

			remoteNode, err := newNodeWithUserDefinedNetworks("remoteNode", "192.168.127.202/24", netInfo)
			Expect(err).NotTo(HaveOccurred())
			remoteNode.Annotations["k8s.ovn.org/zone-name"] = "other-zone" // force remote
			remoteNode.Annotations[util.OvnTransitSwitchPortAddr] = `{"ipv4":"100.88.0.4/16"}`

			remotePod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "remote-pod",
					Namespace: ns,
				},
				Spec: corev1.PodSpec{
					NodeName:   remoteNode.Name,
					Containers: []corev1.Container{{Name: "c", Image: "scratch"}},
				},
			}

			localPod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-pod",
					Namespace: ns,
				},
				Spec: corev1.PodSpec{
					NodeName:   localNode.Name,
					Containers: []corev1.Container{{Name: "c", Image: "scratch"}},
				},
			}

			// Preload DB
			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{}, &corev1.NamespaceList{Items: []corev1.Namespace{*n}},
				&corev1.NodeList{Items: []corev1.Node{*localNode, *remoteNode}},
				&corev1.PodList{Items: []corev1.Pod{localPod}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}})

			Expect(fakeOvn.networkManager.Start()).To(Succeed())
			defer fakeOvn.networkManager.Stop()

			userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
			Expect(ok).To(BeTrue())
			userDefinedNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
			l2Controller, ok := fakeOvn.fullL2UDNControllers[netInfo.netName]
			Expect(ok).To(BeTrue())
			mutableNetInfo := util.NewMutableNetInfo(l2Controller.GetNetInfo())
			mutableNetInfo.SetNetworkID(2)
			err = util.ReconcileNetInfo(l2Controller.ReconcilableNetInfo, mutableNetInfo)
			Expect(err).NotTo(HaveOccurred())
			err = l2Controller.init()
			Expect(err).NotTo(HaveOccurred())
			Expect(userDefinedNetController.bnc.WatchNodes()).To(Succeed())

			By("Remote node should not have a transit-router port before activation")
			Consistently(func() bool {
				p := func(item *nbdb.LogicalRouterPort) bool {
					return item.ExternalIDs[ovntypes.NodeExternalID] == remoteNode.Name && item.ExternalIDs[ovntypes.NetworkExternalID] == l2Controller.GetNetworkName()
				}
				ports, err := libovsdbops.FindLogicalRouterPortWithPredicate(fakeOvn.nbClient, p)
				return err == nil && len(ports) > 0
			}).WithTimeout(500 * time.Millisecond).Should(BeFalse())

			By("Creating a pod on the remote node should activate it")
			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(ns).Create(context.TODO(), &remotePod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				return fakeOvn.networkManager.Interface().NodeHasNetwork(remoteNode.Name, netInfo.netName)
			}).WithTimeout(3 * time.Second).Should(BeTrue())
			By("Triggering networkRefChange callback after updating remote node as active on NAD")
			l2Controller.HandleNetworkRefChange(remoteNode.Name, true)

			By("Remote node should have a transit-router port created")
			Eventually(func() bool {
				p := func(item *nbdb.LogicalRouterPort) bool {
					return item.ExternalIDs[ovntypes.NodeExternalID] == remoteNode.Name && item.ExternalIDs[ovntypes.NetworkExternalID] == l2Controller.GetNetworkName()
				}
				ports, err := libovsdbops.FindLogicalRouterPortWithPredicate(fakeOvn.nbClient, p)
				if err == nil && len(ports) > 0 {
					return true
				}
				return false
			}).WithTimeout(3 * time.Second).Should(BeTrue())

			By("Deleting a pod on the remote node should set it as inactive")
			err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(ns).Delete(context.TODO(), remotePod.Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				return fakeOvn.networkManager.Interface().NodeHasNetwork(remoteNode.Name, netInfo.netName)
			}).WithTimeout(3 * time.Second).Should(BeFalse())
			By("Triggering networkRefChange callback after updating remote node as inactive on NAD")
			l2Controller.HandleNetworkRefChange(remoteNode.Name, false)
			By("Remote node should not have a port on transit subnet")
			Eventually(func() bool {
				p := func(item *nbdb.LogicalRouterPort) bool {
					return item.ExternalIDs[ovntypes.NodeExternalID] == remoteNode.Name && item.ExternalIDs[ovntypes.NetworkExternalID] == l2Controller.GetNetworkName()
				}
				ports, err := libovsdbops.FindLogicalRouterPortWithPredicate(fakeOvn.nbClient, p)
				if err == nil && len(ports) > 0 {
					return true
				}
				return false
			}).WithTimeout(3 * time.Second).Should(BeFalse())

			By("verifying that local node trtos and stotr ports still exist after remote node removal")
			expectedLRP := &nbdb.LogicalRouterPort{
				Name: "trtos-isolatednet_ovn_layer2_switch",
				MAC:  "0a:58:64:c8:00:01",
				GatewayChassis: []string{
					"00000000-0000-0000-0000-000000000000",
				},
				Networks: []string{
					"100.200.0.1/16",
				},
				Options: map[string]string{
					"gateway_mtu":       "1400",
					"requested-tnl-key": "1",
				},
			}

			expectedLSP := &nbdb.LogicalSwitchPort{
				Name:      "stotr-isolatednet_ovn_layer2_switch",
				Type:      "router",
				Addresses: []string{"router"},
				Options: map[string]string{
					"router-port": "trtos-isolatednet_ovn_layer2_switch",
				},
				ExternalIDs: map[string]string{
					"k8s.ovn.org/network":  "isolatednet",
					"k8s.ovn.org/topology": "layer2",
				},
			}

			Eventually(fakeOvn.nbClient).WithTimeout(3 * time.Second).Should(
				libovsdbtest.HaveDataSubset([]libovsdbtest.TestData{expectedLRP, expectedLSP}),
			)
		})

		It("does not filter pods from other namespaces of the same primary UDN", func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
			config.OVNKubernetesFeature.EnableInterconnect = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.Default.Zone = testICZone

			netInfo := dummyLayer2PrimaryUserDefinedNetwork("100.200.0.0/16")
			nsA := "namespace-a"
			nsB := "namespace-b"
			nsAObj := newUDNNamespace(nsA)
			nsBObj := newUDNNamespace(nsB)

			netInfoA := netInfo
			netInfoA.nadName = namespacedName(nsA, nadName)
			netInfoB := netInfo
			netInfoB.nadName = namespacedName(nsB, nadName)

			nadA, err := newNetworkAttachmentDefinition(nsA, nadName, *netInfoA.netconf())
			Expect(err).NotTo(HaveOccurred())
			nadB, err := newNetworkAttachmentDefinition(nsB, nadName, *netInfoB.netconf())
			Expect(err).NotTo(HaveOccurred())

			parsedNetInfoA, err := util.NewNetInfo(netInfoA.netconf())
			Expect(err).NotTo(HaveOccurred())
			mutableA := util.NewMutableNetInfo(parsedNetInfoA)
			mutableA.SetNADs(namespacedName(nsA, nadName))

			parsedNetInfoB, err := util.NewNetInfo(netInfoB.netconf())
			Expect(err).NotTo(HaveOccurred())
			mutableB := util.NewMutableNetInfo(parsedNetInfoB)
			mutableB.SetNADs(namespacedName(nsB, nadName))

			fakeOvn.networkManager = &testnm.FakeNetworkManager{
				PrimaryNetworks: map[string]util.NetInfo{
					nsA: mutableA,
					nsB: mutableB,
				},
				NADNetworks: map[string]util.NetInfo{
					namespacedName(nsA, nadName): mutableA,
					namespacedName(nsB, nadName): mutableB,
				},
			}

			localNode, err := newNodeWithUserDefinedNetworks(nodeName, "192.168.126.202/24", netInfo)
			Expect(err).NotTo(HaveOccurred())

			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{},
				&corev1.NamespaceList{Items: []corev1.Namespace{*nsAObj, *nsBObj}},
				&corev1.NodeList{Items: []corev1.Node{*localNode}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nadA, *nadB}},
			)

			Expect(fakeOvn.NewUserDefinedNetworkController(nadB)).To(Succeed())
			l2Controller, ok := fakeOvn.fullL2UDNControllers[netInfo.netName]
			Expect(ok).To(BeTrue())
			mutableNetInfo := util.NewMutableNetInfo(l2Controller.GetNetInfo())
			mutableNetInfo.SetNADs(namespacedName(nsB, nadName))
			err = util.ReconcileNetInfo(l2Controller.ReconcilableNetInfo, mutableNetInfo)
			Expect(err).NotTo(HaveOccurred())
			By("confirming the controller only tracks the local namespace NAD")
			Expect(l2Controller.GetNetInfo().GetNADNamespaces()).To(ConsistOf(nsB))

			remotePod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "remote-pod",
					Namespace: nsA,
				},
				Spec: corev1.PodSpec{
					NodeName:   localNode.Name,
					Containers: []corev1.Container{{Name: "c", Image: "scratch"}},
				},
			}

			By("ensuring the pod is not filtered out by the UDN controller")
			Expect(l2Controller.FilterOutResource(factory.PodType, remotePod)).To(BeFalse())
		})
	})
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
	// nodeID is hardcoded in newNodeWithSecondaryNets
	return getTestTransitRouterInfoWithNodeID(netInfo, "4")
}

func getTestTransitRouterInfoWithNodeID(netInfo util.NetInfo, nodeID string) *transitRouterInfo {
	transitRouterInfo, err := getTransitRouterInfo(netInfo, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				ovnNodeID: nodeID,
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

func expectedLayer2EgressEntities(netInfo util.NetInfo, gwConfig util.L3GatewayConfig, nodeSubnet *net.IPNet, staleNode bool) []libovsdbtest.TestData {
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
			Options: map[string]string{
				libovsdbops.GatewayMTU:      "1400",
				libovsdbops.RequestedTnlKey: "1", // as defined by getTransitRouterPortTunnelKey(nodeID)
			},
		},
		&nbdb.LogicalRouterPort{
			UUID:        rtorLRPUUID,
			Name:        rtorLRPName,
			Networks:    []string{trInfo.transitRouterNets[0].String()},
			MAC:         util.IPAddrToHWAddr(trInfo.transitRouterNets[0].IP).String(),
			Options:     map[string]string{libovsdbops.RequestedTnlKey: "14"},
			Peer:        ptr.To(fmt.Sprintf("%s%s", ovntypes.RouterToTransitRouterPrefix, gwRouterName)),
			ExternalIDs: standardNonDefaultNetworkExtIDs(netInfo),
		},
		expectedGRStaticRoute(staticRouteUUID1, nodeSubnet.String(), lrsrNextHop, &nbdb.LogicalRouterStaticRoutePolicySrcIP, nil, netInfo),
		expectedGRStaticRoute(staticRouteUUID2, gwRouterJoinIPAddress().IP.String(), trInfo.gatewayRouterNets[0].IP.String(), nil, nil, netInfo),
		expectedLogicalRouterPolicy(routerPolicyUUID1, netInfo, nodeName, nodeIP, managementPortIP(nodeSubnet).String()),
		masqSNAT,
		&nbdb.GatewayChassis{UUID: gatewayChassisUUID, Name: gwChassisName, Priority: 1, ChassisName: gwConfig.ChassisID},
	}
	if staleNode {
		staleNodeName := "stale-node"
		staleNodeChassisID := chassisIDForNode("stale-node")
		// create remote router port
		remoteRouterName := fmt.Sprintf("GR_%s_%s", netInfo.GetNetworkName(), staleNodeName)
		remotePortName := fmt.Sprintf("%s%s", ovntypes.TransitRouterToRouterPrefix, remoteRouterName)
		// use a different nodeID to avoid collisions with the real node
		remoteTRInfo := getTestTransitRouterInfoWithNodeID(netInfo, "5")
		externalIDs := standardNonDefaultNetworkExtIDs(netInfo)
		externalIDs[ovntypes.NodeExternalID] = staleNodeName
		remotePort := &nbdb.LogicalRouterPort{
			UUID:     remotePortName + "-UUID",
			Name:     remotePortName,
			Networks: []string{remoteTRInfo.transitRouterNets[0].String()},
			MAC:      util.IPAddrToHWAddr(remoteTRInfo.transitRouterNets[0].IP).String(),
			Options: map[string]string{
				libovsdbops.RequestedTnlKey:  "15", // as defined by getTransitRouterPortTunnelKey(nodeID)
				libovsdbops.RequestedChassis: staleNodeChassisID},
			ExternalIDs: externalIDs,
		}
		expectedEntities = append(expectedEntities, remotePort)
		// create remote route
		remoteRoute := &nbdb.LogicalRouterStaticRoute{
			UUID:        "remote-route-UUID",
			ExternalIDs: externalIDs,
			IPPrefix:    "100.65.0.5",
			Nexthop:     remoteTRInfo.gatewayRouterNets[0].IP.String(),
		}
		expectedEntities = append(expectedEntities, remoteRoute)
		// update transit router to reference the port and route
		trRouter := expectedEntities[0].(*nbdb.LogicalRouter)
		trRouter.Ports = append(trRouter.Ports, remotePort.UUID)
		trRouter.StaticRoutes = append(trRouter.StaticRoutes, remoteRoute.UUID)
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

func setupFakeOvnForLayer2Topology(fakeOvn *FakeOVN, initialDB libovsdbtest.TestSetup, netInfo userDefinedNetInfo, testNodes []corev1.Node, podInfo testPod, pod *corev1.Pod, extraObjects ...runtime.Object) error {
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
		&corev1.NodeList{Items: testNodes},
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
	if config.OVNKubernetesFeature.EnableInterconnect {
		config.Default.Zone = testICZone
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
