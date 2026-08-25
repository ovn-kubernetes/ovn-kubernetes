// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package libovsdb_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/observability-lib/sampledecoder"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/observability"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestSampleDecoderUsesUnixEndpointAndMonitorsRequiredTables(t *testing.T) {
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
				ExternalIDs: map[string]string{
					libovsdbops.OwnerTypeKey.String():       libovsdbops.AdminNetworkPolicyOwnerType,
					libovsdbops.ObjectNameKey.String():      "test-policy",
					libovsdbops.PolicyDirectionKey.String(): "Ingress",
				},
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

	ctx, cancel := context.WithTimeout(context.Background(), sampledecoder.OVSDBTimeout)
	t.Cleanup(cancel)
	decoder, err := sampledecoder.NewSampleDecoder(ctx, socketPath)
	require.NoError(t, err)
	t.Cleanup(decoder.Shutdown)

	event, err := decoder.DecodeCookieIDs(uint32(observability.ACLNewTrafficSamplingID)<<24, 1)
	require.NoError(t, err)
	require.Equal(t, "Allowed by admin network policy test-policy, direction Ingress", event.String())
}
