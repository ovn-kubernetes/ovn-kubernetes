package subnet

import (
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	ipam "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("Subnet IP allocator operations", func() {
	const subnetName = "subnet1"
	var (
		allocator Allocator
	)

	ginkgo.BeforeEach(func() {
		allocator = NewAllocator()
	})

	ginkgo.Context("when adding subnets", func() {
		ginkgo.It("creates each IPAM and reserves IPs correctly", func() {
			subnets := []string{
				"10.1.1.0/24",
				"2000::/64",
			}

			expectedIPs := []string{"10.1.1.1", "2000::1"}

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err := allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for i, ip := range ips {
				gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
			}
		})

		ginkgo.It("handles updates to the subnets correctly", func() {
			subnets := []string{
				"10.1.1.0/24",
				"2000::/64",
			}

			expectedIPs := []string{"10.1.1.1", "2000::1"}

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err := allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for i, ip := range ips {
				gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
			}
			subnets = []string{"10.1.2.0/24"}
			expectedIPs = []string{"10.1.2.1"}
			err = allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err = allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for i, ip := range ips {
				gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
			}
		})

		ginkgo.It("excludes subnets correctly", func() {
			subnets := []string{
				"10.1.1.0/24",
			}
			excludes := []string{
				"10.1.1.0/29",
			}

			expectedIPs := []string{"10.1.1.8"}

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:           subnetName,
				Subnets:        ovntest.MustParseIPNets(subnets...),
				ExcludeSubnets: ovntest.MustParseIPNets(excludes...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err := allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for i, ip := range ips {
				gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
			}
		})

	})

	ginkgo.Context("when allocating IP addresses", func() {
		ginkgo.It("IPAM for each subnet allocates IPs contiguously", func() {
			subnets := []string{
				"10.1.1.0/24",
				"2000::/64",
			}

			expectedIPAllocations := [][]string{
				{"10.1.1.1", "2000::1"},
				{"10.1.1.2", "2000::2"},
			}

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, expectedIPs := range expectedIPAllocations {
				ips, err := allocator.AllocateNextIPs(subnetName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}
			}
		})

		ginkgo.It("IPAM allocates, releases, and reallocates IPs correctly", func() {
			subnets := []string{
				"10.1.1.0/24",
			}

			expectedIPAllocations := [][]string{
				{"10.1.1.1"},
				{"10.1.1.2"},
			}
			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for _, expectedIPs := range expectedIPAllocations {
				ips, err := allocator.AllocateNextIPs(subnetName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}
				err = allocator.ReleaseIPs(subnetName, ips)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = allocator.AllocateIPPerSubnet(subnetName, ips)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
		})

		ginkgo.It("fails to allocate multiple IPs from the same subnet", func() {
			subnets := []string{"10.1.1.0/24", "2000::/64"}

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ips, err := util.ParseIPNets([]string{"10.1.1.1/24", "10.1.1.2/24"})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(allocator.AllocateIPPerSubnet(subnetName, ips)).To(gomega.MatchError(
				"failed to allocate IP 10.1.1.2 for subnet1: attempted to reserve multiple IPs in the same continuous IPAM instance",
			))
		})

		ginkgo.It("releases IPs for other subnets when any other subnet allocation fails", func() {
			subnets := []string{
				"10.1.1.0/24",
				"10.1.2.0/29",
			}

			expectedIPAllocations := [][]string{
				{"10.1.1.1", "10.1.2.1"},
				{"10.1.1.2", "10.1.2.2"},
				{"10.1.1.3", "10.1.2.3"},
				{"10.1.1.4", "10.1.2.4"},
				{"10.1.1.5", "10.1.2.5"},
			}

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// exhaust valid ips in second subnet
			for _, expectedIPs := range expectedIPAllocations {
				ips, err := allocator.AllocateNextIPs(subnetName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}
			}
			ips, err := allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedIPAllocation := [][]string{
				{"10.1.1.6", "10.1.2.6"},
			}
			for _, expectedIPs := range expectedIPAllocation {
				for i, ip := range ips {
					gomega.Expect(ip.IP.String()).To(gomega.Equal(expectedIPs[i]))
				}
			}

			// now try one more allocation and expect it to fail
			ips, err = allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).To(gomega.MatchError(ipam.ErrFull))
			gomega.Expect(ips).To(gomega.BeEmpty())
		})

		ginkgo.It("fails correctly when trying to block a previously allocated IP", func() {
			subnets := []string{
				"10.1.1.0/24",
				"2000::/64",
			}

			expectedIPs := []string{
				"10.1.1.1/24",
				"2000::1/64",
			}

			err := allocator.AddOrUpdateSubnet(SubnetConfig{
				Name:    subnetName,
				Subnets: ovntest.MustParseIPNets(subnets...),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ips, err := allocator.AllocateNextIPs(subnetName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			for i, ip := range ips {
				gomega.Expect(ip.String()).To(gomega.Equal(expectedIPs[i]))
			}
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = allocator.AllocateIPPerSubnet(subnetName, ovntest.MustParseIPNets(expectedIPs...))
			gomega.Expect(err).To(gomega.MatchError(ipam.ErrAllocated))
		})

	})

	// Reserved subnets test cases
	ginkgo.It("reserves subnets correctly and allows specific allocation", func() {
		subnets := []string{
			"10.1.1.0/24",
		}
		reservedSubnets := []string{
			"10.1.1.16/28", // Reserve 10.1.1.16-31
		}
		expectedIP := "10.1.1.1/24"

		err := allocator.AddOrUpdateSubnet(SubnetConfig{
			Name:            subnetName,
			Subnets:         ovntest.MustParseIPNets(subnets...),
			ReservedSubnets: ovntest.MustParseIPNets(reservedSubnets...),
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ips, err := allocator.AllocateNextIPs(subnetName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ips).To(gomega.HaveLen(1))
		gomega.Expect(ips[0].String()).To(gomega.Equal(expectedIP))

		// Should be able to allocate the first IP from the reserved range
		reservedIPs, err := util.ParseIPNets([]string{"10.1.1.16/24"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = allocator.AllocateIPPerSubnet(subnetName, reservedIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Should be able to allocate the last IP from the reserved range
		reservedIPs, err = util.ParseIPNets([]string{"10.1.1.31/24"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = allocator.AllocateIPPerSubnet(subnetName, reservedIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Should not be able to allocate an IP that is already allocated
		err = allocator.AllocateIPPerSubnet(subnetName, reservedIPs)
		gomega.Expect(err).To(gomega.MatchError(ipam.ErrAllocated))
	})

	ginkgo.It("handles reserved together with exclude subnets correctly", func() {
		subnets := []string{
			"10.1.1.0/24",
		}
		reservedSubnets := []string{
			"10.1.1.0/28",   // Reserve 10.1.1.0-15
			"10.1.1.240/28", // Reserve 10.1.1.240-255
		}
		excludeSubnets := []string{
			"10.1.1.200/29", // Exclude 10.1.1.200-207
		}
		expectedIP := "10.1.1.16/24"

		err := allocator.AddOrUpdateSubnet(SubnetConfig{
			Name:            subnetName,
			Subnets:         ovntest.MustParseIPNets(subnets...),
			ExcludeSubnets:  ovntest.MustParseIPNets(excludeSubnets...),
			ReservedSubnets: ovntest.MustParseIPNets(reservedSubnets...),
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ips, err := allocator.AllocateNextIPs(subnetName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ips).To(gomega.HaveLen(1))
		gomega.Expect(ips[0].String()).To(gomega.Equal(expectedIP))

		// Should be able to allocate from reserved range but not excluded range
		reservedIPs, err := util.ParseIPNets([]string{"10.1.1.5/24"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = allocator.AllocateIPPerSubnet(subnetName, reservedIPs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Should NOT be able to allocate from excluded range
		excludedIPs, err := util.ParseIPNets([]string{"10.1.1.202/24"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = allocator.AllocateIPPerSubnet(subnetName, excludedIPs)
		gomega.Expect(err).To(gomega.MatchError(ipam.ErrAllocated))

		// Should not be able to allocate the network IP
		reservedIPs, err = util.ParseIPNets([]string{"10.1.1.0/24"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = allocator.AllocateIPPerSubnet(subnetName, reservedIPs)
		gomega.Expect(err).To(gomega.HaveOccurred())

		// Should not be able to allocate the broadcast IP
		reservedIPs, err = util.ParseIPNets([]string{"10.1.1.255/24"})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = allocator.AllocateIPPerSubnet(subnetName, reservedIPs)
		gomega.Expect(err).To(gomega.HaveOccurred())
	})

})

func TestSubnetIPAllocator(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Subnet IP allocator Operations Suite")
}
