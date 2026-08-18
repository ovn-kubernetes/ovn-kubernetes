// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
)

const (
	newV4JoinSubnet    = "100.66.0.0/16"
	newV4TransitSubnet = "100.90.0.0/16"
	newV6JoinSubnet    = "fd99::/64"
	newV6TransitSubnet = "fd96::/64"
)

var _ = ginkgo.Describe("e2e stale route cleanup", ginkgo.Serial, func() {
	f := wrappedTestFramework("stale-routes")

	ginkgo.It("should remove stale static routes from ovn_cluster_router after join and transit subnet change", func() {
		ovnKubeNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
		nodeContainerName := getNodeContainerName()
		isIPv6 := IsIPv6Cluster(f.ClientSet)

		var newJoinSubnet, newTransitSubnet, oldJoinPrefix, oldTransitPrefix, newJoinPrefix, newTransitPrefix string
		if isIPv6 {
			newJoinSubnet = newV6JoinSubnet
			newTransitSubnet = newV6TransitSubnet
			oldJoinPrefix = "fd98:"
			oldTransitPrefix = "fd97:"
			newJoinPrefix = "fd99:"
			newTransitPrefix = "fd96:"
		} else {
			newJoinSubnet = newV4JoinSubnet
			newTransitSubnet = newV4TransitSubnet
			oldJoinPrefix = "100.64."
			oldTransitPrefix = "100.88."
			newJoinPrefix = "100.66."
			newTransitPrefix = "100.90."
		}

		joinSubnetEnvVar := "OVN_V4_JOIN_SUBNET"
		transitSubnetEnvVar := "OVN_V4_TRANSIT_SUBNET"
		if isIPv6 {
			joinSubnetEnvVar = "OVN_V6_JOIN_SUBNET"
			transitSubnetEnvVar = "OVN_V6_TRANSIT_SUBNET"
		}

		ginkgo.By("Saving original join and transit subnet env vars")
		origJoinSubnetNode := getTemplateContainerEnv(ovnKubeNamespace, "daemonset/ovnkube-node", nodeContainerName, joinSubnetEnvVar)
		origTransitSubnetNode := getTemplateContainerEnv(ovnKubeNamespace, "daemonset/ovnkube-node", nodeContainerName, transitSubnetEnvVar)
		origJoinSubnetCP := getTemplateContainerEnv(ovnKubeNamespace, "deployment/ovnkube-control-plane", "ovnkube-cluster-manager", joinSubnetEnvVar)
		origTransitSubnetCP := getTemplateContainerEnv(ovnKubeNamespace, "deployment/ovnkube-control-plane", "ovnkube-cluster-manager", transitSubnetEnvVar)

		ginkgo.DeferCleanup(func() {
			ginkgo.By("Restoring original join and transit subnet env vars")
			restoreEnvNode := map[string]string{}
			restoreEnvCP := map[string]string{}
			var unsetNode, unsetCP []string

			if origJoinSubnetNode != "" {
				restoreEnvNode[joinSubnetEnvVar] = origJoinSubnetNode
			} else {
				unsetNode = append(unsetNode, joinSubnetEnvVar)
			}
			if origTransitSubnetNode != "" {
				restoreEnvNode[transitSubnetEnvVar] = origTransitSubnetNode
			} else {
				unsetNode = append(unsetNode, transitSubnetEnvVar)
			}
			if origJoinSubnetCP != "" {
				restoreEnvCP[joinSubnetEnvVar] = origJoinSubnetCP
			} else {
				unsetCP = append(unsetCP, joinSubnetEnvVar)
			}
			if origTransitSubnetCP != "" {
				restoreEnvCP[transitSubnetEnvVar] = origTransitSubnetCP
			} else {
				unsetCP = append(unsetCP, transitSubnetEnvVar)
			}

			setUnsetTemplateContainerEnv(f.ClientSet, ovnKubeNamespace, "deployment/ovnkube-control-plane", "ovnkube-cluster-manager", restoreEnvCP, unsetCP...)
			setUnsetTemplateContainerEnv(f.ClientSet, ovnKubeNamespace, "daemonset/ovnkube-node", nodeContainerName, restoreEnvNode, unsetNode...)
			err := waitOVNKubernetesHealthy(f)
			framework.ExpectNoError(err, "OVN-Kubernetes not healthy after restoring original subnets")

			ginkgo.By("Waiting for routes to converge after restoring original subnets")
			waitForRouteConvergence(f, ovnKubeNamespace, oldJoinPrefix, oldTransitPrefix, newJoinPrefix, newTransitPrefix)
		})

		ginkgo.By("Getting initial routes from ovn_cluster_router on all nodes")
		allRoutes := getAllClusterRouterRoutes(f, ovnKubeNamespace)
		for node, routes := range allRoutes {
			framework.Logf("Initial ovn_cluster_router routes on %s:\n%s", node, routes)
		}

		ginkgo.By("Changing join and transit subnets on ovnkube-control-plane and ovnkube-node")
		cpEnv := map[string]string{
			joinSubnetEnvVar:    newJoinSubnet,
			transitSubnetEnvVar: newTransitSubnet,
		}
		nodeEnv := map[string]string{
			joinSubnetEnvVar: newJoinSubnet,
		}
		setUnsetTemplateContainerEnv(f.ClientSet, ovnKubeNamespace, "deployment/ovnkube-control-plane", "ovnkube-cluster-manager", cpEnv)
		setUnsetTemplateContainerEnv(f.ClientSet, ovnKubeNamespace, "daemonset/ovnkube-node", nodeContainerName, nodeEnv)

		ginkgo.By("Waiting for OVN-Kubernetes to become healthy after subnet change")
		err := waitOVNKubernetesHealthy(f)
		framework.ExpectNoError(err, "OVN-Kubernetes not healthy after subnet change")

		ginkgo.By("Verifying ovn_cluster_router routes use new subnet IPs and stale routes are removed on all nodes")
		waitForRouteConvergence(f, ovnKubeNamespace, newJoinPrefix, newTransitPrefix, oldJoinPrefix, oldTransitPrefix)
	})
})

// waitForRouteConvergence polls all ovnkube-node pods until routes contain the expected
// prefixes and no longer contain the stale prefixes.
func waitForRouteConvergence(f *framework.Framework, ovnKubeNamespace, expectedJoinPrefix, expectedTransitPrefix, staleJoinPrefix, staleTransitPrefix string) {
	err := wait.PollImmediate(5*time.Second, 120*time.Second, func() (bool, error) {
		allRoutes := getAllClusterRouterRoutes(f, ovnKubeNamespace)
		for node, routes := range allRoutes {
			if strings.Contains(routes, staleJoinPrefix) {
				framework.Logf("Node %s: stale join subnet IPs (%s) still present", node, staleJoinPrefix)
				return false, nil
			}
			if strings.Contains(routes, staleTransitPrefix) {
				framework.Logf("Node %s: stale transit subnet IPs (%s) still present", node, staleTransitPrefix)
				return false, nil
			}
			if !strings.Contains(routes, expectedJoinPrefix) {
				framework.Logf("Node %s: expected join subnet IPs (%s) not yet present", node, expectedJoinPrefix)
				return false, nil
			}
			if !strings.Contains(routes, expectedTransitPrefix) {
				framework.Logf("Node %s: expected transit subnet IPs (%s) not yet present", node, expectedTransitPrefix)
				return false, nil
			}
		}
		return true, nil
	})
	framework.ExpectNoError(err, "route convergence timed out")
}

// getAllClusterRouterRoutes returns ovn_cluster_router routes from every ovnkube-node pod,
// keyed by pod name. In single-node-zone interconnect mode each pod has a node-local nb-ovsdb.
func getAllClusterRouterRoutes(f *framework.Framework, ovnKubeNamespace string) map[string]string {
	pods, err := f.ClientSet.CoreV1().Pods(ovnKubeNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
	})
	framework.ExpectNoError(err, "failed to list ovnkube-node pods")
	gomega.Expect(pods.Items).NotTo(gomega.BeEmpty(), "no ovnkube-node pods found")

	routes := make(map[string]string, len(pods.Items))
	for _, pod := range pods.Items {
		output, err := e2ekubectl.RunKubectl(ovnKubeNamespace, "exec", pod.Name, "-c", "nb-ovsdb",
			"--", "ovn-nbctl", "lr-route-list", "ovn_cluster_router")
		if err != nil {
			framework.Failf("failed to run ovn-nbctl lr-route-list on pod %s: %v", pod.Name, err)
		}
		routes[pod.Name] = output
	}
	return routes
}

func getClusterRouterRoutes(f *framework.Framework, ovnKubeNamespace string) string {
	allRoutes := getAllClusterRouterRoutes(f, ovnKubeNamespace)
	var combined []string
	for _, routes := range allRoutes {
		combined = append(combined, routes)
	}
	return fmt.Sprintf("%s", strings.Join(combined, "\n"))
}
