// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/portalloc"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/testcontext"
	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/utils/ptr"
)

const ProviderName = "kube"

const ovnKubeNodeLabel = "app in (ovnkube-node,ovnkube-node-dpu,ovnkube-node-dpu-host)"

type kube struct {
	hostPort      *portalloc.PortAllocator
	nodeShellsMu  sync.Mutex
	nodeShells    map[string]*corev1.Pod
	nodeShellCall singleflight.Group
}

func New() api.Provider {
	return &kube{
		hostPort:   portalloc.New(1024, 65535),
		nodeShells: map[string]*corev1.Pod{},
	}
}

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

func (k *kube) PreloadImages(_ []string) {
	ginkgo.DeferCleanup(k.deleteNodeShells)
}

func (k *kube) deleteNodeShells() error {
	client, err := framework.LoadClientset()
	if err != nil {
		return err
	}
	k.nodeShellsMu.Lock()
	defer k.nodeShellsMu.Unlock()
	var errs []error
	for _, shell := range k.nodeShells {
		ctx, cancel := apiCallContext()
		err := client.CoreV1().Pods(shell.Namespace).Delete(ctx, shell.Name, metav1.DeleteOptions{})
		cancel()
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (k *kube) PrimaryNetwork() (api.Network, error) {
	return nil, skip("PrimaryNetwork", "the kube API does not expose infrastructure networks")
}

func (k *kube) GetK8NodeNetworkInterface(string, api.Network) (api.NetworkInterface, error) {
	return api.NetworkInterface{}, skip("GetK8NodeNetworkInterface", "the kube API does not expose host interfaces")
}

func (k *kube) ExecK8NodeCommand(nodeName string, cmd []string) (string, error) {
	if stopsKubelet(cmd) {
		return "", skip("ExecK8NodeCommand", "node commands use the kubelet and cannot restart it after stopping it")
	}
	shell, err := k.nodeShell(nodeName)
	if err != nil {
		return "", err
	}
	return e2ekubectl.RunKubectl(shell.Namespace,
		append([]string{"exec", shell.Name, "--", "chroot", "/host"}, cmd...)...)
}

func stopsKubelet(cmd []string) bool {
	return len(cmd) >= 3 && cmd[0] == "systemctl" && cmd[1] == "stop" && cmd[2] == "kubelet.service"
}

func (k *kube) nodeShell(nodeName string) (*corev1.Pod, error) {
	client, err := framework.LoadClientset()
	if err != nil {
		return nil, err
	}
	return k.nodeShellWithClient(client, nodeName, func() (*corev1.Pod, error) {
		namespace := deploymentconfig.Get().OVNKubernetesNamespace()
		image, err := ovnKubeNodeImage(client, namespace, nodeName)
		if err != nil {
			return nil, err
		}
		return k.createNodeShell(client, namespace, nodeName, image)
	})
}

func (k *kube) nodeShellWithClient(client clientset.Interface, nodeName string, create func() (*corev1.Pod, error)) (*corev1.Pod, error) {
	value, err, _ := k.nodeShellCall.Do(nodeName, func() (any, error) {
		k.nodeShellsMu.Lock()
		shell := k.nodeShells[nodeName]
		k.nodeShellsMu.Unlock()
		if shell != nil {
			ctx, cancel := apiCallContext()
			current, err := client.CoreV1().Pods(shell.Namespace).Get(ctx, shell.Name, metav1.GetOptions{})
			cancel()
			if err != nil && !apierrors.IsNotFound(err) {
				return nil, err
			}
			if err == nil && current.DeletionTimestamp == nil && current.Status.Phase == corev1.PodRunning {
				return current, nil
			}
			if err == nil && current.DeletionTimestamp == nil {
				ctx, cancel = apiCallContext()
				err = client.CoreV1().Pods(shell.Namespace).Delete(ctx, shell.Name, metav1.DeleteOptions{})
				cancel()
				if err != nil && !apierrors.IsNotFound(err) {
					return nil, err
				}
			}
			k.nodeShellsMu.Lock()
			delete(k.nodeShells, nodeName)
			k.nodeShellsMu.Unlock()
		}
		shell, err := create()
		if shell != nil {
			k.nodeShellsMu.Lock()
			k.nodeShells[nodeName] = shell
			k.nodeShellsMu.Unlock()
		}
		return shell, err
	})
	if err != nil {
		return nil, err
	}
	return value.(*corev1.Pod), nil
}

func (k *kube) createNodeShell(client clientset.Interface, namespace, nodeName, image string) (*corev1.Pod, error) {
	ctx, cancel := apiCallContext()
	shell, err := client.CoreV1().Pods(namespace).Create(ctx,
		nodeShellPod(nodeName, image), metav1.CreateOptions{})
	cancel()
	if err != nil {
		return nil, err
	}
	if err := e2epod.WaitForPodRunningInNamespace(context.Background(), client, shell); err != nil {
		ctx, cancel := apiCallContext()
		deleteErr := client.CoreV1().Pods(namespace).Delete(ctx, shell.Name, metav1.DeleteOptions{})
		cancel()
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return shell, errors.Join(err, deleteErr)
		}
		return nil, err
	}
	return shell, nil
}

func ovnKubeNodeImage(client clientset.Interface, namespace, nodeName string) (string, error) {
	ctx, cancel := apiCallContext()
	defer cancel()
	pods, err := client.CoreV1().Pods(namespace).List(ctx,
		metav1.ListOptions{LabelSelector: ovnKubeNodeLabel, FieldSelector: "spec.nodeName=" + nodeName})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no ovnkube-node pod on node %s to take a shell image from", nodeName)
	}
	return pods.Items[0].Spec.Containers[0].Image, nil
}

func apiCallContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), framework.SingleCallTimeout)
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
				VolumeMounts:    []corev1.VolumeMount{{Name: "host", MountPath: "/host"}},
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
	return nil, skip("ListNetworks", "the kube API does not expose infrastructure networks")
}

func (k *kube) GetNetwork(string) (api.Network, error) {
	return nil, skip("GetNetwork", "the kube API does not expose infrastructure networks")
}

func (k *kube) GetExternalContainerNetworkInterface(api.ExternalContainer, api.Network) (api.NetworkInterface, error) {
	return api.NetworkInterface{}, skip("GetExternalContainerNetworkInterface", "the kube API does not manage external containers")
}

func (k *kube) ExecExternalContainerCommand(api.ExternalContainer, []string) (string, error) {
	return "", skip("ExecExternalContainerCommand", "the kube API does not manage external containers")
}

func (k *kube) GetExternalContainerLogs(api.ExternalContainer) (string, error) {
	return "", skip("GetExternalContainerLogs", "the kube API does not manage external containers")
}

func (k *kube) GetExternalContainerPort() uint16 {
	ginkgo.Skip("kube provider does not manage external containers", 2)
	return 0
}

func (k *kube) ExternalContainerPrimaryInterfaceName() string {
	ginkgo.Skip("kube provider does not manage external containers", 2)
	return ""
}

func (k *kube) NewTestContext() api.Context {
	context := &testcontext.TestContext{}
	ginkgo.DeferCleanup(context.CleanUp)
	return &contextKube{TestContext: context}
}

type contextKube struct {
	*testcontext.TestContext
}

func (c *contextKube) CreateNetwork(string, ...string) (api.Network, error) {
	return nil, skip("CreateNetwork", "the kube API does not manage infrastructure networks")
}

func (c *contextKube) DeleteNetwork(api.Network) error {
	return skip("DeleteNetwork", "the kube API does not manage infrastructure networks")
}

func (c *contextKube) CreateExternalContainer(api.ExternalContainer) (api.ExternalContainer, error) {
	return api.ExternalContainer{}, skip("CreateExternalContainer", "the kube API does not manage external containers")
}

func (c *contextKube) DeleteExternalContainer(api.ExternalContainer) error {
	return skip("DeleteExternalContainer", "the kube API does not manage external containers")
}

func (c *contextKube) AttachNetwork(api.Network, string) (api.NetworkInterface, error) {
	return api.NetworkInterface{}, skip("AttachNetwork", "the kube API does not manage infrastructure networks")
}

func (c *contextKube) DetachNetwork(api.Network, string) error {
	return skip("DetachNetwork", "the kube API does not manage infrastructure networks")
}

func (c *contextKube) SetupUnderlay(*framework.Framework, api.Underlay) error {
	return skip("SetupUnderlay", "no spare host uplink is declared for this cluster")
}
