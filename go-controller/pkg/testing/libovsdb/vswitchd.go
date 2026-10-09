// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package libovsdb

import (
	"context"
	"time"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// EmulateVSwitchdConfig acknowledges OVS configuration changes by advancing
// cur_cfg to next_cfg. The returned function stops the emulator and waits for
// it to exit; call it before cleaning up the OVS test harness.
func EmulateVSwitchdConfig(ovsClient libovsdbclient.Client) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				var rows []vswitchd.OpenvSwitch
				if err := ovsClient.List(ctx, &rows); err != nil || len(rows) == 0 {
					continue
				}
				ovs := rows[0]
				if ovs.CurCfg >= ovs.NextCfg {
					continue
				}
				updated := &vswitchd.OpenvSwitch{UUID: ovs.UUID, CurCfg: ovs.NextCfg}
				ops, err := ovsClient.Where(&vswitchd.OpenvSwitch{UUID: ovs.UUID}).Update(updated, &updated.CurCfg)
				if err == nil {
					_, _ = ovsClient.Transact(ctx, ops...)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
