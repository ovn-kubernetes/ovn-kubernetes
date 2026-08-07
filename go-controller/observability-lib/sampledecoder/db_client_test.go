// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package sampledecoder

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestGetLocalNBClientUsesUnixEndpointAndMonitorsRequiredTables(t *testing.T) {
	const (
		aclUUID    = "00000000-0000-0000-0000-000000000001"
		sampleUUID = "00000000-0000-0000-0000-000000000002"
		switchUUID = "00000000-0000-0000-0000-000000000003"
	)

	sampleNew := sampleUUID
	serverClient, testContext, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			&nbdb.Sample{
				UUID:     sampleUUID,
				Metadata: 1,
			},
			&nbdb.ACL{
				UUID:      aclUUID,
				Action:    nbdb.ACLActionAllow,
				Direction: nbdb.ACLDirectionFromLport,
				Match:     "1 == 1",
				Priority:  1000,
				SampleNew: &sampleNew,
			},
			&nbdb.LogicalSwitch{
				UUID: switchUUID,
				ACLs: []string{aclUUID},
				Name: "test-switch",
			},
		},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(testContext.Cleanup)

	endpoint := serverClient.CurrentEndpoint()
	socketPath, found := strings.CutPrefix(endpoint, "unix:")
	require.True(t, found, "test server endpoint %q is not a Unix socket", endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), OVSDBTimeout)
	t.Cleanup(cancel)
	nbClient, err := getLocalNBClient(ctx, socketPath)
	require.NoError(t, err)
	t.Cleanup(nbClient.Close)

	assert.True(t, nbClient.Connected())
	assert.Equal(t, endpoint, nbClient.CurrentEndpoint())

	var acls []*nbdb.ACL
	require.NoError(t, nbClient.List(ctx, &acls))
	require.Len(t, acls, 1)
	assert.Equal(t, aclUUID, acls[0].UUID)

	var samples []*nbdb.Sample
	require.NoError(t, nbClient.List(ctx, &samples))
	require.Len(t, samples, 1)
	assert.Equal(t, sampleUUID, samples[0].UUID)
}
