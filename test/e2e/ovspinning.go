// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	testutils "k8s.io/kubernetes/test/utils"
	"k8s.io/utils/cpuset"
)

const ovsCPUAffinityEnableFile = "/etc/openvswitch/enable_dynamic_cpu_affinity"

type kubeletServiceAction string

const (
	startKubelet kubeletServiceAction = "start"
	stopKubelet  kubeletServiceAction = "stop"
)

var _ = ginkgo.Describe("OVS CPU affinity pinning", feature.OVSCPUPin, ginkgo.Serial, func() {

	f := wrappedTestFramework("ovspinning")

	ginkgo.It("can be enabled on specific nodes by creating enable_dynamic_cpu_affinity file", func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 1))
		nodeWithEnabledOvsAffinityPinning := nodes.Items[0].Name
		nodeWithDisabledOvsAffinityPinning := nodes.Items[1].Name

		err = enableOVSDynamicCPUAffinity(nodeWithEnabledOvsAffinityPinning)
		framework.ExpectNoError(err)

		err = restartOVNKubeNodePodsInParallel(f.ClientSet, deploymentconfig.Get().OVNKubernetesNamespace(), nodeWithEnabledOvsAffinityPinning, nodeWithDisabledOvsAffinityPinning)
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		enabledNodeLogs, err := getOVNKubePodLogsFiltered(f.ClientSet, deploymentconfig.Get().OVNKubernetesNamespace(),
			nodeWithEnabledOvsAffinityPinning, ".*ovspinning_linux.go.*$")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())

		gomega.Expect(enabledNodeLogs).To(gomega.ContainSubstring("Starting OVS daemon CPU pinning"))

		disabledNodeLogs, err := getOVNKubePodLogsFiltered(f.ClientSet, deploymentconfig.Get().OVNKubernetesNamespace(),
			nodeWithDisabledOvsAffinityPinning, ".*ovspinning_linux.go.*$")
		gomega.Expect(err).ToNot(gomega.HaveOccurred())
		gomega.Expect(disabledNodeLogs).To(gomega.ContainSubstring("OVS CPU affinity pinning disabled"))
	})

	ginkgo.It("retains OVS daemon affinity when Pod Resources queries fail", func(ctx ginkgo.SpecContext) {
		nodesCtx, cancelNodes := context.WithTimeout(ctx, framework.SingleCallTimeout)
		defer cancelNodes()
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(nodesCtx, f.ClientSet, 1)
		framework.ExpectNoError(err, "failed to find a ready schedulable node for the Pod Resources failure test")
		gomega.Expect(nodes.Items).ToNot(gomega.BeEmpty(), "expected a ready schedulable node for the Pod Resources failure test")
		testNode := nodes.Items[0].Name

		markerState, err := getOVSDynamicCPUAffinityMarkerState(ctx, testNode)
		framework.ExpectNoError(err, "failed to read the prior OVS CPU affinity marker state on node %s", testNode)
		err = enableOVSDynamicCPUAffinityWithContext(ctx, testNode)
		framework.ExpectNoError(err, "failed to enable OVS CPU affinity on node %s", testNode)
		var daemonAffinities []ovsDaemonAffinity
		deferOVSPinningCleanup(f, testNode, markerState, &daemonAffinities)

		restartCtx, cancelRestart := context.WithTimeout(ctx, framework.PodDeleteTimeout+3*time.Minute)
		err = restartOVSPinningNodePod(restartCtx, f.ClientSet, deploymentconfig.Get().OVNKubernetesNamespace(), testNode)
		cancelRestart()
		framework.ExpectNoError(err, "failed to restart ovnkube-node on node %s", testNode)
		startupLogs := waitForOVSPinningLog(ctx, f.ClientSet, testNode, "OVS CPU dynamic pinning reservedSystemCPUs set:")
		fallbackCPUs, err := parseReservedCPUsFromLogs(startupLogs)
		framework.ExpectNoError(err, "failed to read the effective reserved CPU mask from OVS CPU affinity logs on node %s", testNode)

		err = setKubeletService(ctx, testNode, stopKubelet)
		framework.ExpectNoError(err, "failed to stop kubelet on node %s", testNode)

		daemonAffinities, err = getOVSDaemonAffinities(ctx, testNode, fallbackCPUs)
		framework.ExpectNoError(err, "failed to read OVS daemon affinities on node %s", testNode)
		// Use a valid mask distinct from reservedSystemCPUs so the old fallback would change it.
		for _, daemon := range daemonAffinities {
			_, err = execOVSPinningNodeCommand(ctx, testNode, []string{"taskset", "-apc", daemon.testCPUSet, daemon.pid})
			framework.ExpectNoError(err, "failed to narrow %s affinity on node %s", daemon.name, testNode)
		}

		gomega.Consistently(ctx, func(ctx context.Context) error {
			for _, daemon := range daemonAffinities {
				actualAffinity, err := getProcessCPUAffinity(ctx, testNode, daemon.pid)
				if err != nil {
					return fmt.Errorf("failed to read %s affinity on node %s: %w", daemon.name, testNode, err)
				}
				if actualAffinity != daemon.testCPUSet {
					return fmt.Errorf("%s affinity on node %s changed from %s to %s during the Pod Resources API outage",
						daemon.name, testNode, daemon.testCPUSet, actualAffinity)
				}
			}
			return nil
		}).WithTimeout(4*time.Second).WithPolling(time.Second).Should(gomega.Succeed(),
			"OVS daemon affinities should remain unchanged while kubelet is stopped on node %s", testNode)

		err = setKubeletService(ctx, testNode, startKubelet)
		framework.ExpectNoError(err, "failed to start kubelet on node %s", testNode)
		waitForOVSPinningLog(ctx, f.ClientSet, testNode, "Error while trying to get system non pinned CPUs")
	})
})

func waitForOVSPinningLog(ctx context.Context, clientset kubernetes.Interface, nodeName, message string) string {
	var logs string
	gomega.Eventually(ctx, func(ctx context.Context) error {
		var err error
		logs, err = getOVSPinningPodLogs(ctx, clientset, deploymentconfig.Get().OVNKubernetesNamespace(), nodeName)
		if err != nil {
			return fmt.Errorf("failed to read OVS CPU affinity logs from node %s: %w", nodeName, err)
		}
		if !strings.Contains(logs, message) {
			return fmt.Errorf("OVS CPU affinity logs on node %s do not contain %q yet", nodeName, message)
		}
		return nil
	}).WithTimeout(2*time.Minute).WithPolling(2*time.Second).Should(gomega.Succeed(),
		"waiting for OVS CPU affinity log %q on node %s", message, nodeName)
	return logs
}

func deferOVSPinningCleanup(f *framework.Framework, nodeName string, markerState ovsCPUAffinityMarkerState, daemonAffinities *[]ovsDaemonAffinity) {
	ginkgo.DeferCleanup(func() {
		var cleanupErrors []error
		if err := setKubeletService(context.Background(), nodeName, startKubelet); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to start kubelet on node %s during cleanup: %w", nodeName, err))
		}

		if err := restoreOVSDynamicCPUAffinityMarkerState(context.Background(), nodeName, markerState); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to restore %s on node %s during cleanup: %w", ovsCPUAffinityEnableFile, nodeName, err))
		}

		restartCtx, cancelRestart := context.WithTimeout(context.Background(), framework.PodDeleteTimeout+3*time.Minute)
		if err := restartOVSPinningNodePod(restartCtx, f.ClientSet, deploymentconfig.Get().OVNKubernetesNamespace(), nodeName); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to restart ovnkube-node on node %s during cleanup: %w", nodeName, err))
		}
		cancelRestart()

		for _, daemon := range *daemonAffinities {
			processName, err := execOVSPinningNodeCommand(context.Background(), nodeName, []string{"ps", "-p", daemon.pid, "-o", "comm="})
			if err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to check whether %s process %s survived on node %s: %w", daemon.name, daemon.pid, nodeName, err))
				continue
			}
			if strings.TrimSpace(processName) != daemon.name {
				continue
			}

			_, err = execOVSPinningNodeCommand(context.Background(), nodeName, []string{"taskset", "-apc", daemon.originalCPUSet, daemon.pid})
			if err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to restore %s affinity on node %s: %w", daemon.name, nodeName, err))
			}
		}

		framework.ExpectNoError(errors.Join(cleanupErrors...), "failed to clean up OVS CPU affinity test on node %s", nodeName)
	})
}

type ovsCPUAffinityMarkerState struct {
	exists   bool
	nonEmpty bool
}

func getOVSDynamicCPUAffinityMarkerState(ctx context.Context, nodeName string) (ovsCPUAffinityMarkerState, error) {
	output, err := execOVSPinningNodeCommand(ctx, nodeName, []string{
		"find", "/etc/openvswitch", "-maxdepth", "1", "-name", "enable_dynamic_cpu_affinity", "-printf", "%s",
	})
	if err != nil {
		return ovsCPUAffinityMarkerState{}, fmt.Errorf("failed to inspect OVS CPU affinity marker on node %s: %w", nodeName, err)
	}
	if output == "" {
		return ovsCPUAffinityMarkerState{}, nil
	}
	size, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return ovsCPUAffinityMarkerState{}, fmt.Errorf("invalid size %q reported for OVS CPU affinity marker on node %s: %w", output, nodeName, err)
	}
	return ovsCPUAffinityMarkerState{exists: true, nonEmpty: size > 0}, nil
}

func restoreOVSDynamicCPUAffinityMarkerState(ctx context.Context, nodeName string, state ovsCPUAffinityMarkerState) error {
	if !state.exists {
		_, err := execOVSPinningNodeCommand(ctx, nodeName, []string{"rm", "-f", ovsCPUAffinityEnableFile})
		return err
	}
	if state.nonEmpty {
		return nil
	}
	_, err := execOVSPinningNodeCommand(ctx, nodeName, []string{"truncate", "-s", "0", ovsCPUAffinityEnableFile})
	return err
}

func execOVSPinningNodeCommand(ctx context.Context, nodeName string, cmd []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("context expired before executing %q on node %s: %w", strings.Join(cmd, " "), nodeName, err)
	}
	output, err := infraprovider.Get().ExecK8NodeCommand(nodeName, cmd)
	if err != nil {
		return "", fmt.Errorf("failed to execute %q on node %s: %w", strings.Join(cmd, " "), nodeName, err)
	}
	return output, nil
}

func setKubeletService(ctx context.Context, nodeName string, action kubeletServiceAction) error {
	if action != startKubelet && action != stopKubelet {
		return fmt.Errorf("unsupported kubelet service action %q", action)
	}
	_, err := execOVSPinningNodeCommand(ctx, nodeName, []string{"systemctl", string(action), "kubelet.service"})
	if err != nil {
		return fmt.Errorf("failed to %s kubelet on node %s: %w", action, nodeName, err)
	}
	return nil
}

func enableOVSDynamicCPUAffinity(nodeName string) error {
	_, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"truncate", "-s", "1", ovsCPUAffinityEnableFile})
	if err != nil {
		return fmt.Errorf("failed to enable OVS dynamic CPU affinity on node %s: %w", nodeName, err)
	}
	return nil
}

func enableOVSDynamicCPUAffinityWithContext(ctx context.Context, nodeName string) error {
	_, err := execOVSPinningNodeCommand(ctx, nodeName, []string{"truncate", "-s", "1", ovsCPUAffinityEnableFile})
	if err != nil {
		return fmt.Errorf("failed to enable OVS dynamic CPU affinity on node %s: %w", nodeName, err)
	}
	return nil
}

func restartOVSPinningNodePod(ctx context.Context, clientset kubernetes.Interface, namespace, nodeName string) error {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return fmt.Errorf("failed to list ovnkube-node pods on node %s: %w", nodeName, err)
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("could not find ovnkube-node pod running on node %s", nodeName)
	}
	for i := range pods.Items {
		if err := deletePodWithWait(ctx, clientset, &pods.Items[i]); err != nil {
			return fmt.Errorf("could not delete ovnkube-node pod on node %s: %w", nodeName, err)
		}
	}

	framework.Logf("waiting for node %s to have running ovnkube-node pod", nodeName)
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil {
			return false, err
		}
		for i := range pods.Items {
			ready, err := testutils.PodRunningReady(&pods.Items[i])
			if !ready {
				framework.Logf("ovnkube-node pod %s is not ready yet: %v", pods.Items[i].Name, err)
				return false, nil
			}
		}
		return len(pods.Items) > 0, nil
	}); err != nil {
		return fmt.Errorf("timed out waiting for ovnkube-node pod on node %s: %w", nodeName, err)
	}
	return nil
}

func getOVSPinningPodLogs(ctx context.Context, clientset kubernetes.Interface, namespace, nodeName string) (string, error) {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list ovnkube-node pods on node %s: %w", nodeName, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no ovnkube-node pod found on node %s", nodeName)
	}
	logs, err := e2epod.GetPodLogs(ctx, clientset, namespace, pods.Items[0].Name, getNodeContainerName())
	if err != nil {
		return "", fmt.Errorf("failed to get ovnkube-node [%s/%s] logs: %w", namespace, pods.Items[0].Name, err)
	}
	var filtered strings.Builder
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "ovspinning_linux.go") {
			filtered.WriteString(line)
			filtered.WriteByte('\n')
		}
	}
	return filtered.String(), nil
}

type ovsDaemonAffinity struct {
	name           string
	pid            string
	originalCPUSet string
	testCPUSet     string
}

func getOVSDaemonAffinities(ctx context.Context, nodeName string, fallbackCPUs cpuset.CPUSet) ([]ovsDaemonAffinity, error) {
	var affinities []ovsDaemonAffinity
	for _, name := range []string{"ovs-vswitchd", "ovsdb-server"} {
		pidOutput, err := execOVSPinningNodeCommand(ctx, nodeName, []string{"pgrep", "-xo", name})
		if err != nil {
			return nil, fmt.Errorf("failed to find %s process: %w", name, err)
		}
		pid := strings.TrimSpace(pidOutput)
		if pid == "" {
			return nil, fmt.Errorf("no PID returned for %s process", name)
		}

		cpuList, err := getProcessCPUAffinity(ctx, nodeName, pid)
		if err != nil {
			return nil, fmt.Errorf("failed to get %s CPU affinity: %w", name, err)
		}
		originalCPUs, err := cpuset.Parse(cpuList)
		if err != nil {
			return nil, fmt.Errorf("cannot parse %s CPU affinity %q: %w", name, cpuList, err)
		}
		testCPUs, err := chooseTestCPUMask(originalCPUs, fallbackCPUs)
		if err != nil {
			return nil, fmt.Errorf("cannot choose a test affinity mask for %s: %w", name, err)
		}
		affinities = append(affinities, ovsDaemonAffinity{
			name:           name,
			pid:            pid,
			originalCPUSet: originalCPUs.String(),
			testCPUSet:     testCPUs.String(),
		})
	}
	return affinities, nil
}

func getProcessCPUAffinity(ctx context.Context, nodeName, pid string) (string, error) {
	status, err := execOVSPinningNodeCommand(ctx, nodeName, []string{"cat", "/proc/" + pid + "/status"})
	if err != nil {
		return "", fmt.Errorf("failed to read process %s status: %w", pid, err)
	}
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "Cpus_allowed_list:" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("process %s status has no Cpus_allowed_list", pid)
}

func chooseTestCPUMask(originalCPUs, fallbackCPUs cpuset.CPUSet) (cpuset.CPUSet, error) {
	for _, cpu := range originalCPUs.List() {
		candidate := cpuset.New(cpu)
		if !candidate.Equals(fallbackCPUs) {
			return candidate, nil
		}
	}
	return cpuset.CPUSet{}, fmt.Errorf("no CPU in original mask %s produces a test mask different from fallback mask %s", originalCPUs, fallbackCPUs)
}

func parseReservedCPUsFromLogs(logs string) (cpuset.CPUSet, error) {
	const reservedCPUsLogPrefix = "OVS CPU dynamic pinning reservedSystemCPUs set:"
	for _, line := range strings.Split(logs, "\n") {
		index := strings.Index(line, reservedCPUsLogPrefix)
		if index == -1 {
			continue
		}
		cpuList := strings.TrimSpace(line[index+len(reservedCPUsLogPrefix):])
		if cpuList == "" {
			return cpuset.New(), nil
		}
		cpus, err := cpuset.Parse(cpuList)
		if err != nil {
			return cpuset.CPUSet{}, fmt.Errorf("failed to parse reserved CPU mask %q: %w", cpuList, err)
		}
		return cpus, nil
	}
	return cpuset.CPUSet{}, fmt.Errorf("OVS CPU affinity logs do not contain %q", reservedCPUsLogPrefix)
}
