// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package udnvip

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"

	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// ---------------------------------------------------------------------------
// networkAllocator unit tests
// ---------------------------------------------------------------------------

func mustParseCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	require.NoError(t, err)
	return n
}

func newTestAllocator(t *testing.T, cidrs ...string) *networkAllocator {
	t.Helper()
	subnets := make([]*net.IPNet, len(cidrs))
	for i, c := range cidrs {
		subnets[i] = mustParseCIDR(t, c)
	}
	return &networkAllocator{
		subnets:   subnets,
		allocated: make(map[string]net.IP),
	}
}

func TestNetworkAllocator_AllocateSequential(t *testing.T) {
	alloc := newTestAllocator(t, "10.10.10.0/30") // .1, .2 usable (.0 network, .3 broadcast)

	ip1, err := alloc.allocate("ns/svc1")
	require.NoError(t, err)
	assert.Equal(t, "10.10.10.1", ip1.String())

	ip2, err := alloc.allocate("ns/svc2")
	require.NoError(t, err)
	assert.Equal(t, "10.10.10.2", ip2.String())

	// Third allocation should fail (only 2 host addresses)
	_, err = alloc.allocate("ns/svc3")
	assert.Error(t, err)
}

func TestNetworkAllocator_Idempotent(t *testing.T) {
	alloc := newTestAllocator(t, "10.10.10.0/30")

	ip1, err := alloc.allocate("ns/svc1")
	require.NoError(t, err)

	// Same key should return same IP
	ip2, err := alloc.allocate("ns/svc1")
	require.NoError(t, err)
	assert.Equal(t, ip1.String(), ip2.String())
}

func TestNetworkAllocator_Release(t *testing.T) {
	alloc := newTestAllocator(t, "10.10.10.0/30")

	ip1, err := alloc.allocate("ns/svc1")
	require.NoError(t, err)

	alloc.release("ns/svc1")

	// After release, the IP should be reusable
	ip2, err := alloc.allocate("ns/svc2")
	require.NoError(t, err)
	assert.Equal(t, ip1.String(), ip2.String(), "released IP should be reused")
}

func TestNetworkAllocator_Seed(t *testing.T) {
	alloc := newTestAllocator(t, "10.10.10.0/30")
	seedIP := net.ParseIP("10.10.10.1")
	alloc.seed("ns/svc1", seedIP)

	// Seed should prevent that IP from being allocated to another service
	ip, err := alloc.allocate("ns/svc2")
	require.NoError(t, err)
	assert.NotEqual(t, "10.10.10.1", ip.String())
}

func TestNetworkAllocator_MultipleSubnets(t *testing.T) {
	// /30 has .1, .2; second /30 has .5, .6 (10.10.10.4/30 → .5, .6)
	alloc := newTestAllocator(t, "10.10.10.0/30", "10.10.10.4/30")

	for i := 1; i <= 4; i++ {
		_, err := alloc.allocate("ns/svc" + string(rune('0'+i)))
		require.NoError(t, err)
	}

	// Fifth allocation should fail
	_, err := alloc.allocate("ns/svc5")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Helper function tests
// ---------------------------------------------------------------------------

func TestServiceSubnetsFromCUDN(t *testing.T) {
	cudn := &udnv1.ClusterUserDefinedNetwork{
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:           udnv1.NetworkRolePrimary,
					ServiceSubnets: []udnv1.CIDR{"10.10.10.0/24", "10.10.11.0/24"},
				},
			},
		},
	}

	subnets := serviceSubnetsFromCUDN(cudn)
	require.Len(t, subnets, 2)
	assert.Equal(t, "10.10.10.0/24", subnets[0].String())
	assert.Equal(t, "10.10.11.0/24", subnets[1].String())
}

func TestServiceSubnetsFromCUDN_NoLayer2(t *testing.T) {
	cudn := &udnv1.ClusterUserDefinedNetwork{
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
			},
		},
	}
	assert.Nil(t, serviceSubnetsFromCUDN(cudn))
}

func TestIsUDNLoadBalancer(t *testing.T) {
	class := types.UDNLoadBalancerClass
	svc := &corev1.Service{
		Spec: corev1.ServiceSpec{
			Type:              corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass: &class,
		},
	}
	assert.True(t, IsUDNLoadBalancer(svc))
	assert.True(t, IsUDNVIPService(svc))
}

func TestNotUDNLoadBalancer(t *testing.T) {
	other := "some-other-class"
	svc := &corev1.Service{
		Spec: corev1.ServiceSpec{
			Type:              corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass: &other,
		},
	}
	assert.False(t, IsUDNLoadBalancer(svc))
	assert.False(t, IsUDNVIPService(svc))
}

// ---------------------------------------------------------------------------
// isBroadcast / nextIP helper tests
// ---------------------------------------------------------------------------

func TestNextIP(t *testing.T) {
	ip := net.ParseIP("10.0.0.1").To4()
	next := nextIP(ip)
	assert.Equal(t, "10.0.0.2", next.String())
}

func TestIsBroadcast(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.10.10.0/30")
	assert.False(t, isBroadcast(net.ParseIP("10.10.10.1").To4(), subnet))
	assert.False(t, isBroadcast(net.ParseIP("10.10.10.2").To4(), subnet))
	assert.True(t, isBroadcast(net.ParseIP("10.10.10.3").To4(), subnet))
}

func TestIsBroadcast_Slash32(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.10.10.1/32")
	// /32 has no broadcast
	assert.False(t, isBroadcast(net.ParseIP("10.10.10.1").To4(), subnet))
}

// ---------------------------------------------------------------------------
// rebuildAllocators tests — using a minimal fake controller
// ---------------------------------------------------------------------------
