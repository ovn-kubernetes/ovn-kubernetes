// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package libovsdb

import (
	"context"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// RecordingClient wraps a libovsdb client and records the operations of every
// transaction it forwards, so tests can assert on what was written to the DB.
type RecordingClient struct {
	libovsdbclient.Client
	// Transactions holds a copy of the ops of each Transact call, in order.
	Transactions [][]ovsdb.Operation
}

// NewRecordingClient returns a RecordingClient wrapping the given client.
func NewRecordingClient(client libovsdbclient.Client) *RecordingClient {
	return &RecordingClient{Client: client}
}

// Transact records the operations and forwards them to the wrapped client.
func (c *RecordingClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	c.Transactions = append(c.Transactions, append([]ovsdb.Operation(nil), ops...))
	return c.Client.Transact(ctx, ops...)
}
