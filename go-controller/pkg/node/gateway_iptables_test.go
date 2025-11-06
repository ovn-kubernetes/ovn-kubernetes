package node

import (
	"fmt"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/coreos/go-iptables/iptables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	nodeipt "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Gateway IPTables", func() {
	var testNS ns.NetNS
	var iptCtrl *nodeipt.Controller

	BeforeEach(func() {
		if ovntest.NoRoot() {
			Skip("Test requires root privileges")
		}

		var err error
		runtime.LockOSThread()
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())

		// Start iptables controller
		iptCtrl = nodeipt.NewController()
		stopCh := make(chan struct{})
		wg := &sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = testNS.Do(func(ns.NetNS) error {
				iptCtrl.Run(stopCh, 50*time.Millisecond)
				return nil
			})
		}()

		DeferCleanup(func() {
			close(stopCh)
			wg.Wait()
			Expect(testNS.Close()).To(Succeed())
			Expect(testutils.UnmountNS(testNS)).To(Succeed())
			runtime.UnlockOSThread()
		})
	})

	Context("configureForwardingRules", func() {
		var originalDisableForwarding bool
		var originalClusterSubnets []config.CIDRNetworkEntry
		var originalServiceCIDRs []*net.IPNet
		var originalIPv4Mode bool
		var originalIPv6Mode bool
		var originalMasqueradeIP net.IP

		BeforeEach(func() {
			// Save original config
			originalDisableForwarding = config.Gateway.DisableForwarding
			originalClusterSubnets = config.Default.ClusterSubnets
			originalServiceCIDRs = config.Kubernetes.ServiceCIDRs
			originalIPv4Mode = config.IPv4Mode
			originalIPv6Mode = config.IPv6Mode
			if config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP != nil {
				originalMasqueradeIP = append(net.IP(nil), config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP...)
			} else {
				originalMasqueradeIP = nil
			}

			// Setup test config
			config.IPv4Mode = true
			config.IPv6Mode = false
		})

		AfterEach(func() {
			// Restore original config
			config.Gateway.DisableForwarding = originalDisableForwarding
			config.Default.ClusterSubnets = originalClusterSubnets
			config.Kubernetes.ServiceCIDRs = originalServiceCIDRs
			config.IPv4Mode = originalIPv4Mode
			config.IPv6Mode = originalIPv6Mode
			config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP = append(net.IP(nil), originalMasqueradeIP...)
		})

		It("should add FORWARD rules when DisableForwarding is true", func() {
			// Setup test configuration
			config.Gateway.DisableForwarding = true
			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
				{CIDR: ovntest.MustParseIPNet("10.128.0.0/14")},
			}
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{
				ovntest.MustParseIPNet("172.30.0.0/16"),
			}
			config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP = net.ParseIP("169.254.0.1")

			err := testNS.Do(func(ns.NetNS) error {
				return configureForwardingRules()
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify rules were added
			Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
					if err != nil {
						return err
					}

					// Check cluster subnet rules
					exists, err := ipt.Exists("filter", "FORWARD", "-s", "10.128.0.0/14", "-j", "ACCEPT")
					if err != nil {
						return err
					}
					if !exists {
						return fmt.Errorf("cluster subnet source rule not found")
					}

					exists, err = ipt.Exists("filter", "FORWARD", "-d", "10.128.0.0/14", "-j", "ACCEPT")
					if err != nil {
						return err
					}
					if !exists {
						return fmt.Errorf("cluster subnet dest rule not found")
					}

					// Check service CIDR rules
					exists, err = ipt.Exists("filter", "FORWARD", "-s", "172.30.0.0/16", "-j", "ACCEPT")
					if err != nil {
						return err
					}
					if !exists {
						return fmt.Errorf("service CIDR source rule not found")
					}

					exists, err = ipt.Exists("filter", "FORWARD", "-d", "172.30.0.0/16", "-j", "ACCEPT")
					if err != nil {
						return err
					}
					if !exists {
						return fmt.Errorf("service CIDR dest rule not found")
					}

					// Check masquerade IP rules
					exists, err = ipt.Exists("filter", "FORWARD", "-s", "169.254.0.1", "-j", "ACCEPT")
					if err != nil {
						return err
					}
					if !exists {
						return fmt.Errorf("masquerade IP source rule not found")
					}

					exists, err = ipt.Exists("filter", "FORWARD", "-d", "169.254.0.1", "-j", "ACCEPT")
					if err != nil {
						return err
					}
					if !exists {
						return fmt.Errorf("masquerade IP dest rule not found")
					}

					return nil
				})
			}, 2*time.Second).Should(Succeed())
		})

		It("should remove FORWARD rules when DisableForwarding is false", func() {
			// Setup test configuration
			config.Gateway.DisableForwarding = true
			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
				{CIDR: ovntest.MustParseIPNet("10.128.0.0/14")},
			}
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{
				ovntest.MustParseIPNet("172.30.0.0/16"),
			}
			config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP = net.ParseIP("169.254.0.1")

			// First add the rules
			err := testNS.Do(func(ns.NetNS) error {
				return configureForwardingRules()
			})
			Expect(err).NotTo(HaveOccurred())

			// Wait for rules to be added
			Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
					if err != nil {
						return err
					}

					// Verify at least one rule exists
					exists, err := ipt.Exists("filter", "FORWARD", "-s", "10.128.0.0/14", "-j", "ACCEPT")
					if err != nil {
						return err
					}
					if !exists {
						return fmt.Errorf("waiting for rules to be added")
					}
					return nil
				})
			}, 2*time.Second).Should(Succeed())

			// Now change config to remove rules
			config.Gateway.DisableForwarding = false

			err = testNS.Do(func(ns.NetNS) error {
				return configureForwardingRules()
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify rules were removed
			Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
					if err != nil {
						return err
					}

					// Check that cluster subnet rules are removed
					exists, err := ipt.Exists("filter", "FORWARD", "-s", "10.128.0.0/14", "-j", "ACCEPT")
					if err != nil {
						return err
					}
					if exists {
						return fmt.Errorf("cluster subnet source rule should be removed")
					}

					exists, err = ipt.Exists("filter", "FORWARD", "-d", "10.128.0.0/14", "-j", "ACCEPT")
					if err != nil {
						return err
					}
					if exists {
						return fmt.Errorf("cluster subnet dest rule should be removed")
					}

					return nil
				})
			}, 2*time.Second).Should(Succeed())
		})

		It("should handle multiple cluster subnets and service CIDRs", func() {
			// Setup test configuration with multiple CIDRs
			config.Gateway.DisableForwarding = true
			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
				{CIDR: ovntest.MustParseIPNet("10.128.0.0/14")},
				{CIDR: ovntest.MustParseIPNet("10.132.0.0/14")},
			}
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{
				ovntest.MustParseIPNet("172.30.0.0/16"),
				ovntest.MustParseIPNet("172.31.0.0/16"),
			}
			config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP = net.ParseIP("169.254.0.1")

			err := testNS.Do(func(ns.NetNS) error {
				return configureForwardingRules()
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify all rules were added
			Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
					if err != nil {
						return err
					}

					// Check all cluster subnet rules
					for _, subnet := range []string{"10.128.0.0/14", "10.132.0.0/14"} {
						exists, err := ipt.Exists("filter", "FORWARD", "-s", subnet, "-j", "ACCEPT")
						if err != nil {
							return err
						}
						if !exists {
							return fmt.Errorf("cluster subnet source rule for %s not found", subnet)
						}

						exists, err = ipt.Exists("filter", "FORWARD", "-d", subnet, "-j", "ACCEPT")
						if err != nil {
							return err
						}
						if !exists {
							return fmt.Errorf("cluster subnet dest rule for %s not found", subnet)
						}
					}

					// Check all service CIDR rules
					for _, cidr := range []string{"172.30.0.0/16", "172.31.0.0/16"} {
						exists, err := ipt.Exists("filter", "FORWARD", "-s", cidr, "-j", "ACCEPT")
						if err != nil {
							return err
						}
						if !exists {
							return fmt.Errorf("service CIDR source rule for %s not found", cidr)
						}

						exists, err = ipt.Exists("filter", "FORWARD", "-d", cidr, "-j", "ACCEPT")
						if err != nil {
							return err
						}
						if !exists {
							return fmt.Errorf("service CIDR dest rule for %s not found", cidr)
						}
					}

					return nil
				})
			}, 2*time.Second).Should(Succeed())
		})
	})
})
