package util

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
)

// WaitUntilNorthdSyncOnce ensures northd has sync'd at least once by increments nb_cfg value in NB DB and waiting
// for northd to copy it to SB DB. Poll SB DB until context is cancelled.
// The expectation is that the data you wish to be sync'd to SB DB has already been written to NB DB so when we get the initial
// nb_cfg value, we know that if we increment that by one and see that value or greater in SB DB, then the data has sync'd.
// All other processes interacting with nb_cfg increment it. This function depends on other processes respecting that.
// No guarantee of any changes in SB DB made after this func.
func WaitUntilNorthdSyncOnce(ctx context.Context, nbClient, sbClient client.Client) error {
	// 1. Get value of nb_cfg
	// 2. Increment value of nb_cfg
	// 3. Wait until value appears in SB DB after northd copies it.
	nbGlobal := &nbdb.NBGlobal{}
	nbGlobal, err := libovsdbops.GetNBGlobal(nbClient, nbGlobal)
	if err != nil {
		return fmt.Errorf("failed to find OVN Northbound NB_Global table"+
			" entry: %w", err)
	}
	// increment nb_cfg value by 1. When northd consumes updates from NB DB, it will copy this value to SB DBs SB_Global table nb_cfg field.
	ops, err := nbClient.Where(nbGlobal).Mutate(nbGlobal, model.Mutation{
		Field:   &nbGlobal.NbCfg,
		Mutator: ovsdb.MutateOperationAdd,
		Value:   1,
	})
	if err != nil {
		return fmt.Errorf("failed to generate ops to mutate nb_cfg: %w", err)
	}
	expectedNbCfgValue := nbGlobal.NbCfg + 1
	if _, err = libovsdbops.TransactAndCheck(nbClient, ops); err != nil {
		return fmt.Errorf("failed to transact to increment nb_cfg: %w", err)
	}
	sbGlobal := &sbdb.SBGlobal{}
	// poll until we see the expected value in SB DB every 5 milliseconds until context is cancelled.
	err = wait.PollUntilContextCancel(ctx, time.Millisecond*5, true, func(_ context.Context) (done bool, err error) {
		if sbGlobal, err = libovsdbops.GetSBGlobal(sbClient, sbGlobal); err != nil {
			// northd hasn't added an entry yet
			if errors.Is(err, client.ErrNotFound) {
				return false, nil
			}
			return false, fmt.Errorf("failed to get sb_global table entry from SB DB: %w", err)
		}
		return sbGlobal.NbCfg >= expectedNbCfgValue, nil // we only need to ensure it is greater than or equal to the expected value
	})
	if err != nil {
		return fmt.Errorf("failed while waiting for nb_cfg value greater than or equal %d in sb db sb_global table: %w", expectedNbCfgValue, err)
	}
	return nil
}
