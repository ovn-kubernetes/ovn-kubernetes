// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package sampledecoder

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/observability-lib/ovsdb"
)

// TestValidateCollectorReuse covers the collector registration collision semantics:
// re-registration by the same owner with the same group is idempotent (so a consumer
// can recover its own collector after a restart), while a different owner or a
// different group for the same collector ID is rejected as a conflict.
func TestValidateCollectorReuse(t *testing.T) {
	group10 := 10
	existing := func(owner string, group *int) *ovsdb.FlowSampleCollectorSet {
		return &ovsdb.FlowSampleCollectorSet{
			ID:           1,
			LocalGroupID: group,
			ExternalIDs:  map[string]string{"owner": owner},
		}
	}

	tests := []struct {
		name       string
		existing   *ovsdb.FlowSampleCollectorSet
		groupID    int
		ownerName  string
		wantReuse  bool
		wantErrSub string
	}{
		{
			name:      "same owner and group is reused (restart case)",
			existing:  existing("netobserv", &group10),
			groupID:   10,
			ownerName: "netobserv",
			wantReuse: true,
		},
		{
			name:       "different owner is a conflict",
			existing:   existing("netobserv", &group10),
			groupID:    10,
			ownerName:  "other",
			wantErrSub: "already in use",
		},
		{
			name:       "same owner different group is a conflict",
			existing:   existing("netobserv", &group10),
			groupID:    20,
			ownerName:  "netobserv",
			wantErrSub: "already in use",
		},
		{
			name:       "existing collector without a group is a conflict",
			existing:   existing("netobserv", nil),
			groupID:    10,
			ownerName:  "netobserv",
			wantErrSub: "already in use",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCollectorReuse(tt.existing, tt.groupID, tt.ownerName)
			if tt.wantReuse {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErrSub)
		})
	}
}
