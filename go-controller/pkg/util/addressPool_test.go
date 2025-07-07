package util

import (
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NetworkPoolManager", func() {
	var (
		poolManager *NetworkPoolManager
		testNetwork string
		testIP1     *net.IPNet
		testIP2     *net.IPNet
		testMAC1    net.HardwareAddr
		testMAC2    net.HardwareAddr
		ownerID1    string
		ownerID2    string
	)

	BeforeEach(func() {
		poolManager = NewNetworkPoolManager()
		testNetwork = "test-network"
		testIP1 = &net.IPNet{IP: net.ParseIP("192.168.1.10"), Mask: net.CIDRMask(24, 32)}
		testIP2 = &net.IPNet{IP: net.ParseIP("2001:db8::20"), Mask: net.CIDRMask(128, 128)}
		ownerID1 = "namespace1/pod1"
		ownerID2 = "namespace2/pod2"

		var err error
		testMAC1, err = net.ParseMAC("aa:bb:cc:dd:ee:f1")
		Expect(err).NotTo(HaveOccurred())
		testMAC2, err = net.ParseMAC("aa:bb:cc:dd:ee:f2")
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("IP Pool Operations", func() {
		Context("when adding IPs to pool", func() {
			It("should handle empty slice gracefully", func() {
				poolManager.AddIPsToPool(testNetwork, []*net.IPNet{}, ownerID1)

				ipCount, _ := poolManager.GetPoolStats(testNetwork)
				Expect(ipCount).To(BeZero())
			})

			It("should handle slice with all nil entries", func() {
				ips := []*net.IPNet{nil, nil, nil}
				poolManager.AddIPsToPool(testNetwork, ips, ownerID1)

				ipCount, _ := poolManager.GetPoolStats(testNetwork)
				Expect(ipCount).To(BeZero())
			})

			It("should successfully add IP to pool", func() {
				poolManager.AddIPsToPool(testNetwork, []*net.IPNet{testIP1}, ownerID1)

				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1}, ownerID2)
				Expect(conflictingIPs).To(HaveLen(1))
			})

			It("should handle multiple IPs in same network", func() {
				poolManager.AddIPsToPool(testNetwork, []*net.IPNet{testIP1}, ownerID1)
				poolManager.AddIPsToPool(testNetwork, []*net.IPNet{testIP2}, ownerID1)

				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1, testIP2}, ownerID2)
				Expect(conflictingIPs).To(HaveLen(2))

				ipCount, _ := poolManager.GetPoolStats(testNetwork)
				Expect(ipCount).To(Equal(2))
			})

			It("should successfully add multiple IPs in single call", func() {
				ips := []*net.IPNet{testIP1, testIP2}
				poolManager.AddIPsToPool(testNetwork, ips, ownerID1)

				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1, testIP2}, ownerID2)
				Expect(conflictingIPs).To(HaveLen(2))

				ipCount, _ := poolManager.GetPoolStats(testNetwork)
				Expect(ipCount).To(Equal(2))
			})

			It("should allow adding same IP multiple times without duplicates", func() {
				poolManager.AddIPsToPool(testNetwork, []*net.IPNet{testIP1}, ownerID1)
				poolManager.AddIPsToPool(testNetwork, []*net.IPNet{testIP1}, ownerID1)

				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1}, ownerID2)
				Expect(conflictingIPs).To(HaveLen(1))

				// Pool should still have only 1 IP
				ipCount, _ := poolManager.GetPoolStats(testNetwork)
				Expect(ipCount).To(Equal(1))
			})

			It("should not return IP conflict for entry of same owner", func() {
				poolManager.AddIPsToPool(testNetwork, []*net.IPNet{testIP1}, ownerID1)

				// Same owner should not get conflict
				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1}, ownerID1)
				Expect(conflictingIPs).To(BeEmpty())

				// Different owner should get conflict
				conflictingIPs = poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1}, ownerID2)
				Expect(conflictingIPs).To(HaveLen(1))
			})

			It("should isolate IPs between different networks", func() {
				anotherNetwork := "another-network"

				poolManager.AddIPsToPool(testNetwork, []*net.IPNet{testIP1}, ownerID1)
				poolManager.AddIPsToPool(anotherNetwork, []*net.IPNet{testIP2}, ownerID1)

				// IP1 should conflict only in test-network
				conflictingIPs1InTest := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1}, ownerID2)
				Expect(conflictingIPs1InTest).To(HaveLen(1))

				conflictingIPs1InAnother := poolManager.CheckIPConflicts(anotherNetwork, []*net.IPNet{testIP1}, ownerID2)
				Expect(conflictingIPs1InAnother).To(BeEmpty())

				// IP2 should conflict only in another-network
				conflictingIPs2InTest := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP2}, ownerID2)
				conflictingIPs2InAnother := poolManager.CheckIPConflicts(anotherNetwork, []*net.IPNet{testIP2}, ownerID2)
				Expect(conflictingIPs2InTest).To(BeEmpty())
				Expect(conflictingIPs2InAnother).To(HaveLen(1))
			})

			It("should handle duplicates within the same slice", func() {
				ips := []*net.IPNet{testIP1, testIP2, testIP1} // duplicate
				poolManager.AddIPsToPool(testNetwork, ips, ownerID1)

				// Should only have 2 unique IPs
				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1, testIP2}, ownerID2)
				Expect(conflictingIPs).To(HaveLen(2))

				ipCount, _ := poolManager.GetPoolStats(testNetwork)
				Expect(ipCount).To(Equal(2))
			})
		})

		Context("when removing IPs from pool", func() {
			It("should successfully remove multiple IPs in single call", func() {
				// Add multiple IPs
				ips := []*net.IPNet{testIP1, testIP2}
				poolManager.AddIPsToPool(testNetwork, ips, ownerID1)

				// Remove all IPs
				poolManager.RemoveIPsFromPool(testNetwork, ips)

				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1, testIP2}, ownerID2)
				Expect(conflictingIPs).To(BeEmpty())

				ipCount, _ := poolManager.GetPoolStats(testNetwork)
				Expect(ipCount).To(BeZero())
			})

			It("should handle removing subset of IPs", func() {
				// Add multiple IPs
				allIPs := []*net.IPNet{testIP1, testIP2}
				poolManager.AddIPsToPool(testNetwork, allIPs, ownerID1)

				// Remove only one IP
				removeIPs := []*net.IPNet{testIP1}
				poolManager.RemoveIPsFromPool(testNetwork, removeIPs)

				// Check that only testIP2 remains
				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1, testIP2}, ownerID2)
				Expect(conflictingIPs).To(HaveLen(1))
				Expect(conflictingIPs[0]).To(Equal(testIP2.IP))

				ipCount, _ := poolManager.GetPoolStats(testNetwork)
				Expect(ipCount).To(Equal(1))
			})

			It("should handle empty slice gracefully", func() {
				// Add an IP first
				poolManager.AddIPsToPool(testNetwork, []*net.IPNet{testIP1}, ownerID1)

				// Remove empty slice - should not affect pool
				poolManager.RemoveIPsFromPool(testNetwork, []*net.IPNet{})

				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1}, ownerID2)
				Expect(conflictingIPs).To(HaveLen(1))

				ipCount, _ := poolManager.GetPoolStats(testNetwork)
				Expect(ipCount).To(Equal(1))
			})

			It("should handle slice with nil entries", func() {
				// Add multiple IPs
				ips := []*net.IPNet{testIP1, testIP2}
				poolManager.AddIPsToPool(testNetwork, ips, ownerID1)

				// Remove with nil entries
				removeIPs := []*net.IPNet{testIP1, nil}
				poolManager.RemoveIPsFromPool(testNetwork, removeIPs)

				// Only testIP2 should remain
				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1, testIP2}, ownerID2)
				Expect(conflictingIPs).To(HaveLen(1))
				Expect(conflictingIPs[0]).To(Equal(testIP2.IP))

				ipCount, _ := poolManager.GetPoolStats(testNetwork)
				Expect(ipCount).To(Equal(1))
			})

			It("should handle removing non-existent IPs gracefully", func() {
				// Add one IP
				poolManager.AddIPsToPool(testNetwork, []*net.IPNet{testIP1}, ownerID1)

				// Try to remove a different IP
				poolManager.RemoveIPsFromPool(testNetwork, []*net.IPNet{testIP2})

				// Original IP should still be there
				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1}, ownerID2)
				Expect(conflictingIPs).To(HaveLen(1))

				ipCount, _ := poolManager.GetPoolStats(testNetwork)
				Expect(ipCount).To(Equal(1))
			})
		})

		Context("when checking IP conflicts", func() {
			It("should return empty list for non-existent network", func() {
				conflictingIPs := poolManager.CheckIPConflicts("non-existent-network", []*net.IPNet{testIP1}, ownerID1)
				Expect(conflictingIPs).To(BeEmpty())
			})

			It("should handle nil IP gracefully", func() {
				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{{IP: nil}}, ownerID1)
				Expect(conflictingIPs).To(BeEmpty())
			})

			It("should return empty list for empty pool", func() {
				conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1}, ownerID1)
				Expect(conflictingIPs).To(BeEmpty())
			})
		})
	})

	Describe("MAC Pool Operations", func() {
		Context("when adding MACs to pool", func() {
			It("should successfully add MAC to pool", func() {
				poolManager.AddMACToPool(testNetwork, testMAC1, ownerID1)

				Expect(poolManager.IsMACConflict(testNetwork, testMAC1, ownerID2)).To(BeTrue())
			})

			It("should handle nil MAC gracefully", func() {
				poolManager.AddMACToPool(testNetwork, nil, ownerID1)

				// Should not crash and pool should remain empty
				_, macCount := poolManager.GetPoolStats(testNetwork)
				Expect(macCount).To(BeZero())
			})

			It("should allow adding same MAC multiple times without duplicates", func() {
				poolManager.AddMACToPool(testNetwork, testMAC1, ownerID1)
				poolManager.AddMACToPool(testNetwork, testMAC1, ownerID1)

				Expect(poolManager.IsMACConflict(testNetwork, testMAC1, ownerID2)).To(BeTrue())

				// Pool should still have only 1 MAC
				_, macCount := poolManager.GetPoolStats(testNetwork)
				Expect(macCount).To(Equal(1))
			})

			It("should not return MAC conflict for entry of same owner", func() {
				poolManager.AddMACToPool(testNetwork, testMAC1, ownerID1)

				// Same owner should not get conflict
				Expect(poolManager.IsMACConflict(testNetwork, testMAC1, ownerID1)).To(BeFalse())

				// Different owner should get conflict
				Expect(poolManager.IsMACConflict(testNetwork, testMAC1, ownerID2)).To(BeTrue())
			})

			It("should handle multiple MACs in same network", func() {
				poolManager.AddMACToPool(testNetwork, testMAC1, ownerID1)
				poolManager.AddMACToPool(testNetwork, testMAC2, ownerID1)

				Expect(poolManager.IsMACConflict(testNetwork, testMAC1, ownerID2)).To(BeTrue())
				Expect(poolManager.IsMACConflict(testNetwork, testMAC2, ownerID2)).To(BeTrue())

				_, macCount := poolManager.GetPoolStats(testNetwork)
				Expect(macCount).To(Equal(2))
			})

			It("should isolate MACs between different networks", func() {
				anotherNetwork := "another-network"

				poolManager.AddMACToPool(testNetwork, testMAC1, ownerID1)
				poolManager.AddMACToPool(anotherNetwork, testMAC2, ownerID1)

				// MAC1 should conflict only in test-network
				Expect(poolManager.IsMACConflict(testNetwork, testMAC1, ownerID2)).To(BeTrue())
				Expect(poolManager.IsMACConflict(anotherNetwork, testMAC1, ownerID2)).To(BeFalse())

				// MAC2 should conflict only in another-network
				Expect(poolManager.IsMACConflict(testNetwork, testMAC2, ownerID2)).To(BeFalse())
				Expect(poolManager.IsMACConflict(anotherNetwork, testMAC2, ownerID2)).To(BeTrue())
			})

		})

		Context("when removing MACs from pool", func() {
			It("should successfully remove existing MAC", func() {
				poolManager.AddMACToPool(testNetwork, testMAC1, ownerID1)
				poolManager.RemoveMACFromPool(testNetwork, testMAC1)

				Expect(poolManager.IsMACConflict(testNetwork, testMAC1, ownerID2)).To(BeFalse())

				_, macCount := poolManager.GetPoolStats(testNetwork)
				Expect(macCount).To(BeZero())
			})

			It("should handle removing non-existent MAC gracefully", func() {
				poolManager.RemoveMACFromPool(testNetwork, testMAC1)

				// Should not crash
				Expect(poolManager.IsMACConflict(testNetwork, testMAC1, ownerID1)).To(BeFalse())
			})

			It("should handle nil MAC gracefully", func() {
				poolManager.RemoveMACFromPool(testNetwork, nil)

				// Should not crash
				_, macCount := poolManager.GetPoolStats(testNetwork)
				Expect(macCount).To(BeZero())
			})

			It("should handle removing from non-existent network gracefully", func() {
				poolManager.RemoveMACFromPool("non-existent-network", testMAC1)

				// Should not crash
				Expect(poolManager.IsMACConflict("non-existent-network", testMAC1, ownerID1)).To(BeFalse())
			})
		})

		Context("when checking MAC conflicts", func() {
			It("should return false for non-existent network", func() {
				Expect(poolManager.IsMACConflict("non-existent-network", testMAC1, ownerID1)).To(BeFalse())
			})

			It("should handle nil MAC gracefully", func() {
				Expect(poolManager.IsMACConflict(testNetwork, nil, ownerID1)).To(BeFalse())
			})

			It("should return false for empty pool", func() {
				Expect(poolManager.IsMACConflict(testNetwork, testMAC1, ownerID1)).To(BeFalse())
			})
		})
	})

	Describe("Mixed IP and MAC operations", func() {
		It("should handle IP and MAC operations independently", func() {
			poolManager.AddIPsToPool(testNetwork, []*net.IPNet{testIP1}, ownerID1)
			poolManager.AddMACToPool(testNetwork, testMAC1, ownerID1)

			conflictingIPs := poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1}, ownerID2)
			Expect(conflictingIPs).To(HaveLen(1))
			Expect(poolManager.IsMACConflict(testNetwork, testMAC1, ownerID2)).To(BeTrue())

			ipCount, macCount := poolManager.GetPoolStats(testNetwork)
			Expect(ipCount).To(Equal(1))
			Expect(macCount).To(Equal(1))

			// Remove IP, MAC should remain
			poolManager.RemoveIPsFromPool(testNetwork, []*net.IPNet{testIP1})
			conflictingIPs = poolManager.CheckIPConflicts(testNetwork, []*net.IPNet{testIP1}, ownerID2)
			Expect(conflictingIPs).To(BeEmpty())
			Expect(poolManager.IsMACConflict(testNetwork, testMAC1, ownerID2)).To(BeTrue())

			ipCount, macCount = poolManager.GetPoolStats(testNetwork)
			Expect(ipCount).To(BeZero())
			Expect(macCount).To(Equal(1))
		})
	})

	Describe("GetPoolStats", func() {
		It("should return 0,0 for non-existent network", func() {
			ipCount, macCount := poolManager.GetPoolStats("non-existent-network")
			Expect(ipCount).To(BeZero())
			Expect(macCount).To(BeZero())
		})

		It("should return correct counts for mixed pools", func() {
			poolManager.AddIPsToPool(testNetwork, []*net.IPNet{testIP1, testIP2}, ownerID1)
			poolManager.AddMACToPool(testNetwork, testMAC1, ownerID1)

			ipCount, macCount := poolManager.GetPoolStats(testNetwork)
			Expect(ipCount).To(Equal(2))
			Expect(macCount).To(Equal(1))
		})
	})
})
