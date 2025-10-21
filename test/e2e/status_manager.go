package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"

	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
)

var _ = ginkgo.Describe("Status manager validation", feature.EgressFirewall, func() {
	const (
		svcname                string = "status-manager"
		egressFirewallYamlFile string = "egress-fw.yml"
	)

	f := wrappedTestFramework(svcname)

	ginkgo.BeforeEach(func() {
		// create EgressFirewall
		var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: 1.2.3.4/24
`, f.Namespace.Name)
		// write the config to a file for application and defer the removal
		if err := os.WriteFile(egressFirewallYamlFile, []byte(egressFirewallConfig), 0644); err != nil {
			framework.Failf("Unable to write CRD config to disk: %v", err)
		}
		// create the CRD config parameters
		applyArgs := []string{
			"apply",
			fmt.Sprintf("--namespace=%s", f.Namespace.Name),
			"-f",
			egressFirewallYamlFile,
		}
		framework.Logf("Applying EgressFirewall configuration: %s ", applyArgs)
		// apply the egress firewall configuration
		e2ekubectl.RunKubectlOrDie(f.Namespace.Name, applyArgs...)
		// check status
		checkEgressFirewallStatus(f.Namespace.Name, false, true, true)
	})

	ginkgo.AfterEach(func() {
		if err := os.Remove(egressFirewallYamlFile); err != nil {
			framework.Logf("Unable to remove the CRD config from disk: %v", err)
		}
	})

	ginkgo.It("Should validate the egress firewall managedFields are updated when a real worker node is deleted and restored", ginkgo.Serial, func() {
		ginkgo.By("Checking we have at least two nodes with at least one worker")
		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		if len(nodes.Items) < 2 {
			ginkgo.Skip("Test requires at least 2 nodes")
		}

		zones := make(map[string]bool)
		for _, node := range nodes.Items {
			zoneName, ok := node.Annotations["k8s.ovn.org/zone-name"]
			if !ok {
				zoneName = node.Name
			}
			zones[zoneName] = true
		}
		if len(zones) < len(nodes.Items) {
			ginkgo.Skip("Skipping test: we need one zone per node for this test to run.")
		}

		var workerNode *v1.Node
		for i := range nodes.Items {
			node := &nodes.Items[i]
			if _, hasControlPlaneRole := node.Labels[framework.ControlPlaneLabel]; !hasControlPlaneRole {
				workerNode = node
				break
			}
		}
		if workerNode == nil {
			ginkgo.Skip("Test requires at least one worker node")
		}

		workerNodeName := workerNode.Name
		zoneName := workerNode.Annotations["k8s.ovn.org/zone-name"]
		if zoneName == "" {
			zoneName = workerNodeName
		}

		ginkgo.By(fmt.Sprintf("Verifying egress firewall has message for the worker node %s zone %s", workerNodeName, zoneName))
		gomega.Eventually(func() bool {
			return hasEgressFirewallStatusMessage(f.Namespace.Name, zoneName)
		}, 30*time.Second, 1*time.Second).Should(gomega.BeTrue())

		ginkgo.By(fmt.Sprintf("Verifying egress firewall has managedFields entry for the worker node %s zone %s", workerNodeName, zoneName))
		gomega.Expect(hasManagedFieldEntryForManager(f.Namespace.Name, zoneName)).To(gomega.BeTrue())

		// Save the complete node definition in order to restore it later
		savedNode := workerNode.DeepCopy()
		// Clear fields that will be set by the API server
		savedNode.ResourceVersion = ""
		savedNode.UID = ""
		savedNode.CreationTimestamp = metav1.Time{}
		savedNode.ManagedFields = nil
		savedNode.Status = v1.NodeStatus{} // avoid sending stale status

		restored := false
		restoreWorkerNode := func() {
			ginkgo.By(fmt.Sprintf("ensuring worker node %s is restored", workerNodeName))
			_, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), workerNodeName, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					// Node not found, re-create it
					_, err = f.ClientSet.CoreV1().Nodes().Create(context.TODO(), savedNode, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					restored = true
				} else {
					// Another error occurred, fail the test
					framework.Failf("Failed to get node %s: %v", workerNodeName, err)
				}
			}

			// Wait for the node to become ready
			gomega.Eventually(func() bool {
				node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), workerNodeName, metav1.GetOptions{})
				if err != nil {
					return false
				}
				for _, condition := range node.Status.Conditions {
					if condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue {
						return true
					}
				}
				return false
			}, 300*time.Second, 5*time.Second).Should(gomega.BeTrue())

			// Wait for the ovnkube-node pod to be running
			ovnNs := deploymentconfig.Get().OVNKubernetesNamespace()
			gomega.Eventually(func() bool {
				pods, err := f.ClientSet.CoreV1().Pods(ovnNs).List(context.TODO(), metav1.ListOptions{
					LabelSelector: "app=ovnkube-node",
					FieldSelector: fmt.Sprintf("spec.nodeName=%s", workerNodeName),
				})
				if err != nil {
					return false
				}
				if len(pods.Items) == 0 {
					return false
				}
				for _, pod := range pods.Items {
					if pod.Status.Phase != v1.PodRunning {
						return false
					}
					for _, containerStatus := range pod.Status.ContainerStatuses {
						if !containerStatus.Ready {
							return false
						}
					}
				}
				return true
			}, 300*time.Second, 5*time.Second).Should(gomega.BeTrue())
		}

		ginkgo.By(fmt.Sprintf("Deleting worker node %s", workerNodeName))
		err = f.ClientSet.CoreV1().Nodes().Delete(context.TODO(), workerNodeName, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Defer the node's restoration to ensure it's re-created if the test fails early
		defer func() {
			if !restored {
				restoreWorkerNode()
			}
		}()

		ginkgo.By("Waiting for node to be deleted")
		gomega.Eventually(func() bool {
			_, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), workerNodeName, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, 60*time.Second, 1*time.Second).Should(gomega.BeTrue())

		ginkgo.By("Verifying egress firewall status message for deleted zone is removed")
		gomega.Eventually(func() bool {
			return hasEgressFirewallStatusMessage(f.Namespace.Name, zoneName)
		}, 60*time.Second, 1*time.Second).Should(gomega.BeFalse())

		ginkgo.By("Verifying egress firewall managedFields entry for deleted zone is cleaned up")
		gomega.Eventually(func() bool {
			return hasManagedFieldEntryForManager(f.Namespace.Name, zoneName)
		}, 60*time.Second, 1*time.Second).Should(gomega.BeFalse())

		restoreWorkerNode()

		ginkgo.By("Verifying egress firewall status message for restored zone is back")
		gomega.Eventually(func() bool {
			return hasEgressFirewallStatusMessage(f.Namespace.Name, zoneName)
		}, 60*time.Second, 1*time.Second).Should(gomega.BeTrue())

		ginkgo.By("Verifying egress firewall managedFields entry for restored zone is back")
		gomega.Eventually(func() bool {
			return hasManagedFieldEntryForManager(f.Namespace.Name, zoneName)
		}, 60*time.Second, 1*time.Second).Should(gomega.BeTrue())
	})

	ginkgo.It("Should validate the egress firewall status when adding an unknown zone", func() {
		// add unknown node
		newNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "new-node",
			},
		}
		_, err := f.ClientSet.CoreV1().Nodes().Create(context.TODO(), newNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			err := f.ClientSet.CoreV1().Nodes().Delete(context.TODO(), newNode.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()
		// check status not changed
		checkEgressFirewallStatus(f.Namespace.Name, false, true, false)
	})

	ginkgo.It("Should validate the egress firewall status when adding a new zone", func() {
		// add node with new zone
		newNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "new-node",
				Annotations: map[string]string{"k8s.ovn.org/zone-name": "new-zone"},
			},
		}
		_, err := f.ClientSet.CoreV1().Nodes().Create(context.TODO(), newNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer func() {
			if err != nil {
				err := f.ClientSet.CoreV1().Nodes().Delete(context.TODO(), newNode.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
		}()
		// check status is unset
		checkEgressFirewallStatus(f.Namespace.Name, true, false, true)
		// delete node
		err = f.ClientSet.CoreV1().Nodes().Delete(context.TODO(), newNode.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// check status is back
		checkEgressFirewallStatus(f.Namespace.Name, false, true, true)
	})
})

func checkEgressFirewallStatus(namespace string, empty bool, success bool, eventually bool) {
	checkStatus := func() bool {
		output, err := e2ekubectl.RunKubectl(namespace, "get", "egressfirewall", "default", "-o", "jsonpath={.status.status}")
		if err != nil {
			framework.Failf("could not get the egressfirewall default in namespace: %s", namespace)
		}
		return empty && output == "" || success && strings.Contains(output, "EgressFirewall Rules applied")
	}
	if eventually {
		gomega.Eventually(checkStatus, 5*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
	} else {
		gomega.Consistently(checkStatus, 5*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
	}
}

func hasManagedFieldEntryForManager(namespace, manager string) bool {
	// a manager might not have any fields, which is ok, but will return an error when using jsonpath.
	// in that case, we should parse the whole object and check if the manager is there.
	output, err := e2ekubectl.RunKubectl(namespace, "get", "egressfirewall", "default", "-o", "jsonpath={.metadata.managedFields}", "--show-managed-fields")
	if err != nil {
		// command failed, try to get the whole object and see if there are any managers
		output, err = e2ekubectl.RunKubectl(namespace, "get", "egressfirewall", "default", "-o", "json", "--show-managed-fields")
		if err != nil {
			framework.Logf("could not get the egressfirewall default in namespace: %s", namespace)
			return false
		}
	}
	var managedFields []metav1.ManagedFieldsEntry
	// if the output is empty, it means there are no managed fields, so the manager is not there.
	output = strings.TrimSpace(output)
	if output == "" {
		return false
	}
	err = json.Unmarshal([]byte(output), &managedFields)
	if err != nil {
		// maybe we got the whole object, try to unmarshal it.
		var ef egressfirewallapi.EgressFirewall
		if err := json.Unmarshal([]byte(output), &ef); err != nil {
			framework.Logf("could not unmarshal egressfirewall: %v", err)
			return false
		}
		managedFields = ef.ManagedFields
	}

	for _, field := range managedFields {
		if field.Manager == manager {
			return true
		}
	}
	return false
}

func hasEgressFirewallStatusMessage(namespace, zoneName string) bool {
	output, err := e2ekubectl.RunKubectl(namespace, "get", "egressfirewall", "default", "-o", "json")
	if err != nil {
		framework.Logf("could not get the egressfirewall default in namespace: %s", namespace)
		return false
	}

	var ef egressfirewallapi.EgressFirewall
	if err := json.Unmarshal([]byte(output), &ef); err != nil {
		framework.Logf("could not unmarshal egressfirewall: %v", err)
		return false
	}

	// Check if any status message contains the zone name
	for _, message := range ef.Status.Messages {
		if strings.Contains(message, zoneName+":") {
			return true
		}
	}
	return false
}
