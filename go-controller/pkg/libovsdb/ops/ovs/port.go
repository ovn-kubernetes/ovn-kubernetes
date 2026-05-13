// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovs

import (
	"context"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type portPredicate func(*vswitchd.Port) bool

// GetOVSPort looks up an OVS port by name.
func GetOVSPort(ovsClient libovsdbclient.Client, name string) (*vswitchd.Port, error) {
	return libovsdbops.GetOVSPort(ovsClient, name)
}

// GetBridgeContainingPort returns the OVS bridge that owns the named port,
// the libovsdb equivalent of `ovs-vsctl port-to-br <port>`. Returns
// ErrNotFound if the port does not exist or no bridge references it.
func GetBridgeContainingPort(ovsClient libovsdbclient.Client, portName string) (*vswitchd.Bridge, error) {
	return libovsdbops.GetBridgeContainingPort(ovsClient, portName)
}

// FindOVSPortsWithPredicate returns all OVS ports matching the predicate.
func FindOVSPortsWithPredicate(ovsClient libovsdbclient.Client, p portPredicate) ([]*vswitchd.Port, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	var ports []*vswitchd.Port
	err := ovsClient.WhereCache(p).List(ctx, &ports)
	return ports, err
}

// CreateOrUpdatePortWithInterface creates or updates an OVS port and its
// (single, type=internal) interface on a bridge in one transaction.
func CreateOrUpdatePortWithInterface(ovsClient libovsdbclient.Client, bridgeName, portName string, portExternalIDs, ifaceExternalIDs map[string]string) error {
	return libovsdbops.CreateOrUpdatePortWithInterface(ovsClient, bridgeName, portName, portExternalIDs, ifaceExternalIDs)
}

// CreateOrUpdatePortWithInterfaceOps returns operations to create or update an
// OVS port and its interface for chaining into a larger transaction.
func CreateOrUpdatePortWithInterfaceOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, bridgeName, portName string, portExternalIDs, ifaceExternalIDs map[string]string) ([]ovsdb.Operation, error) {
	return libovsdbops.CreateOrUpdatePortWithInterfaceOps(ovsClient, ops, bridgeName, portName, portExternalIDs, ifaceExternalIDs)
}

// DeletePortWithInterfaces deletes an OVS port and all its interfaces from a
// bridge. Idempotent: a missing port is not an error. This is the libovsdb
// equivalent of `ovs-vsctl --if-exists --with-iface del-port <bridge> <port>`.
func DeletePortWithInterfaces(ovsClient libovsdbclient.Client, bridgeName, portName string) error {
	return libovsdbops.DeletePortWithInterfaces(ovsClient, bridgeName, portName)
}

// DeletePortWithInterfacesOps returns operations to delete an OVS port and its
// interfaces for chaining into a larger transaction.
func DeletePortWithInterfacesOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, port *vswitchd.Port, bridgeName string) ([]ovsdb.Operation, error) {
	return libovsdbops.DeletePortWithInterfacesOps(ovsClient, ops, port, bridgeName)
}
