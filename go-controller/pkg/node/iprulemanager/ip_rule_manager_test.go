// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package iprulemanager

import (
	"fmt"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/vishvananda/netlink"

	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
)

func TestIPRuleEquality(t *testing.T) {
	v4Rule := IPRuleFromNetlinkRule(&netlink.Rule{
		Priority: 2000,
		Table:    1020,
		Family:   netlink.FAMILY_V4,
		Mark:     0x1003,
	})
	v6Rule := IPRuleFromNetlinkRule(&netlink.Rule{
		Priority: 2000,
		Table:    1020,
		Family:   netlink.FAMILY_V6,
		Mark:     0x1003,
	})

	if v4Rule == v6Rule {
		t.Fatalf("expected IPv4 and IPv6 rules with the same mark/table to be different")
	}
}

func isRuleInSlice(rules []netlink.Rule, candidate IPRule) bool {
	for _, r := range rules {
		if IPRuleFromNetlinkRule(&r) == candidate {
			return true
		}
	}
	return false
}

// FIXME(mk) - Within GH VM, if I need to create a new NetNs. I see the following error:
// "failed to create new network namespace: mount --make-rshared /run/user/1001/netns failed: "operation not permitted""
var _ = ginkgo.XDescribe("IP Rule Manager", func() {
	var stopCh chan struct{}
	var wg *sync.WaitGroup
	var testNS ns.NetNS
	var c *Controller
	var _, testIPNet, _ = net.ParseCIDR("192.168.1.5/24")
	ruleWithDst := netlink.NewRule()
	ruleWithDst.Priority = 3000
	ruleWithDst.Table = 254
	ruleWithDst.Dst = testIPNet
	ruleWithSrc := netlink.NewRule()
	ruleWithSrc.Priority = 3000
	ruleWithSrc.Table = 254
	ruleWithSrc.Src = testIPNet

	ipRuleWithDst := IPRuleFromNetlinkRule(ruleWithDst)
	ipRuleWithSrc := IPRuleFromNetlinkRule(ruleWithSrc)

	defer ginkgo.GinkgoRecover()
	if ovntest.NoRoot() {
		ginkgo.Skip("Test requires root privileges")
	}

	ginkgo.BeforeEach(func() {
		var err error
		runtime.LockOSThread()
		testNS, err = testutils.NewNS()
		gomega.Expect(err).ShouldNot(gomega.HaveOccurred())

		wg = &sync.WaitGroup{}
		stopCh = make(chan struct{})
		wg.Add(1)
		c = NewController(true, true)
		go func() {
			defer ginkgo.GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				c.Run(stopCh, time.Millisecond*50)
				return nil
			})
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		}()
	})

	ginkgo.AfterEach(func() {
		defer runtime.UnlockOSThread()
		close(stopCh)
		wg.Wait()
		gomega.Expect(testNS.Close()).ShouldNot(gomega.HaveOccurred())
		gomega.Expect(testutils.UnmountNS(testNS)).To(gomega.Succeed())
	})

	ginkgo.Context("Add rule", func() {
		ginkgo.It("ensure rule exist", func() {
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					return c.Add(ipRuleWithDst)
				})
			}()).Should(gomega.Succeed())

			gomega.Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					rules, err := netlink.RuleList(netlink.FAMILY_ALL)
					if err != nil {
						return err
					}
					if !isRuleInSlice(rules, ipRuleWithDst) {
						return fmt.Errorf("failed to find rule %q", ipRuleWithDst.String())
					}
					return nil
				})
			}).WithTimeout(time.Second).Should(gomega.Succeed())
		})

		ginkgo.It("ensure rule is restored if it is removed", func() {
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					return c.Add(ipRuleWithDst)
				})
			}()).Should(gomega.Succeed())

			gomega.Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					rules, err := netlink.RuleList(netlink.FAMILY_ALL)
					if err != nil {
						return err
					}
					if !isRuleInSlice(rules, ipRuleWithDst) {
						return fmt.Errorf("failed to find rule %q", ipRuleWithDst.String())
					}
					return nil
				})
			}).WithTimeout(time.Second).Should(gomega.Succeed())
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					return netlink.RuleDel(ruleWithDst)
				})
			}()).Should(gomega.Succeed())
			// check that rule is restored
			gomega.Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					rules, err := netlink.RuleList(netlink.FAMILY_ALL)
					if err != nil {
						return err
					}
					if !isRuleInSlice(rules, ipRuleWithDst) {
						return fmt.Errorf("failed to find rule %q", ipRuleWithDst.String())
					}
					return nil
				})
			}).WithTimeout(time.Second).Should(gomega.Succeed())
		})

		ginkgo.It("ensure multiple rules are restored if they're removed", func() {
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					return c.Add(ipRuleWithDst)
				})
			}()).Should(gomega.Succeed())
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					return c.Add(ipRuleWithSrc)
				})
			}()).Should(gomega.Succeed())

			gomega.Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					rules, err := netlink.RuleList(netlink.FAMILY_ALL)
					if err != nil {
						return err
					}
					if !isRuleInSlice(rules, ipRuleWithDst) {
						return fmt.Errorf("failed to find rule with dst")
					}
					if !isRuleInSlice(rules, ipRuleWithSrc) {
						return fmt.Errorf("failed to find rule with src")
					}
					return nil
				})
			}).WithTimeout(time.Second).Should(gomega.Succeed())
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					return netlink.RuleDel(ruleWithDst)
				})
			}()).Should(gomega.Succeed())

			gomega.Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					rules, err := netlink.RuleList(netlink.FAMILY_ALL)
					if err != nil {
						return err
					}
					if !isRuleInSlice(rules, ipRuleWithDst) {
						return fmt.Errorf("failed to find rule %s", ipRuleWithDst.String())
					}
					return nil
				})
			}).WithTimeout(time.Second).Should(gomega.Succeed())
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					return netlink.RuleDel(ruleWithSrc)
				})
			}()).Should(gomega.Succeed())

			gomega.Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					rules, err := netlink.RuleList(netlink.FAMILY_ALL)
					if err != nil {
						return err
					}
					if !isRuleInSlice(rules, ipRuleWithSrc) {
						return fmt.Errorf("failed to find rule %s", ipRuleWithSrc.String())
					}
					return nil
				})
			}).WithTimeout(time.Second).Should(gomega.Succeed())
		})
	})

	ginkgo.Context("AddNodeIPFwMarkRule", func() {
		ginkgo.DescribeTable("installs fwmark bypass rule for the given IP family",
			func(family int) {
				expectedRule := IPRule{
					Priority: nodetypes.FwMarkBypassPriority,
					Mark:     nodetypes.OvnKubeNodeSNATMarkValue,
					Table:    254,
					Family:   family,
				}

				gomega.Expect(func() error {
					return testNS.Do(func(ns.NetNS) error {
						return c.AddNodeIPFwMarkRule(family)
					})
				}()).Should(gomega.Succeed())

				gomega.Eventually(func() error {
					return testNS.Do(func(ns.NetNS) error {
						rules, err := netlink.RuleList(family)
						if err != nil {
							return err
						}
						if !isRuleInSlice(rules, expectedRule) {
							return fmt.Errorf("fwmark bypass rule not found for family %d", family)
						}
						return nil
					})
				}).WithTimeout(time.Second).Should(gomega.Succeed())
			},
			ginkgo.Entry("IPv4", netlink.FAMILY_V4),
			ginkgo.Entry("IPv6", netlink.FAMILY_V6),
		)
	})

	ginkgo.Context("Del rule", func() {
		ginkgo.It("doesn't fail when no rule to delete", func() {
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					return c.Delete(ipRuleWithDst)
				})
			}()).Should(gomega.Succeed())
		})

		ginkgo.It("deletes a rule", func() {
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					if err := c.Add(ipRuleWithDst); err != nil {
						return err
					}
					if err := c.Delete(ipRuleWithDst); err != nil {
						return err
					}
					return nil
				})
			}()).Should(gomega.Succeed())

			gomega.Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					rules, err := netlink.RuleList(netlink.FAMILY_ALL)
					if err != nil {
						return err
					}
					if isRuleInSlice(rules, ipRuleWithDst) {
						return fmt.Errorf("expected rule (%s) to be deleted but it was found", ipRuleWithDst.String())
					}
					return nil
				})
			}).WithTimeout(time.Second).Should(gomega.Succeed())
		})
	})
})
