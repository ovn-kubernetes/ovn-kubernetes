// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
)

const (
	arpProxyIdleTimeout         = 300 * time.Second
	macBindingStabilizationTime = 30 * time.Second
	proxyPollInterval           = 5 * time.Second
	proxyIdlePollInterval       = 10 * time.Second
	proxyUDNReadyTimeout        = 30 * time.Second
	proxyUDNReadyPollInterval   = 1 * time.Second
	proxyPodDeletionTimeout     = 2 * time.Minute
	proxyCmdRetryTimeout        = 30 * time.Second
	proxyMigratedMAC            = "02:42:ac:12:00:ff"
)

type udnProxyTestType struct {
	createFunc func(cs clientset.Interface, f *framework.Framework, namespace string) (func(), string, error)
}

var _ = Describe("Network Segmentation: ARP Proxy", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("network-segmentation-arp-proxy")
	f.SkipNamespaceCreation = true

	var (
		cs          clientset.Interface
		providerCtx infraapi.Context
	)

	BeforeEach(func() {
		if os.Getenv("ENABLE_UDN_ARP_PROXY") != "true" {
			ginkgo.Skip("requires ENABLE_UDN_ARP_PROXY=true")
		}
		cs = f.ClientSet
		if !isIPv4Supported(cs) {
			ginkgo.Skip("ARP proxy requires IPv4 support in the cluster")
		}
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		Expect(err).NotTo(HaveOccurred())
		f.Namespace = namespace
		providerCtx = infraprovider.Get().NewTestContext()
	})

	DescribeTable("should install ARP proxy flows and manage MAC_Binding lifecycle for IPv4",
		func(udnType udnProxyTestType) {
			ctx := context.TODO()
			primaryNamespace := f.Namespace.Name
			ovnNs := deploymentconfig.Get().OVNKubernetesNamespace()

			By("getting schedulable nodes")
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, cs, 2)
			framework.ExpectNoError(err)
			Expect(nodes.Items).To(HaveLen(2), "need at least 2 schedulable nodes")
			targetNode := nodes.Items[0]
			targetNodeName := targetNode.Name

			By("creating an external container on the primary (kind) network")
			primaryNetwork, err := infraprovider.Get().PrimaryNetwork()
			framework.ExpectNoError(err)
			extPort := infraprovider.Get().GetExternalContainerPort()
			extContainer, err := providerCtx.CreateExternalContainer(infraapi.ExternalContainer{
				Name:    "arp-proxy-ext-server",
				Image:   images.AgnHost(),
				Network: primaryNetwork,
				CmdArgs: getAgnHostHTTPPortBindCMDArgs(extPort),
				ExtPort: extPort,
			})
			framework.ExpectNoError(err)
			extIPv4 := extContainer.IPv4
			Expect(extIPv4).NotTo(BeEmpty(), "external container must have an IPv4 address")

			extIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(extContainer, primaryNetwork)
			framework.ExpectNoError(err)
			extMAC := extIface.MAC
			Expect(extMAC).NotTo(BeEmpty(), "external container must have a MAC address")
			framework.Logf("external container: IPv4=%s MAC=%s port=%d", extIPv4, extMAC, extPort)

			By("verifying the default GR external port has flood disabled")
			defaultGR := fmt.Sprintf("GR_%s", targetNodeName)
			defaultGRExtPort := fmt.Sprintf("rtoe-%s", defaultGR)
			ovnkPod := getProxyOVNKubeNodePod(cs, ovnNs, targetNodeName)
			verifyGRPortNoFlood(f, ovnkPod, defaultGRExtPort)

			By("creating the primary UDN and waiting until ready")
			cleanup, _, err := udnType.createFunc(cs, f, primaryNamespace)
			framework.ExpectNoError(err)
			DeferCleanup(cleanup)

			By("creating 3 additional primary L3 UDNs in separate namespaces")
			for i := 0; i < 3; i++ {
				extraNs, err := f.CreateNamespace(ctx, fmt.Sprintf("%s-extra-%d", f.BaseName, i), map[string]string{
					"e2e-framework":           f.BaseName,
					RequiredUDNNamespaceLabel: "",
				})
				framework.ExpectNoError(err)

				extraName := fmt.Sprintf("extra-udn-%d", i)
				extraManifest := newPrimaryUserDefinedNetworkManifest(cs, extraName)
				extraCleanup, err := createManifest(extraNs.Name, extraManifest)
				framework.ExpectNoError(err)
				DeferCleanup(extraCleanup)
				Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, extraNs.Name, extraName),
					proxyUDNReadyTimeout, proxyUDNReadyPollInterval).Should(Succeed())
			}

			By("creating a test pod on the UDN")
			testPod := e2epod.NewAgnhostPod(primaryNamespace, "arp-proxy-client", nil, nil, nil)
			testPod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": targetNodeName}
			testPod, err = cs.CoreV1().Pods(primaryNamespace).Create(ctx, testPod, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			DeferCleanup(func() {
				_ = cs.CoreV1().Pods(primaryNamespace).Delete(ctx, testPod.Name, metav1.DeleteOptions{})
			})
			err = e2epod.WaitForPodRunningInNamespace(ctx, cs, testPod)
			framework.ExpectNoError(err)

			By("sending traffic from the pod to the external container")
			curlCmd := fmt.Sprintf("curl -s --connect-timeout 10 http://%s:%d/hostname", extIPv4, extPort)
			stdout, stderr, err := ExecShellInPodWithFullOutput(f, primaryNamespace, testPod.Name, curlCmd)
			framework.ExpectNoError(err, "curl failed: stdout=%s stderr=%s", stdout, stderr)

			By("verifying the default GR has a MAC_Binding for the external container IPv4")
			Eventually(checkProxyMACBindingExists(f, ovnkPod, defaultGRExtPort, extIPv4, extMAC),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

			By("verifying ARP responder flows exist on br-ex for external container IPv4")
			Eventually(checkProxyARPResponderFlowExists(ovnkPod, extIPv4),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

			By("verifying the kernel ARP table has an entry for the external container")
			Eventually(checkProxyKernelARPEntry(targetNodeName, extIPv4, extMAC),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

			By("verifying MAC_Binding is removed after idle timeout")
			Eventually(checkProxyMACBindingAbsent(f, ovnkPod, defaultGRExtPort, extIPv4),
				arpProxyIdleTimeout+macBindingStabilizationTime, proxyIdlePollInterval).Should(Succeed())

			By("verifying ARP responder flows are removed after idle timeout")
			Eventually(checkProxyARPResponderFlowExists(ovnkPod, extIPv4),
				macBindingStabilizationTime, proxyPollInterval).Should(MatchError(ContainSubstring("not found on")))

			By("resuming traffic from the pod to the external container")
			stdout, stderr, err = ExecShellInPodWithFullOutput(f, primaryNamespace, testPod.Name, curlCmd)
			framework.ExpectNoError(err, "curl failed after idle: stdout=%s stderr=%s", stdout, stderr)

			By("verifying entries are re-created after resumed traffic")
			Eventually(checkProxyMACBindingExists(f, ovnkPod, defaultGRExtPort, extIPv4, extMAC),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())
			Eventually(checkProxyARPResponderFlowExists(ovnkPod, extIPv4),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())
			Eventually(checkProxyKernelARPEntry(targetNodeName, extIPv4, extMAC),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

			By(fmt.Sprintf("modifying MAC address of external container to simulate migration, new MAC: %s", proxyMigratedMAC))
			_, err = infraprovider.Get().ExecExternalContainerCommand(extContainer, []string{
				"ip", "link", "set", "dev", infraprovider.Get().ExternalContainerPrimaryInterfaceName(), "addr", proxyMigratedMAC,
			})
			framework.ExpectNoError(err, "failed to set MAC on external container")
			providerCtx.AddCleanUpFn(func() error {
				_, err := infraprovider.Get().ExecExternalContainerCommand(extContainer, []string{
					"ip", "link", "set", "dev", infraprovider.Get().ExternalContainerPrimaryInterfaceName(), "addr", extMAC,
				})
				return err
			})

			By("sending traffic from the pod to the external container after MAC migration")
			stdout, stderr, err = ExecShellInPodWithFullOutput(f, primaryNamespace, testPod.Name, curlCmd)
			framework.ExpectNoError(err, "curl failed after MAC migration: stdout=%s stderr=%s", stdout, stderr)

			By("verifying MAC_Binding for old MAC is removed on default GR")
			Eventually(checkProxyMACBindingAbsent(f, ovnkPod, defaultGRExtPort, extIPv4),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

			By("verifying MAC_Binding for new MAC is created on default GR")
			Eventually(checkProxyMACBindingExists(f, ovnkPod, defaultGRExtPort, extIPv4, proxyMigratedMAC),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

			By("verifying ARP responder flows exist on br-ex after MAC migration")
			Eventually(checkProxyARPResponderFlowExists(ovnkPod, extIPv4),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

			By("verifying kernel ARP entry is updated with the new MAC")
			Eventually(checkProxyKernelARPEntry(targetNodeName, extIPv4, proxyMigratedMAC),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

			By("deleting the test pod and waiting for deletion")
			err = cs.CoreV1().Pods(primaryNamespace).Delete(ctx, testPod.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err)
			err = e2epod.WaitForPodNotFoundInNamespace(ctx, cs, testPod.Name, primaryNamespace, proxyPodDeletionTimeout)
			framework.ExpectNoError(err)

			By("verifying no MAC_Binding entries remain for the external container IP")
			Eventually(checkProxyNoMACBindingsForIP(f, ovnkPod, extIPv4),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

			By("verifying no ARP responder flows remain on br-ex for the external container IP")
			Eventually(checkProxyARPResponderFlowExists(ovnkPod, extIPv4),
				macBindingStabilizationTime, proxyPollInterval).Should(MatchError(ContainSubstring("not found on")))

		},
		Entry("with L3 primary UDN", udnProxyTestType{
			createFunc: func(cs clientset.Interface, f *framework.Framework, namespace string) (func(), string, error) {
				name := "l3-primary-udn"
				manifest := newPrimaryUserDefinedNetworkManifest(cs, name)
				cleanup, err := createManifest(namespace, manifest)
				if err != nil {
					return cleanup, "", err
				}
				Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, namespace, name),
					proxyUDNReadyTimeout, proxyUDNReadyPollInterval).Should(Succeed())
				return cleanup, name, nil
			},
		}),
		Entry("with CUDN primary", udnProxyTestType{
			createFunc: func(cs clientset.Interface, f *framework.Framework, namespace string) (func(), string, error) {
				name := "cudn-primary"
				manifest := newPrimaryClusterUDNManifest(cs, name, namespace)
				cleanup, err := createManifest("", manifest)
				if err != nil {
					return cleanup, "", err
				}
				cudnCleanup := func() {
					cleanup()
					_ = cs.CoreV1().Pods(namespace).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
					e2ekubectl.RunKubectlOrDie("", "delete", "clusteruserdefinednetwork", name, "--wait", "--timeout=120s")
				}
				Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, name),
					proxyUDNReadyTimeout, proxyUDNReadyPollInterval).Should(Succeed())
				return cudnCleanup, name, nil
			},
		}),
	)
})

var _ = Describe("Network Segmentation: NDP Proxy", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("network-segmentation-ndp-proxy")
	f.SkipNamespaceCreation = true

	var (
		cs          clientset.Interface
		providerCtx infraapi.Context
	)

	BeforeEach(func() {
		if os.Getenv("ENABLE_UDN_NDP_PROXY") != "true" {
			ginkgo.Skip("requires ENABLE_UDN_NDP_PROXY=true")
		}
		cs = f.ClientSet
		if !isIPv6Supported(cs) {
			ginkgo.Skip("NDP proxy requires IPv6 support in the cluster")
		}
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		Expect(err).NotTo(HaveOccurred())
		f.Namespace = namespace
		providerCtx = infraprovider.Get().NewTestContext()
	})

	DescribeTable("should propagate MAC_Binding entries to all UDN GRs for IPv6",
		func(udnType udnProxyTestType) {
			ctx := context.TODO()
			primaryNamespace := f.Namespace.Name
			ovnNs := deploymentconfig.Get().OVNKubernetesNamespace()

			By("getting schedulable nodes")
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, cs, 2)
			framework.ExpectNoError(err)
			Expect(nodes.Items).To(HaveLen(2), "need at least 2 schedulable nodes")
			targetNode := nodes.Items[0]
			targetNodeName := targetNode.Name

			By("creating an external container on the primary (kind) network")
			primaryNetwork, err := infraprovider.Get().PrimaryNetwork()
			framework.ExpectNoError(err)
			extPort := infraprovider.Get().GetExternalContainerPort()
			extContainer, err := providerCtx.CreateExternalContainer(infraapi.ExternalContainer{
				Name:    "ndp-proxy-ext-server",
				Image:   images.AgnHost(),
				Network: primaryNetwork,
				CmdArgs: getAgnHostHTTPPortBindCMDArgs(extPort),
				ExtPort: extPort,
			})
			framework.ExpectNoError(err)
			extIPv6 := extContainer.IPv6
			Expect(extIPv6).NotTo(BeEmpty(), "external container must have an IPv6 address")

			extIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(extContainer, primaryNetwork)
			framework.ExpectNoError(err)
			extMAC := extIface.MAC
			Expect(extMAC).NotTo(BeEmpty(), "external container must have a MAC address")
			framework.Logf("external container: IPv6=%s MAC=%s port=%d", extIPv6, extMAC, extPort)

			By("creating the primary UDN and waiting until ready")
			cleanup, udnName, err := udnType.createFunc(cs, f, primaryNamespace)
			framework.ExpectNoError(err)
			DeferCleanup(cleanup)

			By("creating 3 additional primary L3 UDNs in separate namespaces")
			type udnInfo struct {
				namespace string
				name      string
			}
			var allUDNs []udnInfo
			allUDNs = append(allUDNs, udnInfo{namespace: primaryNamespace, name: udnName})
			for i := 0; i < 3; i++ {
				extraNs, err := f.CreateNamespace(ctx, fmt.Sprintf("%s-extra-%d", f.BaseName, i), map[string]string{
					"e2e-framework":           f.BaseName,
					RequiredUDNNamespaceLabel: "",
				})
				framework.ExpectNoError(err)

				extraName := fmt.Sprintf("extra-udn-%d", i)
				extraManifest := newPrimaryUserDefinedNetworkManifest(cs, extraName)
				extraCleanup, err := createManifest(extraNs.Name, extraManifest)
				framework.ExpectNoError(err)
				DeferCleanup(extraCleanup)
				Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, extraNs.Name, extraName),
					proxyUDNReadyTimeout, proxyUDNReadyPollInterval).Should(Succeed())
				allUDNs = append(allUDNs, udnInfo{namespace: extraNs.Name, name: extraName})
			}

			By("creating a test pod on the UDN")
			testPod := e2epod.NewAgnhostPod(primaryNamespace, "ndp-proxy-client", nil, nil, nil)
			testPod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": targetNodeName}
			testPod, err = cs.CoreV1().Pods(primaryNamespace).Create(ctx, testPod, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			DeferCleanup(func() {
				_ = cs.CoreV1().Pods(primaryNamespace).Delete(ctx, testPod.Name, metav1.DeleteOptions{})
			})
			err = e2epod.WaitForPodRunningInNamespace(ctx, cs, testPod)
			framework.ExpectNoError(err)

			By("sending IPv6 traffic from the pod to the external container")
			curlCmd := fmt.Sprintf("curl -6 -s --connect-timeout 10 http://[%s]:%d/hostname", extIPv6, extPort)
			stdout, stderr, err := ExecShellInPodWithFullOutput(f, primaryNamespace, testPod.Name, curlCmd)
			framework.ExpectNoError(err, "curl failed: stdout=%s stderr=%s", stdout, stderr)

			ovnkPod := getProxyOVNKubeNodePod(cs, ovnNs, targetNodeName)
			defaultGR := fmt.Sprintf("GR_%s", targetNodeName)
			defaultGRExtPort := fmt.Sprintf("rtoe-%s", defaultGR)

			By("verifying the default GR has a MAC_Binding for the external container IPv6")
			Eventually(checkProxyMACBindingExists(f, ovnkPod, defaultGRExtPort, extIPv6, extMAC),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

			By("verifying ALL UDN GRs have MAC_Binding entries propagated for the external container IPv6")
			for _, udn := range allUDNs {
				udnGRExtPort := proxyUDNGatewayRouterExtPort(udn.namespace, udn.name, targetNodeName)
				Eventually(checkProxyMACBindingExists(f, ovnkPod, udnGRExtPort, extIPv6, extMAC),
					macBindingStabilizationTime, proxyPollInterval).Should(Succeed())
			}

			By("verifying default GR MAC_Binding is removed after idle timeout")
			Eventually(checkProxyMACBindingAbsent(f, ovnkPod, defaultGRExtPort, extIPv6),
				arpProxyIdleTimeout+macBindingStabilizationTime, proxyIdlePollInterval).Should(Succeed())

			By("verifying all UDN GR MAC_Bindings are removed after idle timeout")
			for _, udn := range allUDNs {
				udnGRExtPort := proxyUDNGatewayRouterExtPort(udn.namespace, udn.name, targetNodeName)
				Eventually(checkProxyMACBindingAbsent(f, ovnkPod, udnGRExtPort, extIPv6),
					macBindingStabilizationTime, proxyPollInterval).Should(Succeed())
			}

			By("resuming IPv6 traffic from the pod to the external container")
			stdout, stderr, err = ExecShellInPodWithFullOutput(f, primaryNamespace, testPod.Name, curlCmd)
			framework.ExpectNoError(err, "curl failed after idle: stdout=%s stderr=%s", stdout, stderr)

			By("verifying entries are re-created after resumed traffic")
			Eventually(checkProxyMACBindingExists(f, ovnkPod, defaultGRExtPort, extIPv6, extMAC),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())
			for _, udn := range allUDNs {
				udnGRExtPort := proxyUDNGatewayRouterExtPort(udn.namespace, udn.name, targetNodeName)
				Eventually(checkProxyMACBindingExists(f, ovnkPod, udnGRExtPort, extIPv6, extMAC),
					macBindingStabilizationTime, proxyPollInterval).Should(Succeed())
			}

			By(fmt.Sprintf("modifying MAC address of external container to simulate migration, new MAC: %s", proxyMigratedMAC))
			_, err = infraprovider.Get().ExecExternalContainerCommand(extContainer, []string{
				"ip", "link", "set", "dev", infraprovider.Get().ExternalContainerPrimaryInterfaceName(), "addr", proxyMigratedMAC,
			})
			framework.ExpectNoError(err, "failed to set MAC on external container")
			providerCtx.AddCleanUpFn(func() error {
				_, err := infraprovider.Get().ExecExternalContainerCommand(extContainer, []string{
					"ip", "link", "set", "dev", infraprovider.Get().ExternalContainerPrimaryInterfaceName(), "addr", extMAC,
				})
				return err
			})

			By("sending IPv6 traffic from the pod to the external container after MAC migration")
			stdout, stderr, err = ExecShellInPodWithFullOutput(f, primaryNamespace, testPod.Name, curlCmd)
			framework.ExpectNoError(err, "curl failed after MAC migration: stdout=%s stderr=%s", stdout, stderr)

			By("verifying MAC_Binding for old MAC is removed on default GR")
			Eventually(checkProxyMACBindingAbsent(f, ovnkPod, defaultGRExtPort, extIPv6),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

			By("verifying MAC_Binding for new MAC is created on default GR")
			Eventually(checkProxyMACBindingExists(f, ovnkPod, defaultGRExtPort, extIPv6, proxyMigratedMAC),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

			By("verifying ALL UDN GRs have MAC_Binding entries updated with the new MAC")
			for _, udn := range allUDNs {
				udnGRExtPort := proxyUDNGatewayRouterExtPort(udn.namespace, udn.name, targetNodeName)
				Eventually(checkProxyMACBindingExists(f, ovnkPod, udnGRExtPort, extIPv6, proxyMigratedMAC),
					macBindingStabilizationTime, proxyPollInterval).Should(Succeed())
			}

			By("deleting the test pod and waiting for deletion")
			err = cs.CoreV1().Pods(primaryNamespace).Delete(ctx, testPod.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err)
			err = e2epod.WaitForPodNotFoundInNamespace(ctx, cs, testPod.Name, primaryNamespace, proxyPodDeletionTimeout)
			framework.ExpectNoError(err)

			By("verifying no MAC_Binding entries remain for the external container IPv6")
			Eventually(checkProxyNoMACBindingsForIP(f, ovnkPod, extIPv6),
				macBindingStabilizationTime, proxyPollInterval).Should(Succeed())
		},
		Entry("with L3 primary UDN", udnProxyTestType{
			createFunc: func(cs clientset.Interface, f *framework.Framework, namespace string) (func(), string, error) {
				name := "l3-primary-udn"
				manifest := newPrimaryUserDefinedNetworkManifest(cs, name)
				cleanup, err := createManifest(namespace, manifest)
				if err != nil {
					return cleanup, "", err
				}
				Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, namespace, name),
					proxyUDNReadyTimeout, proxyUDNReadyPollInterval).Should(Succeed())
				return cleanup, name, nil
			},
		}),
		Entry("with CUDN primary", udnProxyTestType{
			createFunc: func(cs clientset.Interface, f *framework.Framework, namespace string) (func(), string, error) {
				name := "cudn-primary"
				manifest := newPrimaryClusterUDNManifest(cs, name, namespace)
				cleanup, err := createManifest("", manifest)
				if err != nil {
					return cleanup, "", err
				}
				cudnCleanup := func() {
					cleanup()
					_ = cs.CoreV1().Pods(namespace).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
					e2ekubectl.RunKubectlOrDie("", "delete", "clusteruserdefinednetwork", name, "--wait", "--timeout=120s")
				}
				Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, name),
					proxyUDNReadyTimeout, proxyUDNReadyPollInterval).Should(Succeed())
				return cudnCleanup, name, nil
			},
		}),
	)
})

var _ = Describe("Network Segmentation: ARP Proxy Flow Counter Regression", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("network-segmentation-arp-flow-regression")
	f.SkipNamespaceCreation = true

	const (
		flowCounterMonitorPod = "arp-flow-counter-monitor"
		numExtraUDNs          = 2
	)

	type udnPortInfo struct {
		namespace string
		name      string
		patchPort string
		ofport    int
	}

	var (
		cs          clientset.Interface
		providerCtx infraapi.Context

		targetNodeName string
		ovnkPod        proxyOVNKubePodRef
		grExtMAC       string
		nodeIPv4       string
		extContainer   infraapi.ExternalContainer
		extIPv4        string
		extMAC         string
		extPort        uint16

		primaryUDN    udnPortInfo
		extraUDNs     []udnPortInfo
		defaultOfport int
	)

	BeforeEach(func() {
		if os.Getenv("ENABLE_UDN_ARP_PROXY") != "true" {
			ginkgo.Skip("requires ENABLE_UDN_ARP_PROXY=true")
		}
		cs = f.ClientSet
		if !isIPv4Supported(cs) {
			ginkgo.Skip("ARP proxy flow counter regression requires IPv4 support")
		}

		ctx := context.TODO()
		namespace, err := f.CreateNamespace(ctx, f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		Expect(err).NotTo(HaveOccurred())
		f.Namespace = namespace
		providerCtx = infraprovider.Get().NewTestContext()
		primaryNamespace := f.Namespace.Name

		By("getting schedulable nodes")
		nodes, err := e2enode.GetReadySchedulableNodes(ctx, cs)
		framework.ExpectNoError(err)
		Expect(nodes.Items).ToNot(BeEmpty(), "need at least 1 schedulable node")
		targetNode := nodes.Items[0]
		targetNodeName = targetNode.Name

		By("getting target node IPv4")
		targetNodeIPv4 := e2enode.GetAddressesByTypeAndFamily(&targetNode, v1.NodeInternalIP, v1.IPv4Protocol)
		Expect(targetNodeIPv4).NotTo(BeEmpty(), "target node %s must have an IPv4 InternalIP", targetNodeName)
		nodeIPv4 = targetNodeIPv4[0]

		ovnNs := deploymentconfig.Get().OVNKubernetesNamespace()
		ovnkPod = getProxyOVNKubeNodePod(cs, ovnNs, targetNodeName)

		By("getting the GR rtoe MAC (shared by all GRs on this node)")
		defaultGR := fmt.Sprintf("GR_%s", targetNodeName)
		defaultGRExtPort := fmt.Sprintf("rtoe-%s", defaultGR)
		grExtMAC = getProxyGRExtPortMAC(f, ovnkPod, defaultGRExtPort)
		framework.Logf("target node: %s IPv4=%s GR-rtoe-MAC=%s", targetNodeName, nodeIPv4, grExtMAC)

		By("creating the primary UDN")
		udnName := "l3-primary-udn"
		udnManifest := newPrimaryUserDefinedNetworkManifest(cs, udnName)
		udnCleanup, err := createManifest(primaryNamespace, udnManifest)
		framework.ExpectNoError(err)
		DeferCleanup(udnCleanup)
		Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, primaryNamespace, udnName),
			proxyUDNReadyTimeout, proxyUDNReadyPollInterval).Should(Succeed())

		primaryPatchPort := getBrexUDNPatchPortName(primaryNamespace, udnName, targetNodeName)
		primaryOfport, err := getPatchPortOfport(ovnkPod, primaryPatchPort)
		framework.ExpectNoError(err, "failed to get ofport for primary UDN patch port %s", primaryPatchPort)
		primaryUDN = udnPortInfo{
			namespace: primaryNamespace,
			name:      udnName,
			patchPort: primaryPatchPort,
			ofport:    primaryOfport,
		}
		framework.Logf("primary UDN patch port: %s ofport=%d", primaryPatchPort, primaryOfport)

		By(fmt.Sprintf("creating %d extra UDNs in separate namespaces", numExtraUDNs))
		extraUDNs = nil
		for i := 0; i < numExtraUDNs; i++ {
			extraNs, err := f.CreateNamespace(ctx, fmt.Sprintf("%s-extra-%d", f.BaseName, i), map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			framework.ExpectNoError(err)

			extraName := fmt.Sprintf("extra-udn-%d", i)
			extraManifest := newPrimaryUserDefinedNetworkManifest(cs, extraName)
			extraCleanup, err := createManifest(extraNs.Name, extraManifest)
			framework.ExpectNoError(err)
			DeferCleanup(extraCleanup)
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, extraNs.Name, extraName),
				proxyUDNReadyTimeout, proxyUDNReadyPollInterval).Should(Succeed())

			extraPatchPort := getBrexUDNPatchPortName(extraNs.Name, extraName, targetNodeName)
			extraOfport, err := getPatchPortOfport(ovnkPod, extraPatchPort)
			framework.ExpectNoError(err, "failed to get ofport for extra UDN %d patch port %s", i, extraPatchPort)
			extraUDNs = append(extraUDNs, udnPortInfo{
				namespace: extraNs.Name,
				name:      extraName,
				patchPort: extraPatchPort,
				ofport:    extraOfport,
			})
			framework.Logf("extra UDN %d patch port: %s ofport=%d", i, extraPatchPort, extraOfport)
		}

		By("getting the default network patch port ofport")
		defaultPatchPort := getBrexPatchPortName(targetNodeName)
		defaultOfport, err = getPatchPortOfport(ovnkPod, defaultPatchPort)
		framework.ExpectNoError(err, "failed to get ofport for default patch port %s", defaultPatchPort)
		framework.Logf("default patch port: %s ofport=%d", defaultPatchPort, defaultOfport)

		By("creating an external container on the primary (kind) network")
		primaryNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err)
		extPort = infraprovider.Get().GetExternalContainerPort()
		extContainer, err = providerCtx.CreateExternalContainer(infraapi.ExternalContainer{
			Name:    "arp-flow-regression-ext",
			Image:   images.AgnHost(),
			Network: primaryNetwork,
			CmdArgs: getAgnHostHTTPPortBindCMDArgs(extPort),
			ExtPort: extPort,
		})
		framework.ExpectNoError(err)
		extIPv4 = extContainer.IPv4
		Expect(extIPv4).NotTo(BeEmpty(), "external container must have an IPv4 address")

		extIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(extContainer, primaryNetwork)
		framework.ExpectNoError(err)
		extMAC = extIface.MAC
		Expect(extMAC).NotTo(BeEmpty(), "external container must have a MAC address")
		framework.Logf("external container: IPv4=%s MAC=%s port=%d", extIPv4, extMAC, extPort)
	})

	It("should not flood outbound ARP requests to unrelated UDN patch ports", func() {
		// Scenario: Pod on UDN1 sends traffic to external container. UDN1's GR needs to resolve the external container's MAC.
		// 			 It sends an ARP request out its patch port. NORMAL floods the request to all other patch ports.
		// Steps:
		// Setup: nodes, primary UDN + 2 extra UDNs, external container, test pod on UDN1
		// Get OVS patch port ofport numbers for all 3 UDN ports + default port
		// Get bridgeMAC from the GR rtoe port
		// Snapshot n_packets on the priority=10 egress flows for all patch ports
		// Send traffic from pod → external container (curl)
		// Wait for MAC_Binding to stabilize (proves ARP resolved)
		// Snapshot n_packets again
		// Assert: The UDN1 patch egress counter increased (it sent the ARP request)
		// Assert: The other UDN patch egress counters show the ARP request was NORMAL-flooded to them and they also emitted ARP (this proves the regression — with the fix, they should NOT increment)
		// Log all deltas for diagnostic visibility
		//
		// Key flow: priority=10, in_port=<patch>, dl_src=<bridgeMAC> → NORMAL

		ctx := context.TODO()

		By("creating a test pod on the primary UDN")
		testPod := e2epod.NewAgnhostPod(f.Namespace.Name, "arp-flow-outbound-client", nil, nil, nil)
		testPod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": targetNodeName}
		testPod, err := cs.CoreV1().Pods(f.Namespace.Name).Create(ctx, testPod, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		DeferCleanup(func() {
			_ = cs.CoreV1().Pods(f.Namespace.Name).Delete(ctx, testPod.Name, metav1.DeleteOptions{})
		})
		err = e2epod.WaitForPodRunningInNamespace(ctx, cs, testPod)
		framework.ExpectNoError(err)

		By("snapshotting per-patch egress flow counters before traffic")
		primaryBefore, err := getPatchEgressARPCount(ovnkPod, grExtMAC, primaryUDN.ofport)
		framework.ExpectNoError(err, "failed to get primary UDN egress count before")
		extraBefore := make([]int64, len(extraUDNs))
		for i, u := range extraUDNs {
			extraBefore[i], err = getPatchEgressARPCount(ovnkPod, grExtMAC, u.ofport)
			framework.ExpectNoError(err, "failed to get extra UDN %d egress count before", i)
		}
		framework.Logf("before traffic: primary egress=%d, extra egress=%v", primaryBefore, extraBefore)

		By("sending traffic from the pod to the external container to trigger ARP resolution")
		curlCmd := fmt.Sprintf("curl -s --connect-timeout 10 http://%s:%d/hostname", extIPv4, extPort)
		stdout, stderr, err := ExecShellInPodWithFullOutput(f, f.Namespace.Name, testPod.Name, curlCmd)
		framework.ExpectNoError(err, "curl failed: stdout=%s stderr=%s", stdout, stderr)

		By("waiting for MAC_Binding to stabilize")
		defaultGRExtPort := fmt.Sprintf("rtoe-GR_%s", targetNodeName)
		Eventually(checkProxyMACBindingExists(f, ovnkPod, defaultGRExtPort, extIPv4, extMAC),
			macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

		By("snapshotting per-patch egress flow counters after traffic")
		primaryAfter, err := getPatchEgressARPCount(ovnkPod, grExtMAC, primaryUDN.ofport)
		framework.ExpectNoError(err, "failed to get primary UDN egress count after")
		extraAfter := make([]int64, len(extraUDNs))
		for i, u := range extraUDNs {
			extraAfter[i], err = getPatchEgressARPCount(ovnkPod, grExtMAC, u.ofport)
			framework.ExpectNoError(err, "failed to get extra UDN %d egress count after", i)
		}

		primaryDelta := primaryAfter - primaryBefore
		framework.Logf("after traffic: primary egress=%d (delta=%d)", primaryAfter, primaryDelta)
		for i := range extraUDNs {
			framework.Logf("after traffic: extra UDN %d egress=%d (delta=%d)", i, extraAfter[i], extraAfter[i]-extraBefore[i])
		}

		By("asserting: primary UDN patch egress counter increased (ARP was sent)")
		Expect(primaryDelta).To(BeNumerically(">", 0),
			"primary UDN patch egress counter should have increased after ARP resolution")

		By("asserting: other UDN patch egress counters did NOT increase (no broadcast flood)")
		for i, u := range extraUDNs {
			delta := extraAfter[i] - extraBefore[i]
			Expect(delta).To(BeNumerically("==", 0),
				"extra UDN %d (%s) patch egress counter should not have increased (delta=%d), "+
					"indicating outbound ARP broadcast flood to unrelated patch ports", i, u.patchPort, delta)
		}
	})

	It("should not flood inbound unicast ARP replies to all patch ports", func() {
		// Scenario: External container sends a unicast ARP reply to the node's bridgeMAC.
		// 			 The priority-10 unicast flood rule sends it to ALL patch ports.
		// Steps:
		// Setup: nodes, primary UDN + 2 extra UDNs, external container, test pod on UDN1
		// Flush external container's ARP for the node IP
		// Snapshot n_packets on the unicast flood flow (priority=10, dl_dst=<bridgeMAC> → output:2,output:153,...,NORMAL)
		// Ping the node from external container (triggers broadcast ARP request → unicast reply back)
		// Snapshot n_packets again
		// Assert: The unicast flood flow counter increased — confirming the reply was flooded to all patches
		// Verify the flow action string contains all patch port ofport numbers (proving it fans out to all N)
		// Log the delta and the flow action for visibility
		//
		// Key flow: priority=10, dl_dst=<bridgeMAC> → output:2,output:153,output:154,NORMAL

		By("flushing external container's ARP entry for the node IP")
		_, err := infraprovider.Get().ExecExternalContainerCommand(extContainer, []string{
			"ip", "neigh", "flush", nodeIPv4,
		})
		framework.ExpectNoError(err, "failed to flush neighbor for %s on external container", nodeIPv4)

		By("finding the unicast flood flow and snapshotting its packet count")
		unicastFlowLine, err := getFlowLineForTable(ovnkPod, 0, "priority=10,", fmt.Sprintf("dl_dst=%s", grExtMAC))
		framework.ExpectNoError(err, "failed to find unicast flood flow for dl_dst=%s", grExtMAC)
		framework.Logf("unicast flood flow: %s", unicastFlowLine)

		countBefore, err := parseFlowPacketCount(unicastFlowLine)
		framework.ExpectNoError(err, "failed to parse n_packets from unicast flood flow")

		By("pinging the node from the external container to trigger ARP reply")
		pingOutput, err := infraprovider.Get().ExecExternalContainerCommand(extContainer, []string{
			"ping", "-c", "3", "-W", "2", nodeIPv4,
		})
		framework.ExpectNoError(err, "ping from external container to node %s failed: %s", nodeIPv4, pingOutput)

		By("snapshotting unicast flood flow packet count after ping")
		countAfter, err := getFlowPacketCountForTable(ovnkPod, 0, "priority=10,", fmt.Sprintf("dl_dst=%s", grExtMAC))
		framework.ExpectNoError(err, "failed to get unicast flood flow count after ping")
		delta := countAfter - countBefore
		framework.Logf("unicast flood flow: before=%d after=%d delta=%d", countBefore, countAfter, delta)

		By("checking how many output ports the unicast flood flow fans out to")
		actions := parseFlowActions(unicastFlowLine)
		numOutputs := countOutputActions(actions)
		framework.Logf("unicast flood flow actions: %s (output count=%d)", actions, numOutputs)

		By("asserting: unicast flood flow should target only 1 patch port (not all N)")
		Expect(numOutputs).To(BeNumerically("<=", 1),
			"unicast flood flow fans out to %d patch ports (actions=%s); with the fix it should target only 1",
			numOutputs, actions)
	})

	It("should not produce duplicate ARP replies from multiple GRs for inbound broadcast", func() {
		// Scenario: External host sends broadcast "Who has nodeIP?".
		// 			 Falls to priority-0 NORMAL. All GRs reply, producing N+1 duplicate ARP replies on the wire.
		// Steps:
		// Setup: nodes, primary UDN + 2 extra UDNs, external container
		// Flush external container's neighbor table for the node IP
		// Start tcpdump on eth0 (physical interface) with arpReplyFilterForMACs(grExtMAC, extMAC) — this filters ARP replies from the node MAC to the external container MAC
		// Snapshot per-patch egress flow counters (all patch ports)
		// Ping the node from the external container (forces broadcast ARP "Who has nodeIP?")
		// Read tcpdump logs — count ARP replies
		// Snapshot per-patch egress flow counters again
		// Assert: tcpdump shows N+1 ARP replies (one from each GR) — proves the duplication problem
		// Assert: ALL per-patch egress counters incremented — each GR replied
		// Assert (future, with fix): Only 1 ARP reply, only 1 patch counter increments
		// Log all counts for visibility
		//
		// Key flows: Per-patch egress + tcpdump on eth0

		By("flushing external container's neighbor table for the node IP")
		_, err := infraprovider.Get().ExecExternalContainerCommand(extContainer, []string{
			"ip", "neigh", "flush", nodeIPv4,
		})
		framework.ExpectNoError(err, "failed to flush neighbor for %s on external container", nodeIPv4)

		By("starting tcpdump on eth0 for ARP replies from the node MAC to the external container MAC")
		arpFilter, err := arpReplyFilterForMACs(grExtMAC, extMAC)
		framework.ExpectNoError(err, "failed to build ARP reply filter for sender=%s target=%s", grExtMAC, extMAC)
		monitorCtx, monitorCancel := context.WithCancel(context.Background())
		defer monitorCancel()
		brex := deploymentconfig.Get().ExternalBridgeName()
		briface := strings.Trim(brex, "br")
		quotedFilter := "'" + strings.ReplaceAll(arpFilter, "'", `'"'"'`) + "'"
		startTcpdumpMonitorPodOnNode(monitorCtx, f, proxyCmdRetryTimeout, flowCounterMonitorPod,
			targetNodeName, briface, "-n -vv -l", quotedFilter)
		framework.Logf("tcpdump monitor started on node %s interface %s", targetNodeName, briface)

		By("snapshotting per-patch egress flow counters before ping")
		primaryBefore, err := getPatchEgressARPCount(ovnkPod, grExtMAC, primaryUDN.ofport)
		framework.ExpectNoError(err, "failed to get primary UDN egress count before")
		extraBefore := make([]int64, len(extraUDNs))
		for i, u := range extraUDNs {
			extraBefore[i], err = getPatchEgressARPCount(ovnkPod, grExtMAC, u.ofport)
			framework.ExpectNoError(err, "failed to get extra UDN %d egress count before", i)
		}
		defaultBefore, err := getPatchEgressARPCount(ovnkPod, grExtMAC, defaultOfport)
		framework.ExpectNoError(err, "failed to get default patch egress count before")
		framework.Logf("before ping: default=%d primary=%d extra=%v", defaultBefore, primaryBefore, extraBefore)

		By("pinging the node from the external container (triggers broadcast ARP 'Who has nodeIP?')")
		pingOutput, err := infraprovider.Get().ExecExternalContainerCommand(extContainer, []string{
			"ping", "-c", "3", "-W", "2", nodeIPv4,
		})
		framework.ExpectNoError(err, "ping from external container to node %s failed: %s", nodeIPv4, pingOutput)

		By("reading tcpdump logs and counting ARP replies")
		monitorOutput, err := e2ekubectl.RunKubectl(f.Namespace.Name, "logs", flowCounterMonitorPod)
		framework.ExpectNoError(err, "failed to read tcpdump monitor logs")
		_, arpReplies := countARPPackets(monitorOutput, nodeIPv4)
		framework.Logf("tcpdump captured %d ARP replies for node IP %s", arpReplies, nodeIPv4)
		framework.Logf("tcpdump output:\n%s", monitorOutput)

		By("snapshotting per-patch egress flow counters after ping")
		primaryAfter, err := getPatchEgressARPCount(ovnkPod, grExtMAC, primaryUDN.ofport)
		framework.ExpectNoError(err, "failed to get primary UDN egress count after")
		extraAfter := make([]int64, len(extraUDNs))
		for i, u := range extraUDNs {
			extraAfter[i], err = getPatchEgressARPCount(ovnkPod, grExtMAC, u.ofport)
			framework.ExpectNoError(err, "failed to get extra UDN %d egress count after", i)
		}
		defaultAfter, err := getPatchEgressARPCount(ovnkPod, grExtMAC, defaultOfport)
		framework.ExpectNoError(err, "failed to get default patch egress count after")

		defaultDelta := defaultAfter - defaultBefore
		primaryDelta := primaryAfter - primaryBefore
		framework.Logf("after ping: default=%d (delta=%d) primary=%d (delta=%d)",
			defaultAfter, defaultDelta, primaryAfter, primaryDelta)

		patchesWithDelta := 0
		if defaultDelta > 0 {
			patchesWithDelta++
		}
		if primaryDelta > 0 {
			patchesWithDelta++
		}
		for i, u := range extraUDNs {
			delta := extraAfter[i] - extraBefore[i]
			framework.Logf("after ping: extra UDN %d (%s) egress=%d (delta=%d)", i, u.patchPort, extraAfter[i], delta)
			if delta > 0 {
				patchesWithDelta++
			}
		}

		By("asserting: exactly 1 ARP reply should appear on the wire (not N+1 duplicates)")
		Expect(arpReplies).To(BeNumerically("==", 1),
			"expected exactly 1 ARP reply for node IP %s, got %d — each GR is generating a duplicate reply", nodeIPv4, arpReplies)

		By("asserting: only 1 patch egress counter should have incremented")
		Expect(patchesWithDelta).To(BeNumerically("==", 1),
			"expected only 1 patch port to emit an ARP reply, but %d patch ports had egress counter increases", patchesWithDelta)
	})
})

var _ = Describe("Network Segmentation: NDP Proxy Flow Counter Regression", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("network-segmentation-ndp-flow-regression")
	f.SkipNamespaceCreation = true

	const (
		ndpFlowCounterMonitorPod = "ndp-flow-counter-monitor"
		numExtraUDNs             = 2
	)

	type udnPortInfo struct {
		namespace string
		name      string
		patchPort string
		ofport    int
	}

	var (
		cs          clientset.Interface
		providerCtx infraapi.Context

		targetNodeName string
		ovnkPod        proxyOVNKubePodRef
		grExtMAC       string
		nodeIPv6       string
		extContainer   infraapi.ExternalContainer
		extIPv6        string
		extMAC         string
		extPort        uint16

		primaryUDN    udnPortInfo
		extraUDNs     []udnPortInfo
		defaultOfport int
	)

	BeforeEach(func() {
		if os.Getenv("ENABLE_UDN_NDP_PROXY") != "true" {
			ginkgo.Skip("requires ENABLE_UDN_NDP_PROXY=true")
		}
		cs = f.ClientSet
		if !isIPv6Supported(cs) {
			ginkgo.Skip("NDP proxy flow counter regression requires IPv6 support")
		}

		ctx := context.TODO()
		namespace, err := f.CreateNamespace(ctx, f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		Expect(err).NotTo(HaveOccurred())
		f.Namespace = namespace
		providerCtx = infraprovider.Get().NewTestContext()
		primaryNamespace := f.Namespace.Name

		By("getting schedulable nodes")
		nodes, err := e2enode.GetReadySchedulableNodes(ctx, cs)
		framework.ExpectNoError(err)
		Expect(nodes.Items).ToNot(BeEmpty(), "need at least 1 schedulable node")
		targetNode := nodes.Items[0]
		targetNodeName = targetNode.Name

		By("getting target node IPv6")
		targetNodeIPv6 := e2enode.GetAddressesByTypeAndFamily(&targetNode, v1.NodeInternalIP, v1.IPv6Protocol)
		Expect(targetNodeIPv6).NotTo(BeEmpty(), "target node %s must have an IPv6 InternalIP", targetNodeName)
		nodeIPv6 = targetNodeIPv6[0]

		ovnNs := deploymentconfig.Get().OVNKubernetesNamespace()
		ovnkPod = getProxyOVNKubeNodePod(cs, ovnNs, targetNodeName)

		By("getting the GR rtoe MAC (shared by all GRs on this node)")
		defaultGR := fmt.Sprintf("GR_%s", targetNodeName)
		defaultGRExtPort := fmt.Sprintf("rtoe-%s", defaultGR)
		grExtMAC = getProxyGRExtPortMAC(f, ovnkPod, defaultGRExtPort)
		framework.Logf("target node: %s IPv6=%s GR-rtoe-MAC=%s", targetNodeName, nodeIPv6, grExtMAC)

		By("creating the primary UDN")
		udnName := "l3-primary-udn"
		udnManifest := newPrimaryUserDefinedNetworkManifest(cs, udnName)
		udnCleanup, err := createManifest(primaryNamespace, udnManifest)
		framework.ExpectNoError(err)
		DeferCleanup(udnCleanup)
		Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, primaryNamespace, udnName),
			proxyUDNReadyTimeout, proxyUDNReadyPollInterval).Should(Succeed())

		primaryPatchPort := getBrexUDNPatchPortName(primaryNamespace, udnName, targetNodeName)
		primaryOfport, err := getPatchPortOfport(ovnkPod, primaryPatchPort)
		framework.ExpectNoError(err, "failed to get ofport for primary UDN patch port %s", primaryPatchPort)
		primaryUDN = udnPortInfo{
			namespace: primaryNamespace,
			name:      udnName,
			patchPort: primaryPatchPort,
			ofport:    primaryOfport,
		}
		framework.Logf("primary UDN patch port: %s ofport=%d", primaryPatchPort, primaryOfport)

		By(fmt.Sprintf("creating %d extra UDNs in separate namespaces", numExtraUDNs))
		extraUDNs = nil
		for i := 0; i < numExtraUDNs; i++ {
			extraNs, err := f.CreateNamespace(ctx, fmt.Sprintf("%s-extra-%d", f.BaseName, i), map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			framework.ExpectNoError(err)

			extraName := fmt.Sprintf("extra-udn-%d", i)
			extraManifest := newPrimaryUserDefinedNetworkManifest(cs, extraName)
			extraCleanup, err := createManifest(extraNs.Name, extraManifest)
			framework.ExpectNoError(err)
			DeferCleanup(extraCleanup)
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, extraNs.Name, extraName),
				proxyUDNReadyTimeout, proxyUDNReadyPollInterval).Should(Succeed())

			extraPatchPort := getBrexUDNPatchPortName(extraNs.Name, extraName, targetNodeName)
			extraOfport, err := getPatchPortOfport(ovnkPod, extraPatchPort)
			framework.ExpectNoError(err, "failed to get ofport for extra UDN %d patch port %s", i, extraPatchPort)
			extraUDNs = append(extraUDNs, udnPortInfo{
				namespace: extraNs.Name,
				name:      extraName,
				patchPort: extraPatchPort,
				ofport:    extraOfport,
			})
			framework.Logf("extra UDN %d patch port: %s ofport=%d", i, extraPatchPort, extraOfport)
		}

		By("getting the default network patch port ofport")
		defaultPatchPort := getBrexPatchPortName(targetNodeName)
		defaultOfport, err = getPatchPortOfport(ovnkPod, defaultPatchPort)
		framework.ExpectNoError(err, "failed to get ofport for default patch port %s", defaultPatchPort)
		framework.Logf("default patch port: %s ofport=%d", defaultPatchPort, defaultOfport)

		By("creating an external container on the primary (kind) network")
		primaryNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err)
		extPort = infraprovider.Get().GetExternalContainerPort()
		extContainer, err = providerCtx.CreateExternalContainer(infraapi.ExternalContainer{
			Name:    "ndp-flow-regression-ext",
			Image:   images.AgnHost(),
			Network: primaryNetwork,
			CmdArgs: getAgnHostHTTPPortBindCMDArgs(extPort),
			ExtPort: extPort,
		})
		framework.ExpectNoError(err)
		extIPv6 = extContainer.IPv6
		Expect(extIPv6).NotTo(BeEmpty(), "external container must have an IPv6 address")

		extIface, err := infraprovider.Get().GetExternalContainerNetworkInterface(extContainer, primaryNetwork)
		framework.ExpectNoError(err)
		extMAC = extIface.MAC
		Expect(extMAC).NotTo(BeEmpty(), "external container must have a MAC address")
		framework.Logf("external container: IPv6=%s MAC=%s port=%d", extIPv6, extMAC, extPort)
	})

	It("should not flood outbound NDP Neighbor Solicitations to unrelated UDN patch ports", func() {
		// Problem: The default GR's outbound NS hits priority=102,icmp6,in_port=2,icmp_type=135 → ct,NORMAL.
		//          NORMAL floods the NS to ALL breth0 ports including UDN patches.
		//          UDN GRs' outbound NDP goes through priority=100,ipv6 → output:1 (directed, no flood),
		//          but the default GR lacks this directed handling.
		// Trigger: Default-network pod sends IPv6 to external container → default GR does NDP.
		// Scenario: Default-network pod sends IPv6 traffic to external container. The default GR's outbound NS hits priority=102,icmp6,in_port=2,icmp_type=135 → ct(commit),NORMAL. The NORMAL action floods the NS to all breth0 ports, including UDN patch ports that have no need for it. Meanwhile, UDN GRs' outbound NDP correctly goes through priority=100,ipv6 → output:1 (directed, no flood).
		// Important: Unlike ARP Test 1 (which uses a UDN pod because UDN GRs are fresh and need ARP), NDP Test 1 must use a default-network pod because only the default GR has the flooding priority=102,icmp6 flows. UDN GRs already use directed priority=100,ipv6 → output:1 — there is no flooding problem for UDN GR NDP.
		// MAC_Binding flush required: The default GR's MAC_Binding for the external container's IPv6 is typically pre-populated (from unsolicited NDP NA or kernel-level NDP) before the test runs, so the GR never needs to send an NDP NS. The test must explicitly delete the MAC_Binding via ovn-sbctl destroy MAC_Binding <uuid> before snapshotting counters and sending traffic. Without this flush, the NS counter stays at 0 and the test fails spuriously.
		// Steps:
		// Create a default-network namespace (no UDN label) and pod on the target node
		// Flush MAC_Binding for the external container's IPv6 on the default GR's rtoe port (ovn-sbctl find + destroy MAC_Binding)
		// Wait for MAC_Binding to be absent (Eventually(checkProxyMACBindingAbsent(...)))
		// Snapshot n_packets on priority=102,icmp6,in_port=<defaultOfport>,dl_src=<bridgeMAC>,icmp_type=135
		// Send IPv6 traffic from default-network pod to external container (curl -6)
		// Wait for MAC_Binding to reappear on default GR (proves NDP resolved)
		// Snapshot n_packets again
		// Assert: delta > 0 — NS was sent by default GR
		// Parse the flow action string
		// Assert: action should NOT contain NORMAL — with fix, should use directed output:1
		// Structural check: verify NO priority=102,icmp6,icmp_type=135 flows exist for UDN patch port in_port values — confirms the asymmetry (UDN GRs use priority=100,ipv6 directed flows)
		//
		// Key flow: table=0: priority=102,icmp6,in_port=2,dl_src=bridgeMAC,icmp_type=135 → ct(commit,...),NORMAL

		ctx := context.TODO()

		By("creating a default-network namespace (no UDN label) for the trigger pod")
		defaultNs, err := f.CreateNamespace(ctx, fmt.Sprintf("%s-default", f.BaseName), map[string]string{
			"e2e-framework": f.BaseName,
		})
		framework.ExpectNoError(err)

		By("creating a test pod on the default network")
		testPod := e2epod.NewAgnhostPod(defaultNs.Name, "ndp-flow-outbound-client", nil, nil, nil)
		testPod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": targetNodeName}
		testPod, err = cs.CoreV1().Pods(defaultNs.Name).Create(ctx, testPod, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		DeferCleanup(func() {
			_ = cs.CoreV1().Pods(defaultNs.Name).Delete(ctx, testPod.Name, metav1.DeleteOptions{})
		})
		err = e2epod.WaitForPodRunningInNamespace(ctx, cs, testPod)
		framework.ExpectNoError(err)

		defaultGRExtPort := fmt.Sprintf("rtoe-GR_%s", targetNodeName)

		By("flushing MAC_Binding for external container IPv6 on default GR to force NDP resolution")
		output, err := runProxyOVNSBCtlCmd(f, ovnkPod, "--no-headings", "--columns=_uuid", "find", "MAC_Binding",
			fmt.Sprintf("logical_port=%s", defaultGRExtPort), fmt.Sprintf("ip=\"%s\"", extIPv6))
		framework.ExpectNoError(err, "failed to look up MAC_Binding for %s ip=%s", defaultGRExtPort, extIPv6)
		for _, uuid := range strings.Fields(output) {
			_, err := runProxyOVNSBCtlCmd(f, ovnkPod, "destroy", "MAC_Binding", uuid)
			framework.ExpectNoError(err, "failed to destroy MAC_Binding %s", uuid)
		}
		Eventually(checkProxyMACBindingAbsent(f, ovnkPod, defaultGRExtPort, extIPv6),
			10*time.Second, time.Second).Should(Succeed())

		By("snapshotting default GR outbound NS flow packet count")
		nsFlowLine, err := getFlowLineForTable(ovnkPod, 0,
			"priority=102,",
			"icmp6,",
			fmt.Sprintf("in_port=%d,", defaultOfport),
			fmt.Sprintf("dl_src=%s,", grExtMAC),
			"icmp_type=135",
		)
		framework.ExpectNoError(err, "failed to find default GR outbound NS flow (priority=102,icmp6,icmp_type=135)")
		framework.Logf("default GR outbound NS flow: %s", nsFlowLine)

		countBefore, err := parseFlowPacketCount(nsFlowLine)
		framework.ExpectNoError(err, "failed to parse n_packets from NS flow")

		By("sending IPv6 traffic from default-network pod to external container to trigger NDP")
		curlCmd := fmt.Sprintf("curl -6 -s --connect-timeout 10 http://[%s]:%d/hostname", extIPv6, extPort)
		stdout, stderr, err := ExecShellInPodWithFullOutput(f, defaultNs.Name, testPod.Name, curlCmd)
		framework.ExpectNoError(err, "curl failed: stdout=%s stderr=%s", stdout, stderr)

		By("waiting for MAC_Binding to appear on default GR")
		Eventually(checkProxyMACBindingExists(f, ovnkPod, defaultGRExtPort, extIPv6, extMAC),
			macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

		By("snapshotting default GR outbound NS flow packet count after traffic")
		countAfter, err := getFlowPacketCountForTable(ovnkPod, 0,
			"priority=102,",
			"icmp6,",
			fmt.Sprintf("in_port=%d,", defaultOfport),
			fmt.Sprintf("dl_src=%s,", grExtMAC),
			"icmp_type=135",
		)
		framework.ExpectNoError(err, "failed to get NS flow count after traffic")
		delta := countAfter - countBefore
		framework.Logf("default GR outbound NS flow: before=%d after=%d delta=%d", countBefore, countAfter, delta)

		By("asserting: default GR sent at least one NS")
		Expect(delta).To(BeNumerically(">", 0),
			"default GR outbound NS flow counter should have increased after NDP resolution")

		By("checking flow action for flooding behavior")
		actions := parseFlowActions(nsFlowLine)
		framework.Logf("default GR outbound NS flow actions: %s", actions)

		By("asserting: outbound NS should NOT use NORMAL (flooding) — should use directed output")
		Expect(actions).NotTo(ContainSubstring("NORMAL"),
			"default GR outbound NS flow uses NORMAL (floods to all ports including UDN patches); "+
				"with the fix it should use directed output")

		By("structural check: verifying no priority=102 icmp6 icmp_type=135 flows exist for UDN patch ports")
		for _, u := range append([]udnPortInfo{primaryUDN}, extraUDNs...) {
			_, err := getFlowLineForTable(ovnkPod, 0,
				"priority=102,",
				"icmp6,",
				fmt.Sprintf("in_port=%d,", u.ofport),
				"icmp_type=135",
			)
			Expect(err).To(HaveOccurred(),
				"unexpected priority=102 icmp6 icmp_type=135 flow found for UDN patch port %s (ofport=%d) — "+
					"UDN GRs should use priority=100 ipv6 directed flows instead", u.patchPort, u.ofport)
		}
		framework.Logf("confirmed: no priority=102 icmp6 flows for UDN patch ports (they use directed priority=100 ipv6 flows)")
	})

	It("should not flood inbound unicast NDP NA to all patch ports", func() {
		// Problem: Inbound unicast IPv6 (including NDP NA) hits priority=50 → ct(table=1).
		//          In table=1, NDP NAs without established conntrack entries hit
		//          priority=14,icmp6,icmp_type=136 → FLOOD (sends to ALL ports).
		//          When conntrack tracks NS→NA correctly, the NA hits priority=100,ct_state=+est+trk
		//          → directed output. But the FLOOD fallback remains for unsolicited NAs and ct mismatches.
		// Scenario: Inbound unicast IPv6 (including NDP NA) hits priority=50,ipv6,dl_dst=bridgeMAC → ct(table=1,zone=64000,nat). In table=1, NDP NAs without established conntrack entries hit priority=14,icmp6,icmp_type=136 → FLOOD. FLOOD sends to all ports — every GR receives every NA. When conntrack correctly tracks NS→NA (solicited NAs), the NA hits priority=100,ct_state=+est+trk → output:<correct_port> (directed). But the priority=14 FLOOD rule remains as a blanket fallback for unsolicited NAs and ct mismatches.
		// Key difference from ARP Test 2: ARP unicast reply flood happens in table=0 via priority=10,dl_dst=bridgeMAC → output:all_patches,NORMAL. NDP NA flood happens in table=1 via priority=14,icmp6,icmp_type=136 → FLOOD. This test uses getFlowLineForTable(pod, 1, ...) and getFlowPacketCountForTable(pod, 1, ...).
		// Steps:
		// Find table=1 flow: priority=14,icmp6,icmp_type=136
		// Verify the action string
		// Assert: action should NOT be FLOOD — with fix, should be directed or removed
		// Create UDN pod, send IPv6 traffic to external container (triggers NS → NA exchange)
		// Snapshot table=1 counters before and after traffic:
		// priority=100,ct_state=+est+trk,...,actions=output:<primaryUDN.ofport> — ct-directed path
		// priority=14,icmp6,icmp_type=136 — FLOOD path
		// Log which path handled the NA and all counter deltas
		//
		// Key flows:
		// table=0: priority=50,ipv6,dl_dst=bridgeMAC → ct(table=1,zone=64000,nat)
		// table=1: priority=14,icmp6,icmp_type=136 → FLOOD           # ← the problem
		// table=1: priority=100,ct_state=+est+trk,ct_mark=0x4,ipv6 → output:3  # ← correct path
		ctx := context.TODO()

		By("verifying table=1 FLOOD rule for NDP NA exists")
		naFloodLine, err := getFlowLineForTable(ovnkPod, 1,
			"priority=14,",
			"icmp6,",
			"icmp_type=136",
		)
		framework.ExpectNoError(err, "failed to find table=1 NDP NA flood flow (priority=14,icmp6,icmp_type=136)")
		framework.Logf("table=1 NDP NA flood flow: %s", naFloodLine)

		By("checking that the flow action is FLOOD")
		actions := parseFlowActions(naFloodLine)
		framework.Logf("table=1 NDP NA flood flow actions: %s", actions)

		By("asserting: the NDP NA flood flow should NOT use FLOOD action")
		Expect(actions).NotTo(ContainSubstring("FLOOD"),
			"table=1 NDP NA flow uses FLOOD (sends to all ports); "+
				"with the fix it should use directed output or be removed")

		By("triggering NDP: creating a UDN pod and sending IPv6 traffic to external container")
		testPod := e2epod.NewAgnhostPod(f.Namespace.Name, "ndp-flow-inbound-client", nil, nil, nil)
		testPod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": targetNodeName}
		testPod, err = cs.CoreV1().Pods(f.Namespace.Name).Create(ctx, testPod, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		DeferCleanup(func() {
			_ = cs.CoreV1().Pods(f.Namespace.Name).Delete(ctx, testPod.Name, metav1.DeleteOptions{})
		})
		err = e2epod.WaitForPodRunningInNamespace(ctx, cs, testPod)
		framework.ExpectNoError(err)

		By("snapshotting table=1 counters before traffic")
		floodBefore, err := getFlowPacketCountForTable(ovnkPod, 1,
			"priority=14,",
			"icmp6,",
			"icmp_type=136",
		)
		framework.ExpectNoError(err, "failed to get NDP NA flood flow count before traffic")

		udnGRExtPort := proxyUDNGatewayRouterExtPort(primaryUDN.namespace, primaryUDN.name, targetNodeName)
		ctDirectedSubstrings := []string{
			"priority=100,",
			"ct_state=+est+trk,",
			"ipv6",
			fmt.Sprintf("actions=output:%d", primaryUDN.ofport),
		}
		ctBefore, err := getFlowPacketCountForTable(ovnkPod, 1, ctDirectedSubstrings...)
		framework.ExpectNoError(err, "failed to get ct-directed flow count before traffic")

		By("sending IPv6 traffic from UDN pod to external container")
		curlCmd := fmt.Sprintf("curl -6 -s --connect-timeout 10 http://[%s]:%d/hostname", extIPv6, extPort)
		stdout, stderr, err := ExecShellInPodWithFullOutput(f, f.Namespace.Name, testPod.Name, curlCmd)
		framework.ExpectNoError(err, "curl failed: stdout=%s stderr=%s", stdout, stderr)

		By("waiting for MAC_Binding to stabilize on UDN GR")
		Eventually(checkProxyMACBindingExists(f, ovnkPod, udnGRExtPort, extIPv6, extMAC),
			macBindingStabilizationTime, proxyPollInterval).Should(Succeed())

		By("snapshotting table=1 counters after traffic")
		floodAfter, err := getFlowPacketCountForTable(ovnkPod, 1,
			"priority=14,",
			"icmp6,",
			"icmp_type=136",
		)
		framework.ExpectNoError(err, "failed to get NDP NA flood flow count after traffic")

		ctAfter, err := getFlowPacketCountForTable(ovnkPod, 1, ctDirectedSubstrings...)
		framework.ExpectNoError(err, "failed to get ct-directed flow count after traffic")

		floodDelta := floodAfter - floodBefore
		ctDelta := ctAfter - ctBefore
		framework.Logf("table=1 after traffic: FLOOD flow before=%d after=%d delta=%d", floodBefore, floodAfter, floodDelta)
		framework.Logf("table=1 after traffic: ct-directed flow before=%d after=%d delta=%d", ctBefore, ctAfter, ctDelta)

		if floodDelta > 0 {
			framework.Logf("WARNING: priority=14 FLOOD rule fired %d times — NDP NAs were flooded to all ports", floodDelta)
		}
		if ctDelta > 0 {
			framework.Logf("conntrack correctly directed %d NDP NAs to the right port", ctDelta)
		}
	})

	It("should not produce duplicate NDP NA from multiple GRs for inbound multicast NS", func() {
		// Problem: External host sends NS to solicited-node multicast (dl_dst=33:33:ff:XX:XX:XX).
		//          Falls to priority=0 NORMAL. Floods to ALL breth0 ports.
		//          Multiple entities (default GR, host) respond with NA → duplicate NAs on the wire.
		// Scenario: External host sends NS to solicited-node multicast (dl_dst=33:33:ff:XX:XX:XX).
		// 			 Falls to priority=0 NORMAL. NORMAL floods to all breth0 ports.
		// 			 Each entity owning the target IPv6 (default GR + host) responds with NA — producing duplicate NAs on the wire.
		// Same mechanism as ARP Test 3, with NDP-specific details: tcpdump uses an ICMPv6 type 136 filter (icmp6 and ip6[40]==136 and ether src <bridgeMAC>) instead of ARP reply filter, and NS/NA counters use priority=102,icmp6,icmp_type=136 flows for the default GR (in_port=<defaultOfport>) and host (in_port=LOCAL).
		// Steps:
		// Flush external container's neighbor table for node's IPv6 (ip -6 neigh flush <nodeIPv6>)
		// Start tcpdump on the physical interface (eth0) for NDP NAs from bridgeMAC: filter ndpNAFilterForMAC(grExtMAC), flags -n -vv -l
		// Snapshot n_packets on priority=102,icmp6,in_port=<defaultOfport>,dl_src=<bridgeMAC>,icmp_type=136 (default GR outbound NA)
		// Also snapshot for in_port=LOCAL (host outbound NA)
		// Ping the node from the external container via IPv6 (ping -6 -c 3 -W 2 <nodeIPv6>)
		// Read tcpdump logs, count NDP NAs using countNDPPackets(output, nodeIPv6)
		// Snapshot counters again, calculate deltas
		// Log NA count on wire and all flow deltas
		// Assert: exactly 1 NDP NA should appear on the wire (not 2+ duplicates)
		// Assert: only 1 responder counter should have incremented (default GR or host, not both)
		//
		// Key flows:
		// table=0: priority=102,icmp6,in_port=2,dl_src=bridgeMAC,icmp_type=136 → ct(commit,...),NORMAL  # default GR outbound NA
		// table=0: priority=102,icmp6,in_port=LOCAL,dl_src=bridgeMAC,icmp_type=136 → ct(commit,...),NORMAL  # host outbound NA
		// table=0: priority=0 → NORMAL  # catch-all floods inbound NS to all ports

		By("flushing external container's neighbor table for the node IPv6")
		_, err := infraprovider.Get().ExecExternalContainerCommand(extContainer, []string{
			"ip", "-6", "neigh", "flush", nodeIPv6,
		})
		framework.ExpectNoError(err, "failed to flush neighbor for %s on external container", nodeIPv6)

		By("starting tcpdump on physical interface for NDP NAs from bridgeMAC")
		ndpFilter := ndpNAFilterForMAC(grExtMAC)
		monitorCtx, monitorCancel := context.WithCancel(context.Background())
		defer monitorCancel()
		brex := deploymentconfig.Get().ExternalBridgeName()
		briface := strings.Trim(brex, "br")
		quotedFilter := "'" + strings.ReplaceAll(ndpFilter, "'", `'"'"'`) + "'"
		startTcpdumpMonitorPodOnNode(monitorCtx, f, proxyCmdRetryTimeout, ndpFlowCounterMonitorPod,
			targetNodeName, briface, "-n -vv -l", quotedFilter)
		framework.Logf("tcpdump monitor started on node %s interface %s with filter: %s", targetNodeName, briface, ndpFilter)

		By("snapshotting default GR and host outbound NA flow counters before ping")
		defaultNABefore, err := getFlowPacketCountForTable(ovnkPod, 0,
			"priority=102,",
			"icmp6,",
			fmt.Sprintf("in_port=%d,", defaultOfport),
			fmt.Sprintf("dl_src=%s,", grExtMAC),
			"icmp_type=136",
		)
		framework.ExpectNoError(err, "failed to get default GR outbound NA count before")
		localNABefore, err := getFlowPacketCountForTable(ovnkPod, 0,
			"priority=102,",
			"icmp6,",
			"in_port=LOCAL,",
			fmt.Sprintf("dl_src=%s,", grExtMAC),
			"icmp_type=136",
		)
		framework.ExpectNoError(err, "failed to get host (LOCAL) outbound NA count before")
		framework.Logf("before ping: default GR NA=%d, host NA=%d", defaultNABefore, localNABefore)

		By("pinging the node from the external container via IPv6 (triggers NDP NS → NA exchange)")
		pingOutput, err := infraprovider.Get().ExecExternalContainerCommand(extContainer, []string{
			"ping", "-6", "-c", "3", "-W", "2", nodeIPv6,
		})
		framework.ExpectNoError(err, "ping from external container to node %s failed: %s", nodeIPv6, pingOutput)

		By("reading tcpdump logs and counting NDP NAs")
		monitorOutput, err := e2ekubectl.RunKubectl(f.Namespace.Name, "logs", ndpFlowCounterMonitorPod)
		framework.ExpectNoError(err, "failed to read tcpdump monitor logs")
		_, ndpNAs := countNDPPackets(monitorOutput, nodeIPv6)
		framework.Logf("tcpdump captured %d NDP NAs for node IPv6 %s", ndpNAs, nodeIPv6)
		framework.Logf("tcpdump output:\n%s", monitorOutput)

		By("snapshotting default GR and host outbound NA flow counters after ping")
		defaultNAAfter, err := getFlowPacketCountForTable(ovnkPod, 0,
			"priority=102,",
			"icmp6,",
			fmt.Sprintf("in_port=%d,", defaultOfport),
			fmt.Sprintf("dl_src=%s,", grExtMAC),
			"icmp_type=136",
		)
		framework.ExpectNoError(err, "failed to get default GR outbound NA count after")
		localNAAfter, err := getFlowPacketCountForTable(ovnkPod, 0,
			"priority=102,",
			"icmp6,",
			"in_port=LOCAL,",
			fmt.Sprintf("dl_src=%s,", grExtMAC),
			"icmp_type=136",
		)
		framework.ExpectNoError(err, "failed to get host (LOCAL) outbound NA count after")

		defaultNADelta := defaultNAAfter - defaultNABefore
		localNADelta := localNAAfter - localNABefore
		framework.Logf("after ping: default GR NA=%d (delta=%d), host NA=%d (delta=%d)",
			defaultNAAfter, defaultNADelta, localNAAfter, localNADelta)

		respondersWithDelta := 0
		if defaultNADelta > 0 {
			respondersWithDelta++
		}
		if localNADelta > 0 {
			respondersWithDelta++
		}

		By("asserting: exactly 1 NDP NA should appear on the wire (not 2+ duplicates)")
		Expect(ndpNAs).To(BeNumerically("==", 1),
			"expected exactly 1 NDP NA for node IPv6 %s, got %d — multiple entities are generating duplicate NAs",
			nodeIPv6, ndpNAs)

		By("asserting: only 1 responder (default GR or host) should have sent an NA")
		Expect(respondersWithDelta).To(BeNumerically("==", 1),
			"expected only 1 entity to emit an NDP NA, but %d had counter increases (default GR delta=%d, host delta=%d)",
			respondersWithDelta, defaultNADelta, localNADelta)
	})
})

// --- Helper types and functions ---

type proxyOVNKubePodRef struct {
	namespace string
	name      string
}

func getProxyOVNKubeNodePod(cs clientset.Interface, ovnNs, nodeName string) proxyOVNKubePodRef {
	pods, err := cs.CoreV1().Pods(ovnNs).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	framework.ExpectNoError(err, "failed to list ovnkube-node pods on %s", nodeName)
	Expect(pods.Items).NotTo(BeEmpty(), "no ovnkube-node pod found on node %s", nodeName)
	return proxyOVNKubePodRef{namespace: ovnNs, name: pods.Items[0].Name}
}

func runProxyOVNSBCtlCmd(f *framework.Framework, pod proxyOVNKubePodRef, args ...string) (string, error) {
	cmd := append([]string{"ovn-sbctl"}, args...)
	stdout, stderr, err := ExecCommandInContainerWithFullOutput(f, pod.namespace, pod.name, "sb-ovsdb", cmd...)
	if err != nil {
		return "", fmt.Errorf("ovn-sbctl %v failed: %v (stderr: %s)", args, err, stderr)
	}
	return strings.TrimSpace(stdout), nil
}

func runProxyOVSOfctlCmd(pod proxyOVNKubePodRef, args string) (string, error) {
	cmd := fmt.Sprintf("ovs-ofctl %s", args)
	stdout, err := e2epodoutput.RunHostCmdWithRetries(pod.namespace, pod.name, cmd, framework.Poll, proxyCmdRetryTimeout)
	if err != nil {
		return "", fmt.Errorf("ovs-ofctl command failed: %s: %w", cmd, err)
	}
	return strings.TrimSpace(stdout), nil
}

func verifyGRPortNoFlood(f *framework.Framework, pod proxyOVNKubePodRef, portName string) {
	cmd := []string{"ovn-nbctl", "get", "logical_router_port", portName, "options"}
	stdout, stderr, err := ExecCommandInContainerWithFullOutput(f, pod.namespace, pod.name, "nb-ovsdb", cmd...)
	framework.ExpectNoError(err, "failed to get options for port %s: stderr=%s", portName, stderr)
	Expect(stdout).To(ContainSubstring("no-flood"),
		"GR external port %s should have flood-related options set, got: %s", portName, stdout)
}

func checkProxyMACBindingExists(f *framework.Framework, pod proxyOVNKubePodRef, logicalPort, ip, mac string) func() error {
	return func() error {
		output, err := runProxyOVNSBCtlCmd(f, pod, "--columns=ip,mac", "--no-headings", "find", "MAC_Binding",
			fmt.Sprintf("logical_port=%s", logicalPort), fmt.Sprintf("ip=\"%s\"", ip))
		if err != nil {
			return err
		}
		if output == "" {
			return fmt.Errorf("MAC_Binding entry not found for port=%s ip=%s", logicalPort, ip)
		}
		if !strings.Contains(strings.ToLower(output), strings.ToLower(mac)) {
			return fmt.Errorf("MAC_Binding for port=%s ip=%s does not contain MAC %s, got: %s", logicalPort, ip, mac, output)
		}
		return nil
	}
}

func checkProxyMACBindingAbsent(f *framework.Framework, pod proxyOVNKubePodRef, logicalPort, ip string) func() error {
	return func() error {
		output, err := runProxyOVNSBCtlCmd(f, pod, "--columns=ip,mac", "--no-headings", "find", "MAC_Binding",
			fmt.Sprintf("logical_port=%s", logicalPort), fmt.Sprintf("ip=\"%s\"", ip))
		if err != nil {
			return err
		}
		if output != "" {
			return fmt.Errorf("MAC_Binding entry still exists for port=%s ip=%s: %s", logicalPort, ip, output)
		}
		return nil
	}
}

func checkProxyNoMACBindingsForIP(f *framework.Framework, pod proxyOVNKubePodRef, ip string) func() error {
	return func() error {
		output, err := runProxyOVNSBCtlCmd(f, pod, "--columns=logical_port,ip,mac", "--no-headings", "find", "MAC_Binding",
			fmt.Sprintf("ip=\"%s\"", ip))
		if err != nil {
			return err
		}
		if output != "" {
			return fmt.Errorf("MAC_Binding entries still exist for ip=%s: %s", ip, output)
		}
		return nil
	}
}

func checkProxyARPResponderFlowExists(pod proxyOVNKubePodRef, ip string) func() error {
	return func() error {
		brex := deploymentconfig.Get().ExternalBridgeName()
		output, err := runProxyOVSOfctlCmd(pod, fmt.Sprintf("dump-flows %s table=0", brex))
		if err != nil {
			return err
		}
		for _, line := range strings.Split(output, "\n") {
			if strings.Contains(line, "priority=40") && strings.Contains(line, ip) && strings.Contains(line, "arp") {
				return nil
			}
		}
		return fmt.Errorf("ARP responder flow (priority=40) for ip=%s not found on %s", ip, brex)
	}
}

func checkProxyKernelARPEntry(nodeName, ip, mac string) func() error {
	return func() error {
		output, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "neigh", "show", ip})
		if err != nil {
			return fmt.Errorf("failed to query ARP table on node %s for ip=%s: %w", nodeName, ip, err)
		}
		if !strings.Contains(strings.ToLower(output), strings.ToLower(mac)) {
			return fmt.Errorf("kernel ARP entry for ip=%s on node %s does not contain MAC %s, got: %s", ip, nodeName, mac, output)
		}
		return nil
	}
}

// proxyUDNGatewayRouterExtPort returns the NB-DB logical router port name for
// the external port of the UDN's gateway router on the given node.
// Mirrors GenerateUDNNetworkName (namespace + "_" + udnName) followed by
// GetUserDefinedNetworkPrefix (replace "-" and "/" with ".").
func proxyUDNGatewayRouterExtPort(namespace, udnName, nodeName string) string {
	networkName := namespace + "_" + udnName
	sanitized := strings.ReplaceAll(networkName, "-", ".")
	sanitized = strings.ReplaceAll(sanitized, "/", ".")
	return fmt.Sprintf("rtoe-GR_%s_%s", sanitized, nodeName)
}

// arpReplyFilterForMACs builds a BPF filter matching ARP reply packets (opcode=2)
// where the sender hardware address (bytes 8-13) equals senderMAC and the
// target hardware address (bytes 18-23) equals targetMAC.
// ARP layout: [8:4]+[12:2] = sender MAC, [18:4]+[22:2] = target MAC.
func arpReplyFilterForMACs(sMAC, tMAC string) (string, error) {
	senderMAC, err := net.ParseMAC(sMAC)
	if err != nil {
		return "", err
	}

	targetMAC, err := net.ParseMAC(tMAC)
	if err != nil {
		return "", err
	}

	if len(senderMAC) != 6 || len(targetMAC) != 6 {
		return "", fmt.Errorf("MACs have wrong len")
	}
	return fmt.Sprintf("arp[6:2] == 2 and arp[8:4] == 0x%02x%02x%02x%02x and arp[12:2] == 0x%02x%02x and arp[18:4] == 0x%02x%02x%02x%02x and arp[22:2] == 0x%02x%02x",
		senderMAC[0], senderMAC[1], senderMAC[2], senderMAC[3], senderMAC[4], senderMAC[5],
		targetMAC[0], targetMAC[1], targetMAC[2], targetMAC[3], targetMAC[4], targetMAC[5]), nil
}

func getProxyGRExtPortMAC(f *framework.Framework, pod proxyOVNKubePodRef, portName string) string {
	cmd := []string{"ovn-nbctl", "--no-headings", "get", "logical_router_port", portName, "mac"}
	stdout, stderr, err := ExecCommandInContainerWithFullOutput(f, pod.namespace, pod.name, "nb-ovsdb", cmd...)
	framework.ExpectNoError(err, "failed to get MAC for port %s: stderr=%s", portName, stderr)
	return strings.Trim(strings.TrimSpace(stdout), "\"")
}

func countARPPackets(output, ip string) (requests int, replies int) {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "Request") && strings.Contains(line, ip) {
			requests++
		}
		if strings.Contains(line, "Reply") && strings.Contains(line, ip) {
			replies++
		}
	}
	return
}

func getBrexPatchPortName(nodeName string) string {
	brex := deploymentconfig.Get().ExternalBridgeName()
	return fmt.Sprintf("patch-%s_%s-to-br-int", brex, nodeName)
}

func getBrexUDNPatchPortName(namespace, udnName, nodeName string) string {
	brex := deploymentconfig.Get().ExternalBridgeName()
	networkName := namespace + "_" + udnName
	sanitized := strings.ReplaceAll(networkName, "-", ".")
	sanitized = strings.ReplaceAll(sanitized, "/", ".")
	return fmt.Sprintf("patch-%s_%s_%s-to-br-int", brex, sanitized, nodeName)
}

func getPatchPortOfport(pod proxyOVNKubePodRef, portName string) (int, error) {
	cmd := fmt.Sprintf("ovs-vsctl get Interface %s ofport", portName)
	stdout, err := e2epodoutput.RunHostCmdWithRetries(pod.namespace, pod.name, cmd, framework.Poll, proxyCmdRetryTimeout)
	if err != nil {
		return -1, fmt.Errorf("failed to get ofport for %s: %w", portName, err)
	}
	return strconv.Atoi(strings.TrimSpace(stdout))
}

func getFlowLineForTable(pod proxyOVNKubePodRef, table int, matchSubstrings ...string) (string, error) {
	brex := deploymentconfig.Get().ExternalBridgeName()
	output, err := runProxyOVSOfctlCmd(pod, fmt.Sprintf("dump-flows %s table=%d", brex, table))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		allMatch := true
		for _, sub := range matchSubstrings {
			if !strings.Contains(line, sub) {
				allMatch = false
				break
			}
		}
		if allMatch {
			return line, nil
		}
	}
	return "", fmt.Errorf("no flow matching %v found in %s table=%d", matchSubstrings, brex, table)
}

func parseFlowPacketCount(flowLine string) (int64, error) {
	const prefix = "n_packets="
	idx := strings.Index(flowLine, prefix)
	if idx == -1 {
		return -1, fmt.Errorf("n_packets not found in flow line: %s", flowLine)
	}
	rest := flowLine[idx+len(prefix):]
	end := strings.IndexAny(rest, ", ")
	if end == -1 {
		end = len(rest)
	}
	return strconv.ParseInt(rest[:end], 10, 64)
}

func getFlowPacketCountForTable(pod proxyOVNKubePodRef, table int, matchSubstrings ...string) (int64, error) {
	line, err := getFlowLineForTable(pod, table, matchSubstrings...)
	if err != nil {
		return -1, err
	}
	return parseFlowPacketCount(line)
}

func getPatchEgressARPCount(pod proxyOVNKubePodRef, bridgeMAC string, ofport int) (int64, error) {
	return getFlowPacketCountForTable(pod, 0,
		"priority=10,",
		fmt.Sprintf("in_port=%d,", ofport),
		fmt.Sprintf("dl_src=%s", bridgeMAC),
	)
}

func parseFlowActions(flowLine string) string {
	const prefix = "actions="
	idx := strings.Index(flowLine, prefix)
	if idx == -1 {
		return ""
	}
	return flowLine[idx+len(prefix):]
}

func countOutputActions(actions string) int {
	return strings.Count(actions, "output:")
}

func countNDPPackets(output, targetIP string) (solicitations int, advertisements int) {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "neighbor sol") && strings.Contains(line, targetIP) {
			solicitations++
		}
		if strings.Contains(line, "neighbor adv") && strings.Contains(line, targetIP) {
			advertisements++
		}
	}
	return
}

func ndpNAFilterForMAC(senderMAC string) string {
	return fmt.Sprintf("icmp6 and ip6[40]==136 and ether src %s", senderMAC)
}
