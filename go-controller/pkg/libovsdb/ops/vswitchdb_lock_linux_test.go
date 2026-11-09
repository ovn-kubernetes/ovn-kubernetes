// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
)

type lockEndpointClient struct {
	libovsdbclient.Client
	endpoint string
}

func (c lockEndpointClient) CurrentEndpoint() string { return c.endpoint }

func TestOVSInterfaceLockProcess(t *testing.T) {
	endpoint := os.Getenv("OVN_LOCK_TEST_ENDPOINT")
	if endpoint == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := LockOVSInterfaces(ctx, lockEndpointClient{endpoint: endpoint}, "rep0")
	if os.Getenv("OVN_LOCK_TEST_BLOCKED") == "true" {
		require.ErrorIs(t, err, context.DeadlineExceeded)
	} else {
		require.NoError(t, err)
		// Deliberately leave the descriptor open: exiting must release it.
	}
}

func TestLockOVSInterfacesAcrossProcesses(t *testing.T) {
	c := lockEndpointClient{endpoint: "unix:" + filepath.Join(t.TempDir(), "db.sock")}
	release, err := LockOVSInterfaces(context.Background(), c, "rep0")
	require.NoError(t, err)
	t.Cleanup(release)
	executable, err := os.Executable()
	require.NoError(t, err)
	runChild := func(blocked string) {
		t.Helper()
		cmd := exec.Command(executable, "-test.run=^TestOVSInterfaceLockProcess$")
		cmd.Env = append(os.Environ(), "OVN_LOCK_TEST_ENDPOINT="+c.endpoint, "OVN_LOCK_TEST_BLOCKED="+blocked)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	runChild("true")
	other, err := LockOVSInterfaces(context.Background(), c, "rep1")
	require.NoError(t, err, "different representors must not block each other")
	other()
	release()
	runChild("false")
	release, err = LockOVSInterfaces(context.Background(), c, "rep0")
	require.NoError(t, err, "process exit must release the lock")
	release()
}

func TestLockOVSInterfacesCancellation(t *testing.T) {
	c := lockEndpointClient{endpoint: "unix:" + filepath.Join(t.TempDir(), "db.sock")}
	releaseB, err := LockOVSInterfaces(context.Background(), c, "b")
	require.NoError(t, err)
	defer releaseB()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = LockOVSInterfaces(ctx, c, "b", "a", "a")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	// A partially acquired set must be released on cancellation.
	releaseA, err := LockOVSInterfaces(context.Background(), c, "a", "a")
	require.NoError(t, err)
	releaseA()
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	_, err = LockOVSInterfaces(ctx, c, "free")
	require.ErrorIs(t, err, context.Canceled)
}
