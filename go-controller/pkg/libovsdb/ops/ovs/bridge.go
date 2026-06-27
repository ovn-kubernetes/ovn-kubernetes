// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovs

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

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

// GetBridge looks up an ovs bridge by name from the cache.
func GetBridge(ovsClient libovsdbclient.Client, name string) (*vswitchd.Bridge, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedBridges := []*vswitchd.Bridge{}
	err := ovsClient.WhereCache(func(bridge *vswitchd.Bridge) bool {
		return bridge.Name == name
	}).List(ctx, &searchedBridges)
	if err != nil {
		return nil, err
	}
	if len(searchedBridges) == 0 {
		return nil, fmt.Errorf("ovs bridge %s not found: %w", name, libovsdbclient.ErrNotFound)
	}
	return searchedBridges[0], nil
}
