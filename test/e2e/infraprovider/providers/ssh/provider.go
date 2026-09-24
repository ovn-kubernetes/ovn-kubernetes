// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package ssh provides remote KinD infrastructure through a container runtime over SSH.
// It connects to the runtime host, not individual Kubernetes nodes.
package ssh

import (
	"errors"
	"fmt"

	"github.com/onsi/ginkgo/v2"

	"k8s.io/kubernetes/test/e2e/framework"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/container"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/portalloc"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/runner"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/testcontext"
)

// Compile-time interface assertions.
var (
	_ api.Provider = (*Provider)(nil)
	_ api.Context  = (*providerContext)(nil)
)

// ErrUnsupported is returned by capabilities this provider does not implement,
// so callers can detect them with errors.Is.
var ErrUnsupported = errors.New("capability not supported by the ssh infra provider")

// Provider runs container runtime commands on a host reached over SSH. Cluster
// nodes must be containers on that runtime, which is the remote KinD topology
// this provider supports. The embedded engine supplies both the external
// container surface and the container operations the node methods use. Provider
// creates the SSH runner, so Provider closes it.
type Provider struct {
	*container.Engine
	cfg      Config
	runner   api.Runner
	hostPort *portalloc.PortAllocator
}

// New builds the SSH runner from cfg's connection fields and returns a Provider
// driving cfg.Runtime through it.
func New(cfg Config) (*Provider, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	sshRunner, err := runner.NewSSHRunnerWithOptions(cfg.Host, cfg.User, cfg.Port, cfg.PrivateKeyPath, runner.SSHOptions{
		KnownHostsPath:  cfg.KnownHostsPath,
		InsecureHostKey: cfg.InsecureHostKey,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create ssh runner for %s@%s: %w", cfg.User, cfg.Host, err)
	}
	framework.Logf("ssh infra provider: runtime %q on %s@%s", cfg.Runtime, cfg.User, cfg.Host)
	return &Provider{
		Engine:   container.NewEngine(cfg.Runtime, sshRunner),
		cfg:      cfg,
		runner:   sshRunner,
		hostPort: portalloc.New(1024, 65535),
	}, nil
}

// Close releases the SSH connection created in New. The connection is shared by
// every test in the suite, so only suite teardown calls this (see TestMain).
func (p *Provider) Close() error {
	if closer, ok := p.runner.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// UnsupportedCapabilities lists the api.Provider capabilities this provider does
// not implement, so specs depending on them are skipped.
func (p *Provider) UnsupportedCapabilities() []string {
	return []string{api.CapabilitySetupUnderlay}
}

// --- api.ClusterProvider ---

func (p *Provider) Name() string {
	return "ssh"
}

func (p *Provider) PrimaryNetwork() (api.Network, error) {
	return p.GetNetwork(p.cfg.PrimaryNetwork)
}

func (p *Provider) GetK8NodeNetworkInterface(instance string, network api.Network) (api.NetworkInterface, error) {
	ni, err := p.GetNetworkInterface(instance, network.Name())
	if err != nil {
		return ni, fmt.Errorf("get network interface for node %q on network %q: %w", instance, network.Name(), err)
	}
	return ni, nil
}

func (p *Provider) ExecK8NodeCommand(nodeName string, cmd []string) (string, error) {
	out, err := p.ExecContainerCommand(nodeName, cmd)
	if err != nil {
		return out, fmt.Errorf("exec command on node %q: %w", nodeName, err)
	}
	return out, nil
}

func (p *Provider) GetK8HostPort() uint16 {
	return p.hostPort.Allocate()
}

func (p *Provider) ShutdownNode(nodeName string) error {
	if err := p.StopContainer(nodeName); err != nil {
		return fmt.Errorf("shutdown node %q: %w", nodeName, err)
	}
	return nil
}

func (p *Provider) StartNode(nodeName string) error {
	if err := p.StartContainer(nodeName); err != nil {
		return fmt.Errorf("start node %q: %w", nodeName, err)
	}
	return nil
}

// PreloadImages delegates to Config.ImagePreloader. Preloading writes into the
// image stores of the cluster's nodes, which this provider cannot reach, so
// without a preloader the images must already be in those stores, or pullable
// from them.
func (p *Provider) PreloadImages(images []string) {
	if p.cfg.ImagePreloader == nil {
		framework.Logf("Warning: ssh provider does not preload images (%d requested); they must already be in the "+
			"cluster nodes' image stores or pullable from them", len(images))
		return
	}
	if err := p.cfg.ImagePreloader(images); err != nil {
		framework.Logf("Warning: ssh provider failed to preload %d image(s): %v", len(images), err)
	}
}

func (p *Provider) GetDefaultTimeoutContext() *framework.TimeoutContext {
	return framework.NewTimeoutContext()
}

// --- api.Provider ---

func (p *Provider) NewTestContext() api.Context {
	tc := &testcontext.TestContext{}
	ginkgo.DeferCleanup(tc.CleanUp)
	return &providerContext{
		TestContext: tc,
		Engine:      p.Engine.WithTestContext(tc),
	}
}

// providerContext is the per-test sandbox. The context bound engine registers
// the cleanup of everything it creates on the test context.
type providerContext struct {
	*testcontext.TestContext
	*container.Engine
}

// SetupUnderlay is unimplemented: the localnet underlay wiring lives in the kind
// provider's unexported OVS helpers. Specs that need it are skipped through
// infraprovider.SupportsSetupUnderlay.
func (c *providerContext) SetupUnderlay(_ *framework.Framework, _ api.Underlay) error {
	return fmt.Errorf("SetupUnderlay: %w", ErrUnsupported)
}
