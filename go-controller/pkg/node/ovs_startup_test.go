// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// Observe the first completed lookup, so the test can publish daemon state
// only after startup has encountered its absence.
type observedStartupClient struct {
	client.Client
	lookedUp  chan struct{}
	once      sync.Once
	lookupErr error
}

func (c *observedStartupClient) Where(models ...model.Model) client.ConditionalAPI {
	return &observedStartupLookup{ConditionalAPI: c.Client.Where(models...), owner: c}
}

type observedStartupLookup struct {
	client.ConditionalAPI
	owner *observedStartupClient
}

func (l *observedStartupLookup) List(ctx context.Context, result interface{}) error {
	err := l.owner.lookupErr
	if err == nil {
		err = l.ConditionalAPI.List(ctx, result)
	}
	l.owner.once.Do(func() { close(l.owner.lookedUp) })
	return err
}

func TestFlowCollectorsWaitForIntegrationBridge(t *testing.T) {
	require.NoError(t, config.PrepareTestConfig())
	t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
	c, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "root"},
	}})
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	observed := &observedStartupClient{Client: c, lookedUp: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- setOVSFlowTargets(observed, &corev1.Node{}) }()
	select {
	case <-observed.lookedUp:
	case <-time.After(time.Second):
		t.Fatal("bridge was not queried")
	}
	select {
	case err := <-done:
		t.Fatalf("collector setup returned before br-int existed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, ovsops.CreateOrUpdateBridge(c, "br-int", vswitchd.BridgeFailModeSecure, 1400))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("collector setup did not observe br-int")
	}
	lookupErr := errors.New("bridge lookup failed")
	observed = &observedStartupClient{Client: c, lookedUp: make(chan struct{}), lookupErr: lookupErr}
	require.ErrorIs(t, setOVSFlowTargets(observed, &corev1.Node{}), lookupErr)
}

func TestWaitForBridgeMAC(t *testing.T) {
	for _, initial := range []*string{nil, ptr.To("")} {
		t.Run("unpublished", func(t *testing.T) {
			c, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root", Bridges: []string{"bridge"}},
				&vswitchd.Bridge{UUID: "bridge", Name: "br-ex", Ports: []string{"port"}},
				&vswitchd.Port{UUID: "port", Name: "br-ex", Interfaces: []string{"iface"}},
				&vswitchd.Interface{UUID: "iface", Name: "br-ex", Type: "internal", MACInUse: initial},
			}})
			require.NoError(t, err)
			t.Cleanup(cleanup.Cleanup)
			observed := &observedStartupClient{Client: c, lookedUp: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				mac, err := waitForBridgeMAC(ctx, observed, "br-ex")
				if err == nil && mac.String() != "00:11:22:33:44:55" {
					err = errors.New("unexpected bridge MAC")
				}
				done <- err
			}()
			select {
			case <-observed.lookedUp:
			case <-time.After(time.Second):
				t.Fatal("MAC was not queried")
			}
			iface, err := ovsops.GetOVSInterface(c, "br-ex")
			require.NoError(t, err)
			iface.MACInUse = ptr.To("00:11:22:33:44:55")
			operations, err := c.Where(iface).Update(iface, &iface.MACInUse)
			require.NoError(t, err)
			_, err = ovsops.TransactAndCheck(c, operations)
			require.NoError(t, err)
			require.NoError(t, <-done)
			cancel()
			_, err = waitForBridgeMAC(ctx, c, "br-ex")
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}
