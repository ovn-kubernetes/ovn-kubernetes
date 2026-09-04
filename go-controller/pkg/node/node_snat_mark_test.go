// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"errors"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type recordingRuleManager struct {
	rules  []iprulemanager.IPRule
	addErr error
}

func (r *recordingRuleManager) Run(<-chan struct{}, time.Duration) {}
func (r *recordingRuleManager) Add(rule iprulemanager.IPRule) error {
	if r.addErr != nil {
		return r.addErr
	}
	r.rules = append(r.rules, rule)
	return nil
}
func (r *recordingRuleManager) AddWithMetadata(iprulemanager.IPRule, string) error { return nil }
func (r *recordingRuleManager) Delete(iprulemanager.IPRule) error                  { return nil }
func (r *recordingRuleManager) DeleteWithMetadata(string) error                    { return nil }
func (r *recordingRuleManager) OwnPriority(int) error                              { return nil }

var _ = Describe("setupNodeSNATMarkRoutingRules", func() {
	const (
		ipv4Enabled  = true
		ipv4Disabled = false
		ipv6Enabled  = true
		ipv6Disabled = false
	)

	DescribeTable("adds rules",
		func(enableIPv4, enableIPv6 bool, expectedRules []iprulemanager.IPRule) {
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.IPv4Mode = enableIPv4
			config.IPv6Mode = enableIPv6
			fexec := ovntest.NewFakeExec()
			if enableIPv4 {
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "sysctl -w net.ipv4.conf.all.src_valid_mark=1",
					Output: "net.ipv4.conf.all.src_valid_mark = 1",
				})
			}
			Expect(util.SetExec(fexec)).To(Succeed())
			DeferCleanup(util.ResetRunner)

			ruleManager := &recordingRuleManager{}
			Expect(setupNodeSNATMarkRoutingRules(ruleManager)).To(Succeed())
			Expect(ruleManager.rules).To(Equal(expectedRules))
			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
		},
		Entry("for dual-stack", ipv4Enabled, ipv6Enabled, []iprulemanager.IPRule{
			{Priority: nodetypes.FwMarkBypassPriority, Mark: nodetypes.OvnKubeNodeSNATMarkValue, Table: unix.RT_TABLE_MAIN, Family: netlink.FAMILY_V4},
			{Priority: nodetypes.FwMarkBypassPriority, Mark: nodetypes.OvnKubeNodeSNATMarkValue, Table: unix.RT_TABLE_MAIN, Family: netlink.FAMILY_V6},
		}),
		Entry("for IPv4", ipv4Enabled, ipv6Disabled, []iprulemanager.IPRule{
			{Priority: nodetypes.FwMarkBypassPriority, Mark: nodetypes.OvnKubeNodeSNATMarkValue, Table: unix.RT_TABLE_MAIN, Family: netlink.FAMILY_V4},
		}),
		Entry("for IPv6", ipv4Disabled, ipv6Enabled, []iprulemanager.IPRule{
			{Priority: nodetypes.FwMarkBypassPriority, Mark: nodetypes.OvnKubeNodeSNATMarkValue, Table: unix.RT_TABLE_MAIN, Family: netlink.FAMILY_V6},
		}),
	)

	It("returns an error when adding a rule fails", func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.IPv4Mode = true
		config.IPv6Mode = false

		ruleManager := &recordingRuleManager{addErr: errors.New("add failed")}
		Expect(setupNodeSNATMarkRoutingRules(ruleManager)).To(MatchError(ContainSubstring("failed to create IPv4 fwmark bypass rule: add failed")))
	})

	DescribeTable("returns an error when setting src_valid_mark fails",
		func(output string, sysctlErr error, expectedError string) {
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.IPv4Mode = true
			config.IPv6Mode = false
			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "sysctl -w net.ipv4.conf.all.src_valid_mark=1",
				Output: output,
				Err:    sysctlErr,
			})
			Expect(util.SetExec(fexec)).To(Succeed())
			DeferCleanup(util.ResetRunner)

			Expect(setupNodeSNATMarkRoutingRules(&recordingRuleManager{})).To(MatchError(ContainSubstring(expectedError)))
			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
		},
		Entry("because sysctl returns an error", "", errors.New("sysctl failed"), "failed to set sysctl net.ipv4.conf.all.src_valid_mark to 1: sysctl failed"),
		Entry("because sysctl returns unexpected output", "unexpected", nil, "failed to set sysctl net.ipv4.conf.all.src_valid_mark to 1"),
	)
})
