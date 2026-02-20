package ovn

import (
	"context"
	"fmt"
	"net"
	"strings"
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
	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
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
			if config.OVNKubernetesFeature.EnableInterconnect {
				config.Default.Zone = testICZone
			}
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
				n := testing.NewNamespace(ns)
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
				networkPolicy := testing.NewMatchLabelsNetworkPolicy(denyPolicyName, ns, "", "", false, false)
				nodes := []corev1.Node{*testNode}
				if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
					testNode2, err := newNodeWithUserDefinedNetworks("test-node2", "192.168.127.202/24", netInfo)
					Expect(err).NotTo(HaveOccurred())
					testNode2.Annotations["k8s.ovn.org/zone-name"] = "blah"
					By("adding an extra node that should be ignored by Dynamic UDN Allocation")
					nodes = append(nodes, *testNode2)
				}
				fakeOvn.startWithDBSetup(
					initialDB,
					&corev1.NamespaceList{
						Items: []corev1.Namespace{
							*n,
						},
					},
					&corev1.NodeList{
						Items: nodes,
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
							).expectedLogicalSwitchesAndPorts()...)))

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
		Entry("with dynamic UDN allocation, a remote node with no NAD is ignored",
			dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24"),
			icClusterTestConfiguration(func(config *testConfiguration) {
				config.configToOverride.EnableDynamicUDNAllocation = true
				config.configToOverride.EnableNetworkSegmentation = true
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
			netInfo := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "192.168.1.0/24")
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

			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{}, &corev1.NamespaceList{Items: []corev1.Namespace{*n}},
				&corev1.NodeList{Items: []corev1.Node{*localNode, *remoteNode}},
				&corev1.PodList{Items: []corev1.Pod{localPod}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}})

			Expect(fakeOvn.networkManager.Start()).To(Succeed())
			defer fakeOvn.networkManager.Stop()

			userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[userDefinedNetworkName]
			Expect(ok).To(BeTrue())
			userDefinedNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
			l3Controller, ok := fakeOvn.fullL3UDNControllers[netInfo.netName]
			Expect(ok).To(BeTrue())
			mutableNetInfo := util.NewMutableNetInfo(l3Controller.GetNetInfo())
			mutableNetInfo.SetNetworkID(2)
			err = util.ReconcileNetInfo(l3Controller.ReconcilableNetInfo, mutableNetInfo)
			Expect(err).NotTo(HaveOccurred())
			err = l3Controller.init()
			Expect(err).NotTo(HaveOccurred())
			Expect(userDefinedNetController.bnc.WatchNodes()).To(Succeed())

			By("Remote node should not have a port on transit subnet before activation")
			Consistently(func() bool {
				p := func(item *nbdb.LogicalSwitchPort) bool {
					return item.ExternalIDs["node"] == remoteNode.Name
				}
				portList, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOvn.nbClient, p)
				return err == nil && len(portList) > 0
			}).WithTimeout(500 * time.Millisecond).Should(BeFalse())

			By("Creating a pod on the remote node should activate it")
			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(ns).Create(context.TODO(), &remotePod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				return fakeOvn.networkManager.Interface().NodeHasNetwork(remoteNode.Name, netInfo.netName)
			}).WithTimeout(3 * time.Second).Should(BeTrue())
			By("Triggering networkRefChange callback after updating remote node as active on NAD")
			l3Controller.HandleNetworkRefChange(remoteNode.Name, true)

			By("Remote node should have a port created on transit subnet")
			Eventually(func() bool {
				p := func(item *nbdb.LogicalSwitchPort) bool {
					return item.ExternalIDs["node"] == remoteNode.Name
				}
				portList, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOvn.nbClient, p)
				if err == nil && len(portList) > 0 {
					return true
				}
				return false
			}).Should(BeTrue())

			By("Deleting a pod on the remote node should set it as inactive")
			err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(ns).Delete(context.TODO(), remotePod.Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				return fakeOvn.networkManager.Interface().NodeHasNetwork(remoteNode.Name, netInfo.netName)
			}).WithTimeout(3 * time.Second).Should(BeFalse())
			By("Triggering networkRefChange callback after updating remote node as inactive on NAD")
			l3Controller.HandleNetworkRefChange(remoteNode.Name, false)
			By("Remote node should not have a port on transit subnet")
			Eventually(func() bool {
				p := func(item *nbdb.LogicalSwitchPort) bool {
					return item.ExternalIDs["node"] == remoteNode.Name
				}
				portList, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOvn.nbClient, p)
				if err == nil && len(portList) > 0 {
					return true
				}
				return false
			}).WithTimeout(3 * time.Second).Should(BeFalse())

			By("Verifying core gateway router and cluster router are intact after remote node removal")
			hasPort := func(ports []string, target string) bool {
				for _, port := range ports {
					if port == target {
						return true
					}
				}
				return false
			}
			hasRouterPorts := func(routerName string, portNames ...string) bool {
				routers, err := libovsdbops.FindLogicalRoutersWithPredicate(fakeOvn.nbClient, func(router *nbdb.LogicalRouter) bool {
					return router.Name == routerName
				})
				if err != nil || len(routers) == 0 {
					return false
				}
				router := routers[0]
				for _, portName := range portNames {
					ports, err := libovsdbops.FindLogicalRouterPortWithPredicate(fakeOvn.nbClient, func(port *nbdb.LogicalRouterPort) bool {
						return port.Name == portName
					})
					if err != nil || len(ports) == 0 {
						return false
					}
					if !hasPort(router.Ports, ports[0].UUID) {
						return false
					}
				}
				return true
			}
			hasSwitchPorts := func(switchName string, portNames ...string) bool {
				switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(fakeOvn.nbClient, func(sw *nbdb.LogicalSwitch) bool {
					return sw.Name == switchName
				})
				if err != nil || len(switches) == 0 {
					return false
				}
				sw := switches[0]
				for _, portName := range portNames {
					ports, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOvn.nbClient, func(port *nbdb.LogicalSwitchPort) bool {
						return port.Name == portName
					})
					if err != nil || len(ports) == 0 {
						return false
					}
					if !hasPort(sw.Ports, ports[0].UUID) {
						return false
					}
				}
				return true
			}
			Eventually(func() bool {
				return hasRouterPorts(
					"GR_isolatednet_test-node",
					"rtoj-GR_isolatednet_test-node",
					"rtoe-GR_isolatednet_test-node",
				) &&
					hasRouterPorts(
						"isolatednet_ovn_cluster_router",
						"rtoj-isolatednet_ovn_cluster_router",
						"rtos-isolatednet_test-node",
						"isolatednet_rtots-test-node",
					) &&
					hasSwitchPorts(
						"isolatednet_join",
						"jtor-GR_isolatednet_test-node",
						"jtor-isolatednet_ovn_cluster_router",
					) &&
					hasSwitchPorts(
						"isolatednet_transit_switch",
						"isolatednet_tstor-test-node",
					)
			}).WithTimeout(3 * time.Second).Should(BeTrue())
		})

		It("activates a remote node when a CUDN NAD becomes active in another namespace", func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
			config.OVNKubernetesFeature.EnableInterconnect = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.Default.Zone = testICZone
			config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"

			const (
				cudnName   = "cudn-shared"
				nsA        = "namespace-a"
				nsB        = "namespace-b"
				nadAName   = "cudn-nad-a"
				nadBName   = "cudn-nad-b"
				remoteName = "remoteNode"
			)

			netName := util.GenerateCUDNNetworkName(cudnName)
			netInfoA := userDefinedNetInfo{
				netName:        netName,
				nadName:        namespacedName(nsA, nadAName),
				topology:       types.Layer3Topology,
				clustersubnets: "192.168.0.0/16",
				hostsubnets:    "192.168.1.0/24",
				isPrimary:      true,
			}
			netInfoB := userDefinedNetInfo{
				netName:        netName,
				nadName:        namespacedName(nsB, nadBName),
				topology:       types.Layer3Topology,
				clustersubnets: "192.168.0.0/16",
				hostsubnets:    "192.168.2.0/24",
				isPrimary:      true,
			}

			nsObjA := newUDNNamespace(nsA)
			nsObjB := newUDNNamespace(nsB)
			nadA, err := newNetworkAttachmentDefinition(nsA, nadAName, *netInfoA.netconf())
			Expect(err).NotTo(HaveOccurred())
			nadA.OwnerReferences = []metav1.OwnerReference{makeCUDNOwnerRef(cudnName)}
			nadB, err := newNetworkAttachmentDefinition(nsB, nadBName, *netInfoB.netconf())
			Expect(err).NotTo(HaveOccurred())
			nadB.OwnerReferences = []metav1.OwnerReference{makeCUDNOwnerRef(cudnName)}

			localNode, err := newNodeWithUserDefinedNetworks(nodeName, "192.168.126.202/24", netInfoA)
			Expect(err).NotTo(HaveOccurred())
			localNode.Annotations[util.OvnTransitSwitchPortAddr] = `{"ipv4":"100.88.0.3/16"}`

			remoteNode, err := newNodeWithUserDefinedNetworks(remoteName, "192.168.127.202/24", netInfoB)
			Expect(err).NotTo(HaveOccurred())
			remoteNode.Annotations["k8s.ovn.org/zone-name"] = "other-zone"
			remoteNode.Annotations[util.OvnTransitSwitchPortAddr] = `{"ipv4":"100.88.0.4/16"}`

			localPod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "local-pod",
					Namespace: nsA,
				},
				Spec: corev1.PodSpec{
					NodeName:   localNode.Name,
					Containers: []corev1.Container{{Name: "c", Image: "scratch"}},
				},
			}

			remotePod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "remote-pod",
					Namespace: nsB,
				},
				Spec: corev1.PodSpec{
					NodeName:   remoteNode.Name,
					Containers: []corev1.Container{{Name: "c", Image: "scratch"}},
				},
			}

			fakeOvn.startWithDBSetup(libovsdbtest.TestSetup{}, &corev1.NamespaceList{Items: []corev1.Namespace{*nsObjA, *nsObjB}},
				&corev1.NodeList{Items: []corev1.Node{*localNode, *remoteNode}},
				&corev1.PodList{Items: []corev1.Pod{localPod}},
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nadA, *nadB}},
			)

			Expect(fakeOvn.networkManager.Start()).To(Succeed())
			defer fakeOvn.networkManager.Stop()

			userDefinedNetController, ok := fakeOvn.userDefinedNetworkControllers[netName]
			Expect(ok).To(BeTrue())
			userDefinedNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
			l3Controller, ok := fakeOvn.fullL3UDNControllers[netName]
			Expect(ok).To(BeTrue())

			mutableNetInfo := util.NewMutableNetInfo(l3Controller.GetNetInfo())
			mutableNetInfo.SetNetworkID(2)
			err = util.ReconcileNetInfo(l3Controller.ReconcilableNetInfo, mutableNetInfo)
			Expect(err).NotTo(HaveOccurred())
			Expect(l3Controller.init()).To(Succeed())
			Expect(userDefinedNetController.bnc.WatchNodes()).To(Succeed())

			By("Remote node should not have a port on transit subnet before activation")
			Consistently(func() bool {
				p := func(item *nbdb.LogicalSwitchPort) bool {
					return item.ExternalIDs["node"] == remoteNode.Name
				}
				portList, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOvn.nbClient, p)
				return err == nil && len(portList) > 0
			}).WithTimeout(500 * time.Millisecond).Should(BeFalse())

			By("Creating a pod on the remote node in another namespace should activate it")
			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nsB).Create(context.TODO(), &remotePod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				return fakeOvn.networkManager.Interface().NodeHasNetwork(remoteNode.Name, netName)
			}).WithTimeout(3 * time.Second).Should(BeTrue())
			By("Triggering networkRefChange callback after updating remote node as active on NAD")
			l3Controller.HandleNetworkRefChange(remoteNode.Name, true)

			By("Remote node should have a port created on transit subnet")
			Eventually(func() bool {
				p := func(item *nbdb.LogicalSwitchPort) bool {
					return item.ExternalIDs["node"] == remoteNode.Name
				}
				portList, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOvn.nbClient, p)
				return err == nil && len(portList) > 0
			}).Should(BeTrue())
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

func makeCUDNOwnerRef(name string) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: userdefinednetworkv1.SchemeGroupVersion.String(),
		Kind:       "ClusterUserDefinedNetwork",
		Name:       name,
		Controller: &controller,
	}
}

func getNetworkRole(netInfo util.NetInfo) string {
	return util.GetUserDefinedNetworkRole(netInfo.IsPrimaryNetwork())
}

func (sni *userDefinedNetInfo) setupOVNDependencies(dbData *libovsdbtest.TestSetup) error {
	netInfo, err := util.NewNetInfo(sni.netconf())
	if err != nil {
		return err
	}

	externalIDs := util.GenerateExternalIDsForSwitchOrRouter(netInfo)
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

	zone := types.OvnDefaultZone
	if config.OVNKubernetesFeature.EnableInterconnect {
		zone = testICZone
	}

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				util.Layer2TopologyVersion:                                  util.TransitRouterTopoVersion,
				"k8s.ovn.org/node-primary-ifaddr":                           fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4CIDR, ""),
				"k8s.ovn.org/node-subnets":                                  parsedNodeSubnets,
				util.OVNNodeHostCIDRs:                                       fmt.Sprintf("[\"%s\"]", nodeIPv4CIDR),
				"k8s.ovn.org/zone-name":                                     zone,
				"k8s.ovn.org/l3-gateway-config":                             fmt.Sprintf("{\"default\":{\"mode\":\"shared\",\"bridge-id\":\"breth0\",\"interface-id\":\"breth0_ovn-worker\",\"mac-address\":%q,\"ip-addresses\":[%[2]q],\"ip-address\":%[2]q,\"next-hops\":[%[3]q],\"next-hop\":%[3]q,\"node-port-enable\":\"true\",\"vlan-id\":\"0\"}}", util.IPAddrToHWAddr(nodeIP), nodeCIDR, nextHopIP),
				util.OvnNodeChassisID:                                       chassisIDForNode(nodeName),
				"k8s.ovn.org/network-ids":                                   fmt.Sprintf("{\"default\":\"0\",\"isolatednet\":\"%s\"}", userDefinedNetworkID),
				util.OvnNodeID:                                              "4",
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
