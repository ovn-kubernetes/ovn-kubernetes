// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package libovsdb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type blockedVSwitchdClient struct {
	libovsdbclient.Client
	started  chan struct{}
	finished chan struct{}
}

func (c *blockedVSwitchdClient) Transact(ctx context.Context, _ ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	close(c.started)
	<-ctx.Done()
	close(c.finished)
	return nil, ctx.Err()
}

func TestEmulateVSwitchdConfigShutdown(t *testing.T) {
	ovsClient, cleanup, err := NewOVSTestHarness(TestSetup{
		OVSData: []TestData{&vswitchd.OpenvSwitch{UUID: "root", NextCfg: 1}},
	})
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)
	client := &blockedVSwitchdClient{
		Client:   ovsClient,
		started:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	stop := EmulateVSwitchdConfig(client)
	t.Cleanup(stop)
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("emulator did not start a transaction")
	}

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("emulator did not stop an in-flight transaction")
	}
	select {
	case <-client.finished:
	default:
		t.Fatal("stop returned before the transaction finished")
	}
}
