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

// ListBridges looks up all ovs bridges from the cache
func ListBridges(ovsClient libovsdbclient.Client) ([]*vswitchd.Bridge, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedBridges := []*vswitchd.Bridge{}
	err := ovsClient.List(ctx, &searchedBridges)
	return searchedBridges, err
}

// GetBridge looks up an OVS bridge by name.
func GetBridge(ovsClient libovsdbclient.Client, name string) (*vswitchd.Bridge, error) {
	return libovsdbops.GetBridge(ovsClient, name)
}

// DeleteBridge deletes an OVS bridge and all its ports/interfaces, and
// detaches it from the Open_vSwitch root row. It is idempotent: a missing
// bridge is not an error. This is the libovsdb equivalent of
// `ovs-vsctl --if-exists del-br <name>`.
func DeleteBridge(ovsClient libovsdbclient.Client, bridgeName string) error {
	return libovsdbops.DeleteBridge(ovsClient, bridgeName)
}

// DeleteBridgeOps returns operations to delete an OVS bridge for chaining
// into a larger transaction. See DeleteBridge for semantics.
func DeleteBridgeOps(ovsClient libovsdbclient.Client, ops []ovsdb.Operation, bridgeName string) ([]ovsdb.Operation, error) {
	return libovsdbops.DeleteBridgeOps(ovsClient, ops, bridgeName)
}
