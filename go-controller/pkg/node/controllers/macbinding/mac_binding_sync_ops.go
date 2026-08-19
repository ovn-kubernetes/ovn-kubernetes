// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"fmt"
	"strings"

	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
)

const (
	ARPResponderTable     = 0
	ARPResponderPriority  = 40
	ARPResponderCookie    = "0x0306"
	ARPFlowCacheKeyPrefix = "MAC_BINDING_ARP_"
)

type macSyncer struct {
	sbClient    libovsdbclient.Client
	flowManager openFlowManager
}

// NewMACBindingSyncOps returns a MacBindingSyncOps that operates on the SB MAC_Binding table
// and programs ARP responder flows on the external gateway bridge via flowManager.
func newMACBindingSyncOps(sbClient libovsdbclient.Client, flowManager openFlowManager) macBindingSyncOps {
	return &macSyncer{sbClient: sbClient, flowManager: flowManager}
}

// AddMACBinding inserts a MAC_Binding for each port in a single transaction.
func (b *macSyncer) AddMACBinding(ip, mac string, timestamp int, ports []portInfo) error {
	allOps := make([]ovsdb.Operation, 0, len(ports))
	for _, port := range ports {
		AddMACBindingOps, err := b.sbClient.Create(&sbdb.MACBinding{
			IP:          ip,
			MAC:         mac,
			LogicalPort: port.LogicalPort,
			Datapath:    port.DatapathUUID,
			Timestamp:   timestamp,
		})
		if err != nil {
			return err
		}
		allOps = append(allOps, AddMACBindingOps...)
	}
	_, err := ops.TransactAndCheck(b.sbClient, allOps)
	return err
}

// UpdateMACBinding conditionally updates existing MAC_Binding entries for each port in a single transaction.
// The update only applies when the existing row's timestamp is strictly less than the new timestamp,
// preventing overwrites if the UDN GR's own statctrl has already refreshed to a newer value.
func (b *macSyncer) UpdateMACBinding(ip, mac string, timestamp int, ports []portInfo) error {
	allOps := make([]ovsdb.Operation, 0, len(ports))
	for _, port := range ports {
		mb := &sbdb.MACBinding{
			LogicalPort: port.LogicalPort,
			IP:          ip,
			MAC:         mac,
			Datapath:    port.DatapathUUID,
			Timestamp:   timestamp,
		}
		updateOps, err := b.sbClient.WhereAll(mb,
			model.Condition{
				Field:    &mb.Timestamp,
				Function: ovsdb.ConditionLessThan,
				Value:    timestamp,
			},
			model.Condition{
				Field:    &mb.LogicalPort,
				Function: ovsdb.ConditionEqual,
				Value:    port.LogicalPort,
			},
			model.Condition{
				Field:    &mb.IP,
				Function: ovsdb.ConditionEqual,
				Value:    ip,
			}).Update(mb, &mb.MAC, &mb.Timestamp)
		if err != nil {
			return err
		}
		allOps = append(allOps, updateOps...)
	}
	_, err := ops.TransactAndCheck(b.sbClient, allOps)
	return err
}

// DeleteAndAddMACBinding removes the MAC_Binding entries for each port
// and adds them again in a single transaction.
func (b *macSyncer) DeleteAndAddMACBinding(ip, mac string, timestamp int, ports []portInfo) error {
	allOps := make([]ovsdb.Operation, 0, len(ports))
	for _, port := range ports {
		mb := &sbdb.MACBinding{
			LogicalPort: port.LogicalPort,
			IP:          ip,
		}
		deleteOps, err := b.sbClient.Where(mb).Delete()
		if err != nil {
			return err
		}
		allOps = append(allOps, deleteOps...)

		AddMACBindingOps, err := b.sbClient.Create(&sbdb.MACBinding{
			IP:          ip,
			MAC:         mac,
			LogicalPort: port.LogicalPort,
			Datapath:    port.DatapathUUID,
			Timestamp:   timestamp,
		})
		if err != nil {
			return err
		}
		allOps = append(allOps, AddMACBindingOps...)
	}
	_, err := ops.TransactAndCheck(b.sbClient, allOps)
	return err
}

func macToHex(mac string) string {
	return strings.ReplaceAll(mac, ":", "")
}

func arpFlowCacheKey(ip string) string {
	return ARPFlowCacheKeyPrefix + ip
}

func arpReplyFlow(ip, mac string) string {
	macHex := macToHex(mac)
	flow := fmt.Sprintf(
		"cookie=%s,table=%d,priority=%d,arp,arp_op=1,arp_tpa=%s,"+
			"actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],"+
			"mod_dl_src:%s,"+
			"load:0x2->NXM_OF_ARP_OP[],"+
			"move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],"+
			"load:0x%s->NXM_NX_ARP_SHA[],"+
			"move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],"+
			"move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],"+
			"move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],"+
			"IN_PORT",
		ARPResponderCookie, ARPResponderTable, ARPResponderPriority,
		ip, mac, macHex,
	)
	return flow
}
func (b *macSyncer) EnsureARPFlow(ip, mac string) error {
	flow := arpReplyFlow(ip, mac)
	b.flowManager.UpdateFlowCacheEntry(arpFlowCacheKey(ip), []string{flow})
	b.flowManager.RequestFlowSync()
	return nil
}

// diffFlowKeys checks if each item in flowkeys exist (is equal or substring)
// of items in allFlowKeys, if not add them to diffSlice.
// The keys in diffSlice must be prefixed by ARPFlowCacheKeyPrefix.
func diffFlowKeys(allFlowsKeys []string, flowkeysMap map[string]struct{}) []string {
	var diffSlice []string
	for _, key := range allFlowsKeys {
		if strings.Contains(key, ARPFlowCacheKeyPrefix) {
			if _, found := flowkeysMap[key]; !found {
				diffSlice = append(diffSlice, key)
			}
		}
	}
	return diffSlice
}

func (b *macSyncer) SyncARPFlows(iptomac map[string]string) error {
	flowkeysMap := make(map[string]struct{}, len(iptomac))
	for ip, mac := range iptomac {
		flow := arpReplyFlow(ip, mac)
		flowkey := arpFlowCacheKey(ip)
		flowkeysMap[flowkey] = struct{}{}
		b.flowManager.UpdateFlowCacheEntry(flowkey, []string{flow})

	}
	allFlowsKeys := b.flowManager.Flowskeys()
	flowkeysDelete := diffFlowKeys(allFlowsKeys, flowkeysMap)
	for _, flowkeyDel := range flowkeysDelete {
		b.flowManager.DeleteFlowsByKey(flowkeyDel)
	}
	b.flowManager.RequestFlowSync()
	return nil
}

func (b *macSyncer) DeleteARPFlow(ip string) error {
	b.flowManager.DeleteFlowsByKey(arpFlowCacheKey(ip))
	b.flowManager.RequestFlowSync()
	return nil
}
