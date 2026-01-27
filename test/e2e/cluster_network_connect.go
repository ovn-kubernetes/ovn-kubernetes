package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"
)

// ============================================================================
// CNC Test Constants
// ============================================================================
const (
	// Annotation keys used by the CNC controller
	ovnNetworkConnectSubnetAnnotation   = "k8s.ovn.org/network-connect-subnet"
	ovnConnectRouterTunnelKeyAnnotation = "k8s.ovn.org/connect-router-tunnel-key"

	// CNC connect subnet configuration
	cncConnectSubnetIPv4CIDR   = "192.168.0.0/16"
	cncConnectSubnetIPv4Prefix = 24
	// IPv6 networkPrefix must satisfy: 32 - ipv4Prefix == 128 - ipv6Prefix
	// With ipv4Prefix=24: 32-24=8, so ipv6Prefix must be 128-8=120
	cncConnectSubnetIPv6CIDR   = "fd00:10::/112"
	cncConnectSubnetIPv6Prefix = 120

	// Layer3 UDN CIDRs with hostSubnet (IPv4: /24, IPv6: /64)
	layer3UserDefinedNetworkIPv4CIDR       = "172.31.0.0/16"
	layer3UserDefinedNetworkIPv4HostSubnet = 24
	layer3UserDefinedNetworkIPv6CIDR       = "2014:100:200::0/60"
	layer3UserDefinedNetworkIPv6HostSubnet = 64

	// Layer2 UDN CIDRs
	layer2UserDefinedNetworkIPv4CIDR = "10.200.0.0/16"
	layer2UserDefinedNetworkIPv6CIDR = "2015:100:200::0/60"
)

// cncAnnotationSubnet represents the subnet annotation structure
type cncAnnotationSubnet struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

// ============================================================================
// CNC Test Global Utilities
// ============================================================================

// generateCNCName generates a random CNC name
func generateCNCName() string {
	return fmt.Sprintf("test-cnc-%s", rand.String(5))
}

// generateConnectSubnets generates connectSubnets YAML based on cluster IP family support
func generateConnectSubnets(cs clientset.Interface) string {
	return generateConnectSubnetsWithCIDRs(cs, cncConnectSubnetIPv4CIDR, cncConnectSubnetIPv4Prefix,
		cncConnectSubnetIPv6CIDR, cncConnectSubnetIPv6Prefix)
}

// generateConnectSubnetsWithCIDRs generates connectSubnets YAML with custom CIDRs
func generateConnectSubnetsWithCIDRs(cs clientset.Interface, v4CIDR string, v4Prefix int, v6CIDR string, v6Prefix int) string {
	var subnets []string
	if isIPv4Supported(cs) {
		subnets = append(subnets, fmt.Sprintf(`    - cidr: "%s"
      networkPrefix: %d`, v4CIDR, v4Prefix))
	}
	if isIPv6Supported(cs) {
		subnets = append(subnets, fmt.Sprintf(`    - cidr: "%s"
      networkPrefix: %d`, v6CIDR, v6Prefix))
	}
	return strings.Join(subnets, "\n")
}

// generateNetworkSubnets generates subnets YAML with custom CIDRs
// Pass empty strings to use defaults
func generateNetworkSubnets(cs clientset.Interface, topology, v4Subnet, v6Subnet string) string {
	// Use custom subnets if provided, otherwise fall back to defaults
	l3v4CIDR := layer3UserDefinedNetworkIPv4CIDR
	l3v6CIDR := layer3UserDefinedNetworkIPv6CIDR
	l2v4CIDR := layer2UserDefinedNetworkIPv4CIDR
	l2v6CIDR := layer2UserDefinedNetworkIPv6CIDR

	if v4Subnet != "" {
		l3v4CIDR = v4Subnet
		l2v4CIDR = v4Subnet
	}
	if v6Subnet != "" {
		l3v6CIDR = v6Subnet
		l2v6CIDR = v6Subnet
	}

	if topology == "Layer3" {
		var subnets []string
		if isIPv4Supported(cs) {
			subnets = append(subnets, fmt.Sprintf(`{cidr: "%s", hostSubnet: %d}`, l3v4CIDR, layer3UserDefinedNetworkIPv4HostSubnet))
		}
		if isIPv6Supported(cs) {
			subnets = append(subnets, fmt.Sprintf(`{cidr: "%s", hostSubnet: %d}`, l3v6CIDR, layer3UserDefinedNetworkIPv6HostSubnet))
		}
		return fmt.Sprintf("[%s]", strings.Join(subnets, ","))
	}
	// Layer2 format
	var quotedCidrs []string
	if isIPv4Supported(cs) {
		quotedCidrs = append(quotedCidrs, fmt.Sprintf(`"%s"`, l2v4CIDR))
	}
	if isIPv6Supported(cs) {
		quotedCidrs = append(quotedCidrs, fmt.Sprintf(`"%s"`, l2v6CIDR))
	}
	return fmt.Sprintf("[%s]", strings.Join(quotedCidrs, ","))
}

// createUDNNamespaceWithName creates a namespace with UDN label and optional additional labels
func createUDNNamespaceWithName(cs clientset.Interface, name string, labels map[string]string) *corev1.Namespace {
	if labels == nil {
		labels = map[string]string{}
	}
	labels[RequiredUDNNamespaceLabel] = ""
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
	createdNs, err := cs.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	return createdNs
}

// createUDNNamespace creates a namespace with UDN label and a random suffix
func createUDNNamespace(cs clientset.Interface, baseName string, labels map[string]string) *corev1.Namespace {
	return createUDNNamespaceWithName(cs, baseName+"-"+rand.String(5), labels)
}

// deleteNamespace deletes a namespace
func deleteNamespace(cs clientset.Interface, nsName string) {
	_ = cs.CoreV1().Namespaces().Delete(context.Background(), nsName, metav1.DeleteOptions{})
}

// createOrUpdateCNC creates or updates a CNC with CUDN and/or PUDN selectors using default connect subnets
// Uses kubectl apply, so can be called to update an existing CNC
func createOrUpdateCNC(cs clientset.Interface, cncName string, cudnLabelSelector, udnLabelSelector map[string]string) {
	createOrUpdateCNCWithSubnets(cncName, cudnLabelSelector, udnLabelSelector, generateConnectSubnets(cs))
}

// createOrUpdateCNCWithSubnets creates or updates a CNC with custom connect subnets
func createOrUpdateCNCWithSubnets(cncName string, cudnLabelSelector, udnLabelSelector map[string]string, connectSubnets string) {
	Expect(cudnLabelSelector != nil || udnLabelSelector != nil).To(BeTrue(),
		"createOrUpdateCNCWithSubnets requires at least one selector (cudnLabelSelector or udnLabelSelector)")

	var networkSelectors []string

	if cudnLabelSelector != nil {
		cudnLabelSelectorStr := ""
		for k, v := range cudnLabelSelector {
			if cudnLabelSelectorStr != "" {
				cudnLabelSelectorStr += "\n            "
			}
			cudnLabelSelectorStr += fmt.Sprintf("%s: \"%s\"", k, v)
		}
		networkSelectors = append(networkSelectors, fmt.Sprintf(`    - networkSelectionType: "ClusterUserDefinedNetworks"
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            %s`, cudnLabelSelectorStr))
	}

	if udnLabelSelector != nil {
		udnLabelSelectorStr := ""
		for k, v := range udnLabelSelector {
			if udnLabelSelectorStr != "" {
				udnLabelSelectorStr += "\n            "
			}
			udnLabelSelectorStr += fmt.Sprintf("%s: \"%s\"", k, v)
		}
		networkSelectors = append(networkSelectors, fmt.Sprintf(`    - networkSelectionType: "PrimaryUserDefinedNetworks"
      primaryUserDefinedNetworkSelector:
        namespaceSelector:
          matchLabels:
            %s`, udnLabelSelectorStr))
	}

	manifest := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: ClusterNetworkConnect
metadata:
  name: %s
spec:
  networkSelectors:
%s
  connectSubnets:
%s
  connectivity: ["PodNetwork"]
`, cncName, strings.Join(networkSelectors, "\n"), connectSubnets)
	_, err := e2ekubectl.RunKubectlInput("", manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
}

// deleteCNC deletes a CNC
func deleteCNC(cncName string) {
	_, _ = e2ekubectl.RunKubectl("", "delete", "clusternetworkconnect", cncName, "--ignore-not-found")
}

// createPrimaryCUDN creates a primary CUDN with specified topology
func createPrimaryCUDN(cs clientset.Interface, cudnName, topology string, labels map[string]string, targetNamespaces ...string) {
	createPrimaryCUDNWithSubnets(cs, cudnName, topology, labels, "", "", targetNamespaces...)
}

// createPrimaryCUDNWithSubnets creates a primary CUDN with specified topology and custom subnets.
// Pass empty strings for v4Subnet/v6Subnet to use defaults.
func createPrimaryCUDNWithSubnets(cs clientset.Interface, cudnName, topology string, labels map[string]string, v4Subnet, v6Subnet string, targetNamespaces ...string) {
	targetNs := strings.Join(targetNamespaces, ",")
	labelAnnotations := ""
	for k, v := range labels {
		if labelAnnotations != "" {
			labelAnnotations += "\n    "
		}
		labelAnnotations += fmt.Sprintf("%s: \"%s\"", k, v)
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
`, cudnName, labelAnnotations, targetNs, topology, topologyLower, generateNetworkSubnets(cs, topology, v4Subnet, v6Subnet))
	_, err := e2ekubectl.RunKubectlInput("", manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
}

// deleteCUDN deletes a CUDN
func deleteCUDN(cudnName string) {
	_, _ = e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", "--timeout=60s", "--ignore-not-found")
}

// createPrimaryUDN creates a primary UDN with specified topology
func createPrimaryUDN(cs clientset.Interface, namespace, udnName, topology string) {
	createPrimaryUDNWithSubnets(cs, namespace, udnName, topology, "", "")
}

// createPrimaryUDNWithSubnets creates a primary UDN with specified topology and custom subnets.
// Pass empty strings for v4Subnet/v6Subnet to use defaults.
func createPrimaryUDNWithSubnets(cs clientset.Interface, namespace, udnName, topology, v4Subnet, v6Subnet string) {
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
`, udnName, topology, topologyLower, generateNetworkSubnets(cs, topology, v4Subnet, v6Subnet))
	_, err := e2ekubectl.RunKubectlInput(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
}

// deleteUDN deletes a UDN
func deleteUDN(namespace, udnName string) {
	_, _ = e2ekubectl.RunKubectl(namespace, "delete", "userdefinednetwork", udnName, "--wait", "--timeout=60s", "--ignore-not-found")
}

// createLayer3PrimaryCUDN creates a Layer3 primary CUDN (convenience function)
func createLayer3PrimaryCUDN(cs clientset.Interface, cudnName string, labels map[string]string, targetNamespaces ...string) {
	createPrimaryCUDN(cs, cudnName, "Layer3", labels, targetNamespaces...)
}

// createLayer3PrimaryCUDNWithSubnets creates a Layer3 primary CUDN with custom subnets
func createLayer3PrimaryCUDNWithSubnets(cs clientset.Interface, cudnName string, labels map[string]string, v4Subnet, v6Subnet string, targetNamespaces ...string) {
	createPrimaryCUDNWithSubnets(cs, cudnName, "Layer3", labels, v4Subnet, v6Subnet, targetNamespaces...)
}

// createLayer2PrimaryCUDN creates a Layer2 primary CUDN (convenience function)
func createLayer2PrimaryCUDN(cs clientset.Interface, cudnName string, labels map[string]string, targetNamespaces ...string) {
	createPrimaryCUDN(cs, cudnName, "Layer2", labels, targetNamespaces...)
}

// createLayer2PrimaryCUDNWithSubnets creates a Layer2 primary CUDN with custom subnets
func createLayer2PrimaryCUDNWithSubnets(cs clientset.Interface, cudnName string, labels map[string]string, v4Subnet, v6Subnet string, targetNamespaces ...string) {
	createPrimaryCUDNWithSubnets(cs, cudnName, "Layer2", labels, v4Subnet, v6Subnet, targetNamespaces...)
}

// createLayer3PrimaryUDN creates a Layer3 primary UDN (convenience function)
func createLayer3PrimaryUDN(cs clientset.Interface, namespace, udnName string) {
	createPrimaryUDN(cs, namespace, udnName, "Layer3")
}

// createLayer3PrimaryUDNWithSubnets creates a Layer3 primary UDN with custom subnets
func createLayer3PrimaryUDNWithSubnets(cs clientset.Interface, namespace, udnName, v4Subnet, v6Subnet string) {
	createPrimaryUDNWithSubnets(cs, namespace, udnName, "Layer3", v4Subnet, v6Subnet)
}

// createLayer2PrimaryUDN creates a Layer2 primary UDN (convenience function)
func createLayer2PrimaryUDN(cs clientset.Interface, namespace, udnName string) {
	createPrimaryUDN(cs, namespace, udnName, "Layer2")
}

// createLayer2PrimaryUDNWithSubnets creates a Layer2 primary UDN with custom subnets
func createLayer2PrimaryUDNWithSubnets(cs clientset.Interface, namespace, udnName, v4Subnet, v6Subnet string) {
	createPrimaryUDNWithSubnets(cs, namespace, udnName, "Layer2", v4Subnet, v6Subnet)
}

// getCNCAnnotations gets CNC annotations
func getCNCAnnotations(cncName string) (map[string]string, error) {
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

// getCNCTunnelID gets CNC tunnel ID from annotations
func getCNCTunnelID(cncName string) string {
	annotations, err := getCNCAnnotations(cncName)
	Expect(err).NotTo(HaveOccurred())
	return annotations[ovnConnectRouterTunnelKeyAnnotation]
}

type cncStatusPayload struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// getCNCCondition returns the requested CNC status condition (or nil if missing).
func getCNCCondition(cncName, conditionType string) (*metav1.Condition, error) {
	out, err := e2ekubectl.RunKubectl("", "get", "clusternetworkconnect", cncName, "-o", "jsonpath={.status}")
	if err != nil {
		return nil, err
	}
	var payload cncStatusPayload
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return nil, err
	}
	for i := range payload.Conditions {
		if payload.Conditions[i].Type == conditionType {
			return &payload.Conditions[i], nil
		}
	}
	return nil, nil
}

func verifyCNCAcceptedConditionFailed(cncName, errorSubstring string) {
	Expect(errorSubstring).NotTo(BeEmpty(), "verifyCNCAcceptedConditionFailed requires a non-empty error substring")
	Eventually(func(g Gomega) {
		cond, err := getCNCCondition(cncName, "Accepted")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cond).NotTo(BeNil(), "CNC should have an Accepted condition")
		g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(cond.Reason).To(Equal("ResourceAllocationFailed"))
		g.Expect(cond.Message).To(ContainSubstring(errorSubstring))
	}, 5*time.Second, 500*time.Millisecond).Should(Succeed())
}

// verifyCNCHasOnlyTunnelIDAnnotation verifies CNC has only tunnel ID annotation
func verifyCNCHasOnlyTunnelIDAnnotation(cncName string) {
	Eventually(func(g Gomega) {
		annotations, err := getCNCAnnotations(cncName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(annotations).To(HaveKey(ovnConnectRouterTunnelKeyAnnotation), "CNC should have tunnel ID annotation")
		if subnetAnnotation, exists := annotations[ovnNetworkConnectSubnetAnnotation]; exists {
			g.Expect(subnetAnnotation).To(Equal("{}"), "subnet annotation should be empty when no networks match")
		}
	}, 30*time.Second, 1*time.Second).Should(Succeed())
}

// verifyCNCHasBothAnnotations verifies CNC has both tunnel ID and subnet annotations
func verifyCNCHasBothAnnotations(cncName string) {
	Eventually(func(g Gomega) {
		annotations, err := getCNCAnnotations(cncName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(annotations).To(HaveKey(ovnConnectRouterTunnelKeyAnnotation), "CNC should have tunnel ID annotation")
		g.Expect(annotations).To(HaveKey(ovnNetworkConnectSubnetAnnotation), "CNC should have subnet annotation")
		subnetAnnotation := annotations[ovnNetworkConnectSubnetAnnotation]
		g.Expect(subnetAnnotation).NotTo(Equal("{}"), "subnet annotation should not be empty when networks match")
		var subnets map[string]cncAnnotationSubnet
		err = json.Unmarshal([]byte(subnetAnnotation), &subnets)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(len(subnets)).To(BeNumerically(">", 0), "should have at least one network subnet")
	}, 60*time.Second, 2*time.Second).Should(Succeed())
}

// verifyCNCSubnetAnnotationNetworkCount verifies CNC subnet annotation has expected network count
func verifyCNCSubnetAnnotationNetworkCount(cncName string, expectedCount int) {
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

// verifyCNCSubnetAnnotationContent verifies subnet annotation content: key format, topology counts, and CIDR format
// expectedTopologies is a list of expected topologies (e.g., ["Layer3", "Layer2", "Layer3"])
func verifyCNCSubnetAnnotationContent(cncName string, expectedTopologies []string) {
	Eventually(func(g Gomega) {
		annotations, err := getCNCAnnotations(cncName)
		g.Expect(err).NotTo(HaveOccurred())
		subnetAnnotation := annotations[ovnNetworkConnectSubnetAnnotation]
		var subnets map[string]cncAnnotationSubnet
		err = json.Unmarshal([]byte(subnetAnnotation), &subnets)
		g.Expect(err).NotTo(HaveOccurred())

		// Count topologies found
		topologyCounts := map[string]int{"layer2": 0, "layer3": 0}
		for networkKey, subnet := range subnets {
			// Key format should be <topology>_<networkID> e.g., "layer3_1", "layer2_2"
			g.Expect(networkKey).To(MatchRegexp(`^(layer2|layer3)_\d+$`),
				fmt.Sprintf("network key %s should match format <topology>_<networkID>", networkKey))

			if strings.HasPrefix(networkKey, "layer2_") {
				topologyCounts["layer2"]++
			} else if strings.HasPrefix(networkKey, "layer3_") {
				topologyCounts["layer3"]++
			}

			// Verify at least one of IPv4 or IPv6 is present
			hasIPv4 := subnet.IPv4 != ""
			hasIPv6 := subnet.IPv6 != ""
			g.Expect(hasIPv4 || hasIPv6).To(BeTrue(),
				fmt.Sprintf("network %s should have at least one subnet", networkKey))

			isLayer2 := strings.HasPrefix(networkKey, "layer2_")

			// Verify IPv4 format if present (should be CIDR within connectSubnets range)
			if hasIPv4 {
				g.Expect(subnet.IPv4).To(MatchRegexp(`^192\.16[89]\.\d+\.\d+/\d+$`),
					fmt.Sprintf("network %s IPv4 subnet should be in connectSubnets range (192.168.x.x or 192.169.x.x)", networkKey))
				// Layer2 networks use point-to-point /31 subnets
				if isLayer2 {
					g.Expect(subnet.IPv4).To(HaveSuffix("/31"),
						fmt.Sprintf("Layer2 network %s IPv4 should have /31 mask", networkKey))
				}
			}

			// Verify IPv6 format if present (should be CIDR within connectSubnets range)
			if hasIPv6 {
				g.Expect(subnet.IPv6).To(MatchRegexp(`^fd00:1[01]::[0-9a-f:]*/\d+$`),
					fmt.Sprintf("network %s IPv6 subnet should be in connectSubnets range (fd00:10:: or fd00:11::)", networkKey))
				// Layer2 networks use point-to-point /127 subnets
				if isLayer2 {
					g.Expect(subnet.IPv6).To(HaveSuffix("/127"),
						fmt.Sprintf("Layer2 network %s IPv6 should have /127 mask", networkKey))
				}
			}
		}

		// Verify expected topology counts match
		expectedCounts := map[string]int{"layer2": 0, "layer3": 0}
		for _, topo := range expectedTopologies {
			expectedCounts[strings.ToLower(topo)]++
		}
		g.Expect(topologyCounts["layer2"]).To(Equal(expectedCounts["layer2"]),
			fmt.Sprintf("expected %d Layer2 networks, got %d", expectedCounts["layer2"], topologyCounts["layer2"]))
		g.Expect(topologyCounts["layer3"]).To(Equal(expectedCounts["layer3"]),
			fmt.Sprintf("expected %d Layer3 networks, got %d", expectedCounts["layer3"], topologyCounts["layer3"]))
	}, 60*time.Second, 2*time.Second).Should(Succeed())
}

var _ = Describe("ClusterNetworkConnect ClusterManagerController", feature.NetworkConnect, func() {
	f := wrappedTestFramework("cnc-controller")
	// disable automatic namespace creation, we need to add the required UDN label
	f.SkipNamespaceCreation = true

	var (
		cs clientset.Interface
	)

	BeforeEach(func() {
		cs = f.ClientSet
	})

	// ===========================================
	// Group 1: No Matching Networks (1 test)
	// ===========================================
	Context("when CNC has no matching networks", func() {
		It("has only tunnel ID annotation", func() {
			cncName := generateCNCName()
			DeferCleanup(func() {
				deleteCNC(cncName)
			})

			By("creating a CNC with selector that matches no networks")
			createOrUpdateCNC(cs, cncName, map[string]string{"nonexistent": "label"}, nil)

			By("verifying CNC has only tunnel ID annotation")
			verifyCNCHasOnlyTunnelIDAnnotation(cncName)

			By("verifying tunnel ID is valid")
			tunnelID := getCNCTunnelID(cncName)
			Expect(tunnelID).NotTo(BeEmpty(), "CNC should have tunnel ID even with no matching networks")
		})
	})

	// ===========================================
	// Group 2: Static Creation - Networks exist first, then CNC created (7 tests)
	// ===========================================
	Context("when networks exist before CNC creation", func() {
		// Single network tests using DescribeTable
		DescribeTable("single network: has both subnet and tunnel ID annotations",
			func(topology, kind string) {
				cncName := generateCNCName()
				networkName := fmt.Sprintf("test-%s-%s", strings.ToLower(kind), rand.String(5))
				testLabel := map[string]string{fmt.Sprintf("test-%s-%s", strings.ToLower(kind), strings.ToLower(topology)): "true"}

				if kind == "UDN" {
					ns := createUDNNamespace(cs, fmt.Sprintf("test-%s-%s", strings.ToLower(kind), strings.ToLower(topology)), testLabel)
					DeferCleanup(func() {
						deleteCNC(cncName)
						deleteUDN(ns.Name, networkName)
						cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
					})

					By(fmt.Sprintf("creating a %s primary UDN", topology))
					createPrimaryUDN(cs, ns.Name, networkName, topology)

					By("waiting for UDN to be ready")
					Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns.Name, networkName), 30*time.Second, time.Second).Should(Succeed())

					By("creating a CNC with PUDN selector")
					createOrUpdateCNC(cs, cncName, nil, testLabel)
				} else {
					ns := createUDNNamespace(cs, fmt.Sprintf("test-%s-%s", strings.ToLower(kind), strings.ToLower(topology)), nil)
					DeferCleanup(func() {
						deleteCNC(cncName)
						deleteCUDN(networkName)
						cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
					})

					By(fmt.Sprintf("creating a %s primary CUDN", topology))
					createPrimaryCUDN(cs, networkName, topology, testLabel, ns.Name)

					By("waiting for CUDN to be ready")
					Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, networkName), 30*time.Second, time.Second).Should(Succeed())

					By("creating a CNC with CUDN selector")
					createOrUpdateCNC(cs, cncName, testLabel, nil)
				}

				By("verifying CNC has both subnet and tunnel ID annotations")
				verifyCNCHasBothAnnotations(cncName)
				verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
				verifyCNCSubnetAnnotationContent(cncName, []string{topology})
			},
			Entry("L3 P-UDN", "Layer3", "UDN"),
			Entry("L2 P-UDN", "Layer2", "UDN"),
			Entry("L3 P-CUDN", "Layer3", "CUDN"),
			Entry("L2 P-CUDN", "Layer2", "CUDN"),
		)

		// Multiple networks of same kind tests using DescribeTable
		DescribeTable("multiple networks (2xL3 + 2xL2): has all networks in subnet annotation",
			func(kind string) {
				cncName := generateCNCName()
				testLabel := map[string]string{fmt.Sprintf("test-multi-%s", strings.ToLower(kind)): "true"}
				var namespaces []*corev1.Namespace
				var networkNames []string
				var expectedTopologies []string

				if kind == "UDN" {
					// Create 4 namespaces with the same label for PUDN selector
					for i := 1; i <= 4; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-udn-%d", i), testLabel))
						networkNames = append(networkNames, fmt.Sprintf("udn%d", i))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						for i, ns := range namespaces {
							deleteUDN(ns.Name, networkNames[i])
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By("creating 2 Layer3 and 2 Layer2 primary UDNs")
					createLayer3PrimaryUDN(cs, namespaces[0].Name, networkNames[0])
					expectedTopologies = append(expectedTopologies, "Layer3")
					createLayer3PrimaryUDN(cs, namespaces[1].Name, networkNames[1])
					expectedTopologies = append(expectedTopologies, "Layer3")
					createLayer2PrimaryUDN(cs, namespaces[2].Name, networkNames[2])
					expectedTopologies = append(expectedTopologies, "Layer2")
					createLayer2PrimaryUDN(cs, namespaces[3].Name, networkNames[3])
					expectedTopologies = append(expectedTopologies, "Layer2")

					By("waiting for all UDNs to be ready")
					for i, ns := range namespaces {
						Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns.Name, networkNames[i]), 30*time.Second, time.Second).Should(Succeed())
					}

					By("creating a CNC with PUDN selector")
					createOrUpdateCNC(cs, cncName, nil, testLabel)
				} else {
					// CUDN case - one CUDN targets multiple namespaces
					for i := 1; i <= 5; i++ { // 5 namespaces for multi-ns CUDN test
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-cudn-ns%d", i), nil))
						networkNames = append(networkNames, fmt.Sprintf("cudn-%d-%s", i, rand.String(5)))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						for i := 0; i < 4; i++ { // only 4 CUDNs
							deleteCUDN(networkNames[i])
						}
						for _, ns := range namespaces {
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By("creating 2 Layer3 and 2 Layer2 primary CUDNs (one L3 targets multiple namespaces)")
					createPrimaryCUDN(cs, networkNames[0], "Layer3", testLabel, namespaces[0].Name)
					expectedTopologies = append(expectedTopologies, "Layer3")
					createPrimaryCUDN(cs, networkNames[1], "Layer3", testLabel, namespaces[1].Name, namespaces[4].Name) // multi-ns
					expectedTopologies = append(expectedTopologies, "Layer3")
					createPrimaryCUDN(cs, networkNames[2], "Layer2", testLabel, namespaces[2].Name)
					expectedTopologies = append(expectedTopologies, "Layer2")
					createPrimaryCUDN(cs, networkNames[3], "Layer2", testLabel, namespaces[3].Name)
					expectedTopologies = append(expectedTopologies, "Layer2")

					By("waiting for all CUDNs to be ready")
					for i := 0; i < 4; i++ {
						Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, networkNames[i]), 30*time.Second, time.Second).Should(Succeed())
					}

					By("creating a CNC with CUDN selector")
					createOrUpdateCNC(cs, cncName, testLabel, nil)
				}

				By("verifying CNC has 4 networks in subnet annotation")
				verifyCNCHasBothAnnotations(cncName)
				verifyCNCSubnetAnnotationNetworkCount(cncName, 4)
				verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
			},
			Entry("P-UDNs", "UDN"),
			Entry("P-CUDNs (one multi-ns)", "CUDN"),
		)

		It("full matrix (2x each type) - has all 8 networks in subnet annotation", func() {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-full-matrix": "true"}
			udnLabel := map[string]string{"test-full-matrix": "true"}

			var cudnNames []string
			var udnNames []string
			var cudnNamespaces []*corev1.Namespace
			var udnNamespaces []*corev1.Namespace
			var expectedTopologies []string

			// Create namespaces and network names
			for i := 1; i <= 4; i++ {
				cudnNames = append(cudnNames, fmt.Sprintf("fm-cudn-%d-%s", i, rand.String(5)))
				udnNames = append(udnNames, fmt.Sprintf("udn%d", i))
				cudnNamespaces = append(cudnNamespaces, createUDNNamespace(cs, fmt.Sprintf("fm-cudn-ns%d", i), nil))
				udnNamespaces = append(udnNamespaces, createUDNNamespace(cs, fmt.Sprintf("fm-udn-ns%d", i), udnLabel))
			}

			DeferCleanup(func() {
				deleteCNC(cncName)
				for _, name := range cudnNames {
					deleteCUDN(name)
				}
				for i, ns := range udnNamespaces {
					deleteUDN(ns.Name, udnNames[i])
				}
				for _, ns := range cudnNamespaces {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
				for _, ns := range udnNamespaces {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
			})

			By("creating 4 CUDNs (2xL3 + 2xL2)")
			createLayer3PrimaryCUDN(cs, cudnNames[0], cudnLabel, cudnNamespaces[0].Name)
			expectedTopologies = append(expectedTopologies, "Layer3")
			createLayer3PrimaryCUDN(cs, cudnNames[1], cudnLabel, cudnNamespaces[1].Name)
			expectedTopologies = append(expectedTopologies, "Layer3")
			createLayer2PrimaryCUDN(cs, cudnNames[2], cudnLabel, cudnNamespaces[2].Name)
			expectedTopologies = append(expectedTopologies, "Layer2")
			createLayer2PrimaryCUDN(cs, cudnNames[3], cudnLabel, cudnNamespaces[3].Name)
			expectedTopologies = append(expectedTopologies, "Layer2")

			By("creating 4 UDNs (2xL3 + 2xL2)")
			createLayer3PrimaryUDN(cs, udnNamespaces[0].Name, udnNames[0])
			expectedTopologies = append(expectedTopologies, "Layer3")
			createLayer3PrimaryUDN(cs, udnNamespaces[1].Name, udnNames[1])
			expectedTopologies = append(expectedTopologies, "Layer3")
			createLayer2PrimaryUDN(cs, udnNamespaces[2].Name, udnNames[2])
			expectedTopologies = append(expectedTopologies, "Layer2")
			createLayer2PrimaryUDN(cs, udnNamespaces[3].Name, udnNames[3])
			expectedTopologies = append(expectedTopologies, "Layer2")

			By("waiting for all networks to be ready")
			for _, name := range cudnNames {
				Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, name), 30*time.Second, time.Second).Should(Succeed())
			}
			for i, ns := range udnNamespaces {
				Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns.Name, udnNames[i]), 30*time.Second, time.Second).Should(Succeed())
			}

			By("creating a CNC with both CUDN and PUDN selectors")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("verifying CNC has all 8 networks in subnet annotation")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 8)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
		})
	})

	// ===========================================
	// Group 3: Dynamic Creation - CNC created first, then networks (7 tests)
	// ===========================================
	Context("when CNC is created before networks", func() {
		// Single network tests using DescribeTable
		DescribeTable("single network created after CNC: annotations are updated",
			func(topology, kind string) {
				cncName := generateCNCName()
				networkName := fmt.Sprintf("test-%s-%s", strings.ToLower(kind), rand.String(5))
				testLabel := map[string]string{fmt.Sprintf("test-dyn-%s-%s", strings.ToLower(kind), strings.ToLower(topology)): "true"}
				var expectedTopologies []string

				if kind == "UDN" {
					ns := createUDNNamespace(cs, fmt.Sprintf("test-dyn-%s-%s", strings.ToLower(kind), strings.ToLower(topology)), testLabel)
					DeferCleanup(func() {
						deleteCNC(cncName)
						deleteUDN(ns.Name, networkName)
						cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
					})

					By("creating a CNC with PUDN selector (no matching networks yet)")
					createOrUpdateCNC(cs, cncName, nil, testLabel)

					By("verifying CNC has only tunnel ID annotation initially")
					verifyCNCHasOnlyTunnelIDAnnotation(cncName)

					By(fmt.Sprintf("creating a %s primary UDN", topology))
					createPrimaryUDN(cs, ns.Name, networkName, topology)
					expectedTopologies = append(expectedTopologies, topology)

					By("waiting for UDN to be ready")
					Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns.Name, networkName), 30*time.Second, time.Second).Should(Succeed())
				} else {
					ns := createUDNNamespace(cs, fmt.Sprintf("test-dyn-%s-%s", strings.ToLower(kind), strings.ToLower(topology)), nil)
					DeferCleanup(func() {
						deleteCNC(cncName)
						deleteCUDN(networkName)
						cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
					})

					By("creating a CNC with CUDN selector (no matching networks yet)")
					createOrUpdateCNC(cs, cncName, testLabel, nil)

					By("verifying CNC has only tunnel ID annotation initially")
					verifyCNCHasOnlyTunnelIDAnnotation(cncName)

					By(fmt.Sprintf("creating a %s primary CUDN", topology))
					createPrimaryCUDN(cs, networkName, topology, testLabel, ns.Name)
					expectedTopologies = append(expectedTopologies, topology)

					By("waiting for CUDN to be ready")
					Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, networkName), 30*time.Second, time.Second).Should(Succeed())
				}

				By("verifying CNC annotations are updated to include the network")
				verifyCNCHasBothAnnotations(cncName)
				verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
				verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
			},
			Entry("L3 P-UDN", "Layer3", "UDN"),
			Entry("L2 P-UDN", "Layer2", "UDN"),
			Entry("L3 P-CUDN", "Layer3", "CUDN"),
			Entry("L2 P-CUDN", "Layer2", "CUDN"),
		)

		// Multiple networks created after CNC
		DescribeTable("multiple networks created after CNC: annotations are updated",
			func(kind string) {
				cncName := generateCNCName()
				testLabel := map[string]string{fmt.Sprintf("test-dyn-multi-%s", strings.ToLower(kind)): "true"}
				var namespaces []*corev1.Namespace
				var networkNames []string
				var expectedTopologies []string

				if kind == "UDN" {
					// Create namespaces first (with label for PUDN selector)
					for i := 1; i <= 4; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-dyn-udn-%d", i), testLabel))
						networkNames = append(networkNames, fmt.Sprintf("udn%d", i))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						for i, ns := range namespaces {
							deleteUDN(ns.Name, networkNames[i])
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By("creating a CNC with PUDN selector (no matching networks yet)")
					createOrUpdateCNC(cs, cncName, nil, testLabel)

					By("verifying CNC has only tunnel ID annotation initially")
					verifyCNCHasOnlyTunnelIDAnnotation(cncName)

					By("creating 2 Layer3 and 2 Layer2 primary UDNs")
					createLayer3PrimaryUDN(cs, namespaces[0].Name, networkNames[0])
					expectedTopologies = append(expectedTopologies, "Layer3")
					createLayer3PrimaryUDN(cs, namespaces[1].Name, networkNames[1])
					expectedTopologies = append(expectedTopologies, "Layer3")
					createLayer2PrimaryUDN(cs, namespaces[2].Name, networkNames[2])
					expectedTopologies = append(expectedTopologies, "Layer2")
					createLayer2PrimaryUDN(cs, namespaces[3].Name, networkNames[3])
					expectedTopologies = append(expectedTopologies, "Layer2")

					By("waiting for all UDNs to be ready")
					for i, ns := range namespaces {
						Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns.Name, networkNames[i]), 30*time.Second, time.Second).Should(Succeed())
					}
				} else {
					// CUDN case
					for i := 1; i <= 5; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-dyn-cudn-ns%d", i), nil))
						networkNames = append(networkNames, fmt.Sprintf("dyn-cudn-%d-%s", i, rand.String(5)))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						for i := 0; i < 4; i++ {
							deleteCUDN(networkNames[i])
						}
						for _, ns := range namespaces {
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By("creating a CNC with CUDN selector (no matching networks yet)")
					createOrUpdateCNC(cs, cncName, testLabel, nil)

					By("verifying CNC has only tunnel ID annotation initially")
					verifyCNCHasOnlyTunnelIDAnnotation(cncName)

					By("creating 2 Layer3 and 2 Layer2 primary CUDNs (one L3 targets multiple namespaces)")
					createPrimaryCUDN(cs, networkNames[0], "Layer3", testLabel, namespaces[0].Name)
					expectedTopologies = append(expectedTopologies, "Layer3")
					createPrimaryCUDN(cs, networkNames[1], "Layer3", testLabel, namespaces[1].Name, namespaces[4].Name)
					expectedTopologies = append(expectedTopologies, "Layer3")
					createPrimaryCUDN(cs, networkNames[2], "Layer2", testLabel, namespaces[2].Name)
					expectedTopologies = append(expectedTopologies, "Layer2")
					createPrimaryCUDN(cs, networkNames[3], "Layer2", testLabel, namespaces[3].Name)
					expectedTopologies = append(expectedTopologies, "Layer2")

					By("waiting for all CUDNs to be ready")
					for i := 0; i < 4; i++ {
						Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, networkNames[i]), 30*time.Second, time.Second).Should(Succeed())
					}
				}

				By("verifying CNC has 4 networks in subnet annotation")
				verifyCNCHasBothAnnotations(cncName)
				verifyCNCSubnetAnnotationNetworkCount(cncName, 4)
				verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
			},
			Entry("P-UDNs", "UDN"),
			Entry("P-CUDNs (one multi-ns)", "CUDN"),
		)

		It("full matrix created after CNC - annotations are updated with all 8 networks", func() {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-dyn-full-matrix": "true"}
			udnLabel := map[string]string{"test-dyn-full-matrix": "true"}

			var cudnNames []string
			var udnNames []string
			var cudnNamespaces []*corev1.Namespace
			var udnNamespaces []*corev1.Namespace
			var expectedTopologies []string

			// Create namespaces first
			for i := 1; i <= 4; i++ {
				cudnNames = append(cudnNames, fmt.Sprintf("dyn-fm-cudn-%d-%s", i, rand.String(5)))
				udnNames = append(udnNames, fmt.Sprintf("udn%d", i))
				cudnNamespaces = append(cudnNamespaces, createUDNNamespace(cs, fmt.Sprintf("dyn-fm-cudn-ns%d", i), nil))
				udnNamespaces = append(udnNamespaces, createUDNNamespace(cs, fmt.Sprintf("dyn-fm-udn-ns%d", i), udnLabel))
			}

			DeferCleanup(func() {
				deleteCNC(cncName)
				for _, name := range cudnNames {
					deleteCUDN(name)
				}
				for i, ns := range udnNamespaces {
					deleteUDN(ns.Name, udnNames[i])
				}
				for _, ns := range cudnNamespaces {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
				for _, ns := range udnNamespaces {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
			})

			By("creating a CNC with both CUDN and PUDN selectors (no matching networks yet)")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("verifying CNC has only tunnel ID annotation initially")
			verifyCNCHasOnlyTunnelIDAnnotation(cncName)

			By("creating 4 CUDNs (2xL3 + 2xL2)")
			createLayer3PrimaryCUDN(cs, cudnNames[0], cudnLabel, cudnNamespaces[0].Name)
			expectedTopologies = append(expectedTopologies, "Layer3")
			createLayer3PrimaryCUDN(cs, cudnNames[1], cudnLabel, cudnNamespaces[1].Name)
			expectedTopologies = append(expectedTopologies, "Layer3")
			createLayer2PrimaryCUDN(cs, cudnNames[2], cudnLabel, cudnNamespaces[2].Name)
			expectedTopologies = append(expectedTopologies, "Layer2")
			createLayer2PrimaryCUDN(cs, cudnNames[3], cudnLabel, cudnNamespaces[3].Name)
			expectedTopologies = append(expectedTopologies, "Layer2")

			By("creating 4 UDNs (2xL3 + 2xL2)")
			createLayer3PrimaryUDN(cs, udnNamespaces[0].Name, udnNames[0])
			expectedTopologies = append(expectedTopologies, "Layer3")
			createLayer3PrimaryUDN(cs, udnNamespaces[1].Name, udnNames[1])
			expectedTopologies = append(expectedTopologies, "Layer3")
			createLayer2PrimaryUDN(cs, udnNamespaces[2].Name, udnNames[2])
			expectedTopologies = append(expectedTopologies, "Layer2")
			createLayer2PrimaryUDN(cs, udnNamespaces[3].Name, udnNames[3])
			expectedTopologies = append(expectedTopologies, "Layer2")

			By("waiting for all networks to be ready")
			for _, name := range cudnNames {
				Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, name), 30*time.Second, time.Second).Should(Succeed())
			}
			for i, ns := range udnNamespaces {
				Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns.Name, udnNames[i]), 30*time.Second, time.Second).Should(Succeed())
			}

			By("verifying CNC has all 8 networks in subnet annotation")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 8)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
		})
	})

	// ===========================================
	// Group 4: Adding Networks - networks added to existing CNC (4 tests)
	// ===========================================
	Context("when networks are added to existing CNC", func() {
		// Adding single network to CNC with existing networks
		DescribeTable("adding a network to CNC with existing networks: count increases",
			func(initialTopology, addedTopology, kind string) {
				cncName := generateCNCName()
				testLabel := map[string]string{fmt.Sprintf("test-add-%s", strings.ToLower(kind)): "true"}
				var namespaces []*corev1.Namespace
				var networkNames []string
				var expectedTopologies []string

				if kind == "UDN" {
					// Create 2 namespaces - one for initial, one for added
					for i := 1; i <= 2; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-add-udn-%d", i), testLabel))
						networkNames = append(networkNames, fmt.Sprintf("udn%d", i))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						for i, ns := range namespaces {
							deleteUDN(ns.Name, networkNames[i])
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By(fmt.Sprintf("creating initial %s primary UDN", initialTopology))
					createPrimaryUDN(cs, namespaces[0].Name, networkNames[0], initialTopology)
					expectedTopologies = append(expectedTopologies, initialTopology)
					Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, namespaces[0].Name, networkNames[0]), 30*time.Second, time.Second).Should(Succeed())

					By("creating CNC with PUDN selector")
					createOrUpdateCNC(cs, cncName, nil, testLabel)

					By("verifying CNC has 1 network in subnet annotation")
					verifyCNCHasBothAnnotations(cncName)
					verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
					verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

					By(fmt.Sprintf("adding a %s primary UDN", addedTopology))
					createPrimaryUDN(cs, namespaces[1].Name, networkNames[1], addedTopology)
					expectedTopologies = append(expectedTopologies, addedTopology)
					Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, namespaces[1].Name, networkNames[1]), 30*time.Second, time.Second).Should(Succeed())
				} else {
					// CUDN case
					for i := 1; i <= 2; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-add-cudn-ns%d", i), nil))
						networkNames = append(networkNames, fmt.Sprintf("add-cudn-%d-%s", i, rand.String(5)))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						for _, name := range networkNames {
							deleteCUDN(name)
						}
						for _, ns := range namespaces {
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By(fmt.Sprintf("creating initial %s primary CUDN", initialTopology))
					createPrimaryCUDN(cs, networkNames[0], initialTopology, testLabel, namespaces[0].Name)
					expectedTopologies = append(expectedTopologies, initialTopology)
					Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, networkNames[0]), 30*time.Second, time.Second).Should(Succeed())

					By("creating CNC with CUDN selector")
					createOrUpdateCNC(cs, cncName, testLabel, nil)

					By("verifying CNC has 1 network in subnet annotation")
					verifyCNCHasBothAnnotations(cncName)
					verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
					verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

					By(fmt.Sprintf("adding a %s primary CUDN", addedTopology))
					createPrimaryCUDN(cs, networkNames[1], addedTopology, testLabel, namespaces[1].Name)
					expectedTopologies = append(expectedTopologies, addedTopology)
					Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, networkNames[1]), 30*time.Second, time.Second).Should(Succeed())
				}

				By("verifying CNC now has 2 networks in subnet annotation")
				verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
				verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
			},
			Entry("add L2 P-UDN to L3 P-UDN", "Layer3", "Layer2", "UDN"),
			Entry("add L3 P-UDN to L2 P-UDN", "Layer2", "Layer3", "UDN"),
			Entry("add L2 P-CUDN to L3 P-CUDN", "Layer3", "Layer2", "CUDN"),
			Entry("add L3 P-CUDN to L2 P-CUDN", "Layer2", "Layer3", "CUDN"),
		)

		It("adding mixed networks (P-UDN + P-CUDN) to existing CNC - all networks appear", func() {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-add-mixed": "true"}
			udnLabel := map[string]string{"test-add-mixed": "true"}
			var expectedTopologies []string

			// Initial: 1 L3 CUDN + 1 L3 UDN
			initialCudnName := fmt.Sprintf("add-mixed-cudn-init-%s", rand.String(5))
			initialUdnName := "udn-init"
			cudnNs := createUDNNamespace(cs, "test-add-mixed-cudn", nil)
			udnNs := createUDNNamespace(cs, "test-add-mixed-udn", udnLabel)

			// Added: 1 L2 CUDN + 1 L2 UDN
			addedCudnName := fmt.Sprintf("add-mixed-cudn-add-%s", rand.String(5))
			addedUdnName := "udn-add"
			addedCudnNs := createUDNNamespace(cs, "test-add-mixed-cudn2", nil)
			addedUdnNs := createUDNNamespace(cs, "test-add-mixed-udn2", udnLabel)

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(initialCudnName)
				deleteCUDN(addedCudnName)
				deleteUDN(udnNs.Name, initialUdnName)
				deleteUDN(addedUdnNs.Name, addedUdnName)
				for _, ns := range []*corev1.Namespace{cudnNs, udnNs, addedCudnNs, addedUdnNs} {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
			})

			By("creating initial L3 CUDN and L3 UDN")
			createLayer3PrimaryCUDN(cs, initialCudnName, cudnLabel, cudnNs.Name)
			expectedTopologies = append(expectedTopologies, "Layer3")
			createLayer3PrimaryUDN(cs, udnNs.Name, initialUdnName)
			expectedTopologies = append(expectedTopologies, "Layer3")

			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, initialCudnName), 30*time.Second, time.Second).Should(Succeed())
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, udnNs.Name, initialUdnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with both selectors")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("verifying CNC has 2 networks initially")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

			By("adding L2 CUDN and L2 UDN")
			createLayer2PrimaryCUDN(cs, addedCudnName, cudnLabel, addedCudnNs.Name)
			expectedTopologies = append(expectedTopologies, "Layer2")
			createLayer2PrimaryUDN(cs, addedUdnNs.Name, addedUdnName)
			expectedTopologies = append(expectedTopologies, "Layer2")

			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, addedCudnName), 30*time.Second, time.Second).Should(Succeed())
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, addedUdnNs.Name, addedUdnName), 30*time.Second, time.Second).Should(Succeed())

			By("verifying CNC now has 4 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)
		})
	})

	// ===========================================
	// Group 5: Network Deletion - networks removed from CNC (4 tests)
	// ===========================================
	Context("when networks are deleted from CNC", func() {
		// Deleting single network from CNC with multiple networks
		DescribeTable("deleting networks from CNC: count decreases to zero",
			func(topology, kind string) {
				cncName := generateCNCName()
				testLabel := map[string]string{fmt.Sprintf("test-del-%s", strings.ToLower(kind)): "true"}
				var namespaces []*corev1.Namespace
				var networkNames []string

				if kind == "UDN" {
					// Create 2 namespaces with 2 networks
					for i := 1; i <= 2; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-del-udn-%d", i), testLabel))
						networkNames = append(networkNames, fmt.Sprintf("udn%d", i))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						// Networks already deleted in test
						for _, ns := range namespaces {
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By("creating 2 primary UDNs (L3 + topology)")
					createLayer3PrimaryUDN(cs, namespaces[0].Name, networkNames[0])
					createPrimaryUDN(cs, namespaces[1].Name, networkNames[1], topology)
					for i, ns := range namespaces {
						Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns.Name, networkNames[i]), 30*time.Second, time.Second).Should(Succeed())
					}

					By("creating CNC with PUDN selector")
					createOrUpdateCNC(cs, cncName, nil, testLabel)

					By("verifying CNC has 2 networks initially")
					verifyCNCHasBothAnnotations(cncName)
					verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
					verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", topology})

					By(fmt.Sprintf("deleting the %s UDN", topology))
					deleteUDN(namespaces[1].Name, networkNames[1])

					By("verifying CNC now has 1 network")
					verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
					verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

					By("deleting the remaining L3 UDN")
					deleteUDN(namespaces[0].Name, networkNames[0])
				} else {
					// CUDN case
					for i := 1; i <= 2; i++ {
						namespaces = append(namespaces, createUDNNamespace(cs, fmt.Sprintf("test-del-cudn-ns%d", i), nil))
						networkNames = append(networkNames, fmt.Sprintf("del-cudn-%d-%s", i, rand.String(5)))
					}

					DeferCleanup(func() {
						deleteCNC(cncName)
						// Networks already deleted in test
						for _, ns := range namespaces {
							cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
						}
					})

					By("creating 2 primary CUDNs (L3 + topology)")
					createLayer3PrimaryCUDN(cs, networkNames[0], testLabel, namespaces[0].Name)
					createPrimaryCUDN(cs, networkNames[1], topology, testLabel, namespaces[1].Name)
					for i := 0; i < 2; i++ {
						Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, networkNames[i]), 30*time.Second, time.Second).Should(Succeed())
					}

					By("creating CNC with CUDN selector")
					createOrUpdateCNC(cs, cncName, testLabel, nil)

					By("verifying CNC has 2 networks initially")
					verifyCNCHasBothAnnotations(cncName)
					verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
					verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", topology})

					By(fmt.Sprintf("deleting the %s CUDN", topology))
					deleteCUDN(networkNames[1])

					By("verifying CNC now has 1 network")
					verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
					verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

					By("deleting the remaining L3 CUDN")
					deleteCUDN(networkNames[0])
				}

				By("verifying CNC reverts to only tunnel ID annotation")
				verifyCNCHasOnlyTunnelIDAnnotation(cncName)
			},
			Entry("delete L2 then L3 P-UDN", "Layer2", "UDN"),
			Entry("delete L3 then L3 P-UDN", "Layer3", "UDN"),
			Entry("delete L2 then L3 P-CUDN", "Layer2", "CUDN"),
			Entry("delete L3 then L3 P-CUDN", "Layer3", "CUDN"),
		)

		It("deleting mixed networks (P-UDN + P-CUDN) - annotations update correctly", func() {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-del-mixed": "true"}
			udnLabel := map[string]string{"test-del-mixed": "true"}

			// Create 2 CUDNs + 2 UDNs
			cudnNs1 := createUDNNamespace(cs, "test-del-mixed-cudn1", nil)
			cudnNs2 := createUDNNamespace(cs, "test-del-mixed-cudn2", nil)
			udnNs1 := createUDNNamespace(cs, "test-del-mixed-udn1", udnLabel)
			udnNs2 := createUDNNamespace(cs, "test-del-mixed-udn2", udnLabel)

			cudnName1 := fmt.Sprintf("del-mixed-cudn1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("del-mixed-cudn2-%s", rand.String(5))
			udnName1 := "udn1"
			udnName2 := "udn2"

			DeferCleanup(func() {
				deleteCNC(cncName)
				// Only delete remaining networks (others deleted in test)
				deleteCUDN(cudnName1)
				deleteUDN(udnNs1.Name, udnName1)
				for _, ns := range []*corev1.Namespace{cudnNs1, cudnNs2, udnNs1, udnNs2} {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
			})

			By("creating 2 CUDNs (L3 + L2) and 2 UDNs (L3 + L2)")
			createLayer3PrimaryCUDN(cs, cudnName1, cudnLabel, cudnNs1.Name)
			createLayer2PrimaryCUDN(cs, cudnName2, cudnLabel, cudnNs2.Name)
			createLayer3PrimaryUDN(cs, udnNs1.Name, udnName1)
			createLayer2PrimaryUDN(cs, udnNs2.Name, udnName2)

			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, udnNs1.Name, udnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, udnNs2.Name, udnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with both selectors")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("verifying CNC has 4 networks initially")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2", "Layer3", "Layer2"})

			By("deleting L2 CUDN and L2 UDN")
			deleteCUDN(cudnName2)
			deleteUDN(udnNs2.Name, udnName2)

			By("verifying CNC has 2 L3 networks remaining")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer3"})

			By("deleting L3 CUDN and L3 UDN")
			deleteCUDN(cudnName1)
			deleteUDN(udnNs1.Name, udnName1)

			By("verifying CNC has no networks remaining")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 0)
			verifyCNCSubnetAnnotationContent(cncName, []string{})
		})
	})

	// ===========================================
	// Group 6: CNC Selector Update - CNC spec.networkSelectors changed (4 tests)
	// ===========================================
	Context("when CNC selector is updated", func() {
		It("widening then narrowing CUDN selector - count increases then decreases", func() {
			cncName := generateCNCName()
			commonLabel := map[string]string{"test-cudn-sel": "true"}
			specificLabel := map[string]string{"test-cudn-sel": "true", "specific": "true"}

			ns1 := createUDNNamespace(cs, "test-cudn-sel-ns1", nil)
			ns2 := createUDNNamespace(cs, "test-cudn-sel-ns2", nil)
			cudnName1 := fmt.Sprintf("cudn-sel1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("cudn-sel2-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 CUDNs - both with common label, second also has specific label")
			createLayer3PrimaryCUDN(cs, cudnName1, commonLabel, ns1.Name)
			createLayer2PrimaryCUDN(cs, cudnName2, specificLabel, ns2.Name)
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with specific selector (matches only second CUDN)")
			createOrUpdateCNC(cs, cncName, specificLabel, nil)

			By("verifying CNC has 1 network initially")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})

			By("widening CNC selector to common label - count increases")
			createOrUpdateCNC(cs, cncName, commonLabel, nil)

			By("verifying CNC now has 2 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"})

			By("narrowing CNC selector back to specific - count decreases")
			createOrUpdateCNC(cs, cncName, specificLabel, nil)

			By("verifying CNC now has 1 network")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})
		})

		It("widening then narrowing PUDN namespace selector - count increases then decreases", func() {
			cncName := generateCNCName()
			commonLabel := map[string]string{"test-pudn-sel": "true"}
			specificLabel := map[string]string{"test-pudn-sel": "true", "specific": "true"}

			ns1 := createUDNNamespace(cs, "test-pudn-sel-ns1", commonLabel)
			ns2 := createUDNNamespace(cs, "test-pudn-sel-ns2", specificLabel)
			udnName1 := "udn1"
			udnName2 := "udn2"

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteUDN(ns1.Name, udnName1)
				deleteUDN(ns2.Name, udnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 UDNs in namespaces - both with common label, second also has specific")
			createLayer3PrimaryUDN(cs, ns1.Name, udnName1)
			createLayer2PrimaryUDN(cs, ns2.Name, udnName2)
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns1.Name, udnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns2.Name, udnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with specific selector (matches only second namespace)")
			createOrUpdateCNC(cs, cncName, nil, specificLabel)

			By("verifying CNC has 1 network initially")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})

			By("widening CNC selector to common label - count increases")
			createOrUpdateCNC(cs, cncName, nil, commonLabel)

			By("verifying CNC now has 2 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"})

			By("narrowing CNC selector back to specific - count decreases")
			createOrUpdateCNC(cs, cncName, nil, specificLabel)

			By("verifying CNC now has 1 network")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})
		})

		It("adding and removing PUDN selector from CNC - count increases then decreases", func() {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-toggle-pudn-sel": "true"}
			udnLabel := map[string]string{"test-toggle-pudn-sel": "true"}

			cudnNs := createUDNNamespace(cs, "test-toggle-pudn-sel-cudn", nil)
			udnNs := createUDNNamespace(cs, "test-toggle-pudn-sel-udn", udnLabel)
			cudnName := fmt.Sprintf("toggle-pudn-sel-cudn-%s", rand.String(5))
			udnName := "udn1"

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName)
				deleteUDN(udnNs.Name, udnName)
				cs.CoreV1().Namespaces().Delete(context.Background(), cudnNs.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), udnNs.Name, metav1.DeleteOptions{})
			})

			By("creating L3 CUDN and L2 UDN")
			createLayer3PrimaryCUDN(cs, cudnName, cudnLabel, cudnNs.Name)
			createLayer2PrimaryUDN(cs, udnNs.Name, udnName)
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, udnNs.Name, udnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with only CUDN selector")
			createOrUpdateCNC(cs, cncName, cudnLabel, nil)

			By("verifying CNC has 1 network initially (CUDN only)")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

			By("adding PUDN selector to CNC - count increases")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("verifying CNC now has 2 networks (CUDN + PUDN)")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"})

			By("removing PUDN selector from CNC - count decreases")
			createOrUpdateCNC(cs, cncName, cudnLabel, nil)

			By("verifying CNC now has 1 network (CUDN only)")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})
		})

		It("adding and removing CUDN selector from CNC - count increases then decreases", func() {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-toggle-cudn-sel": "true"}
			udnLabel := map[string]string{"test-toggle-cudn-sel": "true"}

			cudnNs := createUDNNamespace(cs, "test-toggle-cudn-sel-cudn", nil)
			udnNs := createUDNNamespace(cs, "test-toggle-cudn-sel-udn", udnLabel)
			cudnName := fmt.Sprintf("toggle-cudn-sel-cudn-%s", rand.String(5))
			udnName := "udn1"

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName)
				deleteUDN(udnNs.Name, udnName)
				cs.CoreV1().Namespaces().Delete(context.Background(), cudnNs.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), udnNs.Name, metav1.DeleteOptions{})
			})

			By("creating L3 CUDN and L2 UDN")
			createLayer3PrimaryCUDN(cs, cudnName, cudnLabel, cudnNs.Name)
			createLayer2PrimaryUDN(cs, udnNs.Name, udnName)
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, udnNs.Name, udnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with only PUDN selector")
			createOrUpdateCNC(cs, cncName, nil, udnLabel)

			By("verifying CNC has 1 network initially (PUDN only)")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})

			By("adding CUDN selector to CNC - count increases")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("verifying CNC now has 2 networks (CUDN + PUDN)")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"})

			By("removing CUDN selector from CNC - count decreases")
			createOrUpdateCNC(cs, cncName, nil, udnLabel)

			By("verifying CNC now has 1 network (PUDN only)")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})

			By("changing PUDN selector to non-matching label - count decreases to 0")
			createOrUpdateCNC(cs, cncName, nil, map[string]string{"nonexistent": "label"})

			By("verifying CNC has no networks remaining")
			verifyCNCHasOnlyTunnelIDAnnotation(cncName) // No networks match, so subnet annotation is empty
			verifyCNCSubnetAnnotationNetworkCount(cncName, 0)
		})
	})

	// ===========================================
	// Group 7: Label Mutation - network/namespace labels changed (2 tests)
	// ===========================================
	Context("when network or namespace labels are mutated", func() {
		It("CUDN label mutation - adding then removing label changes CNC count", func() {
			cncName := generateCNCName()
			cncLabel := map[string]string{"test-cudn-label": "true"}

			ns1 := createUDNNamespace(cs, "test-cudn-label-ns1", nil)
			ns2 := createUDNNamespace(cs, "test-cudn-label-ns2", nil)
			cudnName1 := fmt.Sprintf("cudn-label1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("cudn-label2-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 CUDNs - first with matching label, second without")
			createLayer3PrimaryCUDN(cs, cudnName1, cncLabel, ns1.Name)
			createLayer2PrimaryCUDN(cs, cudnName2, map[string]string{"other": "label"}, ns2.Name)
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with CUDN selector")
			createOrUpdateCNC(cs, cncName, cncLabel, nil)

			By("verifying CNC has 1 network initially")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

			By("adding matching label to second CUDN - count increases")
			_, err := e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", cudnName2, "test-cudn-label=true")
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC now has 2 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"})

			By("removing matching label from second CUDN - count decreases")
			_, err = e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", cudnName2, "test-cudn-label-")
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC now has 1 network")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

			By("removing matching label from first CUDN - count decreases to 0")
			_, err = e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", cudnName1, "test-cudn-label-")
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC has no networks remaining")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 0)
			verifyCNCSubnetAnnotationContent(cncName, []string{})
		})

		It("namespace label mutation - adding then removing label changes CNC count", func() {
			cncName := generateCNCName()
			cncLabel := map[string]string{"test-ns-label": "true"}

			ns1 := createUDNNamespace(cs, "test-ns-label-ns1", cncLabel)
			ns2 := createUDNNamespace(cs, "test-ns-label-ns2", nil) // no matching label initially
			udnName1 := "udn1"
			udnName2 := "udn2"

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteUDN(ns1.Name, udnName1)
				deleteUDN(ns2.Name, udnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 UDNs - first in namespace with matching label, second without")
			createLayer3PrimaryUDN(cs, ns1.Name, udnName1)
			createLayer2PrimaryUDN(cs, ns2.Name, udnName2)
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns1.Name, udnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns2.Name, udnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC with PUDN namespace selector")
			createOrUpdateCNC(cs, cncName, nil, cncLabel)

			By("verifying CNC has 1 network initially")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

			By("adding matching label to second namespace - count increases")
			_, err := cs.CoreV1().Namespaces().Patch(context.Background(), ns2.Name,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"test-ns-label":"true"}}}`),
				metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC now has 2 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"})

			By("removing matching label from second namespace - count decreases")
			_, err = cs.CoreV1().Namespaces().Patch(context.Background(), ns2.Name,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"test-ns-label":null}}}`),
				metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC now has 1 network")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3"})

			By("removing matching label from first namespace - count decreases to 0")
			_, err = cs.CoreV1().Namespaces().Patch(context.Background(), ns1.Name,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"test-ns-label":null}}}`),
				metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CNC has no networks remaining")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 0)
			verifyCNCSubnetAnnotationContent(cncName, []string{})
		})
	})

	// ===========================================
	// Group 8: Multiple CNCs - multiple CNCs in cluster (3 tests)
	// ===========================================
	Context("when multiple CNCs exist", func() {
		// Second CNC connect subnet configuration (must be different from first CNC)
		const (
			cnc2ConnectSubnetIPv4CIDR   = "192.169.0.0/16"
			cnc2ConnectSubnetIPv4Prefix = 24
			cnc2ConnectSubnetIPv6CIDR   = "fd00:11::/112"
			cnc2ConnectSubnetIPv6Prefix = 120
		)

		It("two CNCs with non-overlapping selectors - each tracks its own networks", func() {
			cncName1 := generateCNCName()
			cncName2 := generateCNCName()
			label1 := map[string]string{"test-multi-cnc-1": "true"}
			label2 := map[string]string{"test-multi-cnc-2": "true"}

			ns1 := createUDNNamespace(cs, "test-multi-cnc-ns1", nil)
			ns2 := createUDNNamespace(cs, "test-multi-cnc-ns2", nil)
			cudnName1 := fmt.Sprintf("multi-cnc-cudn1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("multi-cnc-cudn2-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName1)
				deleteCNC(cncName2)
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 CUDNs with different labels")
			createLayer3PrimaryCUDN(cs, cudnName1, label1, ns1.Name)
			createLayer2PrimaryCUDN(cs, cudnName2, label2, ns2.Name)
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating first CNC matching first CUDN (with first connect subnet)")
			createOrUpdateCNCWithSubnets(cncName1, label1, nil, generateConnectSubnets(cs))

			By("creating second CNC matching second CUDN (with different connect subnet)")
			createOrUpdateCNCWithSubnets(cncName2, label2, nil, generateConnectSubnetsWithCIDRs(cs, cnc2ConnectSubnetIPv4CIDR, cnc2ConnectSubnetIPv4Prefix, cnc2ConnectSubnetIPv6CIDR, cnc2ConnectSubnetIPv6Prefix))

			By("verifying first CNC has only first network")
			verifyCNCHasBothAnnotations(cncName1)
			verifyCNCSubnetAnnotationNetworkCount(cncName1, 1)
			verifyCNCSubnetAnnotationContent(cncName1, []string{"Layer3"})

			By("verifying second CNC has only second network")
			verifyCNCHasBothAnnotations(cncName2)
			verifyCNCSubnetAnnotationNetworkCount(cncName2, 1)
			verifyCNCSubnetAnnotationContent(cncName2, []string{"Layer2"})

			By("verifying CNCs have different tunnel IDs")
			annotations1, err := getCNCAnnotations(cncName1)
			Expect(err).NotTo(HaveOccurred())
			annotations2, err := getCNCAnnotations(cncName2)
			Expect(err).NotTo(HaveOccurred())
			Expect(annotations1[ovnConnectRouterTunnelKeyAnnotation]).NotTo(Equal(annotations2[ovnConnectRouterTunnelKeyAnnotation]),
				"CNCs should have different tunnel IDs")
		})

		It("two CNCs matching same network - both track the network (this works but is usually treated as misconfiguration)", func() {
			cncName1 := generateCNCName()
			cncName2 := generateCNCName()
			sharedLabel := map[string]string{"test-shared-cudn": "true"}

			ns := createUDNNamespace(cs, "test-shared-cudn-ns", nil)
			cudnName := fmt.Sprintf("shared-cudn-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName1)
				deleteCNC(cncName2)
				deleteCUDN(cudnName)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
			})

			By("creating a CUDN with shared label")
			createLayer3PrimaryCUDN(cs, cudnName, sharedLabel, ns.Name)
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating first CNC matching the CUDN (with first connect subnet)")
			createOrUpdateCNCWithSubnets(cncName1, sharedLabel, nil, generateConnectSubnets(cs))

			By("creating second CNC also matching the CUDN (with different connect subnet)")
			createOrUpdateCNCWithSubnets(cncName2, sharedLabel, nil, generateConnectSubnetsWithCIDRs(cs, cnc2ConnectSubnetIPv4CIDR, cnc2ConnectSubnetIPv4Prefix, cnc2ConnectSubnetIPv6CIDR, cnc2ConnectSubnetIPv6Prefix))

			By("verifying both CNCs have the network in their annotations")
			verifyCNCHasBothAnnotations(cncName1)
			verifyCNCSubnetAnnotationNetworkCount(cncName1, 1)
			verifyCNCSubnetAnnotationContent(cncName1, []string{"Layer3"})

			verifyCNCHasBothAnnotations(cncName2)
			verifyCNCSubnetAnnotationNetworkCount(cncName2, 1)
			verifyCNCSubnetAnnotationContent(cncName2, []string{"Layer3"})

			By("verifying CNCs have different tunnel IDs")
			annotations1, err := getCNCAnnotations(cncName1)
			Expect(err).NotTo(HaveOccurred())
			annotations2, err := getCNCAnnotations(cncName2)
			Expect(err).NotTo(HaveOccurred())
			Expect(annotations1[ovnConnectRouterTunnelKeyAnnotation]).NotTo(Equal(annotations2[ovnConnectRouterTunnelKeyAnnotation]),
				"CNCs should have different tunnel IDs")
		})

		It("deleting one CNC does not affect the other", func() {
			cncName1 := generateCNCName()
			cncName2 := generateCNCName()
			label1 := map[string]string{"test-cnc-delete-1": "true"}
			label2 := map[string]string{"test-cnc-delete-2": "true"}

			ns1 := createUDNNamespace(cs, "test-cnc-delete-ns1", nil)
			ns2 := createUDNNamespace(cs, "test-cnc-delete-ns2", nil)
			cudnName1 := fmt.Sprintf("cnc-delete-cudn1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("cnc-delete-cudn2-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName2) // cncName1 deleted in test
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 CUDNs with different labels")
			createLayer3PrimaryCUDN(cs, cudnName1, label1, ns1.Name)
			createLayer2PrimaryCUDN(cs, cudnName2, label2, ns2.Name)
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating two CNCs with different selectors and different connect subnets")
			createOrUpdateCNCWithSubnets(cncName1, label1, nil, generateConnectSubnets(cs))
			createOrUpdateCNCWithSubnets(cncName2, label2, nil, generateConnectSubnetsWithCIDRs(cs, cnc2ConnectSubnetIPv4CIDR, cnc2ConnectSubnetIPv4Prefix, cnc2ConnectSubnetIPv6CIDR, cnc2ConnectSubnetIPv6Prefix))

			By("verifying both CNCs have their networks")
			verifyCNCHasBothAnnotations(cncName1)
			verifyCNCSubnetAnnotationNetworkCount(cncName1, 1)
			verifyCNCHasBothAnnotations(cncName2)
			verifyCNCSubnetAnnotationNetworkCount(cncName2, 1)

			By("deleting first CNC")
			deleteCNC(cncName1)

			By("verifying second CNC is unaffected")
			verifyCNCHasBothAnnotations(cncName2)
			verifyCNCSubnetAnnotationNetworkCount(cncName2, 1)
			verifyCNCSubnetAnnotationContent(cncName2, []string{"Layer2"})
		})
	})

	// ===========================================
	// Group 9: CNC Lifecycle - CNC deletion and recreation
	// ===========================================
	Context("CNC lifecycle", func() {
		It("CNC deletion and recreation - tunnel ID is allocated after recreate", func() {
			cncName := generateCNCName()
			cncLabel := map[string]string{"test-cnc-lifecycle": "true"}

			ns := createUDNNamespace(cs, "test-cnc-lifecycle-ns", nil)
			cudnName := fmt.Sprintf("cnc-lifecycle-cudn-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
			})

			By("creating a CUDN")
			createLayer3PrimaryCUDN(cs, cudnName, cncLabel, ns.Name)
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC")
			createOrUpdateCNC(cs, cncName, cncLabel, nil)

			By("verifying CNC has network and tunnel ID")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			originalTunnelID := getCNCTunnelID(cncName)
			Expect(originalTunnelID).NotTo(BeEmpty())

			By("deleting CNC")
			deleteCNC(cncName)

			By("verifying CNC is gone")
			Eventually(func() bool {
				_, err := getCNCAnnotations(cncName)
				return err != nil
			}, 30*time.Second, time.Second).Should(BeTrue())

			By("recreating CNC with same name")
			createOrUpdateCNC(cs, cncName, cncLabel, nil)

			By("verifying CNC has network again")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)

			By("verifying tunnel ID is newly allocated after CNC recreation")
			newTunnelID := getCNCTunnelID(cncName)
			Expect(newTunnelID).NotTo(BeEmpty())
			Expect(newTunnelID).NotTo(Equal(originalTunnelID))
		})

		It("tunnel ID is stable across CNC spec updates", func() {
			cncName := generateCNCName()
			label1 := map[string]string{"test-tunnel-stable-1": "true"}
			label2 := map[string]string{"test-tunnel-stable-2": "true"}

			ns1 := createUDNNamespace(cs, "test-tunnel-stable-ns1", nil)
			ns2 := createUDNNamespace(cs, "test-tunnel-stable-ns2", nil)
			cudnName1 := fmt.Sprintf("tunnel-stable-cudn1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("tunnel-stable-cudn2-%s", rand.String(5))

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				cs.CoreV1().Namespaces().Delete(context.Background(), ns1.Name, metav1.DeleteOptions{})
				cs.CoreV1().Namespaces().Delete(context.Background(), ns2.Name, metav1.DeleteOptions{})
			})

			By("creating 2 CUDNs with different labels")
			createLayer3PrimaryCUDN(cs, cudnName1, label1, ns1.Name)
			createLayer2PrimaryCUDN(cs, cudnName2, label2, ns2.Name)
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())

			By("creating CNC matching first CUDN")
			createOrUpdateCNC(cs, cncName, label1, nil)

			By("verifying CNC has network and recording tunnel ID")
			verifyCNCHasBothAnnotations(cncName)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			originalTunnelID := getCNCTunnelID(cncName)

			By("updating CNC to match second CUDN instead")
			createOrUpdateCNC(cs, cncName, label2, nil)

			By("verifying CNC now has second network")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"})

			By("verifying tunnel ID is unchanged")
			newTunnelID := getCNCTunnelID(cncName)
			Expect(newTunnelID).To(Equal(originalTunnelID),
				"tunnel ID should be stable across spec updates")

			By("updating CNC to match both CUDNs")
			// Add label1 to second CUDN so we can match both
			_, err := e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", cudnName2, "test-tunnel-stable-1=true")
			Expect(err).NotTo(HaveOccurred())
			createOrUpdateCNC(cs, cncName, label1, nil)

			By("verifying CNC now has both networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)

			By("verifying tunnel ID is still unchanged")
			finalTunnelID := getCNCTunnelID(cncName)
			Expect(finalTunnelID).To(Equal(originalTunnelID),
				"tunnel ID should remain stable")
		})

	})

	// ===========================================
	// Group 10: Full Lifecycle Workflow (1 comprehensive test)
	// ===========================================
	Context("full lifecycle workflow", func() {
		It("comprehensive workflow - create, add, update, remove networks through CNC lifecycle", func() {
			cncName := generateCNCName()
			cudnLabel := map[string]string{"test-lifecycle": "true"}
			udnLabel := map[string]string{"test-lifecycle": "true"}
			var expectedTopologies []string

			// Create namespaces
			cudnNs1 := createUDNNamespace(cs, "lifecycle-cudn-ns1", nil)
			cudnNs2 := createUDNNamespace(cs, "lifecycle-cudn-ns2", nil)
			udnNs1 := createUDNNamespace(cs, "lifecycle-udn-ns1", udnLabel)
			udnNs2 := createUDNNamespace(cs, "lifecycle-udn-ns2", udnLabel)

			cudnName1 := fmt.Sprintf("lifecycle-cudn1-%s", rand.String(5))
			cudnName2 := fmt.Sprintf("lifecycle-cudn2-%s", rand.String(5))
			udnName1 := "udn1"
			udnName2 := "udn2"

			DeferCleanup(func() {
				deleteCNC(cncName)
				deleteCUDN(cudnName1)
				deleteCUDN(cudnName2)
				deleteUDN(udnNs1.Name, udnName1)
				deleteUDN(udnNs2.Name, udnName2)
				for _, ns := range []*corev1.Namespace{cudnNs1, cudnNs2, udnNs1, udnNs2} {
					cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
				}
			})

			// Phase 1: Create CNC with no matching networks
			By("Phase 1: Creating CNC with no matching networks yet")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)
			verifyCNCHasOnlyTunnelIDAnnotation(cncName)
			originalTunnelID := getCNCTunnelID(cncName)

			// Phase 2: Create first L3 CUDN - count goes to 1
			By("Phase 2: Creating first L3 CUDN")
			createLayer3PrimaryCUDN(cs, cudnName1, cudnLabel, cudnNs1.Name)
			expectedTopologies = append(expectedTopologies, "Layer3")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName1), 30*time.Second, time.Second).Should(Succeed())
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

			// Phase 3: Create first L2 UDN - count goes to 2
			By("Phase 3: Creating first L2 UDN")
			createLayer2PrimaryUDN(cs, udnNs1.Name, udnName1)
			expectedTopologies = append(expectedTopologies, "Layer2")
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, udnNs1.Name, udnName1), 30*time.Second, time.Second).Should(Succeed())
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

			// Phase 4: Create second L2 CUDN - count goes to 3
			By("Phase 4: Creating second L2 CUDN")
			createLayer2PrimaryCUDN(cs, cudnName2, cudnLabel, cudnNs2.Name)
			expectedTopologies = append(expectedTopologies, "Layer2")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnName2), 30*time.Second, time.Second).Should(Succeed())
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

			// Phase 5: Create second L3 UDN - count goes to 4
			By("Phase 5: Creating second L3 UDN")
			createLayer3PrimaryUDN(cs, udnNs2.Name, udnName2)
			expectedTopologies = append(expectedTopologies, "Layer3")
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, udnNs2.Name, udnName2), 30*time.Second, time.Second).Should(Succeed())
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)
			verifyCNCSubnetAnnotationContent(cncName, expectedTopologies)

			// Verify tunnel ID is stable
			By("Verifying tunnel ID unchanged after adding networks")
			Expect(getCNCTunnelID(cncName)).To(Equal(originalTunnelID))

			// Phase 6: Remove PUDN selector - count goes to 2 (only CUDNs remain)
			By("Phase 6: Removing PUDN selector from CNC")
			createOrUpdateCNC(cs, cncName, cudnLabel, nil)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer3", "Layer2"}) // cudnName1 is L3, cudnName2 is L2

			// Verify tunnel ID is stable
			By("Verifying tunnel ID unchanged after selector update")
			Expect(getCNCTunnelID(cncName)).To(Equal(originalTunnelID))

			// Phase 7: Delete one CUDN - count goes to 1
			By("Phase 7: Deleting first CUDN")
			deleteCUDN(cudnName1)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 1)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2"}) // only cudnName2 remains

			// Phase 8: Add PUDN selector back - count goes to 3 (1 CUDN + 2 UDNs)
			By("Phase 8: Adding PUDN selector back to CNC")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2", "Layer2", "Layer3"}) // cudnName2(L2), udn1(L2), udn2(L3)

			// Verify tunnel ID is stable
			By("Verifying tunnel ID unchanged after adding selector back")
			Expect(getCNCTunnelID(cncName)).To(Equal(originalTunnelID))

			// Phase 9: Remove label from namespace - UDN1 no longer matches - count goes to 2
			By("Phase 9: Removing label from first UDN namespace")
			_, err := cs.CoreV1().Namespaces().Patch(context.Background(), udnNs1.Name,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"test-lifecycle":null}}}`),
				metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)
			verifyCNCSubnetAnnotationContent(cncName, []string{"Layer2", "Layer3"}) // cudnName2(L2), udn2(L3)

			// Phase 10: Delete remaining networks - count goes to 0
			By("Phase 10: Deleting remaining networks")
			deleteCUDN(cudnName2)
			deleteUDN(udnNs2.Name, udnName2)
			verifyCNCSubnetAnnotationNetworkCount(cncName, 0)
			verifyCNCSubnetAnnotationContent(cncName, []string{})

			// Verify tunnel ID is stable even with no networks
			By("Verifying tunnel ID unchanged even with no networks")
			Expect(getCNCTunnelID(cncName)).To(Equal(originalTunnelID))

			// Final verification: CNC still exists with only tunnel ID
			By("Final: Verifying CNC has only tunnel ID annotation")
			verifyCNCHasOnlyTunnelIDAnnotation(cncName)

			By("Deleting CNC")
			deleteCNC(cncName)
		})
	})

	It("reports error on selected networks subnet overlap", func() {
		cncName := generateCNCName()
		testLabel := map[string]string{"test-udn": "true"}

		topology := "Layer3"
		udnName1 := fmt.Sprintf("test-udn-a-%s", rand.String(5))
		udnName2 := fmt.Sprintf("test-udn-b-%s", rand.String(5))
		// Use the same subnet(s) for both UDNs to force overlap across selected networks.
		overlapV4Subnet := "10.250.0.0/16"
		overlapV6Subnet := "fd98:1:1::/60"

		ns1 := createUDNNamespace(cs, "test-udn-a", testLabel)
		ns2 := createUDNNamespace(cs, "test-udn-b", testLabel)
		DeferCleanup(func() {
			deleteCNC(cncName)
			deleteUDN(ns1.Name, udnName1)
			deleteUDN(ns2.Name, udnName2)
			deleteNamespace(cs, ns1.Name)
			deleteNamespace(cs, ns2.Name)
		})

		By(fmt.Sprintf("creating 2 %s primary UDNs with overlapping subnets", topology))
		createPrimaryUDNWithSubnets(cs, ns1.Name, udnName1, topology, overlapV4Subnet, overlapV6Subnet)
		createPrimaryUDNWithSubnets(cs, ns2.Name, udnName2, topology, overlapV4Subnet, overlapV6Subnet)

		By("waiting for UDNs to be ready")
		Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns1.Name, udnName1), 30*time.Second, time.Second).Should(Succeed())
		Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, ns2.Name, udnName2), 30*time.Second, time.Second).Should(Succeed())

		By("creating a CNC with PUDN selector matching both namespaces")
		createOrUpdateCNC(cs, cncName, nil, testLabel)

		By("verifying CNC status reports selected networks subnet overlap")
		verifyCNCAcceptedConditionFailed(cncName, "selected networks have overlapping subnets")
	})
})

// ============================================================================
// OVN Database Side Testing - End-to-End Connectivity Validation
// ============================================================================
var _ = Describe("ClusterNetworkConnect OVN-Kubernetes Controller", feature.NetworkConnect, func() {
	f := wrappedTestFramework("cnc-ovndb")
	// disable automatic namespace creation, we need to add the required UDN label
	f.SkipNamespaceCreation = true

	var (
		cs clientset.Interface
	)

	BeforeEach(func() {
		cs = f.ClientSet
	})

	// httpServerPodConfig returns a podConfiguration for an HTTP server pod
	httpServerPodConfig := func(podName, namespace string) podConfiguration {
		cfg := *podConfig(podName, withCommand(func() []string {
			return httpServerContainerCmd(8080)
		}))
		cfg.namespace = namespace
		return cfg
	}

	// getPrimaryNetworkPodIPs gets the pod IPs for supported IP families on a given primary network
	// Returns a slice of IP strings based on cluster's supported IP families
	// Uses Eventually to wait for OVN annotations to be populated
	getPrimaryNetworkPodIPs := func(namespace, podName, networkName string) []string {
		var ips []string
		Eventually(func() error {
			ips = make([]string, 0, 2)
			for _, family := range getSupportedIPFamiliesSlice(cs) {
				ip, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(cs, namespace, podName, networkName, family)
				if err != nil {
					return err
				}
				if ip != "" {
					ips = append(ips, ip)
				}
			}
			if len(ips) == 0 {
				return fmt.Errorf("pod %s/%s has no IPs on network %s yet", namespace, podName, networkName)
			}
			return nil
		}, 2*time.Minute, 5*time.Second).Should(Succeed(),
			fmt.Sprintf("waiting for pod %s/%s to have IPs on network %s", namespace, podName, networkName))
		return ips
	}

	// checkConnectivity checks connectivity from one pod to another
	// Returns true if connection succeeds/fails as expected
	checkConnectivity := func(fromNamespace, fromPodName, toIP string, expectSuccess bool) bool {
		// net.JoinHostPort properly handles IPv6 addresses by adding brackets
		url := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(toIP, "8080"))
		stdout, err := e2ekubectl.RunKubectl(fromNamespace, "exec", fromPodName, "--",
			"curl", "--connect-timeout", "1", "-s", "-o", "/dev/null", "-w", "%{http_code}", url)
		if expectSuccess {
			return err == nil && stdout == "200"
		}
		return err != nil || stdout != "200"
	}

	// verifyCrossNetworkConnectivity verifies connectivity from a set of pods to another set
	// Supports dual-stack by testing all IPs for each pod
	// Uses Eventually for expected success
	// For expected failure: first waits for connectivity to fail (Eventually), then verifies it stays failed (Consistently)
	verifyCrossNetworkConnectivity := func(fromPods map[string]*corev1.Pod, toPodIPs map[string][]string, expectSuccess bool) {
		for fromName, fromPod := range fromPods {
			for toName, toIPs := range toPodIPs {
				for _, toIP := range toIPs {
					msg := fmt.Sprintf("cross-network connectivity from %s to %s (%s) expectSuccess=%v", fromName, toName, toIP, expectSuccess)
					if expectSuccess {
						Eventually(func() bool {
							return checkConnectivity(fromPod.Namespace, fromPod.Name, toIP, true)
						}, 10*time.Second, 1*time.Second).Should(BeTrue(), msg)
					} else {
						// First wait for connectivity to fail (OVN flows take time to update)
						Eventually(func() bool {
							return checkConnectivity(fromPod.Namespace, fromPod.Name, toIP, false)
						}, 5*time.Second, 1*time.Second).Should(BeTrue(), msg+" (waiting for failure)")
						// Then verify it stays failed consistently
						Consistently(func() bool {
							return checkConnectivity(fromPod.Namespace, fromPod.Name, toIP, false)
						}, 10*time.Second, 1*time.Second).Should(BeTrue(), msg+" (consistent failure)")
					}
				}
			}
		}
	}

	// verifyFullMeshConnectivity verifies connectivity from source pods to all target pod IPs
	// Uses Eventually for expected success, Eventually+Consistently for expected failure
	verifyFullMeshConnectivity := func(srcPods map[string]*corev1.Pod, dstPodIPs map[string][]string, expectSuccess bool) {
		for fromName, fromPod := range srcPods {
			for toName, toIPs := range dstPodIPs {
				if fromName == toName {
					continue // Skip self-connectivity
				}
				for _, toIP := range toIPs {
					msg := fmt.Sprintf("connectivity from %s to %s (%s) expectSuccess=%v", fromName, toName, toIP, expectSuccess)
					if expectSuccess {
						Eventually(func() bool {
							return checkConnectivity(fromPod.Namespace, fromPod.Name, toIP, true)
						}, 10*time.Second, 1*time.Second).Should(BeTrue(), msg)
					} else {
						// First wait for connectivity to fail (OVN flows take time to update)
						Eventually(func() bool {
							return checkConnectivity(fromPod.Namespace, fromPod.Name, toIP, false)
						}, 5*time.Second, 1*time.Second).Should(BeTrue(), msg+" (waiting for failure)")
						// Then verify it stays failed consistently
						Consistently(func() bool {
							return checkConnectivity(fromPod.Namespace, fromPod.Name, toIP, false)
						}, 10*time.Second, 1*time.Second).Should(BeTrue(), msg+" (consistent failure)")
					}
				}
			}
		}
	}

	/*
	   This test validates end-to-end connectivity through CNC (ClusterNetworkConnect).

	   Test Scenario:
	   - Create 2 CUDNs: "black" (L3) and "white" (L2), each serving 2 namespaces with pods on different nodes
	   - Create 2 UDNs: "blue" (L3) and "green" (L2), each in its own namespace with 2 pods on different nodes
	   - Initially verify pods cannot communicate across different networks
	   - Create CNC selecting all 4 networks, verify cross-network connectivity (same-node and cross-node)
	   - Test all network deselection/reselection methods:
	     * UDN via namespace label change (blue - L3)
	     * UDN via CNC selector update (green - L2)
	     * CUDN via network label change (black - L3)
	     * CUDN via CNC selector update (black+white)
	   - Verify proper isolation when networks are disconnected
	   - Test CNC deletion and re-creation

	   Steps:
	   1.  Create 2 CUDNs: black (L3) and white (L2), each with 2 namespaces and pods on different nodes
	   2.  Create 2 UDNs: blue (L3) and green (L2), each with 2 pods on different nodes
	   3.  Verify initial isolation - pods cannot talk across networks
	   4.  Create CNC selecting all 4 networks
	   5.  Verify CNC annotations are set correctly (subnet allocation)
	   6.  Verify pods can communicate across all networks (same-node and cross-node)
	   7.  Deselect blue UDN (L3) via namespace label removal
	   8.  Verify blue pods isolated, other networks still connected
	   9.  Deselect green UDN (L2) via CNC selector update (remove PUDN selector)
	   10. Verify green pods isolated, CUDNs still connected
	   11. Re-select green UDN via CNC selector update (blue still deselected - no label)
	   12. Re-select blue UDN via namespace label restoration
	   13. Deselect black CUDN (L3) via CUDN label removal
	   14. Verify black pods isolated, other networks still connected
	   15. Re-select black CUDN via CUDN label restoration
	   16. Verify black pods can communicate again
	   17. Deselect both CUDNs (black+white) via CNC selector update (remove CUDN selector)
	   18. Verify CUDN pods isolated, UDNs still connected
	   19. Re-select CUDNs via CNC selector update
	   20. Verify CUDN pods can communicate again
	   21. Delete CNC
	   22. Verify all cross-network connectivity disabled
	   23. Re-create CNC
	   24. Verify all cross-network connectivity restored
	*/
	Context("End-to-end connectivity validation", func() {
		const nodeHostnameKey = "kubernetes.io/hostname"

		It("should manage cross-network connectivity through CNC lifecycle", func() {
			// Test identifiers
			testID := rand.String(5)
			cncName := generateCNCName()

			// Get 2 schedulable nodes for cross-node testing
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodes.Items)).To(BeNumerically(">=", 2), "test requires at least 2 schedulable nodes")
			node1Name, node2Name := nodes.Items[0].Name, nodes.Items[1].Name

			// Network names
			blackCUDN := fmt.Sprintf("black-cudn-%s", testID)
			whiteCUDN := fmt.Sprintf("white-cudn-%s", testID)
			blueUDN := "blue-udn"
			greenUDN := "green-udn"

			// Namespace names (fixed for predictability in CUDN selectors)
			blackNs0 := fmt.Sprintf("black-ns-0-%s", testID)
			blackNs1 := fmt.Sprintf("black-ns-1-%s", testID)
			whiteNs0 := fmt.Sprintf("white-ns-0-%s", testID)
			whiteNs1 := fmt.Sprintf("white-ns-1-%s", testID)
			blueNs := fmt.Sprintf("blue-ns-%s", testID)
			greenNs := fmt.Sprintf("green-ns-%s", testID)

			// Labels for CNC selection
			cudnLabel := map[string]string{"cnc-test": testID, "type": "cudn"}
			udnLabel := map[string]string{"cnc-test": testID, "type": "pudn"}

			// Store pods and their IPs for connectivity testing (supports dual-stack)
			pods := make(map[string]*corev1.Pod)
			podIPs := make(map[string][]string)

			// Cleanup
			DeferCleanup(func() {
				By("Cleanup: Deleting all test resources")
				deleteCNC(cncName)

				// Delete pods first
				for _, pod := range pods {
					_ = cs.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
				}

				// Delete UDNs
				deleteUDN(blueNs, blueUDN)
				deleteUDN(greenNs, greenUDN)

				// Delete CUDNs
				deleteCUDN(blackCUDN)
				deleteCUDN(whiteCUDN)

				// Delete namespaces
				deleteNamespace(cs, blackNs0)
				deleteNamespace(cs, blackNs1)
				deleteNamespace(cs, whiteNs0)
				deleteNamespace(cs, whiteNs1)
				deleteNamespace(cs, blueNs)
				deleteNamespace(cs, greenNs)
			})

			// =====================================================================
			// Step 1: Create 2 CUDNs (black, white) each with 2 namespaces and pods
			// =====================================================================
			By("1. Creating namespaces for black and white CUDNs")
			createUDNNamespaceWithName(cs, blackNs0, nil)
			createUDNNamespaceWithName(cs, blackNs1, nil)
			createUDNNamespaceWithName(cs, whiteNs0, nil)
			createUDNNamespaceWithName(cs, whiteNs1, nil)

			By("1. Creating black CUDN targeting black-ns-0 and black-ns-1")
			createLayer3PrimaryCUDNWithSubnets(cs, blackCUDN, cudnLabel, "10.128.0.0/16", "2014:100:200::0/60", blackNs0, blackNs1)

			By("1. Waiting for black CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, blackCUDN), 60*time.Second, time.Second).Should(Succeed())

			By("1. Creating white CUDN targeting white-ns-0 and white-ns-1")
			createLayer2PrimaryCUDNWithSubnets(cs, whiteCUDN, cudnLabel, "10.129.0.0/16", "2014:100:300::0/60", whiteNs0, whiteNs1)

			By("1. Waiting for white CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, whiteCUDN), 60*time.Second, time.Second).Should(Succeed())

			By("1. Creating pods in black CUDN namespaces (on different nodes for cross-node testing)")
			blackPodConfig0 := httpServerPodConfig("black-pod-0", blackNs0)
			blackPodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			blackPodConfig1 := httpServerPodConfig("black-pod-1", blackNs1)
			blackPodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["black-pod-0"] = runUDNPod(cs, blackNs0, blackPodConfig0, nil)
			pods["black-pod-1"] = runUDNPod(cs, blackNs1, blackPodConfig1, nil)
			podIPs["black-pod-0"] = getPrimaryNetworkPodIPs(blackNs0, "black-pod-0", blackCUDN)
			podIPs["black-pod-1"] = getPrimaryNetworkPodIPs(blackNs1, "black-pod-1", blackCUDN)

			By("1. Creating pods in white CUDN namespaces (on different nodes for cross-node testing)")
			whitePodConfig0 := httpServerPodConfig("white-pod-0", whiteNs0)
			whitePodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			whitePodConfig1 := httpServerPodConfig("white-pod-1", whiteNs1)
			whitePodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["white-pod-0"] = runUDNPod(cs, whiteNs0, whitePodConfig0, nil)
			pods["white-pod-1"] = runUDNPod(cs, whiteNs1, whitePodConfig1, nil)
			podIPs["white-pod-0"] = getPrimaryNetworkPodIPs(whiteNs0, "white-pod-0", whiteCUDN)
			podIPs["white-pod-1"] = getPrimaryNetworkPodIPs(whiteNs1, "white-pod-1", whiteCUDN)

			// =====================================================================
			// Step 2: Create 2 UDNs (blue, green) each with 1 namespace and 2 pods on different nodes
			// =====================================================================
			By("2. Creating namespaces for blue and green UDNs with PUDN labels")
			createUDNNamespaceWithName(cs, blueNs, udnLabel)
			createUDNNamespaceWithName(cs, greenNs, udnLabel)

			By("2. Creating blue UDN (L3)")
			createLayer3PrimaryUDNWithSubnets(cs, blueNs, blueUDN, "10.130.0.0/16", "2014:100:400::0/60")

			By("2. Waiting for blue UDN to be ready")
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, blueNs, blueUDN), 60*time.Second, time.Second).Should(Succeed())

			By("2. Creating green UDN (L2)")
			createLayer2PrimaryUDNWithSubnets(cs, greenNs, greenUDN, "10.131.0.0/16", "2014:100:500::0/60")

			By("2. Waiting for green UDN to be ready")
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, greenNs, greenUDN), 60*time.Second, time.Second).Should(Succeed())

			By("2. Creating pods in blue UDN namespace (on different nodes for same and cross-node testing)")
			bluePodConfig0 := httpServerPodConfig("blue-pod-0", blueNs)
			bluePodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			bluePodConfig1 := httpServerPodConfig("blue-pod-1", blueNs)
			bluePodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["blue-pod-0"] = runUDNPod(cs, blueNs, bluePodConfig0, nil)
			pods["blue-pod-1"] = runUDNPod(cs, blueNs, bluePodConfig1, nil)
			podIPs["blue-pod-0"] = getPrimaryNetworkPodIPs(blueNs, "blue-pod-0", blueUDN)
			podIPs["blue-pod-1"] = getPrimaryNetworkPodIPs(blueNs, "blue-pod-1", blueUDN)

			By("2. Creating pods in green UDN namespace (on different nodes for same and cross-node testing)")
			greenPodConfig0 := httpServerPodConfig("green-pod-0", greenNs)
			greenPodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			greenPodConfig1 := httpServerPodConfig("green-pod-1", greenNs)
			greenPodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["green-pod-0"] = runUDNPod(cs, greenNs, greenPodConfig0, nil)
			pods["green-pod-1"] = runUDNPod(cs, greenNs, greenPodConfig1, nil)
			podIPs["green-pod-0"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-0", greenUDN)
			podIPs["green-pod-1"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-1", greenUDN)

			// =====================================================================
			// Step 3: Verify initial isolation - pods cannot talk across networks
			// =====================================================================
			By("3. Verifying initial isolation - black pods cannot reach white pods")
			blackPods := map[string]*corev1.Pod{"black-pod-0": pods["black-pod-0"], "black-pod-1": pods["black-pod-1"]}
			whitePodIPs := map[string][]string{"white-pod-0": podIPs["white-pod-0"], "white-pod-1": podIPs["white-pod-1"]}
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, false)

			By("3. Verifying initial isolation - white pods cannot reach blue pods")
			whitePods := map[string]*corev1.Pod{"white-pod-0": pods["white-pod-0"], "white-pod-1": pods["white-pod-1"]}
			bluePodIPs := map[string][]string{"blue-pod-0": podIPs["blue-pod-0"], "blue-pod-1": podIPs["blue-pod-1"]}
			verifyCrossNetworkConnectivity(whitePods, bluePodIPs, false)

			By("3. Verifying initial isolation - blue pods cannot reach green pods")
			bluePods := map[string]*corev1.Pod{"blue-pod-0": pods["blue-pod-0"], "blue-pod-1": pods["blue-pod-1"]}
			greenPodIPs := map[string][]string{"green-pod-0": podIPs["green-pod-0"], "green-pod-1": podIPs["green-pod-1"]}
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, false)

			// =====================================================================
			// Step 4: Create CNC selecting all 4 networks
			// =====================================================================
			By("4. Creating CNC selecting all 4 networks (2 CUDNs + 2 UDNs)")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			// =====================================================================
			// Step 5: Verify CM annotations are set correctly
			// =====================================================================
			By("5. Verifying CNC has both tunnel ID and subnet annotations")
			verifyCNCHasBothAnnotations(cncName)

			By("5. Verifying CNC subnet annotation has 4 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// =====================================================================
			// Step 6: Verify pods can now communicate across all networks
			// =====================================================================
			By("6. Verifying pods can communicate across all connected networks")
			// Use one pod per network as source (targets include both nodes for same/cross-node coverage)
			srcPods := map[string]*corev1.Pod{
				"black-pod-0": pods["black-pod-0"],
				"white-pod-0": pods["white-pod-0"],
				"blue-pod-0":  pods["blue-pod-0"],
				"green-pod-0": pods["green-pod-0"],
			}
			verifyFullMeshConnectivity(srcPods, podIPs, true)

			// =====================================================================
			// Step 7: Deselect blue UDN by changing namespace label
			// =====================================================================
			By("7. Removing PUDN label from blue namespace to deselect blue UDN")
			_, err = cs.CoreV1().Namespaces().Patch(context.Background(), blueNs,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"type":null}}}`),
				metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("7. Verifying CNC subnet annotation now has 3 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)

			// =====================================================================
			// Step 8: Verify blue pods cannot talk to other network pods
			// =====================================================================
			By("8. Verifying blue pods cannot reach other network pods")
			verifyCrossNetworkConnectivity(bluePods, whitePodIPs, false)
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, false)

			By("8. Verifying other networks can still communicate with each other")
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, true)
			verifyCrossNetworkConnectivity(whitePods, greenPodIPs, true)

			// =====================================================================
			// Step 9: Deselect green UDN by updating CNC to remove PUDN selector
			// =====================================================================
			By("9. Updating CNC to remove PUDN selector (deselects green UDN)")
			createOrUpdateCNC(cs, cncName, cudnLabel, nil)

			By("9. Verifying CNC subnet annotation now has 2 networks (only CUDNs)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)

			// =====================================================================
			// Step 10: Verify green pods cannot talk to other network pods
			// =====================================================================
			greenPods := map[string]*corev1.Pod{"green-pod-0": pods["green-pod-0"], "green-pod-1": pods["green-pod-1"]}
			By("10. Verifying green pods cannot reach other network pods")
			verifyCrossNetworkConnectivity(greenPods, whitePodIPs, false)
			verifyCrossNetworkConnectivity(greenPods, bluePodIPs, false)

			By("10. Verifying black and white CUDNs can still communicate")
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, true)
			verifyCrossNetworkConnectivity(whitePods, map[string][]string{"black-pod-0": podIPs["black-pod-0"]}, true)

			// =====================================================================
			// Step 11: Re-select green UDN via CNC selector update
			// (blue still deselected - doesn't have namespace label from step 7)
			// =====================================================================
			By("11. Updating CNC to add PUDN selector back (re-selects green UDN)")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("11. Verifying CNC subnet annotation has 3 networks (green re-selected, blue still deselected)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)

			By("11. Verifying green pods can communicate with black and white CUDNs pods")
			blackPodIPs := map[string][]string{"black-pod-0": podIPs["black-pod-0"], "black-pod-1": podIPs["black-pod-1"]}
			verifyCrossNetworkConnectivity(greenPods, whitePodIPs, true)
			verifyCrossNetworkConnectivity(greenPods, blackPodIPs, true)

			// =====================================================================
			// Step 12: Re-select blue UDN via namespace label
			// =====================================================================
			By("12. Adding PUDN label back to blue namespace (re-selects blue UDN)")
			_, err = cs.CoreV1().Namespaces().Patch(context.Background(), blueNs,
				types.MergePatchType,
				[]byte(`{"metadata":{"labels":{"type":"pudn"}}}`),
				metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("12. Verifying CNC subnet annotation has 4 networks again")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			By("12. Verifying blue pods can communicate with other network pods")
			verifyCrossNetworkConnectivity(bluePods, whitePodIPs, true)
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, true)

			// =====================================================================
			// Step 13: Deselect black CUDN via CUDN label change
			// =====================================================================
			By("13. Removing CNC-matching label from black CUDN (deselects black CUDN)")
			_, err = e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", blackCUDN, "type-")
			Expect(err).NotTo(HaveOccurred())

			By("13. Verifying CNC subnet annotation now has 3 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 3)

			// =====================================================================
			// Step 14: Verify black pods cannot talk with other networks
			// =====================================================================
			By("14. Verifying black pods cannot reach other network pods")
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, false)
			verifyCrossNetworkConnectivity(blackPods, bluePodIPs, false)
			verifyCrossNetworkConnectivity(blackPods, greenPodIPs, false)

			By("14. Verifying white, blue, and green networks can still communicate")
			verifyCrossNetworkConnectivity(whitePods, bluePodIPs, true)
			verifyCrossNetworkConnectivity(whitePods, greenPodIPs, true)
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, true)

			// =====================================================================
			// Step 15: Re-select black CUDN via CUDN label
			// =====================================================================
			By("15. Adding CNC-matching label back to black CUDN (re-selects black CUDN)")
			_, err = e2ekubectl.RunKubectl("", "label", "clusteruserdefinednetwork", blackCUDN, "type=cudn")
			Expect(err).NotTo(HaveOccurred())

			By("15. Verifying CNC subnet annotation has 4 networks again")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// =====================================================================
			// Step 16: Verify black pods can communicate again
			// =====================================================================
			By("16. Verifying black pods can reach other network pods again")
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, true)
			verifyCrossNetworkConnectivity(blackPods, bluePodIPs, true)
			verifyCrossNetworkConnectivity(blackPods, greenPodIPs, true)

			// =====================================================================
			// Step 17: Deselect both CUDNs (black+white) via CNC selector update
			// =====================================================================
			By("17. Updating CNC to remove CUDN selector (deselects black and white CUDNs)")
			createOrUpdateCNC(cs, cncName, nil, udnLabel)

			By("17. Verifying CNC subnet annotation now has 2 networks (only UDNs)")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 2)

			// =====================================================================
			// Step 18: Verify both CUDN pods cannot talk with other networks
			// =====================================================================
			By("18. Verifying black and white CUDN pods cannot reach UDN pods")
			verifyCrossNetworkConnectivity(blackPods, bluePodIPs, false)
			verifyCrossNetworkConnectivity(blackPods, greenPodIPs, false)
			verifyCrossNetworkConnectivity(whitePods, bluePodIPs, false)
			verifyCrossNetworkConnectivity(whitePods, greenPodIPs, false)

			By("18. Verifying blue and green UDNs can still communicate")
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, true)

			// =====================================================================
			// Step 19: Re-select CUDNs via CNC selector update
			// =====================================================================
			By("19. Updating CNC to add CUDN selector back (re-selects black and white CUDNs)")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("19. Verifying CNC subnet annotation has 4 networks again")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// =====================================================================
			// Step 20: Verify CUDN pods can communicate again
			// =====================================================================
			By("20. Verifying black and white CUDN pods can reach other network pods again")
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, true)
			verifyCrossNetworkConnectivity(blackPods, bluePodIPs, true)
			verifyCrossNetworkConnectivity(whitePods, greenPodIPs, true)

			// =====================================================================
			// Step 21: Delete CNC
			// =====================================================================
			By("21. Deleting CNC")
			deleteCNC(cncName)

			By("21. Waiting for CNC deletion to complete")
			Eventually(func() bool {
				_, err := getCNCAnnotations(cncName)
				return err != nil
			}, 60*time.Second, 2*time.Second).Should(BeTrue())

			// =====================================================================
			// Step 22: Verify all pods cannot talk to pods in other networks
			// =====================================================================
			By("22. Verifying all cross-network connectivity is disabled")
			verifyCrossNetworkConnectivity(blackPods, whitePodIPs, false)
			verifyCrossNetworkConnectivity(blackPods, bluePodIPs, false)
			verifyCrossNetworkConnectivity(blackPods, greenPodIPs, false)
			verifyCrossNetworkConnectivity(whitePods, bluePodIPs, false)
			verifyCrossNetworkConnectivity(whitePods, greenPodIPs, false)
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, false)

			// =====================================================================
			// Step 23: Re-create the CNC
			// =====================================================================
			By("23. Re-creating CNC selecting all 4 networks")
			createOrUpdateCNC(cs, cncName, cudnLabel, udnLabel)

			By("23. Verifying CNC has both tunnel ID and subnet annotations")
			verifyCNCHasBothAnnotations(cncName)

			By("23. Verifying CNC subnet annotation has 4 networks")
			verifyCNCSubnetAnnotationNetworkCount(cncName, 4)

			// =====================================================================
			// Step 24: Verify pods can communicate across all networks again
			// =====================================================================
			By("24. Verifying pods can communicate across all connected networks again")
			verifyFullMeshConnectivity(srcPods, podIPs, true)

			By("Test completed successfully - CNC lifecycle validated")
		})
	})

	/*
	   Multiple CNCs with overlapping network selection:

	   This test validates behavior when multiple CNCs exist in the cluster with
	   overlapping network selections. It creates:
	   - CNC-1: selects blue (L2 UDN) and red (L3 CUDN)
	   - CNC-2: selects blue (L2 UDN) and green (L3 UDN)

	   Network topology:
	   - Blue UDN (L2): shared between both CNCs
	   - Red CUDN (L3): only in CNC-1
	   - Green UDN (L3): only in CNC-2

	   Expected connectivity:
	   - blue <-> red (via CNC-1)
	   - blue <-> green (via CNC-2)
	   - red <-/-> green (no direct connection - non-transitive)

	   This validates that:
	   1. CNCs can be created before networks exist
	   2. Networks get dynamically added to existing CNCs when they match selectors
	   3. A network can be part of multiple CNCs simultaneously
	   4. Connectivity is non-transitive (no indirect routes through shared networks)
	   5. Each CNC maintains independent routing domains

	   Steps:
	   1. Create CNC-1 selecting blue and red (no networks exist yet)
	   2. Create CNC-2 selecting blue and green (no networks exist yet)
	   3. Create blue UDN (L2) with pods - gets added to both CNCs
	   4. Create red CUDN (L3) with pods - gets added to CNC-1
	   5. Create green UDN (L3) with pods - gets added to CNC-2
	   6. Verify blue <-> red connectivity via CNC-1
	   7. Verify blue <-> green connectivity via CNC-2
	   8. Verify red <-/-> green (non-transitive - no connectivity)
	   9. Delete CNC-1, verify blue <-> green still works, blue <-/-> red
	   10. Delete CNC-2, verify all networks isolated
	*/
	Context("Multiple CNCs with overlapping network selection", func() {
		const nodeHostnameKey = "kubernetes.io/hostname"

		// Second CNC connect subnet configuration (must be different from first CNC)
		const (
			cnc2ConnectSubnetIPv4CIDR   = "192.169.0.0/16"
			cnc2ConnectSubnetIPv4Prefix = 24
			cnc2ConnectSubnetIPv6CIDR   = "fd00:11::/112"
			cnc2ConnectSubnetIPv6Prefix = 120
		)

		It("should maintain non-transitive connectivity when a network is selected by multiple CNCs", func() {
			// Test identifiers
			testID := rand.String(5)
			cnc1Name := fmt.Sprintf("color-1-%s", testID)
			cnc2Name := fmt.Sprintf("color-2-%s", testID)

			// Get 2 schedulable nodes for cross-node testing
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodes.Items)).To(BeNumerically(">=", 2), "test requires at least 2 schedulable nodes")
			node1Name, node2Name := nodes.Items[0].Name, nodes.Items[1].Name

			// Network names
			blueUDN := "blue-udn"                         // L2 UDN - shared between both CNCs
			redCUDN := fmt.Sprintf("red-cudn-%s", testID) // L3 CUDN - CNC-1 only
			greenUDN := "green-udn"                       // L3 UDN - CNC-2 only

			// Namespace names
			blueNs := fmt.Sprintf("blue-ns-%s", testID)
			redNs := fmt.Sprintf("red-ns-%s", testID)
			greenNs := fmt.Sprintf("green-ns-%s", testID)

			// Labels for CNC selection
			// Blue needs labels for both CNC-1 (via blueLabel) and CNC-2 (via cnc2Label)
			blueLabel := map[string]string{"network-color": "blue", "test-id": testID, "cnc2-member": "true"}
			redLabel := map[string]string{"network-color": "red", "test-id": testID}
			// Green needs label for CNC-2
			greenLabel := map[string]string{"network-color": "green", "test-id": testID, "cnc2-member": "true"}
			// CNC-2 selector - matches both blue and green namespaces
			cnc2Label := map[string]string{"cnc2-member": "true"}

			// Store pods and their IPs
			pods := make(map[string]*corev1.Pod)
			podIPs := make(map[string][]string)

			// Cleanup
			DeferCleanup(func() {
				By("Cleanup: Deleting all test resources")
				deleteCNC(cnc1Name)
				deleteCNC(cnc2Name)

				for _, pod := range pods {
					_ = cs.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
				}

				deleteUDN(blueNs, blueUDN)
				deleteUDN(greenNs, greenUDN)
				deleteCUDN(redCUDN)

				deleteNamespace(cs, blueNs)
				deleteNamespace(cs, redNs)
				deleteNamespace(cs, greenNs)
			})

			// =====================================================================
			// Step 1: Create CNC-1 selecting blue and red (no networks exist yet)
			// Each CNC must have different connect subnets
			// =====================================================================
			By("1. Creating CNC-1 (color-1) selecting blue UDN and red CUDN with first connect subnet")
			createOrUpdateCNCWithSubnets(cnc1Name, redLabel, blueLabel, generateConnectSubnets(cs))

			By("1. Verifying CNC-1 has only tunnel ID annotation (no networks yet)")
			verifyCNCHasOnlyTunnelIDAnnotation(cnc1Name)

			// =====================================================================
			// Step 2: Create CNC-2 selecting blue and green (no networks exist yet)
			// =====================================================================
			By("2. Creating CNC-2 (color-2) selecting blue UDN and green UDN with second connect subnet")
			createOrUpdateCNCWithSubnets(cnc2Name, nil, cnc2Label, generateConnectSubnetsWithCIDRs(cs, cnc2ConnectSubnetIPv4CIDR, cnc2ConnectSubnetIPv4Prefix, cnc2ConnectSubnetIPv6CIDR, cnc2ConnectSubnetIPv6Prefix))

			By("2. Verifying CNC-2 has only tunnel ID annotation (no networks yet)")
			verifyCNCHasOnlyTunnelIDAnnotation(cnc2Name)

			// =====================================================================
			// Step 3: Create blue UDN (L2) with pods - gets added to both CNCs
			// =====================================================================
			By("3. Creating blue namespace and L2 UDN")
			createUDNNamespaceWithName(cs, blueNs, blueLabel)
			createLayer2PrimaryUDNWithSubnets(cs, blueNs, blueUDN, "10.128.0.0/16", "2014:100:200::0/60")

			By("3. Waiting for blue UDN to be ready")
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, blueNs, blueUDN), 60*time.Second, time.Second).Should(Succeed())

			By("3. Verifying CNC-1 now has 1 network (blue)")
			verifyCNCSubnetAnnotationNetworkCount(cnc1Name, 1)

			By("3. Verifying CNC-2 now has 1 network (blue)")
			verifyCNCSubnetAnnotationNetworkCount(cnc2Name, 1)

			By("3. Creating pods in blue UDN namespace")
			bluePodConfig0 := httpServerPodConfig("blue-pod-0", blueNs)
			bluePodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			bluePodConfig1 := httpServerPodConfig("blue-pod-1", blueNs)
			bluePodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["blue-pod-0"] = runUDNPod(cs, blueNs, bluePodConfig0, nil)
			pods["blue-pod-1"] = runUDNPod(cs, blueNs, bluePodConfig1, nil)
			podIPs["blue-pod-0"] = getPrimaryNetworkPodIPs(blueNs, "blue-pod-0", blueUDN)
			podIPs["blue-pod-1"] = getPrimaryNetworkPodIPs(blueNs, "blue-pod-1", blueUDN)

			// =====================================================================
			// Step 4: Create red CUDN (L3) with pods - gets added to CNC-1
			// =====================================================================
			By("4. Creating red namespace and L3 CUDN")
			createUDNNamespaceWithName(cs, redNs, nil)
			createLayer3PrimaryCUDNWithSubnets(cs, redCUDN, redLabel, "10.129.0.0/16", "2014:100:300::0/60", redNs)

			By("4. Waiting for red CUDN to be ready")
			Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, redCUDN), 60*time.Second, time.Second).Should(Succeed())

			By("4. Verifying CNC-1 now has 2 networks (blue + red)")
			verifyCNCSubnetAnnotationNetworkCount(cnc1Name, 2)

			By("4. Verifying CNC-2 still has 1 network (blue only)")
			verifyCNCSubnetAnnotationNetworkCount(cnc2Name, 1)

			By("4. Creating pods in red CUDN namespace")
			redPodConfig0 := httpServerPodConfig("red-pod-0", redNs)
			redPodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			redPodConfig1 := httpServerPodConfig("red-pod-1", redNs)
			redPodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["red-pod-0"] = runUDNPod(cs, redNs, redPodConfig0, nil)
			pods["red-pod-1"] = runUDNPod(cs, redNs, redPodConfig1, nil)
			podIPs["red-pod-0"] = getPrimaryNetworkPodIPs(redNs, "red-pod-0", redCUDN)
			podIPs["red-pod-1"] = getPrimaryNetworkPodIPs(redNs, "red-pod-1", redCUDN)

			// =====================================================================
			// Step 5: Create green UDN (L3) with pods - gets added to CNC-2
			// =====================================================================
			By("5. Creating green namespace and L3 UDN")
			createUDNNamespaceWithName(cs, greenNs, greenLabel)
			createLayer3PrimaryUDNWithSubnets(cs, greenNs, greenUDN, "10.130.0.0/16", "2014:100:400::0/60")

			By("5. Waiting for green UDN to be ready")
			Eventually(userDefinedNetworkReadyFunc(f.DynamicClient, greenNs, greenUDN), 60*time.Second, time.Second).Should(Succeed())

			By("5. Verifying CNC-1 still has 2 networks (blue + red)")
			verifyCNCSubnetAnnotationNetworkCount(cnc1Name, 2)

			By("5. Verifying CNC-2 now has 2 networks (blue + green)")
			verifyCNCSubnetAnnotationNetworkCount(cnc2Name, 2)

			By("5. Creating pods in green UDN namespace")
			greenPodConfig0 := httpServerPodConfig("green-pod-0", greenNs)
			greenPodConfig0.nodeSelector = map[string]string{nodeHostnameKey: node1Name}
			greenPodConfig1 := httpServerPodConfig("green-pod-1", greenNs)
			greenPodConfig1.nodeSelector = map[string]string{nodeHostnameKey: node2Name}
			pods["green-pod-0"] = runUDNPod(cs, greenNs, greenPodConfig0, nil)
			pods["green-pod-1"] = runUDNPod(cs, greenNs, greenPodConfig1, nil)
			podIPs["green-pod-0"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-0", greenUDN)
			podIPs["green-pod-1"] = getPrimaryNetworkPodIPs(greenNs, "green-pod-1", greenUDN)

			// Define pod groups for connectivity testing
			bluePods := map[string]*corev1.Pod{"blue-pod-0": pods["blue-pod-0"], "blue-pod-1": pods["blue-pod-1"]}
			redPods := map[string]*corev1.Pod{"red-pod-0": pods["red-pod-0"], "red-pod-1": pods["red-pod-1"]}
			greenPods := map[string]*corev1.Pod{"green-pod-0": pods["green-pod-0"], "green-pod-1": pods["green-pod-1"]}

			bluePodIPs := map[string][]string{"blue-pod-0": podIPs["blue-pod-0"], "blue-pod-1": podIPs["blue-pod-1"]}
			redPodIPs := map[string][]string{"red-pod-0": podIPs["red-pod-0"], "red-pod-1": podIPs["red-pod-1"]}
			greenPodIPs := map[string][]string{"green-pod-0": podIPs["green-pod-0"], "green-pod-1": podIPs["green-pod-1"]}

			// =====================================================================
			// Step 6: Verify blue <-> red connectivity via CNC-1
			// =====================================================================
			By("6. Verifying blue <-> red connectivity via CNC-1")
			verifyCrossNetworkConnectivity(bluePods, redPodIPs, true)
			verifyCrossNetworkConnectivity(redPods, bluePodIPs, true)

			// =====================================================================
			// Step 7: Verify blue <-> green connectivity via CNC-2
			// =====================================================================
			By("7. Verifying blue <-> green connectivity via CNC-2")
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, true)
			verifyCrossNetworkConnectivity(greenPods, bluePodIPs, true)

			// =====================================================================
			// Step 8: Verify red <-/-> green (non-transitive)
			// =====================================================================
			By("8. Verifying red <-/-> green (non-transitive - no direct connectivity)")
			verifyCrossNetworkConnectivity(redPods, greenPodIPs, false)
			verifyCrossNetworkConnectivity(greenPods, redPodIPs, false)

			By("8. Verifying blue still connected to both red and green")
			verifyCrossNetworkConnectivity(bluePods, redPodIPs, true)
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, true)

			// =====================================================================
			// Step 9: Delete CNC-1, verify blue <-> green still works
			// =====================================================================
			By("9. Deleting CNC-1")
			deleteCNC(cnc1Name)

			By("9. Waiting for CNC-1 deletion to complete")
			Eventually(func() bool {
				_, err := getCNCAnnotations(cnc1Name)
				return err != nil
			}, 60*time.Second, 2*time.Second).Should(BeTrue())

			By("9. Verifying blue <-> green still connected via CNC-2")
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, true)
			verifyCrossNetworkConnectivity(greenPods, bluePodIPs, true)

			By("9. Verifying blue <-/-> red (CNC-1 deleted)")
			verifyCrossNetworkConnectivity(bluePods, redPodIPs, false)
			verifyCrossNetworkConnectivity(redPods, bluePodIPs, false)

			// =====================================================================
			// Step 10: Delete CNC-2, verify all networks isolated
			// =====================================================================
			By("10. Deleting CNC-2")
			deleteCNC(cnc2Name)

			By("10. Waiting for CNC-2 deletion to complete")
			Eventually(func() bool {
				_, err := getCNCAnnotations(cnc2Name)
				return err != nil
			}, 60*time.Second, 2*time.Second).Should(BeTrue())

			By("10. Verifying all networks are now isolated")
			verifyCrossNetworkConnectivity(bluePods, redPodIPs, false)
			verifyCrossNetworkConnectivity(bluePods, greenPodIPs, false)
			verifyCrossNetworkConnectivity(redPods, greenPodIPs, false)

			By("Test completed successfully - Multiple CNCs with non-transitive connectivity validated")
		})
	})
})
