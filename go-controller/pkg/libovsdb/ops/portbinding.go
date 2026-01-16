package ops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
)

type encapPredicate func(item *sbdb.Encap) bool

func FindEncapWithPredicate(sbClient libovsdbclient.Client, predicate encapPredicate) ([]*sbdb.Encap, error) {
	found := []*sbdb.Encap{}
	opModel := operationModel{
		ModelPredicate: predicate,
		ExistingResult: &found,
		ErrNotFound:    false,
		BulkOp:         false,
	}

	m := newModelClient(sbClient)
	err := m.Lookup(opModel)
	if err != nil {
		return nil, err
	}
	return found, nil
}

// UpdatePortBindingSetEncap updates the port binding's encap field for the given LSP.
// TODO: A elegant way to handle this is to set the requested-encap-ip option in the LSP.
// Then, ovn-northd can populate the corresponding Port Binding’s encap field accordingly.
func UpdatePortBindingSetEncap(sbClient libovsdbclient.Client, lspName, chassisName, encapIP string) error {
	encapPredicate := func(item *sbdb.Encap) bool {
		return item.ChassisName == chassisName && item.IP == encapIP
	}

	encaps, err := FindEncapWithPredicate(sbClient, encapPredicate)
	if err != nil {
		return fmt.Errorf("failed to find encap ip %s on chassis %s: %w", encapIP, chassisName, err)
	}
	if len(encaps) == 0 {
		return fmt.Errorf("can't find encap ip %s on chassis %s", encapIP, chassisName)
	}
	encap := encaps[0]

	_, err = waitForPortBinding(sbClient, lspName)
	if err != nil {
		return err
	}

	pb := &sbdb.PortBinding{
		LogicalPort: lspName,
	}
	if err = doUpdatePortBindingSetEncap(sbClient, pb, encap); err != nil {
		return fmt.Errorf("failed to update port binding for %s: %w", lspName, err)
	}

	return nil
}

func doUpdatePortBindingSetEncap(sbClient libovsdbclient.Client, portBinding *sbdb.PortBinding, encap *sbdb.Encap) error {

	portBinding.Encap = &encap.UUID

	opModel := operationModel{
		Model:          portBinding,
		OnModelUpdates: []interface{}{&portBinding.Encap},
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(sbClient)
	_, err := m.CreateOrUpdate(opModel)
	return err
}

func waitForPortBinding(sbClient libovsdbclient.Client, lspName string) (*sbdb.PortBinding, error) {
	var pb *sbdb.PortBinding
	var err error
	if err = wait.PollUntilContextTimeout(context.Background(), 200*time.Millisecond, 10*time.Second, true, func(_ context.Context) (bool, error) {
		pb, err = GetPortBinding(sbClient, &sbdb.PortBinding{LogicalPort: lspName})
		if err != nil {
			if errors.Is(err, libovsdbclient.ErrNotFound) {
				// wait for port binding to be created
				return false, nil
			}
			return false, fmt.Errorf("error getting port binding for %s: %w", lspName, err)
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("timed out waiting for Port Binding %s: %w", lspName, err)
	}

	return pb, nil
}

// GetPortBinding looks up a portBinding in SBDB
func GetPortBinding(sbClient libovsdbclient.Client, portBinding *sbdb.PortBinding) (*sbdb.PortBinding, error) {
	found := []*sbdb.PortBinding{}
	opModel := operationModel{
		Model:          portBinding,
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
