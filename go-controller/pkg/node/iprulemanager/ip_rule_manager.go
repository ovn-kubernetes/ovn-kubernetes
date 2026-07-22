// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package iprulemanager

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"

	"k8s.io/klog/v2"

	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

type Interface interface {
	Run(stopCh <-chan struct{}, syncPeriod time.Duration)
	Add(rule netlink.Rule) error
	AddWithMetadata(rule netlink.Rule, metadata string) error
	Delete(rule netlink.Rule) error
	DeleteWithMetadata(metadata string) error
	OwnPriority(priority int) error
}

type ipRule struct {
	rule     *netlink.Rule
	metadata string
	delete   bool
}

type Controller struct {
	mu    *sync.Mutex
	rules map[string]ipRule
	// only explicit IP rules (via fn Add) are allowed when a priority is owned. Other IP rules will be removed.
	ownPriorities map[int]bool
	v4            bool
	v6            bool
}

func (rule ipRule) toString() string {
	return fmt.Sprintf("%s family:%d mark:%d", rule.rule.String(), rule.rule.Family, rule.rule.Mark)
}

// NewController creates a new linux IP rule manager
func NewController(v4, v6 bool) *Controller {
	return &Controller{
		mu:            &sync.Mutex{},
		rules:         make(map[string]ipRule),
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
func (rm *Controller) Add(rule netlink.Rule) error {
	return rm.AddWithMetadata(rule, "")
}

// AddWithMetadata ensures an IP rule along with its metadata is applied even if it is altered by something else, it will be restored
func (rm *Controller) AddWithMetadata(rule netlink.Rule, metadata string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	addedRule := ipRule{rule: &rule, metadata: metadata, delete: false}
	// check if we are already managing this rule and if so, no-op
	if _, ok := rm.rules[addedRule.toString()]; ok {
		return nil
	}

	if err := netlink.RuleAdd(&rule); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("failed to add IP rule (%s): %w", addedRule.toString(), err)
	}
	rm.rules[addedRule.toString()] = addedRule

	return nil
}

// Delete stops managed an IP rule and ensures its deleted
func (rm *Controller) Delete(rule netlink.Rule) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	iprule := ipRule{rule: &rule, metadata: "", delete: true}
	if _, ok := rm.rules[iprule.toString()]; !ok {
		return nil
	}

	rm.rules[iprule.toString()] = iprule
	return rm.reconcile()
}

// DeleteWithMetadata stops managing all IP rules with the provided metadata and ensures they are all deleted
func (rm *Controller) DeleteWithMetadata(metadata string) error {
	if metadata == "" {
		return nil
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	var reconcileNeeded bool
	for i, r := range rm.rules {
		if r.metadata == metadata {
			updatedRule := rm.rules[i]
			updatedRule.delete = true
			rm.rules[i] = updatedRule
			reconcileNeeded = true
		}
	}
	if reconcileNeeded {
		return rm.reconcile()
	}
	return nil
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
		return err
	}
	var errors []error
	rulesToKeep := make(map[string]ipRule, 0)
	notInNetlink := make(map[string]ipRule, len(rm.rules))
	maps.Copy(notInNetlink, rm.rules)

	for _, r := range rulesFound {
		key := ipRule{rule: &r, metadata: "", delete: false}.toString()
		// check if rule is managed by OVN-K
		if rule, ok := rm.rules[key]; ok {
			// rule is present in Netlink
			delete(notInNetlink, key)
			if rule.delete {
				if err = netlink.RuleDel(rule.rule); err != nil {
					// retry later
					rulesToKeep[key] = rule
					errors = append(errors, fmt.Errorf("failed to delete IP rule (%s): %w", rule.toString(), err))
				}
			} else {
				rulesToKeep[key] = rule
			}
		} else if _, ok := rm.ownPriorities[r.Priority]; ok {
			// if not managed, delete if priority is owned
			klog.Infof("Rule manager: deleting stale IP rule (%s) found at priority %d", r.String(), r.Priority)
			if err = netlink.RuleDel(&r); err != nil {
				errors = append(errors, fmt.Errorf("failed to delete stale IP rule (%s) found at priority %d: %w",
					r.String(), r.Priority, err))
			}
		}
	}

	// add missing rules to Netlink
	for _, r := range notInNetlink {
		if r.delete {
			continue
		}

		rulesToKeep[r.toString()] = r
		if found, _ := isNetlinkRuleInSlice(rulesFound, r.rule); !found {
			if err = netlink.RuleAdd(r.rule); err != nil {
				errors = append(errors, fmt.Errorf("failed to add IP rule (%s): %w", r.toString(), err))
			}
		}
	}

	rm.rules = rulesToKeep
	return utilerrors.Join(errors...)
}

func areNetlinkRulesEqual(r1, r2 *netlink.Rule) bool {
	if r1.Priority != r2.Priority {
		return false
	}
	if r1.Table != r2.Table {
		return false
	}
	if !areRuleFamiliesEqual(r1.Family, r2.Family) {
		return false
	}
	if r1.Type != r2.Type {
		return false
	}
	if r1.Mark != r2.Mark {
		return false
	}

	return areIPNetsEqual(r1.Src, r2.Src) && areIPNetsEqual(r1.Dst, r2.Dst)
}

func areRuleFamiliesEqual(f1, f2 int) bool {
	return f1 == f2 || f1 == netlink.FAMILY_ALL || f2 == netlink.FAMILY_ALL
}

func areIPNetsEqual(n1, n2 *net.IPNet) bool {
	if n1 == nil && n2 == nil {
		return true
	}
	if n1 == nil || n2 == nil {
		return false
	}

	if !n1.IP.Equal(n2.IP) {
		return false
	}

	n1ones, n1bits := n1.Mask.Size()
	n2ones, n2bits := n2.Mask.Size()
	return n1ones == n2ones && n1bits == n2bits
}

func isNetlinkRuleInSlice(rules []netlink.Rule, candidate *netlink.Rule) (bool, *netlink.Rule) {
	for _, r := range rules {
		r := r
		if areNetlinkRulesEqual(&r, candidate) {
			return true, &r
		}
	}
	return false, netlink.NewRule()
}
