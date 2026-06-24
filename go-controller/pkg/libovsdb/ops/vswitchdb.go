// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// OVS bridge predicates for filtering.
type ovsBridgePredicate func(*vswitchd.Bridge) bool

// OVS port predicates for filtering
type ovsPortPredicate func(*vswitchd.Port) bool

// OVS interface predicates for filtering.
type ovsInterfacePredicate func(*vswitchd.Interface) bool

// GetOpenvSwitch returns the singleton Open_vSwitch row from the cache.
// When no row exists, the returned error wraps libovsdbclient.ErrNotFound so
// callers can detect that case via errors.Is. This is the libovsdb equivalent
// of `ovs-vsctl list Open_vSwitch`.
func GetOpenvSwitch(ovsClient libovsdbclient.Client) (*vswitchd.OpenvSwitch, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	openvSwitchList := []*vswitchd.OpenvSwitch{}
	err := ovsClient.List(ctx, &openvSwitchList)
	if err != nil {
		return nil, err
	}
	if len(openvSwitchList) == 0 {
		return nil, fmt.Errorf("no openvSwitch entry found: %w", libovsdbclient.ErrNotFound)
	}

	return openvSwitchList[0], nil
}

// UpdateOpenvSwitchExternalIDs merges the given map into the Open_vSwitch
// row's external_ids. Every entry in the map is inserted or overwritten;
// existing keys that are not in the map are left alone. Returns ErrNotFound
// if the row does not exist. This is the libovsdb equivalent of
// `ovs-vsctl set Open_vSwitch . external_ids:key=value ...`.
func UpdateOpenvSwitchExternalIDs(ovsClient libovsdbclient.Client, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	ovs := &vswitchd.OpenvSwitch{ExternalIDs: kv}
	opModel := operationModel{
		Model:            ovs,
		ModelPredicate:   func(*vswitchd.OpenvSwitch) bool { return true },
		OnModelMutations: []interface{}{&ovs.ExternalIDs},
		ErrNotFound:      true,
		BulkOp:           false,
	}
	m := newModelClient(ovsClient)
	_, err := m.CreateOrUpdate(opModel)
	return err
}

// RemoveOpenvSwitchExternalIDs removes the given keys from the Open_vSwitch
// row's external_ids. Keys that are not present, and a missing row, are both
// no-ops. This is the libovsdb equivalent of
// `ovs-vsctl --if-exists remove Open_vSwitch . external_ids <key> ...`.
func RemoveOpenvSwitchExternalIDs(ovsClient libovsdbclient.Client, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	// modelClient interprets a map field with empty string values as a
	// delete-by-key mutation (see buildMutationsFromFields).
	ids := make(map[string]string, len(keys))
	for _, k := range keys {
		ids[k] = ""
	}
	ovs := &vswitchd.OpenvSwitch{ExternalIDs: ids}
	opModel := operationModel{
		Model:            ovs,
		ModelPredicate:   func(*vswitchd.OpenvSwitch) bool { return true },
		OnModelMutations: []interface{}{&ovs.ExternalIDs},
		ErrNotFound:      false,
		BulkOp:           false,
	}
	m := newModelClient(ovsClient)
	return m.Delete(opModel)
}

// FindOVSPortsWithPredicate returns all OVS ports matching the predicate. This
// is the libovsdb equivalent of `ovs-vsctl find Port <conditions>`.
func FindOVSPortsWithPredicate(ovsClient libovsdbclient.Client, p ovsPortPredicate) ([]*vswitchd.Port, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	var ports []*vswitchd.Port
	err := ovsClient.WhereCache(p).List(ctx, &ports)
	return ports, err
}

// ListBridges looks up all OVS bridges from the cache. This is the libovsdb
// equivalent of `ovs-vsctl list-br`.
func ListBridges(ovsClient libovsdbclient.Client) ([]*vswitchd.Bridge, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedBridges := []*vswitchd.Bridge{}
	err := ovsClient.List(ctx, &searchedBridges)
	return searchedBridges, err
}

// FindBridgesWithPredicate returns all OVS bridges in the cache that match the
// predicate. This is the libovsdb equivalent of `ovs-vsctl find Bridge
// <conditions>`.
func FindBridgesWithPredicate(ovsClient libovsdbclient.Client, p ovsBridgePredicate) ([]*vswitchd.Bridge, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedBridges := []*vswitchd.Bridge{}

	err := ovsClient.WhereCache(p).List(ctx, &searchedBridges)
	return searchedBridges, err
}

// GetPortBridge returns the OVS bridge that owns the named port.
// Returns ErrNotFound if the port does not exist or no bridge references it
// in its ports column. This is the libovsdb equivalent of
// `ovs-vsctl port-to-br <port>`.
func GetPortBridge(ovsClient libovsdbclient.Client, portName string) (*vswitchd.Bridge, error) {
	port, err := GetOVSPort(ovsClient, portName)
	if err != nil {
		return nil, err
	}
	bridges, err := FindBridgesWithPredicate(ovsClient, func(bridge *vswitchd.Bridge) bool {
		for _, uuid := range bridge.Ports {
			if uuid == port.UUID {
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	if len(bridges) > 1 {
		return nil, fmt.Errorf("OVSDB corruption: port %q is referenced by multiple bridges: %w", portName, errMultipleResults)
	}
	if len(bridges) == 1 {
		return bridges[0], nil
	}
	return nil, fmt.Errorf("no bridge contains port %q: %w", portName, libovsdbclient.ErrNotFound)
}

// CreateOrUpdateNicBridge creates (or reconfigures) an OVS bridge for an
// uplink NIC. It sets fail-mode=standalone, the supplied hardware address,
// and external_ids bridge-id/bridge-uplink on the bridge; attaches the
// uplink port with other_config:transient=true; and references the bridge
// from the Open_vSwitch root row.
//
// Like `ovs-vsctl add-br`, this also creates the bridge's same-named
// internal Port/Interface (type=internal); without it, ovs-vswitchd never
// materialises the kernel netdev and the bridge's `mac_in_use` stays empty.
//
// external_ids/other_config are merged key-by-key, so any pre-existing
// metadata on the bridge or uplink port is preserved. If the uplink port
// (or the bridge's same-named internal port) already exists on a different
// bridge the call fails, matching `ovs-vsctl add-port`'s cross-bridge
// behaviour.
//
// This is the libovsdb equivalent of:
//
//	ovs-vsctl -- --may-exist add-br <bridge>
//	          -- br-set-external-id <bridge> bridge-id <bridge>
//	          -- br-set-external-id <bridge> bridge-uplink <uplink>
//	          -- set bridge <bridge> fail-mode=standalone other_config:hwaddr=<hwaddr>
//	          -- --may-exist add-port <bridge> <uplink>
//	          -- set port <uplink> other-config:transient=true
func CreateOrUpdateNicBridge(ovsClient libovsdbclient.Client, bridgeName, uplinkName, hwaddr string) error {
	// Refuse to silently re-parent any port that's already attached to a
	// different bridge — ovs-vsctl add-port errors out in that case. This
	// applies to both the uplink port and the bridge's same-named internal
	// port.
	for _, portName := range []string{bridgeName, uplinkName} {
		if portName == "" {
			continue
		}
		existing, err := GetPortBridge(ovsClient, portName)
		if err != nil {
			if errors.Is(err, libovsdbclient.ErrNotFound) {
				continue
			}
			return fmt.Errorf("failed to check existing bridge for port %q: %w", portName, err)
		}
		if existing.Name != bridgeName {
			return fmt.Errorf("port %q is already attached to bridge %q", portName, existing.Name)
		}
	}

	bridge := &vswitchd.Bridge{
		Name:     bridgeName,
		FailMode: ptr.To(vswitchd.BridgeFailModeStandalone),
		ExternalIDs: map[string]string{
			"bridge-id":     bridgeName,
			"bridge-uplink": uplinkName,
		},
		OtherConfig: map[string]string{
			"hwaddr": hwaddr,
		},
	}
	// Internal bridge port/interface (named after the bridge).
	internalIface := &vswitchd.Interface{Name: bridgeName, Type: "internal"}
	internalPort := &vswitchd.Port{Name: bridgeName}
	// Uplink port/interface.
	uplinkIface := &vswitchd.Interface{Name: uplinkName}
	uplinkPort := &vswitchd.Port{
		Name:        uplinkName,
		OtherConfig: map[string]string{"transient": "true"},
	}

	internalIfaceModel := operationModel{
		Model:          internalIface,
		OnModelUpdates: []interface{}{&internalIface.Type},
		DoAfter: func() {
			internalPort.Interfaces = []string{internalIface.UUID}
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	internalPortModel := operationModel{
		Model:          internalPort,
		OnModelUpdates: []interface{}{&internalPort.Interfaces},
		DoAfter: func() {
			bridge.Ports = append(bridge.Ports, internalPort.UUID)
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	uplinkIfaceModel := operationModel{
		Model: uplinkIface,
		DoAfter: func() {
			uplinkPort.Interfaces = []string{uplinkIface.UUID}
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	uplinkPortModel := operationModel{
		Model:            uplinkPort,
		OnModelUpdates:   []interface{}{&uplinkPort.Interfaces},
		OnModelMutations: []interface{}{&uplinkPort.OtherConfig},
		DoAfter: func() {
			bridge.Ports = append(bridge.Ports, uplinkPort.UUID)
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	ovs := &vswitchd.OpenvSwitch{}
	bridgeModel := operationModel{
		Model:            bridge,
		OnModelUpdates:   []interface{}{&bridge.FailMode},
		OnModelMutations: []interface{}{&bridge.ExternalIDs, &bridge.OtherConfig, &bridge.Ports},
		DoAfter: func() {
			ovs.Bridges = []string{bridge.UUID}
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	ovsModel := operationModel{
		Model:            ovs,
		ModelPredicate:   func(*vswitchd.OpenvSwitch) bool { return true },
		OnModelMutations: []interface{}{&ovs.Bridges},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(ovsClient)
	ops, err := m.CreateOrUpdateOps(nil,
		internalIfaceModel, internalPortModel,
		uplinkIfaceModel, uplinkPortModel,
		bridgeModel, ovsModel)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(ovsClient, ops)
	return err
}

// GetBridge looks up an OVS bridge by name. This is the libovsdb equivalent of
// `ovs-vsctl br-exists <name>` plus `ovs-vsctl list Bridge <name>`.
func GetBridge(ovsClient libovsdbclient.Client, name string) (*vswitchd.Bridge, error) {
	found := []*vswitchd.Bridge{}
	opModel := operationModel{
		Model:          &vswitchd.Bridge{Name: name},
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(ovsClient)
	if err := m.Lookup(opModel); err != nil {
		return nil, err
	}

	return found[0], nil
}

// DeleteBridge deletes an OVS bridge and detaches it from the Open_vSwitch root
// row. OVSDB garbage collection removes Port and Interface rows that become
// unreachable through the bridge's strong references. It is idempotent: a
// missing bridge is not an error. This is the libovsdb equivalent of
// `ovs-vsctl --if-exists del-br <name>`.
func DeleteBridge(ovsClient libovsdbclient.Client, bridgeName string) error {
	ops, err := DeleteBridgeOps(ovsClient, nil, bridgeName)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return nil
	}
	_, err = TransactAndCheck(ovsClient, ops)
	return err
}

// DeleteBridgeOps returns operations to delete an OVS bridge and detach it from
// the Open_vSwitch root row's bridges set. A missing bridge yields zero
// operations.
func DeleteBridgeOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, bridgeName string) ([]ovsdb.Operation, error) {
	bridge, err := GetBridge(ovsClient, bridgeName)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return ops, nil
		}
		return nil, err
	}

	m := newModelClient(ovsClient)
	ovs := &vswitchd.OpenvSwitch{Bridges: []string{bridge.UUID}}
	return m.DeleteOps(ops,
		operationModel{
			Model:            ovs,
			ModelPredicate:   func(*vswitchd.OpenvSwitch) bool { return true },
			OnModelMutations: []interface{}{&ovs.Bridges},
			ErrNotFound:      false,
			BulkOp:           false,
		},
		operationModel{
			Model:       bridge,
			ErrNotFound: false,
			BulkOp:      false,
		},
	)
}

// GetOVSInterface looks up an OVS interface by name. Returns ErrNotFound if no
// interface with that name exists. This is the libovsdb equivalent of
// `ovs-vsctl find Interface name=<name>`.
func GetOVSInterface(ovsClient libovsdbclient.Client, name string) (*vswitchd.Interface, error) {
	found := []*vswitchd.Interface{}
	opModel := operationModel{
		Model:          &vswitchd.Interface{Name: name},
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(ovsClient)
	if err := m.Lookup(opModel); err != nil {
		return nil, err
	}

	return found[0], nil
}

// ListInterfaces looks up all OVS interfaces from the cache. This is the
// libovsdb equivalent of `ovs-vsctl list Interface`.
func ListInterfaces(ovsClient libovsdbclient.Client) ([]*vswitchd.Interface, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedInterfaces := []*vswitchd.Interface{}
	err := ovsClient.List(ctx, &searchedInterfaces)
	return searchedInterfaces, err
}

// FindInterfacesWithPredicate returns all OVS interfaces in the cache that
// match the predicate. This is the libovsdb equivalent of
// `ovs-vsctl find Interface <conditions>`.
func FindInterfacesWithPredicate(ovsClient libovsdbclient.Client, p ovsInterfacePredicate) ([]*vswitchd.Interface, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedInterfaces := []*vswitchd.Interface{}

	err := ovsClient.WhereCache(p).List(ctx, &searchedInterfaces)
	return searchedInterfaces, err
}

// GetOVSPort looks up an OVS port by name. Returns ErrNotFound if no port with
// that name exists. This is the libovsdb equivalent of
// `ovs-vsctl find Port name=<name>`.
func GetOVSPort(ovsClient libovsdbclient.Client, name string) (*vswitchd.Port, error) {
	found := []*vswitchd.Port{}
	opModel := operationModel{
		Model:          &vswitchd.Port{Name: name},
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(ovsClient)
	if err := m.Lookup(opModel); err != nil {
		return nil, err
	}

	return found[0], nil
}

// CreateOrUpdatePortWithInterface creates or updates an OVS port and its interface on a bridge.
// This creates both the Port and Interface objects atomically in a single transaction,
// and attaches the port to the specified bridge.
// The interface type is set to "internal". This is the libovsdb equivalent of:
//
//	ovs-vsctl --may-exist add-port <bridge> <port>
//	          -- set Interface <port> type=internal external_ids:...
func CreateOrUpdatePortWithInterface(ovsClient libovsdbclient.Client, bridgeName, portName string, portExternalIDs, ifaceExternalIDs map[string]string) error {
	ops, err := CreateOrUpdatePortWithInterfaceOps(ovsClient, nil, bridgeName, portName, portExternalIDs, ifaceExternalIDs)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(ovsClient, ops)
	return err
}

// CreateOrUpdatePortWithInterfaceOps returns operations to create or update an OVS port and its interface.
// OVS uses the following hierarchy: Bridge/Port/Interface. A port is referenced by a bridge,
// and an interface is referenced by a port.
func CreateOrUpdatePortWithInterfaceOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, bridgeName, portName string, portExternalIDs, ifaceExternalIDs map[string]string) ([]ovsdb.Operation, error) {
	iface := &vswitchd.Interface{Name: portName, Type: "internal", ExternalIDs: ifaceExternalIDs}
	port := &vswitchd.Port{Name: portName, ExternalIDs: portExternalIDs}
	bridge := &vswitchd.Bridge{Name: bridgeName}

	// Interface model - DoAfter captures UUID for port reference
	ifaceModel := operationModel{
		Model:          iface,
		OnModelUpdates: []interface{}{&iface.Type, &iface.ExternalIDs},
		DoAfter: func() {
			port.Interfaces = []string{iface.UUID}
		},
		ErrNotFound: false,
		BulkOp:      false,
	}

	// Port model - DoAfter captures UUID for bridge reference
	portModel := operationModel{
		Model:          port,
		OnModelUpdates: []interface{}{&port.ExternalIDs},
		DoAfter: func() {
			bridge.Ports = append(bridge.Ports, port.UUID)
		},
		ErrNotFound: false,
		BulkOp:      false,
	}

	// Bridge model - mutates Ports to add our port
	bridgeModel := operationModel{
		Model:            bridge,
		OnModelMutations: []interface{}{&bridge.Ports},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(ovsClient)
	return m.CreateOrUpdateOps(ops, ifaceModel, portModel, bridgeModel)
}

// CreateOrUpdatePodPort creates or updates an OVS port and its single backing
// interface on bridgeName in one transaction. The helper updates these
// columns from the caller-supplied models:
//   - Port: OtherConfig, ExternalIDs
//   - Interface: Type, Options, MTURequest, ExternalIDs
//
// Both port.Name and iface.Name are forced to portName for consistency.
// On create, the model is written as-is; on update, only the listed columns
// are touched. This is the libovsdb equivalent of an `ovs-vsctl --may-exist
// add-port BRIDGE PORT -- set Interface PORT key=value ...` chain.
func CreateOrUpdatePodPort(ovsClient libovsdbclient.Client, bridgeName, portName string, port *vswitchd.Port, iface *vswitchd.Interface) error {
	ops, err := CreateOrUpdatePodPortOps(ovsClient, nil, bridgeName, portName, port, iface)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(ovsClient, ops)
	return err
}

// CreateOrUpdatePodPortOps returns the operations to create or update a pod's
// OVS port and interface for chaining into a larger transaction. See
// CreateOrUpdatePodPort for semantics.
func CreateOrUpdatePodPortOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, bridgeName, portName string, port *vswitchd.Port, iface *vswitchd.Interface) ([]ovsdb.Operation, error) {
	if port == nil {
		return nil, fmt.Errorf("CreateOrUpdatePodPortOps: nil port")
	}
	if iface == nil {
		return nil, fmt.Errorf("CreateOrUpdatePodPortOps: nil iface")
	}
	// Reject ports that already live on a different bridge. OVS's schema
	// does not enforce one-bridge-per-port (Bridge.ports is a strong-set
	// with no Port back-reference), so without this check the mutation
	// below would leave the same Port UUID in two Bridges' ports lists.
	// This matches the user-space safety check that `ovs-vsctl
	// --may-exist add-port BRIDGE PORT` performs.
	existing, err := GetPortBridge(ovsClient, portName)
	if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil, err
	}
	if existing != nil && existing.Name != bridgeName {
		return nil, fmt.Errorf("port %q is already attached to bridge %q", portName, existing.Name)
	}

	iface.Name = portName
	port.Name = portName
	bridge := &vswitchd.Bridge{Name: bridgeName}

	ifaceModel := operationModel{
		Model:          iface,
		OnModelUpdates: []interface{}{&iface.Type, &iface.Options, &iface.MTURequest, &iface.ExternalIDs},
		DoAfter: func() {
			port.Interfaces = []string{iface.UUID}
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	portModel := operationModel{
		Model:          port,
		OnModelUpdates: []interface{}{&port.Interfaces, &port.OtherConfig, &port.ExternalIDs},
		DoAfter: func() {
			bridge.Ports = append(bridge.Ports, port.UUID)
		},
		ErrNotFound: false,
		BulkOp:      false,
	}
	bridgeModel := operationModel{
		Model:            bridge,
		OnModelMutations: []interface{}{&bridge.Ports},
		ErrNotFound:      true,
		BulkOp:           false,
	}

	m := newModelClient(ovsClient)
	return m.CreateOrUpdateOps(ops, ifaceModel, portModel, bridgeModel)
}

// DeletePortWithInterfaces deletes an OVS port and all its interfaces from a
// bridge. The deletion is scoped to bridgeName: a port that exists with the
// given name but is attached to a different bridge is left untouched, matching
// the semantics of `ovs-vsctl --if-exists --with-iface del-port <bridge>
// <port>`. The function is idempotent - a missing port, or a missing bridge,
// are both no-ops.
func DeletePortWithInterfaces(ovsClient libovsdbclient.Client, bridgeName, portName string) error {
	port, err := GetOVSPort(ovsClient, portName)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil // Port doesn't exist, nothing to delete
		}
		return err
	}
	bridge, err := GetBridge(ovsClient, bridgeName)
	if err != nil {
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil // Bridge gone; nothing to detach from
		}
		return err
	}
	onBridge := false
	for _, uuid := range bridge.Ports {
		if uuid == port.UUID {
			onBridge = true
			break
		}
	}
	if !onBridge {
		return nil // Port lives on a different bridge; out of scope
	}
	ops, err := DeletePortWithInterfacesOps(ovsClient, nil, port, bridgeName)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return nil // Port doesn't exist
	}
	_, err = TransactAndCheck(ovsClient, ops)
	return err
}

// DeletePortWithInterfacesOps returns operations to delete an OVS port and all its interfaces.
// This handles both the Port and Interface objects, and detaches the port from the bridge.
func DeletePortWithInterfacesOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, port *vswitchd.Port, bridgeName string) ([]ovsdb.Operation, error) {
	bridge := &vswitchd.Bridge{Name: bridgeName}

	m := newModelClient(ovsClient)
	if len(port.Interfaces) > 0 {
		interfaceUUIDs := make(map[string]struct{}, len(port.Interfaces))
		for _, ifaceUUID := range port.Interfaces {
			interfaceUUIDs[ifaceUUID] = struct{}{}
		}
		interfaces, err := FindInterfacesWithPredicate(ovsClient, func(iface *vswitchd.Interface) bool {
			_, ok := interfaceUUIDs[iface.UUID]
			return ok
		})
		if err != nil {
			return nil, fmt.Errorf("failed to find interfaces for port %q: %w", port.Name, err)
		}
		for _, iface := range interfaces {
			ifaceOps, err := m.delete(iface)
			if err != nil {
				return nil, fmt.Errorf("failed to build delete interface ops: %w", err)
			}
			ops = append(ops, ifaceOps...)
		}
	}

	// Delete port and remove from bridge
	bridge.Ports = []string{port.UUID}
	portModel := operationModel{
		Model:       port,
		ErrNotFound: false,
		BulkOp:      false,
	}
	bridgeModel := operationModel{
		Model:            bridge,
		OnModelMutations: []interface{}{&bridge.Ports},
		ErrNotFound:      false,
		BulkOp:           false,
	}

	return m.DeleteOps(ops, portModel, bridgeModel)
}
