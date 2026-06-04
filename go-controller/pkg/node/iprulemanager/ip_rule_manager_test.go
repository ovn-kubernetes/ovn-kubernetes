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

func TestAreNetlinkRulesEqualChecksFamily(t *testing.T) {
	v4Rule := netlink.NewRule()
	v4Rule.Priority = 2000
	v4Rule.Table = 1020
	v4Rule.Family = netlink.FAMILY_V4
	v4Rule.Mark = 0x1003

	v6Rule := netlink.NewRule()
	v6Rule.Priority = v4Rule.Priority
	v6Rule.Table = v4Rule.Table
	v6Rule.Family = netlink.FAMILY_V6
	v6Rule.Mark = v4Rule.Mark

	if areNetlinkRulesEqual(v4Rule, v6Rule) {
		t.Fatalf("expected IPv4 and IPv6 rules with the same mark/table to be different")
	}
}

func TestAreNetlinkRulesEqualTreatsFamilyAllAsWildcard(t *testing.T) {
	v4Rule := netlink.NewRule()
	v4Rule.Priority = 2000
	v4Rule.Table = 1020
	v4Rule.Family = netlink.FAMILY_V4
	v4Rule.Mark = 0x1003

	anyFamilyRule := netlink.NewRule()
	anyFamilyRule.Priority = v4Rule.Priority
	anyFamilyRule.Table = v4Rule.Table
	anyFamilyRule.Family = netlink.FAMILY_ALL
	anyFamilyRule.Mark = v4Rule.Mark

	if !areNetlinkRulesEqual(v4Rule, anyFamilyRule) {
		t.Fatalf("expected FAMILY_ALL to match a specific rule family")
	}
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
					return c.Add(*ruleWithDst)
				})
			}()).Should(gomega.Succeed())

			gomega.Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					rules, err := netlink.RuleList(netlink.FAMILY_ALL)
					if err != nil {
						return err
					}
					if ok, _ := isNetlinkRuleInSlice(rules, ruleWithDst); !ok {
						return fmt.Errorf("failed to find rule %q", ruleWithDst.String())
					}
					return nil
				})
			}).WithTimeout(time.Second).Should(gomega.Succeed())
		})

		ginkgo.It("ensure rule is restored if it is removed", func() {
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					return c.Add(*ruleWithDst)
				})
			}()).Should(gomega.Succeed())

			gomega.Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					rules, err := netlink.RuleList(netlink.FAMILY_ALL)
					if err != nil {
						return err
					}
					if ok, _ := isNetlinkRuleInSlice(rules, ruleWithDst); !ok {
						return fmt.Errorf("failed to find rule %q", ruleWithDst.String())
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
					if ok, _ := isNetlinkRuleInSlice(rules, ruleWithDst); !ok {
						return fmt.Errorf("failed to find rule %q", ruleWithDst.String())
					}
					return nil
				})
			}).WithTimeout(time.Second).Should(gomega.Succeed())
		})

		ginkgo.It("ensure multiple rules are restored if they're removed", func() {
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					return c.Add(*ruleWithDst)
				})
			}()).Should(gomega.Succeed())
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					return c.Add(*ruleWithSrc)
				})
			}()).Should(gomega.Succeed())

			gomega.Eventually(func() error {
				return testNS.Do(func(ns.NetNS) error {
					rules, err := netlink.RuleList(netlink.FAMILY_ALL)
					if err != nil {
						return err
					}
					if ok, _ := isNetlinkRuleInSlice(rules, ruleWithDst); !ok {
						return fmt.Errorf("failed to find rule with dst")
					}
					if ok2, _ := isNetlinkRuleInSlice(rules, ruleWithSrc); !ok2 {
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
					if ok, _ := isNetlinkRuleInSlice(rules, ruleWithDst); !ok {
						return fmt.Errorf("failed to find rule %s", ruleWithDst.String())
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
					if ok, _ := isNetlinkRuleInSlice(rules, ruleWithSrc); !ok {
						return fmt.Errorf("failed to find rule %s", ruleWithSrc)
					}
					return nil
				})
			}).WithTimeout(time.Second).Should(gomega.Succeed())
		})
	})

	ginkgo.Context("AddNodeIPFwMarkRule", func() {
		ginkgo.DescribeTable("installs fwmark bypass rule for the given IP family",
			func(family int) {
				expectedRule := netlink.NewRule()
				expectedRule.Priority = nodetypes.FwMarkBypassPriority
				expectedRule.Mark = nodetypes.OvnKubeNodeSNATMarkValue
				expectedRule.Table = 254
				expectedRule.Family = family

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
						if ok, _ := isNetlinkRuleInSlice(rules, expectedRule); !ok {
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
					return c.Delete(*ruleWithDst)
				})
			}()).Should(gomega.Succeed())
		})

		ginkgo.It("deletes a rule", func() {
			gomega.Expect(func() error {
				return testNS.Do(func(ns.NetNS) error {
					if err := c.Add(*ruleWithDst); err != nil {
						return err
					}
					if err := c.Delete(*ruleWithDst); err != nil {
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
					if ok, _ := isNetlinkRuleInSlice(rules, ruleWithDst); ok {
						return fmt.Errorf("expected rule (%s) to be deleted but it was found", ruleWithDst)
					}
					return nil
				})
			}).WithTimeout(time.Second).Should(gomega.Succeed())
		})
	})
})
