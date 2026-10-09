// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package subnet

import (
	"net"
	"sync"
	"testing"

	"github.com/onsi/gomega"

	ipam "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
)

func newOwnershipTest(t *testing.T) (*allocator, []*net.IPNet) {
	t.Helper()
	g := gomega.NewWithT(t)
	allocator := NewAllocator()
	g.Expect(allocator.AddOrUpdateSubnet(ownershipSubnetConfig(nil))).To(gomega.Succeed())
	return allocator, ovntest.MustParseIPNets("10.0.0.3/24", "fd00::3/64")
}

func ownershipSubnetConfig(initial map[string][]*net.IPNet) SubnetConfig {
	return SubnetConfig{
		Name:               "node",
		Subnets:            ovntest.MustParseIPNets("10.0.0.0/24", "fd00::/64"),
		ExcludeSubnets:     ovntest.MustParseIPNets("10.0.0.1/32", "10.0.0.2/32", "fd00::1/128", "fd00::2/128"),
		InitialAllocations: initial,
	}
}

func TestIPOwnershipReleaseAndReacquisition(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator, ips := newOwnershipTest(t)
	g.Expect(allocator.AllocateIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "a", ips)).To(gomega.Equal(ipam.ErrAllocated))
	g.Expect(allocator.ReleaseIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.ReleaseIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "b", ips)).To(gomega.Succeed())
	g.Expect(allocator.ReleaseIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("node", "b", ips)).To(gomega.BeTrue())
	err := allocator.AllocateIPs("node", "a", ips)
	g.Expect(err).To(gomega.MatchError(ipam.ErrAllocatedByOther))
	g.Expect(ipam.IsErrAllocated(err)).To(gomega.BeTrue())
	g.Expect(allocator.ReleaseIPs("node", "b", ips)).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.ReleaseIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.cache["node"].owners).To(gomega.BeEmpty())
}

func TestIPOwnershipIsolation(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator, ips := newOwnershipTest(t)
	g.Expect(allocator.AllocateIPs("node", "uid-a/nad-a", ips)).To(gomega.Succeed())
	config := ownershipSubnetConfig(nil)
	config.Name = "other-node"
	g.Expect(allocator.AddOrUpdateSubnet(config)).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("other-node", "uid-b/nad-a", ips)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("node", "uid-a/nad-a", ips)).To(gomega.BeTrue())
	allocator.DeleteSubnet("node")
	g.Expect(allocator.ReleaseIPs("node", "uid-a/nad-a", ips)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("node", "uid-a/nad-a", ips)).To(gomega.BeFalse())
	g.Expect(allocator.OwnsIPs("other-node", "uid-b/nad-a", ips)).To(gomega.BeTrue())
}

func TestIPOwnershipRollback(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator, ips := newOwnershipTest(t)
	g.Expect(allocator.AllocateIPs("node", "b", ips[1:])).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "a", ips)).To(gomega.MatchError(ipam.ErrAllocatedByOther))
	g.Expect(allocator.AllocateIPs("node", "a", ips[:1])).To(gomega.Succeed())
	g.Expect(allocator.ReleaseIPs("node", "a", ips)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("node", "b", ips[1:])).To(gomega.BeTrue())
	g.Expect(allocator.ReleaseIPs("node", "b", ips)).To(gomega.Succeed())
	badIPs := []*net.IPNet{ips[0], ovntest.MustParseIPNet("fd01::3/64")}
	g.Expect(allocator.AllocateIPs("node", "a", badIPs)).NotTo(gomega.Succeed())
	g.Expect(allocator.cache["node"].owners).To(gomega.BeEmpty())
	g.Expect(allocator.AllocateIPs("node", "b", ips)).To(gomega.Succeed())
}

func TestIPOwnershipConcurrentStaleRelease(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator, _ := newOwnershipTest(t)
	ips, err := allocator.AllocateNextIPs("node", "a")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(allocator.ReleaseIPs("node", "a", ips)).To(gomega.Succeed())
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 20 {
		wg.Go(func() {
			<-start
			for range 50 {
				if err := allocator.ReleaseIPs("node", "a", ips); err != nil {
					t.Errorf("stale release: %v", err)
				}
			}
		})
	}
	close(start)
	g.Expect(allocator.AllocateIPs("node", "b", ips)).To(gomega.Succeed())
	wg.Wait()
	g.Expect(allocator.OwnsIPs("node", "b", ips)).To(gomega.BeTrue())
}

func TestIPOwnershipRejectsPartialReservation(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator, ips := newOwnershipTest(t)
	g.Expect(allocator.AllocateIPs("node", "owner", ips[:1])).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "owner", ips)).NotTo(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("node", "owner", ips[:1])).To(gomega.BeTrue())
	g.Expect(allocator.AllocateIPs("node", "other", ips[1:])).To(gomega.Succeed(), "failed allocation must not reserve the missing family")
}

func TestIPOwnershipOnSubnetInitialization(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator := NewAllocator()
	ips := ovntest.MustParseIPNets("10.0.0.3/24", "fd00::3/64")
	otherIPs := ovntest.MustParseIPNets("10.0.0.4/24", "fd00::4/64")
	initial := map[string][]*net.IPNet{
		"vm-a": append(append([]*net.IPNet{}, ips...), ips...), // Migration pods share a reservation.
		"vm-b": otherIPs,
	}
	for range 2 {
		g.Expect(allocator.AddOrUpdateSubnet(ownershipSubnetConfig(initial))).To(gomega.Succeed())
		g.Expect(allocator.OwnsIPs("node", "vm-a", ips)).To(gomega.BeTrue())
		g.Expect(allocator.OwnsIPs("node", "vm-b", otherIPs)).To(gomega.BeTrue())
		g.Expect(allocator.AllocateIPs("node", "vm-a", ips)).To(gomega.Equal(ipam.ErrAllocated))
		g.Expect(allocator.AllocateIPs("node", "other", ips)).To(gomega.MatchError(ipam.ErrAllocatedByOther))
		g.Expect(allocator.ReleaseIPs("node", "other", ips)).To(gomega.Succeed())
		allocated, err := allocator.AllocateNextIPs("node", "pod")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(allocated).To(gomega.Equal(ovntest.MustParseIPNets("10.0.0.5/24", "fd00::5/64")))
	}
	g.Expect(allocator.ReleaseIPs("node", "vm-a", ips)).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "other", ips)).To(gomega.Succeed())
	g.Expect(allocator.ReleaseIPs("node", "vm-a", ips)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("node", "other", ips)).To(gomega.BeTrue())

	// Restored allocations must not turn infrastructure exclusions into releasable IPs.
	infrastructure := ovntest.MustParseIPNets("10.0.0.1/24", "fd00::2/64")
	g.Expect(allocator.ReleaseIPs("node", "vm-a", infrastructure)).To(gomega.Succeed())
	g.Expect(allocator.AllocateIPs("node", "vm-a", infrastructure)).To(gomega.MatchError(ipam.ErrAllocatedByOther))
}

func TestIPOwnershipFailedSubnetInitialization(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial map[string][]*net.IPNet
	}{
		{
			name: "conflicting owners",
			initial: map[string][]*net.IPNet{
				"vm-a": ovntest.MustParseIPNets("10.0.0.3/24", "fd00::3/64"),
				"vm-b": ovntest.MustParseIPNets("10.0.0.4/24", "fd00::3/64"),
			},
		},
		{
			name:    "outside managed subnet",
			initial: map[string][]*net.IPNet{"vm": ovntest.MustParseIPNets("10.0.0.3/24", "fd01::3/64")},
		},
		{
			name:    "excluded IPv4 address",
			initial: map[string][]*net.IPNet{"vm": ovntest.MustParseIPNets("10.0.0.1/24")},
		},
		{
			name:    "excluded IPv6 address",
			initial: map[string][]*net.IPNet{"vm": ovntest.MustParseIPNets("fd00::2/64")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			allocator, ips := newOwnershipTest(t)
			g.Expect(allocator.AllocateIPs("node", "original", ips)).To(gomega.Succeed())
			config := ownershipSubnetConfig(tc.initial)
			err := allocator.AddOrUpdateSubnet(config)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(allocator.OwnsIPs("node", "original", ips)).To(gomega.BeTrue())
			g.Expect(allocator.AllocateIPs("node", "other", ips)).To(gomega.MatchError(ipam.ErrAllocatedByOther))
			g.Expect(allocator.ReleaseIPs("node", "original", ips)).To(gomega.Succeed())
			g.Expect(allocator.AllocateIPs("node", "other", ips)).To(gomega.Succeed())
		})
	}
}

func TestIPOwnershipStaticReservations(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator := NewAllocator()
	ips := ovntest.MustParseIPNets("10.0.0.1/30", "fd00::1/126")
	config := SubnetConfig{
		Name:               "network",
		Subnets:            ovntest.MustParseIPNets("10.0.0.0/30", "fd00::/126"),
		ReservedSubnets:    ovntest.MustParseIPNets("10.0.0.0/30", "fd00::/126"),
		InitialAllocations: map[string][]*net.IPNet{"original": ips},
	}
	g.Expect(allocator.AddOrUpdateSubnet(config)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("network", "original", ips)).To(gomega.BeTrue())
	g.Expect(allocator.AllocateIPs("network", "replacement", ips)).To(gomega.MatchError(ipam.ErrAllocatedByOther))
	g.Expect(allocator.ReleaseIPs("network", "original", ips)).To(gomega.Succeed())
	// Releasing a static reservation must not free the dynamic pool's exclusion.
	_, err := allocator.AllocateNextIPs("network", "dynamic")
	g.Expect(err).To(gomega.MatchError(ipam.ErrFull))
	g.Expect(allocator.AllocateIPs("network", "replacement", ips)).To(gomega.Succeed())
	g.Expect(allocator.ReleaseIPs("network", "original", ips)).To(gomega.Succeed())
	g.Expect(allocator.OwnsIPs("network", "replacement", ips)).To(gomega.BeTrue())
	g.Expect(allocator.ReleaseIPs("network", "replacement", ips)).To(gomega.Succeed())

	badIPs := []*net.IPNet{ips[0], ovntest.MustParseIPNet("fd01::1/126")}
	g.Expect(allocator.AllocateIPs("network", "failed", badIPs)).NotTo(gomega.Succeed())
	g.Expect(allocator.cache["network"].owners).To(gomega.BeEmpty())
	g.Expect(allocator.AllocateIPs("network", "original", ips)).To(gomega.Succeed())
}

func TestIPOwnershipAllocateNextRollback(t *testing.T) {
	g := gomega.NewWithT(t)
	allocator := NewAllocator()
	g.Expect(allocator.AddOrUpdateSubnet(SubnetConfig{
		Name:           "network",
		Subnets:        ovntest.MustParseIPNets("10.0.0.0/30", "fd00::/126"),
		ExcludeSubnets: ovntest.MustParseIPNets("fd00::/126"),
	})).To(gomega.Succeed())
	_, err := allocator.AllocateNextIPs("network", "failed")
	g.Expect(err).To(gomega.MatchError(ipam.ErrFull))
	g.Expect(allocator.cache["network"].owners).To(gomega.BeEmpty())
	// The first family must be available despite failing on the second one.
	for _, ip := range []string{"10.0.0.1/30", "10.0.0.2/30"} {
		g.Expect(allocator.AllocateIPs("network", "other", ovntest.MustParseIPNets(ip))).To(gomega.Succeed())
	}
}
