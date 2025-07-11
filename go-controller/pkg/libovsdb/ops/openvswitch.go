package ops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// Get OpenvSwitch entry from the cache
func GetOpenvSwitch(ovsClient libovsdbclient.Client) (*vswitchd.OpenvSwitch, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	openvSwitchList := []*vswitchd.OpenvSwitch{}
	err := ovsClient.List(ctx, &openvSwitchList)
	if err != nil {
		return nil, err
	}
	if len(openvSwitchList) == 0 {
		return nil, fmt.Errorf("no openvSwitch entry found")
	}

	return openvSwitchList[0], err
}

func UpdateOpenvSwitchSetExternalIDsOps(ovsdbClient libovsdbclient.Client, ops []ovsdb.Operation,
	vswitch *vswitchd.OpenvSwitch) ([]ovsdb.Operation, error) {
	externalIds := vswitch.ExternalIDs
	vswitch, err := GetOpenvSwitch(ovsdbClient)
	if err != nil {
		return nil, err
	}

	if vswitch.ExternalIDs == nil {
		vswitch.ExternalIDs = map[string]string{}
	}

	for k, v := range externalIds {
		if v == "" {
			delete(vswitch.ExternalIDs, k)
		} else {
			vswitch.ExternalIDs[k] = v
		}
	}

	opModel := operationModel{
		Model:          vswitch,
		OnModelUpdates: []any{&vswitch.ExternalIDs},
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(ovsdbClient)
	return m.CreateOrUpdateOps(ops, opModel)
}

func UpdateOpenvSwitchSetExternalIDs(ovsdbClient libovsdbclient.Client, vswitch *vswitchd.OpenvSwitch) error {
	ops, err := UpdateOpenvSwitchSetExternalIDsOps(ovsdbClient, nil, vswitch)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(ovsdbClient, ops)
	return err
}
