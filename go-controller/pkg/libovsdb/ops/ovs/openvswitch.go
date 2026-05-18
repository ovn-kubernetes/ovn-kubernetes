// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovs

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// GetOpenvSwitch returns the singleton Open_vSwitch row from the cache.
// When no row exists, the returned error wraps libovsdbclient.ErrNotFound so
// callers can detect that case via errors.Is.
// Equivalent: `ovs-vsctl list Open_vSwitch`.
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

// UpdateOpenvSwitchExternalIDs is the libovsdb equivalent of
// `ovs-vsctl set Open_vSwitch . external_ids:key=value ...`.
func UpdateOpenvSwitchExternalIDs(ovsClient libovsdbclient.Client, kv map[string]string) error {
	return libovsdbops.UpdateOpenvSwitchExternalIDs(ovsClient, kv)
}

// RemoveOpenvSwitchExternalIDs is the libovsdb equivalent of
// `ovs-vsctl --if-exists remove Open_vSwitch . external_ids <key> ...`.
func RemoveOpenvSwitchExternalIDs(ovsClient libovsdbclient.Client, keys ...string) error {
	return libovsdbops.RemoveOpenvSwitchExternalIDs(ovsClient, keys...)
}
