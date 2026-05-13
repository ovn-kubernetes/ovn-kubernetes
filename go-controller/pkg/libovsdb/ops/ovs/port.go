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

// GetOVSPort: `ovs-vsctl find Port name=<name>`.
func GetOVSPort(ovsClient libovsdbclient.Client, name string) (*vswitchd.Port, error) {
	return libovsdbops.GetOVSPort(ovsClient, name)
}

// GetBridgeContainingPort: `ovs-vsctl port-to-br <port>`.
func GetBridgeContainingPort(ovsClient libovsdbclient.Client, portName string) (*vswitchd.Bridge, error) {
	return libovsdbops.GetBridgeContainingPort(ovsClient, portName)
}

// FindOVSPortsWithPredicate: `ovs-vsctl find Port <conditions>`.
func FindOVSPortsWithPredicate(ovsClient libovsdbclient.Client, p portPredicate) ([]*vswitchd.Port, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	var ports []*vswitchd.Port
	err := ovsClient.WhereCache(p).List(ctx, &ports)
	return ports, err
}

// CreateOrUpdatePortWithInterface: `ovs-vsctl --may-exist add-port <br> <port>
// -- set Interface <port> type=internal external_ids:...`.
func CreateOrUpdatePortWithInterface(ovsClient libovsdbclient.Client, bridgeName, portName string, portExternalIDs, ifaceExternalIDs map[string]string) error {
	return libovsdbops.CreateOrUpdatePortWithInterface(ovsClient, bridgeName, portName, portExternalIDs, ifaceExternalIDs)
}

// CreateOrUpdatePodPort: `ovs-vsctl --may-exist add-port <br> <port> -- set
// Interface <port> type=... mtu_request=... external_ids:...`.
func CreateOrUpdatePodPort(ovsClient libovsdbclient.Client, bridgeName, portName string, port *vswitchd.Port, iface *vswitchd.Interface) error {
	return libovsdbops.CreateOrUpdatePodPort(ovsClient, bridgeName, portName, port, iface)
}

// CreateOrUpdatePodPortOps: see CreateOrUpdatePodPort.
func CreateOrUpdatePodPortOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, bridgeName, portName string, port *vswitchd.Port, iface *vswitchd.Interface) ([]ovsdb.Operation, error) {
	return libovsdbops.CreateOrUpdatePodPortOps(ovsClient, ops, bridgeName, portName, port, iface)
}

// CreateOrUpdatePortWithInterfaceOps: see CreateOrUpdatePortWithInterface.
func CreateOrUpdatePortWithInterfaceOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, bridgeName, portName string, portExternalIDs, ifaceExternalIDs map[string]string) ([]ovsdb.Operation, error) {
	return libovsdbops.CreateOrUpdatePortWithInterfaceOps(ovsClient, ops, bridgeName, portName, portExternalIDs, ifaceExternalIDs)
}

// DeletePortWithInterfaces: `ovs-vsctl --if-exists --with-iface del-port <br> <port>`.
func DeletePortWithInterfaces(ovsClient libovsdbclient.Client, bridgeName, portName string) error {
	return libovsdbops.DeletePortWithInterfaces(ovsClient, bridgeName, portName)
}

// DeletePortWithInterfacesOps: see DeletePortWithInterfaces.
func DeletePortWithInterfacesOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, port *vswitchd.Port, bridgeName string) ([]ovsdb.Operation, error) {
	return libovsdbops.DeletePortWithInterfacesOps(ovsClient, ops, port, bridgeName)
}
