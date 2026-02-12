package ovn

import (
	"net"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
)

var _ = ginkgo.Describe("No-Overlay SNAT Exemption Address Set", func() {
	var (
		fakeOvn           *FakeOVN
		addressSetFactory addressset.AddressSetFactory
		netInfo           *testNetInfo
		controllerName    = DefaultNetworkControllerName
	)

	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		fakeOvn = NewFakeOVN(false)
		fakeOvn.start()
		addressSetFactory = fakeOvn.controller.addressSetFactory
		netInfo = &testNetInfo{
			NetInfo: fakeOvn.controller.GetNetInfo(),
		}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("syncNoOverlaySNATExemptionAddressSet", func() {
		ginkgo.BeforeEach(func() {
			// Set up cluster subnets for testing
			_, cidr, err := net.ParseCIDR("10.128.0.0/14")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			netInfo.subnets = []config.CIDRNetworkEntry{
				{CIDR: cidr, HostSubnetLength: 24},
			}

			_, err = initNoOverlaySNATExemptionAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("syncs cluster subnets and node IPs to the address set", func() {
			nodeIPs := []string{"192.168.1.10", "192.168.1.11"}
			err := syncNoOverlaySNATExemptionAddressSet(addressSetFactory, netInfo, controllerName, nodeIPs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Verify subnets and node IPs were added
			dbIDs := libovsdbops.NewDbObjectIDs(
				libovsdbops.AddressSetNoOverlaySNATExemption,
				controllerName,
				map[libovsdbops.ExternalIDKey]string{
					libovsdbops.ObjectNameKey: asName,
					libovsdbops.NetworkKey:    netInfo.GetNetworkName(),
				},
			)
			as, err := addressSetFactory.GetAddressSet(dbIDs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ipv4Addrs, ipv6Addrs := as.GetAddresses()
			// Check that addresses match the cluster subnets plus node IPs
			expectedAddrs := []string{}
			for _, subnet := range netInfo.Subnets() {
				expectedAddrs = append(expectedAddrs, subnet.CIDR.String())
			}
			expectedAddrs = append(expectedAddrs, nodeIPs...)
			allAddrs := make([]string, 0, len(ipv4Addrs)+len(ipv6Addrs))
			allAddrs = append(allAddrs, ipv4Addrs...)
			allAddrs = append(allAddrs, ipv6Addrs...)
			gomega.Expect(allAddrs).To(gomega.ConsistOf(expectedAddrs))
		})

		ginkgo.It("handles idempotent syncs correctly", func() {
			nodeIPs := []string{"192.168.1.10", "192.168.1.11"}

			// First sync
			err := syncNoOverlaySNATExemptionAddressSet(addressSetFactory, netInfo, controllerName, nodeIPs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Get the addresses after first sync
			dbIDs := libovsdbops.NewDbObjectIDs(
				libovsdbops.AddressSetNoOverlaySNATExemption,
				controllerName,
				map[libovsdbops.ExternalIDKey]string{
					libovsdbops.ObjectNameKey: asName,
					libovsdbops.NetworkKey:    netInfo.GetNetworkName(),
				},
			)
			as, err := addressSetFactory.GetAddressSet(dbIDs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			firstIPv4, firstIPv6 := as.GetAddresses()

			// Second sync with same data
			err = syncNoOverlaySNATExemptionAddressSet(addressSetFactory, netInfo, controllerName, nodeIPs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Verify addresses remain the same
			secondIPv4, secondIPv6 := as.GetAddresses()
			gomega.Expect(secondIPv4).To(gomega.Equal(firstIPv4))
			gomega.Expect(secondIPv6).To(gomega.Equal(firstIPv6))
		})

		ginkgo.It("updates node IPs when they change", func() {
			// First sync with initial node IPs
			initialNodeIPs := []string{"192.168.1.10"}
			err := syncNoOverlaySNATExemptionAddressSet(addressSetFactory, netInfo, controllerName, initialNodeIPs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Verify initial state
			dbIDs := libovsdbops.NewDbObjectIDs(
				libovsdbops.AddressSetNoOverlaySNATExemption,
				controllerName,
				map[libovsdbops.ExternalIDKey]string{
					libovsdbops.ObjectNameKey: asName,
					libovsdbops.NetworkKey:    netInfo.GetNetworkName(),
				},
			)
			as, err := addressSetFactory.GetAddressSet(dbIDs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ipv4Addrs, ipv6Addrs := as.GetAddresses()
			initialExpected := []string{}
			for _, subnet := range netInfo.Subnets() {
				initialExpected = append(initialExpected, subnet.CIDR.String())
			}
			initialExpected = append(initialExpected, initialNodeIPs...)
			allAddrs := make([]string, 0, len(ipv4Addrs)+len(ipv6Addrs))
			allAddrs = append(allAddrs, ipv4Addrs...)
			allAddrs = append(allAddrs, ipv6Addrs...)
			gomega.Expect(allAddrs).To(gomega.ConsistOf(initialExpected))

			// Sync with updated node IPs
			updatedNodeIPs := []string{"192.168.1.10", "192.168.1.11", "192.168.1.12"}
			err = syncNoOverlaySNATExemptionAddressSet(addressSetFactory, netInfo, controllerName, updatedNodeIPs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Verify updated state
			ipv4Addrs, ipv6Addrs = as.GetAddresses()
			updatedExpected := []string{}
			for _, subnet := range netInfo.Subnets() {
				updatedExpected = append(updatedExpected, subnet.CIDR.String())
			}
			updatedExpected = append(updatedExpected, updatedNodeIPs...)
			allAddrs = make([]string, 0, len(ipv4Addrs)+len(ipv6Addrs))
			allAddrs = append(allAddrs, ipv4Addrs...)
			allAddrs = append(allAddrs, ipv6Addrs...)
			gomega.Expect(allAddrs).To(gomega.ConsistOf(updatedExpected))
		})
	})

	ginkgo.Context("cleanupNoOverlaySNATExemptionAddressSet", func() {
		ginkgo.It("removes the address set", func() {
			_, err := initNoOverlaySNATExemptionAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Verify address set exists before cleanup
			dbIDs := libovsdbops.NewDbObjectIDs(
				libovsdbops.AddressSetNoOverlaySNATExemption,
				controllerName,
				map[libovsdbops.ExternalIDKey]string{
					libovsdbops.ObjectNameKey: asName,
					libovsdbops.NetworkKey:    netInfo.GetNetworkName(),
				},
			)
			as, err := addressSetFactory.GetAddressSet(dbIDs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(as).NotTo(gomega.BeNil())

			// Verify UUIDs exist before cleanup
			v4UUID, v6UUID := as.GetASUUID()
			if config.IPv4Mode {
				gomega.Expect(v4UUID).NotTo(gomega.BeEmpty())
			}
			if config.IPv6Mode {
				gomega.Expect(v6UUID).NotTo(gomega.BeEmpty())
			}

			// Cleanup
			err = cleanupNoOverlaySNATExemptionAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Verify address set was removed - UUIDs should be empty after cleanup
			as, err = addressSetFactory.GetAddressSet(dbIDs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v4UUID, v6UUID = as.GetASUUID()
			gomega.Expect(v4UUID).To(gomega.BeEmpty())
			gomega.Expect(v6UUID).To(gomega.BeEmpty())
		})
	})

	ginkgo.Context("getNoOverlaySNATExemptionAsUUID", func() {
		ginkgo.It("returns UUIDs for IPv4 and IPv6 address sets when it exists", func() {
			_, err := initNoOverlaySNATExemptionAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			v4UUID, v6UUID, err := getNoOverlaySNATExemptionAsUUID(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// For IPv4 mode, v4UUID should be set
			if config.IPv4Mode {
				gomega.Expect(v4UUID).NotTo(gomega.BeEmpty())
			}
			// For IPv6 mode, v6UUID should be set
			if config.IPv6Mode {
				gomega.Expect(v6UUID).NotTo(gomega.BeEmpty())
			}
		})

		ginkgo.It("returns empty strings when it doesn't exist", func() {
			v4UUID, v6UUID, err := getNoOverlaySNATExemptionAsUUID(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(v4UUID).To(gomega.BeEmpty())
			gomega.Expect(v6UUID).To(gomega.BeEmpty())
		})
	})
})
