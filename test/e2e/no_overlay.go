package e2e

import (
	"context"
	"fmt"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
)

var _ = ginkgo.Describe("No-Overlay: Default network is enabled with no-overlay", feature.NoOverlay, func() {
	f := wrappedTestFramework("no-overlay-default-network")

	ginkgo.It("should have pod subnet routes in GR but not in ovn_cluster_router for no-overlay", func() {
		if !isNoOverlayEnabled() {
			ginkgo.Skip("Test requires no-overlay mode to be enabled")
		}

		ginkgo.By("Getting all nodes and their pod subnets")
		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		framework.ExpectNoError(err, "Failed to list nodes")
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">=", 2), "Test requires at least 2 nodes")

		ginkgo.By("Verifying pod subnet routes are in GR but not in ovn_cluster_router for each node")
		for i, node := range nodes.Items {
			// Get pod CIDR for this node from OVN annotation (this is what OVN actually uses)
			podCIDRs, err := getNodePodSubnets(&node)
			framework.ExpectNoError(err, "Failed to get node pod subnets for %s", node.Name)
			gomega.Expect(len(podCIDRs)).To(gomega.BeNumerically(">", 0), "Node %s should have pod CIDRs", node.Name)
			framework.Logf("Checking node %s (pod CIDRs: %v)", node.Name, podCIDRs)
			// For each node, check that OTHER nodes' pod subnets appear in its GR
			for j, otherNode := range nodes.Items {
				if i == j {
					// Skip checking the node's own subnet in its own GR
					continue
				}
				otherPodCIDRs, err := getNodePodSubnets(&otherNode)
				framework.ExpectNoError(err, "Failed to get node pod subnets for %s", otherNode.Name)
				gomega.Expect(len(otherPodCIDRs)).To(gomega.BeNumerically(">", 0), "Node %s should have pod CIDRs", otherNode.Name)

				for _, otherPodCIDR := range otherPodCIDRs {
					framework.Logf("Verifying %s's pod subnet %s appears in %s's GR but not in ovn_cluster_router",
						otherNode.Name, otherPodCIDR, node.Name)
					// - Routes to remote pod subnets SHOULD exist in each node's GR (imported from BGP)
					// - Routes to remote pod subnets should NOT exist in ovn_cluster_router (BGP handles routing)
					err := checkOVNLogicalRouterRoutes(otherPodCIDR, node.Name, true, false)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
			}
		}
	})

})

// checkOVNLogicalRouterRoutes verifies that routes exist or don't exist in OVN logical routers.
func checkOVNLogicalRouterRoutes(podCIDR, nodeName string, shouldExistInGR, shouldExistInClusterRouter bool) error {
	ovnKubeNamespace := deploymentconfig.Get().OVNKubernetesNamespace()

	// Get the ovnkube-node pod for this node
	ovnkubeNodePodName, err := e2ekubectl.RunKubectl(ovnKubeNamespace, "get", "pods", "-l", "app=ovnkube-node", "--field-selector", fmt.Sprintf("spec.nodeName=%s", nodeName), "-o=jsonpath='{.items[0].metadata.name}'")
	if err != nil {
		return fmt.Errorf("failed to get ovnkube-node pod for node %s: %w", nodeName, err)
	}

	ovnkubeNodePodName = strings.Trim(ovnkubeNodePodName, "'")
	if ovnkubeNodePodName == "" {
		return fmt.Errorf("no ovnkube-node pod found for node %s", nodeName)
	}
	framework.Logf("Using ovnkube-node pod: %s on node %s", ovnkubeNodePodName, nodeName)

	// Check GR (Gateway Router)
	grName := "GR_" + nodeName
	grRoutes, err := e2ekubectl.RunKubectl(ovnKubeNamespace, "exec", ovnkubeNodePodName, "--", "ovn-nbctl", "--no-leader-only", "lr-route-list", grName)
	if err != nil {
		return fmt.Errorf("failed to list routes in %s: %w", grName, err)
	}

	grContainsRoute := strings.Contains(grRoutes, podCIDR)
	if shouldExistInGR && !grContainsRoute {
		return fmt.Errorf("route %s should exist in %s but was not found. Routes:\n%s", podCIDR, grName, grRoutes)
	}
	if !shouldExistInGR && grContainsRoute {
		return fmt.Errorf("route %s should NOT exist in %s but was found. Routes:\n%s", podCIDR, grName, grRoutes)
	}
	framework.Logf("✓ Route %s in %s: expected=%v, found=%v", podCIDR, grName, shouldExistInGR, grContainsRoute)

	// Check ovn_cluster_router
	clusterRouterRoutes, err := e2ekubectl.RunKubectl(ovnKubeNamespace, "exec", ovnkubeNodePodName, "--", "ovn-nbctl", "--no-leader-only", "lr-route-list", "ovn_cluster_router")
	if err != nil {
		return fmt.Errorf("failed to list routes in ovn_cluster_router: %w", err)
	}

	clusterRouterContainsRoute := strings.Contains(clusterRouterRoutes, podCIDR)
	if shouldExistInClusterRouter && !clusterRouterContainsRoute {
		return fmt.Errorf("route %s should exist in ovn_cluster_router but was not found. Routes:\n%s", podCIDR, clusterRouterRoutes)
	}
	if !shouldExistInClusterRouter && clusterRouterContainsRoute {
		return fmt.Errorf("route %s should NOT exist in ovn_cluster_router but was found. Routes:\n%s", podCIDR, clusterRouterRoutes)
	}
	framework.Logf("✓ Route %s in ovn_cluster_router: expected=%v, found=%v", podCIDR, shouldExistInClusterRouter, clusterRouterContainsRoute)

	return nil
}
