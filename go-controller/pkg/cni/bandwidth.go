// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"errors"
	"fmt"
	"slices"
	"strconv"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// clearOrphanedPodQoS removes root QoS rows left behind by earlier teardown.
// Never detach live ports here: another attachment may still own them.
func clearOrphanedPodQoS(ovsClient libovsdbclient.Client, sandboxID string) error {
	ops, err := deleteUnusedPodQoSOps(ovsClient, sandboxID, nil)
	if err != nil {
		return err
	}
	_, err = ovsops.TransactAndCheck(ovsClient, ops)
	if err != nil {
		return fmt.Errorf("failed to clear orphaned QoS for sandbox %s: %w", sandboxID, err)
	}
	return nil
}

// clearPodBandwidthOps clears only the same-named, single-interface pod port
// owned by this sandbox on br-int, leaving bonds and other bridges untouched.
func clearPodBandwidthOps(ovsClient libovsdbclient.Client, sandboxID, portName string) ([]ovsdb.Operation, error) {
	iface, err := ovsops.GetOVSInterface(ovsClient, portName)
	if errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get interface %s for bandwidth cleanup: %w", portName, err)
	}
	if iface.ExternalIDs["sandbox"] != sandboxID {
		return nil, nil
	}
	port, err := ovsops.GetOVSPort(ovsClient, portName)
	if errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get port %s for bandwidth cleanup: %w", portName, err)
	}
	if port.QOS == nil || len(port.Interfaces) != 1 || port.Interfaces[0] != iface.UUID {
		return nil, nil
	}
	bridge, err := ovsops.GetBridge(ovsClient, "br-int")
	if err != nil {
		return nil, fmt.Errorf("failed to get br-int for bandwidth cleanup: %w", err)
	}
	if !slices.Contains(bridge.Ports, port.UUID) {
		return nil, nil
	}
	update := &vswitchd.Port{UUID: port.UUID}
	ops, err := ovsClient.Where(update).Update(update, &update.QOS)
	if err != nil {
		return nil, fmt.Errorf("failed to clear QoS reference on port %s: %w", portName, err)
	}
	qosOps, err := deleteUnusedPodQoSOps(ovsClient, sandboxID, port)
	if err != nil {
		return nil, err
	}
	return append(ops, qosOps...), nil
}

// QoS is a root table. Delete unused rows explicitly, but preserve every row
// referenced by a port other than the one being cleared in this transaction.
func deleteUnusedPodQoSOps(ovsClient libovsdbclient.Client, sandboxID string, clearedPort *vswitchd.Port) ([]ovsdb.Operation, error) {
	qos := &vswitchd.QoS{}
	conditions := []model.Condition{{
		Field: &qos.ExternalIDs, Function: ovsdb.ConditionIncludes,
		Value: map[string]string{"sandbox": sandboxID},
	}}
	if clearedPort != nil {
		conditions = append(conditions, model.Condition{Field: &qos.UUID, Function: ovsdb.ConditionEqual, Value: *clearedPort.QOS})
	}
	otherPorts, err := ovsops.FindOVSPortsWithPredicate(ovsClient, func(port *vswitchd.Port) bool {
		return port.QOS != nil && (clearedPort == nil ||
			(port.UUID != clearedPort.UUID && *port.QOS == *clearedPort.QOS))
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find remaining QoS references: %w", err)
	}
	if clearedPort != nil && len(otherPorts) > 0 {
		return nil, nil // Another port still references this attachment's QoS.
	}
	for _, port := range otherPorts {
		conditions = append(conditions, model.Condition{Field: &qos.UUID, Function: ovsdb.ConditionNotEqual, Value: *port.QOS})
	}
	ops, err := ovsClient.WhereAll(qos, conditions...).Delete()
	if err != nil {
		return nil, fmt.Errorf("failed to delete unused QoS for sandbox %s: %w", sandboxID, err)
	}
	return ops, nil
}

func setPodBandwidthOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, sandboxID, ifname string, ingressBPS, egressBPS int64) ([]ovsdb.Operation, error) {
	// Pod ingress is OVS egress, and pod egress is OVS ingress.

	if ingressBPS > 0 {
		port, err := ovsops.GetOVSPort(ovsClient, ifname)
		if err != nil {
			return nil, fmt.Errorf("failed to get OVS port %s: %w", ifname, err)
		}
		qos := &vswitchd.QoS{
			// The named UUID lets the port update reference this row in the
			// same transaction while OVSDB assigns its persistent UUID.
			UUID:        "podQoS",
			Type:        "linux-htb",
			OtherConfig: map[string]string{"max-rate": strconv.FormatInt(ingressBPS, 10)},
			ExternalIDs: map[string]string{"sandbox": sandboxID},
		}
		qosOps, err := ovsClient.Create(qos)
		if err != nil {
			return nil, fmt.Errorf("failed to build QoS creation operation for port %s: %w", ifname, err)
		}
		ops = append(ops, qosOps...)

		portUpdate := &vswitchd.Port{UUID: port.UUID, QOS: &qos.UUID}
		portOps, err := ovsClient.Where(&vswitchd.Port{UUID: port.UUID}).Update(portUpdate, &portUpdate.QOS)
		if err != nil {
			return nil, fmt.Errorf("failed to build QoS update operation for port %s: %w", ifname, err)
		}
		ops = append(ops, portOps...)
	}

	// Zero removes this interface's limit without changing other attachments.
	iface, err := ovsops.GetOVSInterface(ovsClient, ifname)
	if err != nil {
		return nil, fmt.Errorf("failed to get OVS interface %s: %w", ifname, err)
	}
	egressKBPS := int(egressBPS / 1000)
	ifaceUpdate := &vswitchd.Interface{
		UUID:                 iface.UUID,
		IngressPolicingRate:  egressKBPS,
		IngressPolicingBurst: egressKBPS / 10,
	}
	ifaceOps, err := ovsClient.Where(&vswitchd.Interface{UUID: iface.UUID}).Update(ifaceUpdate,
		&ifaceUpdate.IngressPolicingRate, &ifaceUpdate.IngressPolicingBurst)
	if err != nil {
		return nil, fmt.Errorf("failed to build policing update operation for interface %s: %w", ifname, err)
	}
	ops = append(ops, ifaceOps...)

	return ops, nil
}
