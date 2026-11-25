// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package base implements the provider behavior that does not depend on how the
// cluster was built, so any infrastructure provider can embed it.
package base

import (
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/test/e2e/framework"
)

// Provider runs external containers through the given ExternalContainerProvider,
// such as a container engine on the test runner or on a remote host.
type Provider struct {
	api.ExternalContainerProvider
}

func (Provider) GetDefaultTimeoutContext() *framework.TimeoutContext {
	return framework.NewTimeoutContext()
}

func (Provider) GetK8HostPort(f *framework.Framework, cleanup api.ContextCleanUp) uint16 {
	// Cap below 10250 (kubelet default) and the ephemeral range (ip_local_port_range).
	port, err := allocators.AllocateHostPort(f, cleanup, 1024, 10249)
	framework.ExpectNoError(err)
	return port
}

func (Provider) InfrastructureNetworkExclusions() (ipv4, ipv6 sets.Set[string]) {
	return nil, nil
}
