// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"errors"
	"fmt"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container/network"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/portalloc"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/testcontext"
	"k8s.io/kubernetes/test/e2e/framework"
)

type Engine struct {
	ops                   *ops.ContainerOps
	externalContainerPort *portalloc.PortAllocator
	testContext           *testcontext.TestContext
}

func NewEngine(runtime string, runner api.Runner) *Engine {
	containerOps := ops.NewContainerOps(runtime, runner)
	return &Engine{ops: containerOps,
		// upper bound stays below the ephemeral range (net.ipv4.ip_local_port_range)
		externalContainerPort: portalloc.New(12000, 32767)}
}

func (p *Engine) ExternalContainerPrimaryInterfaceName() string {
	return "eth0"
}

func (p *Engine) GetExternalContainerNetworkInterface(container api.ExternalContainer, network api.Network) (api.NetworkInterface, error) {
	return p.ops.GetNetworkInterface(container.Name, network.Name())
}

func (p *Engine) ExecExternalContainerCommand(container api.ExternalContainer, cmd []string) (string, error) {
	return p.ops.ExecContainerCommand(container.Name, cmd)
}

func (p *Engine) GetExternalContainerPort() uint16 {
	return p.externalContainerPort.Allocate()
}

func (p *Engine) GetNetwork(name string) (api.Network, error) {
	return p.ops.GetNetwork(name)
}

func (p *Engine) GetNetworkInterface(container string, network string) (api.NetworkInterface, error) {
	return p.ops.GetNetworkInterface(container, network)
}

func (p *Engine) ExecContainerCommand(container string, cmd []string) (string, error) {
	return p.ops.ExecContainerCommand(container, cmd)
}

func (p *Engine) GetExternalContainerLogs(container api.ExternalContainer) (string, error) {
	return p.ops.GetExternalContainerLogs(container)
}

func (p *Engine) ListNetworks() ([]string, error) {
	return p.ops.ListNetworks()
}

func (p *Engine) StopContainer(name string) error {
	return p.ops.StopContainer(name)
}

func (p *Engine) StartContainer(name string) error {
	return p.ops.StartContainer(name)
}

func (p *Engine) GetContainerState(container string) (string, error) {
	return p.ops.GetContainerState(container)
}

func (p *Engine) WithTestContext(context *testcontext.TestContext) *Engine {
	return &Engine{ops: p.ops,
		externalContainerPort: p.externalContainerPort,
		testContext:           context}
}

func (p *Engine) CreateExternalContainer(container api.ExternalContainer) (api.ExternalContainer, error) {
	if p.testContext == nil {
		return container, fmt.Errorf("CreateExternalContainer is invoked for %s without test context",
			container.Name)
	}
	container, err := p.ops.CreateExternalContainer(container)
	if err != nil {
		return container, err
	}
	p.testContext.AddCleanUpFn(func() error {
		framework.Logf("Deleting container %s", container.Name)
		err := p.ops.DeleteExternalContainer(container)
		if err != nil && errors.Is(err, api.NotFound) {
			return nil
		}
		return err
	})
	return container, nil
}

func (p *Engine) DeleteExternalContainer(container api.ExternalContainer) error {
	if p.testContext == nil {
		return fmt.Errorf("DeleteExternalContainer is invoked for %s without test context", container.Name)
	}
	return p.ops.DeleteExternalContainer(container)
}

func (p *Engine) CreateNetwork(name string, subnets ...string) (api.Network, error) {
	if p.testContext == nil {
		return nil, fmt.Errorf("CreateNetwork is invoked for network %s without test context", name)
	}
	network := network.ContainerEngineNetwork{NetName: name, Configs: nil}
	err := p.ops.CreateNetwork(name, subnets...)
	if err != nil {
		return network, err
	}
	p.testContext.AddCleanUpFn(func() error {
		framework.Logf("Deleting network %s", network.Name())
		err := p.ops.DeleteNetwork(network)
		if err != nil && errors.Is(err, api.NotFound) {
			return nil
		}
		return err
	})
	return p.ops.GetNetwork(name)
}

func (p *Engine) AttachNetwork(network api.Network, container string) (api.NetworkInterface, error) {
	if p.testContext == nil {
		return api.NetworkInterface{},
			fmt.Errorf("AttachNetwork is invoked for container %s and network %s without test context",
				container, network.Name())
	}
	err := p.ops.AttachNetwork(network, container)
	if err != nil {
		return api.NetworkInterface{}, err
	}
	p.testContext.AddCleanUpFn(func() error {
		framework.Logf("Detaching network %s from %s", network.Name(), container)
		err := p.ops.DetachNetwork(network, container)
		if err != nil && errors.Is(err, api.NotFound) {
			return nil
		}
		return err
	})
	return p.ops.GetNetworkInterface(container, network.Name())
}

func (p *Engine) DetachNetwork(network api.Network, container string) error {
	if p.testContext == nil {
		return fmt.Errorf("DetachNetwork is invoked for container %s and network %s without test context",
			container, network.Name())
	}
	return p.ops.DetachNetwork(network, container)
}

func (p *Engine) DeleteNetwork(network api.Network) error {
	if p.testContext == nil {
		return fmt.Errorf("DeleteNetwork is invoked for network %s without test context", network.Name())
	}
	return p.ops.DeleteNetwork(network)
}
