package ops

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
)

// ListChassis looks up all chassis from the cache
func ListChassis(sbClient libovsdbclient.Client) ([]*sbdb.Chassis, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	searchedChassis := []*sbdb.Chassis{}
	err := sbClient.List(ctx, &searchedChassis)
	return searchedChassis, err
}

// ListChassisPrivate looks up all chassis private models from the cache
func ListChassisPrivate(sbClient libovsdbclient.Client) ([]*sbdb.ChassisPrivate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	found := []*sbdb.ChassisPrivate{}
	err := sbClient.List(ctx, &found)
	return found, err
}

// GetChassis looks up a chassis from the cache using the 'Name' column which is an indexed
// column.
func GetChassis(sbClient libovsdbclient.Client, chassis *sbdb.Chassis) (*sbdb.Chassis, error) {
	found := []*sbdb.Chassis{}
	opModel := operationModel{
		Model:          chassis,
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(sbClient)
	err := m.Lookup(opModel)
	if err != nil {
		return nil, err
	}

	return found[0], nil
}

// DeleteChassis deletes the provided chassis and associated private chassis
func DeleteChassis(sbClient libovsdbclient.Client, chassis ...*sbdb.Chassis) error {
	opModels := make([]operationModel, 0, len(chassis))
	for i := range chassis {
		foundChassis := []*sbdb.Chassis{}
		chassisPrivate := sbdb.ChassisPrivate{
			Name: chassis[i].Name,
		}
		chassisUUID := ""
		opModel := []operationModel{
			{
				Model:          chassis[i],
				ExistingResult: &foundChassis,
				ErrNotFound:    false,
				BulkOp:         false,
				DoAfter: func() {
					if len(foundChassis) > 0 {
						chassisPrivate.Name = foundChassis[0].Name
						chassisUUID = foundChassis[0].UUID
					}
				},
			},
			{
				Model:       &chassisPrivate,
				ErrNotFound: false,
				BulkOp:      false,
			},
			// IGMPGroup has a weak link to chassis, deleting multiple chassis may result in IGMP_Groups
			// with identical values on columns "address", "datapath", and "chassis", when "chassis" goes empty
			{
				Model: &sbdb.IGMPGroup{},
				ModelPredicate: func(group *sbdb.IGMPGroup) bool {
					return group.Chassis != nil && chassisUUID != "" && *group.Chassis == chassisUUID
				},
				ErrNotFound: false,
				BulkOp:      true,
			},
		}
		opModels = append(opModels, opModel...)
	}

	m := newModelClient(sbClient)
	err := m.Delete(opModels...)
	return err
}

type chassisPredicate func(*sbdb.Chassis) bool

// DeleteChassisWithPredicate looks up chassis from the cache based on a given
// predicate and deletes them as well as the associated private chassis
func DeleteChassisWithPredicate(sbClient libovsdbclient.Client, p chassisPredicate) error {
	foundChassis := []*sbdb.Chassis{}
	foundChassisNames := sets.NewString()
	foundChassisUUIDS := sets.NewString()
	opModels := []operationModel{
		{
			Model:          &sbdb.Chassis{},
			ModelPredicate: p,
			ExistingResult: &foundChassis,
			ErrNotFound:    false,
			BulkOp:         true,
			DoAfter: func() {
				for _, chassis := range foundChassis {
					foundChassisNames.Insert(chassis.Name)
					foundChassisUUIDS.Insert(chassis.UUID)
				}
			},
		},
		{
			Model:          &sbdb.ChassisPrivate{},
			ModelPredicate: func(item *sbdb.ChassisPrivate) bool { return foundChassisNames.Has(item.Name) },
			ErrNotFound:    false,
			BulkOp:         true,
		},
		// IGMPGroup has a weak link to chassis, deleting multiple chassis may result in IGMP_Groups
		// with identical values on columns "address", "datapath", and "chassis", when "chassis" goes empty
		{
			Model:          &sbdb.IGMPGroup{},
			ModelPredicate: func(group *sbdb.IGMPGroup) bool { return group.Chassis != nil && foundChassisUUIDS.Has(*group.Chassis) },
			ErrNotFound:    false,
			BulkOp:         true,
		},
	}
	m := newModelClient(sbClient)
	err := m.Delete(opModels...)
	return err
}

// CreateOrUpdateChassis creates or updates the chassis record along with the encap record
func CreateOrUpdateChassis(sbClient libovsdbclient.Client, chassis *sbdb.Chassis, encaps ...*sbdb.Encap) error {
	m := newModelClient(sbClient)
	opModels := make([]operationModel, 0, len(encaps)+1)
	for i := range encaps {
		encap := encaps[i]
		opModel := operationModel{
			Model: encap,
			DoAfter: func() {
				encapsList := append(chassis.Encaps, encap.UUID)
				chassis.Encaps = sets.New(encapsList...).UnsortedList()
			},
			OnModelUpdates: onModelUpdatesNone(),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	opModel := operationModel{
		Model:            chassis,
		OnModelMutations: []interface{}{&chassis.OtherConfig},
		OnModelUpdates:   []interface{}{&chassis.Encaps},
		ErrNotFound:      false,
		BulkOp:           false,
	}

	opModels = append(opModels, opModel)
	if _, err := m.CreateOrUpdate(opModels...); err != nil {
		return err
	}

	return nil
}

// validateRequestedChassisOption is a guard to ensure a caller is using the chassis-id (uuid format)
// for the requested chassis option.
func validateRequestedChassisOption(options map[string]string) error {
	if len(options) == 0 {
		return nil
	}
	chassisID, ok := options[RequestedChassis]
	if !ok || chassisID == "" {
		return nil
	}
	if _, err := uuid.Parse(chassisID); err != nil {
		return fmt.Errorf("requested-chassis must be a valid UUID, got %q", chassisID)
	}
	return nil
}
