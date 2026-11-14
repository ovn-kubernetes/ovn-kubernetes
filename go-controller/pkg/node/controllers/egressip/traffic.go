// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressip

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"sigs.k8s.io/knftables"

	ovnconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	eipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	trafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/trafficclass/v1"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/egressip"
)

const selectiveSNATChain = "egress-ip-snat-filtered"

// A class change can reject a winner or promote a previously rejected peer.
// Queue every selective owner, including owners that no longer select this class.
func (c *Controller) onTrafficClassChange(_ interface{}) {
	c.enqueueSelectivePeers()
}

func (c *Controller) onTrafficClassUpdate(old, new interface{}) {
	before := old.(*trafficv1.TrafficClass)
	after := new.(*trafficv1.TrafficClass)
	if reflect.DeepEqual(before.Labels, after.Labels) && reflect.DeepEqual(before.Spec, after.Spec) {
		return
	}
	c.enqueueSelectivePeers()
}

func containsRoute(routes []netlink.Route, candidate netlink.Route) bool {
	for _, route := range routes {
		if util.RouteEqual(&route, &candidate) {
			return true
		}
	}
	return false
}

// Caller holds the owner's cache lock. Copy its contribution before publishing;
// no worker reads another owner's mutable applied state.
func (c *Controller) syncSelectiveSNAT(owner string, applied *state) error {
	var contribution []string
	if applied != nil {
		for _, pods := range applied.namespacesWithPodIPConfigs {
			for _, pod := range pods {
				for _, config := range pod.elems {
					if config.trafficMatch.CIDR.IsValid() {
						contribution = append(contribution, selectiveSNATRule(config))
					}
				}
			}
		}
	}
	slices.Sort(contribution)
	contribution = slices.Compact(contribution)
	c.snatMutex.Lock()
	defer c.snatMutex.Unlock()
	if !c.snatDirty && slices.Equal(c.snatRules[owner], contribution) {
		return nil
	}
	if len(contribution) == 0 {
		delete(c.snatRules, owner)
	} else {
		c.snatRules[owner] = contribution
	}
	// Retain contributions on transaction failure so the next publication retries them.
	var rules []string
	for _, ownerRules := range c.snatRules {
		rules = append(rules, ownerRules...)
	}
	err := c.syncSelectiveSNATRules(rules)
	c.snatDirty = err != nil
	return err
}

func (c *Controller) syncSelectiveSNATRules(rules []string) error {
	if !ovnconfig.OVNKubernetesFeature.EnableTrafficClass {
		return nil
	}
	slices.Sort(rules)
	rules = slices.Compact(rules)
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()
	tx.Add(&knftables.Chain{Name: selectiveSNATChain})
	tx.Flush(&knftables.Chain{Name: selectiveSNATChain})
	for _, rule := range rules {
		tx.Add(&knftables.Rule{Chain: selectiveSNATChain, Rule: rule})
	}
	return nft.Run(context.TODO(), tx)
}

func selectiveSNATRule(config *podIPConfig) string {
	family := "ip"
	if config.v6 {
		family = "ip6"
	}
	d := config.trafficMatch
	parts := []string{family, "saddr", config.snatKey, "oifname", fmt.Sprintf("%q", config.ifName), family, "daddr", d.CIDR.String()}
	if d.HasL4Filter() {
		protocol := d.ProtocolName()
		parts = append(parts, "meta", "l4proto", protocol)
		if d.DestinationPort != (egressip.PortRange{}) {
			parts = append(parts, protocol, "dport", d.DestinationPort.String())
		}
		if d.SourcePort != (egressip.PortRange{}) {
			parts = append(parts, protocol, "sport", d.SourcePort.String())
		}
	}
	parts = append(parts, "snat", family, "to", config.snatValue)
	return strings.Join(parts, " ")
}

// All selective peers may change winners when one owner is edited or removed.
func (c *Controller) enqueueSelectivePeers() {
	if !ovnconfig.OVNKubernetesFeature.EnableTrafficClass {
		return
	}
	eips, err := c.eIPLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list selective EgressIPs: %v", err)
		return
	}
	for _, eip := range eips {
		if egressip.HasTrafficSelector(eip.Spec) {
			c.eIPQueue.Add(eip.Name)
		}
	}
}

// resolvePodTraffic snapshots peers once per owner reconciliation or startup repair.
func (c *Controller) resolvePodTraffic(eips []*eipv1.EgressIP) (egressip.PodTrafficSelection, error) {
	if !ovnconfig.OVNKubernetesFeature.EnableTrafficClass {
		return nil, nil
	}
	classes, err := c.trafficLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	return egressip.ResolvePodTraffic(eips, classes)
}

func acceptsPodTraffic(selection egressip.PodTrafficSelection, eip *eipv1.EgressIP, pod *corev1.Pod, namespace *corev1.Namespace) bool {
	if !egressip.HasTrafficSelector(eip.Spec) {
		return true
	}
	selected := selection.SelectPod(pod, namespace.Labels)
	conflict, included := selected[eip.Name]
	return included && conflict == nil
}
