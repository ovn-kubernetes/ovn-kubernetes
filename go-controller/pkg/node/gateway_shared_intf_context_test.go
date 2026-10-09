// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package node

import (
	"context"
	"errors"
	"fmt"
	"testing"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	libovsdbmodel "github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type gatewayCleanupWaitClient struct {
	libovsdbclient.Client
	cancel  context.CancelFunc
	applied bool
}

func (c *gatewayCleanupWaitClient) Get(ctx context.Context, model libovsdbmodel.Model) error {
	if c.cancel != nil {
		// The first acknowledgement read happens after the deletion committed.
		c.cancel()
		return ctx.Err()
	}
	if err := c.Client.Get(ctx, model); err != nil {
		return err
	}
	ovs := model.(*vswitchd.OpenvSwitch)
	ovs.CurCfg = ovs.NextCfg
	c.applied = true
	return nil
}

func TestCleanupSharedGatewayWait(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelWait), func(t *testing.T) {
			if err := config.PrepareTestConfig(); err != nil {
				t.Fatalf("prepare config: %v", err)
			}
			t.Cleanup(func() { _ = config.PrepareTestConfig() })
			config.OvnKubeNode.Mode = types.NodeModeFull
			ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"bridge"},
					ExternalIDs: map[string]string{"ovn-bridge-mappings": types.PhysicalNetworkName + ":br-ex"}},
				&vswitchd.Bridge{UUID: "bridge", Name: "br-ex", Ports: []string{"patch-port"}},
				&vswitchd.Port{UUID: "patch-port", Name: "patch-ovn", Interfaces: []string{"patch-iface"},
					ExternalIDs: map[string]string{"ovn-localnet-port": "localnet"}},
				&vswitchd.Interface{UUID: "patch-iface", Name: "patch-ovn", Type: "patch"},
			}})
			if err != nil {
				t.Fatalf("harness setup: %v", err)
			}
			t.Cleanup(cleanup.Cleanup)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waitClient := &gatewayCleanupWaitClient{Client: ovsClient}
			if cancelWait {
				waitClient.cancel = cancel
			}

			fexec := ovntest.NewFakeExec()
			if !cancelWait {
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl -O OpenFlow13 replace-flows br-ex -",
					Action: func() error {
						if !waitClient.applied {
							return errors.New("restored NORMAL flows before ovs-vswitchd acknowledged removal")
						}
						return nil
					},
				})
			}
			if err := util.SetExec(fexec); err != nil {
				t.Fatalf("set fake exec: %v", err)
			}
			t.Cleanup(util.ResetRunner)

			err = cleanupSharedGateway(ctx, waitClient)
			if cancelWait {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cleanup returned %v, want context.Canceled", err)
				}
			} else if err != nil {
				t.Fatalf("cleanup failed: %v", err)
			}
			if !fexec.CalledMatchesExpected() {
				t.Fatalf("unexpected flow restoration: %s", fexec.ErrorDesc())
			}
			if _, err := ovsops.GetOVSPort(ovsClient, "patch-ovn"); !errors.Is(err, libovsdbclient.ErrNotFound) {
				t.Fatalf("committed patch-port deletion was not retained: %v", err)
			}
		})
	}
}
