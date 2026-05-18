// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovs

import (
	"context"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type interfacePredicate func(*vswitchd.Interface) bool

// GetOVSInterface looks up an OVS interface by name.
// Equivalent: `ovs-vsctl find Interface name=<name>`.
func GetOVSInterface(ovsClient libovsdbclient.Client, name string) (*vswitchd.Interface, error) {
	return libovsdbops.GetOVSInterface(ovsClient, name)
}

// ListInterfaces looks up all ovs interfaces from the cache.
// Equivalent: `ovs-vsctl list Interface`.
func ListInterfaces(ovsClient libovsdbclient.Client) ([]*vswitchd.Interface, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedInterfaces := []*vswitchd.Interface{}
	err := ovsClient.List(ctx, &searchedInterfaces)
	return searchedInterfaces, err
}

// FindInterfacesWithPredicate returns all the ovs interfaces in the cache
// that matches the lookup function.
// Equivalent: `ovs-vsctl find Interface <conditions>`.
func FindInterfacesWithPredicate(ovsClient libovsdbclient.Client, p interfacePredicate) ([]*vswitchd.Interface, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedInterfaces := []*vswitchd.Interface{}

	err := ovsClient.WhereCache(p).List(ctx, &searchedInterfaces)
	return searchedInterfaces, err
}
