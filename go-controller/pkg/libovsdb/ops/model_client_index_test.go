// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestFilterIndexedResults(t *testing.T) {
	first := &nbdb.AddressSet{UUID: uuid.NewString(), Name: "first"}
	second := &nbdb.AddressSet{UUID: uuid.NewString(), Name: "second"}
	client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{first, second},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	m := newModelClient(client)
	for _, tt := range []struct {
		name    string
		model   *nbdb.AddressSet
		filter  bool
		match   string
		visited int
		want    *nbdb.AddressSet
	}{
		{name: "default index precedence", model: &nbdb.AddressSet{Name: first.Name}, match: second.Name, want: first},
		{name: "filter matching candidate", model: &nbdb.AddressSet{Name: first.Name}, filter: true, match: first.Name, visited: 1, want: first},
		{name: "filter rejects indexed candidate", model: &nbdb.AddressSet{Name: first.Name}, filter: true, match: second.Name, visited: 1},
		{name: "empty index result stays empty", model: &nbdb.AddressSet{Name: "missing"}, filter: true, match: second.Name},
		{name: "UUID retains precedence", model: &nbdb.AddressSet{UUID: first.UUID}, filter: true, match: second.Name, want: first},
	} {
		t.Run(tt.name, func(t *testing.T) {
			visited := 0
			var found []*nbdb.AddressSet
			err := m.Lookup(operationModel{
				Model: tt.model, ExistingResult: &found, ErrNotFound: true,
				FilterIndexedResults: tt.filter,
				ModelPredicate: func(item *nbdb.AddressSet) bool {
					visited++
					return item.Name == tt.match
				},
			})
			if tt.want == nil {
				require.ErrorIs(t, err, libovsdbclient.ErrNotFound)
				require.Empty(t, found)
			} else {
				require.NoError(t, err)
				require.Equal(t, []*nbdb.AddressSet{tt.want}, found)
			}
			require.Equal(t, tt.visited, visited)
		})
	}
}
