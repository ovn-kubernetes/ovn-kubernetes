// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"errors"
	"fmt"
	"slices"
	"strconv"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// ensureSourcePortGroup creates the per-NetworkQoS source port group used on
// ipamless localnet networks to match source pods via inport == @pg. Pods are
// added/removed to it in the pod path; membership is intentionally left
// untouched here (create-if-absent), so it is safe to call on every reconcile.
func (c *Controller) ensureSourcePortGroup(qosState *networkQoSState) error {
	pgIDs := GetNetworkQoSPortGroupDbIDs(qosState.namespace, qosState.name, c.controllerName)
	pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
	if c.IsUserDefinedNetwork() {
		pg.ExternalIDs[types.NetworkExternalID] = c.GetNetworkName()
	}
	if err := libovsdbops.CreatePortGroup(c.nbClient, pg); err != nil {
		return fmt.Errorf("failed to create source port group %s for NetworkQoS %s/%s: %w", pg.Name, qosState.namespace, qosState.name, err)
	}
	return nil
}

func (c *Controller) findLogicalSwitch(switchName string) (*nbdb.LogicalSwitch, error) {
	if lsws, err := libovsdbops.FindLogicalSwitchesWithPredicate(c.nbClient, func(item *nbdb.LogicalSwitch) bool {
		return item.Name == switchName
	}); err != nil {
		return nil, fmt.Errorf("failed to look up logical switch %s: %w", switchName, err)
	} else if len(lsws) > 0 {
		return lsws[0], nil
	}
	return nil, fmt.Errorf("logical switch %s not found", switchName)
}

func (c *Controller) addQoSToLogicalSwitch(qosState *networkQoSState, switchName string) error {
	// find lsw
	lsw, err := c.findLogicalSwitch(switchName)
	if err != nil {
		return err
	}
	// construct qoses
	qoses := []*nbdb.QoS{}
	ipv4Enabled, ipv6Enabled := c.IPMode()
	for index, rule := range qosState.EgressRules {
		dbIDs := qosState.getDbObjectIDs(c.controllerName, index)
		qos := &nbdb.QoS{
			Action:      map[string]int{},
			Bandwidth:   map[string]int{},
			Direction:   nbdb.QoSDirectionToLport,
			ExternalIDs: dbIDs.GetExternalIDs(),
			Match:       generateNetworkQoSMatch(qosState, rule, ipv4Enabled, ipv6Enabled, c.isIPAMlessLocalnet()),
			Priority:    rule.Priority,
		}
		if c.IsUserDefinedNetwork() {
			qos.ExternalIDs[types.NetworkExternalID] = c.GetNetworkName()
		}
		if rule.Dscp >= 0 {
			qos.Action[nbdb.QoSActionDSCP] = rule.Dscp
		}
		if rule.Rate != nil && *rule.Rate > 0 {
			qos.Bandwidth[nbdb.QoSBandwidthRate] = *rule.Rate
		}
		if rule.Burst != nil && *rule.Burst > 0 {
			qos.Bandwidth[nbdb.QoSBandwidthBurst] = *rule.Burst
		}
		qoses = append(qoses, qos)
	}
	ops := []ovsdb.Operation{}
	ops, err = libovsdbops.CreateOrUpdateQoSesOps(c.nbClient, ops, qoses...)
	if err != nil {
		return fmt.Errorf("failed to create QoS operations for %s/%s: %w", qosState.namespace, qosState.name, err)
	}
	// identify qoses need binding to lsw
	newQoSes := []*nbdb.QoS{}
	for _, qos := range qoses {
		if slices.Contains(lsw.QOSRules, qos.UUID) {
			continue
		}
		newQoSes = append(newQoSes, qos)
	}
	if len(newQoSes) > 0 {
		ops, err = libovsdbops.AddQoSesToLogicalSwitchOps(c.nbClient, ops, switchName, newQoSes...)
		if err != nil {
			return fmt.Errorf("failed to create operations to add QoS to switch %s: %w", switchName, err)
		}
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, ops); err != nil {
		return fmt.Errorf("failed to execute ops to add QoSes to switch %s, err: %w", switchName, err)
	}
	return nil
}

// remove qos from a list of logical switches
func (c *Controller) removeQoSFromLogicalSwitches(qosState *networkQoSState, switchNames []string) error {
	qoses, err := libovsdbops.FindQoSesWithPredicate(c.nbClient, func(qos *nbdb.QoS) bool {
		return qos.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			qos.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.NetworkQoSOwnerType) &&
			qos.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosState.getObjectNameKey()
	})
	if err != nil {
		return fmt.Errorf("failed to look up QoSes for %s/%s: %v", qosState.namespace, qosState.name, err)
	}
	unbindQoSOps := []ovsdb.Operation{}
	// remove qos rules from logical switches
	for _, lsName := range switchNames {
		ops, err := libovsdbops.RemoveQoSesFromLogicalSwitchOps(c.nbClient, nil, lsName, qoses...)
		if err != nil {
			return fmt.Errorf("failed to get ops to remove QoSes from switches %s for NetworkQoS %s/%s: %w", lsName, qosState.namespace, qosState.name, err)
		}
		unbindQoSOps = append(unbindQoSOps, ops...)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, unbindQoSOps); err != nil {
		return fmt.Errorf("failed to execute ops to remove QoSes from logical switches, err: %w", err)
	}
	return nil
}

func (c *Controller) cleanupStaleOvnObjects(qosState *networkQoSState) error {
	// find existing QoSes owned by NetworkQoS
	existingQoSes, err := libovsdbops.FindQoSesWithPredicate(c.nbClient, func(qos *nbdb.QoS) bool {
		return qos.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			qos.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.NetworkQoSOwnerType) &&
			qos.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosState.getObjectNameKey()
	})
	if err != nil {
		return fmt.Errorf("error looking up existing QoSes for %s/%s: %v", qosState.namespace, qosState.name, err)
	}
	staleSwitchQoSMap := map[string][]*nbdb.QoS{}
	totalNumOfRules := len(qosState.EgressRules)
	for _, qos := range existingQoSes {
		index := qos.ExternalIDs[libovsdbops.RuleIndex.String()]
		numIndex, convError := strconv.Atoi(index)
		indexWithinRange := false
		if index != "" && convError == nil && numIndex < totalNumOfRules {
			// rule index is valid
			indexWithinRange = true
		}
		// qos is considered stale since the index is out of range
		// get switches that reference to the stale qos
		switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(c.nbClient, func(ls *nbdb.LogicalSwitch) bool {
			return util.SliceHasStringItem(ls.QOSRules, qos.UUID)
		})
		if err != nil {
			if !errors.Is(err, libovsdbclient.ErrNotFound) {
				return fmt.Errorf("error looking up logical switches by qos: %w", err)
			}
			continue
		}
		// build map of switch->list(qos)
		for _, ls := range switches {
			if _, qosInUse := qosState.SwitchRefs.Load(ls.Name); indexWithinRange && qosInUse {
				continue
			}
			qosList := staleSwitchQoSMap[ls.Name]
			if qosList == nil {
				qosList = []*nbdb.QoS{}
			}
			qosList = append(qosList, qos)
			staleSwitchQoSMap[ls.Name] = qosList
		}
	}
	allOps, err := c.findStaleAddressSets(qosState)
	if err != nil {
		return fmt.Errorf("failed to get ops to delete stale address sets for NetworkQoS %s/%s: %w", qosState.namespace, qosState.name, err)
	}
	// remove stale qos rules from logical switches
	for lsName, qoses := range staleSwitchQoSMap {
		var switchOps []ovsdb.Operation
		switchOps, err = libovsdbops.RemoveQoSesFromLogicalSwitchOps(c.nbClient, switchOps, lsName, qoses...)
		if err != nil {
			return fmt.Errorf("failed to get ops to remove stale QoSes from switches %s for NetworkQoS %s/%s: %w", lsName, qosState.namespace, qosState.name, err)
		}
		allOps = append(allOps, switchOps...)
	}
	// commit allOps
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, allOps); err != nil {
		return fmt.Errorf("failed to execute ops to clean up stale QoSes, err: %w", err)
	}
	return nil
}

// delete ovn QoSes generated from network qos
func (c *Controller) deleteByName(ovnObjectName string) error {
	qoses, err := libovsdbops.FindQoSesWithPredicate(c.nbClient, func(qos *nbdb.QoS) bool {
		return qos.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			qos.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.NetworkQoSOwnerType) &&
			qos.ExternalIDs[libovsdbops.ObjectNameKey.String()] == ovnObjectName
	})
	if err != nil {
		return fmt.Errorf("failed to look up QoSes by name %s: %v", ovnObjectName, err)
	}
	if err = c.deleteOvnQoSes(qoses); err != nil {
		return fmt.Errorf("error cleaning up OVN QoSes for %s: %v", ovnObjectName, err)
	}
	// remove address sets
	if err = c.deleteAddressSet(ovnObjectName); err != nil {
		return fmt.Errorf("error cleaning up address sets for %s: %w", ovnObjectName, err)
	}
	// On ipamless localnet the source is matched by a per-NetworkQoS port group
	// instead of a src-IP address set; remove it on delete. Sequenced after the
	// QoS-row removal above so no QoS row transiently references a deleted port
	// group. IPAM-enabled networks never create one, so this is skipped for them.
	if c.isIPAMlessLocalnet() {
		if err = c.deleteSourcePortGroup(ovnObjectName); err != nil {
			return fmt.Errorf("error cleaning up source port group for %s: %w", ovnObjectName, err)
		}
	}
	return nil
}

// deleteSourcePortGroup deletes the per-NetworkQoS source port group owned by this
// object (matched by owner external IDs, mirroring deleteAddressSet). Used only on
// ipamless localnet networks. Exhaustive stale/orphan PG GC (rename/restart) is out
// of scope for the PoC.
func (c *Controller) deleteSourcePortGroup(qosName string) error {
	delOps, err := libovsdbops.DeletePortGroupsWithPredicateOps(c.nbClient, nil, func(item *nbdb.PortGroup) bool {
		return item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			item.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.NetworkQoSOwnerType) &&
			item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosName
	})
	if err != nil {
		return fmt.Errorf("failed to get ops to delete source port group: %w", err)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, delOps); err != nil {
		return fmt.Errorf("failed to execute ops to delete source port group, err: %w", err)
	}
	return nil
}

// delete a list of ovn QoSes
func (c *Controller) deleteOvnQoSes(qoses []*nbdb.QoS) error {
	switchQoSMap := map[string][]*nbdb.QoS{}
	for _, qos := range qoses {
		switches, err := libovsdbops.FindLogicalSwitchesWithPredicate(c.nbClient, func(ls *nbdb.LogicalSwitch) bool {
			return util.SliceHasStringItem(ls.QOSRules, qos.UUID)
		})
		if err != nil {
			if !errors.Is(err, libovsdbclient.ErrNotFound) {
				return fmt.Errorf("failed to look up logical switches by qos: %w", err)
			}
			continue
		}
		// get switches that reference to the stale qoses
		for _, ls := range switches {
			qosList := switchQoSMap[ls.Name]
			if qosList == nil {
				qosList = []*nbdb.QoS{}
			}
			qosList = append(qosList, qos)
			switchQoSMap[ls.Name] = qosList
		}
	}
	unbindQoSOps := []ovsdb.Operation{}
	// remove qos rules from logical switches
	for lsName, qoses := range switchQoSMap {
		ops, err := libovsdbops.RemoveQoSesFromLogicalSwitchOps(c.nbClient, nil, lsName, qoses...)
		if err != nil {
			return fmt.Errorf("failed to get ops to remove QoSes from switch %s: %w", lsName, err)
		}
		unbindQoSOps = append(unbindQoSOps, ops...)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, unbindQoSOps); err != nil {
		return fmt.Errorf("failed to execute ops to remove QoSes from logical switches, err: %w", err)
	}
	// delete qos
	delQoSOps, err := libovsdbops.DeleteQoSesOps(c.nbClient, nil, qoses...)
	if err != nil {
		return fmt.Errorf("failed to get ops to delete QoSes: %w", err)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, delQoSOps); err != nil {
		return fmt.Errorf("failed to execute ops to delete QoSes, err: %w", err)
	}
	return nil
}

func (c *Controller) deleteAddressSet(qosName string) error {
	// find address sets by networkqos name & controller name
	delAddrSetOps, err := libovsdbops.DeleteAddressSetsWithPredicateOps(c.nbClient, nil, func(item *nbdb.AddressSet) bool {
		return item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			item.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.NetworkQoSOwnerType) &&
			item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosName
	})
	if err != nil {
		return fmt.Errorf("failed to get ops to delete address sets: %w", err)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, delAddrSetOps); err != nil {
		return fmt.Errorf("failed to execute ops to delete address sets, err: %w", err)
	}
	return nil
}

// find stale address sets
// 1. find address sets owned by NetworkQoS
// 2. get address sets in use
// 3. compare and identify those not in use
func (c *Controller) findStaleAddressSets(qosState *networkQoSState) ([]ovsdb.Operation, error) {
	staleAddressSets := []*nbdb.AddressSet{}
	addrsets, err := libovsdbops.FindAddressSetsWithPredicate(c.nbClient, func(item *nbdb.AddressSet) bool {
		return item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == c.controllerName &&
			item.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.NetworkQoSOwnerType) &&
			item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == qosState.getObjectNameKey()
	})
	if err != nil {
		return nil, fmt.Errorf("failed to look up address sets: %w", err)
	}
	addrsetInUse := qosState.getAddressSetHashNames()
	for _, addrset := range addrsets {
		addrsetName := addrset.GetName()
		if !slices.Contains(addrsetInUse, addrsetName) {
			staleAddressSets = append(staleAddressSets, addrset)
		}
	}
	return libovsdbops.DeleteAddressSetsOps(c.nbClient, nil, staleAddressSets...)
}
