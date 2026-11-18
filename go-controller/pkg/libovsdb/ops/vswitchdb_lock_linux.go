// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const ovsInterfaceLockPollInterval = 10 * time.Millisecond

// LockOVSInterfaces serializes representor ownership and Linux link changes
// across CNI processes and the node controller. The local OVS Unix socket's
// directory must be shared with those processes, as in the node deployment.
// Lock files are retained: unlinking one could let another process lock a
// different inode for the same interface. Only use this for reusable devices,
// not per-pod veths. Callers must re-read ownership from OVS after acquiring it;
// another client's monitor can still contain the previous owner's data.
// The returned release function is safe to call more than once.
func LockOVSInterfaces(ctx context.Context, ovsClient libovsdbclient.Client, names ...string) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, types.OVSDBTimeout)
	defer cancel()
	endpoint := ovsClient.CurrentEndpoint()
	socket, local := strings.CutPrefix(endpoint, "unix:")
	if !local || !filepath.IsAbs(socket) {
		return nil, fmt.Errorf("interface locking requires a local OVS Unix socket, got %q", endpoint)
	}
	dir := filepath.Join(filepath.Dir(socket), "."+filepath.Base(socket)+".ovn-kubernetes-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating OVS interface lock directory: %w", err)
	}
	var files []*os.File
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, file := range files {
				// Closing the descriptor releases the flock, including when
				// the process exits unexpectedly.
				if err := file.Close(); err != nil {
					klog.Errorf("Closing OVS interface lock %s: %v", file.Name(), err)
				}
			}
		})
	}
	// Acquire multiple devices in a consistent order to avoid deadlocks.
	names = slices.Clone(names)
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		file, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("%x", sha256.Sum256([]byte(name)))), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			release()
			return nil, fmt.Errorf("opening OVS interface lock for %s: %w", name, err)
		}
		files = append(files, file)
		err = wait.PollUntilContextCancel(ctx, ovsInterfaceLockPollInterval, true, func(ctx context.Context) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			if errors.Is(err, unix.EWOULDBLOCK) {
				return false, nil
			}
			return err == nil, err
		})
		if err != nil {
			release()
			return nil, fmt.Errorf("locking OVS interface %s: %w", name, err)
		}
	}
	return release, nil
}
