// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package iprulemanager

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"

	"k8s.io/klog/v2"

	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

type Interface interface {
	Run(stopCh <-chan struct{}, syncPeriod time.Duration)
	Add(rule IPRule) error
	AddWithMetadata(rule IPRule, metadata string) error
	Delete(rule IPRule) error
	DeleteWithMetadata(metadata string) error
	OwnPriority(priority int) error
	AddNodeIPFwMarkRule(ipFamily int) error
}

// This struct should be updated whenever a new netlink.Rule field is used in the codebase.
type IPRule struct {
	Priority int
	Table    int
	Family   int
	Mark     uint32
	Src      netip.Prefix
	Dst      netip.Prefix
}

type ruleState struct {
	metadata string
	delete   bool
}

type Controller struct {
	mu    *sync.Mutex
	rules map[IPRule]ruleState
	// only explicit IP rules (via fn Add) are allowed when a priority is owned. Other IP rules will be removed.
	ownPriorities map[int]bool
	v4            bool
	v6            bool
}

func (r IPRule) String() string {
	s := fmt.Sprintf("priority:%d table:%d family:%d mark:0x%x", r.Priority, r.Table, r.Family, r.Mark)
	if r.Src.IsValid() {
		s += fmt.Sprintf(" src:%s", r.Src)
	}
	if r.Dst.IsValid() {
		s += fmt.Sprintf(" dst:%s", r.Dst)
	}
	return s
}

// IPRuleFromNetlinkRule will extract relevant fields from a netlink rule.
// This function does not copy a netlink rule one-to-one.
// If additional fields are needed, they must be added to IPRule.
func IPRuleFromNetlinkRule(r *netlink.Rule) IPRule {
	return IPRule{
		Priority: r.Priority,
		Table:    r.Table,
		Family:   r.Family,
		Mark:     r.Mark,
		Src:      ipNetToPrefix(r.Src),
		Dst:      ipNetToPrefix(r.Dst),
	}
}

func (r IPRule) toNetlinkRule() *netlink.Rule {
	nl := netlink.NewRule()
	nl.Priority = r.Priority
	nl.Table = r.Table
	nl.Family = r.Family
	nl.Mark = r.Mark
	nl.Src = prefixToIPNet(r.Src)
	nl.Dst = prefixToIPNet(r.Dst)
	return nl
}

func ipNetToPrefix(n *net.IPNet) netip.Prefix {
	if n == nil {
		return netip.Prefix{}
	}
	addr, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}
	}
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(addr.Unmap(), ones)
}

func prefixToIPNet(p netip.Prefix) *net.IPNet {
	if !p.IsValid() {
		return nil
	}
	addr := p.Addr()
	var ipLen int
	if addr.Is4() {
		ipLen = net.IPv4len
	} else {
		ipLen = net.IPv6len
	}
	ip := addr.As16()
	return &net.IPNet{
		IP:   net.IP(ip[16-ipLen:]),
		Mask: net.CIDRMask(p.Bits(), ipLen*8),
	}
}

// NewController creates a new linux IP rule manager
func NewController(v4, v6 bool) *Controller {
	return &Controller{
		mu:            &sync.Mutex{},
		rules:         make(map[IPRule]ruleState),
		ownPriorities: make(map[int]bool, 0),
		v4:            v4,
		v6:            v6,
	}
}

// Run starts manages linux IP rules
func (rm *Controller) Run(stopCh <-chan struct{}, syncPeriod time.Duration) {
	var err error
	ticker := time.NewTicker(syncPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			rm.mu.Lock()
			if err = rm.reconcile(); err != nil {
				klog.Errorf("IP Rule manager: failed to reconcile (retry in %s): %v", syncPeriod.String(), err)
			}
			rm.mu.Unlock()
		}
	}
}

// Add ensures an IP rule is applied even if it is altered by something else, it will be restored
func (rm *Controller) Add(rule IPRule) error {
	return rm.AddWithMetadata(rule, "")
}

// AddWithMetadata ensures an IP rule along with its metadata is applied even if it is altered by something else, it will be restored
func (rm *Controller) AddWithMetadata(rule IPRule, metadata string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if _, ok := rm.rules[rule]; !ok {
		if err := netlink.RuleAdd(rule.toNetlinkRule()); err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("failed to add IP rule (%s): %w", rule.String(), err)
		}
	}

	rm.rules[rule] = ruleState{metadata: metadata, delete: false}

	return nil
}

// Delete stops managed an IP rule and ensures its deleted
func (rm *Controller) Delete(rule IPRule) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if _, ok := rm.rules[rule]; !ok {
		return nil
	}

	if err := netlink.RuleDel(rule.toNetlinkRule()); err != nil && !errors.Is(err, syscall.ENOENT) {
		rm.rules[rule] = ruleState{metadata: rm.rules[rule].metadata, delete: true}
		return fmt.Errorf("failed to delete IP rule (%s): %w", rule.String(), err)
	}
	delete(rm.rules, rule)
	return nil
}

// DeleteWithMetadata stops managing all IP rules with the provided metadata and ensures they are all deleted
func (rm *Controller) DeleteWithMetadata(metadata string) error {
	if metadata == "" {
		return nil
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	var errs []error
	for rule, r := range rm.rules {
		if r.metadata == metadata {
			if err := netlink.RuleDel(rule.toNetlinkRule()); err != nil && !errors.Is(err, syscall.ENOENT) {
				rm.rules[rule] = ruleState{metadata: r.metadata, delete: true}
				errs = append(errs, fmt.Errorf("failed to delete IP rule (%s): %w", rule.String(), err))
			} else {
				delete(rm.rules, rule)
			}
		}
	}
	return utilerrors.Join(errs...)
}

// OwnPriority ensures any IP rules observed with priority 'priority' must be specified otherwise its removed
func (rm *Controller) OwnPriority(priority int) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.ownPriorities[priority] = true
	return rm.reconcile()
}

func (rm *Controller) reconcile() error {
	start := time.Now()
	defer func() {
		klog.V(5).Infof("Reconciling IP rules took %v", time.Since(start))
	}()
	var family int
	if rm.v4 && rm.v6 {
		family = netlink.FAMILY_ALL
	} else if rm.v4 {
		family = netlink.FAMILY_V4
	} else if rm.v6 {
		family = netlink.FAMILY_V6
	}

	rulesFound, err := netlink.RuleList(family)
	if err != nil {
		return fmt.Errorf("failed to list IP rules: %w", err)
	}
	var errs []error
	rulesToKeep := make(map[IPRule]ruleState)
	notInNetlink := make(map[IPRule]ruleState, len(rm.rules))
	maps.Copy(notInNetlink, rm.rules)

	for _, r := range rulesFound {
		rule := IPRuleFromNetlinkRule(&r)
		if state, ok := rm.rules[rule]; ok {
			delete(notInNetlink, rule)
			if state.delete {
				if err = netlink.RuleDel(&r); err != nil {
					rulesToKeep[rule] = state
					errs = append(errs, fmt.Errorf("failed to delete IP rule (%s): %w", rule.String(), err))
				}
			} else {
				rulesToKeep[rule] = state
			}
		} else if _, ok := rm.ownPriorities[r.Priority]; ok {
			klog.Infof("Rule manager: deleting stale IP rule (%s) found at priority %d", rule.String(), r.Priority)
			if err = netlink.RuleDel(&r); err != nil {
				errs = append(errs, fmt.Errorf("failed to delete stale IP rule (%s) found at priority %d: %w",
					rule.String(), r.Priority, err))
			}
		}
	}

	for rule, state := range notInNetlink {
		if state.delete {
			continue
		}
		rulesToKeep[rule] = state
		if err = netlink.RuleAdd(rule.toNetlinkRule()); err != nil && !errors.Is(err, syscall.EEXIST) {
			errs = append(errs, fmt.Errorf("failed to add IP rule (%s): %w", rule.String(), err))
		}
	}

	rm.rules = rulesToKeep
	return utilerrors.Join(errs...)
}

// AddNodeIPFwMarkRule adds an ip rule that routes packets marked with
// OvnKubeNodeSNATMark (0x3f0/1008) via the main routing table, ensuring
// they are handled before egress IP and egress service routing rules in LGW mode.
func (rm *Controller) AddNodeIPFwMarkRule(ipFamily int) error {
	return rm.Add(IPRule{
		Priority: nodetypes.FwMarkBypassPriority,
		Mark:     nodetypes.OvnKubeNodeSNATMarkValue,
		Table:    254, // main
		Family:   ipFamily,
	})
}
