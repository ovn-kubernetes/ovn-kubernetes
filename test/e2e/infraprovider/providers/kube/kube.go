// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package kube runs the e2e suite against a cluster it did not create.
package kube

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/portalloc"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/runner"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/testcontext"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
)

const ProviderName = "kube"

const (
	// ContainerHostEnvVar names a host reachable over SSH whose container runtime runs the
	// external containers some specs need.
	ContainerHostEnvVar = "OVN_TEST_CONTAINER_HOST"
	// PrimaryNetworkEnvVar names the network on that host over which it reaches the Nodes.
	PrimaryNetworkEnvVar = "OVN_TEST_PRIMARY_NETWORK"

	containerHostUserEnvVar = "OVN_TEST_CONTAINER_HOST_USER"
	containerHostPortEnvVar = "OVN_TEST_CONTAINER_HOST_PORT"
	containerHostKeyEnvVar  = "OVN_TEST_CONTAINER_HOST_KEY"
	containerRuntimeEnvVar  = "CONTAINER_RUNTIME"
)

const whyNodeNotContainer = "the Nodes are not containers on any host the suite can reach"

type kube struct {
	engine         *container.Engine
	primaryNetwork string
	hostPort       *portalloc.PortAllocator
	nodeShells     map[string]*corev1.Pod
}

func New() api.Provider {
	return &kube{
		engine:         newContainerEngine(),
		primaryNetwork: os.Getenv(PrimaryNetworkEnvVar),
		hostPort:       portalloc.New(1024, 65535),
		nodeShells:     map[string]*corev1.Pod{},
	}
}

// newContainerEngine returns nil where no host is declared, leaving the specs needing one to
// skip. A declared host that cannot be used stops the run before any spec reports success.
func newContainerEngine() *container.Engine {
	host := os.Getenv(ContainerHostEnvVar)
	if host == "" {
		return nil
	}
	if os.Getenv(PrimaryNetworkEnvVar) == "" {
		klog.Fatalf("%s is set, so %s must name the network on it that reaches the Nodes",
			ContainerHostEnvVar, PrimaryNetworkEnvVar)
	}
	sshRunner, err := runner.NewSSHRunner(host, cmp.Or(os.Getenv(containerHostUserEnvVar), "root"),
		cmp.Or(os.Getenv(containerHostPortEnvVar), "22"), os.Getenv(containerHostKeyEnvVar))
	if err != nil {
		klog.Fatalf("%s is set to %q but cannot be used: %v", ContainerHostEnvVar, host, err)
	}
	return container.NewEngine(cmp.Or(os.Getenv(containerRuntimeEnvVar), "docker"), sshRunner)
}

var noContainerHost = fmt.Sprintf("this spec needs a container outside the cluster; set %s to a host that can run one",
	ContainerHostEnvVar)

func (k *kube) containerEngine() *container.Engine {
	if k.engine == nil {
		ginkgo.Skip(noContainerHost, 2)
	}
	return k.engine
}

// skip panics out of the calling spec, so the returned error never reaches the caller. The
// callerSkip of 2 attributes the skip to the spec rather than to this file.
func skip(op, why string) error {
	err := fmt.Errorf("%s provider does not serve %s: %s", ProviderName, op, why)
	ginkgo.Skip(err.Error(), 2)
	return err
}

func (k *kube) Name() string {
	return ProviderName
}

func (k *kube) GetDefaultTimeoutContext() *framework.TimeoutContext {
	return framework.NewTimeoutContext()
}

func (k *kube) GetK8HostPort() uint16 {
	return k.hostPort.Allocate()
}

// PreloadImages is the only hook the provider gets inside BeforeSuite, and so the one place it can
// register cleanup: Ginkgo ends the process if DeferCleanup is called from within a cleanup, as the
// suite's node commands would.
func (k *kube) PreloadImages(_ []string) {
	ginkgo.DeferCleanup(k.deleteNodeShells)
}

func (k *kube) deleteNodeShells() error {
	if len(k.nodeShells) == 0 {
		return nil
	}
	client, err := framework.LoadClientset()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var errs []error
	for _, shell := range k.nodeShells {
		err := client.CoreV1().Pods(shell.Namespace).Delete(ctx, shell.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (k *kube) PrimaryNetwork() (api.Network, error) {
	return k.containerEngine().GetNetwork(k.primaryNetwork)
}

// GetK8NodeNetworkInterface takes the address OVN-K publishes for its own uplink, rather than
// choosing among the addresses a Node carries, where an egress IP and an API VIP are
// indistinguishable from the Node's own.
func (k *kube) GetK8NodeNetworkInterface(instance string, network api.Network) (api.NetworkInterface, error) {
	if network.Name() != k.primaryNetwork {
		return api.NetworkInterface{}, skip("GetK8NodeNetworkInterface",
			fmt.Sprintf("network %s is not the one the Nodes are on, and they cannot be put on another", network.Name()))
	}
	client, err := framework.LoadClientset()
	if err != nil {
		return api.NetworkInterface{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	node, err := client.CoreV1().Nodes().Get(ctx, instance, metav1.GetOptions{})
	if err != nil {
		return api.NetworkInterface{}, fmt.Errorf("get node %s: %w", instance, err)
	}
	primary, err := util.ParseNodePrimaryIfAddr(node)
	if util.IsAnnotationNotSetError(err) {
		return api.NetworkInterface{}, skip("GetK8NodeNetworkInterface",
			fmt.Sprintf("node %s does not publish the address of its uplink yet", instance))
	}
	if err != nil {
		return api.NetworkInterface{}, err
	}
	v4Subnet, v6Subnet, err := network.IPv4IPv6Subnets()
	if err != nil {
		return api.NetworkInterface{}, err
	}

	inf := api.NetworkInterface{}
	if subnetHolds(v4Subnet, primary.V4.IP) {
		inf.IPv4, inf.IPv4Prefix = primary.V4.IP.String(), prefixOf(primary.V4.Net)
	}
	if subnetHolds(v6Subnet, primary.V6.IP) {
		inf.IPv6, inf.IPv6Prefix = primary.V6.IP.String(), prefixOf(primary.V6.Net)
	}
	if inf.IPv4 == "" && inf.IPv6 == "" {
		return api.NetworkInterface{}, fmt.Errorf("node %s is on neither subnet of network %s (%q and %q), so %s does not name a network that reaches the Nodes",
			instance, network.Name(), v4Subnet, v6Subnet, PrimaryNetworkEnvVar)
	}
	if inf.InfName, inf.MAC, err = k.nodeLinkHolding(instance, cmp.Or(inf.IPv4, inf.IPv6)); err != nil {
		return api.NetworkInterface{}, err
	}
	return inf, nil
}

func prefixOf(subnet *net.IPNet) string {
	ones, _ := subnet.Mask.Size()
	return strconv.Itoa(ones)
}

func subnetHolds(subnet string, ip net.IP) bool {
	_, ipNet, err := utilnet.ParseCIDRSloppy(subnet)
	return err == nil && ipNet.Contains(ip)
}

// nodeLinkHolding returns the interface carrying the given address. Several holders is not a tie
// to break: specs put a copy of a Node address on its loopback, whose MAC is the wrong answer.
func (k *kube) nodeLinkHolding(instance, address string) (string, string, error) {
	out, err := k.ExecK8NodeCommand(instance, []string{"ip", "-j", "addr", "show"})
	if err != nil {
		return "", "", err
	}
	var links []struct {
		Name     string `json:"ifname"`
		MAC      string `json:"address"`
		AddrInfo []struct {
			Local string `json:"local"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return "", "", fmt.Errorf("failed to read the interfaces of node %s from %q: %v", instance, out, err)
	}

	var holders []string
	var name, mac string
	want := net.ParseIP(address)
	for _, link := range links {
		for _, addr := range link.AddrInfo {
			if net.ParseIP(addr.Local).Equal(want) {
				holders = append(holders, link.Name)
				name, mac = link.Name, link.MAC
			}
		}
	}
	if len(holders) == 0 {
		return "", "", fmt.Errorf("no interface on node %s holds %s, which it publishes as the address of its uplink", instance, address)
	}
	if len(holders) > 1 {
		return "", "", fmt.Errorf("interfaces %v on node %s all hold %s, so the one the specs mean cannot be told apart", holders, instance, address)
	}
	return name, mac, nil
}

func (k *kube) ExecK8NodeCommand(nodeName string, cmd []string) (string, error) {
	shell, err := k.nodeShell(nodeName)
	if err != nil {
		return "", err
	}
	return e2ekubectl.RunKubectl(shell.Namespace,
		append([]string{"exec", shell.Name, "--", "chroot", "/host"}, cmd...)...)
}

// nodeShell returns a running shell pod on nodeName, one per node for the whole run. It lives in
// the OVN-K namespace because a privileged host-network pod is already admitted there.
func (k *kube) nodeShell(nodeName string) (*corev1.Pod, error) {
	client, err := framework.LoadClientset()
	if err != nil {
		return nil, err
	}
	if shell, ok := k.nodeShells[nodeName]; ok {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		current, err := client.CoreV1().Pods(shell.Namespace).Get(ctx, shell.Name, metav1.GetOptions{})
		if err == nil && current.Status.Phase == corev1.PodRunning && current.DeletionTimestamp == nil {
			return current, nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get node shell on %s: %w", nodeName, err)
		}
		_ = client.CoreV1().Pods(shell.Namespace).Delete(ctx, shell.Name, metav1.DeleteOptions{})
		delete(k.nodeShells, nodeName)
	}
	namespace := deploymentconfig.Get().OVNKubernetesNamespace()
	image, err := ovnKubeNodeImage(client, namespace, nodeName)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	shell, err := client.CoreV1().Pods(namespace).Create(ctx, nodeShellPod(nodeName, image), metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create node shell on %s: %w", nodeName, err)
	}
	if err := e2epod.WaitForPodRunningInNamespace(ctx, client, shell); err != nil {
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer deleteCancel()
		_ = client.CoreV1().Pods(namespace).Delete(deleteCtx, shell.Name, metav1.DeleteOptions{})
		return nil, fmt.Errorf("wait for node shell on %s: %w", nodeName, err)
	}
	k.nodeShells[nodeName] = shell
	return shell, nil
}

func ovnKubeNodeImage(client clientset.Interface, namespace, nodeName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pods, err := client.CoreV1().Pods(namespace).List(ctx,
		metav1.ListOptions{LabelSelector: "app=ovnkube-node", FieldSelector: "spec.nodeName=" + nodeName})
	if err != nil {
		return "", fmt.Errorf("list ovnkube-node pods on %s: %w", nodeName, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no ovnkube-node pod on node %s to take a shell image from", nodeName)
	}
	return pods.Items[0].Spec.Containers[0].Image, nil
}

func nodeShellPod(nodeName, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "ovn-e2e-node-shell-"},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			HostNetwork:   true,
			HostPID:       true,
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:            "shell",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sleep", "infinity"},
				SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				}},
				VolumeMounts: []corev1.VolumeMount{{Name: "host", MountPath: "/host"}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "host",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}},
			}},
		},
	}
}

func (k *kube) ShutdownNode(string) error {
	return skip("ShutdownNode", "the kube API does not power nodes on or off")
}

func (k *kube) StartNode(string) error {
	return skip("StartNode", "the kube API does not power nodes on or off")
}

func (k *kube) ListNetworks() ([]string, error) {
	return k.containerEngine().ListNetworks()
}

func (k *kube) GetNetwork(name string) (api.Network, error) {
	return k.containerEngine().GetNetwork(name)
}

func (k *kube) GetExternalContainerNetworkInterface(external api.ExternalContainer, network api.Network) (api.NetworkInterface, error) {
	return k.containerEngine().GetExternalContainerNetworkInterface(external, network)
}

func (k *kube) ExecExternalContainerCommand(external api.ExternalContainer, cmd []string) (string, error) {
	return k.containerEngine().ExecExternalContainerCommand(external, cmd)
}

func (k *kube) GetExternalContainerLogs(external api.ExternalContainer) (string, error) {
	return k.containerEngine().GetExternalContainerLogs(external)
}

func (k *kube) GetExternalContainerPort() uint16 {
	return k.containerEngine().GetExternalContainerPort()
}

func (k *kube) ExternalContainerPrimaryInterfaceName() string {
	return k.containerEngine().ExternalContainerPrimaryInterfaceName()
}

func (k *kube) NewTestContext() api.Context {
	context := &testcontext.TestContext{}
	ginkgo.DeferCleanup(context.CleanUp)
	return &contextKube{TestContext: context, kube: k}
}

type contextKube struct {
	*testcontext.TestContext
	kube *kube
}

func (c *contextKube) containerEngine() *container.Engine {
	if c.kube.engine == nil {
		ginkgo.Skip(noContainerHost, 2)
	}
	return c.kube.engine.WithTestContext(c.TestContext)
}

func (c *contextKube) CreateNetwork(name string, subnets ...string) (api.Network, error) {
	return c.containerEngine().CreateNetwork(name, subnets...)
}

func (c *contextKube) DeleteNetwork(network api.Network) error {
	return c.containerEngine().DeleteNetwork(network)
}

func (c *contextKube) CreateExternalContainer(external api.ExternalContainer) (api.ExternalContainer, error) {
	return c.containerEngine().CreateExternalContainer(external)
}

func (c *contextKube) DeleteExternalContainer(external api.ExternalContainer) error {
	return c.containerEngine().DeleteExternalContainer(external)
}

// AttachNetwork is given a Node as often as an external container and the two cannot be told
// apart by name, so it skips rather than working for some specs and failing for others.
func (c *contextKube) AttachNetwork(api.Network, string) (api.NetworkInterface, error) {
	return api.NetworkInterface{}, skip("AttachNetwork", whyNodeNotContainer)
}

func (c *contextKube) DetachNetwork(api.Network, string) error {
	return skip("DetachNetwork", whyNodeNotContainer)
}

func (c *contextKube) SetupUnderlay(*framework.Framework, api.Underlay) error {
	return skip("SetupUnderlay", "no spare host uplink is declared for this cluster")
}
