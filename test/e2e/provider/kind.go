package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/containerruntime"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	utilnet "k8s.io/utils/net"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type kind struct {
	externalContainerPort *portAllocator
	hostPort              *portAllocator
}

func newKinDProvider() Provider {
	return &kind{externalContainerPort: newPortAllocator(12000, 65535), hostPort: newPortAllocator(1024, 65535)}
}

func (k *kind) Name() string {
	return "kind"
}

func (k *kind) PrimaryNetwork() (Network, error) {
	return getNetwork("kind")
}

func (k *kind) PrimaryInterfaceName() string {
	return "eth0"
}

func (k *kind) GetNetwork(name string) (Network, error) {
	return getNetwork(name)
}

func (k *kind) GetExternalContainerNetworkInterface(container ExternalContainer, network Network) (NetworkInterface, error) {
	return getNetworkInterface(container.Name, network.Name)
}

func (k *kind) GetK8NodeNetworkInterface(instance string, network Network) (NetworkInterface, error) {
	return getNetworkInterface(instance, network.Name)
}

func (k *kind) ExecK8NodeCommand(nodeName string, cmd []string) (string, error) {
	if !doesContainerExist(nodeName) {
		return "", fmt.Errorf("cannot exist into container %q because it doesn't exist", nodeName)
	}
	if len(cmd) == 0 {
		panic("ExecK8NodeCommand(): insufficient command arguments")
	}
	cmdArgs := append([]string{"exec", nodeName}, cmd...)
	stdOut, err := exec.Command(containerruntime.Get().String(), cmdArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run %q: %s (%s)", strings.Join(cmd, " "), err, stdOut)
	}
	return string(stdOut), nil
}

func (k *kind) ExecExternalContainerCommand(container ExternalContainer, cmd []string) (string, error) {
	if !doesContainerExist(container.Name) {
		return "", fmt.Errorf("cannot exist into container %q because it doesn't exist", container.Name)
	}
	cmdArgs := append([]string{"exec", container.Name}, cmd...)
	out, err := exec.Command(containerruntime.Get().String(), cmdArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to exec container command (%s): err: %v, stdout: %q", strings.Join(cmdArgs, " "), err, out)
	}
	return string(out), nil
}

func (k *kind) GetExternalContainerLogs(container ExternalContainer) (string, error) {
	if !doesContainerExist(container.Name) {
		return "", fmt.Errorf("container %q doesn't exist, therefore no logs can be retreived", container.Name)
	}
	stdOut, err := exec.Command(containerruntime.Get().String(), "logs", container.Name).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get logs of external container (%s): %v (%s)", container, err, stdOut)
	}
	return string(stdOut), nil
}

func (k *kind) GetExternalContainerPort() uint16 {
	return k.externalContainerPort.allocate()
}

func (k *kind) GetK8HostPort() uint16 {
	return k.hostPort.allocate()
}

func (k *kind) NewTestContext() Context {
	ck := &contextKind{Mutex: sync.Mutex{}}
	ginkgo.DeferCleanup(ck.CleanUp)
	return ck
}

type contextKind struct {
	sync.Mutex
	cleanUpNetworkAttachments NetworkAttachments
	cleanUpNetworks           Networks
	cleanUpContainers         []ExternalContainer
	cleanUpFns                []func() error
}

func (c *contextKind) CreateExternalContainer(container ExternalContainer) (ExternalContainer, error) {
	c.Lock()
	defer c.Unlock()
	return c.createExternalContainer(container)
}

func (c *contextKind) createExternalContainer(container ExternalContainer) (ExternalContainer, error) {
	if valid, err := container.isValidPreCreateContainer(); !valid {
		return container, err
	}
	if doesContainerExist(container.Name) {
		return container, fmt.Errorf("container %s already exists", container.Name)
	}
	cmd := []string{"run", "-itd", "--privileged", "--name", container.Name, "--network", container.Network.Name, "--hostname", container.Name}
	cmd = append(cmd, container.Image)
	if len(container.Args) > 0 {
		cmd = append(cmd, container.Args...)
	} else {
		if images.AgnHost() == container.Image {
			cmd = append(cmd, "pause")
		}
	}
	fmt.Printf("creating container with command: %q\n", strings.Join(cmd, " "))
	stdOut, err := exec.Command(containerruntime.Get().String(), cmd...).CombinedOutput()
	if err != nil {
		return container, fmt.Errorf("failed to create container %s: %s (%s)", container, err, stdOut)
	}
	// fetch IPs for the attached network
	err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 360*time.Second, true, func(ctx context.Context) (done bool, err error) {
		ni, err := getNetworkInterface(container.Name, container.Network.Name)
		if err != nil {
			framework.Logf("attempt to get container %s network interface attached to network %s failed: %v, retrying...", container.Name, container.Network.Name, err)
			return false, nil
		}
		if ni.IPv4 == "" && ni.IPv6 == "" {
			return false, nil
		}
		container.ipv4, container.ipv6 = ni.IPv4, ni.IPv6
		return true, nil
	})

	if valid, err := container.isValidPostCreate(); !valid {
		return container, err
	}
	c.cleanUpContainers = append(c.cleanUpContainers, container)
	return container, nil
}

func (c *contextKind) DeleteExternalContainer(container ExternalContainer) error {
	c.Lock()
	defer c.Unlock()
	return c.deleteExternalContainer(container)
}

func (c *contextKind) deleteExternalContainer(container ExternalContainer) error {
	// check if it is present before deleting
	if !doesContainerExist(container.Name) {
		return nil
	}
	stdOut, err := exec.Command(containerruntime.Get().String(), "rm", "-f", container.Name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to delete external container (%s): %v (%s)", container, err, stdOut)
	}
	err = wait.ExponentialBackoff(wait.Backoff{Duration: 1 * time.Second, Factor: 5, Steps: 5}, wait.ConditionFunc(func() (done bool, err error) {
		stdOut, err = exec.Command(containerruntime.Get().String(), "ps", "-f", fmt.Sprintf("Name=^%s$", container.Name), "-q").CombinedOutput()
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

func (c *contextKind) CreateNetwork(name string, subnets ...string) (Network, error) {
	c.Lock()
	defer c.Unlock()
	return c.createNetwork(name, subnets...)
}

func (c *contextKind) createNetwork(name string, subnets ...string) (Network, error) {
	network := Network{name, nil}
	if doesNetworkExist(name) {
		attachedContainers, err := getContainerAttachedToNetwork(name)
		if err != nil {
			framework.Logf("failed to get containers attached to network %s: %v", name, err)
		}
		return network, fmt.Errorf("network %s already exits with containers attached: '%v'", name, attachedContainers)
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
	stdOut, err := exec.Command(containerruntime.Get().String(), cmdArgs...).CombinedOutput()
	if err != nil {
		return network, fmt.Errorf("failed to create Network with command %q: %s (%s)", strings.Join(cmdArgs, " "), err, stdOut)
	}
	c.cleanUpNetworks.InsertNoDupe(network)
	return getNetwork(name)
}

func (c *contextKind) AttachNetwork(network Network, instance string) (NetworkInterface, error) {
	c.Lock()
	defer c.Unlock()
	return c.attachNetwork(network, instance)
}

func (c *contextKind) attachNetwork(network Network, instance string) (NetworkInterface, error) {
	if !doesNetworkExist(network.Name) {
		return NetworkInterface{}, fmt.Errorf("network %s doesn't exist", network.Name)
	}
	if isNetworkAttachedToInstance(network.Name, instance) {
		return NetworkInterface{}, fmt.Errorf("network %s is already attached to instance %s", network.Name, instance)
	}
	// return if the network is connected to the container
	stdOut, err := exec.Command(containerruntime.Get().String(), "network", "connect", network.Name, instance).CombinedOutput()
	if err != nil {
		return NetworkInterface{}, fmt.Errorf("failed to attach network to instance %s: %s (%s)", instance, err, stdOut)
	}
	c.cleanUpNetworkAttachments.insertNoDupe(networkAttachment{network: network, node: instance})
	return getNetworkInterface(instance, network.Name)
}

func (c *contextKind) DetachNetwork(network Network, instance string) error {
	c.Lock()
	defer c.Unlock()
	return c.detachNetwork(network, instance)
}

func (c *contextKind) detachNetwork(network Network, instance string) error {
	if !doesNetworkExist(network.Name) {
		return fmt.Errorf("detaching network %s failed because it already detached from instance %s", network.Name, instance)
	}
	if !isNetworkAttachedToInstance(network.Name, instance) {
		return fmt.Errorf("network %s is already detached from instance %s", network.Name, instance)
	}
	stdOut, err := exec.Command(containerruntime.Get().String(), "network", "disconnect", network.Name, instance).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to detach network %s from node %s: %s (%s)", network, instance, err, stdOut)
	}
	return nil
}

func (c *contextKind) DeleteNetwork(network Network) error {
	c.Lock()
	defer c.Unlock()
	return c.deleteNetwork(network)
}

func (c *contextKind) deleteNetwork(network Network) error {
	return wait.PollImmediate(1*time.Second, 10*time.Second, func() (done bool, err error) {
		if !doesNetworkExist(network.Name) {
			return true, nil
		}
		// ensure all containers are disconnected from the network and if any are found, disconnect it.
		delimiter := " "
		stdOutBytes, err := exec.Command(containerruntime.Get().String(),
			"network", "inspect", "-f", fmt.Sprintf("'{{range .Containers}}{{.Name}}%s{{end}}'", delimiter), network.Name).CombinedOutput()
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
						"still attached to network", network.Name, containerName)
					return false, nil
				}
			}
		}
		stdOut, err := exec.Command(containerruntime.Get().String(), "network", "rm", network.Name).CombinedOutput()
		if err != nil {
			framework.Logf("failed to delete network %s: %s (%s)", network.Name, err, stdOut)
			return false, nil
		}
		return true, nil
	})
}

func (c *contextKind) GetAttachedNetworks() (Networks, error) {
	c.Lock()
	defer c.Unlock()
	return c.getAttachedNetworks()
}

func (c *contextKind) getAttachedNetworks() (Networks, error) {
	primaryNetwork, err := provider.PrimaryNetwork()
	networks := Networks{List: []Network{primaryNetwork}}
	if err != nil {
		return networks, fmt.Errorf("failed to get primary network: %v", err)
	}
	for _, attachment := range c.cleanUpNetworkAttachments.List {
		networks.List = append(networks.List, attachment.network)
	}
	return networks, nil
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
		framework.Logf("CleanUp: exec cleanup func %d of %d", i+1, len(c.cleanUpFns))
		if err := c.cleanUpFns[i](); err != nil {
			errs = append(errs, err)
		}
	}
	c.cleanUpFns = nil
	// detach network(s) from nodes
	for _, na := range c.cleanUpNetworkAttachments.List {
		framework.Logf("CleanUp: detaching network %s from node %s", na.network.Name, na.node)
		if err := c.detachNetwork(na.network, na.node); err != nil {
			errs = append(errs, err)
		}
	}
	// remove containers
	for _, container := range c.cleanUpContainers {
		framework.Logf("CleanUp: deleting container %s", container.Name)
		if err := c.deleteExternalContainer(container); err != nil {
			errs = append(errs, err)
		}
	}
	c.cleanUpContainers = nil
	// delete secondary networks
	for _, network := range c.cleanUpNetworks.List {
		framework.Logf("CleanUp: deleting network %s", network.Name)
		if err := c.deleteNetwork(network); err != nil {
			errs = append(errs, err)
		}
	}
	c.cleanUpNetworks.List = nil
	return condenseErrors(errs)
}

const (
	inspectNetworkIPAMJSON         = "{{json .IPAM.Config }}"
	inspectNetworkIPv4GWKeyStr     = "{{ .NetworkSettings.Networks.%s.Gateway }}"
	inspectNetworkIPv4AddrKeyStr   = "{{ .NetworkSettings.Networks.%s.IPAddress }}"
	inspectNetworkIPv4PrefixKeyStr = "{{ .NetworkSettings.Networks.%s.IPPrefixLen }}"
	inspectNetworkIPv6GWKeyStr     = "{{ .NetworkSettings.Networks.%s.IPv6Gateway }}"
	inspectNetworkIPv6AddrKeyStr   = "{{ .NetworkSettings.Networks.%s.GlobalIPv6Address }}"
	inspectNetworkIPv6PrefixKeyStr = "{{ .NetworkSettings.Networks.%s.GlobalIPv6PrefixLen }}"
	inspectNetworkMACKeyStr        = "{{ .NetworkSettings.Networks.%s.MacAddress }}"
	inspectNetworkContainersKeyStr = "{{ range $key, $value := .Containers }}{{ printf \"%s\\n\" $value.Name}}{{ end }}'"
	emptyValue                     = "<no value>"
)

func isNetworkAttachedToInstance(networkName, containerName string) bool {
	// error is returned if failed to find network attached to instance or no IPv4/IPv6 Ips.
	_, err := getNetworkInterface(containerName, networkName)
	if err != nil {
		return false
	}
	return true
}

func doesContainerExist(name string) bool {
	// check if it is present before retrieving logs
	stdOut, err := exec.Command(containerruntime.Get().String(), "ps", "-f", fmt.Sprintf("Name=^%s$", name), "-q").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("failed to check if external container (%s) exists: %v (%s)", name, err, stdOut))
	}
	if string(stdOut) == "" {
		return false
	}
	return true
}

func doesNetworkExist(networkName string) bool {
	n, _ := getNetwork(networkName)
	return len(n.Configs) > 0
}

func getNetwork(networkName string) (Network, error) {
	n := Network{Name: networkName}
	configs := make([]NetworkConfig, 0, 1)
	dataBytes, err := exec.Command(containerruntime.Get().String(), "network", "inspect", "-f", inspectNetworkIPAMJSON, networkName).CombinedOutput()
	if err != nil {
		return n, fmt.Errorf("failed to extract network %q data: %v", networkName, err)
	}
	dataBytes = []byte(strings.Trim(string(dataBytes), "\n"))
	if err = json.Unmarshal(dataBytes, &configs); err != nil {
		return n, fmt.Errorf("failed to unmarshall network %q configuration using network inspect -f %q: %v", networkName, inspectNetworkIPAMJSON, err)
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
	dataBytes, err := exec.Command(containerruntime.Get().String(), "network", "inspect", "-f", inspectNetworkContainersKeyStr, networkName).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch containers attached to network %q, err: %v", networkName, err)
	}
	return strings.Split(string(dataBytes), "\n"), nil
}

func getNetworkInterface(containerName, networkName string) (NetworkInterface, error) {
	getContainerNetwork := func(inspectTemplate string) (string, error) {
		value, err := exec.Command(containerruntime.Get().String(), "inspect", "-f",
			fmt.Sprintf(inspectTemplate, networkName), containerName).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to extract %s network data for container %s using inspect template %s: %v",
				networkName, containerName, inspectTemplate, err)
		}
		valueStr := strings.Trim(string(value), "\n")
		if valueStr == emptyValue {
			return "", nil
		}
		return valueStr, nil
	}

	getIPFamilyFlagForIPRoute2 := func(ipStr string) string {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			panic("invalid IP")
		}
		if utilnet.IsIPv6(ip) {
			return "-6"
		}
		return "-4"
	}

	getInterfaceNameUsingIP := func(ip string) (string, error) {
		allInfAddrBytes, err := exec.Command(containerruntime.Get().String(), "exec", "-i", containerName, "ip", "-br", getIPFamilyFlagForIPRoute2(ip), "a", "sh").CombinedOutput()
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
		out, err := exec.Command(containerruntime.Get().String(), "exec", "-i", containerName, "ip", "link", "show", infName).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to validate that interface name %q with IP %s exists in container %s: err %v, out: %s",
				infName, ip, containerName, err, out)
		}
		return infName, nil // second value is veth in 'host' netns
	}

	var err error
	var ni = NetworkInterface{}
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
		return ni, fmt.Errorf("failed to get an IPv4 and/or IPv6 address for interface attached to instance %q"+
			" and attached to network %q", containerName, networkName)
	}
	return ni, nil
}
