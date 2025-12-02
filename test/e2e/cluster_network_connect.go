package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"
)

const (
	// Annotation keys used by the CNC controller
	ovnNetworkConnectSubnetAnnotation   = "k8s.ovn.org/network-connect-subnet"
	ovnConnectRouterTunnelKeyAnnotation = "k8s.ovn.org/connect-router-tunnel-key"
)

// cncAnnotationSubnet represents the subnet annotation structure
type cncAnnotationSubnet struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

var _ = Describe("ClusterNetworkConnect ClusterManagerController", feature.NetworkSegmentation, func() {
	f := wrappedTestFramework("cnc-controller")
	// disable automatic namespace creation, we need to add the required UDN label
	f.SkipNamespaceCreation = true

	var (
		cs clientset.Interface
	)

	const (
		cncConnectSubnetIPv4CIDR   = "192.168.0.0/16"
		cncConnectSubnetIPv4Prefix = 24
		cncConnectSubnetIPv6CIDR   = "fd00:10::/48"
		cncConnectSubnetIPv6Prefix = 64
		// Layer3 UDN CIDRs with hostSubnet (IPv4: /24, IPv6: /64)
		layer3UserDefinedNetworkIPv4CIDR       = "172.31.0.0/16"
		layer3UserDefinedNetworkIPv4HostSubnet = 24
		layer3UserDefinedNetworkIPv6CIDR       = "2014:100:200::0/60"
		layer3UserDefinedNetworkIPv6HostSubnet = 64
		// Layer2 UDN CIDRs
		layer2UserDefinedNetworkIPv4CIDR = "10.200.0.0/16"
		layer2UserDefinedNetworkIPv6CIDR = "2015:100:200::0/60"
	)

	BeforeEach(func() {
		cs = f.ClientSet
	})

	// Helper to generate connectSubnets YAML based on cluster IP family support
	generateConnectSubnets := func() string {
		var subnets []string
		if isIPv4Supported(cs) {
			subnets = append(subnets, fmt.Sprintf(`    - cidr: "%s"
      networkPrefix: %d`, cncConnectSubnetIPv4CIDR, cncConnectSubnetIPv4Prefix))
		}
		if isIPv6Supported(cs) {
			subnets = append(subnets, fmt.Sprintf(`    - cidr: "%s"
      networkPrefix: %d`, cncConnectSubnetIPv6CIDR, cncConnectSubnetIPv6Prefix))
		}
		return strings.Join(subnets, "\n")
	}

	// Helper to create a namespace with UDN label
	createUDNNamespace := func(baseName string, labels map[string]string) *corev1.Namespace {
		if labels == nil {
			labels = map[string]string{}
		}
		labels[RequiredUDNNamespaceLabel] = ""
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   baseName + "-" + rand.String(5),
				Labels: labels,
			},
		}
		createdNs, err := cs.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		return createdNs
	}

	// Helper to generate a random CNC name
	generateCNCName := func() string {
		return fmt.Sprintf("test-cnc-%s", rand.String(5))
	}

	// Helper to create a CNC with CUDN selector
	createCNCWithCUDNSelector := func(cncName string, labelSelector map[string]string) {
		labelSelectorStr := ""
		for k, v := range labelSelector {
			if labelSelectorStr != "" {
				labelSelectorStr += "\n            "
			}
			labelSelectorStr += fmt.Sprintf("%s: %s", k, v)
		}
		manifest := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: %s
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            %s
  connectSubnets:
%s
  connectivity: ["PodNetwork"]
`, cncName, labelSelectorStr, generateConnectSubnets())
		_, err := e2ekubectl.RunKubectlInput("", manifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
	}

	// Helper to create a CNC with PUDN selector
	createCNCWithPUDNSelector := func(cncName string, labelSelector map[string]string) {
		labelSelectorStr := ""
		for k, v := range labelSelector {
			if labelSelectorStr != "" {
				labelSelectorStr += "\n            "
			}
			labelSelectorStr += fmt.Sprintf("%s: %s", k, v)
		}
		manifest := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: %s
spec:
  networkSelectors:
    - networkSelectionType: "PrimaryUserDefinedNetworks"
      primaryUserDefinedNetworkSelector:
        namespaceSelector:
          matchLabels:
            %s
  connectSubnets:
%s
  connectivity: ["PodNetwork"]
`, cncName, labelSelectorStr, generateConnectSubnets())
		_, err := e2ekubectl.RunKubectlInput("", manifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
	}

	// Helper to create a CNC with both CUDN and PUDN selectors
	createCNCWithBothSelectors := func(cncName string, cudnLabelSelector, pudnLabelSelector map[string]string) {
		cudnLabelSelectorStr := ""
		for k, v := range cudnLabelSelector {
			if cudnLabelSelectorStr != "" {
				cudnLabelSelectorStr += "\n            "
			}
			cudnLabelSelectorStr += fmt.Sprintf("%s: %s", k, v)
		}
		pudnLabelSelectorStr := ""
		for k, v := range pudnLabelSelector {
			if pudnLabelSelectorStr != "" {
				pudnLabelSelectorStr += "\n            "
			}
			pudnLabelSelectorStr += fmt.Sprintf("%s: %s", k, v)
		}
		manifest := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: %s
spec:
  networkSelectors:
    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            %s
    - networkSelectionType: "PrimaryUserDefinedNetworks"
      primaryUserDefinedNetworkSelector:
        namespaceSelector:
          matchLabels:
            %s
  connectSubnets:
%s
  connectivity: ["PodNetwork"]
`, cncName, cudnLabelSelectorStr, pudnLabelSelectorStr, generateConnectSubnets())
		_, err := e2ekubectl.RunKubectlInput("", manifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
	}

	// Helper to generate subnets YAML based on topology and cluster IP family support
	// Layer3 uses [{cidr: "...", hostSubnet: N}] format, Layer2 uses ["..."] format
	generateNetworkSubnets := func(topology string) string {
		if topology == "Layer3" {
			// Layer3 format: [{cidr: "10.0.0.0/16", hostSubnet: 24}, {cidr: "fd00::/60", hostSubnet: 64}]
			var subnets []string
			if isIPv4Supported(cs) {
				subnets = append(subnets, fmt.Sprintf(`{cidr: "%s", hostSubnet: %d}`, layer3UserDefinedNetworkIPv4CIDR, layer3UserDefinedNetworkIPv4HostSubnet))
			}
			if isIPv6Supported(cs) {
				subnets = append(subnets, fmt.Sprintf(`{cidr: "%s", hostSubnet: %d}`, layer3UserDefinedNetworkIPv6CIDR, layer3UserDefinedNetworkIPv6HostSubnet))
			}
			return fmt.Sprintf("[%s]", strings.Join(subnets, ","))
		}
		// Layer2 format: ["10.0.0.0/16", "fd00::/60"]
		var quotedCidrs []string
		if isIPv4Supported(cs) {
			quotedCidrs = append(quotedCidrs, fmt.Sprintf(`"%s"`, layer2UserDefinedNetworkIPv4CIDR))
		}
		if isIPv6Supported(cs) {
			quotedCidrs = append(quotedCidrs, fmt.Sprintf(`"%s"`, layer2UserDefinedNetworkIPv6CIDR))
		}
		return fmt.Sprintf("[%s]", strings.Join(quotedCidrs, ","))
	}

	// Helper to create a primary CUDN with specified topology
	createPrimaryCUDN := func(cudnName, topology string, labels map[string]string, targetNamespaces ...string) {
		targetNs := strings.Join(targetNamespaces, ",")
		labelAnnotations := ""
		for k, v := range labels {
			if labelAnnotations != "" {
				labelAnnotations += "\n    "
			}
			labelAnnotations += fmt.Sprintf("%s: %s", k, v)
		}
		topologyLower := strings.ToLower(topology)
		manifest := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: %s
  labels:
    %s
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: [ %s ]
  network:
    topology: %s
    %s:
      role: Primary
      subnets: %s
`, cudnName, labelAnnotations, targetNs, topology, topologyLower, generateNetworkSubnets(topology))
		_, err := e2ekubectl.RunKubectlInput("", manifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
	}

	// Helper to create a primary CUDN (Layer3 - default)
	createLayer3PrimaryCUDN := func(cudnName string, labels map[string]string, targetNamespaces ...string) {
		createPrimaryCUDN(cudnName, "Layer3", labels, targetNamespaces...)
	}

	// Helper to create a Layer2 primary CUDN
	createLayer2PrimaryCUDN := func(cudnName string, labels map[string]string, targetNamespaces ...string) {
		createPrimaryCUDN(cudnName, "Layer2", labels, targetNamespaces...)
	}

	// Helper to create a primary UDN with specified topology
	createPrimaryUDN := func(namespace, udnName, topology string) {
		topologyLower := strings.ToLower(topology)
		manifest := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: %s
spec:
  topology: %s
  %s:
    role: Primary
    subnets: %s
`, udnName, topology, topologyLower, generateNetworkSubnets(topology))
		_, err := e2ekubectl.RunKubectlInput(namespace, manifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
	}

	// Helper to create a primary UDN (Layer3 - default)
	createLayer3PrimaryUDN := func(namespace, udnName string) {
		createPrimaryUDN(namespace, udnName, "Layer3")
	}

	// Helper to create a Layer2 primary UDN
	createLayer2PrimaryUDN := func(namespace, udnName string) {
		createPrimaryUDN(namespace, udnName, "Layer2")
	}

	// Helper to delete a CNC
	deleteCNC := func(cncName string) {
		_, _ = e2ekubectl.RunKubectl("", "delete", "clusternetworkconnect", cncName, "--ignore-not-found")
	}

	// Helper to delete a CUDN
	deleteCUDN := func(cudnName string) {
		_, _ = e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", "--timeout=60s", "--ignore-not-found")
	}

	// Helper to delete a UDN
	deleteUDN := func(namespace, udnName string) {
		_, _ = e2ekubectl.RunKubectl(namespace, "delete", "userdefinednetwork", udnName, "--wait", "--timeout=60s", "--ignore-not-found")
	}

	// Helper to get CNC annotations
	getCNCAnnotations := func(cncName string) (map[string]string, error) {
		annotationsJSON, err := e2ekubectl.RunKubectl("", "get", "clusternetworkconnect", cncName, "-o", "jsonpath={.metadata.annotations}")
		if err != nil {
			return nil, err
		}
		if annotationsJSON == "" {
			return map[string]string{}, nil
		}
		var annotations map[string]string
		if err := json.Unmarshal([]byte(annotationsJSON), &annotations); err != nil {
			return nil, err
		}
		return annotations, nil
	}

	// Helper to verify CNC has only tunnel ID annotation
	verifyCNCHasOnlyTunnelIDAnnotation := func(cncName string) {
		Eventually(func(g Gomega) {
			annotations, err := getCNCAnnotations(cncName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(annotations).To(HaveKey(ovnConnectRouterTunnelKeyAnnotation), "CNC should have tunnel ID annotation")
			// Subnet annotation should either not exist or be empty
			if subnetAnnotation, exists := annotations[ovnNetworkConnectSubnetAnnotation]; exists {
				g.Expect(subnetAnnotation).To(Equal("{}"), "subnet annotation should be empty when no networks match")
			}
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	}

	// Helper to verify CNC has both annotations
	verifyCNCHasBothAnnotations := func(cncName string) {
		Eventually(func(g Gomega) {
			annotations, err := getCNCAnnotations(cncName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(annotations).To(HaveKey(ovnConnectRouterTunnelKeyAnnotation), "CNC should have tunnel ID annotation")
			g.Expect(annotations).To(HaveKey(ovnNetworkConnectSubnetAnnotation), "CNC should have subnet annotation")
			// Verify subnet annotation is not empty
			subnetAnnotation := annotations[ovnNetworkConnectSubnetAnnotation]
			g.Expect(subnetAnnotation).NotTo(Equal("{}"), "subnet annotation should not be empty when networks match")
			// Parse and verify it contains valid subnet data
			var subnets map[string]cncAnnotationSubnet
			err = json.Unmarshal([]byte(subnetAnnotation), &subnets)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(len(subnets)).To(BeNumerically(">", 0), "should have at least one network subnet")
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	}

	// Helper to verify CNC subnet annotation count
	verifyCNCSubnetAnnotationNetworkCount := func(cncName string, expectedCount int) {
		Eventually(func(g Gomega) {
			annotations, err := getCNCAnnotations(cncName)
			g.Expect(err).NotTo(HaveOccurred())
			subnetAnnotation := annotations[ovnNetworkConnectSubnetAnnotation]
			var subnets map[string]cncAnnotationSubnet
			err = json.Unmarshal([]byte(subnetAnnotation), &subnets)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(len(subnets)).To(Equal(expectedCount), fmt.Sprintf("should have %d network subnets", expectedCount))
		}, 60*time.Second, 2*time.Second).Should(Succeed())
	}

	Context("CNC Annotation Tests", func() {

		It("1. CNC created with 0 matching networks only has tunnel ID annotation", func() {
			cncName := generateCNCName()
			DeferCleanup(func() {
				deleteCNC(cncName)
			})

			By("creating a CNC with selector that matches no networks")
			createCNCWithCUDNSelector(cncName, map[string]string{"nonexistent": "label"})

			By("verifying CNC has only tunnel ID annotation")
			verifyCNCHasOnlyTunnelIDAnnotation(cncName)
		})

		It("2. CNC created with matching P-UDNs has both subnet and tunnel ID annotations", func() {
			cncName := generateCNCName()
			testLabel := map[string]string{"test-udn": "true"}
			ns := createUDNNamespace("test-udn", testLabel)
			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteUDN(ns.Name, "test-udn")
				cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
			})

			By("creating a Layer3 primary UDN in the namespace")
			createLayer3PrimaryUDN(ns.Name, "test-udn")

			By("waiting for UDN to be ready")
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns.Name, "test-udn"), 30*time.Second, time.Second).Should(Succeed())

			By("creating a CNC with PUDN selector matching the namespace")
			createCNCWithPUDNSelector(cncName, testLabel)

			By("verifying CNC has both subnet and tunnel ID annotations")
			verifyCNCHasBothAnnotations(cncName)
		})

		It("3. CNC created with matching P-CUDNs has both subnet and tunnel ID annotations", func() {
			cncName := generateCNCName()
			cudnName := fmt.Sprintf("test-cudn-%s", rand.String(5))
			testLabel := map[string]string{"test-cudn": "true"}
			ns := createUDNNamespace("test-cudn", nil)
			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
			})

			By("creating a Layer3 primary CUDN")
			createLayer3PrimaryCUDN(cudnName, testLabel, ns.Name)

			By("waiting for CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating a CNC with CUDN selector matching the CUDN")
			createCNCWithCUDNSelector(cncName, testLabel)

			By("verifying CNC has both subnet and tunnel ID annotations")
			verifyCNCHasBothAnnotations(cncName)
		})

		It("4. CNC created with both P-UDNs and P-CUDNs has both subnet and tunnel ID annotations", func() {
			cncName := generateCNCName()
			cudnName := fmt.Sprintf("test-cudn-%s", rand.String(5))
			cudnLabel := map[string]string{"test-cudn-multi": "true"}
			pudnLabel := map[string]string{"test-udn-multi": "true"}

			cudnNs := createUDNNamespace("test-cudn-multi", nil)
			udnNs := createUDNNamespace("test-udn-multi", pudnLabel)

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName)
				deleteUDN(udnNs.Name, "test-udn")
				cs.CoreV1().Namespaces().Delete(context.Background(), cudnNs.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), udnNs.Name, metav1.DeleteOptions{})
			})

			By("creating a Layer3 primary CUDN")
			createLayer3PrimaryCUDN(cudnName, cudnLabel, cudnNs.Name)

			By("waiting for CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating a Layer2 primary UDN")
			createLayer2PrimaryUDN(udnNs.Name, "test-udn")

			By("waiting for UDN to be ready")
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, udnNs.Name, "test-udn"), 30*time.Second, time.Second).Should(Succeed())

			By("creating a CNC with both CUDN and PUDN selectors")
			createCNCWithBothSelectors(cncName, cudnLabel, pudnLabel)

			By("verifying CNC has both subnet and tunnel ID annotations")
			verifyCNCHasBothAnnotations(cncName)

			By("verifying CNC has 2 networks in subnet annotation")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
		})

		It("5. CNC annotations are updated when network selector stops matching one of two networks", func() {
			cncName := generateCNCName()
			cudn1Name := fmt.Sprintf("test-cudn1-%s", rand.String(5))
			cudn2Name := fmt.Sprintf("test-cudn2-%s", rand.String(5))
			// Both CUDNs share a common label for initial selection, but have unique labels too
			commonLabel := map[string]string{"cnc-selector-test": "true"}
			cudn1UniqueLabel := map[string]string{"cnc-selector-test": "true", "cudn": "first"}
			cudn2UniqueLabel := map[string]string{"cnc-selector-test": "true", "cudn": "second"}
			ns1 := createUDNNamespace("test-selector1", nil)
			ns2 := createUDNNamespace("test-selector2", nil)

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudn1Name)
				deleteCUDN(cudn2Name)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating first Layer3 primary CUDN with matching labels")
			createLayer3PrimaryCUDN(cudn1Name, cudn1UniqueLabel, ns1.Name)

			By("waiting for first CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudn1Name), 30*time.Second, time.Second).Should(Succeed())

			By("creating second Layer2 primary CUDN with matching labels")
			createLayer2PrimaryCUDN(cudn2Name, cudn2UniqueLabel, ns2.Name)

			By("waiting for second CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudn2Name), 30*time.Second, time.Second).Should(Succeed())

			By("creating a CNC with CUDN selector matching both CUDNs")
			createCNCWithCUDNSelector(cncName, commonLabel)

			By("verifying CNC has both annotations with 2 networks")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)

			By("updating CNC selector to only match the first CUDN")
			// Update the CNC to use a more specific selector that only matches the first CUDN
			createCNCWithCUDNSelector(cncName, map[string]string{"cudn": "first"})

			By("verifying CNC annotations are updated to only have 1 network")
			Eventually(func(g Gomega) {
				annotations, err := getCNCAnnotations(cncName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(annotations).To(HaveKey(ovnConnectRouterTunnelKeyAnnotation))
				g.Expect(annotations).To(HaveKey(ovnNetworkConnectSubnetAnnotation))
				subnetAnnotation := annotations[ovnNetworkConnectSubnetAnnotation]
				g.Expect(subnetAnnotation).NotTo(Equal("{}"), "subnet annotation should not be empty")
				var subnets map[string]cncAnnotationSubnet
				err = json.Unmarshal([]byte(subnetAnnotation), &subnets)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(subnets)).To(Equal(1), "should have exactly 1 network subnet after unmatching one CUDN")
			}, 60*time.Second, 2*time.Second).Should(Succeed())
		})

		It("6. CNC annotations are updated when network selector starts matching", func() {
			cncName := generateCNCName()
			cudnName := fmt.Sprintf("test-cudn-%s", rand.String(5))
			testLabel := map[string]string{"cnc-start-match": "true"}
			ns := createUDNNamespace("test-start-match", nil)

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
			})

			By("creating a CNC with CUDN selector (no matching CUDNs yet)")
			createCNCWithCUDNSelector(cncName, testLabel)

			By("verifying CNC has only tunnel ID annotation initially")
			verifyCNCHasOnlyTunnelIDAnnotation(cncName)

			By("creating a Layer3 primary CUDN that matches the selector")
			createLayer3PrimaryCUDN(cudnName, testLabel, ns.Name)

			By("waiting for CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())

			By("verifying CNC annotations are updated to include subnet")
			verifyCNCHasBothAnnotations(cncName)
		})

		It("7. CNC annotations are updated when a 3rd network starts matching (already has 2)", func() {
			cncName := generateCNCName()
			cudn1Name := fmt.Sprintf("test-cudn1-%s", rand.String(5))
			cudn2Name := fmt.Sprintf("test-cudn2-%s", rand.String(5))
			cudn3Name := fmt.Sprintf("test-cudn3-%s", rand.String(5))
			testLabel := map[string]string{"cnc-add-third": "true"}
			ns1 := createUDNNamespace("test-add-third1", nil)
			ns2 := createUDNNamespace("test-add-third2", nil)
			ns3 := createUDNNamespace("test-add-third3", nil)

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudn1Name)
				deleteCUDN(cudn2Name)
				deleteCUDN(cudn3Name)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns3.Name, metav1.DeleteOptions{})
			})

			By("creating Layer3 and Layer2 primary CUDNs with matching labels")
			createLayer3PrimaryCUDN(cudn1Name, testLabel, ns1.Name)
			createLayer2PrimaryCUDN(cudn2Name, testLabel, ns2.Name)

			By("waiting for CUDNs to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudn1Name), 30*time.Second, time.Second).Should(Succeed())
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudn2Name), 30*time.Second, time.Second).Should(Succeed())

			By("creating a CNC with CUDN selector matching both CUDNs")
			createCNCWithCUDNSelector(cncName, testLabel)

			By("verifying CNC has 2 networks in subnet annotation")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)

			By("creating a 3rd Layer3 primary CUDN that matches the selector")
			createLayer3PrimaryCUDN(cudn3Name, testLabel, ns3.Name)

			By("waiting for 3rd CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudn3Name), 30*time.Second, time.Second).Should(Succeed())

			By("verifying CNC subnet annotation is updated to have 3 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)
		})

		It("8. CNC annotations are updated when a matching network is deleted", func() {
			cncName := generateCNCName()
			cudnName1 := fmt.Sprintf("test-cudn1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("test-cudn2-%s", rand.String(5))
			testLabel := map[string]string{"cnc-delete-test": "true"}
			ns1 := createUDNNamespace("test-delete1", nil)
			ns2 := createUDNNamespace("test-delete2", nil)

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating Layer3 and Layer2 primary CUDNs with matching labels")
			createLayer3PrimaryCUDN(cudnName1, testLabel, ns1.Name)
			createLayer2PrimaryCUDN(cudnName2, testLabel, ns2.Name)

			By("waiting for CUDNs to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating a CNC with CUDN selector")
			createCNCWithCUDNSelector(cncName, testLabel)

			By("verifying CNC has 2 networks in subnet annotation")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)

			By("deleting the Layer2 CUDN")
			deleteCUDN(cudnName2)

			By("waiting for CUDN to be deleted")
			Eventually(func() bool {
				_, err := e2ekubectl.RunKubectl("", "get", "clusteruserdefinednetwork", cudnName2)
				return err != nil
			}, 60*time.Second, 2*time.Second).Should(BeTrue())

			By("verifying CNC subnet annotation is updated to have only 1 network")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
		})

		It("9. CNC annotations are updated when a new matching network is created", func() {
			cncName := generateCNCName()
			cudnName1 := fmt.Sprintf("test-cudn1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("test-cudn2-%s", rand.String(5))
			testLabel := map[string]string{"cnc-create-test": "true"}
			ns1 := createUDNNamespace("test-create1", nil)
			ns2 := createUDNNamespace("test-create2", nil)

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating one Layer3 primary CUDN with matching label")
			createLayer3PrimaryCUDN(cudnName1, testLabel, ns1.Name)

			By("waiting for CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())

			By("creating a CNC with CUDN selector")
			createCNCWithCUDNSelector(cncName, testLabel)

			By("verifying CNC has 1 network in subnet annotation")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)

			By("creating a second Layer2 CUDN with matching label")
			createLayer2PrimaryCUDN(cudnName2, testLabel, ns2.Name)

			By("waiting for second CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())

			By("verifying CNC subnet annotation is updated to have 2 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
		})

		It("10. CNC annotations are updated when CUDN label changes to match selector", func() {
			cncName := generateCNCName()
			cudnName := fmt.Sprintf("test-cudn-%s", rand.String(5))
			matchingLabel := map[string]string{"cnc-label-match": "true"}
			nonMatchingLabel := map[string]string{"cnc-label-match": "false"}
			ns := createUDNNamespace("test-label-match", nil)

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
			})

			By("creating a CNC with CUDN selector")
			createCNCWithCUDNSelector(cncName, matchingLabel)

			By("verifying CNC has only tunnel ID annotation initially (no matching networks)")
			verifyCNCHasOnlyTunnelIDAnnotation(cncName)

			By("creating a Layer3 primary CUDN with non-matching label")
			createLayer3PrimaryCUDN(cudnName, nonMatchingLabel, ns.Name)

			By("waiting for CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())

			By("verifying CNC still has only tunnel ID annotation (CUDN doesn't match)")
			verifyCNCHasOnlyTunnelIDAnnotation(cncName)

			By("updating CUDN label to match the CNC selector")
			_, err := e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", cudnName, "cnc-label-match=true", "--overwrite")
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC annotations are updated to include the network")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
		})

		It("11. CNC annotations are updated when CUDN label changes to stop matching selector", func() {
			cncName := generateCNCName()
			cudnName := fmt.Sprintf("test-cudn-%s", rand.String(5))
			matchingLabel := map[string]string{"cnc-label-unmatch": "true"}
			ns := createUDNNamespace("test-label-unmatch", nil)

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
			})

			By("creating a Layer3 primary CUDN with matching label")
			createLayer3PrimaryCUDN(cudnName, matchingLabel, ns.Name)

			By("waiting for CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating a CNC with CUDN selector")
			createCNCWithCUDNSelector(cncName, matchingLabel)

			By("verifying CNC has both annotations with the matching network")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)

			By("updating CUDN label to no longer match the CNC selector")
			_, err := e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", cudnName, "cnc-label-unmatch=false", "--overwrite")
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC annotations are updated (subnet annotation becomes empty)")
			Eventually(func(g Gomega) {
				annotations, err := getCNCAnnotations(cncName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(annotations).To(HaveKey(ovnConnectRouterTunnelKeyAnnotation))
				subnetAnnotation := annotations[ovnNetworkConnectSubnetAnnotation]
				g.Expect(subnetAnnotation).To(Equal("{}"), "subnet annotation should be empty when no networks match")
			}, 60*time.Second, 2*time.Second).Should(Succeed())
		})
	})
})
