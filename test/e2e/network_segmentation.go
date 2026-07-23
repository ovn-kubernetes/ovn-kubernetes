// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	ovnkubeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/pointer"
)

const openDefaultPortsAnnotation = "k8s.ovn.org/open-default-ports"
const RequiredUDNNamespaceLabel = "k8s.ovn.org/primary-user-defined-network"
const OvnPodAnnotationName = "k8s.ovn.org/pod-networks"
const expectedUDNCNIVersion = "1.1.0"

var _ = Describe("Network Segmentation", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("network-segmentation")
	// disable automatic namespace creation, we need to add the required UDN label
	f.SkipNamespaceCreation = true

	var (
		cs        clientset.Interface
		nadClient nadclient.K8sCniCncfIoV1Interface
	)

	const (
		nodeHostnameKey                     = "kubernetes.io/hostname"
		podClusterNetPort            uint16 = 9000
		podClusterNetDefaultPort     uint16 = 8080
		userDefinedNetworkIPv4Subnet        = "172.16.0.0/16" // first subnet in private range 172.16.0.0/12 (rfc1918)
		userDefinedNetworkIPv6Subnet        = "2014:100:200::0/60"
		customL2IPv4Gateway                 = "172.16.0.3"
		customL2IPv6Gateway                 = "2014:100:200::3"
		customL2IPv4ReservedCIDR            = "172.16.1.0/24"
		customL2IPv6ReservedCIDR            = "2014:100:200::100/120"
		customL2IPv4InfraCIDR               = "172.16.0.0/30"
		customL2IPv6InfraCIDR               = "2014:100:200::/122"
		userDefinedNetworkName              = "hogwarts"
		nadName                             = "gryffindor"
	)

	BeforeEach(func() {
		cs = f.ClientSet

		var err error
		nadClient, err = nadclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		f.Namespace = namespace
		Expect(err).NotTo(HaveOccurred())
	})

	Context("a user defined primary network", func() {

		DescribeTableSubtree("created using",
			func(createNetworkFn func(c *networkAttachmentConfigParams) error) {

				DescribeTable(
					"creates a networkStatus Annotation with UDN interface",
					func(netConfig *networkAttachmentConfigParams) {
						By("creating the network")
						netConfig.namespace = f.Namespace.Name
						netConfig.cidr = filterCIDRsAndJoin(f.ClientSet, netConfig.cidr)
						Expect(createNetworkFn(netConfig)).To(Succeed())

						By("creating a pod on the udn namespace")
						podConfig := *podConfig("some-pod")
						podConfig.namespace = f.Namespace.Name
						pod := runUDNPod(cs, f.Namespace.Name, podConfig, nil)

						By("asserting the pod UDN interface on the network-status annotation")
						udnNetStat, err := podNetworkStatus(pod, func(status nadapi.NetworkStatus) bool {
							return status.Default
						})
						Expect(err).NotTo(HaveOccurred())
						const (
							expectedDefaultNetStatusLen = 1
							ovnUDNInterface             = "ovn-udn1"
						)
						Expect(udnNetStat).To(HaveLen(expectedDefaultNetStatusLen))
						Expect(udnNetStat[0].Interface).To(Equal(ovnUDNInterface))

						for _, serverIP := range udnNetStat[0].IPs {
							By("asserting the server pod has an IP from the configured range")
							Expect(inAnyConfiguredSubnet(netConfig.cidr, serverIP)).To(Succeed())
						}
					},
					Entry("L2 primary UDN",
						&networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer2",
							cidr:     joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:     "primary",
						},
					),
					Entry("L2 primary UDN with custom network",
						&networkAttachmentConfigParams{
							name:                nadName,
							topology:            "layer2",
							cidr:                joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:                "primary",
							defaultGatewayIPs:   joinStrings(customL2IPv4Gateway, customL2IPv6Gateway),
							reservedCIDRs:       joinStrings(customL2IPv4ReservedCIDR, customL2IPv6ReservedCIDR),
							infrastructureCIDRs: joinStrings(customL2IPv4InfraCIDR, customL2IPv6InfraCIDR),
						},
					),
					Entry("L3 primary UDN",
						&networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer3",
							cidr:     primaryLayer3MultiCIDRs(),
							role:     "primary",
						},
					),
				)

				DescribeTable(
					"can perform east/west traffic between nodes",
					func(
						netConfig *networkAttachmentConfigParams,
						clientPodConfig podConfiguration,
						serverPodConfig podConfiguration,
					) {
						By("ensure 2 scheduable Nodes")
						nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
						framework.ExpectNoError(err)
						if len(nodes.Items) < 2 {
							ginkgo.Skip("requires at least 2 Nodes")
						}
						node1Name, node2Name := nodes.Items[0].GetName(), nodes.Items[1].GetName()

						By("creating the network")
						netConfig.namespace = f.Namespace.Name
						netConfig.cidr = filterCIDRsAndJoin(f.ClientSet, netConfig.cidr)
						Expect(createNetworkFn(netConfig)).To(Succeed())

						By("creating client/server pods")
						serverPodConfig.namespace = f.Namespace.Name
						serverPodConfig.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
						clientPodConfig.namespace = f.Namespace.Name
						clientPodConfig.nodeSelector = map[string]string{nodeHostnameKey: node2Name}

						runUDNPod(cs, f.Namespace.Name, serverPodConfig, nil)
						runUDNPod(cs, f.Namespace.Name, clientPodConfig, nil)

						serverIPs, err := getPodAnnotationIPsForAttachment(cs, f.Namespace.Name, serverPodConfig.name, namespacedName(f.Namespace.Name, netConfig.name))
						Expect(err).NotTo(HaveOccurred())
						for _, serverIP := range serverIPs {
							By("asserting the server pod has an IP from the configured range")
							Expect(inAnyConfiguredSubnet(netConfig.cidr, serverIP.IP.String())).To(Succeed())

							By("asserting the *client* pod can contact the server pod exposed endpoint")
							Eventually(func() error {
								return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP.IP.String(), podClusterNetPort)
							}, 2*time.Minute, 6*time.Second).Should(Succeed())
						}
					},
					Entry(
						"two pods connected over a L2 primary UDN",
						&networkAttachmentConfigParams{
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
								return httpServerContainerCmd(podClusterNetPort)
							}),
						),
					),
					Entry(
						"two pods connected over a L2 primary UDN with custom network",
						&networkAttachmentConfigParams{
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
								return httpServerContainerCmd(podClusterNetPort)
							}),
						),
					),
					Entry(
						"two pods connected over a L3 primary UDN",
						&networkAttachmentConfigParams{
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
								return httpServerContainerCmd(podClusterNetPort)
							}),
						),
					),
				)

				DescribeTable(
					"is isolated from the default network",
					func(
						netConfigParams *networkAttachmentConfigParams,
						udnPodConfig podConfiguration,
					) {
						By("ensure enough schedable nodes exist")
						nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), cs, 1)
						Expect(err).NotTo(HaveOccurred())
						if len(nodes.Items) < 1 {
							framework.Failf("expect at least one Node: %v", err)
						}
						nodeName := nodes.Items[0].Name
						udnPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodeName}

						By("Creating second namespace for default network pods")
						defaultNetNamespace := f.Namespace.Name + "-default"
						_, err = cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
							ObjectMeta: metav1.ObjectMeta{
								Name: defaultNetNamespace,
							},
						}, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
						// required so the namespaces get cleaned up
						defer func() {
							Expect(cs.CoreV1().Namespaces().Delete(context.Background(), defaultNetNamespace, metav1.DeleteOptions{})).To(Succeed())
						}()

						By("creating the network")
						netConfigParams.namespace = f.Namespace.Name
						Expect(createNetworkFn(netConfigParams)).To(Succeed())

						udnPodConfig.namespace = f.Namespace.Name
						udnPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[0].Name}

						udnPod := runUDNPod(cs, f.Namespace.Name, udnPodConfig, func(pod *v1.Pod) {
							pod.Spec.Containers[0].ReadinessProbe = &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									HTTPGet: &v1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(int(podClusterNetPort)),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       1,
								FailureThreshold:    1,
							}
							pod.Spec.Containers[0].LivenessProbe = &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									HTTPGet: &v1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(int(podClusterNetPort)),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       1,
								FailureThreshold:    1,
							}
							pod.Spec.Containers[0].StartupProbe = &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									HTTPGet: &v1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(int(podClusterNetPort)),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       1,
								FailureThreshold:    3,
							}
							// add NET_ADMIN to change pod routes
							pod.Spec.Containers[0].SecurityContext = &v1.SecurityContext{
								Capabilities: &v1.Capabilities{
									Add: []v1.Capability{"NET_ADMIN"},
								},
							}
						})

						By("creating default network pod")
						defaultPod, err := createPod(f, "default-net-pod", nodeName,
							defaultNetNamespace, []string{"/agnhost", "netexec"}, nil)
						Expect(err).NotTo(HaveOccurred())
						By("creating default network client pod")
						defaultClientPod, err := createPod(f, "default-net-client-pod", nodeName,
							defaultNetNamespace, []string{}, nil)
						Expect(err).NotTo(HaveOccurred())

						udnIPv4, udnIPv6, err := podIPsForDefaultNetwork(
							cs,
							f.Namespace.Name,
							udnPod.GetName(),
						)
						Expect(err).NotTo(HaveOccurred())

						for _, destIP := range []string{udnIPv4, udnIPv6} {
							if destIP == "" {
								continue
							}
							// positive case for UDN pod is a successful healthcheck, checked later
							By("checking the default network pod can't reach UDN pod on IP " + destIP)
							Consistently(func() bool {
								return connectToServer(podConfiguration{namespace: defaultPod.Namespace, name: defaultPod.Name}, destIP, podClusterNetPort) != nil
							}, 5*time.Second).Should(BeTrue())
						}

						defaultIPv4, defaultIPv6, err := podIPsForDefaultNetwork(
							cs,
							defaultPod.Namespace,
							defaultPod.Name,
						)
						Expect(err).NotTo(HaveOccurred())

						for _, destIP := range []string{defaultIPv4, defaultIPv6} {
							if destIP == "" {
								continue
							}
							By("checking the default network client pod can reach default pod on IP " + destIP)
							Eventually(func() bool {
								return connectToServer(podConfiguration{namespace: defaultClientPod.Namespace, name: defaultClientPod.Name}, destIP, podClusterNetDefaultPort) == nil
							}).Should(BeTrue())
							By("checking the UDN pod can't reach the default network pod on IP " + destIP)
							Consistently(func() bool {
								return connectToServer(udnPodConfig, destIP, podClusterNetDefaultPort) != nil
							}, 5*time.Second).Should(BeTrue())
						}

						// connectivity check is run every second + 1sec initialDelay
						// By this time we have spent at least 8 seconds doing the above checks
						udnPod, err = cs.CoreV1().Pods(udnPod.Namespace).Get(context.Background(), udnPod.Name, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())

						By("asserting healthcheck works (kubelet can access the UDN pod)")
						// The pod should be ready
						Expect(podutils.IsPodReady(udnPod)).To(BeTrue(), fmt.Sprintf("UDN pod is not ready: %v", udnPod))

						Expect(udnPod.Status.ContainerStatuses[0].RestartCount).To(Equal(int32(0)))

						By("restarting kubelet, pod should stay ready")
						_, err = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"systemctl", "restart", "kubelet"})
						Expect(err).NotTo(HaveOccurred())

						By("asserting healthcheck still works (kubelet can access the UDN pod)")
						// The pod should stay ready
						Consistently(func() bool {
							return podutils.IsPodReady(udnPod)
						}, 10*time.Second, 1*time.Second).Should(BeTrue())
						Expect(udnPod.Status.ContainerStatuses[0].RestartCount).To(Equal(int32(0)))

						By("checking default network hostNetwork pod and non-kubelet host process can't reach the UDN pod")
						hostNetPod, err := createPod(f, "host-net-pod", nodeName,
							defaultNetNamespace, []string{}, nil, func(pod *v1.Pod) {
								pod.Spec.HostNetwork = true
							})
						Expect(err).NotTo(HaveOccurred())

						// positive check for reachable default network pod
						for _, destIP := range []string{defaultIPv4, defaultIPv6} {
							if destIP == "" {
								continue
							}
							By("checking the default network hostNetwork can reach default pod on IP " + destIP)
							Eventually(func() bool {
								return connectToServer(podConfiguration{namespace: hostNetPod.Namespace, name: hostNetPod.Name}, destIP, podClusterNetDefaultPort) == nil
							}).Should(BeTrue())
							By("checking the non-kubelet host process can reach default pod on IP " + destIP)
							Eventually(func() bool {
								_, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{
									"curl", "--connect-timeout", "2",
									net.JoinHostPort(destIP, fmt.Sprintf("%d", podClusterNetDefaultPort)),
								})
								return err == nil
							}).Should(BeTrue())
						}
						// negative check for UDN pod
						for _, destIP := range []string{udnIPv4, udnIPv6} {
							if destIP == "" {
								continue
							}

							By("checking the default network hostNetwork pod can't reach UDN pod on IP " + destIP)
							Consistently(func() bool {
								return connectToServer(podConfiguration{namespace: hostNetPod.Namespace, name: hostNetPod.Name}, destIP, podClusterNetPort) != nil
							}, 5*time.Second).Should(BeTrue())

							By("checking the non-kubelet host process can't reach UDN pod on IP " + destIP)
							Consistently(func() bool {
								_, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{
									"curl", "--connect-timeout", "2",
									net.JoinHostPort(destIP, fmt.Sprintf("%d", podClusterNetPort)),
								})
								return err != nil
							}, 5*time.Second).Should(BeTrue())
						}

						By("asserting UDN pod can reach the kapi service in the default network")
						// Use the service name to get test the DNS access
						Consistently(func() bool {
							_, err := e2ekubectl.RunKubectl(
								udnPodConfig.namespace,
								"exec",
								udnPodConfig.name,
								"--",
								"curl",
								"--connect-timeout",
								"2",
								"--insecure",
								"https://kubernetes.default/healthz")
							if err != nil {
								framework.Logf("connecting to kapi service failed: %v", err)
							}
							return err == nil
						}, 5*time.Second).Should(BeTrue())
						By("asserting UDN pod can't reach host via default network interface")
						// Now try to reach the host from the UDN pod
						defaultPodHostIP := udnPod.Status.HostIPs
						for _, hostIP := range defaultPodHostIP {
							By("checking the UDN pod can't reach the host on IP " + hostIP.IP)
							ping := "ping"
							if utilnet.IsIPv6String(hostIP.IP) {
								ping = "ping6"
							}
							Consistently(func() bool {
								_, err := e2ekubectl.RunKubectl(udnPod.Namespace, "exec", udnPod.Name, "--",
									ping, "-I", "eth0", "-c", "1", "-W", "1", hostIP.IP,
								)
								return err == nil
							}, 4*time.Second).Should(BeFalse())
						}

						By("asserting UDN pod can't reach default services via default network interface")
						// route setup is already done, get kapi IPs
						kapi, err := cs.CoreV1().Services("default").Get(context.Background(), "kubernetes", metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						for _, kapiIP := range kapi.Spec.ClusterIPs {
							By("checking the UDN pod can't reach kapi service on IP " + kapiIP + "via eth0")
							Consistently(func() bool {
								_, err := e2ekubectl.RunKubectl(
									udnPodConfig.namespace,
									"exec",
									udnPodConfig.name,
									"--",
									"curl",
									"--connect-timeout",
									"2",
									"--interface",
									"eth0",
									"--insecure",
									fmt.Sprintf("https://%s/healthz", kapiIP))
								return err != nil
							}, 5*time.Second).Should(BeTrue())
						}
					},
					Entry(
						"with L2 primary UDN",
						&networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer2",
							cidr:     joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:     "primary",
						},
						*podConfig(
							"udn-pod",
							withCommand(func() []string {
								return httpServerContainerCmd(podClusterNetPort)
							}),
						),
					),
					Entry(
						"with L2 primary UDN with custom network",
						&networkAttachmentConfigParams{
							name:                nadName,
							topology:            "layer2",
							cidr:                joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:                "primary",
							defaultGatewayIPs:   joinStrings(customL2IPv4Gateway, customL2IPv6Gateway),
							reservedCIDRs:       joinStrings(customL2IPv4ReservedCIDR, customL2IPv6ReservedCIDR),
							infrastructureCIDRs: joinStrings(customL2IPv4InfraCIDR, customL2IPv6InfraCIDR),
						},
						*podConfig(
							"udn-pod",
							withCommand(func() []string {
								return httpServerContainerCmd(podClusterNetPort)
							}),
						),
					),
					Entry(
						"with L3 primary UDN",
						&networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer3",
							cidr:     primaryLayer3MultiCIDRs(),
							role:     "primary",
						},
						*podConfig(
							"udn-pod",
							withCommand(func() []string {
								return httpServerContainerCmd(podClusterNetPort)
							}),
						),
					),
				)
				DescribeTable(
					"isolates overlapping CIDRs",
					func(
						topology string,
						numberOfPods int,
						userDefinedv4Subnet string,
						userDefinedv6Subnet string,

					) {

						red := "red"
						blue := "blue"

						namespaceRed := f.Namespace.Name + "-" + red
						namespaceBlue := f.Namespace.Name + "-" + blue

						nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), f.ClientSet, 2)
						framework.ExpectNoError(err, "two scheduable nodes are required")
						if len(nodes.Items) < 2 {
							ginkgo.Skip("requires at least 2 Nodes")
						}
						node1Name, node2Name := nodes.Items[0].GetName(), nodes.Items[1].GetName()

						for _, namespace := range []string{namespaceRed, namespaceBlue} {
							By("Creating namespace " + namespace)
							_, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
								ObjectMeta: metav1.ObjectMeta{
									Name:   namespace,
									Labels: map[string]string{RequiredUDNNamespaceLabel: ""},
								},
							}, metav1.CreateOptions{})
							Expect(err).NotTo(HaveOccurred())
							defer func() {
								By("Removing namespace " + namespace)
								Expect(cs.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})).To(Succeed())
							}()
						}
						networkNamespaceMap := map[string]string{namespaceRed: red, namespaceBlue: blue}
						for namespace, network := range networkNamespaceMap {
							By("creating the network " + network + " in namespace " + namespace)

							netConfig := &networkAttachmentConfigParams{
								topology:  topology,
								cidr:      joinStrings(userDefinedv4Subnet, userDefinedv6Subnet),
								role:      "primary",
								namespace: namespace,
								name:      network,
							}

							Expect(createNetworkFn(netConfig)).To(Succeed())
							// update the name because createNetworkFn may mutate the netConfig.name
							// for cluster scope objects (i.g.: CUDN cases) to enable parallel testing.
							networkNamespaceMap[namespace] = netConfig.name
						}
						red = networkNamespaceMap[namespaceRed]
						blue = networkNamespaceMap[namespaceBlue]

						pods := []*v1.Pod{}
						podIPs := []string{}
						redIPs := map[string]bool{}
						blueIPs := map[string]bool{}
						bluePort := uint16(9091)
						redPort := uint16(9092)
						for namespace, network := range networkNamespaceMap {
							for i := range numberOfPods {
								httpServerPort := redPort
								if network != red {
									httpServerPort = bluePort
								}
								podConfig := *podConfig(
									fmt.Sprintf("%s-pod-%d", network, i),
									withCommand(func() []string {
										return httpServerContainerCmd(httpServerPort)
									}),
								)
								podConfig.namespace = namespace
								//ensure testing accross nodes
								if i%2 == 0 {
									podConfig.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
								} else {
									podConfig.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
								}
								By("creating pod " + podConfig.name + " in " + podConfig.namespace)
								pod := runUDNPod(cs, podConfig.namespace, podConfig, nil)
								pods = append(pods, pod)
								podIP, err := getPodAnnotationIPsForAttachmentByIndex(
									cs,
									pod.Namespace,
									pod.Name,
									namespacedName(namespace, network),
									0,
								)
								Expect(err).NotTo(HaveOccurred())
								podIPs = append(podIPs, podIP)
								if network == red {
									redIPs[podIP] = true
								} else {
									blueIPs[podIP] = true
								}
							}
						}

						By("ensuring pods only communicate with pods in their network")
						for _, pod := range pods {
							isRedPod := strings.Contains(pod.Name, red)
							expectedHostname := red
							if !isRedPod {
								expectedHostname = blue
							}
							for _, ip := range podIPs {
								isRedIP := redIPs[ip]
								httpServerPort := redPort
								if !isRedIP {
									httpServerPort = bluePort
								}
								result, err := e2ekubectl.RunKubectl(
									pod.Namespace,
									"exec",
									pod.Name,
									"--",
									"curl",
									"--connect-timeout",
									"2",
									net.JoinHostPort(ip, fmt.Sprintf("%d", httpServerPort)+"/hostname"),
								)

								sameNetwork := isRedPod == redIPs[ip]
								if !sameNetwork {
									Expect(err).To(HaveOccurred(), "should isolate from different networks")
								} else {
									Expect(err).NotTo(HaveOccurred())
									Expect(strings.Contains(result, expectedHostname)).To(BeTrue())
								}
							}
						}
					},
					// can completely fill the L2 topology because it does not depend on the size of the clusters hostsubnet
					Entry(
						"with L2 primary UDN",
						"layer2",
						4,
						"172.16.0.0/29",
						"2014:100:200::0/125",
					),
					// limit the number of pods to 10
					Entry(
						"with L3 primary UDN",
						"layer3",
						10,
						primaryLayer3MultiIPv4CIDRs(),
						primaryLayer3MultiIPv6CIDRs(),
					),
				)
			},
			Entry("NetworkAttachmentDefinitions", func(c *networkAttachmentConfigParams) error {
				netConfig := newNetworkAttachmentConfig(*c)
				nad := generateNAD(netConfig, f.ClientSet)
				_, err := nadClient.NetworkAttachmentDefinitions(c.namespace).Create(context.Background(), nad, metav1.CreateOptions{})
				return err
			}),
			Entry("UserDefinedNetwork", func(c *networkAttachmentConfigParams) error {
				udnManifest := generateUserDefinedNetworkManifest(c, f.ClientSet)
				cleanup, err := createManifest(c.namespace, udnManifest)
				DeferCleanup(cleanup)
				Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, c.namespace, c.name), 5*time.Second, time.Second).Should(Succeed())
				return err
			}),
			Entry("ClusterUserDefinedNetwork", func(c *networkAttachmentConfigParams) error {
				cudnName := randomNetworkMetaName()
				c.name = cudnName
				cudnManifest := generateClusterUserDefinedNetworkManifest(c, f.ClientSet)
				cleanup, err := createManifest("", cudnManifest)
				DeferCleanup(func() {
					cleanup()
					By(fmt.Sprintf("delete pods in %s namespace to unblock CUDN CR & associate NAD deletion", c.namespace))
					Expect(cs.CoreV1().Pods(c.namespace).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
					_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", fmt.Sprintf("--timeout=%ds", 120))
					Expect(err).NotTo(HaveOccurred())
				})
				Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, c.name), 5*time.Second, time.Second).Should(Succeed())
				return err
			}),
		)

		It("doesn't cause network name conflict", func() {
			// generate 2 UDNs with ns+name
			// "f.Namespace.Name" + "tenant-blue"
			// "f.Namespace.Name-tenant" + "blue"
			netConfig1 := networkAttachmentConfigParams{
				name:      "tenant-blue",
				namespace: f.Namespace.Name,
				topology:  "layer2",
				cidr:      joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
				role:      "primary",
			}
			netConfig2 := networkAttachmentConfigParams{
				name:      "blue",
				namespace: f.Namespace.Name + "-tenant",
				topology:  "layer2",
				cidr:      joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
				role:      "primary",
			}
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 2 {
				ginkgo.Skip("requires at least 2 Nodes")
			}
			node1Name, node2Name := nodes.Items[0].Name, nodes.Items[1].Name
			clientPodConfig := *podConfig(
				"client-pod",
			)
			serverPodConfig := *podConfig(
				"server-pod",
				withCommand(func() []string {
					return httpServerContainerCmd(podClusterNetPort)
				}),
			)
			By("creating second namespace")
			_, err = cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   netConfig2.namespace,
					Labels: map[string]string{RequiredUDNNamespaceLabel: ""},
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			// required so the namespaces get cleaned up
			defer func() {
				Expect(cs.CoreV1().Namespaces().Delete(context.Background(), netConfig2.namespace, metav1.DeleteOptions{})).To(Succeed())
			}()

			By(fmt.Sprintf("creating the network in namespace %s", netConfig1.namespace))
			udnManifest := generateUserDefinedNetworkManifest(&netConfig1, f.ClientSet)
			cleanup, err := createManifest(netConfig1.namespace, udnManifest)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(cleanup)
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, netConfig1.namespace, netConfig1.name), 5*time.Second, time.Second).Should(Succeed())

			By(fmt.Sprintf("creating client/server pods in namespace %s", netConfig1.namespace))
			serverPodConfig.namespace = netConfig1.namespace
			clientPodConfig.namespace = netConfig1.namespace
			runUDNPod(cs, netConfig1.namespace, serverPodConfig, nil)
			runUDNPod(cs, netConfig1.namespace, clientPodConfig, nil)

			By(fmt.Sprintf("creating the network in namespace %s", netConfig2.namespace))
			udnManifest = generateUserDefinedNetworkManifest(&netConfig2, f.ClientSet)
			cleanup2, err := createManifest(netConfig2.namespace, udnManifest)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(cleanup2)
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, netConfig2.namespace, netConfig2.name), 5*time.Second, time.Second).Should(Succeed())

			By(fmt.Sprintf("creating client/server pods in namespace %s", netConfig2.namespace))
			serverPodConfig.namespace = netConfig2.namespace
			serverPodConfig.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			clientPodConfig.namespace = netConfig2.namespace
			clientPodConfig.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			runUDNPod(cs, netConfig2.namespace, serverPodConfig, nil)
			runUDNPod(cs, netConfig2.namespace, clientPodConfig, nil)

			var serverIP string
			for _, config := range []networkAttachmentConfigParams{netConfig1, netConfig2} {
				serverPodConfig.namespace = config.namespace
				clientPodConfig.namespace = config.namespace
				By(fmt.Sprintf("asserting network works in namespace %s", config.namespace))
				for i, cidr := range strings.Split(config.cidr, ",") {
					if cidr != "" {
						serverIP, err = getPodAnnotationIPsForAttachmentByIndex(
							cs,
							config.namespace,
							serverPodConfig.name,
							namespacedName(config.namespace, config.name),
							i,
						)
						Expect(err).NotTo(HaveOccurred())

						By("asserting the *client* pod can contact the server pod exposed endpoint")
						Eventually(func() error {
							return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, podClusterNetPort)
						}, 2*time.Minute, 6*time.Second).Should(Succeed())
					}
				}
			}
		})

		Context("with multicast feature enabled for namespace", func() {
			var (
				clientNodeInfo, serverNodeInfo nodeInfo
			)
			BeforeEach(func() {

				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
				framework.ExpectNoError(err)
				if len(nodes.Items) < 2 {
					e2eskipper.Skipf(
						"Test requires >= 2 Ready nodes, but there are only %v nodes",
						len(nodes.Items))
				}

				ips := e2enode.CollectAddresses(nodes, v1.NodeInternalIP)

				clientNodeInfo = nodeInfo{
					name:   nodes.Items[0].Name,
					nodeIP: ips[0],
				}

				serverNodeInfo = nodeInfo{
					name:   nodes.Items[1].Name,
					nodeIP: ips[1],
				}

				enableMulticastForNamespace(f)
			})
			DescribeTable("should be able to send multicast UDP traffic between nodes", func(netConfigParams networkAttachmentConfigParams) {
				ginkgo.By("creating the attachment configuration")
				netConfigParams.namespace = f.Namespace.Name
				filterSupportedNetworkConfig(f.ClientSet, &netConfigParams)
				netConfig := newNetworkAttachmentConfig(netConfigParams)
				_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					generateNAD(netConfig, f.ClientSet),
					metav1.CreateOptions{},
				)
				framework.ExpectNoError(err)
				testMulticastUDPTraffic(f, clientNodeInfo, serverNodeInfo, udnPodInterface)
			},
				ginkgo.Entry("with primary layer3 UDN", networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer3",
					cidr:     primaryLayer3MultiCIDRs(),
					role:     "primary",
				}),
				ginkgo.Entry("with primary layer2 UDN", networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer2",
					cidr:     joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:     "primary",
				}),
				ginkgo.Entry("with primary layer2 UDN with custom network", networkAttachmentConfigParams{
					name:                nadName,
					topology:            "layer2",
					cidr:                joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:                "primary",
					defaultGatewayIPs:   joinStrings(customL2IPv4Gateway, customL2IPv6Gateway),
					reservedCIDRs:       joinStrings(customL2IPv4ReservedCIDR, customL2IPv6ReservedCIDR),
					infrastructureCIDRs: joinStrings(customL2IPv4InfraCIDR, customL2IPv6InfraCIDR),
				}),
			)
			DescribeTable("should be able to receive multicast IGMP query", func(netConfigParams networkAttachmentConfigParams) {
				ginkgo.By("creating the attachment configuration")
				netConfigParams.namespace = f.Namespace.Name
				filterSupportedNetworkConfig(f.ClientSet, &netConfigParams)
				netConfig := newNetworkAttachmentConfig(netConfigParams)
				_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					generateNAD(netConfig, f.ClientSet),
					metav1.CreateOptions{},
				)
				framework.ExpectNoError(err)
				testMulticastIGMPQuery(f, clientNodeInfo, serverNodeInfo)
			},
				ginkgo.Entry("with primary layer3 UDN", networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer3",
					cidr:     primaryLayer3MultiCIDRs(),
					role:     "primary",
				}),
				// TODO: this test is broken, see https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5309
				//ginkgo.Entry("with primary layer2 UDN", networkAttachmentConfigParams{
				//	name:     nadName,
				//	topology: "layer2",
				//	cidr:     joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
				//	role:     "primary",
				//}),
				//ginkgo.Entry("with primary layer2 UDN with custom network", networkAttachmentConfigParams{
				//	name:              nadName,
				//	topology:          "layer2",
				//	cidr:              joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
				//	role:              "primary",
				//	defaultGatewayIPs: joinStrings(customL2IPv4Gateway, customL2IPv6Gateway),
				//	reservedCIDRs:     joinStrings(customL2IPv4ReservedCIDR, customL2IPv6ReservedCIDR),
				//	infraCIDRs:        joinStrings(customL2IPv4InfraCIDR, customL2IPv6InfraCIDR),
				//}),
			)
		})
	})

	Context("layer3 primary network with multi-subnets", func() {
		DescribeTableSubtree("created using",
			func(createNetworkFn func(netConfig *networkAttachmentConfigParams) error) {

				BeforeEach(func() {
					nodeList, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
					framework.ExpectNoError(err)
					if len(nodeList.Items) < 3 {
						e2eskipper.Skipf("need at least 3 ready schedulable nodes to run this test")
					}
				})

				DescribeTableSubtree("with",
					func(netConfig *networkAttachmentConfigParams) {
						var (
							networkName                                  string
							vrfName                                      string
							nodes                                        []*v1.Node
							nodeToNodeSubnetV4, nodeToNodeSubnetv6       map[string]string
							nodeToNetworkSubnetV4, nodeToNetworkSubnetV6 map[string]string
							clusterCIDRsv4, clusterCIDRsv6               map[string]struct{}
							ipv4, ipv6                                   bool
						)

						BeforeEach(func() {
							By("validate test config")
							clusterCIDRsv4 = make(map[string]struct{})
							clusterCIDRsv6 = make(map[string]struct{})
							netConfig.cidr = filterCIDRsAndJoin(f.ClientSet, netConfig.cidr)
							for _, cidr := range strings.Split(netConfig.cidr, ",") {
								c, err := getNetCIDRSubnet(cidr)
								Expect(err).NotTo(HaveOccurred())
								if utilnet.IsIPv4CIDRString(c) {
									clusterCIDRsv4[c] = struct{}{}
								} else {
									clusterCIDRsv6[c] = struct{}{}
								}
							}

							ipv4, ipv6 = getSupportedIPFamilies(cs)
							if ipv4 {
								Expect(len(clusterCIDRsv4)).To(BeNumerically(">=", 2), "need at least 2 different IPv4 CIDRs")
							}
							if ipv6 {
								Expect(len(clusterCIDRsv6)).To(BeNumerically(">=", 2), "need at least 2 different IPv6 CIDRs")
							}
							By("creating the network with multiple CIDRs")
							netConfig.namespace = f.Namespace.Name
							Expect(createNetworkFn(netConfig)).To(Succeed())

							By("waiting for node subnet allocation on all schedulable nodes")
							nad, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Get(context.TODO(), netConfig.name, metav1.GetOptions{})
							Expect(err).NotTo(HaveOccurred())
							networkName = nad.Annotations["k8s.ovn.org/network-name"]
							networkID := nad.Annotations[ovntypes.OvnNetworkIDAnnotation]
							Expect(networkID).NotTo(BeEmpty())
							vrfName = fmt.Sprintf("%s%s%s", ovntypes.UDNVRFDevicePrefix, networkID, ovntypes.UDNVRFDeviceSuffix)
							udnNamespace, udnName := ovnkubeutil.ParseNetworkName(networkName)
							if udnNamespace == "" && udnName != "" && len(udnName) <= 15 {
								vrfName = udnName
							}

							if isDynamicUDNEnabled() {
								nodeList, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
								Expect(err).NotTo(HaveOccurred())
								for i := range nodeList.Items {
									node := &nodeList.Items[i]
									runUDNPod(cs, f.Namespace.Name, podConfiguration{
										name:         fmt.Sprintf("subnet-allocator-%d", i),
										namespace:    f.Namespace.Name,
										containerCmd: []string{"/agnhost", "pause"},
										nodeSelector: map[string]string{nodeHostnameKey: node.Name},
									}, nil)
								}
							}

							Eventually(func() error {
								nodeList, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
								if err != nil {
									return err
								}

								nodeToNodeSubnetV4 = map[string]string{}
								nodeToNodeSubnetv6 = map[string]string{}
								nodeToNetworkSubnetV4 = map[string]string{}
								nodeToNetworkSubnetV6 = map[string]string{}
								nodes = nil
								for i := range nodeList.Items {
									node := &nodeList.Items[i]
									ipnets, err := ovnkubeutil.ParseNodeHostSubnetAnnotation(node, networkName)
									if err != nil {
										if ovnkubeutil.IsAnnotationNotSetError(err) {
											continue
										}
										return err
									}
									nodes = append(nodes, node)
									for _, ipnet := range ipnets {
										networkSubnet, err := containingNetworkSubnet(ipnet, clusterCIDRsv4, clusterCIDRsv6)
										if err != nil {
											return err
										}
										if utilnet.IsIPv4CIDR(ipnet) {
											nodeToNodeSubnetV4[node.Name] = ipnet.String()
											nodeToNetworkSubnetV4[node.Name] = networkSubnet
										} else {
											nodeToNodeSubnetv6[node.Name] = ipnet.String()
											nodeToNetworkSubnetV6[node.Name] = networkSubnet
										}
									}
								}
								if len(nodes) != len(nodeList.Items) {
									return fmt.Errorf("only %d/%d nodes have a node subnet allocated for network %q", len(nodes), len(nodeList.Items), networkName)
								}
								if ipv4 && len(nodeToNodeSubnetV4) != len(nodes) {
									return fmt.Errorf("only %d/%d nodes have an IPv4 node subnet allocated for network %q", len(nodeToNodeSubnetV4), len(nodes), networkName)
								}
								if ipv6 && len(nodeToNodeSubnetv6) != len(nodes) {
									return fmt.Errorf("only %d/%d nodes have an IPv6 node subnet allocated for network %q", len(nodeToNodeSubnetv6), len(nodes), networkName)
								}
								return nil
							}, 60*time.Second, 2*time.Second).Should(Succeed())
						})

						findNodePair := func(sameNetworkSubnet bool) (string, string) {
							GinkgoHelper()
							for i := 0; i < len(nodes)-1; i++ {
								for j := i + 1; j < len(nodes); j++ {
									ni, nj := nodes[i].Name, nodes[j].Name
									v4Match := !ipv4 ||
										nodeToNetworkSubnetV4[ni] != "" &&
											nodeToNetworkSubnetV4[nj] != "" &&
											(nodeToNetworkSubnetV4[ni] == nodeToNetworkSubnetV4[nj]) == sameNetworkSubnet
									v6Match := !ipv6 ||
										nodeToNetworkSubnetV6[ni] != "" &&
											nodeToNetworkSubnetV6[nj] != "" &&
											(nodeToNetworkSubnetV6[ni] == nodeToNetworkSubnetV6[nj]) == sameNetworkSubnet
									if v4Match && v6Match {
										return ni, nj
									}
								}
							}
							relation := "different"
							if sameNetworkSubnet {
								relation = "the same"
							}
							Fail(fmt.Sprintf("can not find two nodes allocated from %s IPv4 and IPv6 network subnets", relation))
							return "", ""
						}

						assertHostToPodConnectivity := func(clientNodeName, serverNodeName string) {
							GinkgoHelper()

							By(fmt.Sprintf("creating a primary UDN server pod on node %s", serverNodeName))
							serverPodConfig := podConfiguration{
								name:         "server-pod",
								namespace:    f.Namespace.Name,
								containerCmd: httpServerContainerCmd(podClusterNetPort),
								nodeSelector: map[string]string{nodeHostnameKey: serverNodeName},
							}
							runUDNPod(cs, f.Namespace.Name, serverPodConfig, nil)

							By(fmt.Sprintf("creating a host network client pod on node %s", clientNodeName))
							hostNetPod, err := createPod(
								f,
								"client-pod-host",
								clientNodeName,
								f.Namespace.Name,
								[]string{"sleep", "3600"},
								nil,
								func(pod *v1.Pod) {
									pod.Spec.HostNetwork = true
									pod.Spec.Containers[0].Image = images.Netshoot()
									pod.Spec.Containers[0].SecurityContext = &v1.SecurityContext{
										Privileged: pointer.Bool(true),
									}
								},
							)
							Expect(err).NotTo(HaveOccurred())

							serverIPs, err := getPodAnnotationIPsForAttachment(
								cs,
								f.Namespace.Name,
								serverPodConfig.name,
								namespacedName(f.Namespace.Name, netConfig.name),
							)
							Expect(err).NotTo(HaveOccurred())
							Expect(serverIPs).NotTo(BeEmpty())

							for _, serverIP := range serverIPs {
								By(fmt.Sprintf(
									"checking host network pod on node %s can reach primary UDN pod %s on node %s through VRF %s",
									clientNodeName,
									serverIP.IP,
									serverNodeName,
									vrfName,
								))
								Eventually(func() error {
									output, err := e2ekubectl.RunKubectl(
										hostNetPod.Namespace,
										"exec",
										hostNetPod.Name,
										"--",
										"curl",
										"--fail",
										"--silent",
										"--show-error",
										"--connect-timeout",
										"2",
										"--max-time",
										"5",
										"--interface",
										vrfName,
										"http://"+net.JoinHostPort(serverIP.IP.String(), fmt.Sprintf("%d", podClusterNetPort)),
									)
									if err != nil {
										return fmt.Errorf("failed to connect through VRF %s: %w; output: %s", vrfName, err, output)
									}
									return nil
								}, 6*time.Minute, 6*time.Second).Should(Succeed())
							}
						}

						It("can perform east/west traffic between nodes on different CIDR", func() {
							clientNodeName, serverNodeName := findNodePair(false)

							By("creating pods on nodes")
							clientPodConfig := podConfiguration{
								name:         "client-pod",
								namespace:    f.Namespace.Name,
								nodeSelector: map[string]string{nodeHostnameKey: clientNodeName},
							}
							serverPodConfig := podConfiguration{
								name:         "server-pod",
								namespace:    f.Namespace.Name,
								containerCmd: httpServerContainerCmd(podClusterNetPort),
								nodeSelector: map[string]string{nodeHostnameKey: serverNodeName},
							}
							runUDNPod(cs, f.Namespace.Name, clientPodConfig, nil)
							runUDNPod(cs, f.Namespace.Name, serverPodConfig, nil)

							clientIPs, err := getPodAnnotationIPsForAttachment(cs, f.Namespace.Name, clientPodConfig.name, namespacedName(f.Namespace.Name, netConfig.name))
							Expect(err).NotTo(HaveOccurred())
							for _, clientIP := range clientIPs {
								if utilnet.IsIPv4CIDR(clientIP) {
									Expect(inRange(nodeToNodeSubnetV4[clientNodeName], clientIP.IP.String())).To(Succeed())
								} else {
									Expect(inRange(nodeToNodeSubnetv6[clientNodeName], clientIP.IP.String())).To(Succeed())
								}
							}

							serverIPs, err := getPodAnnotationIPsForAttachment(cs, f.Namespace.Name, serverPodConfig.name, namespacedName(f.Namespace.Name, netConfig.name))
							Expect(err).NotTo(HaveOccurred())
							for _, serverIP := range serverIPs {
								if utilnet.IsIPv4CIDR(serverIP) {
									Expect(inRange(nodeToNodeSubnetV4[serverNodeName], serverIP.IP.String())).To(Succeed())
								} else {
									Expect(inRange(nodeToNodeSubnetv6[serverNodeName], serverIP.IP.String())).To(Succeed())
								}
							}

							By("asserting the *client* pod can contact the server pod exposed endpoint")
							for _, serverIP := range serverIPs {
								Eventually(func() error {
									return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP.IP.String(), podClusterNetPort)
								}, 6*time.Minute, 6*time.Second).Should(Succeed())
							}
						})

						It("can perform host to pod traffic via UDN VRF between nodes on different CIDR", func() {
							clientNodeName, serverNodeName := findNodePair(false)
							assertHostToPodConnectivity(clientNodeName, serverNodeName)
						})

						It("can perform host to pod traffic via UDN VRF between nodes on same CIDR", func() {
							clientNodeName, serverNodeName := findNodePair(true)
							assertHostToPodConnectivity(clientNodeName, serverNodeName)
						})

						It("can perform host to pod traffic via UDN VRF on same node", func() {
							Expect(nodes).NotTo(BeEmpty())
							assertHostToPodConnectivity(nodes[0].Name, nodes[0].Name)
						})
					},
					Entry("L3 primary network",
						&networkAttachmentConfigParams{
							name:     nadName,
							topology: "layer3",
							// Use multiple CIDRs per IP family. The first CIDR
							// is just big enough to allocate subnets for
							// the first two nodes, remaining nodes will be
							// allocated subnets from the second CIDR.
							cidr: primaryLayer3MultiCIDRs(),
							role: "primary",
						},
					),
				)

			},
			Entry("UserDefinedNetwork", func(c *networkAttachmentConfigParams) error {
				udnManifest := generateUserDefinedNetworkManifest(c, f.ClientSet)
				cleanup, err := createManifest(c.namespace, udnManifest)
				DeferCleanup(cleanup)
				Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, c.namespace, c.name), 10*time.Second, time.Second).Should(Succeed())
				return err
			}),
			Entry("ClusterUserDefinedNetwork", func(c *networkAttachmentConfigParams) error {
				cudnName := randomNetworkMetaName()
				c.name = cudnName
				cudnManifest := generateClusterUserDefinedNetworkManifest(c, f.ClientSet)
				cleanup, err := createManifest("", cudnManifest)
				DeferCleanup(func() {
					cleanup()
					By(fmt.Sprintf("delete pods in %s namespace to unblock CUDN CR & associate NAD deletion", c.namespace))
					Expect(cs.CoreV1().Pods(c.namespace).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
					_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", fmt.Sprintf("--timeout=%ds", 120))
					Expect(err).NotTo(HaveOccurred())
				})
				Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, c.name), 10*time.Second, time.Second).Should(Succeed())
				return err
			}),
		)

		DescribeTableSubtree("created using",
			func(createNetworkFn func(netConfig *networkAttachmentConfigParams) error,
				updateNetworkFn func(netConfig *networkAttachmentConfigParams) error,
				getDynamicClient func(netConfig *networkAttachmentConfigParams) dynamic.ResourceInterface) {

				It("add subnet not affecting existing node subnet assignment", func() {
					nodeList, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
					framework.ExpectNoError(err)
					if len(nodeList.Items) < 3 {
						e2eskipper.Skipf("need at least 3 ready schedulable nodes to run this test")
					}

					userDefinedNetworkIPv4Subnet1, userDefinedNetworkIPv4Subnet2 := primaryLayer3IPv4CIDRs()
					userDefinedNetworkIPv6Subnet1, userDefinedNetworkIPv6Subnet2 := primaryLayer3IPv6CIDRs()
					By("creating the initial network with one CIDR")
					netConfig := &networkAttachmentConfigParams{
						name:      randomNetworkMetaName(),
						namespace: f.Namespace.Name,
						topology:  "layer3",
						cidr: joinStrings(
							userDefinedNetworkIPv4Subnet1, userDefinedNetworkIPv6Subnet1,
						),
						role: "primary",
					}
					netConfig.cidr = filterCIDRsAndJoin(f.ClientSet, netConfig.cidr)
					Expect(createNetworkFn(netConfig)).To(Succeed())

					nad, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Get(context.TODO(), netConfig.name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					networkName := nad.Annotations["k8s.ovn.org/network-name"]

					By("getting the node subnet assignment")
					nodeList, err = e2enode.GetReadySchedulableNodes(context.TODO(), cs)
					framework.ExpectNoError(err)

					oldv4, oldv6, err := getNodeSubnetAssignments(cs, networkName)
					Expect(err).NotTo(HaveOccurred())
					Expect(len(oldv4) == 0 || len(oldv4) < len(nodeList.Items)).To(BeTrue(), "expect some nodes do not have subnet assigned")
					Expect(len(oldv6) == 0 || len(oldv6) < len(nodeList.Items)).To(BeTrue(), "expect some nodes do not have subnet assigned")

					By("Add subnets to the network")
					cirds := []string{
						userDefinedNetworkIPv4Subnet1, userDefinedNetworkIPv6Subnet1,
						userDefinedNetworkIPv4Subnet2, userDefinedNetworkIPv6Subnet2,
					}
					netConfig.cidr = joinStrings(cirds...)
					Expect(updateNetworkFn(netConfig)).To(Succeed())

					By("wait all nodes to have subnet assigned")
					newv4, newv6 := map[string]string{}, map[string]string{}
					Eventually(func() error {
						newv4, newv6, err = getNodeSubnetAssignments(cs, networkName)
						Expect(err).NotTo(HaveOccurred())
						if (len(newv4) == 0 || len(newv4) == len(nodeList.Items)) &&
							(len(newv6) == 0 || len(newv6) == len(nodeList.Items)) {
							return nil
						}
						return fmt.Errorf("expect all nodes have subnet")
					}, 60*time.Second, 3*time.Second).Should(Succeed())

					By("Check node subnet assignment after adding subnets")
					for nodeName, subnet := range oldv4 {
						Expect(newv4[nodeName]).To(Equal(subnet), "node %s IPv4 subnet changed from %s to %s", nodeName, subnet, newv4[nodeName])
					}
					for nodeName, subnet := range oldv6 {
						Expect(newv6[nodeName]).To(Equal(subnet), "node %s IPv6 subnet changed from %s to %s", nodeName, subnet, newv6[nodeName])
					}
				})

				It("add bad subnet should not cause change on existing NAD", func() {
					By("creating the intial network with one CIDR")
					netConfig := &networkAttachmentConfigParams{
						name:      randomNetworkMetaName(),
						namespace: f.Namespace.Name,
						topology:  "layer3",
						cidr: joinStrings(
							userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet,
						),
						role: "primary",
					}
					netConfig.cidr = filterCIDRsAndJoin(f.ClientSet, netConfig.cidr)
					Expect(createNetworkFn(netConfig)).To(Succeed())

					nad, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Get(context.TODO(), netConfig.name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					By("Add Join subnets to the network")
					cirds := []string{
						userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet,
						config.Gateway.V4JoinSubnet, config.Gateway.V6JoinSubnet,
					}
					netConfig.cidr = joinStrings(cirds...)
					Expect(updateNetworkFn(netConfig)).To(Succeed())

					By("check status NetworkCreated is changed to false")
					client := getDynamicClient(netConfig)
					Eventually(checkStatusFunc(client, netConfig.name, "NetworkCreated", metav1.ConditionFalse),
						30*time.Second, 2*time.Second).Should(Succeed())

					By("Check NAD is intacted after adding bad subnet")
					curNad, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Get(context.TODO(), netConfig.name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					Expect(curNad.Spec.Config).To(Equal(nad.Spec.Config))
				})

			},
			Entry("UserDefinedNetwork",
				func(c *networkAttachmentConfigParams) error {
					udnManifest := generateUserDefinedNetworkManifest(c, f.ClientSet)
					cleanup, err := createManifest(c.namespace, udnManifest)
					DeferCleanup(cleanup)
					Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, c.namespace, c.name), 10*time.Second, time.Second).Should(Succeed())
					return err
				},
				func(c *networkAttachmentConfigParams) error {
					udnManifest := generateUserDefinedNetworkManifest(c, f.ClientSet)
					cleanup, err := applyManifest(c.namespace, udnManifest)
					DeferCleanup(cleanup)
					return err
				},
				func(c *networkAttachmentConfigParams) dynamic.ResourceInterface {
					return f.DynamicClient.Resource(udnGVR).Namespace(c.namespace)
				},
			),
			Entry("ClusterUserDefinedNetwork",
				func(c *networkAttachmentConfigParams) error {
					cudnManifest := generateClusterUserDefinedNetworkManifest(c, f.ClientSet)
					cleanup, err := createManifest("", cudnManifest)
					DeferCleanup(func() {
						cleanup()
						By(fmt.Sprintf("delete pods in %s namespace to unblock CUDN CR & associate NAD deletion", c.namespace))
						Expect(cs.CoreV1().Pods(c.namespace).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
						_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", c.name, "--wait", fmt.Sprintf("--timeout=%ds", 120))
						Expect(err).NotTo(HaveOccurred())
					})
					Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, c.name), 10*time.Second, time.Second).Should(Succeed())
					return err
				},
				func(c *networkAttachmentConfigParams) error {
					cudnManifest := generateClusterUserDefinedNetworkManifest(c, f.ClientSet)
					cleanup, err := applyManifest("", cudnManifest)
					DeferCleanup(func() {
						cleanup()
					})
					return err
				},
				func(c *networkAttachmentConfigParams) dynamic.ResourceInterface {
					return f.DynamicClient.Resource(clusterUDNGVR)
				},
			),
		)
	})

	Context("UserDefinedNetwork CRD Controller", func() {
		const (
			testUdnName                = "test-net"
			userDefinedNetworkResource = "userdefinednetwork"
		)

		var (
			defaultNetNamespace *v1.Namespace
		)

		Context("for primary UDN without required namespace label", func() {
			BeforeEach(func() {
				// default cluster network namespace, for use when doing negative testing for UDNs/NADs
				defaultNetNamespace = &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: f.Namespace.Name + "-default",
					},
				}
				f.AddNamespacesToDelete(defaultNetNamespace)
				_, err := cs.CoreV1().Namespaces().Create(context.Background(), defaultNetNamespace, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("create tests UserDefinedNetwork")
				cleanup, err := createManifest(defaultNetNamespace.Name, newPrimaryUserDefinedNetworkManifest(cs, testUdnName))
				DeferCleanup(cleanup)
				Expect(err).NotTo(HaveOccurred())
				Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, defaultNetNamespace.Name, testUdnName), 5*time.Second).Should(Not(Succeed()))
			})

			It("should be able to create pod and it will attach to the cluster default network", func() {
				podConfig := *podConfig("some-pod")
				podConfig.namespace = defaultNetNamespace.Name
				pod := runUDNPod(cs, defaultNetNamespace.Name, podConfig, nil)
				ovnPodAnnotation, err := unmarshalPodAnnotationAllNetworks(pod.Annotations)
				Expect(err).NotTo(HaveOccurred())
				Expect(len(ovnPodAnnotation)).To(BeNumerically("==", 1))
				Expect(ovnPodAnnotation).To(HaveKey("default"))
			})

			It("should not be able to update the namespace and add the UDN label", func() {
				defaultNetNamespace.Labels = map[string]string{
					RequiredUDNNamespaceLabel: "",
				}
				_, err := cs.CoreV1().Namespaces().Update(context.TODO(), defaultNetNamespace, metav1.UpdateOptions{})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("The 'k8s.ovn.org/primary-user-defined-network' label cannot be added/removed after the namespace was created"))
			})

			It("should not be able to update the namespace and remove the UDN label", func() {
				udnNamespace, err := cs.CoreV1().Namespaces().Get(context.TODO(), f.Namespace.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				udnNamespace.Labels = map[string]string{}
				_, err = cs.CoreV1().Namespaces().Update(context.TODO(), udnNamespace, metav1.UpdateOptions{})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("The 'k8s.ovn.org/primary-user-defined-network' label cannot be added/removed after the namespace was created"))
			})

		})

		Context("for L2 secondary network", func() {
			BeforeEach(func() {
				// default cluster network namespace, for use when only testing secondary UDNs/NADs
				defaultNetNamespace = &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: f.Namespace.Name + "-default",
					},
				}
				f.AddNamespacesToDelete(defaultNetNamespace)
				_, err := cs.CoreV1().Namespaces().Create(context.Background(), defaultNetNamespace, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("create tests UserDefinedNetwork")
				cleanup, err := createManifest(defaultNetNamespace.Name, newL2SecondaryUDNManifest(testUdnName))
				DeferCleanup(cleanup)
				Expect(err).NotTo(HaveOccurred())
				Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, defaultNetNamespace.Name, testUdnName), 5*time.Second, time.Second).Should(Succeed())
			})

			It("should create NetworkAttachmentDefinition according to spec", func() {
				udnUidRaw, err := e2ekubectl.RunKubectl(defaultNetNamespace.Name, "get", userDefinedNetworkResource, testUdnName, "-o", "jsonpath='{.metadata.uid}'")
				Expect(err).NotTo(HaveOccurred(), "should get the UserDefinedNetwork UID")
				testUdnUID := strings.Trim(udnUidRaw, "'")

				By("verify a NetworkAttachmentDefinition is created according to spec")
				assertL2SecondaryNetAttachDefManifest(nadClient, defaultNetNamespace.Name, testUdnName, testUdnUID)
			})

			It("should delete NetworkAttachmentDefinition when UserDefinedNetwork is deleted", func() {
				By("delete UserDefinedNetwork")
				_, err := e2ekubectl.RunKubectl(defaultNetNamespace.Name, "delete", userDefinedNetworkResource, testUdnName)
				Expect(err).NotTo(HaveOccurred())

				By("verify a NetworkAttachmentDefinition has been deleted")
				Eventually(func() bool {
					_, err := nadClient.NetworkAttachmentDefinitions(defaultNetNamespace.Name).Get(context.Background(), testUdnName, metav1.GetOptions{})
					return err != nil && kerrors.IsNotFound(err)
				}, time.Second*3, time.Second*1).Should(BeTrue(),
					"NetworkAttachmentDefinition should be deleted following UserDefinedNetwork deletion")
			})

			Context("pod connected to UserDefinedNetwork", func() {
				const testPodName = "test-pod-udn"

				var (
					udnInUseDeleteTimeout = 65 * time.Second
					deleteNetworkTimeout  = 5 * time.Second
					deleteNetworkInterval = 1 * time.Second
				)

				BeforeEach(func() {
					By("create pod")
					networkAttachments := []nadapi.NetworkSelectionElement{
						{Name: testUdnName, Namespace: defaultNetNamespace.Name},
					}
					cfg := podConfig(testPodName, withNetworkAttachment(networkAttachments))
					cfg.namespace = defaultNetNamespace.Name
					runUDNPod(cs, defaultNetNamespace.Name, *cfg, nil)
				})

				It("cannot be deleted when being used", func() {
					By("verify UserDefinedNetwork cannot be deleted")
					cmd := e2ekubectl.NewKubectlCommand(defaultNetNamespace.Name, "delete", userDefinedNetworkResource, testUdnName)
					cmd.WithTimeout(time.NewTimer(deleteNetworkTimeout).C)
					_, err := cmd.Exec()
					Expect(err).To(HaveOccurred(),
						"should fail to delete UserDefinedNetwork when used")

					By("verify UserDefinedNetwork associated NetworkAttachmentDefinition cannot be deleted")
					Eventually(func() error {
						ctx, cancel := context.WithTimeout(context.Background(), deleteNetworkTimeout)
						defer cancel()
						_ = nadClient.NetworkAttachmentDefinitions(defaultNetNamespace.Name).Delete(ctx, testUdnName, metav1.DeleteOptions{})
						_, err := nadClient.NetworkAttachmentDefinitions(defaultNetNamespace.Name).Get(ctx, testUdnName, metav1.GetOptions{})
						return err
					}).ShouldNot(HaveOccurred(),
						"should fail to delete UserDefinedNetwork associated NetworkAttachmentDefinition when used")

					By("verify UserDefinedNetwork status reports consuming pod")
					err = validateUDNStatusReportsConsumers(f.DynamicClient, defaultNetNamespace.Name, testUdnName, testPodName)
					Expect(err).ToNot(HaveOccurred())

					By("delete test pod")
					err = cs.CoreV1().Pods(defaultNetNamespace.Name).Delete(context.Background(), testPodName, metav1.DeleteOptions{})
					Expect(err).ToNot(HaveOccurred())

					By("verify UserDefinedNetwork has been deleted")
					Eventually(func() error {
						_, err := e2ekubectl.RunKubectl(defaultNetNamespace.Name, "get", userDefinedNetworkResource, testUdnName)
						return err
					}, udnInUseDeleteTimeout, deleteNetworkInterval).Should(HaveOccurred(),
						"UserDefinedNetwork should be deleted following test pod deletion")

					By("verify UserDefinedNetwork associated NetworkAttachmentDefinition has been deleted")
					Eventually(func() bool {
						_, err := nadClient.NetworkAttachmentDefinitions(defaultNetNamespace.Name).Get(context.Background(), testUdnName, metav1.GetOptions{})
						return err != nil && kerrors.IsNotFound(err)
					}, deleteNetworkTimeout, deleteNetworkInterval).Should(BeTrue(),
						"NetworkAttachmentDefinition should be deleted following UserDefinedNetwork deletion")
				})
			})
		})

		It("should correctly report subsystem error on node subnet allocation", func() {
			cs = f.ClientSet

			nodes, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
			framework.ExpectNoError(err)

			By("create tests UserDefinedNetwork")
			// create network that only has 2 node subnets (/24 cluster subnet has only 2 /25 node subnets)
			udnManifest := `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: ` + testUdnName + `
spec:
  topology: "Layer3"
  layer3:
    role: Secondary
    subnets: 
      - cidr: "10.10.100.0/24"
        hostSubnet: 25
`
			cleanup, err := createManifest(f.Namespace.Name, udnManifest)
			defer cleanup()
			Expect(err).NotTo(HaveOccurred())
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, f.Namespace.Name, testUdnName), 5*time.Second, time.Second).Should(Succeed())

			conditionsJSON, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", "userdefinednetwork", testUdnName, "-o", "jsonpath={.status.conditions}")
			Expect(err).NotTo(HaveOccurred())
			var actualConditions []metav1.Condition
			Expect(json.Unmarshal([]byte(conditionsJSON), &actualConditions)).To(Succeed())

			netAllocationCondition := "NetworkAllocationSucceeded"

			if len(nodes.Items) <= 2 {
				By("when cluster has <= 2 nodes, no error is expected")
				found := false
				for _, condition := range actualConditions {
					if condition.Type == netAllocationCondition && condition.Status == metav1.ConditionTrue {
						found = true
					}
				}
				Expect(found).To(BeTrue(), "NetworkAllocationSucceeded condition should be True when cluster has <= 2 nodes")
			} else {
				found := false
				expectedStatus := metav1.ConditionFalse
				if isDynamicUDNEnabled() {
					By("when cluster has > 2 nodes with dynamic UDN, no error is expected")
					expectedStatus = metav1.ConditionTrue
				} else {
					By("when cluster has > 2 nodes, error is expected")
				}

				for _, condition := range actualConditions {
					if condition.Type == netAllocationCondition && condition.Status == expectedStatus {
						found = true
						break
					}
				}

				Expect(found).To(BeTrue(), fmt.Sprintf("NetworkAllocationSucceeded condition should be %s when cluster has > 2 nodes", expectedStatus))

				events, err := cs.CoreV1().Events(f.Namespace.Name).List(context.Background(), metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				found = false
				for _, event := range events.Items {
					if event.Reason == "NetworkAllocationFailed" && event.LastTimestamp.After(time.Now().Add(-30*time.Second)) &&
						strings.Contains(event.Message, "error allocating network") {
						found = true
						break
					}
				}
				if isDynamicUDNEnabled() {
					Expect(found).To(BeFalse(), "should not have found an event for failed node allocation")
				} else {
					Expect(found).To(BeTrue(), "should have found an event for failed node allocation")
				}
			}
		})
	})

	It("when primary network exist, UserDefinedNetwork status should report not-ready", func() {
		const (
			primaryNadName = "cluster-primary-net"
			primaryUdnName = "primary-net"
		)

		By("create primary network NetworkAttachmentDefinition")
		primaryNetNad := generateNAD(newNetworkAttachmentConfig(networkAttachmentConfigParams{
			role:        "primary",
			topology:    "layer3",
			name:        primaryNadName,
			networkName: primaryNadName,
			cidr:        primaryLayer3MultiCIDRs(),
		}), f.ClientSet)
		_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(context.Background(), primaryNetNad, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create primary network UserDefinedNetwork")
		cleanup, err := createManifest(f.Namespace.Name, newPrimaryUserDefinedNetworkManifest(cs, primaryUdnName))
		DeferCleanup(cleanup)
		Expect(err).NotTo(HaveOccurred())

		expectedMessage := fmt.Sprintf("primary network already exist in namespace %q: %q", f.Namespace.Name, primaryNadName)
		Eventually(func(g Gomega) []metav1.Condition {
			conditionsJSON, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", "userdefinednetwork", primaryUdnName, "-o", "jsonpath={.status.conditions}")
			g.Expect(err).NotTo(HaveOccurred())
			var actualConditions []metav1.Condition
			g.Expect(json.Unmarshal([]byte(conditionsJSON), &actualConditions)).To(Succeed())
			return normalizeConditions(actualConditions)
		}, 5*time.Second, 1*time.Second).Should(ConsistOf(metav1.Condition{
			Type:    "NetworkCreated",
			Status:  metav1.ConditionFalse,
			Reason:  "SyncError",
			Message: expectedMessage,
		}))
	})

	Context("ClusterUserDefinedNetwork CRD Controller", func() {
		const clusterUserDefinedNetworkResource = "clusteruserdefinednetwork"

		var testTenantNamespaces []string
		var defaultNetNamespace *v1.Namespace

		BeforeEach(func() {
			testTenantNamespaces = []string{
				f.Namespace.Name + "blue",
				f.Namespace.Name + "red",
			}

			By("Creating test tenants namespaces")
			for _, nsName := range testTenantNamespaces {
				_, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   nsName,
						Labels: map[string]string{RequiredUDNNamespaceLabel: ""},
					}}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() error {
					err := cs.CoreV1().Namespaces().Delete(context.Background(), nsName, metav1.DeleteOptions{})
					return err
				})
			}
			// default cluster network namespace, for use when only testing secondary UDNs/NADs
			defaultNetNamespace = &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: f.Namespace.Name + "-default",
				},
			}
			f.AddNamespacesToDelete(defaultNetNamespace)
			_, err := cs.CoreV1().Namespaces().Create(context.Background(), defaultNetNamespace, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			testTenantNamespaces = append(testTenantNamespaces, defaultNetNamespace.Name)

		})

		var testClusterUdnName string

		BeforeEach(func() {
			testClusterUdnName = randomNetworkMetaName()
			By("create test CR")
			cleanup, err := createManifest("", newClusterUDNManifest(testClusterUdnName, testTenantNamespaces...))
			DeferCleanup(func() error {
				cleanup()
				_, _ = e2ekubectl.RunKubectl("", "delete", clusterUserDefinedNetworkResource, testClusterUdnName)
				Eventually(func() error {
					_, err := e2ekubectl.RunKubectl("", "get", clusterUserDefinedNetworkResource, testClusterUdnName)
					return err
				}, 1*time.Minute, 3*time.Second).Should(MatchError(ContainSubstring(fmt.Sprintf("clusteruserdefinednetworks.k8s.ovn.org %q not found", testClusterUdnName))))
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, testClusterUdnName), 5*time.Second, time.Second).Should(Succeed())
		})

		It("should create NAD according to spec in each target namespace and report active namespaces", func() {
			Eventually(
				validateClusterUDNStatusReportsActiveNamespacesFunc(f.DynamicClient, testClusterUdnName, testTenantNamespaces...),
				1*time.Minute, 3*time.Second).Should(Succeed())

			udnUidRaw, err := e2ekubectl.RunKubectl("", "get", clusterUserDefinedNetworkResource, testClusterUdnName, "-o", "jsonpath='{.metadata.uid}'")
			Expect(err).NotTo(HaveOccurred(), "should get the ClsuterUserDefinedNetwork UID")
			testUdnUID := strings.Trim(udnUidRaw, "'")

			By("verify a NetworkAttachmentDefinition is created according to spec")
			for _, testNsName := range testTenantNamespaces {
				assertClusterNADManifest(nadClient, testNsName, testClusterUdnName, testUdnUID)
			}
		})

		It("when CR is deleted, should delete all managed NAD in each target namespace", func() {
			By("delete test CR")
			_, err := e2ekubectl.RunKubectl("", "delete", clusterUserDefinedNetworkResource, testClusterUdnName)
			Expect(err).NotTo(HaveOccurred())

			for _, nsName := range testTenantNamespaces {
				By(fmt.Sprintf("verify a NAD has been deleted from namesapce %q", nsName))
				Eventually(func() bool {
					_, err := nadClient.NetworkAttachmentDefinitions(nsName).Get(context.Background(), testClusterUdnName, metav1.GetOptions{})
					return err != nil && kerrors.IsNotFound(err)
				}, time.Second*3, time.Second*1).Should(BeTrue(),
					"NADs in target namespaces should be deleted following ClusterUserDefinedNetwork deletion")
			}
		})

		It("should delete NAD when target namespace is terminating", func() {
			testTerminatingNs := f.Namespace.Name + "terminating"

			By("add new target namespace to CR namespace-selector")
			patch := fmt.Sprintf(`[{"op": "add", "path": "./spec/namespaceSelector/matchExpressions/0/values/-", "value": "%s"}]`, testTerminatingNs)
			_, err := e2ekubectl.RunKubectl("", "patch", clusterUserDefinedNetworkResource, testClusterUdnName, "--type=json", "-p="+patch)
			Expect(err).NotTo(HaveOccurred())

			By("create the target namespace")
			_, err = cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   testTerminatingNs,
					Labels: map[string]string{RequiredUDNNamespaceLabel: ""},
				}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("verify NAD is created in the namespace")
			Eventually(func() error {
				_, err := nadClient.NetworkAttachmentDefinitions(testTerminatingNs).Get(context.Background(), testClusterUdnName, metav1.GetOptions{})
				return err
			}, time.Second*15, time.Second*1).Should(Succeed(), "NAD should be created in target namespace")

			By("delete the namespace to trigger termination")
			err = cs.CoreV1().Namespaces().Delete(context.Background(), testTerminatingNs, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("verify NAD is deleted from the terminating namespace")
			Eventually(func() bool {
				_, err := nadClient.NetworkAttachmentDefinitions(testTerminatingNs).Get(context.Background(), testClusterUdnName, metav1.GetOptions{})
				return err != nil && kerrors.IsNotFound(err)
			}, time.Second*30, time.Second*1).Should(BeTrue(),
				"NAD should be deleted when namespace is terminating")
		})

		It("should create NAD in new created namespaces that apply to namespace-selector", func() {
			testNewNs := f.Namespace.Name + "green"

			By("add new target namespace to CR namespace-selector")
			patch := fmt.Sprintf(`[{"op": "add", "path": "./spec/namespaceSelector/matchExpressions/0/values/-", "value": "%s"}]`, testNewNs)
			_, err := e2ekubectl.RunKubectl("", "patch", clusterUserDefinedNetworkResource, testClusterUdnName, "--type=json", "-p="+patch)
			Expect(err).NotTo(HaveOccurred())
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, testClusterUdnName), 5*time.Second, time.Second).Should(Succeed())
			err = validateClusterUDNStatusReportsActiveNamespacesFunc(f.DynamicClient, testClusterUdnName, testTenantNamespaces...)()
			Expect(err).NotTo(HaveOccurred())

			By("create the new target namespace")
			_, err = cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   testNewNs,
					Labels: map[string]string{RequiredUDNNamespaceLabel: ""},
				}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() error {
				err := cs.CoreV1().Namespaces().Delete(context.Background(), testNewNs, metav1.DeleteOptions{})
				return err
			})

			expectedActiveNamespaces := append(testTenantNamespaces, testNewNs)
			Eventually(
				validateClusterUDNStatusReportsActiveNamespacesFunc(f.DynamicClient, testClusterUdnName, expectedActiveNamespaces...),
				1*time.Minute, 3*time.Second).Should(Succeed())

			udnUidRaw, err := e2ekubectl.RunKubectl("", "get", clusterUserDefinedNetworkResource, testClusterUdnName, "-o", "jsonpath='{.metadata.uid}'")
			Expect(err).NotTo(HaveOccurred(), "should get the ClusterUserDefinedNetwork UID")
			testUdnUID := strings.Trim(udnUidRaw, "'")

			By("verify a NAD exist in new namespace according to spec")
			assertClusterNADManifest(nadClient, testNewNs, testClusterUdnName, testUdnUID)
		})

		When("namespace-selector is mutated", func() {
			It("should create NAD in namespaces that apply to mutated namespace-selector", func() {
				testNewNs := f.Namespace.Name + "green"

				By("create new namespace")
				_, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   testNewNs,
						Labels: map[string]string{RequiredUDNNamespaceLabel: ""},
					}}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() error {
					err := cs.CoreV1().Namespaces().Delete(context.Background(), testNewNs, metav1.DeleteOptions{})
					return err
				})

				By("add new namespace to CR namespace-selector")
				patch := fmt.Sprintf(`[{"op": "add", "path": "./spec/namespaceSelector/matchExpressions/0/values/-", "value": "%s"}]`, testNewNs)
				_, err = e2ekubectl.RunKubectl("", "patch", clusterUserDefinedNetworkResource, testClusterUdnName, "--type=json", "-p="+patch)
				Expect(err).NotTo(HaveOccurred())

				By("verify status reports the new added namespace as active")
				expectedActiveNs := append(testTenantNamespaces, testNewNs)
				Eventually(
					validateClusterUDNStatusReportsActiveNamespacesFunc(f.DynamicClient, testClusterUdnName, expectedActiveNs...),
					1*time.Minute, 3*time.Second).Should(Succeed())
				By("verify a NAD is created in new target namespace according to spec")
				udnUidRaw, err := e2ekubectl.RunKubectl("", "get", clusterUserDefinedNetworkResource, testClusterUdnName, "-o", "jsonpath='{.metadata.uid}'")
				Expect(err).NotTo(HaveOccurred(), "should get the ClusterUserDefinedNetwork UID")
				testUdnUID := strings.Trim(udnUidRaw, "'")
				assertClusterNADManifest(nadClient, testNewNs, testClusterUdnName, testUdnUID)
			})

			It("should delete managed NAD in namespaces that no longer apply to namespace-selector", func() {
				By("remove one active namespace from CR namespace-selector")
				activeTenantNs := testTenantNamespaces[1]
				patch := fmt.Sprintf(`[{"op": "replace", "path": "./spec/namespaceSelector/matchExpressions/0/values", "value": [%q]}]`, activeTenantNs)
				_, err := e2ekubectl.RunKubectl("", "patch", clusterUserDefinedNetworkResource, testClusterUdnName, "--type=json", "-p="+patch)
				Expect(err).NotTo(HaveOccurred())

				By("verify status reports remained target namespaces only as active")
				expectedActiveNs := []string{activeTenantNs}
				Eventually(
					validateClusterUDNStatusReportsActiveNamespacesFunc(f.DynamicClient, testClusterUdnName, expectedActiveNs...),
					1*time.Minute, 3*time.Second).Should(Succeed())

				removedTenantNs := testTenantNamespaces[0]
				By("verify managed NAD not exist in removed target namespace")
				Eventually(func() bool {
					_, err := nadClient.NetworkAttachmentDefinitions(removedTenantNs).Get(context.Background(), testClusterUdnName, metav1.GetOptions{})
					return err != nil && kerrors.IsNotFound(err)
				}, time.Second*300, time.Second*1).Should(BeTrue(),
					"NAD in target namespaces should be deleted following CR namespace-selector mutation")
			})
		})

		Context("pod connected to ClusterUserDefinedNetwork", func() {
			const testPodName = "test-pod-cluster-udn"

			var (
				udnInUseDeleteTimeout = 65 * time.Second
				deleteNetworkTimeout  = 5 * time.Second
				deleteNetworkInterval = 1 * time.Second

				inUseNetTestTenantNamespace string
			)

			BeforeEach(func() {
				inUseNetTestTenantNamespace = defaultNetNamespace.Name

				By("create pod in one of the test tenant namespaces")
				networkAttachments := []nadapi.NetworkSelectionElement{
					{Name: testClusterUdnName, Namespace: inUseNetTestTenantNamespace},
				}
				cfg := podConfig(testPodName, withNetworkAttachment(networkAttachments))
				cfg.namespace = inUseNetTestTenantNamespace
				runUDNPod(cs, inUseNetTestTenantNamespace, *cfg, nil)
			})

			It("CR & managed NADs cannot be deleted when being used", func() {
				By("verify CR cannot be deleted")
				cmd := e2ekubectl.NewKubectlCommand("", "delete", clusterUserDefinedNetworkResource, testClusterUdnName)
				cmd.WithTimeout(time.NewTimer(deleteNetworkTimeout).C)
				_, err := cmd.Exec()
				Expect(err).To(HaveOccurred(), "should fail to delete ClusterUserDefinedNetwork when used")

				By("verify CR associate NAD cannot be deleted")
				Eventually(func() error {
					ctx, cancel := context.WithTimeout(context.Background(), deleteNetworkTimeout)
					defer cancel()
					_ = nadClient.NetworkAttachmentDefinitions(inUseNetTestTenantNamespace).Delete(ctx, testClusterUdnName, metav1.DeleteOptions{})
					_, err := nadClient.NetworkAttachmentDefinitions(inUseNetTestTenantNamespace).Get(ctx, testClusterUdnName, metav1.GetOptions{})
					return err
				}).ShouldNot(HaveOccurred(),
					"should fail to delete UserDefinedNetwork associated NetworkAttachmentDefinition when used")

				By("verify CR status reports consuming pod")
				err = validateClusterUDNStatusReportConsumers(f.DynamicClient, testClusterUdnName, inUseNetTestTenantNamespace, testPodName)
				Expect(err).NotTo(HaveOccurred())

				By("delete test pod")
				err = cs.CoreV1().Pods(inUseNetTestTenantNamespace).Delete(context.Background(), testPodName, metav1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("verify CR is gone")
				Eventually(func() error {
					_, err := e2ekubectl.RunKubectl("", "get", clusterUserDefinedNetworkResource, testClusterUdnName)
					return err
				}, udnInUseDeleteTimeout, deleteNetworkInterval).Should(HaveOccurred(),
					"ClusterUserDefinedNetwork should be deleted following test pod deletion")

				By("verify CR associate NADs are gone")
				for _, nsName := range testTenantNamespaces {
					Eventually(func() bool {
						_, err := nadClient.NetworkAttachmentDefinitions(nsName).Get(context.Background(), testClusterUdnName, metav1.GetOptions{})
						return err != nil && kerrors.IsNotFound(err)
					}, deleteNetworkTimeout, deleteNetworkInterval).Should(BeTrue(),
						"NADs in target namespaces should be deleted following ClusterUserDefinedNetwork deletion")
				}
			})
		})

		It("should correctly report subsystem error on node subnet allocation", func() {
			nodes, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
			framework.ExpectNoError(err)

			By("create test ClusterUserDefinedNetwork")
			// create network that only has 2 node subnets (/24 cluster subnet has only 2 /25 node subnets)
			cudnName := randomNetworkMetaName()
			cudnManifest := `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ` + cudnName + `
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [` + strings.Join(testTenantNamespaces, ",") + `]
  network:
    topology: "Layer3"
    layer3:
      role: Secondary
      subnets:
        - cidr: "10.10.100.0/24"
          hostSubnet: 25
`
			cleanup, err := createManifest("", cudnManifest)
			DeferCleanup(func() {
				cleanup()
				_, _ = e2ekubectl.RunKubectl("", "delete", clusterUserDefinedNetworkResource, cudnName)
			})
			Expect(err).NotTo(HaveOccurred())
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName), 5*time.Second, time.Second).Should(Succeed())

			conditionsJSON, err := e2ekubectl.RunKubectl("", "get", clusterUserDefinedNetworkResource, cudnName, "-o", "jsonpath={.status.conditions}")
			Expect(err).NotTo(HaveOccurred())
			var actualConditions []metav1.Condition
			Expect(json.Unmarshal([]byte(conditionsJSON), &actualConditions)).To(Succeed())

			netAllocationCondition := "NetworkAllocationSucceeded"

			if len(nodes.Items) <= 2 {
				By("when cluster has <= 2 nodes, no error is expected")
				found := false
				for _, condition := range actualConditions {
					if condition.Type == netAllocationCondition && condition.Status == metav1.ConditionTrue {
						found = true
					}
				}
				Expect(found).To(BeTrue(), "NetworkAllocationSucceeded condition should be True when cluster has <= 2 nodes")
			} else {
				found := false
				expectedStatus := metav1.ConditionFalse
				if isDynamicUDNEnabled() {
					By("when cluster has > 2 nodes with dynamic UDN, no error is expected")
					expectedStatus = metav1.ConditionTrue
				} else {
					By("when cluster has > 2 nodes, error is expected")
				}

				for _, condition := range actualConditions {
					if condition.Type == netAllocationCondition && condition.Status == expectedStatus {
						found = true
						break
					}
				}
				Expect(found).To(BeTrue(), fmt.Sprintf("NetworkAllocationSucceeded condition should be %s when cluster has > 2 nodes", expectedStatus))
				events, err := cs.CoreV1().Events("").List(context.Background(), metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				found = false
				for _, event := range events.Items {
					if event.Reason == "NetworkAllocationFailed" && event.LastTimestamp.After(time.Now().Add(-30*time.Second)) &&
						strings.Contains(event.Message, "error allocating network") {
						found = true
						break
					}
				}
				if isDynamicUDNEnabled() {
					Expect(found).To(BeFalse(), "should not have found an event for failed node allocation")
				} else {
					Expect(found).To(BeTrue(), "should have found an event for failed node allocation")
				}
			}
		})
	})

	It("when primary network exist, ClusterUserDefinedNetwork status should report not-ready", func() {
		testTenantNamespaces := []string{
			f.Namespace.Name + "blue",
			f.Namespace.Name + "red",
		}
		By("Creating test tenants namespaces")
		for _, nsName := range testTenantNamespaces {
			_, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   nsName,
					Labels: map[string]string{RequiredUDNNamespaceLabel: ""},
				}}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() error {
				err := cs.CoreV1().Namespaces().Delete(context.Background(), nsName, metav1.DeleteOptions{})
				return err
			})
		}

		By("create primary network NAD in one of the tenant namespaces")
		const primaryNadName = "some-primary-net"
		primaryNetTenantNs := testTenantNamespaces[0]
		primaryNetNad := generateNAD(newNetworkAttachmentConfig(networkAttachmentConfigParams{
			role:        "primary",
			topology:    "layer3",
			name:        primaryNadName,
			networkName: primaryNadName,
			cidr:        primaryLayer3MultiCIDRs(),
		}), f.ClientSet)
		_, err := nadClient.NetworkAttachmentDefinitions(primaryNetTenantNs).Create(context.Background(), primaryNetNad, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("create primary Cluster UDN CR")
		cudnName := randomNetworkMetaName()
		cleanup, err := createManifest(f.Namespace.Name, newPrimaryClusterUDNManifest(cs, cudnName, testTenantNamespaces...))
		DeferCleanup(func() {
			cleanup()
			_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", fmt.Sprintf("--timeout=%ds", 60))
			Expect(err).NotTo(HaveOccurred())
		})

		expectedMessage := fmt.Sprintf("primary network already exist in namespace %q: %q", primaryNetTenantNs, primaryNadName)
		Eventually(func(g Gomega) []metav1.Condition {
			conditionsJSON, err := e2ekubectl.RunKubectl(f.Namespace.Name, "get", "clusteruserdefinednetwork", cudnName, "-o", "jsonpath={.status.conditions}")
			g.Expect(err).NotTo(HaveOccurred())
			var actualConditions []metav1.Condition
			g.Expect(json.Unmarshal([]byte(conditionsJSON), &actualConditions)).To(Succeed())
			return normalizeConditions(actualConditions)
		}, 5*time.Second, 1*time.Second).Should(ContainElement(metav1.Condition{
			Type:    "NetworkCreated",
			Status:  metav1.ConditionFalse,
			Reason:  "NetworkAttachmentDefinitionSyncError",
			Message: expectedMessage,
		}))
	})

	Context("pod2Egress on a user defined primary network", func() {
		const (
			externalContainerName = "ovn-k-egress-test-helper"
		)
		var (
			providerCtx       infraapi.Context
			externalContainer infraapi.ExternalContainer
		)
		BeforeEach(func() {
			providerCtx = infraprovider.Get().NewTestContext()
			providerPrimaryNetwork, err := infraprovider.Get().PrimaryNetwork()
			framework.ExpectNoError(err, "provider primary network must be available")
			externalContainerPort := infraprovider.Get().GetExternalContainerPort()
			externalContainerSpec := infraapi.ExternalContainer{
				Name:    externalContainerName,
				Image:   images.AgnHost(),
				Network: providerPrimaryNetwork,
				CmdArgs: httpServerContainerCmd(uint16(externalContainerPort)),
				ExtPort: externalContainerPort,
			}
			externalContainer, err = providerCtx.CreateExternalContainer(externalContainerSpec)
			framework.ExpectNoError(err, "external container must succeed")
		})

		DescribeTableSubtree("created using",
			func(createNetworkFn func(c *networkAttachmentConfigParams) error) {

				DescribeTable(
					"can be accessed to from the pods running in the Kubernetes cluster",
					func(netConfigParams *networkAttachmentConfigParams, clientPodConfig podConfiguration) {
						clientPodConfig.namespace = f.Namespace.Name

						By("creating the network")
						netConfigParams.namespace = f.Namespace.Name
						Expect(createNetworkFn(netConfigParams)).To(Succeed())

						By("instantiating the client pod")
						clientPod, err := cs.CoreV1().Pods(clientPodConfig.namespace).Create(
							context.Background(),
							generatePodSpec(clientPodConfig),
							metav1.CreateOptions{},
						)
						Expect(err).NotTo(HaveOccurred())
						Expect(clientPod).NotTo(BeNil())

						By("asserting the client pod reaches the `Ready` state")
						var updatedPod *v1.Pod
						Eventually(func() v1.PodPhase {
							updatedPod, err = cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), clientPod.GetName(), metav1.GetOptions{})
							if err != nil {
								return v1.PodFailed
							}
							return updatedPod.Status.Phase
						}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
						framework.Logf("Client pod was created on node %s", updatedPod.Spec.NodeName)

						By("asserting UDN pod is connected to UDN network")
						podAnno, err := unmarshalPodAnnotation(updatedPod.Annotations, f.Namespace.Name+"/"+netConfigParams.name)
						Expect(err).NotTo(HaveOccurred())
						framework.Logf("Client pod's annotation for network %s is %v", netConfigParams.name, podAnno)

						Expect(podAnno.Routes).To(HaveLen(expectedNumberOfRoutes(cs, *netConfigParams)))

						assertClientExternalConnectivity(cs, clientPodConfig, externalContainer.GetIPv4(), externalContainer.GetIPv6(), externalContainer.GetPort())
					},
					Entry("by one pod over a layer2 network",
						&networkAttachmentConfigParams{
							name:     userDefinedNetworkName,
							topology: "layer2",
							cidr:     joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:     "primary",
						},
						*podConfig("client-pod"),
					),
					Entry("by one pod over a layer2 network with custom network",
						&networkAttachmentConfigParams{
							name:                userDefinedNetworkName,
							topology:            "layer2",
							cidr:                joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
							role:                "primary",
							defaultGatewayIPs:   joinStrings(customL2IPv4Gateway, customL2IPv6Gateway),
							reservedCIDRs:       joinStrings(customL2IPv4ReservedCIDR, customL2IPv6ReservedCIDR),
							infrastructureCIDRs: joinStrings(customL2IPv4InfraCIDR, customL2IPv6InfraCIDR),
						},
						*podConfig("client-pod"),
					),
					Entry("by one pod over a layer3 network",
						&networkAttachmentConfigParams{
							name:     userDefinedNetworkName,
							topology: "layer3",
							cidr:     primaryLayer3MultiCIDRs(),
							role:     "primary",
						},
						*podConfig("client-pod"),
					),
				)
			},
			Entry("NetworkAttachmentDefinitions", func(c *networkAttachmentConfigParams) error {
				netConfig := newNetworkAttachmentConfig(*c)
				nad := generateNAD(netConfig, f.ClientSet)
				_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(context.Background(), nad, metav1.CreateOptions{})
				return err
			}),
			Entry("UserDefinedNetwork", func(c *networkAttachmentConfigParams) error {
				udnManifest := generateUserDefinedNetworkManifest(c, f.ClientSet)
				cleanup, err := createManifest(f.Namespace.Name, udnManifest)
				DeferCleanup(cleanup)
				Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, f.Namespace.Name, c.name), 5*time.Second, time.Second).Should(Succeed())
				return err
			}),
			Entry("ClusterUserDefinedNetwork", func(c *networkAttachmentConfigParams) error {
				c.name = randomNetworkMetaName()
				cudnManifest := generateClusterUserDefinedNetworkManifest(c, f.ClientSet)
				cleanup, err := createManifest("", cudnManifest)
				DeferCleanup(func() {
					cleanup()
					By("delete pods in test namespace to unblock CUDN CR & associate NAD deletion")
					Expect(cs.CoreV1().Pods(c.namespace).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
					_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", c.name, "--wait", fmt.Sprintf("--timeout=%ds", 120))
					Expect(err).NotTo(HaveOccurred())
				})
				Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, c.name), 5*time.Second, time.Second).Should(Succeed())
				return err
			}),
		)
	})

	Context("UDN Pod", func() {
		const (
			testUdnName = "test-net"
			testPodName = "test-pod-udn"
		)

		var udnPod *v1.Pod

		BeforeEach(func() {
			By("create tests UserDefinedNetwork")
			cleanup, err := createManifest(f.Namespace.Name, newPrimaryUserDefinedNetworkManifest(cs, testUdnName))
			DeferCleanup(cleanup)
			Expect(err).NotTo(HaveOccurred())
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, f.Namespace.Name, testUdnName), 5*time.Second, time.Second).Should(Succeed())
			By("create UDN pod")
			cfg := podConfig(testPodName, withCommand(func() []string {
				return httpServerContainerCmd(podClusterNetPort)
			}))
			cfg.namespace = f.Namespace.Name
			udnPod = runUDNPod(cs, f.Namespace.Name, *cfg, nil)
		})

		It("should react to k8s.ovn.org/open-default-ports annotations changes", func() {
			By("ensure enough Nodes are available for scheduling")
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), f.ClientSet, 2)
			framework.ExpectNoError(err, "two scheduleable Nodes must be available")
			if len(nodes.Items) < 2 {
				ginkgo.Skip("requires at least 2 Nodes")
			}
			node1Name, node2Name := nodes.Items[0].GetName(), nodes.Items[1].GetName()
			By("Creating second namespace for default network pod")
			defaultNetNamespace := f.Namespace.Name + "-default"
			_, err = cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: defaultNetNamespace,
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				Expect(cs.CoreV1().Namespaces().Delete(context.Background(), defaultNetNamespace, metav1.DeleteOptions{})).To(Succeed())
			}()

			By("creating default network client pod")
			defaultClientPod, err := createPod(f, "default-net-client-pod", node1Name,
				defaultNetNamespace, []string{}, nil)
			Expect(err).NotTo(HaveOccurred())

			By("creating default network hostNetwork client pod")
			hostNetPod, err := createPod(f, "host-net-client-pod", node2Name,
				defaultNetNamespace, []string{}, nil, func(pod *v1.Pod) {
					pod.Spec.HostNetwork = true
				})
			Expect(err).NotTo(HaveOccurred())

			udnIPv4, udnIPv6, err := podIPsForDefaultNetwork(
				cs,
				f.Namespace.Name,
				udnPod.GetName(),
			)
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("verify default network client pod can't access UDN pod on port %d", podClusterNetPort))
			for _, destIP := range []string{udnIPv4, udnIPv6} {
				if destIP == "" {
					continue
				}
				By("checking the default network pod can't reach UDN pod on IP " + destIP)
				Consistently(func() bool {
					return connectToServer(podConfiguration{namespace: defaultClientPod.Namespace, name: defaultClientPod.Name}, destIP, podClusterNetPort) != nil
				}, 5*time.Second).Should(BeTrue())

				By("checking the default hostNetwork pod can't reach UDN pod on IP " + destIP)
				Consistently(func() bool {
					return connectToServer(podConfiguration{namespace: hostNetPod.Namespace, name: hostNetPod.Name}, destIP, podClusterNetPort) != nil
				}, 5*time.Second).Should(BeTrue())
			}

			By("Open UDN pod port")

			udnPod.Annotations[openDefaultPortsAnnotation] = fmt.Sprintf(
				`- protocol: tcp
  port: %d`, podClusterNetPort)
			udnPod, err = cs.CoreV1().Pods(udnPod.Namespace).Update(context.Background(), udnPod, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("verify default network client pod can access UDN pod on open port %d", podClusterNetPort))
			for _, destIP := range []string{udnIPv4, udnIPv6} {
				if destIP == "" {
					continue
				}
				By("checking the default network pod can reach UDN pod on IP " + destIP)
				Eventually(func() bool {
					return connectToServer(podConfiguration{namespace: defaultClientPod.Namespace, name: defaultClientPod.Name}, destIP, podClusterNetPort) == nil
				}, 5*time.Second).Should(BeTrue())

				By("checking the default hostNetwork pod can reach UDN pod on IP " + destIP)
				Eventually(func() bool {
					return connectToServer(podConfiguration{namespace: hostNetPod.Namespace, name: hostNetPod.Name}, destIP, podClusterNetPort) == nil
				}, 5*time.Second).Should(BeTrue())
			}

			By("Update UDN pod port with the wrong syntax")
			// this should clean up open ports and throw an event
			udnPod.Annotations[openDefaultPortsAnnotation] = fmt.Sprintf(
				`- protocol: ppp
  port: %d`, podClusterNetPort)
			udnPod, err = cs.CoreV1().Pods(udnPod.Namespace).Update(context.Background(), udnPod, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("verify default network client pod can't access UDN pod on port %d", podClusterNetPort))
			for _, destIP := range []string{udnIPv4, udnIPv6} {
				if destIP == "" {
					continue
				}
				By("checking the default network pod can't reach UDN pod on IP " + destIP)
				Eventually(func() bool {
					return connectToServer(podConfiguration{namespace: defaultClientPod.Namespace, name: defaultClientPod.Name}, destIP, podClusterNetPort) != nil
				}, 5*time.Second).Should(BeTrue())

				By("checking the default hostNetwork pod can't reach UDN pod on IP " + destIP)
				Eventually(func() bool {
					return connectToServer(podConfiguration{namespace: hostNetPod.Namespace, name: hostNetPod.Name}, destIP, podClusterNetPort) != nil
				}, 5*time.Second).Should(BeTrue())
			}
			By("Verify syntax error is reported via event")
			events, err := cs.CoreV1().Events(udnPod.Namespace).List(context.Background(), metav1.ListOptions{})
			found := false
			for _, event := range events.Items {
				if event.Reason == "ErrorUpdatingResource" && strings.Contains(event.Message, "invalid protocol ppp") {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "should have found an event for invalid protocol")
		})
	})

	Context("Sync", func() {
		DescribeTable(
			"perform east/west traffic between nodes following OVN Kube node pod restart",
			func(
				netConfig networkAttachmentConfigParams,
				clientPodConfig podConfiguration,
				serverPodConfig podConfiguration,
			) {
				By("creating the network")
				netConfig.namespace = f.Namespace.Name
				udnManifest := generateUserDefinedNetworkManifest(&netConfig, f.ClientSet)
				cleanup, err := createManifest(netConfig.namespace, udnManifest)
				Expect(err).ShouldNot(HaveOccurred(), "creating manifest must succeed")
				DeferCleanup(cleanup)
				Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, netConfig.namespace, netConfig.name), 5*time.Second, time.Second).Should(Succeed())
				By("ensure two Nodes are available for scheduling")
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), f.ClientSet, 2)
				Expect(err).ShouldNot(HaveOccurred(), "test requires at least two schedulable nodes")
				if len(nodes.Items) < 2 {
					ginkgo.Skip("requires at least 2 Nodes")
				}
				node1Name, node2Name := nodes.Items[0].GetName(), nodes.Items[1].GetName()
				Expect(len(nodes.Items)).Should(BeNumerically(">=", 2), "test requires >= 2 Ready nodes")
				serverPodConfig.namespace = f.Namespace.Name
				serverPodConfig.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
				clientPodConfig.namespace = f.Namespace.Name
				clientPodConfig.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
				runUDNPod(cs, f.Namespace.Name, serverPodConfig, nil)
				runUDNPod(cs, f.Namespace.Name, clientPodConfig, nil)
				serverIP, err := getPodAnnotationIPsForAttachmentByIndex(cs, f.Namespace.Name, serverPodConfig.name, namespacedName(f.Namespace.Name, netConfig.name), 0)
				Expect(err).ShouldNot(HaveOccurred(), "UDN pod IP must be retrieved")
				By("restart OVNKube node pods on client and server Nodes and ensure connectivity")
				serverPod := getPod(f, serverPodConfig.name)
				clientPod := getPod(f, clientPodConfig.name)
				for _, testPod := range []*v1.Pod{clientPod, serverPod} {
					By(fmt.Sprintf("asserting the server pod IP %v is reachable from client before restart of OVNKube node pod on Node %s", serverIP, testPod.Spec.Hostname))
					Expect(reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, podClusterNetPort)).ShouldNot(HaveOccurred(), "must have connectivity to server pre OVN Kube node Pod restart")
					By(fmt.Sprintf("restarting OVNKube node Pod located on Node %s which hosts test Pod %s/%s", testPod.Spec.NodeName, testPod.Namespace, testPod.Name))
					Expect(restartOVNKubeNodePod(cs, deploymentconfig.Get().OVNKubernetesNamespace(), testPod.Spec.NodeName)).ShouldNot(HaveOccurred(), "restart of OVNKube node pod must succeed")
					By(fmt.Sprintf("asserting the server pod IP %v is reachable from client post restart", serverIP))
					Expect(reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, podClusterNetPort)).ShouldNot(HaveOccurred(), "must have connectivity to server post restart")
				}
			},
			Entry(
				"L3",
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
						return httpServerContainerCmd(podClusterNetPort)
					}),
				),
			),
			Entry(
				"L2",
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
						return httpServerContainerCmd(podClusterNetPort)
					}),
				),
			),
			Entry(
				"L2 with custom network",
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
						return httpServerContainerCmd(podClusterNetPort)
					}),
				),
			),
		)
	})

	Context("concurrent UDN provisioning", func() {
		const concurrentUDNs = 5

		DescribeTable(
			"maintains data path connectivity during burst UDN creation",
			func(
				topology string,
				multiSubnet bool,
			) {
				By("ensuring at least 2 schedulable nodes exist")
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), cs, 2)
				framework.ExpectNoError(err)
				if len(nodes.Items) < 2 {
					ginkgo.Skip("requires at least 2 Nodes")
				}
				node1Name, node2Name := nodes.Items[0].GetName(), nodes.Items[1].GetName()

				By("allocating unique CIDRs for each concurrent UDN")
				nextSubnets := newTestNetworkSubnetsAllocator()
				cidrs := make([]string, concurrentUDNs)
				for i := range concurrentUDNs {
					v4, v6 := nextSubnets(multiSubnet)
					cidrs[i] = filterCIDRsAndJoin(cs, joinStrings(append(v4, v6...)...))
				}

				By("deploying connectivity-probe pods on the default network")
				defaultNetNs := f.Namespace.Name + "-default"
				_, err = cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: defaultNetNs,
					},
				}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func() {
					Expect(cs.CoreV1().Namespaces().Delete(context.Background(), defaultNetNs, metav1.DeleteOptions{})).To(Succeed())
				}()

				probeServerConfig := *podConfig(
					"probe-server",
					withCommand(func() []string {
						return httpServerContainerCmd(podClusterNetPort)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: node1Name}),
				)
				probeServerConfig.namespace = defaultNetNs
				probeClientConfig := *podConfig(
					"probe-client",
					withNodeSelector(map[string]string{nodeHostnameKey: node2Name}),
				)
				probeClientConfig.namespace = defaultNetNs

				runUDNPod(cs, defaultNetNs, probeServerConfig, nil)
				runUDNPod(cs, defaultNetNs, probeClientConfig, nil)

				probeServerIPv4, probeServerIPv6, err := podIPsForDefaultNetwork(cs, defaultNetNs, probeServerConfig.name)
				Expect(err).NotTo(HaveOccurred())

				By("verifying baseline cross-node connectivity before burst creation")
				for _, probeIP := range []string{probeServerIPv4, probeServerIPv6} {
					if probeIP == "" {
						continue
					}
					Expect(connectToServer(probeClientConfig, probeIP, podClusterNetPort)).To(Succeed())
				}

				By("creating namespaces for concurrent UDNs")
				type udnInstance struct {
					namespace string
					netName   string
				}
				udnInstances := make([]udnInstance, concurrentUDNs)
				for i := range concurrentUDNs {
					nsName := fmt.Sprintf("%s-burst-%d-%s", f.Namespace.Name, i, rand.String(5))
					netName := fmt.Sprintf("burst-net-%d", i)
					udnInstances[i] = udnInstance{namespace: nsName, netName: netName}
					_, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Name:   nsName,
							Labels: map[string]string{RequiredUDNNamespaceLabel: ""},
						},
					}, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
					defer func() {
						Expect(cs.CoreV1().Namespaces().Delete(context.Background(), nsName, metav1.DeleteOptions{})).To(Succeed())
					}()
				}

				By("creating all UDNs concurrently to trigger burst provisioning")
				var wg sync.WaitGroup
				wg.Add(concurrentUDNs)
				for i := range concurrentUDNs {
					go func(idx int) {
						defer ginkgo.GinkgoRecover()
						defer wg.Done()
						inst := udnInstances[idx]
						netConfig := &networkAttachmentConfigParams{
							name:      inst.netName,
							namespace: inst.namespace,
							topology:  topology,
							cidr:      cidrs[idx],
							role:      "primary",
						}
						udnManifest := generateUserDefinedNetworkManifest(netConfig, cs)
						cleanup, err := createManifest(inst.namespace, udnManifest)
						Expect(err).NotTo(HaveOccurred())
						DeferCleanup(cleanup)
					}(i)
				}
				done := make(chan struct{})
				go func() {
					wg.Wait()
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(60 * time.Second):
					framework.Failf("timed out waiting for burst UDN creation goroutines to finish")
				}

				By("waiting for all UDNs to reach NetworkCreated condition")
				for _, inst := range udnInstances {
					Eventually(
						userDefinedNetworkReadyFunc(f.DynamicClient, inst.namespace, inst.netName),
						60*time.Second, time.Second,
					).Should(Succeed(), "UDN %s/%s did not reach NetworkCreated", inst.namespace, inst.netName)
				}

				By("verifying cross-node connectivity was not disrupted during the burst")
				for _, probeIP := range []string{probeServerIPv4, probeServerIPv6} {
					if probeIP == "" {
						continue
					}
					Eventually(func() error {
						return connectToServer(probeClientConfig, probeIP, podClusterNetPort)
					}, 30*time.Second, 2*time.Second).Should(Succeed(), "post-burst cross-node connectivity check failed for %s", probeIP)
				}

				By("verifying UDN data path with pods on the first burst-created network")
				verifyInst := udnInstances[0]
				serverPodConfig := *podConfig(
					"udn-server",
					withCommand(func() []string {
						return httpServerContainerCmd(podClusterNetPort)
					}),
					withNodeSelector(map[string]string{nodeHostnameKey: node1Name}),
				)
				serverPodConfig.namespace = verifyInst.namespace
				clientPodConfig := *podConfig("udn-client",
					withNodeSelector(map[string]string{nodeHostnameKey: node2Name}),
				)
				clientPodConfig.namespace = verifyInst.namespace

				runUDNPod(cs, verifyInst.namespace, serverPodConfig, nil)
				runUDNPod(cs, verifyInst.namespace, clientPodConfig, nil)

				serverIP, err := getPodAnnotationIPsForAttachmentByIndex(
					cs,
					verifyInst.namespace,
					serverPodConfig.name,
					namespacedName(verifyInst.namespace, verifyInst.netName),
					0,
				)
				Expect(err).NotTo(HaveOccurred())

				Eventually(func() error {
					return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, podClusterNetPort)
				}, 2*time.Minute, 6*time.Second).Should(Succeed())
			},
			Entry("L3", "layer3", true),
			Entry("L2", "layer2", false),
		)
	})
})

// randomNetworkMetaName return pseudo random name for network related objects (NAD,UDN,CUDN).
// CUDN is cluster-scoped object, in case tests running in parallel, having random names avoids
// conflicting with other tests.
func randomNetworkMetaName() string {
	return fmt.Sprintf("test-net-%s", rand.String(5))
}

var nadToUdnParams = map[string]string{
	"primary":   "Primary",
	"secondary": "Secondary",
	"layer2":    "Layer2",
	"layer3":    "Layer3",
}

func generateUserDefinedNetworkManifest(params *networkAttachmentConfigParams, client clientset.Interface) string {
	filterSupportedNetworkConfig(client, params)

	subnets := generateSubnetsYaml(params)
	manifest := `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: ` + params.name + `
spec:
  topology: ` + nadToUdnParams[params.topology] + `
  ` + params.topology + `: 
    role: ` + nadToUdnParams[params.role] + `
    subnets: ` + subnets + ``
	if params.topology == "layer2" && params.role == "primary" {
		if len(params.reservedCIDRs) > 0 {
			manifest += `
    reservedSubnets: [` + params.reservedCIDRs + `]`
		}
		if len(params.infrastructureCIDRs) > 0 {
			manifest += `
    infrastructureSubnets: [` + params.infrastructureCIDRs + `]`
		}
		if len(params.defaultGatewayIPs) > 0 {
			manifest += `
    defaultGatewayIPs: [` + params.defaultGatewayIPs + `]`
		}
	}
	return manifest
}

func generateClusterUserDefinedNetworkManifest(params *networkAttachmentConfigParams, client clientset.Interface) string {
	filterSupportedNetworkConfig(client, params)

	subnets := generateSubnetsYaml(params)
	manifest := `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ` + params.name + `
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [` + params.namespace + `]
  network:
    topology: ` + nadToUdnParams[params.topology] + `
    ` + params.topology + `: 
      role: ` + nadToUdnParams[params.role] + `
      subnets: ` + subnets + ``

	if params.topology == "layer2" && params.role == "primary" {
		if len(params.reservedCIDRs) > 0 {
			manifest += `
      reservedSubnets: [` + params.reservedCIDRs + `]`
		}
		if len(params.infrastructureCIDRs) > 0 {
			manifest += `
      infrastructureSubnets: [` + params.infrastructureCIDRs + `]`
		}
		if len(params.defaultGatewayIPs) > 0 {
			manifest += `
      defaultGatewayIPs: [` + params.defaultGatewayIPs + `]`
		}
	}

	return manifest
}

func generateSubnetsYaml(params *networkAttachmentConfigParams) string {
	if params.topology == "layer3" {
		l3Subnets := generateLayer3Subnets(params.cidr)
		return fmt.Sprintf("[%s]", strings.Join(l3Subnets, ","))
	}
	return fmt.Sprintf("[%s]", params.cidr)
}

func generateLayer3Subnets(cidrs string) []string {
	cidrList := strings.Split(cidrs, ",")
	var subnets []string
	for _, cidr := range cidrList {
		cidrSplit := strings.Split(cidr, "/")
		switch len(cidrSplit) {
		case 2:
			subnets = append(subnets, fmt.Sprintf(`{cidr: "%s/%s"}`, cidrSplit[0], cidrSplit[1]))
		case 3:
			subnets = append(subnets, fmt.Sprintf(`{cidr: "%s/%s", hostSubnet: %s }`, cidrSplit[0], cidrSplit[1], cidrSplit[2]))
		default:
			panic(fmt.Sprintf("invalid layer3 subnet: %v", cidr))
		}
	}
	return subnets
}

// userDefinedNetworkReadyFunc returns a function that checks for the NetworkCreated condition in the provided udn
func userDefinedNetworkReadyFunc(client dynamic.Interface, namespace, name string) func() error {
	return networkReadyFunc(client.Resource(udnGVR).Namespace(namespace), name)
}

// userDefinedNetworkReadyFunc returns a function that checks for the NetworkCreated condition in the provided cluster udn
func clusterUserDefinedNetworkReadyFunc(client dynamic.Interface, name string) func() error {
	return networkReadyFunc(client.Resource(clusterUDNGVR), name)
}

func networkReadyFunc(client dynamic.ResourceInterface, name string) func() error {
	return func() error {
		cUDN, err := client.Get(context.Background(), name, metav1.GetOptions{}, "status")
		if err != nil {
			return err
		}
		conditions, err := getConditions(cUDN)
		if err != nil {
			return err
		}
		if len(conditions) == 0 {
			return fmt.Errorf("no conditions found in: %v", cUDN)
		}
		for _, condition := range conditions {
			if condition.Type == "NetworkCreated" && condition.Status == metav1.ConditionTrue {
				return nil
			}
		}
		return fmt.Errorf("no NetworkCreated condition found in: %v", cUDN)
	}
}

func checkStatusFunc(client dynamic.ResourceInterface, name string, condType string, condStatus metav1.ConditionStatus) func() error {
	return func() error {
		obj, err := client.Get(context.Background(), name, metav1.GetOptions{}, "status")
		if err != nil {
			return err
		}
		conditions, err := getConditions(obj)
		if err != nil {
			return err
		}
		if len(conditions) == 0 {
			return fmt.Errorf("no conditions found in: %v", obj)
		}
		for _, condition := range conditions {
			if condition.Type == condType {
				if condition.Status == condStatus {
					return nil
				}
				return fmt.Errorf("unexpected condition status: %s", condition.Status)
			}
		}
		return fmt.Errorf("no %s condition found in: %v", condType, obj)
	}
}

func createManifest(namespace, manifest string) (func(), error) {
	tmpDir, err := os.MkdirTemp("", "udn-test")
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			framework.Logf("Unable to remove udn test yaml files from disk %s: %v", tmpDir, err)
		}
	}

	path := filepath.Join(tmpDir, "test-ovn-k-udn-"+rand.String(5)+".yaml")
	if err := os.WriteFile(path, []byte(manifest), 0644); err != nil {
		return cleanup, fmt.Errorf("unable to write udn yaml to disk: %w", err)
	}

	_, err = e2ekubectl.RunKubectl(namespace, "create", "-f", path)
	if err != nil {
		return cleanup, err
	}
	return cleanup, nil
}

func applyManifest(namespace, manifest string) (func(), error) {
	path := "test-" + randString(5) + ".yaml"
	if err := os.WriteFile(path, []byte(manifest), 0644); err != nil {
		framework.Failf("Unable to write yaml to disk: %v", err)
	}
	cleanup := func() {
		if err := os.Remove(path); err != nil {
			framework.Logf("Unable to remove yaml from disk: %v", err)
		}
	}
	_, err := e2ekubectl.RunKubectl(namespace, "apply", "-f", path)
	if err != nil {
		return cleanup, err
	}
	return cleanup, nil
}

func assertL2SecondaryNetAttachDefManifest(nadClient nadclient.K8sCniCncfIoV1Interface, namespace, udnName, udnUID string) {
	nad, err := nadClient.NetworkAttachmentDefinitions(namespace).Get(context.Background(), udnName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	ExpectWithOffset(1, nad.Name).To(Equal(udnName))
	ExpectWithOffset(1, nad.Namespace).To(Equal(namespace))
	ExpectWithOffset(1, nad.OwnerReferences).To(Equal([]metav1.OwnerReference{{
		APIVersion:         "k8s.ovn.org/v1",
		Kind:               "UserDefinedNetwork",
		Name:               "test-net",
		UID:                types.UID(udnUID),
		BlockOwnerDeletion: pointer.Bool(true),
		Controller:         pointer.Bool(true),
	}}))
	expectedNetworkName := namespace + "_" + udnName
	expectedNadName := namespace + "/" + udnName
	ExpectWithOffset(1, nad.Spec.Config).To(MatchJSON(`{
		"cniVersion":"` + expectedUDNCNIVersion + `",
		"type": "ovn-k8s-cni-overlay",
		"name": "` + expectedNetworkName + `",
		"netAttachDefName": "` + expectedNadName + `",
		"topology": "layer2",
		"role": "secondary",
		"subnets": "10.10.100.0/24"
	}`))
}

func validateUDNStatusReportsConsumers(client dynamic.Interface, udnNamesapce, udnName, expectedPodName string) error {
	udn, err := client.Resource(udnGVR).Namespace(udnNamesapce).Get(context.Background(), udnName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	conditions, err := getConditions(udn)
	if err != nil {
		return err
	}
	conditions = normalizeConditions(conditions)
	expectedMsg := fmt.Sprintf("failed to delete NetworkAttachmentDefinition [%[1]s/%[2]s]: network in use by the following pods: [%[1]s/%[3]s]",
		udnNamesapce, udnName, expectedPodName)
	expectedCondition := metav1.Condition{
		Type:    "NetworkCreated",
		Status:  "False",
		Reason:  "SyncError",
		Message: expectedMsg,
	}
	for _, condition := range conditions {
		if condition == expectedCondition {
			return nil
		}
	}
	return fmt.Errorf("expected condition %v not found in %v", expectedCondition, conditions)
}

func normalizeConditions(conditions []metav1.Condition) []metav1.Condition {
	for i := range conditions {
		t := metav1.NewTime(time.Time{})
		conditions[i].LastTransitionTime = t
		conditions[i].ObservedGeneration = 0
	}
	return conditions
}

func assertClusterNADManifest(nadClient nadclient.K8sCniCncfIoV1Interface, namespace, udnName, udnUID string) {
	nad, err := nadClient.NetworkAttachmentDefinitions(namespace).Get(context.Background(), udnName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	ExpectWithOffset(1, nad.Name).To(Equal(udnName))
	ExpectWithOffset(1, nad.Namespace).To(Equal(namespace))
	ExpectWithOffset(1, nad.OwnerReferences).To(Equal([]metav1.OwnerReference{{
		APIVersion:         "k8s.ovn.org/v1",
		Kind:               "ClusterUserDefinedNetwork",
		Name:               udnName,
		UID:                types.UID(udnUID),
		BlockOwnerDeletion: pointer.Bool(true),
		Controller:         pointer.Bool(true),
	}}))
	ExpectWithOffset(1, nad.Labels).To(Equal(map[string]string{"k8s.ovn.org/user-defined-network": ""}))
	ExpectWithOffset(1, nad.Finalizers).To(Equal([]string{"k8s.ovn.org/user-defined-network-protection"}))

	expectedNetworkName := "cluster_udn_" + udnName
	expectedNadName := namespace + "/" + udnName
	ExpectWithOffset(1, nad.Spec.Config).To(MatchJSON(`{
		"cniVersion":"` + expectedUDNCNIVersion + `",
		"type": "ovn-k8s-cni-overlay",
		"name": "` + expectedNetworkName + `",
		"netAttachDefName": "` + expectedNadName + `",
		"topology": "layer2",
		"role": "secondary",
		"subnets": "10.100.0.0/16"
	}`))
}

var clusterUDNGVR = schema.GroupVersionResource{
	Group:    "k8s.ovn.org",
	Version:  "v1",
	Resource: "clusteruserdefinednetworks",
}

var udnGVR = schema.GroupVersionResource{
	Group:    "k8s.ovn.org",
	Version:  "v1",
	Resource: "userdefinednetworks",
}

// getConditions extracts metav1 conditions from .status.conditions of an unstructured object
func getConditions(uns *unstructured.Unstructured) ([]metav1.Condition, error) {
	var conditions []metav1.Condition
	conditionsRaw, found, err := unstructured.NestedFieldNoCopy(uns.Object, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("failed getting conditions in %s: %v", uns.GetName(), err)
	}
	if !found {
		return nil, fmt.Errorf("conditions not found in %v", uns)
	}

	conditionsJSON, err := json.Marshal(conditionsRaw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(conditionsJSON, &conditions); err != nil {
		return nil, err
	}

	return conditions, nil
}

func validateClusterUDNStatusReportsActiveNamespacesFunc(client dynamic.Interface, cUDNName string, expectedActiveNsNames ...string) func() error {
	return func() error {
		cUDN, err := client.Resource(clusterUDNGVR).Get(context.Background(), cUDNName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		conditions, err := getConditions(cUDN)
		if err != nil {
			return err
		}
		if len(conditions) == 0 {
			return fmt.Errorf("expected at least one condition in %v", cUDN)
		}

		// Find NetworkCreated condition among all conditions
		var networkCreatedCond *metav1.Condition
		for i := range conditions {
			if conditions[i].Type == "NetworkCreated" {
				networkCreatedCond = &conditions[i]
				break
			}
		}
		if networkCreatedCond == nil {
			return fmt.Errorf("NetworkCreated condition not found in conditions: %v", conditions)
		}

		if networkCreatedCond.Status != metav1.ConditionTrue {
			return fmt.Errorf("expected True status in NetworkCreated condition, got: %v", networkCreatedCond)
		}
		if networkCreatedCond.Reason != "NetworkAttachmentDefinitionCreated" {
			return fmt.Errorf("expected NetworkAttachmentDefinitionCreated reason in NetworkCreated condition, got: %v", networkCreatedCond)
		}
		if !strings.Contains(networkCreatedCond.Message, "NetworkAttachmentDefinition has been created in following namespaces:") {
			return fmt.Errorf("expected \"NetworkAttachmentDefinition has been created in following namespaces:\" in %s", networkCreatedCond.Message)
		}

		for _, ns := range expectedActiveNsNames {
			if !strings.Contains(networkCreatedCond.Message, ns) {
				return fmt.Errorf("expected to find %q namespace in %s", ns, networkCreatedCond.Message)
			}
		}
		return nil
	}
}

func validateClusterUDNStatusReportConsumers(client dynamic.Interface, cUDNName, udnNamespace, expectedPodName string) error {
	cUDN, err := client.Resource(clusterUDNGVR).Get(context.Background(), cUDNName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	conditions, err := getConditions(cUDN)
	if err != nil {
		return err
	}
	conditions = normalizeConditions(conditions)

	// Find NetworkCreated condition among all conditions
	var networkCreatedCond *metav1.Condition
	for i := range conditions {
		if conditions[i].Type == "NetworkCreated" {
			networkCreatedCond = &conditions[i]
			break
		}
	}
	if networkCreatedCond == nil {
		return fmt.Errorf("NetworkCreated condition not found in conditions: %v", conditions)
	}

	expectedMsg := fmt.Sprintf("failed to delete NetworkAttachmentDefinition [%[1]s/%[2]s]: network in use by the following pods: [%[1]s/%[3]s]",
		udnNamespace, cUDNName, expectedPodName)
	expectedCondition := metav1.Condition{
		Type:    "NetworkCreated",
		Status:  "False",
		Reason:  "NetworkAttachmentDefinitionSyncError",
		Message: expectedMsg,
	}

	if !reflect.DeepEqual(*networkCreatedCond, expectedCondition) {
		return fmt.Errorf("expected NetworkCreated condition: %v, got: %v", expectedCondition, *networkCreatedCond)
	}
	return nil
}

func newClusterUDNManifest(name string, targetNamespaces ...string) string {
	targetNs := strings.Join(targetNamespaces, ",")
	return `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ` + name + `
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [ ` + targetNs + ` ]
  network:
    topology: Layer2
    layer2:
      role: Secondary
      subnets: ["10.100.0.0/16"]
`
}

func newPrimaryClusterUDNManifest(cs clientset.Interface, name string, targetNamespaces ...string) string {
	targetNs := strings.Join(targetNamespaces, ",")
	return `
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: ` + name + `
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [ ` + targetNs + ` ]
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets: ` + generateCIDRforClusterUDN(cs, primaryLayer3MultiCIDRs(), "")
}

func newL2SecondaryUDNManifest(name string) string {
	return `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: ` + name + `
spec:
  topology: "Layer2"
  layer2:
    role: Secondary
    subnets: ["10.10.100.0/24"]
`
}

func newPrimaryUserDefinedNetworkManifest(cs clientset.Interface, name string) string {
	return `
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: ` + name + `
spec:
  topology: Layer3
  layer3:
    role: Primary
    subnets: ` + generateCIDRforUDN(cs, primaryLayer3MultiCIDRs())
}

func generateCIDRforUDN(cs clientset.Interface, cidrs string) string {
	cidrItems := generateLayer3Subnets(filterCIDRsAndJoin(cs, cidrs))
	var subnets strings.Builder
	for _, cidr := range cidrItems {
		subnets.WriteString("\n    - ")
		subnets.WriteString(cidr)
	}
	return subnets.String()
}

func filterDualStackCIDRs(cs clientset.Interface, cidrs udnv1.DualStackCIDRs) udnv1.DualStackCIDRs {
	filteredCIDRs := make(udnv1.DualStackCIDRs, 0, len(cidrs))
	for _, cidr := range cidrs {
		if !isCIDRIPFamilySupported(cs, string(cidr)) {
			continue
		}
		filteredCIDRs = append(filteredCIDRs, cidr)
	}
	return filteredCIDRs
}

func filterL3Subnets(cs clientset.Interface, l3Subnets []udnv1.Layer3Subnet) []udnv1.Layer3Subnet {
	filteredL3Subnets := make([]udnv1.Layer3Subnet, 0, len(l3Subnets))
	for _, l3Subnet := range l3Subnets {
		if !isCIDRIPFamilySupported(cs, string(l3Subnet.CIDR)) {
			continue
		}
		filteredL3Subnets = append(filteredL3Subnets, l3Subnet)
	}
	return filteredL3Subnets
}

func matchL3SubnetsByIPFamilies(families sets.Set[utilnet.IPFamily], in ...udnv1.Layer3Subnet) (out []udnv1.Layer3Subnet) {
	for _, subnet := range in {
		if families.Has(utilnet.IPFamilyOfCIDRString(string(subnet.CIDR))) {
			out = append(out, subnet)
		}
	}
	return
}

func matchL2SubnetsByIPFamilies(families sets.Set[utilnet.IPFamily], in ...udnv1.CIDR) (out []udnv1.CIDR) {
	for _, subnet := range in {
		if families.Has(utilnet.IPFamilyOfCIDRString(string(subnet))) {
			out = append(out, subnet)
		}
	}
	return
}

func generateCIDRforClusterUDN(cs clientset.Interface, v4, v6 string) string {
	cidrItems := generateLayer3Subnets(filterCIDRsAndJoin(cs, joinStrings(v4, v6)))
	return fmt.Sprintf("[%s]", strings.Join(cidrItems, ","))
}

type podOption func(*podConfiguration)

func podConfig(podName string, opts ...podOption) *podConfiguration {
	pod := &podConfiguration{
		name: podName,
	}
	for _, opt := range opts {
		opt(pod)
	}
	return pod
}

func withCommand(cmdGenerationFn func() []string) podOption {
	return func(pod *podConfiguration) {
		pod.containerCmd = cmdGenerationFn()
	}
}

func withNodeSelector(nodeSelector map[string]string) podOption {
	return func(pod *podConfiguration) {
		pod.nodeSelector = nodeSelector
	}
}

func withLabels(labels map[string]string) podOption {
	return func(pod *podConfiguration) {
		pod.labels = labels
	}
}

func withAnnotations(annotations map[string]string) podOption {
	return func(pod *podConfiguration) {
		pod.annotations = annotations
	}
}

func withNetworkAttachment(networks []nadapi.NetworkSelectionElement) podOption {
	return func(pod *podConfiguration) {
		pod.attachments = networks
	}
}

func podIPsForDefaultNetwork(k8sClient clientset.Interface, podNamespace string, podName string) (string, string, error) {
	pod, err := k8sClient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	ipv4, ipv6 := getPodAddresses(pod)
	return ipv4, ipv6, nil
}

func runUDNPod(cs clientset.Interface, namespace string, serverPodConfig podConfiguration, podSpecTweak func(*v1.Pod)) *v1.Pod {
	By(fmt.Sprintf("instantiating the UDN pod %s", serverPodConfig.name))
	podSpec := generatePodSpec(serverPodConfig)
	if podSpecTweak != nil {
		podSpecTweak(podSpec)
	}
	serverPod, err := cs.CoreV1().Pods(serverPodConfig.namespace).Create(
		context.Background(),
		podSpec,
		metav1.CreateOptions{},
	)
	Expect(err).NotTo(HaveOccurred())
	Expect(serverPod).NotTo(BeNil())

	By(fmt.Sprintf("asserting the UDN pod %s reaches the `Ready` state", serverPodConfig.name))
	// Retrieve and use pod start timeout value from deployment config.
	err = e2epod.WaitTimeoutForPodRunningInNamespace(context.Background(), cs, serverPod.GetName(), namespace,
		infraprovider.Get().GetDefaultTimeoutContext().PodStart)
	Expect(err).NotTo(HaveOccurred())
	updatedPod, err := cs.CoreV1().Pods(namespace).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return updatedPod
}

// connectToServerViaDefaultNetwork sends the traffic via the pod's default interface
func connectToServerViaDefaultNetwork(clientPodConfig podConfiguration, serverIP string, port int) error {
	_, err := e2ekubectl.RunKubectl(
		clientPodConfig.namespace,
		"exec",
		clientPodConfig.name,
		"--",
		"curl",
		"--connect-timeout",
		"2",
		"--interface",
		"eth0",
		net.JoinHostPort(serverIP, fmt.Sprintf("%d", port)),
	)
	return err
}

// assertClientExternalConnectivity checks if the client can connect to an externally created IP outside the cluster
func assertClientExternalConnectivity(cs clientset.Interface, clientPodConfig podConfiguration, externalIpv4 string, externalIpv6 string, port uint16) {
	if isIPv4Supported(cs) {
		By("asserting the *client* pod can contact the server's v4 IP located outside the cluster")
		Eventually(func() error {
			return connectToServer(clientPodConfig, externalIpv4, port)
		}, 2*time.Minute, 6*time.Second).Should(Succeed())
	}

	if isIPv6Supported(cs) {
		By("asserting the *client* pod can contact the server's v6 IP located outside the cluster")
		Eventually(func() error {
			return connectToServer(clientPodConfig, externalIpv6, port)
		}, 2*time.Minute, 6*time.Second).Should(Succeed())
	}
}

func expectedNumberOfRoutes(cs clientset.Interface, netConfig networkAttachmentConfigParams) int {
	if netConfig.topology == "layer2" {
		if isIPv6Supported(cs) && isIPv4Supported(cs) {
			return 4 // 2 routes per family
		} else {
			return 2 //one family supported
		}
	}

	routes := len(filterCIDRs(cs, strings.Split(netConfig.cidr, ",")...))
	if isIPv4Supported(cs) {
		routes += 2
	}
	if isIPv6Supported(cs) {
		routes += 2
	}
	return routes
}

func unmarshalPodAnnotationAllNetworks(annotations map[string]string) (map[string]podAnnotation, error) {
	podNetworks := make(map[string]podAnnotation)
	ovnAnnotation, ok := annotations[OvnPodAnnotationName]
	if ok {
		if err := json.Unmarshal([]byte(ovnAnnotation), &podNetworks); err != nil {
			return nil, fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
				ovnAnnotation, err)
		}
	}
	return podNetworks, nil
}

func getNetworkSubnetsFromSpec(networkSpec *udnv1.NetworkSpec) []string {
	var subnets []string
	switch {
	case networkSpec.Layer2 != nil:
		for _, cidr := range networkSpec.Layer2.Subnets {
			subnets = append(subnets, string(cidr))
		}
	case networkSpec.Layer3 != nil:
		for _, subnet := range networkSpec.Layer3.Subnets {
			subnets = append(subnets, string(subnet.CIDR))
		}
	default:
		panic("unsupported network type")
	}
	return subnets
}

func getNodeSubnetAssignments(cs clientset.Interface, networkName string) (map[string]string, map[string]string, error) {
	nodeList, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
	if err != nil {
		return nil, nil, err
	}
	nodeSubnetMapV4 := map[string]string{}
	nodeSubnetMapV6 := map[string]string{}
	for _, node := range nodeList.Items {
		ipnets, err := ovnkubeutil.ParseNodeHostSubnetAnnotation(&node, networkName)
		if err != nil && !ovnkubeutil.IsAnnotationNotSetError(err) {
			return nil, nil, err
		}

		for _, ipnet := range ipnets {
			if utilnet.IsIPv4CIDR(ipnet) {
				nodeSubnetMapV4[node.Name] = ipnet.String()
			} else {
				nodeSubnetMapV6[node.Name] = ipnet.String()
			}
		}
	}
	return nodeSubnetMapV4, nodeSubnetMapV6, nil
}
