package ovn

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

var _ = ginkgo.Describe("Cluster CIDR Address Set", func() {
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

	ginkgo.Context("initClusterCIDRAddressSet", func() {
		ginkgo.It("creates address set for no-overlay mode with outbound SNAT enabled", func() {
			netInfo.topology = types.Layer3Topology
			// Set the transport to no-overlay
			config.Default.Transport = config.TransportNoOverlay
			config.NoOverlay.OutboundSNAT = config.NoOverlaySNATEnabled

			err := initClusterCIDRAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Verify address set was created
			dbIDs := libovsdbops.NewDbObjectIDs(
				libovsdbops.AddressSetClusterCIDR,
				controllerName,
				map[libovsdbops.ExternalIDKey]string{
					libovsdbops.ObjectNameKey: clusterCIDR,
					libovsdbops.NetworkKey:    netInfo.GetNetworkName(),
				},
			)
			as, err := addressSetFactory.GetAddressSet(dbIDs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(as).NotTo(gomega.BeNil())
		})

		ginkgo.It("skips creation for overlay mode", func() {
			netInfo.topology = types.Layer3Topology
			// Set the transport to overlay (geneve)
			config.Default.Transport = config.TransportGeneve

			err := initClusterCIDRAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Verify address set was NOT created using the helper function
			as, err := getClusterCIDRAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(as).To(gomega.BeNil())
		})

		ginkgo.It("skips creation when outbound SNAT is disabled", func() {
			netInfo.topology = types.Layer3Topology
			config.Default.Transport = config.TransportNoOverlay
			config.NoOverlay.OutboundSNAT = config.NoOverlaySNATDisabled

			err := initClusterCIDRAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Verify address set was NOT created using the helper function
			as, err := getClusterCIDRAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(as).To(gomega.BeNil())
		})
	})

	ginkgo.Context("addClusterCIDRsToAddressSet", func() {
		ginkgo.BeforeEach(func() {
			config.Default.Transport = config.TransportNoOverlay
			config.NoOverlay.OutboundSNAT = config.NoOverlaySNATEnabled
			err := initClusterCIDRAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("adds cluster subnets to the address set", func() {
			err := addClusterCIDRsToAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Verify subnets were added
			dbIDs := libovsdbops.NewDbObjectIDs(
				libovsdbops.AddressSetClusterCIDR,
				controllerName,
				map[libovsdbops.ExternalIDKey]string{
					libovsdbops.ObjectNameKey: clusterCIDR,
					libovsdbops.NetworkKey:    netInfo.GetNetworkName(),
				},
			)
			as, err := addressSetFactory.GetAddressSet(dbIDs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ipv4Addrs, ipv6Addrs := as.GetAddresses()
			// Check that addresses match the cluster subnets
			expectedSubnets := []string{}
			for _, subnet := range netInfo.Subnets() {
				expectedSubnets = append(expectedSubnets, subnet.CIDR.String())
			}
			allAddrs := append(ipv4Addrs, ipv6Addrs...)
			gomega.Expect(allAddrs).To(gomega.ConsistOf(expectedSubnets))
		})

		ginkgo.It("handles empty subnets gracefully", func() {
			// Create a test NetInfo with no subnets
			emptyNetInfo := &testNetInfo{
				NetInfo: netInfo.NetInfo,
				subnets: []config.CIDRNetworkEntry{},
			}

			err := addClusterCIDRsToAddressSet(addressSetFactory, emptyNetInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("skips adding for overlay mode", func() {
			config.Default.Transport = config.TransportGeneve

			err := addClusterCIDRsToAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("cleanupClusterCIDRAddressSet", func() {
		ginkgo.It("removes the address set", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.NoOverlay.OutboundSNAT = config.NoOverlaySNATEnabled
			err := initClusterCIDRAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Verify address set exists before cleanup
			dbIDs := libovsdbops.NewDbObjectIDs(
				libovsdbops.AddressSetClusterCIDR,
				controllerName,
				map[libovsdbops.ExternalIDKey]string{
					libovsdbops.ObjectNameKey: clusterCIDR,
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
			err = cleanupClusterCIDRAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Verify address set was removed - UUIDs should be empty after cleanup
			as, err = addressSetFactory.GetAddressSet(dbIDs)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v4UUID, v6UUID = as.GetASUUID()
			gomega.Expect(v4UUID).To(gomega.BeEmpty())
			gomega.Expect(v6UUID).To(gomega.BeEmpty())
		})
	})

	ginkgo.Context("getClusterCIDRAsUUID", func() {
		ginkgo.It("returns UUIDs for IPv4 and IPv6 address sets", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.NoOverlay.OutboundSNAT = config.NoOverlaySNATEnabled
			err := initClusterCIDRAddressSet(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			v4UUID, v6UUID, err := getClusterCIDRAsUUID(addressSetFactory, netInfo, controllerName)
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

		ginkgo.It("returns empty strings for overlay mode", func() {
			config.Default.Transport = config.TransportGeneve

			v4UUID, v6UUID, err := getClusterCIDRAsUUID(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(v4UUID).To(gomega.BeEmpty())
			gomega.Expect(v6UUID).To(gomega.BeEmpty())
		})

		ginkgo.It("returns empty strings when outbound SNAT is disabled", func() {
			config.Default.Transport = config.TransportNoOverlay
			config.NoOverlay.OutboundSNAT = config.NoOverlaySNATDisabled

			v4UUID, v6UUID, err := getClusterCIDRAsUUID(addressSetFactory, netInfo, controllerName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(v4UUID).To(gomega.BeEmpty())
			gomega.Expect(v6UUID).To(gomega.BeEmpty())
		})
	})
})
