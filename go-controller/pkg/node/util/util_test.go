package util

import (
	"net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("node util tests", func() {
	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
	})

	Context("GetDPUHostPrimaryIPAddresses", func() {

		It("returns Gateway IP/Subnet for kubernetes node IP", func() {
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.0.11")
			expectedGwSubnet := []*net.IPNet{
				{IP: nodeIP, Mask: net.CIDRMask(24, 32)},
			}
			gwSubnet, err := GetDPUHostPrimaryIPAddresses(nodeIP, []*net.IPNet{dpuSubnet})
			Expect(err).ToNot(HaveOccurred())
			Expect(gwSubnet).To(Equal(expectedGwSubnet))
		})

		It("Fails if node IP is not in host subnets", func() {
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.1.11")
			_, err := GetDPUHostPrimaryIPAddresses(nodeIP, []*net.IPNet{dpuSubnet})
			Expect(err).To(HaveOccurred())
		})

		It("returns node IP with config.Gateway.RouterSubnet subnet", func() {
			config.Gateway.RouterSubnet = "10.1.0.0/16"
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.1.0.11")
			expectedGwSubnet := []*net.IPNet{
				{IP: nodeIP, Mask: net.CIDRMask(16, 32)},
			}
			gwSubnet, err := GetDPUHostPrimaryIPAddresses(nodeIP, []*net.IPNet{dpuSubnet})
			Expect(err).ToNot(HaveOccurred())
			Expect(gwSubnet).To(Equal(expectedGwSubnet))
		})

		It("Fails if node IP is not in config.Gateway.RouterSubnet subnet", func() {
			config.Gateway.RouterSubnet = "10.1.0.0/16"
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.0.11")
			_, err := GetDPUHostPrimaryIPAddresses(nodeIP, []*net.IPNet{dpuSubnet})
			Expect(err).To(HaveOccurred())
		})
	})
})
