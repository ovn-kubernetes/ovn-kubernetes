// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0
package ovn

import (
	"context"
	"sync"
	"sync/atomic"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

// snatReplayGateClient pauses a gateway NAT lookup in the pod setup client.
// The EgressIP controller keeps its original client, allowing the test to
// verify that assignment waits on the pod lock rather than this client gate.
// The gate is installed before watchers start and stays disarmed until Arm
// so the initial pod reconciliation is not blocked.
type snatReplayGateClient struct {
	libovsdbclient.Client
	routerName   string
	armed        atomic.Bool
	disconnected atomic.Bool
	reached      chan struct{}
	resume       chan struct{}
	once         sync.Once
}

// Arm allows the next matching gateway-router Where() call to pause.
func (c *snatReplayGateClient) Arm() {
	c.armed.Store(true)
}

// Where pauses once on the armed gateway-router lookup, then forwards to the real client.
func (c *snatReplayGateClient) Where(models ...model.Model) libovsdbclient.ConditionalAPI {
	if c.armed.Load() {
		for _, m := range models {
			if router, ok := m.(*nbdb.LogicalRouter); ok && router.Name == c.routerName {
				c.once.Do(func() {
					close(c.reached)
					<-c.resume
				})
			}
		}
	}
	return c.Client.Where(models...)
}

// Transact simulates a disconnected pod client without interrupting EgressIP's client.
func (c *snatReplayGateClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	if c.disconnected.Load() {
		return nil, libovsdbclient.ErrNotConnected
	}
	return c.Client.Transact(ctx, ops...)
}
