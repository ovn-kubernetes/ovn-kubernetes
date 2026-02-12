package ops

import (
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
)

// GetNBGlobal looks up the SB Global entry from the cache
func GetSBGlobal(sbClient libovsdbclient.Client, sbGlobal *sbdb.SBGlobal) (*sbdb.SBGlobal, error) {
	found := []*sbdb.SBGlobal{}
	opModel := operationModel{
		Model:          sbGlobal,
		ModelPredicate: func(_ *sbdb.SBGlobal) bool { return true },
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
