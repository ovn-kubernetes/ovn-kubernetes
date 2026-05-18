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

// ListBridges looks up all ovs bridges from the cache.
// Equivalent: `ovs-vsctl list-br`.
func ListBridges(ovsClient libovsdbclient.Client) ([]*vswitchd.Bridge, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedBridges := []*vswitchd.Bridge{}
	err := ovsClient.List(ctx, &searchedBridges)
	return searchedBridges, err
}

// GetBridge looks up an OVS bridge by name.
// Equivalent: `ovs-vsctl br-exists <name>` + `ovs-vsctl list Bridge <name>`.
func GetBridge(ovsClient libovsdbclient.Client, name string) (*vswitchd.Bridge, error) {
	return libovsdbops.GetBridge(ovsClient, name)
}

// CreateOrUpdateNicBridge ensures an OVS bridge exists with the given uplink
// port and configuration. See libovsdbops.CreateOrUpdateNicBridge for the
// columns touched and edge-case semantics.
// Equivalent:
//
//	ovs-vsctl -- --may-exist add-br <bridge>
//	          -- br-set-external-id <bridge> bridge-id <bridge>
//	          -- br-set-external-id <bridge> bridge-uplink <uplink>
//	          -- set bridge <bridge> fail-mode=standalone other_config:hwaddr=<hwaddr>
//	          -- --may-exist add-port <bridge> <uplink>
//	          -- set port <uplink> other-config:transient=true
func CreateOrUpdateNicBridge(ovsClient libovsdbclient.Client, bridgeName, uplinkName, hwaddr string) error {
	return libovsdbops.CreateOrUpdateNicBridge(ovsClient, bridgeName, uplinkName, hwaddr)
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
