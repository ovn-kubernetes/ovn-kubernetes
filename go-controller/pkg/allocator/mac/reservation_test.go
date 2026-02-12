package mac_test

import (
	"fmt"
	"net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/mac"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ReservationManager", func() {
	const owner1 = "namespace1/pod1"
	const owner2 = "namespace2/pod2"

	var testMgr *mac.ReservationManager
	var mac1 net.HardwareAddr

	BeforeEach(func() {
		var err error
		mac1, err = net.ParseMAC("aa:bb:cc:dd:ee:f1")
		Expect(err).NotTo(HaveOccurred())

		testMgr = mac.NewManager()
	})

	Context("reserve", func() {
		It("should fail on repeated reservation for the same owner", func() {
			Expect(testMgr.Reserve(owner1, mac1)).To(Succeed())
			Expect(testMgr.Reserve(owner1, mac1)).To(MatchError(mac.ErrMACReserved))
		})

		It("should fail reserve existing MAC for different owner", func() {
			Expect(testMgr.Reserve(owner1, mac1)).To(Succeed())
			Expect(testMgr.Reserve(owner2, mac1)).To(MatchError(mac.ErrReserveMACConflict),
				"different owner should raise a conflict")
		})

		It("should succeed", func() {
			for i := 0; i < 5; i++ {
				owner := fmt.Sprintf("ns%d/test", i)
				mac := net.HardwareAddr(fmt.Sprintf("02:02:02:02:02:0%d", i))
				Expect(testMgr.Reserve(owner, mac)).To(Succeed())
			}
		})
	})

	Context("release a reserved mac", func() {
		BeforeEach(func() {
			By("reserve mac1 for owner1")
			Expect(testMgr.Reserve(owner1, mac1)).To(Succeed())
		})

		It("should not release MAC given wrong owner", func() {
			Expect(testMgr.Release(owner2, mac1)).To(MatchError(mac.ErrReleaseMismatchOwner))

			Expect(testMgr.Reserve(owner2, mac1)).To(MatchError(mac.ErrReserveMACConflict),
				"mac1 reserved for owner1, it should raise a conflict")
		})

		It("should succeed", func() {
			Expect(testMgr.Release(owner1, mac1)).To(Succeed())

			Expect(testMgr.Reserve(owner2, mac1)).To(Succeed(),
				"reserving mac1 for different owner should not raise a conflict")
		})
	})

	It("release non reserved mac should succeed (no-op)", func() {
		Expect(testMgr.Release(owner1, mac1)).To(Succeed())
	})
})
