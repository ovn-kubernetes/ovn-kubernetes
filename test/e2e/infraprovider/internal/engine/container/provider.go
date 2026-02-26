package container

import (
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/portalloc"
)

// Provider provides a base implementation of api.Provider with common infrastructure functionality.
// Infrastructure providers can embed this type to inherit default behavior and override specific
// methods when provider-specific customization is required.
type Provider struct {
	*ContainerOps
	ExternalContainerPort *portalloc.PortAllocator
	HostPort              *portalloc.PortAllocator
}

func (p *Provider) ExternalContainerPrimaryInterfaceName() string {
	return "eth0"
}

func (p *Provider) GetExternalContainerNetworkInterface(container api.ExternalContainer, network api.Network) (api.NetworkInterface, error) {
	return p.GetNetworkInterface(container.Name, network.Name())
}

func (p *Provider) GetK8NodeNetworkInterface(container string, network api.Network) (api.NetworkInterface, error) {
	return p.GetNetworkInterface(container, network.Name())
}

func (p *Provider) ExecK8NodeCommand(nodeName string, cmd []string) (string, error) {
	return p.ExecContainerCommand(nodeName, cmd)
}

func (p *Provider) ExecExternalContainerCommand(container api.ExternalContainer, cmd []string) (string, error) {
	return p.ExecContainerCommand(container.Name, cmd)
}

func (p *Provider) GetExternalContainerPort() uint16 {
	return p.ExternalContainerPort.Allocate()
}

func (p *Provider) GetK8HostPort() uint16 {
	return p.HostPort.Allocate()
}
