// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestLogicalRouterPolicyIndexedLookup(t *testing.T) {
	policies := []*nbdb.LogicalRouterPolicy{
		{UUID: uuid.NewString(), Priority: 102, Match: "ip4.src == 10.0.0.0/24", Action: "allow", ExternalIDs: map[string]string{types.PrimaryIDKey: "shared"}},
		{UUID: uuid.NewString(), Priority: 102, Match: "ip4.src == 10.0.1.0/24", Action: "allow", ExternalIDs: map[string]string{types.PrimaryIDKey: "shared"}},
		{UUID: uuid.NewString(), Priority: 102, Match: "ip6", Action: "allow", ExternalIDs: map[string]string{types.PrimaryIDKey: "unique"}},
		{UUID: uuid.NewString(), Priority: 90, Match: "ip", Action: "allow"},
	}
	router := &nbdb.LogicalRouter{UUID: uuid.NewString(), Name: "router"}
	data := []libovsdbtest.TestData{router}
	for _, policy := range policies {
		router.Policies = append(router.Policies, policy.UUID)
		data = append(data, policy)
	}
	client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	m := newModelClient(client)

	for _, tt := range []struct {
		name      string
		model     *nbdb.LogicalRouterPolicy
		predicate bool
		match     string
		want      []*nbdb.LogicalRouterPolicy
		visited   int
		wantErr   error
	}{
		{name: "exact ID without predicate", model: &nbdb.LogicalRouterPolicy{ExternalIDs: map[string]string{types.PrimaryIDKey: "unique"}}, want: policies[2:3]},
		{name: "shared ID retains match filter", model: &nbdb.LogicalRouterPolicy{ExternalIDs: map[string]string{types.PrimaryIDKey: "shared"}}, predicate: true, match: policies[1].Match, want: policies[1:2], visited: 2},
		{name: "shared ID without disambiguation fails", model: &nbdb.LogicalRouterPolicy{ExternalIDs: map[string]string{types.PrimaryIDKey: "shared"}}, wantErr: errMultipleResults},
		{name: "missing ID never scans unrelated policies", model: &nbdb.LogicalRouterPolicy{ExternalIDs: map[string]string{types.PrimaryIDKey: "missing"}}, predicate: true, match: policies[3].Match, wantErr: libovsdbclient.ErrNotFound},
		{name: "index hit rejected by predicate", model: &nbdb.LogicalRouterPolicy{ExternalIDs: map[string]string{types.PrimaryIDKey: "shared"}}, predicate: true, match: policies[3].Match, visited: 2, wantErr: libovsdbclient.ErrNotFound},
		{name: "legacy policy uses predicate", model: &nbdb.LogicalRouterPolicy{}, predicate: true, match: policies[3].Match, want: policies[3:], visited: 4},
		{name: "empty ID uses predicate", model: &nbdb.LogicalRouterPolicy{ExternalIDs: map[string]string{types.PrimaryIDKey: ""}}, predicate: true, match: policies[3].Match, want: policies[3:], visited: 4},
		{name: "UUID takes precedence", model: &nbdb.LogicalRouterPolicy{UUID: policies[0].UUID, ExternalIDs: map[string]string{types.PrimaryIDKey: "unique"}}, predicate: true, match: policies[2].Match, want: policies[:1]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var found []*nbdb.LogicalRouterPolicy
			visited := 0
			op := operationModel{Model: tt.model, ExistingResult: &found, ErrNotFound: true, FilterIndexedResults: true}
			if tt.predicate {
				op.ModelPredicate = func(policy *nbdb.LogicalRouterPolicy) bool {
					visited++
					return policy.Match == tt.match
				}
			}
			err := m.Lookup(op)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.want, found)
				require.Equal(t, tt.want[0].UUID, tt.model.UUID)
			}
			require.Equal(t, tt.visited, visited)
		})
	}

	// Reconcile one subnet-pair policy and then add a new pair sharing its ID.
	// Existing pairs must retain their identities and actions.
	for _, match := range []string{policies[1].Match, "ip4.src == 10.0.2.0/24"} {
		policy := &nbdb.LogicalRouterPolicy{Priority: 102, Match: match, Action: "drop",
			ExternalIDs: map[string]string{types.PrimaryIDKey: "shared"}}
		err := CreateOrUpdateLogicalRouterPolicyWithPredicate(client, router.Name, policy,
			func(item *nbdb.LogicalRouterPolicy) bool {
				return item.ExternalIDs[types.PrimaryIDKey] == "shared" && item.Match == match
			})
		require.NoError(t, err)
	}
	var found []nbdb.LogicalRouterPolicy
	require.NoError(t, client.List(context.Background(), &found))
	require.Len(t, found, 5)
	for _, original := range policies {
		want := *original
		if original.UUID == policies[1].UUID {
			want.Action = "drop"
		}
		require.Contains(t, found, want)
	}
}
