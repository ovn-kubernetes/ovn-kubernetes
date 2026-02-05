package ops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// OVS port predicates for filtering
type ovsPortPredicate func(*vswitchd.Port) bool

// FindOVSPortsWithPredicate returns all OVS ports matching the predicate.
func FindOVSPortsWithPredicate(ovsClient libovsdbclient.Client, p ovsPortPredicate) ([]*vswitchd.Port, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	var ports []*vswitchd.Port
	err := ovsClient.WhereCache(p).List(ctx, &ports)
	return ports, err
}

// GetOVSPort looks up an OVS port by name.
func GetOVSPort(ovsClient libovsdbclient.Client, name string) (*vswitchd.Port, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	port := &vswitchd.Port{Name: name}
	err := ovsClient.Get(ctx, port)
	if err != nil {
		return nil, err
	}
	return port, nil
}

// CreateOrUpdatePortWithInterface creates or updates an OVS port and its interface on a bridge.
// This creates both the Port and Interface objects atomically in a single transaction,
// and attaches the port to the specified bridge.
// The interface type is set to "internal".
func CreateOrUpdatePortWithInterface(ovsClient libovsdbclient.Client, bridgeName, portName string, externalIDs map[string]string) error {
	ops, err := CreateOrUpdatePortWithInterfaceOps(ovsClient, nil, bridgeName, portName, externalIDs)
	if err != nil {
		return err
	}
	_, err = TransactAndCheck(ovsClient, ops)
	return err
}

// CreateOrUpdatePortWithInterfaceOps returns operations to create or update an OVS port and its interface.
// This handles both the Port and Interface objects, and attaches the port to the bridge.
func CreateOrUpdatePortWithInterfaceOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, bridgeName, portName string, externalIDs map[string]string) ([]ovsdb.Operation, error) {
	iface := &vswitchd.Interface{Name: portName, Type: "internal"}
	port := &vswitchd.Port{Name: portName, ExternalIDs: externalIDs}
	bridge := &vswitchd.Bridge{Name: bridgeName}

	// Interface model - DoAfter captures UUID for port reference
	ifaceModel := operationModel{
		Model:          iface,
		OnModelUpdates: []interface{}{&iface.Type},
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

// DeletePortWithInterfaces deletes an OVS port and all its interfaces from a bridge.
// This removes both the Port and Interface objects, and detaches the port from the bridge.
// This function is idempotent - it's safe to call even if the port doesn't exist.
func DeletePortWithInterfaces(ovsClient libovsdbclient.Client, bridgeName, portName string) error {
	ops, err := DeletePortWithInterfacesOps(ovsClient, nil, bridgeName, portName)
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
// Returns empty ops if the port doesn't exist (idempotent).
func DeletePortWithInterfacesOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, bridgeName, portName string) ([]ovsdb.Operation, error) {
	// Look up the port - if not found, nothing to do
	port, err := GetOVSPort(ovsClient, portName)
	if err != nil {
		if err == libovsdbclient.ErrNotFound {
			return ops, nil // Port doesn't exist, nothing to delete
		}
		return nil, fmt.Errorf("failed to lookup port %s: %w", portName, err)
	}

	bridge := &vswitchd.Bridge{Name: bridgeName}

	// Delete interfaces
	for _, ifaceUUID := range port.Interfaces {
		iface := &vswitchd.Interface{UUID: ifaceUUID}
		ifaceModel := operationModel{
			Model:       iface,
			ErrNotFound: false,
			BulkOp:      false,
		}
		m := newModelClient(ovsClient)
		var err error
		ops, err = m.DeleteOps(ops, ifaceModel)
		if err != nil {
			return nil, fmt.Errorf("failed to build delete interface ops: %w", err)
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

	m := newModelClient(ovsClient)
	return m.DeleteOps(ops, portModel, bridgeModel)
}
