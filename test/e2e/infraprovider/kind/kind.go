package kind

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/internal/engine/container"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/portalloc"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
)

// IsProvider returns true if clusters provider is KinD
func IsProvider() bool {
	_, err := exec.LookPath("kubectl")
	if err != nil {
		framework.Logf("kubectl is not installed: %v", err)
		return false
	}
	currentCtx, err := exec.Command("kubectl", "config", "current-context").CombinedOutput()
	if err != nil {
		framework.Logf("unable to get current cluster context: %v", err)
		return false
	}
	if strings.Contains(string(currentCtx), "kind-ovn") {
		return true
	}
	return false
}

type kind struct {
	container.Provider
	engine string
}

func New() api.Provider {
	containerEngine := container.Get()
	kind := &kind{
		Provider: container.Provider{
			ExternalContainerPort: portalloc.New(12000, 65535),
			HostPort:              portalloc.New(1024, 65535),
		},
		engine: containerEngine.String()}
	kind.ContainerOps = &container.ContainerOps{CmdRunner: kind}
	return kind
}

func (r *kind) Run(args ...string) (string, error) {
	out, err := exec.Command(r.engine, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("command failed: %w, output: %s", err, out)
	}
	return string(out), nil
}

func (k *kind) Name() string {
	return "kind"
}

func (k *kind) PrimaryNetwork() (api.Network, error) {
	return k.GetNetwork("kind")
}

func (k *kind) GetNetwork(name string) (api.Network, error) {
	return k.ContainerOps.GetNetwork(name)
}

func (k *kind) GetDefaultTimeoutContext() *framework.TimeoutContext {
	return framework.NewTimeoutContext()
}

func (k *kind) ShutdownNode(nodeName string) error {
	return k.ShutdownContainer(nodeName)
}

func (k *kind) StartNode(nodeName string) error {
	return k.StartContainer(nodeName)
}

func (k *kind) NewTestContext() api.Context {
	ck := &contextKind{
		TestContext: container.TestContext{
			Mutex:        sync.Mutex{},
			ContainerOps: k.ContainerOps,
		},
	}
	ginkgo.DeferCleanup(ck.CleanUp)
	return ck
}

type contextKind struct {
	container.TestContext
}

func (c *contextKind) GetAttachedNetworks() (api.Networks, error) {
	c.Lock()
	defer c.Unlock()
	return c.getAttachedNetworks()
}

func (c *contextKind) getAttachedNetworks() (api.Networks, error) {
	primaryNetwork, err := c.GetNetwork("kind")
	if err != nil {
		return api.Networks{}, fmt.Errorf("failed to get primary network: %v", err)
	}
	attachedNetworks := api.Networks{List: []api.Network{primaryNetwork}}
	for _, attachment := range c.CleanUpNetworkAttachments.List {
		attachedNetworks.InsertNoDupe(attachment.Network)
	}
	return attachedNetworks, nil
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
			underlayInterface, err := c.GetNetworkInterface(ovsPod.Spec.NodeName, underlay.PhysicalNetworkName)
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
