// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import (
	"net"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
)

func cidrs(ss ...string) []vtepv1.CIDR {
	out := make([]vtepv1.CIDR, len(ss))
	for i, s := range ss {
		out[i] = vtepv1.CIDR(s)
	}
	return out
}

func ipNetStrings(ipNets []*net.IPNet) []string {
	out := make([]string, len(ipNets))
	for i, ipNet := range ipNets {
		out[i] = ipNet.String()
	}
	return out
}

var _ = ginkgo.Describe("vtepIPAllocator", func() {

	ginkgo.It("adds IPv4 CIDRs as /32 ranges and IPv6 CIDRs as /128 ranges", func() {
		a, err := newVTEPIPAllocator(cidrs("10.0.0.0/24", "fd00::/120"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		v4count, v6count := a.allocator.RangeCount()
		gomega.Expect(v4count).To(gomega.Equal(uint64(1)))
		gomega.Expect(v6count).To(gomega.Equal(uint64(1)))

		ips, err := a.allocateForNode("node-1")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ips).To(gomega.HaveLen(2))
		gomega.Expect(ips[0].Mask).To(gomega.Equal(net.CIDRMask(32, 32)))
		gomega.Expect(ips[1].Mask).To(gomega.Equal(net.CIDRMask(128, 128)))
	})

	ginkgo.It("allocateForNode is idempotent and returns the same IPs on repeated calls", func() {
		a, err := newVTEPIPAllocator(cidrs("10.0.0.0/24", "fd00::/120"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		first, err := a.allocateForNode("node-1")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(first).To(gomega.HaveLen(2))

		second, err := a.allocateForNode("node-1")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ipNetStrings(second)).To(gomega.Equal(ipNetStrings(first)))
	})

	ginkgo.It("adds multiple same-family CIDRs as separate ranges", func() {
		a, err := newVTEPIPAllocator(cidrs("10.0.0.0/30", "10.0.1.0/30"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		v4count, _ := a.allocator.RangeCount()
		gomega.Expect(v4count).To(gomega.Equal(uint64(2)))
	})

	ginkgo.It("rejects invalid CIDRs", func() {
		_, err := newVTEPIPAllocator(cidrs("not-a-cidr"))
		gomega.Expect(err).To(gomega.HaveOccurred())
	})

	ginkgo.It("markAllocatedForNode wraps IPs in correct /32 and /128 masks", func() {
		a, err := newVTEPIPAllocator(cidrs("10.0.0.0/24", "fd00::/120"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = a.markAllocatedForNode("node-1", []net.IP{
			net.ParseIP("10.0.0.50"),
			net.ParseIP("fd00::aa"),
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Idempotent allocate should return the marked IPs
		ips, err := a.allocateForNode("node-1")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ipNetStrings(ips)).To(gomega.Equal([]string{"10.0.0.50/32", "fd00::aa/128"}))
	})

	ginkgo.It("markAllocatedForNode detects conflicts with existing allocations", func() {
		a, err := newVTEPIPAllocator(cidrs("10.0.0.0/24"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = a.markAllocatedForNode("node-1", []net.IP{net.ParseIP("10.0.0.10")})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = a.markAllocatedForNode("node-2", []net.IP{net.ParseIP("10.0.0.10")})
		gomega.Expect(err).To(gomega.HaveOccurred())
	})

	ginkgo.It("IPv4 and IPv6 overflow independently with different-sized CIDRs", func() {
		// IPv4: two /31 ranges (2 IPs each = 4 total)
		// IPv6: one /126 range (4 IPs)
		a, err := newVTEPIPAllocator(cidrs("10.0.0.0/31", "10.0.1.0/31", "fd00::/126"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		v4count, v6count := a.allocator.RangeCount()
		gomega.Expect(v4count).To(gomega.Equal(uint64(2)))
		gomega.Expect(v6count).To(gomega.Equal(uint64(1)))

		// Allocate 2 nodes: both families serve from their first range
		ips0, err := a.allocateForNode("node-0")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ips0).To(gomega.HaveLen(2))

		ips1, err := a.allocateForNode("node-1")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ips1).To(gomega.HaveLen(2))

		// Allocate node-2: IPv4 first /31 exhausted, overflows to second /31;
		// IPv6 still has room in its /126
		ips2, err := a.allocateForNode("node-2")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ips2).To(gomega.HaveLen(2))
		gomega.Expect(ips2[0].IP.String()).To(gomega.Equal("10.0.1.0"))
	})

	ginkgo.It("addCIDR expands capacity after exhaustion", func() {
		a, err := newVTEPIPAllocator(cidrs("10.0.0.0/31"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// /31 with /32 host length = 2 IPs
		_, err = a.allocateForNode("node-0")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = a.allocateForNode("node-1")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Pool exhausted
		_, err = a.allocateForNode("node-2")
		gomega.Expect(err).To(gomega.HaveOccurred())

		// Expand with a new CIDR
		err = a.addCIDR("10.0.1.0/31")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Should now succeed from the new range
		ips, err := a.allocateForNode("node-2")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ipNetStrings(ips)).To(gomega.Equal([]string{"10.0.1.0/32"}))
	})
})
