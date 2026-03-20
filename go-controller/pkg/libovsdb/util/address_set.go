package util

import (
	"fmt"
	"strings"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

// DeleteAddrSetsWithoutACLRef deletes the address sets related to the predicateIDs without any acl reference.
func DeleteAddrSetsWithoutACLRef(predicateIDs *libovsdbops.DbObjectIDs, nbClient libovsdbclient.Client) error {
	return deleteAddrSetsWithoutACLRef(predicateIDs, nil, nbClient)
}

func DeleteAddrSetsWithoutACLRefAnyController(dbOwnerType *libovsdbops.ObjectIDsType, nbClient libovsdbclient.Client) error {
	return deleteAddrSetsWithoutACLRef(nil, dbOwnerType, nbClient)
}

func deleteAddrSetsWithoutACLRef(predicateIDs *libovsdbops.DbObjectIDs, dbOwnerType *libovsdbops.ObjectIDsType, nbClient libovsdbclient.Client) error {
	// Get the list of existing address sets for the predicateIDs. Fill the address set
	// names and mark them as unreferenced.
	addrSetReferenced := map[string]bool{}
	// this is used for new style controllers that work for all controller/network names
	var predicate func(item *nbdb.AddressSet) bool
	if predicateIDs == nil {
		predicate = libovsdbops.GetAnyControllerPredicate[*nbdb.AddressSet](dbOwnerType, func(item *nbdb.AddressSet) bool {
			addrSetReferenced[item.Name] = false
			return false
		})
	} else {
		predicate = libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, func(item *nbdb.AddressSet) bool {
			addrSetReferenced[item.Name] = false
			return false
		})
	}

	_, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, predicate)
	if err != nil {
		return fmt.Errorf("failed to find address sets with predicate: %w", err)
	}

	// Set addrSetReferenced[addrSetName] = true if referencing acl exists.
	_, err = libovsdbops.FindACLsWithPredicate(nbClient, func(item *nbdb.ACL) bool {
		for addrSetName := range addrSetReferenced {
			if strings.Contains(item.Match, addrSetName) {
				addrSetReferenced[addrSetName] = true
			}
		}
		return false
	})
	if err != nil {
		return fmt.Errorf("cannot find ACLs referencing address set: %v", err)
	}

	// Iterate through each address set and if an address set is not referenced by any
	// acl then delete it.
	ops := []ovsdb.Operation{}
	for addrSetName, isReferenced := range addrSetReferenced {
		if !isReferenced {
			// No references for stale address set, delete.
			ops, err = libovsdbops.DeleteAddressSetsOps(nbClient, ops, &nbdb.AddressSet{
				Name: addrSetName,
			})
			if err != nil {
				return fmt.Errorf("failed to get delete address set ops: %w", err)
			}
		}
	}

	// Delete the stale address sets.
	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to transact db ops to delete address sets: %v", err)
	}
	return nil
}
