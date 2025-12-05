package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

const cudnResourceType = "ClusterUserDefinedNetwork"

var _ = ginkgo.Describe("Network Segmentation: Network Policies", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("network-segmentation")
	f.SkipNamespaceCreation = true

	ginkgo.Context("on a user defined primary network", func() {
		const (
			nadName                      = "tenant-red"
			userDefinedNetworkIPv4Subnet = "172.16.0.0/16" // first subnet in private range 172.16.0.0/12 (rfc1918)
			userDefinedNetworkIPv6Subnet = "2014:100:200::0/60"
			customL2IPv4Gateway          = "172.16.0.3"
			customL2IPv6Gateway          = "2014:100:200::3"
			customL2IPv4ReservedCIDR     = "172.16.1.0/24"
			customL2IPv6ReservedCIDR     = "2014:100:200::100/120"
			customL2IPv4InfraCIDR        = "172.16.0.0/30"
			customL2IPv6InfraCIDR        = "2014:100:200::/122"
			nodeHostnameKey              = "kubernetes.io/hostname"
			workerOneNodeName            = "ovn-worker"
			workerTwoNodeName            = "ovn-worker2"
			port                         = 9000
			randomStringLength           = 5
			nameSpaceYellowSuffix        = "yellow"
			namespaceBlueSuffix          = "blue"
			namespaceRedSuffix           = "red"
			namespaceOrangeSuffix        = "orange"
		)

		var (
			cs                  clientset.Interface
			nadClient           nadclient.K8sCniCncfIoV1Interface
			allowServerPodLabel = map[string]string{"foo": "bar"}
			denyServerPodLabel  = map[string]string{"abc": "xyz"}
		)

		ginkgo.BeforeEach(func() {
			cs = f.ClientSet
			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			f.Namespace = namespace
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			nadClient, err = nadclient.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			namespaceYellow := getNamespaceName(f, nameSpaceYellowSuffix)
			namespaceBlue := getNamespaceName(f, namespaceBlueSuffix)
			namespaceRed := getNamespaceName(f, namespaceRedSuffix)
			namespaceOrange := getNamespaceName(f, namespaceOrangeSuffix)
			for _, namespace := range []string{namespaceYellow, namespaceBlue,
				namespaceRed, namespaceOrange} {
				ginkgo.By("Creating namespace " + namespace)
				ns, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   namespace,
						Labels: map[string]string{RequiredUDNNamespaceLabel: ""},
					},
				}, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				f.AddNamespacesToDelete(ns)
			}
		})

		ginkgo.DescribeTable(
			"pods within namespace should be isolated when deny policy is present",
			func(
				netConfigParams networkAttachmentConfigParams,
				clientPodConfig podConfiguration,
				serverPodConfig podConfiguration,
			) {
				ginkgo.By("Creating the attachment configuration")
				netConfig := newNetworkAttachmentConfig(netConfigParams)
				netConfig.namespace = f.Namespace.Name
				netConfig.cidr = filterCIDRsAndJoin(cs, netConfig.cidr)
				_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					generateNAD(netConfig, f.ClientSet),
					metav1.CreateOptions{},
				)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("creating client/server pods")
				serverPodConfig.namespace = f.Namespace.Name
				clientPodConfig.namespace = f.Namespace.Name
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
				framework.ExpectNoError(err, "")
				if len(nodes.Items) < 2 {
					ginkgo.Skip("requires at least 2 Nodes")
				}
				serverPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}
				clientPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}

				runUDNPod(cs, f.Namespace.Name, serverPodConfig, nil)
				runUDNPod(cs, f.Namespace.Name, clientPodConfig, nil)

				var serverIP string
				for i, cidr := range strings.Split(netConfig.cidr, ",") {
					if cidr != "" {
						ginkgo.By("asserting the server pod has an IP from the configured range")
						serverIP, err = getPodAnnotationIPsForAttachmentByIndex(
							cs,
							f.Namespace.Name,
							serverPodConfig.name,
							namespacedName(f.Namespace.Name, netConfig.name),
							i,
						)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						ginkgo.By(fmt.Sprintf("asserting the server pod IP %v is from the configured range %v", serverIP, cidr))
						subnet, err := getNetCIDRSubnet(cidr)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(inRange(subnet, serverIP)).To(gomega.Succeed())
					}

					ginkgo.By("asserting the *client* pod can contact the server pod exposed endpoint")
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(gomega.Succeed())
				}

				ginkgo.By("creating a \"default deny\" network policy")
				_, err = makeDenyAllPolicy(f, f.Namespace.Name, "deny-all")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("asserting the *client* pod can not contact the server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
				}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

			},
			ginkgo.Entry(
				"in L2 dualstack primary UDN",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer2",
					cidr:     joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:     "primary",
				},
				*podConfig(
					"client-pod",
				),
				*podConfig(
					"server-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
				),
			),
			ginkgo.Entry(
				"in L2 dualstack primary UDN with custom network",
				networkAttachmentConfigParams{
					name:                nadName,
					topology:            "layer2",
					cidr:                joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:                "primary",
					defaultGatewayIPs:   joinStrings(customL2IPv4Gateway, customL2IPv6Gateway),
					reservedCIDRs:       joinStrings(customL2IPv4ReservedCIDR, customL2IPv6ReservedCIDR),
					infrastructureCIDRs: joinStrings(customL2IPv4InfraCIDR, customL2IPv6InfraCIDR),
				},
				*podConfig(
					"client-pod",
				),
				*podConfig(
					"server-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
				),
			),
			ginkgo.Entry(
				"in L3 dualstack primary UDN",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer3",
					cidr:     joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:     "primary",
				},
				*podConfig(
					"client-pod",
				),
				*podConfig(
					"server-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
				),
			),
		)

		ginkgo.DescribeTable(
			"allow ingress traffic to one pod from a particular namespace",
			func(
				topology string,
				clientPodConfig podConfiguration,
				allowServerPodConfig podConfiguration,
				denyServerPodConfig podConfiguration,
			) {

				namespaceYellow := getNamespaceName(f, nameSpaceYellowSuffix)
				namespaceBlue := getNamespaceName(f, namespaceBlueSuffix)
				namespaceRed := getNamespaceName(f, namespaceRedSuffix)
				namespaceOrange := getNamespaceName(f, namespaceOrangeSuffix)

				nad := networkAttachmentConfigParams{
					topology: topology,
					cidr:     joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					// The yellow, blue and red namespaces are going to served by green network.
					// Use random suffix for the network name to avoid race between tests.
					networkName: fmt.Sprintf("%s-%s", "green", rand.String(randomStringLength)),
					role:        "primary",
				}
				filterSupportedNetworkConfig(f.ClientSet, &nad)

				// Use random suffix in net conf name to avoid race between tests.
				netConfName := fmt.Sprintf("sharednet-%s", rand.String(randomStringLength))
				for _, namespace := range []string{namespaceYellow, namespaceBlue} {
					ginkgo.By("creating the attachment configuration for " + netConfName + " in namespace " + namespace)
					netConfig := newNetworkAttachmentConfig(nad)
					netConfig.namespace = namespace
					netConfig.name = netConfName

					_, err := nadClient.NetworkAttachmentDefinitions(namespace).Create(
						context.Background(),
						generateNAD(netConfig, f.ClientSet),
						metav1.CreateOptions{},
					)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}

				ginkgo.By("creating client/server pods")
				allowServerPodConfig.namespace = namespaceYellow
				denyServerPodConfig.namespace = namespaceYellow
				clientPodConfig.namespace = namespaceBlue
				runUDNPod(cs, namespaceYellow, allowServerPodConfig, nil)
				runUDNPod(cs, namespaceYellow, denyServerPodConfig, nil)
				runUDNPod(cs, namespaceBlue, clientPodConfig, nil)

				ginkgo.By("asserting the server pods have an IP from the configured range")
				var allowServerPodIP, denyServerPodIP string
				for i, cidr := range strings.Split(nad.cidr, ",") {
					if cidr == "" {
						continue
					}
					subnet, err := getNetCIDRSubnet(cidr)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					allowServerPodIP, err = getPodAnnotationIPsForAttachmentByIndex(cs, namespaceYellow, allowServerPodConfig.name,
						namespacedName(namespaceYellow, netConfName), i)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					ginkgo.By(fmt.Sprintf("asserting the allow server pod IP %v is from the configured range %v", allowServerPodIP, cidr))
					gomega.Expect(inRange(subnet, allowServerPodIP)).To(gomega.Succeed())
					denyServerPodIP, err = getPodAnnotationIPsForAttachmentByIndex(cs, namespaceYellow, denyServerPodConfig.name,
						namespacedName(namespaceYellow, netConfName), i)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					ginkgo.By(fmt.Sprintf("asserting the deny server pod IP %v is from the configured range %v", denyServerPodIP, cidr))
					gomega.Expect(inRange(subnet, denyServerPodIP)).To(gomega.Succeed())
				}

				ginkgo.By("asserting the *client* pod can contact the allow server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, allowServerPodConfig, clientPodConfig, allowServerPodIP, port)
				}, 2*time.Minute, 6*time.Second).Should(gomega.Succeed())

				ginkgo.By("asserting the *client* pod can contact the deny server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, denyServerPodConfig, clientPodConfig, denyServerPodIP, port)
				}, 2*time.Minute, 6*time.Second).Should(gomega.Succeed())

				ginkgo.By("creating a \"default deny\" network policy")
				_, err := makeDenyAllPolicy(f, namespaceYellow, "deny-all")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("asserting the *client* pod can not contact the allow server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, allowServerPodConfig, clientPodConfig, allowServerPodIP, port)
				}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

				ginkgo.By("asserting the *client* pod can not contact the deny server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, denyServerPodConfig, clientPodConfig, denyServerPodIP, port)
				}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

				ginkgo.By("creating a \"allow-traffic-to-pod\" network policy for blue and red namespace")
				_, err = allowTrafficToPodFromNamespacePolicy(f, namespaceYellow, namespaceBlue, namespaceRed, "allow-traffic-to-pod", allowServerPodLabel)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("asserting the *client* pod can contact the allow server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, allowServerPodConfig, clientPodConfig, allowServerPodIP, port)
				}, 1*time.Minute, 6*time.Second).Should(gomega.Succeed())

				ginkgo.By("asserting the *client* pod can not contact deny server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, denyServerPodConfig, clientPodConfig, denyServerPodIP, port)
				}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

				// Create client pod in red namespace and check network policy is working.
				ginkgo.By("creating client pod in red namespace and check if it is in pending state until NAD is created")
				clientPodConfig.namespace = namespaceRed
				podSpec := generatePodSpec(clientPodConfig)
				_, err = cs.CoreV1().Pods(clientPodConfig.namespace).Create(
					context.Background(),
					podSpec,
					metav1.CreateOptions{},
				)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Consistently(func() v1.PodPhase {
					updatedPod, err := cs.CoreV1().Pods(clientPodConfig.namespace).Get(context.Background(),
						clientPodConfig.name, metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					return updatedPod.Status.Phase
				}, 1*time.Minute, 6*time.Second).Should(gomega.Equal(v1.PodPending))

				// The pod won't run and the namespace address set won't be created until the NAD for the network is added
				// to the namespace and we test here that once that happens the policy is reconciled to account for it.
				ginkgo.By("creating NAD for red and orange namespaces and check pod moves into running state")
				for _, namespace := range []string{namespaceRed, namespaceOrange} {
					ginkgo.By("creating the attachment configuration for " + netConfName + " in namespace " + namespace)
					netConfig := newNetworkAttachmentConfig(nad)
					netConfig.namespace = namespace
					netConfig.name = netConfName

					_, err := nadClient.NetworkAttachmentDefinitions(namespace).Create(
						context.Background(),
						generateNAD(netConfig, f.ClientSet),
						metav1.CreateOptions{},
					)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
				gomega.Eventually(func() v1.PodPhase {
					updatedPod, err := cs.CoreV1().Pods(clientPodConfig.namespace).Get(context.Background(),
						clientPodConfig.name, metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					return updatedPod.Status.Phase
				}, 1*time.Minute, 6*time.Second).Should(gomega.Equal(v1.PodRunning))

				ginkgo.By("asserting the *red client* pod can contact the allow server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, allowServerPodConfig, clientPodConfig, allowServerPodIP, port)
				}, 1*time.Minute, 6*time.Second).Should(gomega.Succeed())

				ginkgo.By("asserting the *red client* pod can not contact deny server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, denyServerPodConfig, clientPodConfig, denyServerPodIP, port)
				}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

				// Create client pod in orange namespace now and check network policy is working.
				ginkgo.By("creating client pod in orange namespace")
				clientPodConfig.namespace = namespaceOrange
				runUDNPod(cs, namespaceOrange, clientPodConfig, nil)

				ginkgo.By("asserting the *orange client* pod can not contact the allow server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, allowServerPodConfig, clientPodConfig, allowServerPodIP, port)
				}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

				ginkgo.By("asserting the *orange client* pod can not contact deny server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, denyServerPodConfig, clientPodConfig, denyServerPodIP, port)
				}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())
			},
			ginkgo.Entry(
				"in L2 primary UDN",
				"layer2",
				*podConfig(
					"client-pod",
					withNodeSelector(map[string]string{nodeHostnameKey: workerOneNodeName}),
				),
				*podConfig(
					"allow-server-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: workerTwoNodeName}),
					withLabels(allowServerPodLabel),
				),
				*podConfig(
					"deny-server-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: workerTwoNodeName}),
					withLabels(denyServerPodLabel),
				),
			),
			ginkgo.Entry(
				"in L3 primary UDN",
				"layer3",
				*podConfig(
					"client-pod",
					withNodeSelector(map[string]string{nodeHostnameKey: workerOneNodeName}),
				),
				*podConfig(
					"allow-server-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: workerTwoNodeName}),
					withLabels(allowServerPodLabel),
				),
				*podConfig(
					"deny-server-pod",
					withCommand(func() []string {
						return httpServerContainerCmd(port)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: workerTwoNodeName}),
					withLabels(denyServerPodLabel),
				),
			))

		ginkgo.Context("Network policies with static IP and MAC assignments", func() {
			const (
				l2Subnet       = "192.168.40.0/24"
				staticPort     = 8080
				cudnLabel      = "cudn-group"
				cudnLabelValue = "l2-net"
			)
			ginkgo.BeforeEach(func() {
				if IsIPv6Cluster(f.ClientSet) {
					ginkgo.Skip("Test not supported on IPv6 clusters")
				}
			})

			var cudnCleanup func()

			ginkgo.AfterEach(func() {
				if cudnCleanup != nil {
					cudnCleanup()
					cudnCleanup = nil
				}
			})

			// Verify ingress network policies with static IP and MAC assignments
			ginkgo.DescribeTable(
				"ingress network policies in L2 primary UDN with static IP and MAC",
				func(
					pod1Config podConfiguration,
					pod2Config podConfiguration,
					pod3Config podConfiguration,
					pod4Config podConfiguration,
				) {
					const (
						cudnName         = "l2-network-ingress"
						defaultGatewayIP = "192.168.40.1"
					)

					namespaceA1Suffix := "a1"
					namespaceA2Suffix := "a2"
					namespaceA1 := fmt.Sprintf("%s-%s", f.Namespace.Name, namespaceA1Suffix)
					namespaceA2 := fmt.Sprintf("%s-%s", f.Namespace.Name, namespaceA2Suffix)

					// Step 1: Create a CUDN with L2 network
					ginkgo.By("1: Creating ClusterUserDefinedNetwork with Layer2 topology")
					cudn := generateClusterUserDefinedNetwork(
						cudnName,
						cudnLabel,
						cudnLabelValue,
						"Layer2",
						"Primary",
						[]string{l2Subnet},
						[]string{defaultGatewayIP},
					)
					var err error
					cudnCleanup, err = createClusterUserDefinedNetwork(cs, cudn)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// Step 2: Create UDN namespace, label it and verify NAD is created
					ginkgo.By("2: Creating namespace a1 with UDN labels and verifying NAD creation")
					ns1, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Name: namespaceA1,
							Labels: map[string]string{
								RequiredUDNNamespaceLabel: "",
								cudnLabel:                 cudnLabelValue,
							},
						},
					}, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					f.AddNamespacesToDelete(ns1)

					gomega.Eventually(func() error {
						_, err := nadClient.NetworkAttachmentDefinitions(namespaceA1).Get(
							context.Background(),
							cudnName,
							metav1.GetOptions{},
						)
						return err
					}, 2*time.Minute, 5*time.Second).Should(gomega.Succeed())

					// Step 3: Create pods with static IPs and MACs
					ginkgo.By("3: Creating pods with statically assigned IP and MAC addresses in namespace a1")
					pod1Config.namespace = namespaceA1
					runUDNPod(cs, namespaceA1, pod1Config, nil)

					pod2Config.namespace = namespaceA1
					runUDNPod(cs, namespaceA1, pod2Config, nil)

					// Step 4: Verify ingress traffic from one pod to another works
					ginkgo.By("4: Verifying ingress traffic between pods in namespace a1 works")
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod1Config, pod2Config, pod1Config.staticIP, staticPort)
					}, 2*time.Minute, 6*time.Second).Should(gomega.Succeed())

					// Step 5: Create ingress deny network policy to block traffic
					ginkgo.By("5: Creating default-deny-ingress network policy and verifying traffic is blocked")
					_, err = makeDenyIngressPolicy(cs, namespaceA1, "default-deny-ingress")
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod1Config, pod2Config, pod1Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

					// Step 6: Create allow ingress from same namespace policy
					ginkgo.By("6: Creating allow-same-namespace policy and verifying traffic is allowed")
					allowSameNSPolicy := &knet.NetworkPolicy{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "allow-same-namespace",
							Namespace: namespaceA1,
						},
						Spec: knet.NetworkPolicySpec{
							PodSelector: metav1.LabelSelector{},
							PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
							Ingress: []knet.NetworkPolicyIngressRule{{
								From: []knet.NetworkPolicyPeer{{
									PodSelector: &metav1.LabelSelector{},
								}},
							}},
						},
					}
					_, err = cs.NetworkingV1().NetworkPolicies(namespaceA1).Create(
						context.TODO(),
						allowSameNSPolicy,
						metav1.CreateOptions{},
					)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod1Config, pod2Config, pod1Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).Should(gomega.Succeed())

					// Step 7: Create another namespace with pods and verify ingress from a2 to a1 does not work
					ginkgo.By("7: Creating namespace a2 with pods and verifying cross-namespace traffic is blocked")
					ns2, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Name: namespaceA2,
							Labels: map[string]string{
								RequiredUDNNamespaceLabel: "",
								cudnLabel:                 cudnLabelValue,
							},
						},
					}, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					f.AddNamespacesToDelete(ns2)

					gomega.Eventually(func() error {
						_, err := nadClient.NetworkAttachmentDefinitions(namespaceA2).Get(
							context.Background(),
							cudnName,
							metav1.GetOptions{},
						)
						return err
					}, 2*time.Minute, 5*time.Second).Should(gomega.Succeed())

					pod3Config.namespace = namespaceA2
					runUDNPod(cs, namespaceA2, pod3Config, nil)

					pod4Config.namespace = namespaceA2
					runUDNPod(cs, namespaceA2, pod4Config, nil)

					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod1Config, pod3Config, pod1Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod1Config, pod4Config, pod1Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

					// Step 8: Create allow ingress from IP block
					ginkgo.By("8: Creating ipblock-ingress policy to allow traffic from specific IP")
					ipBlockPolicy := &knet.NetworkPolicy{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "ipblock-ingress",
							Namespace: namespaceA1,
						},
						Spec: knet.NetworkPolicySpec{
							PodSelector: metav1.LabelSelector{},
							Ingress: []knet.NetworkPolicyIngressRule{{
								From: []knet.NetworkPolicyPeer{{
									IPBlock: &knet.IPBlock{
										CIDR: fmt.Sprintf("%s/32", pod4Config.staticIP),
									},
								}},
							}},
						},
					}
					_, err = cs.NetworkingV1().NetworkPolicies(namespaceA1).Create(
						context.TODO(),
						ipBlockPolicy,
						metav1.CreateOptions{},
					)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ginkgo.By("Verifying traffic from allowed IP (pod4) succeeds but blocked IP (pod3) fails")
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod1Config, pod4Config, pod1Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod1Config, pod3Config, pod1Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

					// Step 9: Add allow from all namespaces policy
					ginkgo.By("9: Creating allow-from-all-namespace policy and verifying traffic from all namespaces is allowed")
					allowAllNSPolicy := &knet.NetworkPolicy{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "allow-from-all-namespace",
							Namespace: namespaceA1,
						},
						Spec: knet.NetworkPolicySpec{
							PodSelector: metav1.LabelSelector{},
							Ingress: []knet.NetworkPolicyIngressRule{{
								From: []knet.NetworkPolicyPeer{{
									NamespaceSelector: &metav1.LabelSelector{},
								}},
							}},
						},
					}
					_, err = cs.NetworkingV1().NetworkPolicies(namespaceA1).Create(
						context.TODO(),
						allowAllNSPolicy,
						metav1.CreateOptions{},
					)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod1Config, pod3Config, pod1Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod1Config, pod4Config, pod1Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
				},
				ginkgo.Entry(
					"with default pod configuration",
					*podConfig(
						"hello-pod",
						withLabels(map[string]string{"name": "hello-pod"}),
						withStaticIPMAC("192.168.40.3", "02:03:04:05:06:01"),
						withCommand(func() []string {
							return httpServerContainerCmd(8080)
						}),
					),
					*podConfig(
						"hello-pod-1",
						withLabels(map[string]string{"name": "hello-pod"}),
						withStaticIPMAC("192.168.40.4", "02:03:04:05:06:02"),
					),
					*podConfig(
						"hello-pod",
						withStaticIPMAC("192.168.40.5", "02:03:04:05:06:04"),
					),
					*podConfig(
						"hello-pod-1",
						withStaticIPMAC("192.168.40.6", "02:03:04:05:06:05"),
					),
				),
			)

			// Verify egress network policies with static IP and MAC assignments
			ginkgo.DescribeTable(
				"egress network policies in L2 primary UDN with static IP and MAC",
				func(
					pod1Config podConfiguration,
					pod2Config podConfiguration,
					pod3Config podConfiguration,
					pod4Config podConfiguration,
				) {
					const (
						cudnName        = "l2-network-egress"
						gatewayIP       = "192.168.40.3"
						reservedSubnet1 = "192.168.40.4/32"
						reservedSubnet2 = "192.168.40.5/32"
					)

					namespaceA1Suffix := "a1"
					namespaceA2Suffix := "a2"
					namespaceA3Suffix := "a3"
					namespaceA1 := fmt.Sprintf("%s-%s", f.Namespace.Name, namespaceA1Suffix)
					namespaceA2 := fmt.Sprintf("%s-%s", f.Namespace.Name, namespaceA2Suffix)
					namespaceA3 := fmt.Sprintf("%s-%s", f.Namespace.Name, namespaceA3Suffix)

					// Step 1: Create two UDN namespaces and CUDN for them
					ginkgo.By("1: Creating ClusterUserDefinedNetwork with Layer2 topology and reserved subnets")
					cudn := generateClusterUserDefinedNetworkWithReserved(
						cudnName,
						cudnLabel,
						cudnLabelValue,
						"Layer2",
						"Primary",
						[]string{l2Subnet},
						[]string{gatewayIP},
						[]string{reservedSubnet1, reservedSubnet2},
					)
					var err error
					cudnCleanup, err = createClusterUserDefinedNetwork(cs, cudn)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ginkgo.By("Creating namespaces a1 and a2 with UDN labels")
					ns1, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Name: namespaceA1,
							Labels: map[string]string{
								RequiredUDNNamespaceLabel: "",
								cudnLabel:                 cudnLabelValue,
							},
						},
					}, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					f.AddNamespacesToDelete(ns1)

					ns2, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Name: namespaceA2,
							Labels: map[string]string{
								RequiredUDNNamespaceLabel: "",
								cudnLabel:                 cudnLabelValue,
							},
						},
					}, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					f.AddNamespacesToDelete(ns2)

					gomega.Eventually(func() error {
						_, err := nadClient.NetworkAttachmentDefinitions(namespaceA1).Get(
							context.Background(),
							cudnName,
							metav1.GetOptions{},
						)
						return err
					}, 2*time.Minute, 5*time.Second).Should(gomega.Succeed())
					gomega.Eventually(func() error {
						_, err := nadClient.NetworkAttachmentDefinitions(namespaceA2).Get(
							context.Background(),
							cudnName,
							metav1.GetOptions{},
						)
						return err
					}, 2*time.Minute, 5*time.Second).Should(gomega.Succeed())

					// Step 2: Create pods in both namespaces with static IPs
					ginkgo.By("2: Creating pods with static IP and MAC in namespaces a1 and a2")
					pod1Config.namespace = namespaceA1
					runUDNPod(cs, namespaceA1, pod1Config, nil)

					pod2Config.namespace = namespaceA2
					runUDNPod(cs, namespaceA2, pod2Config, nil)

					pod3Config.namespace = namespaceA2
					runUDNPod(cs, namespaceA2, pod3Config, nil)

					// Step 3: Create egress default deny policy in a1 and verify pods in a2 are inaccessible
					ginkgo.By("3: Creating default-deny-egress policy and verifying pods in a2 are inaccessible")
					_, err = makeDenyEgressPolicy(cs, namespaceA1, "default-deny-egress")
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod2Config, pod1Config, pod2Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod3Config, pod1Config, pod3Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

					// Step 4: Create another namespace a3 with label and pod
					ginkgo.By("4: Creating namespace a3 with team=operations label and pod")
					ns3, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Name: namespaceA3,
							Labels: map[string]string{
								RequiredUDNNamespaceLabel: "",
								cudnLabel:                 cudnLabelValue,
								"team":                    "operations",
							},
						},
					}, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					f.AddNamespacesToDelete(ns3)

					gomega.Eventually(func() error {
						_, err := nadClient.NetworkAttachmentDefinitions(namespaceA3).Get(
							context.Background(),
							cudnName,
							metav1.GetOptions{},
						)
						return err
					}, 2*time.Minute, 5*time.Second).Should(gomega.Succeed())

					pod4Config.namespace = namespaceA3
					runUDNPod(cs, namespaceA3, pod4Config, nil)

					// Step 5: Label pods and namespaces
					ginkgo.By("5: Labeling pods with color=red and namespace a2 with team=operations")
					pod2Obj, err := cs.CoreV1().Pods(namespaceA2).Get(context.Background(), pod2Config.name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					pod2Obj.Labels["color"] = "red"
					_, err = cs.CoreV1().Pods(namespaceA2).Update(context.Background(), pod2Obj, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					pod4Obj, err := cs.CoreV1().Pods(namespaceA3).Get(context.Background(), pod4Config.name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					pod4Obj.Labels["color"] = "red"
					_, err = cs.CoreV1().Pods(namespaceA3).Update(context.Background(), pod4Obj, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ns2Updated, err := cs.CoreV1().Namespaces().Get(context.Background(), namespaceA2, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					ns2Updated.Labels["team"] = "operations"
					_, err = cs.CoreV1().Namespaces().Update(context.Background(), ns2Updated, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// Step 6: Create network policy with namespace and pod selector
					ginkgo.By("6: Creating egress policy with namespace and pod selectors and verifying selective access")
					allowPodAndPodPolicy := &knet.NetworkPolicy{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "allow-pod-and-pod",
							Namespace: namespaceA1,
						},
						Spec: knet.NetworkPolicySpec{
							PodSelector: metav1.LabelSelector{},
							Egress: []knet.NetworkPolicyEgressRule{{
								To: []knet.NetworkPolicyPeer{{
									NamespaceSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"team": "operations"},
									},
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"color": "red"},
									},
								}},
							}},
						},
					}
					_, err = cs.NetworkingV1().NetworkPolicies(namespaceA1).Create(
						context.TODO(),
						allowPodAndPodPolicy,
						metav1.CreateOptions{},
					)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ginkgo.By("Verifying labeled pods in a2 and a3 are accessible")
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod2Config, pod1Config, pod2Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod4Config, pod1Config, pod4Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).Should(gomega.Succeed())

					ginkgo.By("Verifying unlabeled pod in a2 is not accessible")
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, pod3Config, pod1Config, pod3Config.staticIP, staticPort)
					}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())
				},
				ginkgo.Entry(
					"with default pod configuration",
					*podConfig(
						"hello-pod",
						withLabels(map[string]string{"name": "hello-pod"}),
						withStaticIPMAC("192.168.40.6", "02:03:04:05:06:01"),
					),
					*podConfig(
						"hello-pod",
						withLabels(map[string]string{"name": "hello-pod"}),
						withStaticIPMAC("192.168.40.7", "02:03:04:05:06:02"),
						withCommand(func() []string {
							return httpServerContainerCmd(8080)
						}),
					),
					*podConfig(
						"hello-pod-1",
						withLabels(map[string]string{"name": "hello-pod"}),
						withStaticIPMAC("192.168.40.8", "02:03:04:05:06:03"),
						withCommand(func() []string {
							return httpServerContainerCmd(8080)
						}),
					),
					*podConfig(
						"hello-pod",
						withLabels(map[string]string{"name": "hello-pod"}),
						withStaticIPMAC("192.168.40.9", "02:03:04:05:06:04"),
						withCommand(func() []string {
							return httpServerContainerCmd(8080)
						}),
					),
				),
			)
		})
	})
})

func getNamespaceName(f *framework.Framework, nsSuffix string) string {
	return fmt.Sprintf("%s-%s", f.Namespace.Name, nsSuffix)
}

func allowTrafficToPodFromNamespacePolicy(f *framework.Framework, namespace, fromNamespace1, fromNamespace2, policyName string, podLabel map[string]string) (*knet.NetworkPolicy, error) {
	policy := &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
		},
		Spec: knet.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podLabel},
			PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
			Ingress: []knet.NetworkPolicyIngressRule{{From: []knet.NetworkPolicyPeer{
				{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": fromNamespace1}}},
				{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": fromNamespace2}}}}}},
		},
	}
	return f.ClientSet.NetworkingV1().NetworkPolicies(namespace).Create(context.TODO(), policy, metav1.CreateOptions{})
}

// Helper function to create a default deny ingress policy
func makeDenyIngressPolicy(cs clientset.Interface, namespace, policyName string) (*knet.NetworkPolicy, error) {
	policy := &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      policyName,
			Namespace: namespace,
		},
		Spec: knet.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
		},
	}
	return cs.NetworkingV1().NetworkPolicies(namespace).Create(context.TODO(), policy, metav1.CreateOptions{})
}

// Helper function to create a default deny egress policy
func makeDenyEgressPolicy(cs clientset.Interface, namespace, policyName string) (*knet.NetworkPolicy, error) {
	policy := &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      policyName,
			Namespace: namespace,
		},
		Spec: knet.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []knet.PolicyType{knet.PolicyTypeEgress},
		},
	}
	return cs.NetworkingV1().NetworkPolicies(namespace).Create(context.TODO(), policy, metav1.CreateOptions{})
}

// Helper function to set static IP and MAC via annotations.
// The ip parameter should be the IP address without CIDR suffix (e.g., "192.168.40.3").
// The CIDR suffix "/24" is added automatically.
func withStaticIPMAC(ip, mac string) podOption {
	ipWithCIDR := ip + "/24"
	annotation := fmt.Sprintf(`[{"name":"default","namespace":"ovn-kubernetes","ips":["%s"],"mac":"%s"}]`, ipWithCIDR, mac)
	return func(pod *podConfiguration) {
		if pod.annotations == nil {
			pod.annotations = make(map[string]string)
		}
		pod.annotations["v1.multus-cni.io/default-network"] = annotation
		pod.staticIP = ip
	}
}

// Helper to generate CUDN manifest params without reserved subnets
func generateClusterUserDefinedNetwork(name, labelKey, labelValue, topology, role string, subnets, gatewayIPs []string) *networkAttachmentConfigParams {
	// For label-based CUDN, we store the label info in namespace field temporarily
	// The actual CUDN creation will use label selectors
	return &networkAttachmentConfigParams{
		name:              name,
		topology:          strings.ToLower(topology),
		cidr:              joinStrings(subnets...),
		role:              strings.ToLower(role),
		defaultGatewayIPs: joinStrings(gatewayIPs...),
		labelKey:          labelKey,
		labelValue:        labelValue,
	}
}

// Helper to generate CUDN manifest params with reserved subnets
func generateClusterUserDefinedNetworkWithReserved(name, labelKey, labelValue, topology, role string, subnets, gatewayIPs, reservedSubnets []string) *networkAttachmentConfigParams {
	return &networkAttachmentConfigParams{
		name:              name,
		topology:          strings.ToLower(topology),
		cidr:              joinStrings(subnets...),
		role:              strings.ToLower(role),
		defaultGatewayIPs: joinStrings(gatewayIPs...),
		reservedCIDRs:     joinStrings(reservedSubnets...),
		labelKey:          labelKey,
		labelValue:        labelValue,
	}
}

// Helper to create CUDN from params - generates manifest with label-based selector and creates it
func createClusterUserDefinedNetwork(cs clientset.Interface, params *networkAttachmentConfigParams) (func(), error) {
	// Generate CUDN manifest with label-based namespace selector
	if params.labelKey == "" || params.labelValue == "" {
		return nil, fmt.Errorf("label key and value cannot be empty")
	}
	labelKey := params.labelKey
	labelValue := params.labelValue

	if len(params.topology) == 0 {
		return nil, fmt.Errorf("topology type cannot be empty")
	}
	topologyType := "layer2"
	if strings.ToLower(params.topology) == "layer3" {
		topologyType = "layer3"
	}
	roleType := "Primary"
	if strings.ToLower(params.role) == "secondary" {
		roleType = "Secondary"
	}

	manifest := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: %s
metadata:
  name: %s
spec:
  namespaceSelector:
    matchLabels:
      %s: %s
  network:
    topology: %s
    %s:
      role: %s
      subnets: [%s]`, cudnResourceType, params.name, labelKey, labelValue,
		strings.ToUpper(topologyType[:1])+topologyType[1:], topologyType, roleType, params.cidr)

	// Add optional fields
	if len(params.defaultGatewayIPs) > 0 {
		manifest += fmt.Sprintf("\n      defaultGatewayIPs: [%s]", params.defaultGatewayIPs)
	}
	if len(params.reservedCIDRs) > 0 {
		manifest += fmt.Sprintf("\n      reservedSubnets: [%s]", params.reservedCIDRs)
	}
	if len(params.infrastructureCIDRs) > 0 {
		manifest += fmt.Sprintf("\n      infrastructureSubnets: [%s]", params.infrastructureCIDRs)
	}

	// Create the CUDN using createManifest helper
	fileCleanup, err := createManifest("", manifest)
	if err != nil {
		return fileCleanup, err
	}

	// Return a cleanup function that deletes the CUDN and the temp file
	cleanup := func() {
		// Delete the CUDN resource with timeout to prevent hanging
		_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", params.name, "--ignore-not-found=true", "--timeout=60s", "--wait=true")
		if err != nil {
			// Clean up the temp file
			framework.Logf("Warning: failed to delete CUDN %s: %v", params.name, err)
		}
		fileCleanup()
	}
	return cleanup, nil
}
