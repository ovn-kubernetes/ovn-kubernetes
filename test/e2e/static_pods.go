package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"

	v1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

// waitForAllContainersReadyInNamespaceTimeout waits until timeout for all of a pod containers to become Ready. Will return
// error if a container status is terminated. It is expected that no container will restart with an exit code of 0, otherwise error
// is returned immediately.
func waitForAllContainersReadyInNamespaceTimeout(c clientset.Interface, podName, namespace string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return e2epod.WaitForPodCondition(ctx, c, namespace, podName, fmt.Sprintf("%s", v1.PodRunning), timeout, func(pod *v1.Pod) (bool, error) {
		var (
			errs     []error
			allReady = true
		)
		for _, cs := range pod.Status.ContainerStatuses {
			if !cs.Ready {
				allReady = false
			}
			// if container has terminated with non-zero exit, surface the error(s) to aid debug
			if cs.State.Terminated != nil {
				if cs.State.Terminated.ExitCode != 0 {
					errs = append(errs, fmt.Errorf("container terminated non-zero: %s", cs.State.Terminated.String()))
				} else {
					errs = append(errs, fmt.Errorf("container terminated unexpectedly: %s", cs.State.Terminated.String()))
				}
			}
		}

		if len(errs) > 0 {
			return false, errors.Join(errs...)
		}
		if allReady {
			return true, nil
		}
		return false, nil
	})
}

// pulled from https://github.com/kubernetes/kubernetes/blob/v1.26.2/test/e2e/framework/pod/wait.go#L468
// had to modify function due to restart policy on static pods being set to always, which caused function to fail
// Will return error if pod phase is failed or unknown.
func waitForPodRunningInNamespaceTimeout(c clientset.Interface, podName, namespace string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return e2epod.WaitForPodCondition(ctx, c, namespace, podName, fmt.Sprintf("%s", v1.PodRunning), timeout, func(pod *v1.Pod) (bool, error) {
		switch pod.Status.Phase {
		case v1.PodRunning:
			return true, nil
		case v1.PodFailed, v1.PodUnknown:
			return false, fmt.Errorf("pod %s/%s is in state Failed/Unknown: %q",
				namespace, podName, getNonZeroTerminatedStatus(pod))
		default:
			return false, nil
		}
	})
}

// waitForPodSucceededOrFailedInNamespaceTimeout waits until timeout for a pod to become phase Succeeded.
// FIXME: remove failed as a condition when we fix the EIP healthcheck and ensure it doesn't move an EIP when the ovnkube control plane is restarted.
func waitForPodSucceededOrFailedInNamespaceTimeout(c clientset.Interface, podName, namespace string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return e2epod.WaitForPodCondition(ctx, c, namespace, podName, fmt.Sprintf("%s", v1.PodSucceeded), timeout, func(pod *v1.Pod) (bool, error) {
		switch pod.Status.Phase {
		case v1.PodSucceeded, v1.PodFailed:
			return true, nil
		default:
			return false, nil
		}
	})
}

// getNonZeroTerminatedStatus returns all status's of containers that exited with non zero exit code. Should only be used
// for logging and to aid debugging of failed pods.
func getNonZeroTerminatedStatus(pod *v1.Pod) string {
	if pod == nil {
		return "pod object is nil"
	}
	var containerStatuses []string
	for _, cs := range pod.Status.ContainerStatuses {
		// skip containers that are not terminated (may happen for PodFailed/PodUnknown)
		if cs.State.Terminated == nil {
			continue
		}
		if cs.State.Terminated.ExitCode == 0 {
			continue
		}
		containerStatuses = append(containerStatuses, cs.State.Terminated.String())
	}
	if len(containerStatuses) == 0 {
		return "no terminated containers with non-zero exit codes"
	}
	return strings.Join(containerStatuses, "\n")
}

func createStaticPod(nodeName string, podYaml string) {
	// FIXME; remove need to use a container runtime because its not portable
	runCommand := func(cmd ...string) (string, error) {
		output, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to run %q: %s (%s)", strings.Join(cmd, " "), err, output)
		}
		return string(output), nil
	}
	//create file
	var podFile = "static-pod.yaml"
	if err := os.WriteFile(podFile, []byte(podYaml), 0644); err != nil {
		framework.Failf("Unable to write static-pod.yaml  to disk: %v", err)
	}
	defer func() {
		if err := os.Remove(podFile); err != nil {
			framework.Logf("Unable to remove the static-pod.yaml from disk: %v", err)
		}
	}()
	var dst = fmt.Sprintf("%s:/etc/kubernetes/manifests/%s", nodeName, podFile)
	cmd := []string{"docker", "cp", podFile, dst}
	framework.Logf("Running command %v", cmd)
	_, err := runCommand(cmd...)
	if err != nil {
		framework.Failf("failed to copy pod file to node %s", nodeName)
	}
}

func removeStaticPodFile(nodeName string, podFile string) {
	// FIXME; remove need to use a container runtime because its not portable
	runCommand := func(cmd ...string) (string, error) {
		output, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to run %q: %s (%s)", strings.Join(cmd, " "), err, output)
		}
		return string(output), nil
	}

	cmd := []string{"docker", "exec", nodeName, "bash", "-c", fmt.Sprintf("rm /etc/kubernetes/manifests/%s", podFile)}
	framework.Logf("Running command %v", cmd)
	_, err := runCommand(cmd...)
	if err != nil {
		framework.Failf("failed to remove pod file from node %s", nodeName)
	}
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
		createStaticPod(nodeName, staticPodYaml)
		err = waitForPodRunningInNamespaceTimeout(f.ClientSet, podName, f.Namespace.Name, time.Second*60)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.By("Removing the pod file from the nodes /etc/kubernetes/manifests")
		framework.Logf("Removing %s from %s", podName, nodeName)
		removeStaticPodFile(nodeName, podFile)
		err = e2epod.WaitForPodNotFoundInNamespace(context.TODO(), f.ClientSet, podName, f.Namespace.Name, time.Second*60)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
})
