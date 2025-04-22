package helpers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/gomega"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	testutils "k8s.io/kubernetes/test/utils"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/external"
)

const (
	podNetworkAnnotation = "k8s.ovn.org/pod-networks"
	RetryInterval        = 1 * time.Second  // polling interval timer
	RetryTimeout         = 40 * time.Second // polling timeout
	rolloutTimeout       = 10 * time.Minute
	RedirectIP           = "123.123.123.123"
	RedirectPort         = "13337"
	ExContainerName      = "tcp-continuous-client"
	UdnPodInterface      = "ovn-udn1"
)

type PodCondition = func(pod *v1.Pod) (bool, error)

// SetupHostRedirectPod sets up the host with a redirect pod
func SetupHostRedirectPod(f *framework.Framework, node *v1.Node, exContainerName string, isIPv6 bool) error {
	_, _ = CreateClusterExternalContainer(exContainerName, ExternalContainerImage, []string{"-itd", "--privileged", "--network", external.ContainerNetwork}, []string{})
	nodeV4, nodeV6 := GetContainerAddressesForNetwork(node.Name, external.ContainerNetwork)
	mask := 32
	ipCmd := []string{"ip"}
	nodeIP := nodeV4
	if isIPv6 {
		mask = 128
		ipCmd = []string{"ip", "-6"}
		nodeIP = nodeV6
	}
	cmd := []string{"docker", "exec", exContainerName}
	cmd = append(cmd, ipCmd...)
	cmd = append(cmd, "route", "add", fmt.Sprintf("%s/%d", RedirectIP, mask), "via", nodeIP)
	_, err := RunCommand(cmd...)
	if err != nil {
		return err
	}

	// setup redirect iptables rule in node
	ipTablesArgs := []string{"PREROUTING", "-t", "nat", "--dst", RedirectIP, "-j", "REDIRECT"}
	updateIPTablesRulesForNode("insert", node.Name, ipTablesArgs, isIPv6)

	command := []string{
		"bash", "-c",
		fmt.Sprintf("set -xe; while true; do nc -l -p %s; done",
			RedirectPort),
	}
	tcpServer := "tcp-continuous-server"
	// setup host networked pod to act as server
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: tcpServer,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    tcpServer,
					Image:   AgnhostImage,
					Command: command,
				},
			},
			NodeName:      node.Name,
			RestartPolicy: v1.RestartPolicyNever,
			HostNetwork:   true,
		},
	}
	podClient := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
	_, err = podClient.Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	err = e2epod.WaitForPodNotPending(context.TODO(), f.ClientSet, f.Namespace.Name, tcpServer)
	return err
}

// CheckContinuousConnectivity creates a pod and checks that it can connect to the given host over tries*2 seconds.
// The created pod object is sent to the podChan while any errors along the way are sent to the errChan.
// Callers are expected to read the errChan and verify that they received a nil before fetching
// the pod from the podChan to be sure that the pod was created successfully.
// TODO: this approach with the channels is a bit ugly, it might be worth to refactor this and the other
// functions that use it similarly in this file.
func CheckContinuousConnectivity(f *framework.Framework, nodeName, podName, host string, port, tries, timeout int, podChan chan *v1.Pod, errChan chan error) {
	contName := fmt.Sprintf("%s-Container", podName)

	command := []string{
		"bash", "-c",
		fmt.Sprintf("set -xe; for i in {1..%d}; do nc -vz -w %d %s %d ; sleep 2; done",
			tries, timeout, host, port),
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    contName,
					Image:   AgnhostImage,
					Command: command,
				},
			},
			NodeName:      nodeName,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	podClient := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
	_, err := podClient.Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		errChan <- err
		return
	}

	// Wait for pod network setup to be almost ready
	err = wait.PollImmediate(1*time.Second, 30*time.Second, func() (bool, error) {
		pod, err := podClient.Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		_, ok := pod.Annotations[podNetworkAnnotation]
		return ok, nil
	})
	if err != nil {
		errChan <- err
		return
	}

	err = e2epod.WaitForPodNotPending(context.TODO(), f.ClientSet, f.Namespace.Name, podName)
	if err != nil {
		errChan <- err
		return
	}

	podGet, err := podClient.Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		errChan <- err
		return
	}

	errChan <- nil
	podChan <- podGet

	err = e2epod.WaitForPodSuccessInNamespace(context.TODO(), f.ClientSet, podName, f.Namespace.Name)

	if err != nil {
		logs, logErr := e2epod.GetPodLogs(context.TODO(), f.ClientSet, f.Namespace.Name, pod.Name, contName)
		if logErr != nil {
			framework.Logf("Warning: Failed to get logs from pod %q: %v", pod.Name, logErr)
		} else {
			framework.Logf("pod %s/%s logs:\n%s", f.Namespace.Name, pod.Name, logs)
		}
	}

	errChan <- err
}

// CheckConnectivityPingToHost places the workload on the specified node to test external connectivity
func CheckConnectivityPingToHost(f *framework.Framework, nodeName, podName, host string, pingCmd pingCommand, timeout int) error {
	contName := fmt.Sprintf("%s-Container", podName)
	// Ping options are:
	// -c sends 3 pings
	// -W wait at most 2 seconds for a reply
	// -w timeout
	command := []string{"/bin/sh", "-c"}
	args := []string{fmt.Sprintf("sleep 20; %s -c 3 -W 2 -w %s %s", string(pingCmd), strconv.Itoa(timeout), host)}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    contName,
					Image:   AgnhostImage,
					Command: command,
					Args:    args,
				},
			},
			NodeName:      nodeName,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	podClient := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
	_, err := podClient.Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// Wait for pod network setup to be almost ready
	err = wait.PollImmediate(RetryInterval, RetryTimeout, func() (bool, error) {
		pod, err := podClient.Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		_, ok := pod.Annotations[podNetworkAnnotation]
		return ok, nil
	})
	// Fail the test if no pod annotation is retrieved
	if err != nil {
		framework.Failf("Error trying to get the pod annotation")
	}

	err = e2epod.WaitForPodSuccessInNamespace(context.TODO(), f.ClientSet, podName, f.Namespace.Name)

	if err != nil {
		logs, logErr := e2epod.GetPodLogs(context.TODO(), f.ClientSet, f.Namespace.Name, pod.Name, contName)
		if logErr != nil {
			framework.Logf("Warning: Failed to get logs from pod %q: %v", pod.Name, logErr)
		} else {
			framework.Logf("pod %s/%s logs:\n%s", f.Namespace.Name, pod.Name, logs)
		}
	}

	return err
}

// Place the workload on the specified node and return pod gw route
func getPodGWRoute(f *framework.Framework, nodeName string, podName string) net.IP {
	command := []string{"bash", "-c", "sleep 20000"}
	contName := fmt.Sprintf("%s-Container", podName)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    contName,
					Image:   AgnhostImage,
					Command: command,
				},
			},
			NodeName:      nodeName,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	podClient := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
	_, err := podClient.Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		framework.Failf("Error trying to create pod")
	}

	// Wait for pod network setup to be almost ready
	wait.PollImmediate(1*time.Second, 30*time.Second, func() (bool, error) {
		podGet, err := podClient.Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if podGet.Annotations != nil && podGet.Annotations[podNetworkAnnotation] != "" {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		framework.Failf("Error trying to get the pod annotations")
	}

	podGet, err := podClient.Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		framework.Failf("Error trying to get the pod object")
	}
	annotation, err := UnmarshalPodAnnotation(podGet.Annotations, "default")
	if err != nil {
		framework.Failf("Error trying to unmarshal pod annotations")
	}

	return annotation.Gateways[0]
}

// CreateGenericPod creates a pod on the specified node using the agnostic host image
func CreateGenericPod(f *framework.Framework, podName, nodeSelector, namespace string, command []string) (*v1.Pod, error) {
	return CreatePod(f, podName, nodeSelector, namespace, command, nil)
}

// CreateGenericPodWithLabel creates a pod on the specified node using the agnostic host image
func CreateGenericPodWithLabel(f *framework.Framework, podName, nodeSelector, namespace string, command []string, labels map[string]string, options ...func(*v1.Pod)) (*v1.Pod, error) {
	return CreatePod(f, podName, nodeSelector, namespace, command, labels, options...)
}

func CreateServiceForPodsWithLabel(f *framework.Framework, namespace string, servicePort int32, targetPort string, serviceType string, labels map[string]string) (string, error) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "service-for-pods",
			Namespace: namespace,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Protocol:   v1.ProtocolTCP,
					TargetPort: intstr.Parse(targetPort),
					Port:       servicePort,
				},
			},
			Type:     v1.ServiceType(serviceType),
			Selector: labels,
		},
	}
	serviceClient := f.ClientSet.CoreV1().Services(namespace)
	res, err := serviceClient.Create(context.Background(), service, metav1.CreateOptions{})
	if err != nil {
		return "", errors.Wrapf(err, "Failed to create service %s %s", service.Name, namespace)
	}
	err = wait.PollImmediate(RetryInterval, RetryTimeout, func() (bool, error) {
		res, err = serviceClient.Get(context.Background(), service.Name, metav1.GetOptions{})
		return res.Spec.ClusterIP != "", err
	})
	if err != nil {
		return "", errors.Wrapf(err, "Failed to get service %s %s", service.Name, namespace)
	}
	return res.Spec.ClusterIP, nil
}

// HACK: 'Container runtime' is statically set to docker. For EIP multi network scenario, we require ip6tables support to
// allow isolated ipv6 networks and prevent the bridges from forwarding to each other.
// Docker ipv6+ip6tables support is currently experimental (11/23) [1], and enabling this requires altering the
// Container runtime config. To avoid altering the runtime config, add ip6table rules to prevent the bridges talking
// to each other. Not required to remove the iptables, because when we delete the network, the iptable rules will be removed.
// Remove when this func when it is no longer experimental.
// [1] https://docs.docker.com/config/daemon/ipv6/
func IsolateIPv6Networks(networkA, networkB string) error {
	if ContainerRuntime != "docker" {
		panic("unsupported Container runtime")
	}
	var bridgeInfNames []string
	// docker creates bridges by appending 12 chars from network ID to 'br-'
	bridgeIDLimit := 12
	for _, network := range []string{networkA, networkB} {
		// output will be wrapped in single quotes
		id, err := RunCommand(ContainerRuntime, "inspect", network, "--format", "'{{.Id}}'")
		if err != nil {
			return err
		}
		if len(id) <= bridgeIDLimit+1 {
			return fmt.Errorf("invalid bridge ID %q", id)
		}
		bridgeInfName := fmt.Sprintf("br-%s", id[1:bridgeIDLimit+1])
		// validate bridge exists
		_, err = RunCommand("ip", "link", "show", bridgeInfName)
		if err != nil {
			return fmt.Errorf("bridge %q doesnt exist: %v", bridgeInfName, err)
		}
		bridgeInfNames = append(bridgeInfNames, bridgeInfName)
	}
	if len(bridgeInfNames) != 2 {
		return fmt.Errorf("expected two bridge names but found %d", len(bridgeInfNames))
	}
	_, err := RunCommand("sudo", "ip6tables", "-t", "filter", "-A", "FORWARD", "-i", bridgeInfNames[0], "-o", bridgeInfNames[1], "-j", "DROP")
	if err != nil {
		return err
	}
	_, err = RunCommand("sudo", "ip6tables", "-t", "filter", "-A", "FORWARD", "-i", bridgeInfNames[1], "-o", bridgeInfNames[0], "-j", "DROP")
	return err
}

func CreateNetwork(networkName string, subnet string, v6 bool) {
	args := []string{ContainerRuntime, "network", "create", "--internal", "--driver", "bridge", networkName, "--subnet", subnet}
	if v6 {
		args = append(args, "--ipv6")
	}
	_, err := RunCommand(args...)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		framework.Failf("failed to create secondary network %q with subnet(s) %v: %v", networkName, subnet, err)
	}
}

func DeleteNetwork(networkName string) {
	args := []string{ContainerRuntime, "network", "rm", networkName}
	_, err := RunCommand(args...)
	if err != nil && !strings.Contains(err.Error(), "not found") {
		framework.Failf("failed to delete network %q: %v", networkName, err)
	}
}

func AttachNetwork(networkName, containerName string) {
	args := []string{ContainerRuntime, "network", "connect", networkName, containerName}
	_, err := RunCommand(args...)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		framework.Failf("failed to attach network %q to Container %q: %v", networkName, containerName, err)
	}
}

func DetachNetwork(networkName, containerName string) {
	args := []string{ContainerRuntime, "network", "disconnect", networkName, containerName}
	_, err := RunCommand(args...)
	if err != nil {
		framework.Failf("failed to attach network %q to Container %q: %v", networkName, containerName, err)
	}
}

func CreateClusterExternalContainer(containerName string, containerImage string, dockerArgs []string, entrypointArgs []string) (string, string) {
	args := []string{ContainerRuntime, "run", "-itd"}
	args = append(args, dockerArgs...)
	args = append(args, []string{"--name", containerName, containerImage}...)
	args = append(args, entrypointArgs...)
	_, err := RunCommand(args...)
	if err != nil {
		framework.Failf("failed to start external test Container: %v", err)
	}
	ipv4, err := RunCommand(ContainerRuntime, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", containerName)
	if err != nil {
		framework.Failf("failed to inspect external test Container for its IP: %v", err)
	}
	ipv6, err := RunCommand(ContainerRuntime, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.GlobalIPv6Address}}{{end}}", containerName)
	if err != nil {
		framework.Failf("failed to inspect external test Container for its IP (v6): %v", err)
	}
	if ipv4 == "" && ipv6 == "" {
		framework.Failf("failed to get IPv4 or IPv6 address for Container %s", containerName)
	}
	return strings.Trim(ipv4, "\n"), strings.Trim(ipv6, "\n")
}

func DeleteClusterExternalContainer(containerName string) {
	_, err := RunCommand(ContainerRuntime, "rm", "-f", containerName)
	if err != nil {
		framework.Failf("failed to delete external test Container, err: %v", err)
	}
	gomega.Eventually(func() string {
		output, err := RunCommand(ContainerRuntime, "ps", "-f", fmt.Sprintf("name=%s", containerName), "-q")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return output
	}, 5).Should(gomega.HaveLen(0))
}

// updatesNamespace labels while preserving the required UDN label
func UpdateNamespaceLabels(f *framework.Framework, namespace *v1.Namespace, labels map[string]string) {
	// should never be nil
	n := *namespace
	for k, v := range labels {
		n.Labels[k] = v
	}
	if _, ok := namespace.Labels[RequiredUDNNamespaceLabel]; ok {
		n.Labels[RequiredUDNNamespaceLabel] = ""
	}
	_, err := f.ClientSet.CoreV1().Namespaces().Update(context.Background(), &n, metav1.UpdateOptions{})
	framework.ExpectNoError(err, fmt.Sprintf("unable to update namespace: %s, err: %v", namespace.Name, err))
}
func GetNamespace(f *framework.Framework, name string) *v1.Namespace {
	ns, err := f.ClientSet.CoreV1().Namespaces().Get(context.Background(), name, metav1.GetOptions{})
	framework.ExpectNoError(err, fmt.Sprintf("unable to get namespace: %s, err: %v", name, err))
	return ns
}

func UpdatePod(f *framework.Framework, pod *v1.Pod) {
	_, err := f.ClientSet.CoreV1().Pods(pod.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
	framework.ExpectNoError(err, fmt.Sprintf("unable to update pod: %s, err: %v", pod.Name, err))
}
func GetPod(f *framework.Framework, podName string) *v1.Pod {
	pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), podName, metav1.GetOptions{})
	framework.ExpectNoError(err, fmt.Sprintf("unable to get pod: %s, err: %v", podName, err))
	return pod
}

// Create a pod on the specified node using the agnostic host image
func CreatePod(f *framework.Framework, podName, nodeSelector, namespace string, command []string, labels map[string]string, options ...func(*v1.Pod)) (*v1.Pod, error) {

	contName := fmt.Sprintf("%s-Container", podName)

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   podName,
			Labels: labels,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    contName,
					Image:   AgnhostImage,
					Command: command,
				},
			},
			NodeName:      nodeSelector,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	for _, o := range options {
		o(pod)
	}

	podClient := f.ClientSet.CoreV1().Pods(namespace)
	res, err := podClient.Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		framework.Logf("Warning: Failed to create pod %s %v", pod.Name, err)
		return nil, errors.Wrapf(err, "Failed to create pod %s %s", pod.Name, namespace)
	}

	err = e2epod.WaitForPodRunningInNamespace(context.TODO(), f.ClientSet, res)

	if err != nil {
		res, err = podClient.Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to get pod %s %s", pod.Name, namespace)
		}
		framework.Logf("Warning: Failed to get pod running %v: %v", *res, err)
		logs, logErr := e2epod.GetPodLogs(context.TODO(), f.ClientSet, namespace, pod.Name, contName)
		if logErr != nil {
			framework.Logf("Warning: Failed to get logs from pod %q: %v", pod.Name, logErr)
		} else {
			framework.Logf("pod %s/%s logs:\n%s", namespace, pod.Name, logs)
		}
	}
	// Need to get it again to ensure the ip addresses are filled
	res, err = podClient.Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to get pod %s %s", pod.Name, namespace)
	}
	return res, nil
}

// GetPodAddress the IP address of a pod in the specified namespace
func GetPodAddress(podName, namespace string) string {
	podIP, err := e2ekubectl.RunKubectl(namespace, "get", "pods", podName, "--template={{.status.podIP}}")
	if err != nil {
		framework.Failf("Unable to retrieve the IP for pod %s %v", podName, err)
	}
	return podIP
}

// GetApiAddress returns the IP address of the API server
func GetApiAddress() string {
	apiServerIP, err := e2ekubectl.RunKubectl("default", "get", "svc", "kubernetes", "-o", "jsonpath='{.spec.clusterIP}'")
	apiServerIP = strings.Trim(apiServerIP, "'")
	if err != nil {
		framework.Failf("Error: unable to get API-server IP address, err:  %v", err)
	}
	apiServer := net.ParseIP(apiServerIP)
	if apiServer == nil {
		framework.Failf("Error: unable to parse API-server IP address:  %s", apiServerIP)
	}
	return apiServer.String()
}

// IsGatewayModeLocal returns true if the gateway mode is local
func IsGatewayModeLocal() bool {
	anno, err := e2ekubectl.RunKubectl("default", "get", "node", "ovn-control-plane", "-o", "template", "--template={{.metadata.annotations}}")
	if err != nil {
		framework.Logf("Error getting annotations: %v", err)
		return false
	}
	framework.Logf("Annotations received: %s", anno)
	isLocal := strings.Contains(anno, "local")
	framework.Logf("IsGatewayModeLocal returning: %v", isLocal)
	return isLocal
}

// RunCommand runs the cmd and returns the combined stdout and stderr
func RunCommand(cmd ...string) (string, error) {
	output, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run %q: %s (%s)", strings.Join(cmd, " "), err, output)
	}
	return string(output), nil
}

// RestartOVNKubeNodePod restarts the ovnkube-node pod from namespace, running on nodeName
func RestartOVNKubeNodePod(clientset kubernetes.Interface, namespace string, nodeName string) error {
	ovnKubeNodePods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "name=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return fmt.Errorf("could not get ovnkube-node pods: %w", err)
	}

	if len(ovnKubeNodePods.Items) <= 0 {
		return fmt.Errorf("could not find ovnkube-node pod running on node %s", nodeName)
	}
	for _, pod := range ovnKubeNodePods.Items {
		if err := DeletePodWithWait(context.TODO(), clientset, &pod); err != nil {
			return fmt.Errorf("could not delete ovnkube-node pod on node %s: %w", nodeName, err)
		}
	}

	framework.Logf("waiting for node %s to have running ovnkube-node pod", nodeName)
	err = wait.Poll(2*time.Second, 3*time.Minute, func() (bool, error) {
		ovnKubeNodePods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: "name=ovnkube-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil {
			return false, fmt.Errorf("could not get ovnkube-node pods: %w", err)
		}

		if len(ovnKubeNodePods.Items) <= 0 {
			framework.Logf("Node %s has no ovnkube-node pod yet", nodeName)
			return false, nil
		}
		for _, pod := range ovnKubeNodePods.Items {
			if ready, err := testutils.PodRunningReady(&pod); !ready {
				framework.Logf("%v", err)
				return false, nil
			}
		}
		return true, nil
	})

	return err
}

// RestartOVNKubeNodePodsInParallel restarts multiple ovnkube-node pods in parallel. See `RestartOVNKubeNodePod`
func RestartOVNKubeNodePodsInParallel(clientset kubernetes.Interface, namespace string, nodeNames ...string) error {
	framework.Logf("restarting ovnkube-node for %v", nodeNames)

	restartFuncs := make([]func() error, 0, len(nodeNames))
	for _, n := range nodeNames {
		nodeName := n
		restartFuncs = append(restartFuncs, func() error {
			return RestartOVNKubeNodePod(clientset, OvnNamespace, nodeName)
		})
	}

	return utilerrors.AggregateGoroutines(restartFuncs...)
}

// GetOVNKubePodLogsFiltered retrieves logs from ovnkube-node pods and filters logs lines according to filteringRegexp
func GetOVNKubePodLogsFiltered(clientset kubernetes.Interface, namespace, nodeName, filteringRegexp string) (string, error) {
	ovnKubeNodePods, err := clientset.CoreV1().Pods(OvnNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "name=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return "", fmt.Errorf("GetOVNKubePodLogsFiltered: error while getting ovnkube-node pods: %w", err)
	}

	logs, err := e2epod.GetPodLogs(context.TODO(), clientset, OvnNamespace, ovnKubeNodePods.Items[0].Name, GetNodeContainerName())
	if err != nil {
		return "", fmt.Errorf("GetOVNKubePodLogsFiltered: error while getting ovnkube-node [%s/%s] logs: %w",
			OvnNamespace, ovnKubeNodePods.Items[0].Name, err)
	}

	scanner := bufio.NewScanner(strings.NewReader(logs))
	filteredLogs := ""
	re := regexp.MustCompile(filteringRegexp)
	for scanner.Scan() {
		line := scanner.Text()
		if re.MatchString(line) {
			filteredLogs += line + "\n"
		}
	}

	err = scanner.Err()
	if err != nil {
		return "", fmt.Errorf("GetOVNKubePodLogsFiltered: error while scanning ovnkube-node logs: %w", err)
	}

	return filteredLogs, nil
}

func FindOvnKubeControlPlaneNode(controlPlanePodName, leaseName string) (string, error) {

	ovnkubeControlPlaneNode, err := e2ekubectl.RunKubectl(OvnNamespace, "get", "leases", leaseName,
		"-o", "jsonpath='{.spec.holderIdentity}'")

	framework.ExpectNoError(err, fmt.Sprintf("Unable to retrieve leases (%s)"+
		"from %s %v", leaseName, OvnNamespace, err))

	framework.Logf(fmt.Sprintf("master instance of %s is running on node %s", controlPlanePodName, ovnkubeControlPlaneNode))
	// Strip leading and trailing quotes if present
	if ovnkubeControlPlaneNode[0] == '\'' || ovnkubeControlPlaneNode[0] == '"' {
		ovnkubeControlPlaneNode = ovnkubeControlPlaneNode[1 : len(ovnkubeControlPlaneNode)-1]
	}

	return ovnkubeControlPlaneNode, nil
}

func CreateSrcPod(podName, nodeName string, ipCheckInterval, ipCheckTimeout time.Duration, f *framework.Framework) {
	_, err := CreateGenericPod(f, podName, nodeName, f.Namespace.Name,
		[]string{"bash", "-c", "sleep 20000"})
	if err != nil {
		framework.Failf("Failed to create src pod %s: %v", podName, err)
	}
	// Wait for pod setup to be almost ready
	err = wait.PollImmediate(ipCheckInterval, ipCheckTimeout, func() (bool, error) {
		kubectlOut := GetPodAddress(podName, f.Namespace.Name)
		validIP := net.ParseIP(kubectlOut)
		if validIP == nil {
			return false, nil
		}
		return true, nil
	})
	// Fail the test if no address is ever retrieved
	if err != nil {
		framework.Failf("Error trying to get the pod IP address %v", err)
	}
}

func GetNodePodCIDRs(nodeName string) (string, string, error) {
	// retrieve the pod cidr for the worker node
	jsonFlag := "jsonpath='{.metadata.annotations.k8s\\.ovn\\.org/node-subnets}'"
	kubectlOut, err := e2ekubectl.RunKubectl("default", "get", "node", nodeName, "-o", jsonFlag)
	if err != nil {
		return "", "", err
	}
	// strip the apostrophe from stdout and parse the pod cidr
	annotation := strings.Replace(kubectlOut, "'", "", -1)

	var ipv4CIDR, ipv6CIDR string

	ssSubnets := make(map[string]string)
	if err := json.Unmarshal([]byte(annotation), &ssSubnets); err == nil {
		// If only one subnet, determine if it's v4 or v6
		if subnet, ok := ssSubnets["default"]; ok {
			if strings.Contains(subnet, ":") {
				ipv6CIDR = subnet
			} else {
				ipv4CIDR = subnet
			}
			return ipv4CIDR, ipv6CIDR, nil
		}
	}

	dsSubnets := make(map[string][]string)
	if err := json.Unmarshal([]byte(annotation), &dsSubnets); err == nil {
		if subnets, ok := dsSubnets["default"]; ok && len(subnets) > 0 {
			// Classify each subnet as IPv4 or IPv6
			for _, subnet := range subnets {
				if strings.Contains(subnet, ":") {
					ipv6CIDR = subnet
				} else {
					ipv4CIDR = subnet
				}
			}
			return ipv4CIDR, ipv6CIDR, nil
		}
	}

	return "", "", fmt.Errorf("could not parse annotation %q", annotation)
}
