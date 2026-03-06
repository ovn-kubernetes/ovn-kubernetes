package ovs

import (
	"context"
	"errors"
	"fmt"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type interfacePredicate func(*vswitchd.Interface) bool

// ListInterfaces looks up all ovs interfaces from the cache
func ListInterfaces(ovsClient libovsdbclient.Client) ([]*vswitchd.Interface, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedInterfaces := []*vswitchd.Interface{}
	err := ovsClient.List(ctx, &searchedInterfaces)
	return searchedInterfaces, err
}

// FindInterfacesWithPredicate returns all the ovs interfaces in the cache
// that matches the lookup function
func FindInterfacesWithPredicate(ovsClient libovsdbclient.Client, p interfacePredicate) ([]*vswitchd.Interface, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedInterfaces := []*vswitchd.Interface{}

	err := ovsClient.WhereCache(p).List(ctx, &searchedInterfaces)
	return searchedInterfaces, err
}

func DeletePortsFromBridge(ovsClient libovsdbclient.Client, bridgeName string, interfaces ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()

	var ops []ovsdb.Operation
	portUUIDs := make([]string, 0, len(interfaces))

	for _, iface := range interfaces {
		port := &vswitchd.Port{Name: iface}
		if err := ovsClient.Get(ctx, port); err != nil {
			if errors.Is(err, libovsdbclient.ErrNotFound) {
				continue
			}
			return fmt.Errorf("failed to get port %s: %v", iface, err)
		}
		portUUIDs = append(portUUIDs, port.UUID)

		deletePortOps, err := ovsClient.Where(port).Delete()
		if err != nil {
			return fmt.Errorf("failed to build delete ops for port %s: %v", port.Name, err)
		}
		ops = append(ops, deletePortOps...)

		deleteIfaceOps, err := ovsClient.Where(&vswitchd.Interface{Name: iface}).Delete()
		if err != nil {
			return fmt.Errorf("failed to build delete ops for interface %s: %v", iface, err)
		}
		ops = append(ops, deleteIfaceOps...)
	}

	if len(portUUIDs) == 0 {
		return nil
	}

	bridge := &vswitchd.Bridge{Name: bridgeName}
	mutateOps, err := ovsClient.Where(bridge).Mutate(bridge, model.Mutation{
		Field:   &bridge.Ports,
		Mutator: ovsdb.MutateOperationDelete,
		Value:   portUUIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to build mutate ops for bridge %s: %v", bridgeName, err)
	}

	ops = append(ops, mutateOps...)

	_, err = libovsdbops.TransactAndCheck(ovsClient, ops)
	if err != nil {
		return fmt.Errorf("failed to delete ports from bridge %s: %v", bridgeName, err)
	}

	return nil
}

// DeletePortsFromBridgeWithPredicate finds all OVS interfaces matching the predicate,
// then removes their same-named ports from the specified bridge in a single
// transaction. Assumes a 1:1 port-to-interface mapping (non-bond ports).
// Equivalent to running `ovs-vsctl --if-exists del-port <bridge> <name>`
// for each matching interface. If no interfaces match, this is a no-op.
func DeletePortsFromBridgeWithPredicate(ovsClient libovsdbclient.Client, bridgeName string, p interfacePredicate) error {
	matchingIfaces, err := FindInterfacesWithPredicate(ovsClient, p)
	if err != nil {
		return fmt.Errorf("failed to find interfaces on bridge %s: %v", bridgeName, err)
	}
	if len(matchingIfaces) == 0 {
		return nil
	}

	interfaces := make([]string, 0, len(matchingIfaces))
	for _, iface := range matchingIfaces {
		interfaces = append(interfaces, iface.Name)
	}

	return DeletePortsFromBridge(ovsClient, bridgeName, interfaces...)
}
