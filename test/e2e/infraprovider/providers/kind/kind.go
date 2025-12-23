package kind

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/containerengine"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/portalloc"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	utilnet "k8s.io/utils/net"
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
	externalContainerPort *portalloc.PortAllocator
	hostPort              *portalloc.PortAllocator
}

func New() api.Provider {
	return &kind{externalContainerPort: portalloc.New(12000, 65535), hostPort: portalloc.New(1024, 65535)}
}

func (k *kind) Name() string {
	return "kind"
}

func (k *kind) PrimaryNetwork() (api.Network, error) {
	return getNetwork("kind")
}

func (k *kind) ExternalContainerPrimaryInterfaceName() string {
	return "eth0"
}

func (k *kind) GetNetwork(name string) (api.Network, error) {
	return getNetwork(name)
}

func (k *kind) GetExternalContainerNetworkInterface(container api.ExternalContainer, network api.Network) (api.NetworkInterface, error) {
	return getNetworkInterface(container.Name, network.Name())
}

func (k *kind) GetK8NodeNetworkInterface(container string, network api.Network) (api.NetworkInterface, error) {
	return getNetworkInterface(container, network.Name())
}

func (k *kind) ExecK8NodeCommand(nodeName string, cmd []string) (string, error) {
	exists, err := doesContainerNameExist(nodeName)
	if err != nil {
		return "", fmt.Errorf("failed to check if container %q exists: %w", nodeName, err)
	}
	if !exists {
		return "", fmt.Errorf("cannot exec into container %q because it doesn't exist: %w", nodeName, api.NotFound)
	}
	if len(cmd) == 0 {
		return "", fmt.Errorf("ExecK8NodeCommand(): insufficient command arguments")
	}
	cmdArgs := append([]string{"exec", nodeName}, cmd...)
	stdOut, err := exec.Command(containerengine.Get().String(), cmdArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run %q: %s (%s)", strings.Join(cmd, " "), err, stdOut)
	}
	return string(stdOut), nil
}

func (k *kind) ExecExternalContainerCommand(container api.ExternalContainer, cmd []string) (string, error) {
	exists, err := doesContainerNameExist(container.Name)
	if err != nil {
		return "", fmt.Errorf("failed to check if container %q exists: %w", container.Name, err)
	}
	if !exists {
		return "", fmt.Errorf("cannot exec into container %q because it doesn't exist: %w", container.Name, api.NotFound)
	}
	cmdArgs := append([]string{"exec", container.Name}, cmd...)
	out, err := exec.Command(containerengine.Get().String(), cmdArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to exec container command (%s): err: %v, stdout: %q", strings.Join(cmdArgs, " "), err, out)
	}
	return string(out), nil
}

func (k *kind) GetExternalContainerLogs(container api.ExternalContainer) (string, error) {
	exists, err := doesContainerNameExist(container.Name)
	if err != nil {
		return "", fmt.Errorf("failed to check if container %q exists: %w", container.Name, err)
	}
	if !exists {
		return "", fmt.Errorf("container %q doesn't exist, therefore no logs can be retrieved: %w", container.Name, api.NotFound)
	}
	stdOut, err := exec.Command(containerengine.Get().String(), "logs", container.Name).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get logs of external container (%s): %v (%s)", container, err, stdOut)
	}
	return string(stdOut), nil
}

func (k *kind) GetExternalContainerPort() uint16 {
	return k.externalContainerPort.Allocate()
}

func (k *kind) GetK8HostPort() uint16 {
	return k.hostPort.Allocate()
}

func (k *kind) GetDefaultTimeoutContext() *framework.TimeoutContext {
	return framework.NewTimeoutContext()
}

// getContainerState returns the state of a container by name
// Returns empty string if container doesn't exist
func getContainerState(containerName string) (string, error) {
	stdOut, err := exec.Command(containerengine.Get().String(), "ps", "-a", "-f", fmt.Sprintf("name=^%s$", containerName), "--format", "{{.State}}").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to check container state for %s: %s (%s)", containerName, err, stdOut)
	}

	state := strings.TrimSpace(string(stdOut))
	return state, nil
}

func (k *kind) ShutdownNode(nodeName string) error {
	state, err := getContainerState(nodeName)
	if err != nil {
		return err
	}

	if state == "" {
		return fmt.Errorf("cannot shutdown node %q because it doesn't exist: %w", nodeName, api.NotFound)
	}

	// If container is already stopped/exited, consider it success
	if state == "exited" || state == "stopped" {
		framework.Logf("Node %s is already stopped (state: %s)", nodeName, state)
		return nil
	}

	framework.Logf("Shutting down node %s (current state: %s)", nodeName, state)
	stdOut, err := exec.Command(containerengine.Get().String(), "stop", nodeName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to shutdown node %s: %s (%s)", nodeName, err, stdOut)
	}
	framework.Logf("Successfully shut down node %s", nodeName)
	return nil
}

func (k *kind) StartNode(nodeName string) error {
	state, err := getContainerState(nodeName)
	if err != nil {
		return err
	}

	if state == "" {
		return fmt.Errorf("cannot start node %q because it doesn't exist: %w", nodeName, api.NotFound)
	}

	// If container is already running, consider it success
	if state == "running" || state == "up" {
		framework.Logf("Node %s is already running (state: %s)", nodeName, state)
		return nil
	}

	framework.Logf("Starting node %s (current state: %s)", nodeName, state)
	stdOut, err := exec.Command(containerengine.Get().String(), "start", nodeName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to start node %s: %s (%s)", nodeName, err, stdOut)
	}
	framework.Logf("Successfully started node %s", nodeName)
	return nil
}

func (k *kind) NewTestContext() api.Context {
	ck := &contextKind{Mutex: sync.Mutex{}}
	ginkgo.DeferCleanup(ck.CleanUp)
	return ck
}

type contextKind struct {
	sync.Mutex
	cleanUpNetworkAttachments api.Attachments
	cleanUpNetworks           api.Networks
	cleanUpContainers         []api.ExternalContainer
	cleanUpFns                []func() error
}

func (c *contextKind) CreateExternalContainer(container api.ExternalContainer) (api.ExternalContainer, error) {
	c.Lock()
	defer c.Unlock()
	return c.createExternalContainer(container)
}

func (c *contextKind) createExternalContainer(container api.ExternalContainer) (api.ExternalContainer, error) {
	if valid, err := container.IsValidPreCreateContainer(); !valid {
		return container, err
	}
	exists, err := doesContainerNameExist(container.Name)
	if err != nil {
		return container, fmt.Errorf("failed to check if container %s exists: %w", container.Name, err)
	}
	if exists {
		return container, fmt.Errorf("container %s already exists", container.Name)
	}
	cmd := []string{"run", "-itd", "--privileged", "--name", container.Name, "--network", container.Network.Name(), "--hostname", container.Name}
	if container.Entrypoint != "" {
		cmd = append(cmd, "--entrypoint", container.Entrypoint)
	}
	cmd = append(cmd, container.RuntimeArgs...)
	cmd = append(cmd, container.Image)
	if len(container.CmdArgs) > 0 {
		cmd = append(cmd, container.CmdArgs...)
	} else {
		if images.AgnHost() == container.Image {
			cmd = append(cmd, "pause")
		}
	}
	fmt.Printf("creating container with command: %q\n", strings.Join(cmd, " "))
	stdOut, err := exec.Command(containerengine.Get().String(), cmd...).CombinedOutput()
	if err != nil {
		return container, fmt.Errorf("failed to create container %s: %s (%s)", container, err, stdOut)
	}
	// fetch IPs for the attached container network. Host networked containers do not expose IP information.
	if !isHostNetworked(container.Network.Name()) {
		err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 360*time.Second, true, func(ctx context.Context) (done bool, err error) {
			ni, err := getNetworkInterface(container.Name, container.Network.Name())
			if err != nil {
				framework.Logf("attempt to get container %s network interface attached to network %s failed: %v, retrying...", container.Name, container.Network.Name(), err)
				return false, nil
			}
			if ni.GetIPv4() == "" && ni.GetIPv6() == "" {
				return false, nil
			}
			container.IPv4 = ni.GetIPv4()
			container.IPv6 = ni.GetIPv6()
			return true, nil
		})
		if err != nil {
			return container, fmt.Errorf("failed to get network interface information: %w", err)
		}
	}

	if valid, err := container.IsValidPostCreate(); !valid {
		return container, err
	}
	c.cleanUpContainers = append(c.cleanUpContainers, container)
	return container, nil
}

func (c *contextKind) DeleteExternalContainer(container api.ExternalContainer) error {
	c.Lock()
	defer c.Unlock()
	return c.deleteExternalContainer(container)
}

func (c *contextKind) deleteExternalContainer(container api.ExternalContainer) error {
	// check if it is present before deleting
	exists, err := doesContainerNameExist(container.Name)
	if err != nil {
		return fmt.Errorf("failed to check if container %s exists: %w", container.Name, err)
	}
	if !exists {
		return nil
	}
	stdOut, err := exec.Command(containerengine.Get().String(), "rm", "-f", container.Name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to delete external container (%s): %v (%s)", container, err, stdOut)
	}
	err = wait.ExponentialBackoff(wait.Backoff{Duration: 1 * time.Second, Factor: 5, Steps: 5}, wait.ConditionFunc(func() (done bool, err error) {
		stdOut, err = exec.Command(containerengine.Get().String(), "ps", "-f", fmt.Sprintf("Name=^%s$", container.Name), "-q").CombinedOutput()
		if err != nil {
			return false, fmt.Errorf("failed to check if external container (%s) is deleted: %v (%s)", container, err, stdOut)
		}
		if string(stdOut) != "" {
			return false, nil
		}
		return true, nil
	}))
	if err != nil {
		return fmt.Errorf("failed to delete external container (%s): %v", container, err)
	}
	return nil
}

func (c *contextKind) CreateNetwork(name string, subnets ...string) (api.Network, error) {
	c.Lock()
	defer c.Unlock()
	return c.createNetwork(name, subnets...)
}

func (c *contextKind) createNetwork(name string, subnets ...string) (api.Network, error) {
	network := containerEngineNetwork{name, nil}
	exists, err := doesNetworkExist(name)
	if err != nil {
		return network, fmt.Errorf("failed to check if network %s exists: %w", name, err)
	}
	if exists {
		attachedContainers, err := getContainerAttachedToNetwork(name)
		if err != nil {
			framework.Logf("failed to get containers attached to network %s: %v", name, err)
		}
		if len(attachedContainers) > 0 {
			return network, fmt.Errorf("network %s already exists with containers attached: '%v'", name, attachedContainers)
		}
		return network, fmt.Errorf("network %q already exists", name)
	}
	cmdArgs := []string{"network", "create", "--internal", "--driver", "bridge", name}
	var v6 bool
	// detect if IPv6 flag is required
	for _, subnet := range subnets {
		cmdArgs = append(cmdArgs, "--subnet", subnet)
		if utilnet.IsIPv6CIDRString(subnet) {
			v6 = true
		}
	}
	if v6 {
		cmdArgs = append(cmdArgs, "--ipv6")
	}
	stdOut, err := exec.Command(containerengine.Get().String(), cmdArgs...).CombinedOutput()
	if err != nil {
		return network, fmt.Errorf("failed to create Network with command %q: %s (%s)", strings.Join(cmdArgs, " "), err, stdOut)
	}
	c.cleanUpNetworks.InsertNoDupe(network)
	return getNetwork(name)
}

func (c *contextKind) AttachNetwork(network api.Network, container string) (api.NetworkInterface, error) {
	c.Lock()
	defer c.Unlock()
	return c.attachNetwork(network, container)
}

func (c *contextKind) attachNetwork(network api.Network, container string) (api.NetworkInterface, error) {
	exists, err := doesNetworkExist(network.Name())
	if err != nil {
		return api.NetworkInterface{}, fmt.Errorf("failed to check if network %s exists: %w", network.Name(), err)
	}
	if !exists {
		return api.NetworkInterface{}, fmt.Errorf("network %s doesn't exist", network.Name())
	}
	if isNetworkAttachedToContainer(network.Name(), container) {
		return api.NetworkInterface{}, fmt.Errorf("network %s is already attached to container %s", network.Name(), container)
	}
	// return if the network is connected to the container
	stdOut, err := exec.Command(containerengine.Get().String(), "network", "connect", network.Name(), container).CombinedOutput()
	if err != nil {
		return api.NetworkInterface{}, fmt.Errorf("failed to attach network to container %s: %s (%s)", container, err, stdOut)
	}
	c.cleanUpNetworkAttachments.InsertNoDupe(api.Attachment{Network: network, Instance: container})
	return getNetworkInterface(container, network.Name())
}

func (c *contextKind) DetachNetwork(network api.Network, container string) error {
	c.Lock()
	defer c.Unlock()
	return c.detachNetwork(network, container)
}

func (c *contextKind) detachNetwork(network api.Network, container string) error {
	exists, err := doesNetworkExist(network.Name())
	if err != nil {
		return fmt.Errorf("failed to check if network %s exists: %w", network.Name(), err)
	}
	if !exists {
		return nil
	}
	if !isNetworkAttachedToContainer(network.Name(), container) {
		return nil
	}
	stdOut, err := exec.Command(containerengine.Get().String(), "network", "disconnect", network.Name(), container).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to detach network %s from node %s: %s (%s)", network, container, err, stdOut)
	}
	return nil
}

func (c *contextKind) DeleteNetwork(network api.Network) error {
	c.Lock()
	defer c.Unlock()
	return c.deleteNetwork(network)
}

func (c *contextKind) deleteNetwork(network api.Network) error {
	return wait.PollImmediate(1*time.Second, 10*time.Second, func() (done bool, err error) {
		exists, err := doesNetworkExist(network.Name())
		if err != nil {
			framework.Logf("failed to check if network %s exists: %v", network.Name(), err)
			return false, nil
		}
		if !exists {
			return true, nil
		}
		// ensure all containers are disconnected from the network and if any are found, disconnect it.
		delimiter := " "
		stdOutBytes, err := exec.Command(containerengine.Get().String(),
			"network", "inspect", "-f", fmt.Sprintf("'{{range .Containers}}{{.Name}}%s{{end}}'", delimiter), network.Name()).CombinedOutput()
		if err != nil {
			framework.Logf("failed to list attached containers for network %s:, err: %s, stdout: (%s)", network, err, stdOutBytes)
			return false, nil
		}
		allContainers := strings.TrimSuffix(string(stdOutBytes), "\n")
		if allContainers != "" {
			for _, containerName := range strings.Split(allContainers, delimiter) {
				containerName = strings.TrimLeft(containerName, "'")
				containerName = strings.TrimRight(containerName, "'")
				if containerName == "" {
					continue
				}
				framework.Logf("deleting network encountered a stale container %q and it must be removed before removing the network", containerName)
				framework.Logf("Warning: Fix tests for container %s.. deleting container", containerName)
				if err = c.detachNetwork(network, containerName); err != nil {
					framework.Logf("while trying to delete network %q, attempted to detach container %q that is "+
						"still attached to network", network.Name(), containerName)
					return false, nil
				}
			}
		}
		stdOut, err := exec.Command(containerengine.Get().String(), "network", "rm", network.Name()).CombinedOutput()
		if err != nil {
			framework.Logf("failed to delete network %s: %s (%s)", network.Name(), err, stdOut)
			return false, nil
		}
		return true, nil
	})
}

func (c *contextKind) GetAttachedNetworks() (api.Networks, error) {
	c.Lock()
	defer c.Unlock()
	return c.getAttachedNetworks()
}

func (c *contextKind) getAttachedNetworks() (api.Networks, error) {
	primaryNetwork, err := getNetwork("kind")
	if err != nil {
		return api.Networks{}, fmt.Errorf("failed to get primary network: %v", err)
	}
	attachedNetworks := api.Networks{List: []api.Network{primaryNetwork}}
	for _, attachment := range c.cleanUpNetworkAttachments.List {
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
			underlayInterface, err := getNetworkInterface(ovsPod.Spec.NodeName, underlay.PhysicalNetworkName)
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

func (c *contextKind) AddCleanUpFn(cleanUpFn func() error) {
	c.Lock()
	defer c.Unlock()
	c.addCleanUpFn(cleanUpFn)
}

func (c *contextKind) addCleanUpFn(cleanUpFn func() error) {
	c.cleanUpFns = append(c.cleanUpFns, cleanUpFn)
}

func (c *contextKind) CleanUp() error {
	c.Lock()
	defer c.Unlock()
	err := c.cleanUp()
	if err != nil {
		framework.Logf("Cleanup failed: %v", err)
	}
	return err
}

// CleanUp must be syncronised by caller
func (c *contextKind) cleanUp() error {
	var errs []error
	// generic cleanup activities
	for i := len(c.cleanUpFns) - 1; i >= 0; i-- {
		if err := c.cleanUpFns[i](); err != nil {
			errs = append(errs, err)
		}
	}
	c.cleanUpFns = nil
	// detach network(s) from nodes
	for _, na := range c.cleanUpNetworkAttachments.List {
		if err := c.detachNetwork(na.Network, na.Instance); err != nil && !errors.Is(err, api.NotFound) {
			errs = append(errs, err)
		}
	}
	// remove containers
	for _, container := range c.cleanUpContainers {
		if err := c.deleteExternalContainer(container); err != nil && !errors.Is(err, api.NotFound) {
			errs = append(errs, err)
		}
	}
	c.cleanUpContainers = nil
	// delete secondary networks
	for _, network := range c.cleanUpNetworks.List {
		if err := c.deleteNetwork(network); err != nil && !errors.Is(err, api.NotFound) {
			errs = append(errs, err)
		}
	}
	c.cleanUpNetworks.List = nil
	return condenseErrors(errs)
}

const (
	nameFormat                     = "{{.Name}}"
	inspectNetworkIPv4GWKeyStr     = "{{ with index .NetworkSettings.Networks %q }}{{ .Gateway }}{{ end }}"
	inspectNetworkIPv4AddrKeyStr   = "{{ with index .NetworkSettings.Networks %q }}{{ .IPAddress }}{{ end }}"
	inspectNetworkIPv4PrefixKeyStr = "{{ with index .NetworkSettings.Networks %q }}{{ .IPPrefixLen }}{{ end }}"
	inspectNetworkIPv6GWKeyStr     = "{{ with index .NetworkSettings.Networks %q }}{{ .IPv6Gateway }}{{ end }}"
	inspectNetworkIPv6AddrKeyStr   = "{{ with index .NetworkSettings.Networks %q }}{{ .GlobalIPv6Address }}{{ end }}"
	inspectNetworkIPv6PrefixKeyStr = "{{ with index .NetworkSettings.Networks %q }}{{ .GlobalIPv6PrefixLen }}{{ end }}"
	inspectNetworkMACKeyStr        = "{{ with index .NetworkSettings.Networks %q }}{{ .MacAddress }}{{ end }}"
	inspectNetworkContainersKeyStr = "{{ range $key, $value := .Containers }}{{ printf \"%s\\n\" $value.Name}}{{ end }}'"
	emptyValue                     = "<no value>"
)

func isNetworkAttachedToContainer(networkName, containerName string) bool {
	// error is returned if failed to find network attached to instance or no IPv4/IPv6 Ips.
	_, err := getNetworkInterface(containerName, networkName)
	if err != nil {
		return false
	}
	return true
}

func doesContainerNameExist(name string) (bool, error) {
	state, err := getContainerState(name)
	if err != nil {
		return false, err
	}
	// Empty state means container doesn't exist
	return state != "", nil
}

func doesNetworkExist(networkName string) (bool, error) {
	dataBytes, err := exec.Command(containerengine.Get().String(), "network", "ls", "--format", nameFormat).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("failed to list networks: %w", err)
	}
	for _, existingNetworkName := range strings.Split(strings.Trim(string(dataBytes), "\n"), "\n") {
		if existingNetworkName == networkName {
			return true, nil
		}
	}
	return false, nil
}

func getNetwork(networkName string) (containerEngineNetwork, error) {
	n := containerEngineNetwork{name: networkName}
	exists, err := doesNetworkExist(networkName)
	if err != nil {
		return n, fmt.Errorf("failed to check if network %s exists: %w", networkName, err)
	}
	if !exists {
		return n, api.NotFound
	}
	configs := make([]containerEngineNetworkConfig, 0, 1)

	ce := containerengine.Get()
	netConfFmt := ce.NetworkCIDRsFmt()
	dataBytes, err := exec.Command(ce.String(), "network", "inspect", "-f", netConfFmt, networkName).CombinedOutput()
	if err != nil {
		return n, fmt.Errorf("failed to extract network %q data: %v", networkName, err)
	}
	dataBytes = []byte(strings.Trim(string(dataBytes), "\n"))
	if err = json.Unmarshal(dataBytes, &configs); err != nil {
		return n, fmt.Errorf("failed to unmarshall network %q configuration using network inspect -f %q: %v", networkName, netConfFmt, err)
	}
	if len(configs) == 0 {
		return n, fmt.Errorf("failed to find any IPAM configuration for network %s", networkName)
	}
	// validate configs
	for _, config := range configs {
		if config.Subnet == "" {
			return n, fmt.Errorf("network %s contains invalid subnet config", networkName)
		}
	}
	n.Configs = configs
	return n, nil
}

func getContainerAttachedToNetwork(networkName string) ([]string, error) {
	dataBytes, err := exec.Command(containerengine.Get().String(), "network", "inspect", "-f", inspectNetworkContainersKeyStr, networkName).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch containers attached to network %q, err: %v", networkName, err)
	}
	var containers []string

	for _, container := range strings.Split(string(dataBytes), "\n") {
		container = strings.Trim(container, "'")
		if container != "" {
			containers = append(containers, container)
		}
	}
	return containers, nil
}

func getNetworkInterface(containerName, networkName string) (api.NetworkInterface, error) {
	var ni = api.NetworkInterface{}
	exists, err := doesNetworkExist(networkName)
	if err != nil {
		return ni, fmt.Errorf("failed to check if network %q exists: %w", networkName, err)
	}
	if !exists {
		return ni, fmt.Errorf("failed to find network %q: %w", networkName, api.NotFound)
	}
	exists, err = doesContainerNameExist(containerName)
	if err != nil {
		return ni, fmt.Errorf("failed to check if container %q exists: %w", containerName, err)
	}
	if !exists {
		return ni, fmt.Errorf("failed to find container %q: %w", containerName, api.NotFound)
	}
	getContainerNetwork := func(inspectTemplate string) (string, error) {
		value, err := exec.Command(containerengine.Get().String(), "inspect", "-f",
			fmt.Sprintf("'"+inspectTemplate+"'", networkName), containerName).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to extract %s network data for container %s using inspect template %s: %v",
				networkName, containerName, inspectTemplate, err)
		}
		valueStr := strings.Trim(string(value), "\n")
		valueStr = strings.Trim(valueStr, "'")
		if valueStr == emptyValue {
			return "", nil
		}
		return valueStr, nil
	}

	getIPFamilyFlagForIPRoute2 := func(ipStr string) (string, error) {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return "", fmt.Errorf("invalid IP address: %s", ipStr)
		}
		if utilnet.IsIPv6(ip) {
			return "-6", nil
		}
		return "-4", nil
	}

	getInterfaceNameUsingIP := func(ip string) (string, error) {
		ipFlag, err := getIPFamilyFlagForIPRoute2(ip)
		if err != nil {
			return "", fmt.Errorf("failed to get IP family flag for %s: %w", ip, err)
		}
		allInfAddrBytes, err := exec.Command(containerengine.Get().String(), "exec", "-i", containerName, "ip", "-br", ipFlag, "a", "sh").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to find interface with IP %s on container %s with command 'ip -br a sh': err %v, out: %s", ip, containerName,
				err, allInfAddrBytes)
		}
		var ipLine string
		for _, line := range strings.Split(string(allInfAddrBytes), "\n") {
			if strings.Contains(line, ip) {
				ipLine = line
				break
			}
		}
		if ipLine == "" {
			return "", fmt.Errorf("failed to find IP %q within 'ip a' command on container %q:\n\n%q", ip, containerName, string(allInfAddrBytes))
		}
		ipLineSplit := strings.Split(ipLine, " ")
		if len(ipLine) == 0 {
			return "", fmt.Errorf("failed to find interface name from 'ip a' output line %q", ipLine)
		}
		infNames := ipLineSplit[0]
		splitChar := " "
		if strings.Contains(infNames, "@") {
			splitChar = "@"
		}
		infNamesSplit := strings.Split(infNames, splitChar)
		if len(infNamesSplit) == 0 {
			return "", fmt.Errorf("failed to extract inf name + veth name from %q splitting by %q", infNames, splitChar)
		}
		infName := infNamesSplit[0]
		// validate its an interface name on the Node with iproute2
		out, err := exec.Command(containerengine.Get().String(), "exec", "-i", containerName, "ip", "link", "show", infName).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to validate that interface name %q with IP %s exists in container %s: err %v, out: %s",
				infName, ip, containerName, err, out)
		}
		return infName, nil // second value is veth in 'host' netns
	}

	ni.IPv4Gateway, err = getContainerNetwork(inspectNetworkIPv4GWKeyStr)
	if err != nil {
		// may not be available
		framework.Logf("failed to get network gateway IPv4 %s: %v", err)
	}
	ni.IPv4, err = getContainerNetwork(inspectNetworkIPv4AddrKeyStr)
	if err != nil {
		return ni, err
	}
	if ni.IPv4 != "" {
		ni.InfName, err = getInterfaceNameUsingIP(ni.IPv4)
		if err != nil {
			framework.Logf("failed to get network interface name using IPv4 address %s: %v", ni.IPv4, err)
		}
	}
	ni.IPv6Gateway, err = getContainerNetwork(inspectNetworkIPv6GWKeyStr)
	if err != nil {
		framework.Logf("failed to get network gateway IPv6 %s: %v", err)
	}
	ni.IPv4Prefix, err = getContainerNetwork(inspectNetworkIPv4PrefixKeyStr)
	if err != nil {
		return ni, err
	}
	ni.IPv6, err = getContainerNetwork(inspectNetworkIPv6AddrKeyStr)
	if err != nil {
		return ni, err
	}
	if ni.IPv6 != "" {
		ni.InfName, err = getInterfaceNameUsingIP(ni.IPv6)
		if err != nil {
			framework.Logf("failed to get network interface name using IPv4 address %s: %v", ni.IPv6, err)
		}
	}
	ni.IPv6Prefix, err = getContainerNetwork(inspectNetworkIPv6PrefixKeyStr)
	if err != nil {
		return ni, err
	}
	ni.MAC, err = getContainerNetwork(inspectNetworkMACKeyStr)
	if err != nil {
		return ni, err
	}
	// fail if no IPs were found
	if ni.IPv4 == "" && ni.IPv6 == "" {
		return ni, fmt.Errorf("failed to get an IPv4 and/or IPv6 address for interface attached to container %q"+
			" and attached to network %q", containerName, networkName)
	}
	return ni, nil
}

func isHostNetworked(networkName string) bool {
	return networkName == "host"
}

func condenseErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	err := errs[0]
	for _, e := range errs[1:] {
		err = errors.Join(err, e)
	}
	return err
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
