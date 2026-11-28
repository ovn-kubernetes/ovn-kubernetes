// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"time"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	utilnet "k8s.io/utils/net"
)

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

				serverIPNets, err := getPodAnnotationIPsForAttachment(cs, f.Namespace.Name, serverPodConfig.name, namespacedName(f.Namespace.Name, netConfig.name))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				var serverIPs []string
				for _, serverIP := range serverIPNets {
					ginkgo.By("asserting the server pod has an IP from the configured range")
					gomega.Expect(inAnyConfiguredSubnet(netConfig.cidr, serverIP.IP.String())).To(gomega.Succeed())
					serverIPs = append(serverIPs, serverIP.IP.String())
				}
				gomega.Expect(serverIPs).NotTo(gomega.BeEmpty())

				for _, serverIP := range serverIPs {
					ginkgo.By("asserting the *client* pod can contact the server pod exposed endpoint")
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(gomega.Succeed())
				}

				ginkgo.By("creating a \"default deny\" network policy")
				_, err = makeDenyAllPolicy(f, f.Namespace.Name, "deny-all")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("asserting the *client* pod can not contact the server pod exposed endpoint")
				for _, serverIP := range serverIPs {
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())
				}

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
					cidr:     primaryLayer3MultiCIDRs(),
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
			"ICMP should bypass default deny policy for UDNs when enabled",
			func(
				netConfigParams networkAttachmentConfigParams,
				clientPodConfig podConfiguration,
				serverPodConfig podConfiguration,
			) {
				if !isICMPNetworkPolicyBypassEnabled() {
					ginkgo.Skip("ICMP Network Policy bypass is not enabled, skipping ICMP bypass network policy tests")
				}

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

				ginkgo.By("creating a \"default deny\" network policy")
				_, err = makeDenyAllPolicy(f, f.Namespace.Name, "deny-all")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				serverIPNets, err := getPodAnnotationIPsForAttachment(cs, f.Namespace.Name, serverPodConfig.name, namespacedName(f.Namespace.Name, netConfig.name))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				var serverIPs []string
				for _, serverIP := range serverIPNets {
					gomega.Expect(inAnyConfiguredSubnet(netConfig.cidr, serverIP.IP.String())).To(gomega.Succeed())
					serverIPs = append(serverIPs, serverIP.IP.String())
				}
				gomega.Expect(serverIPs).NotTo(gomega.BeEmpty())

				ginkgo.By("asserting the *client* pod can ping the server pod despite the default deny policy")
				for _, serverIP := range serverIPs {
					gomega.Eventually(func() error {
						return pingServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP)
					}, 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
				}

				ginkgo.By("asserting the *client* pod can not reach the server pod HTTP endpoint due to default deny policy")
				for _, serverIP := range serverIPs {
					gomega.Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())
					gomega.Consistently(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 15*time.Second, 5*time.Second).ShouldNot(gomega.Succeed())
				}
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
					withCommand(func() []string {
						return []string{"/agnhost", "pause"}
					}),
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
					cidr:     primaryLayer3MultiCIDRs(),
					role:     "primary",
				},
				*podConfig(
					"client-pod",
					withCommand(func() []string {
						return []string{"/agnhost", "pause"}
					}),
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
				if topology == "layer3" {
					nad.cidr = primaryLayer3MultiCIDRs()
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
				allowServerPodIPs, err := getPodAnnotationIPsForAttachment(cs, namespaceYellow, allowServerPodConfig.name, namespacedName(namespaceYellow, netConfName))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				denyServerPodIPs, err := getPodAnnotationIPsForAttachment(cs, namespaceYellow, denyServerPodConfig.name, namespacedName(namespaceYellow, netConfName))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(allowServerPodIPs).NotTo(gomega.BeEmpty())
				gomega.Expect(denyServerPodIPs).NotTo(gomega.BeEmpty())
				allowServerPodIP := allowServerPodIPs[0].IP.String()
				denyServerPodIP := denyServerPodIPs[0].IP.String()
				gomega.Expect(inAnyConfiguredSubnet(nad.cidr, allowServerPodIP)).To(gomega.Succeed())
				gomega.Expect(inAnyConfiguredSubnet(nad.cidr, denyServerPodIP)).To(gomega.Succeed())

				ginkgo.By("asserting the *client* pod can contact the allow server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, allowServerPodConfig, clientPodConfig, allowServerPodIP, port)
				}, 2*time.Minute, 6*time.Second).Should(gomega.Succeed())

				ginkgo.By("asserting the *client* pod can contact the deny server pod exposed endpoint")
				gomega.Eventually(func() error {
					return reachServerPodFromClient(cs, denyServerPodConfig, clientPodConfig, denyServerPodIP, port)
				}, 2*time.Minute, 6*time.Second).Should(gomega.Succeed())

				ginkgo.By("creating a \"default deny\" network policy")
				_, err = makeDenyAllPolicy(f, namespaceYellow, "deny-all")
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

		ginkgo.It("should honor allow-from-same-namespace and allow-from-all-namespaces ingress policies on a shared layer3 primary network", ginkgo.Serial, func() {
			namespaceYellow := getNamespaceName(f, nameSpaceYellowSuffix)
			namespaceBlue := getNamespaceName(f, namespaceBlueSuffix)
			namespaceRed := getNamespaceName(f, namespaceRedSuffix)

			nad := networkAttachmentConfigParams{
				topology:    "layer3",
				cidr:        primaryLayer3MultiCIDRs(),
				networkName: fmt.Sprintf("shared-l3-%s", rand.String(randomStringLength)),
				role:        "primary",
			}
			filterSupportedNetworkConfig(f.ClientSet, &nad)
			netConfName := fmt.Sprintf("sharednet-%s", rand.String(randomStringLength))
			for _, namespace := range []string{namespaceYellow, namespaceBlue, namespaceRed} {
				ginkgo.By("creating the shared attachment configuration in namespace " + namespace)
				netConfig := newNetworkAttachmentConfig(nad)
				netConfig.namespace = namespace
				netConfig.name = netConfName
				_, err := nadClient.NetworkAttachmentDefinitions(namespace).Create(
					context.Background(), generateNAD(netConfig, f.ClientSet), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}

			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 2 {
				ginkgo.Skip("requires at least 2 Nodes")
			}

			serverPodConfig := *podConfig("server-pod",
				withCommand(func() []string { return httpServerContainerCmd(port) }),
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}))
			serverPodConfig.namespace = namespaceYellow
			sameNSClientConfig := *podConfig("same-ns-client",
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}))
			sameNSClientConfig.namespace = namespaceYellow
			blueClientConfig := *podConfig("blue-client",
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}))
			blueClientConfig.namespace = namespaceBlue
			redClientConfig := *podConfig("red-client",
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}))
			redClientConfig.namespace = namespaceRed

			runUDNPod(cs, namespaceYellow, serverPodConfig, nil)
			runUDNPod(cs, namespaceYellow, sameNSClientConfig, nil)
			runUDNPod(cs, namespaceBlue, blueClientConfig, nil)
			runUDNPod(cs, namespaceRed, redClientConfig, nil)

			serverIPNets, err := getPodAnnotationIPsForAttachment(cs, namespaceYellow, serverPodConfig.name, namespacedName(namespaceYellow, netConfName))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			var serverIPs []string
			for _, ipNet := range serverIPNets {
				serverIPs = append(serverIPs, ipNet.IP.String())
			}
			gomega.Expect(serverIPs).NotTo(gomega.BeEmpty())

			reaches := func(client podConfiguration) func() error {
				return func() error {
					for _, serverIP := range serverIPs {
						if err := reachServerPodFromClient(cs, serverPodConfig, client, serverIP, port); err != nil {
							return err
						}
					}
					return nil
				}
			}

			ginkgo.By("asserting every client can reach the server before any policy")
			for _, client := range []podConfiguration{sameNSClientConfig, blueClientConfig, redClientConfig} {
				gomega.Eventually(reaches(client), 2*time.Minute, 6*time.Second).Should(gomega.Succeed())
			}

			ginkgo.By("creating a default-deny ingress policy")
			_, err = makeDenyAllIngressPolicy(f, namespaceYellow, "deny-ingress")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, client := range []podConfiguration{sameNSClientConfig, blueClientConfig} {
				gomega.Eventually(reaches(client), 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())
			}

			ginkgo.By("allowing ingress from the same namespace only")
			_, err = allowIngressFromPeersPolicy(f, namespaceYellow, "allow-from-same-namespace", sameNamespacePeer())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(reaches(sameNSClientConfig), 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
			gomega.Eventually(reaches(blueClientConfig), 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

			ginkgo.By("allowing ingress from all namespaces")
			_, err = allowIngressFromPeersPolicy(f, namespaceYellow, "allow-from-all-namespaces", allNamespacesPeer())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, client := range []podConfiguration{blueClientConfig, redClientConfig} {
				gomega.Eventually(reaches(client), 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
			}

			ginkgo.By("narrowing the all-namespaces policy to a labelled namespace")
			gomega.Expect(f.ClientSet.NetworkingV1().NetworkPolicies(namespaceYellow).Delete(context.TODO(), "allow-from-all-namespaces", metav1.DeleteOptions{})).To(gomega.Succeed())
			_, err = allowIngressFromPeersPolicy(f, namespaceYellow, "allow-from-labelled-namespace",
				namespaceWithLabelPeer(map[string]string{"kubernetes.io/metadata.name": namespaceBlue}))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(reaches(blueClientConfig), 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
			gomega.Eventually(reaches(redClientConfig), 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())
		})

		ginkgo.It("should honor allow-to-same-namespace and allow-to-all-namespaces egress policies on a shared layer2 primary network", ginkgo.Serial, func() {
			namespaceYellow := getNamespaceName(f, nameSpaceYellowSuffix)
			namespaceBlue := getNamespaceName(f, namespaceBlueSuffix)
			namespaceRed := getNamespaceName(f, namespaceRedSuffix)

			nad := networkAttachmentConfigParams{
				topology:    "layer2",
				cidr:        joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
				networkName: fmt.Sprintf("shared-l2-%s", rand.String(randomStringLength)),
				role:        "primary",
			}
			filterSupportedNetworkConfig(f.ClientSet, &nad)
			netConfName := fmt.Sprintf("sharednet-%s", rand.String(randomStringLength))
			for _, namespace := range []string{namespaceYellow, namespaceBlue, namespaceRed} {
				ginkgo.By("creating the shared attachment configuration in namespace " + namespace)
				netConfig := newNetworkAttachmentConfig(nad)
				netConfig.namespace = namespace
				netConfig.name = netConfName
				_, err := nadClient.NetworkAttachmentDefinitions(namespace).Create(
					context.Background(), generateNAD(netConfig, f.ClientSet), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}

			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 2 {
				ginkgo.Skip("requires at least 2 Nodes")
			}

			// The source pod (in yellow) initiates egress; the destinations are HTTP
			// servers in the same and other namespaces on the shared network.
			sourcePodConfig := *podConfig("source-pod",
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}))
			sourcePodConfig.namespace = namespaceYellow
			sameNSServerConfig := *podConfig("same-ns-server",
				withCommand(func() []string { return httpServerContainerCmd(port) }),
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}))
			sameNSServerConfig.namespace = namespaceYellow
			blueServerConfig := *podConfig("blue-server",
				withCommand(func() []string { return httpServerContainerCmd(port) }),
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}))
			blueServerConfig.namespace = namespaceBlue
			redServerConfig := *podConfig("red-server",
				withCommand(func() []string { return httpServerContainerCmd(port) }),
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}))
			redServerConfig.namespace = namespaceRed

			runUDNPod(cs, namespaceYellow, sourcePodConfig, nil)
			runUDNPod(cs, namespaceYellow, sameNSServerConfig, nil)
			runUDNPod(cs, namespaceBlue, blueServerConfig, nil)
			runUDNPod(cs, namespaceRed, redServerConfig, nil)

			serverIPs := func(serverConfig podConfiguration) []string {
				ipNets, err := getPodAnnotationIPsForAttachment(cs, serverConfig.namespace, serverConfig.name, namespacedName(serverConfig.namespace, netConfName))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				var ips []string
				for _, ipNet := range ipNets {
					ips = append(ips, ipNet.IP.String())
				}
				gomega.Expect(ips).NotTo(gomega.BeEmpty())
				return ips
			}
			reaches := func(serverConfig podConfiguration) func() error {
				ips := serverIPs(serverConfig)
				return func() error {
					for _, ip := range ips {
						if err := reachServerPodFromClient(cs, serverConfig, sourcePodConfig, ip, port); err != nil {
							return err
						}
					}
					return nil
				}
			}

			ginkgo.By("asserting the source pod can reach every server before any policy")
			for _, server := range []podConfiguration{sameNSServerConfig, blueServerConfig, redServerConfig} {
				gomega.Eventually(reaches(server), 2*time.Minute, 6*time.Second).Should(gomega.Succeed())
			}

			ginkgo.By("creating a default-deny egress policy")
			_, err = makeDenyAllEgressPolicy(f, namespaceYellow, "deny-egress")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, server := range []podConfiguration{sameNSServerConfig, blueServerConfig} {
				gomega.Eventually(reaches(server), 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())
			}

			ginkgo.By("allowing egress to the same namespace only")
			_, err = allowEgressToPeersPolicy(f, namespaceYellow, "allow-to-same-namespace", sameNamespacePeer())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(reaches(sameNSServerConfig), 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
			gomega.Eventually(reaches(blueServerConfig), 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

			ginkgo.By("allowing egress to all namespaces")
			_, err = allowEgressToPeersPolicy(f, namespaceYellow, "allow-to-all-namespaces", allNamespacesPeer())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, server := range []podConfiguration{blueServerConfig, redServerConfig} {
				gomega.Eventually(reaches(server), 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
			}

			ginkgo.By("narrowing the all-namespaces egress policy to a labelled namespace")
			gomega.Expect(f.ClientSet.NetworkingV1().NetworkPolicies(namespaceYellow).Delete(context.TODO(), "allow-to-all-namespaces", metav1.DeleteOptions{})).To(gomega.Succeed())
			_, err = allowEgressToPeersPolicy(f, namespaceYellow, "allow-to-labelled-namespace",
				namespaceWithLabelPeer(map[string]string{"kubernetes.io/metadata.name": namespaceBlue}))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(reaches(blueServerConfig), 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
			gomega.Eventually(reaches(redServerConfig), 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())
		})

		ginkgo.It("should honor ingress ipBlock policies with and without an except clause on a layer3 CUDN", ginkgo.Serial, func() {
			ctx := context.Background()
			namespaceYellow := getNamespaceName(f, nameSpaceYellowSuffix)
			namespaceBlue := getNamespaceName(f, namespaceBlueSuffix)

			nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, cs, 2)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 2 {
				ginkgo.Skip("requires at least 2 Nodes")
			}

			ginkgo.By("creating a layer3 primary CUDN spanning the yellow and blue namespaces")
			cudnName := fmt.Sprintf("cudn-ipblock-ing-%s", rand.String(randomStringLength))
			cudnParams := networkAttachmentConfigParams{
				name:      cudnName,
				namespace: namespaceYellow + "," + namespaceBlue,
				topology:  "layer3",
				cidr:      primaryLayer3MultiCIDRs(),
				role:      "primary",
			}
			cleanupCUDN, err := createManifest("", generateClusterUserDefinedNetworkManifest(&cudnParams, cs))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() {
				for _, ns := range []string{namespaceYellow, namespaceBlue} {
					gomega.Expect(cs.CoreV1().Pods(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
				}
				_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", "--timeout=120s")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				cleanupCUDN()
			})
			gomega.Eventually(clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(gomega.Succeed())

			serverPodConfig := *podConfig("server-pod",
				withCommand(func() []string { return httpServerContainerCmd(port) }),
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}))
			serverPodConfig.namespace = namespaceYellow
			// clientA and clientB share a node so they fall in the same per-node
			// host subnet, which the except clause below relies on.
			clientAConfig := *podConfig("client-a",
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}))
			clientAConfig.namespace = namespaceBlue
			clientBConfig := *podConfig("client-b",
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}))
			clientBConfig.namespace = namespaceBlue

			runUDNPod(cs, namespaceYellow, serverPodConfig, nil)
			runUDNPod(cs, namespaceBlue, clientAConfig, nil)
			runUDNPod(cs, namespaceBlue, clientBConfig, nil)

			podIPs := func(c podConfiguration) []string {
				ipNets, err := getPodAnnotationIPsForAttachment(cs, c.namespace, c.name, namespacedName(c.namespace, cudnName))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				var ips []string
				for _, ipNet := range ipNets {
					ips = append(ips, ipNet.IP.String())
				}
				gomega.Expect(ips).NotTo(gomega.BeEmpty())
				return ips
			}
			serverIPs := podIPs(serverPodConfig)
			clientAIPs := podIPs(clientAConfig)

			reaches := func(client podConfiguration) func() error {
				return func() error {
					for _, serverIP := range serverIPs {
						if err := reachServerPodFromClient(cs, serverPodConfig, client, serverIP, port); err != nil {
							return err
						}
					}
					return nil
				}
			}

			ginkgo.By("creating a default-deny ingress policy on the server namespace")
			_, err = makeDenyAllIngressPolicy(f, namespaceYellow, "deny-ingress")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(reaches(clientAConfig), 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

			ginkgo.By("allowing ingress from clientA's exact IP via ipBlock")
			var exactCIDRs []string
			for _, ip := range clientAIPs {
				exactCIDRs = append(exactCIDRs, exactCIDRForIP(ip))
			}
			_, err = allowIngressFromPeersPolicy(f, namespaceYellow, "ipblock-ingress", ipBlockPeers(exactCIDRs, nil))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(reaches(clientAConfig), 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
			gomega.Eventually(reaches(clientBConfig), 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

			ginkgo.By("replacing the ipBlock with the host subnet and an except clause carving out clientA")
			gomega.Expect(f.ClientSet.NetworkingV1().NetworkPolicies(namespaceYellow).Delete(ctx, "ipblock-ingress", metav1.DeleteOptions{})).To(gomega.Succeed())
			var subnetCIDRs []string
			for _, ip := range clientAIPs {
				subnetCIDRs = append(subnetCIDRs, hostSubnetCIDRForIP(ip))
			}
			_, err = allowIngressFromPeersPolicy(f, namespaceYellow, "ipblock-ingress-except", ipBlockPeers(subnetCIDRs, exactCIDRs))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(reaches(clientBConfig), 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
			gomega.Eventually(reaches(clientAConfig), 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())
		})

		ginkgo.It("should honor egress ipBlock policies with and without an except clause on a layer3 CUDN", ginkgo.Serial, func() {
			ctx := context.Background()
			namespaceYellow := getNamespaceName(f, nameSpaceYellowSuffix)
			namespaceBlue := getNamespaceName(f, namespaceBlueSuffix)

			nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, cs, 2)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 2 {
				ginkgo.Skip("requires at least 2 Nodes")
			}

			ginkgo.By("creating a layer3 primary CUDN spanning the yellow and blue namespaces")
			cudnName := fmt.Sprintf("cudn-ipblock-eg-%s", rand.String(randomStringLength))
			cudnParams := networkAttachmentConfigParams{
				name:      cudnName,
				namespace: namespaceYellow + "," + namespaceBlue,
				topology:  "layer3",
				cidr:      primaryLayer3MultiCIDRs(),
				role:      "primary",
			}
			cleanupCUDN, err := createManifest("", generateClusterUserDefinedNetworkManifest(&cudnParams, cs))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() {
				for _, ns := range []string{namespaceYellow, namespaceBlue} {
					gomega.Expect(cs.CoreV1().Pods(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
				}
				_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", "--timeout=120s")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				cleanupCUDN()
			})
			gomega.Eventually(clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(gomega.Succeed())

			// The source pod (in yellow) initiates egress; destA and destB are HTTP
			// servers sharing a node (hence the same host subnet) in blue.
			sourcePodConfig := *podConfig("source-pod",
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}))
			sourcePodConfig.namespace = namespaceYellow
			destAConfig := *podConfig("dest-a",
				withCommand(func() []string { return httpServerContainerCmd(port) }),
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}))
			destAConfig.namespace = namespaceBlue
			destBConfig := *podConfig("dest-b",
				withCommand(func() []string { return httpServerContainerCmd(port) }),
				withNodeSelector(map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}))
			destBConfig.namespace = namespaceBlue

			runUDNPod(cs, namespaceYellow, sourcePodConfig, nil)
			runUDNPod(cs, namespaceBlue, destAConfig, nil)
			runUDNPod(cs, namespaceBlue, destBConfig, nil)

			podIPs := func(c podConfiguration) []string {
				ipNets, err := getPodAnnotationIPsForAttachment(cs, c.namespace, c.name, namespacedName(c.namespace, cudnName))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				var ips []string
				for _, ipNet := range ipNets {
					ips = append(ips, ipNet.IP.String())
				}
				gomega.Expect(ips).NotTo(gomega.BeEmpty())
				return ips
			}
			destAIPs := podIPs(destAConfig)
			destBIPs := podIPs(destBConfig)

			reaches := func(destConfig podConfiguration, destIPs []string) func() error {
				return func() error {
					for _, ip := range destIPs {
						if err := reachServerPodFromClient(cs, destConfig, sourcePodConfig, ip, port); err != nil {
							return err
						}
					}
					return nil
				}
			}

			ginkgo.By("creating a default-deny egress policy on the source namespace")
			_, err = makeDenyAllEgressPolicy(f, namespaceYellow, "deny-egress")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(reaches(destAConfig, destAIPs), 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

			ginkgo.By("allowing egress to destA's exact IP via ipBlock")
			var exactCIDRs []string
			for _, ip := range destAIPs {
				exactCIDRs = append(exactCIDRs, exactCIDRForIP(ip))
			}
			_, err = allowEgressToPeersPolicy(f, namespaceYellow, "ipblock-egress", ipBlockPeers(exactCIDRs, nil))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(reaches(destAConfig, destAIPs), 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
			gomega.Eventually(reaches(destBConfig, destBIPs), 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())

			ginkgo.By("replacing the ipBlock with the host subnet and an except clause carving out destA")
			gomega.Expect(f.ClientSet.NetworkingV1().NetworkPolicies(namespaceYellow).Delete(ctx, "ipblock-egress", metav1.DeleteOptions{})).To(gomega.Succeed())
			var subnetCIDRs []string
			for _, ip := range destAIPs {
				subnetCIDRs = append(subnetCIDRs, hostSubnetCIDRForIP(ip))
			}
			_, err = allowEgressToPeersPolicy(f, namespaceYellow, "ipblock-egress-except", ipBlockPeers(subnetCIDRs, exactCIDRs))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(reaches(destBConfig, destBIPs), 1*time.Minute, 6*time.Second).Should(gomega.Succeed())
			gomega.Eventually(reaches(destAConfig, destAIPs), 1*time.Minute, 6*time.Second).ShouldNot(gomega.Succeed())
		})
	})
})

func getNamespaceName(f *framework.Framework, nsSuffix string) string {
	return fmt.Sprintf("%s-%s", f.Namespace.Name, nsSuffix)
}

func makeDenyAllIngressPolicy(f *framework.Framework, ns, name string) (*knet.NetworkPolicy, error) {
	policy := &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: knet.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
			Ingress:     []knet.NetworkPolicyIngressRule{},
		},
	}
	return f.ClientSet.NetworkingV1().NetworkPolicies(ns).Create(context.TODO(), policy, metav1.CreateOptions{})
}

func makeDenyAllEgressPolicy(f *framework.Framework, ns, name string) (*knet.NetworkPolicy, error) {
	policy := &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: knet.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []knet.PolicyType{knet.PolicyTypeEgress},
			Egress:      []knet.NetworkPolicyEgressRule{},
		},
	}
	return f.ClientSet.NetworkingV1().NetworkPolicies(ns).Create(context.TODO(), policy, metav1.CreateOptions{})
}

func allowIngressFromPeersPolicy(f *framework.Framework, ns, name string, peers []knet.NetworkPolicyPeer) (*knet.NetworkPolicy, error) {
	policy := &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: knet.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
			Ingress:     []knet.NetworkPolicyIngressRule{{From: peers}},
		},
	}
	return f.ClientSet.NetworkingV1().NetworkPolicies(ns).Create(context.TODO(), policy, metav1.CreateOptions{})
}

func allowEgressToPeersPolicy(f *framework.Framework, ns, name string, peers []knet.NetworkPolicyPeer) (*knet.NetworkPolicy, error) {
	policy := &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: knet.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []knet.PolicyType{knet.PolicyTypeEgress},
			Egress:      []knet.NetworkPolicyEgressRule{{To: peers}},
		},
	}
	return f.ClientSet.NetworkingV1().NetworkPolicies(ns).Create(context.TODO(), policy, metav1.CreateOptions{})
}

// sameNamespacePeer selects all pods in the policy's own namespace.
func sameNamespacePeer() []knet.NetworkPolicyPeer {
	return []knet.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
}

// allNamespacesPeer selects all pods in all namespaces.
func allNamespacesPeer() []knet.NetworkPolicyPeer {
	return []knet.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}}
}

// namespaceWithLabelPeer selects pods in namespaces matching the given labels.
func namespaceWithLabelPeer(nsLabels map[string]string) []knet.NetworkPolicyPeer {
	return []knet.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: nsLabels}}}
}

// ipBlockPeers builds one ipBlock peer per CIDR, all sharing the same except list
// entries that fall within each CIDR's family.
func ipBlockPeers(cidrs []string, except []string) []knet.NetworkPolicyPeer {
	peers := make([]knet.NetworkPolicyPeer, 0, len(cidrs))
	for _, cidr := range cidrs {
		block := &knet.IPBlock{CIDR: cidr}
		for _, ex := range except {
			if cidrsSameFamily(cidr, ex) {
				block.Except = append(block.Except, ex)
			}
		}
		peers = append(peers, knet.NetworkPolicyPeer{IPBlock: block})
	}
	return peers
}

func cidrsSameFamily(a, b string) bool {
	return utilnet.IsIPv6CIDRString(a) == utilnet.IsIPv6CIDRString(b)
}

// exactCIDRForIP returns a host CIDR (/32 or /128) for the given IP.
func exactCIDRForIP(ip string) string {
	if utilnet.IsIPv6String(ip) {
		return ip + "/128"
	}
	return ip + "/32"
}

// hostSubnetCIDRForIP masks the IP to the per-node UDN host subnet prefix
// (/24 for IPv4, /64 for IPv6), matching the Layer3 UDN hostSubnet size.
func hostSubnetCIDRForIP(ip string) string {
	bits := 24
	if utilnet.IsIPv6String(ip) {
		bits = 64
	}
	_, ipnet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", ip, bits))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return ipnet.String()
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
