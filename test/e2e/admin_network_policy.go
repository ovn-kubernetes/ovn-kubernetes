// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"

	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpclientset "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned"
)

// E2E test for ANP status cleanup - reproduces OCPBUGS-78703
// Tests that stale status conditions are removed when nodes are deleted
// during ovnkube-control-plane pod downtime
// Serial: These tests shutdown nodes and scale ovnkube-control-plane to 0,
// which would break other tests running in parallel
var _ = ginkgo.Describe("Admin Network Policy Status Cleanup", ginkgo.Serial, func() {
	const (
		anpName = "test-anp-cleanup"
	)

	f := wrappedTestFramework("anp-status-cleanup")

	var (
		cs        clientset.Interface
		anpClient anpclientset.Interface
	)

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
		var err error
		anpClient, err = anpclientset.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "failed to create ANP client")
	})

	ginkgo.AfterEach(func() {
		// Cleanup: Delete ANP
		ginkgo.By("Cleaning up AdminNetworkPolicy")
		_ = anpClient.PolicyV1alpha1().AdminNetworkPolicies().Delete(
			context.Background(),
			anpName,
			metav1.DeleteOptions{},
		)
	})

	// This test reproduces the EXACT scenario from OCPBUGS-78703:
	// 1. Create ANP - controller creates status entries for all nodes
	// 2. Shutdown a worker node (removes it from cluster)
	// 3. Scale ovnkube-control-plane to 0 (controller down)
	// 4. Delete the node object from API while controller is DOWN
	// 5. Scale ovnkube-control-plane back (new leader election)
	// 6. Verify: WITHOUT fix = stale entry remains, WITH fix = cleanup removes it
	// 7. Restore the node to original cluster state
	ginkgo.It("should remove stale status entries when nodes are deleted during controller downtime", func() {
		var testNodeName string

		ginkgo.By("Step 1: Creating AdminNetworkPolicy")
		anp := newANP(anpName)
		_, err := anpClient.PolicyV1alpha1().AdminNetworkPolicies().Create(
			context.Background(),
			anp,
			metav1.CreateOptions{},
		)
		framework.ExpectNoError(err, "failed to create ANP")

		ginkgo.By("Step 2: Waiting for ANP status to have entries for all nodes")
		initialNodeCount := waitForANPStatusToMatchNodes(anpClient, anpName, cs)
		framework.Logf("ANP has %d status entries matching %d nodes", initialNodeCount, initialNodeCount)

		ginkgo.By("Step 3: Selecting a worker node to shutdown")
		nodes, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
		framework.ExpectNoError(err, "failed to get nodes")
		gomega.Expect(nodes.Items).NotTo(gomega.BeEmpty(), "cluster has no schedulable nodes")

		// Find a worker node (not master/control-plane)
		for i := range nodes.Items {
			node := &nodes.Items[i]
			if !hasRoleLabel(node, "master") && !hasRoleLabel(node, "control-plane") {
				testNodeName = node.Name
				break
			}
		}
		gomega.Expect(testNodeName).NotTo(gomega.BeEmpty(), "no worker node found")
		framework.Logf("Selected worker node for test: %s", testNodeName)

		ginkgo.By("Step 4: Shutting down the worker node")
		err = infraprovider.Get().ShutdownNode(testNodeName)
		framework.ExpectNoError(err, "failed to shutdown node %s", testNodeName)

		// Ensure node is restored even if test fails
		defer func() {
			framework.Logf("Ensuring node %s is restored (cleanup)", testNodeName)
			if startErr := infraprovider.Get().StartNode(testNodeName); startErr != nil {
				framework.Logf("Failed to start node %s during cleanup: %v", testNodeName, startErr)
			} else {
				framework.Logf("Waiting for node %s to become Ready after cleanup", testNodeName)
				waitForNodeReadyState(f, testNodeName, 5*time.Minute, true)
			}
		}()

		// Wait for node to be NotReady
		framework.Logf("Waiting for node %s to be marked NotReady", testNodeName)
		waitForNodeReadyState(f, testNodeName, 2*time.Minute, false)

		ginkgo.By("Step 5: Scaling ovnkube-control-plane to 0 (controller down)")
		originalReplicas := getOVNKubeControlPlaneReplicas(cs)
		framework.Logf("Original replicas: %d", originalReplicas)
		scaleOVNKubeControlPlane(cs, 0)
		time.Sleep(15 * time.Second) // Ensure pods are terminated

		ginkgo.By("Step 6: Deleting node object while controller is DOWN")
		err = cs.CoreV1().Nodes().Delete(context.Background(), testNodeName, metav1.DeleteOptions{})
		framework.ExpectNoError(err, "failed to delete node %s", testNodeName)

		// Verify node is gone from API
		_, err = cs.CoreV1().Nodes().Get(context.Background(), testNodeName, metav1.GetOptions{})
		gomega.Expect(err).To(gomega.HaveOccurred(), "node should be deleted from API")
		framework.Logf("Node %s deleted from API while controller is DOWN", testNodeName)

		ginkgo.By("Step 7: Scaling ovnkube-control-plane back (new leader election)")
		scaleOVNKubeControlPlane(cs, int(originalReplicas))
		time.Sleep(60 * time.Second) // Wait for pods to start and cleanup to run

		ginkgo.By("Step 8: Verifying stale status entry is cleaned up")
		verifyANPStatusMatchesNodes(anpClient, anpName, cs, testNodeName)
		framework.Logf("✅ Stale status entry for %s was cleaned up!", testNodeName)

		ginkgo.By("Step 9: Starting node back to restore cluster state")
		err = infraprovider.Get().StartNode(testNodeName)
		framework.ExpectNoError(err, "failed to start node %s", testNodeName)

		framework.Logf("Waiting for node %s to become Ready", testNodeName)
		waitForNodeReadyState(f, testNodeName, 5*time.Minute, true)

		framework.Logf("✅ SUCCESS: Test completed and cluster state restored!")
	})

	// Simpler test that doesn't require node deletion - just verifies the cleanup logic
	ginkgo.It("should not have stale entries after controller restart", func() {
		ginkgo.By("Creating AdminNetworkPolicy")
		anp := newANP(anpName)
		_, err := anpClient.PolicyV1alpha1().AdminNetworkPolicies().Create(
			context.Background(),
			anp,
			metav1.CreateOptions{},
		)
		framework.ExpectNoError(err, "failed to create ANP")

		ginkgo.By("Waiting for ANP status to stabilize")
		nodeCount := waitForANPStatusToMatchNodes(anpClient, anpName, cs)

		ginkgo.By("Restarting ovnkube-control-plane pods")
		// Scale down then up to trigger restart
		originalReplicas := getOVNKubeControlPlaneReplicas(cs)
		scaleOVNKubeControlPlane(cs, 0)
		time.Sleep(15 * time.Second)
		scaleOVNKubeControlPlane(cs, int(originalReplicas))
		time.Sleep(60 * time.Second)

		ginkgo.By("Verifying ANP status still matches nodes after restart")
		anp, err = anpClient.PolicyV1alpha1().AdminNetworkPolicies().Get(
			context.Background(),
			anpName,
			metav1.GetOptions{},
		)
		framework.ExpectNoError(err, "failed to get ANP")

		readyConditions := countReadyInZoneConditions(anp.Status.Conditions)
		gomega.Expect(readyConditions).To(gomega.Equal(nodeCount),
			"ANP should still have correct number of status entries after restart")

		framework.Logf("✅ ANP status is clean after controller restart")
	})
})

// Helper Functions

// newANP creates a basic AdminNetworkPolicy for testing
func newANP(name string) *anpapi.AdminNetworkPolicy {
	return &anpapi.AdminNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: anpapi.AdminNetworkPolicySpec{
			Priority: 100,
			Subject: anpapi.AdminNetworkPolicySubject{
				Namespaces: &metav1.LabelSelector{},
			},
			Ingress: []anpapi.AdminNetworkPolicyIngressRule{
				{
					Name:   "allow-all",
					Action: anpapi.AdminNetworkPolicyRuleActionAllow,
					From: []anpapi.AdminNetworkPolicyIngressPeer{
						{
							Namespaces: &metav1.LabelSelector{},
						},
					},
				},
			},
		},
	}
}

// waitForANPStatusToMatchNodes waits for ANP status to have entries for all nodes
func waitForANPStatusToMatchNodes(anpClient anpclientset.Interface, anpName string, cs clientset.Interface) int {
	var nodeCount int
	var readyConditions int

	err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
		// Get current node count
		nodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		nodeCount = len(nodes.Items)

		// Get ANP status
		anp, err := anpClient.PolicyV1alpha1().AdminNetworkPolicies().Get(
			context.Background(),
			anpName,
			metav1.GetOptions{},
		)
		if err != nil {
			return false, err
		}

		readyConditions = countReadyInZoneConditions(anp.Status.Conditions)
		return readyConditions == nodeCount, nil
	})

	framework.ExpectNoError(err, "timed out waiting for ANP status to match node count")
	return nodeCount
}

// verifyANPStatusMatchesNodes verifies ANP status matches current nodes and doesn't have stale entry
func verifyANPStatusMatchesNodes(anpClient anpclientset.Interface, anpName string, cs clientset.Interface, deletedNodeName string) {
	// Get current nodes
	nodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	framework.ExpectNoError(err, "failed to list nodes")
	currentNodeCount := len(nodes.Items)

	// Get ANP
	anp, err := anpClient.PolicyV1alpha1().AdminNetworkPolicies().Get(
		context.Background(),
		anpName,
		metav1.GetOptions{},
	)
	framework.ExpectNoError(err, "failed to get ANP")

	// Count Ready-In-Zone conditions
	readyConditions := countReadyInZoneConditions(anp.Status.Conditions)

	// Verify counts match
	gomega.Expect(readyConditions).To(gomega.Equal(currentNodeCount),
		"ANP status should have %d entries matching current node count, but has %d",
		currentNodeCount, readyConditions)

	// Verify deleted node's entry is NOT present
	staleConditionType := fmt.Sprintf("Ready-In-Zone-%s", deletedNodeName)
	for _, cond := range anp.Status.Conditions {
		gomega.Expect(cond.Type).NotTo(gomega.Equal(staleConditionType),
			"Stale condition for deleted node %s should not exist", deletedNodeName)
	}

	framework.Logf("Verified: %d nodes, %d ANP status entries, no stale entry for %s",
		currentNodeCount, readyConditions, deletedNodeName)
}

// countReadyInZoneConditions counts conditions with type starting with "Ready-In-Zone-"
func countReadyInZoneConditions(conditions []metav1.Condition) int {
	count := 0
	for _, cond := range conditions {
		if len(cond.Type) > 14 && cond.Type[:14] == "Ready-In-Zone-" {
			count++
		}
	}
	return count
}

// scaleOVNKubeControlPlane scales the ovnkube-control-plane deployment
func scaleOVNKubeControlPlane(cs clientset.Interface, replicas int) {
	const deploymentName = "ovnkube-control-plane"

	// Try both upstream and OpenShift namespaces
	namespaces := []string{"ovn-kubernetes", "openshift-ovn-kubernetes"}
	var deployment *appsv1.Deployment
	var namespace string
	var err error

	for _, ns := range namespaces {
		deployment, err = cs.AppsV1().Deployments(ns).Get(
			context.Background(),
			deploymentName,
			metav1.GetOptions{},
		)
		if err == nil {
			namespace = ns
			break
		}
	}
	framework.ExpectNoError(err, "failed to get ovnkube-control-plane deployment in any namespace")

	deployment.Spec.Replicas = int32Ptr(int32(replicas))
	_, err = cs.AppsV1().Deployments(namespace).Update(
		context.Background(),
		deployment,
		metav1.UpdateOptions{},
	)
	framework.ExpectNoError(err, "failed to scale ovnkube-control-plane")

	framework.Logf("Scaled ovnkube-control-plane to %d replicas", replicas)
}



// getOVNKubeControlPlaneReplicas returns the current replica count of ovnkube-control-plane deployment
func getOVNKubeControlPlaneReplicas(cs clientset.Interface) int32 {
	const deploymentName = "ovnkube-control-plane"

	// Try both upstream and OpenShift namespaces
	namespaces := []string{"ovn-kubernetes", "openshift-ovn-kubernetes"}
	var deployment *appsv1.Deployment
	var err error

	for _, ns := range namespaces {
		deployment, err = cs.AppsV1().Deployments(ns).Get(
			context.Background(),
			deploymentName,
			metav1.GetOptions{},
		)
		if err == nil {
			break
		}
	}
	framework.ExpectNoError(err, "failed to get ovnkube-control-plane deployment")

	if deployment.Spec.Replicas == nil {
		return 1 // Default to 1 if not specified
	}
	return *deployment.Spec.Replicas
}

// hasRoleLabel checks if node has a specific role label
func hasRoleLabel(node *v1.Node, role string) bool {
	roleLabel := fmt.Sprintf("node-role.kubernetes.io/%s", role)
	_, exists := node.Labels[roleLabel]
	return exists
}

// int32Ptr returns a pointer to int32
func int32Ptr(i int32) *int32 {
	return &i
}
