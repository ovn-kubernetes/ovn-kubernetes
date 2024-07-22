package e2e

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"

	v1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

// pulled from https://github.com/kubernetes/kubernetes/blob/v1.26.2/test/e2e/framework/pod/wait.go#L468
// had to modify function due to restart policy on static pods being set to always, which caused function to fail
func waitForPodRunningInNamespaceTimeout(c clientset.Interface, podName, namespace string, timeout time.Duration) error {
	return e2epod.WaitForPodCondition(context.TODO(), c, namespace, podName, fmt.Sprintf("%s", v1.PodRunning), timeout, func(pod *v1.Pod) (bool, error) {
		switch pod.Status.Phase {
		case v1.PodRunning:
			ginkgo.By("Saw pod running")
			return true, nil
		default:
			return false, nil
		}
	})
}

// This test does the following
// Applies a static-pod.yaml file to a nodes /etc/kubernetes/manifest dir
// Expects the static pod to succeed
var _ = ginkgo.Describe("Creating a static pod on a node", func() {
	const podFile string = "static-pod.yaml"

	f := wrappedTestFramework("staticpods")

	ginkgo.It("Should successfully create then remove a static pod", func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 1 {
			framework.Failf("Test requires 1 Ready node, but there are none")
		}
		nodeName := nodes.Items[0].Name
		podName := fmt.Sprintf("static-pod-%s", nodeName)

		ginkgo.By("creating static pod file")

		var staticPodYaml = fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: static-pod
  namespace: %s
spec: 
  containers: 
    - name: web 
      image: %s
      command: ["/bin/bash", "-c", "trap : TERM INT; sleep infinity & wait"]
`, f.Namespace.Name, images.AgnHost())
		if err := os.WriteFile(podFile, []byte(staticPodYaml), 0644); err != nil {
			framework.Failf("Unable to write static pod config to disk: %v", err)
		}
		defer func() {
			if err := os.Remove(podFile); err != nil {
				framework.Logf("Unable to remove static pod config from disk: %v", err)
			}
		}()
		framework.Logf("Creating the static pod")
		e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", podFile)
		err = waitForPodRunningInNamespaceTimeout(f.ClientSet, podName, f.Namespace.Name, time.Second*30)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = e2epod.WaitForPodNotFoundInNamespace(context.TODO(), f.ClientSet, podName, f.Namespace.Name, time.Second*30)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
})
