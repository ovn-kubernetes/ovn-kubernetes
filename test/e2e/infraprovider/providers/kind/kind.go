// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kind

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/portalloc"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/testcontext"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
)

type kind struct {
	containerRuntime ContainerRuntime
	engine           *container.Engine
	cmdRunner        api.Runner
	HostPort         *portalloc.PortAllocator
}

func New(ce ContainerRuntime, cmdRunner api.Runner) api.Provider {
	if !infraprovider.IsKind() {
		panic("Cluster provider must be KinD type")
	}
	kind := &kind{
		containerRuntime: ce,
		cmdRunner:        cmdRunner,
		engine:           container.NewEngine(ce.String(), cmdRunner),
		HostPort:         portalloc.New(1024, 65535)}
	return kind
}

func (k *kind) Name() string {
	return "kind"
}

func (k *kind) PrimaryNetwork() (api.Network, error) {
	return k.GetNetwork("kind")
}

func (k *kind) GetNetwork(name string) (api.Network, error) {
	return k.engine.GetNetwork(name)
}

func (k *kind) GetDefaultTimeoutContext() *framework.TimeoutContext {
	return framework.NewTimeoutContext()
}

func (k *kind) GetK8HostPort() uint16 {
	return k.HostPort.Allocate()
}

func (k *kind) GetK8NodeNetworkInterface(container string, network api.Network) (api.NetworkInterface, error) {
	return k.engine.GetNetworkInterface(container, network.Name())
}

func (k *kind) ExecK8NodeCommand(nodeName string, cmd []string) (string, error) {
	return k.engine.ExecContainerCommand(nodeName, cmd)
}

func (k *kind) ExecExternalContainerCommand(container api.ExternalContainer, cmd []string) (string, error) {
	return k.engine.ExecExternalContainerCommand(container, cmd)
}

func (k *kind) ExternalContainerPrimaryInterfaceName() string {
	return k.engine.ExternalContainerPrimaryInterfaceName()
}

func (k *kind) GetExternalContainerLogs(container api.ExternalContainer) (string, error) {
	return k.engine.GetExternalContainerLogs(container)
}

func (k *kind) GetExternalContainerNetworkInterface(container api.ExternalContainer, network api.Network) (api.NetworkInterface, error) {
	return k.engine.GetExternalContainerNetworkInterface(container, network)
}

func (k *kind) GetExternalContainerPort() uint16 {
	return k.engine.GetExternalContainerPort()
}

func (k *kind) ListNetworks() ([]string, error) {
	return k.engine.ListNetworks()
}

func (k *kind) PreloadImages(imgs []string) {
	clusterName := kindClusterName()
	if clusterName == "" {
		framework.Logf("Warning: could not determine KIND cluster name, skipping image preload")
		return
	}
	pullBackoff := wait.Backoff{Duration: 5 * time.Second, Factor: 2, Steps: 5}
	for _, img := range imgs {
		framework.Logf("Preloading image %s into KIND cluster %s", img, clusterName)
		var out string
		err := wait.ExponentialBackoff(pullBackoff, func() (bool, error) {
			var pullErr error
			out, pullErr = k.cmdRunner.Run(k.containerRuntime.String(), "pull", img)
			if pullErr != nil {
				framework.Logf("Retrying pull for image %s: %v (%s)", img, pullErr, out)
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			framework.Logf("Warning: failed to pull image %s after retries: %v (%s)", img, err, out)
			continue
		}
		if k.containerRuntime == podman {
			if _, err := k.cmdRunner.Run("rm", "-f", "/tmp/image.tar"); err != nil {
				framework.Logf("Warning: failed to remove stale image archive /tmp/image.tar: %v", err)
			}
			out, err = k.cmdRunner.Run(k.containerRuntime.String(), "save", "-o", "/tmp/image.tar", img)
			if err != nil {
				framework.Logf("Warning: failed to save image %s: %v (%s)", img, err, out)
				continue
			}
			out, err = k.cmdRunner.Run("kind", "load", "image-archive", "/tmp/image.tar", "--name", clusterName)
		} else {
			out, err = k.cmdRunner.Run("kind", "load", "docker-image", img, "--name", clusterName)
		}
		if err != nil {
			framework.Logf("Warning: failed to load image %s into KIND cluster %s: %v (%s)", img, clusterName, err, out)
			continue
		}
		framework.Logf("Preloaded image %s into KIND cluster %s", img, clusterName)
	}
}

func kindClusterName() string {
	// Use exec.Command() directly (not cmdRunner) to read the local kubeconfig.
	// This works for both local execution (upstream) and remote KIND clusters (vendor),
	// since kubeconfig is always local while container operations run on the KIND host.
	currentCtx, err := exec.Command("kubectl", "config", "current-context").CombinedOutput()
	if err != nil {
		return ""
	}
	ctx := strings.TrimSpace(string(currentCtx))
	// KIND contexts are named "kind-<cluster-name>"
	if strings.HasPrefix(ctx, "kind-") {
		return strings.TrimPrefix(ctx, "kind-")
	}
	return ""
}

func (k *kind) ShutdownNode(nodeName string) error {
	return k.engine.StopContainer(nodeName)
}

func (k *kind) StartNode(nodeName string) error {
	return k.engine.StartContainer(nodeName)
}

func (k *kind) NewTestContext() api.Context {
	context := &testcontext.TestContext{}
	ginkgo.DeferCleanup(context.CleanUp)
	ck := &contextKind{
		TestContext: context,
		engine:      k.engine.WithTestContext(context),
	}
	return ck
}

type contextKind struct {
	*testcontext.TestContext
	engine *container.Engine
}

func (c *contextKind) CreateExternalContainer(container api.ExternalContainer) (api.ExternalContainer, error) {
	return c.engine.CreateExternalContainer(container)
}

func (c *contextKind) DeleteExternalContainer(container api.ExternalContainer) error {
	return c.engine.DeleteExternalContainer(container)
}

func (c *contextKind) CreateNetwork(name string, subnets ...string) (api.Network, error) {
	return c.engine.CreateNetwork(name, subnets...)
}

func (c *contextKind) AttachNetwork(network api.Network, container string) (api.NetworkInterface, error) {
	return c.engine.AttachNetwork(network, container)
}

func (c *contextKind) DetachNetwork(network api.Network, container string) error {
	return c.engine.DetachNetwork(network, container)
}

func (c *contextKind) DeleteNetwork(network api.Network) error {
	return c.engine.DeleteNetwork(network)
}

func (c *contextKind) SetupUnderlay(f *framework.Framework, underlay api.Underlay) error {
	if underlay.LogicalNetworkName == "" {
		return fmt.Errorf("underlay logical network name must be set")
	}

	if underlay.PhysicalNetworkName == "" {
		underlay.PhysicalNetworkName = "underlay"
	}

	if underlay.BridgeName == "" {
		underlay.BridgeName = secondaryBridge
	}

	c.AddCleanUpFn(func() error {
		// Find the OVS pods again to cover cases that restart the PODs
		ovsPods, err := findOVSPods(f)
		if err != nil {
			return fmt.Errorf("failed finding OVS pods during kind underlay tear down: %w", err)
		}
		for _, ovsPod := range ovsPods {
			if underlay.BridgeName != deploymentconfig.Get().ExternalBridgeName() {
				if err := removeOVSBridge(ovsPod.Namespace, ovsPod.Name, underlay.BridgeName); err != nil {
					return fmt.Errorf("failed to remove OVS bridge %s for pod %s/%s during cleanup: %w", underlay.BridgeName, ovsPod.Namespace, ovsPod.Name, err)
				}
			}
			if err := configureBridgeMappings(
				ovsPod.Namespace,
				ovsPod.Name,
				defaultNetworkBridgeMapping(),
			); err != nil {
				return fmt.Errorf("failed to restore default bridge mappings for pod %s/%s during cleanup: %w", ovsPod.Namespace, ovsPod.Name, err)
			}
		}
		return nil
	})

	ovsPods, err := findOVSPods(f)
	if err != nil {
		return fmt.Errorf("failed finding OVS pods during kind underlay setup: %w", err)
	}
	for _, ovsPod := range ovsPods {
		if underlay.BridgeName != deploymentconfig.Get().ExternalBridgeName() {
			underlayInterface, err := c.engine.GetNetworkInterface(ovsPod.Spec.NodeName, underlay.PhysicalNetworkName)
			if err != nil {
				return fmt.Errorf("failed to get underlay interface for network %s on node %s: %w", underlay.PhysicalNetworkName, ovsPod.Spec.NodeName, err)
			}
			if err := ensureOVSBridge(ovsPod.Namespace, ovsPod.Name, underlay.BridgeName); err != nil {
				return fmt.Errorf("failed to add OVS bridge %s for pod %s/%s: %w", underlay.BridgeName, ovsPod.Namespace, ovsPod.Name, err)
			}

			if err := ovsAttachPortToBridge(ovsPod.Namespace, ovsPod.Name, underlay.BridgeName, underlayInterface.InfName); err != nil {
				return fmt.Errorf("failed to attach port %s to bridge %s for pod %s/%s: %w", underlayInterface.InfName, underlay.BridgeName, ovsPod.Namespace, ovsPod.Name, err)
			}
			if underlay.VlanID > 0 {
				if err := ovsEnableVLANAccessPort(ovsPod.Namespace, ovsPod.Name, underlay.BridgeName, underlayInterface.InfName, underlay.VlanID); err != nil {
					return fmt.Errorf("failed to enable VLAN %d on port %s for bridge %s for pod %s/%s: %w", underlay.VlanID, underlayInterface.InfName, underlay.BridgeName, ovsPod.Namespace, ovsPod.Name, err)
				}
			}
		}
		if err := configureBridgeMappings(
			ovsPod.Namespace,
			ovsPod.Name,
			defaultNetworkBridgeMapping(),
			bridgeMapping(underlay.LogicalNetworkName, underlay.BridgeName),
		); err != nil {
			return fmt.Errorf("failed to configure bridge mappings for pod %s/%s for logical network %s to bridge %s: %w", ovsPod.Namespace, ovsPod.Name, underlay.LogicalNetworkName, underlay.BridgeName, err)
		}
	}

	return nil
}

func findOVSPods(f *framework.Framework) ([]corev1.Pod, error) {
	const ovsKubeNodeLabel = "app=ovnkube-node"
	ovsPodList, err := f.ClientSet.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: ovsKubeNodeLabel},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list OVS pods with label %q at namespace %q: %w", ovsKubeNodeLabel, deploymentconfig.Get().OVNKubernetesNamespace(), err)
	}

	if len(ovsPodList.Items) == 0 {
		return nil, fmt.Errorf("no pods with label %q in namespace %q", ovsKubeNodeLabel, deploymentconfig.Get().OVNKubernetesNamespace())
	}
	return ovsPodList.Items, nil
}
