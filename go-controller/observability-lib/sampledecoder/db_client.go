// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package sampledecoder

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v4"

	"k8s.io/klog/v2/textlogger"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/observability-lib/ovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

const OVSDBTimeout = 10 * time.Second

func newNBClient(ctx context.Context, endpoint string) (client.Client, error) {
	dbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}

	// define client indexes for ACLs to quickly find them by sample_new or sample_est column.
	dbModel.SetIndexes(map[string][]model.ClientIndex{
		nbdb.ACLTable: {
			{Columns: []model.ColumnKey{{Column: "sample_new"}}},
			{Columns: []model.ColumnKey{{Column: "sample_est"}}},
		},
	})

	c, err := newClient(endpoint, dbModel)
	if err != nil {
		return nil, err
	}

	_, err = c.Monitor(ctx,
		c.NewMonitor(
			client.WithTable(&nbdb.ACL{}),
			client.WithTable(&nbdb.Sample{}),
		),
	)

	if err != nil {
		c.Close()
		return nil, err
	}

	return c, nil
}

func newOVSDBClient(ctx context.Context, endpoint string) (client.Client, error) {
	dbModel, err := ovsdb.ObservDatabaseModel()
	if err != nil {
		return nil, err
	}

	c, err := newClient(endpoint, dbModel)
	if err != nil {
		return nil, err
	}

	_, err = c.Monitor(ctx,
		c.NewMonitor(
			client.WithTable(&ovsdb.FlowSampleCollectorSet{}),
			client.WithTable(&ovsdb.Bridge{}),
			client.WithTable(&ovsdb.Interface{}),
		),
	)
	if err != nil {
		c.Close()
		return nil, err
	}

	return c, nil
}

// newClient creates a client for a local OVSDB endpoint.
func newClient(endpoint string, dbModel model.ClientDBModel) (client.Client, error) {
	const connectTimeout = OVSDBTimeout * 2
	const inactivityTimeout = OVSDBTimeout * 18
	// Don't log anything from the libovsdb client by default
	config := textlogger.NewConfig(textlogger.Verbosity(0))
	logger := textlogger.NewLogger(config)

	options := []client.Option{
		// Reading and parsing the DB after reconnect at scale can (unsurprisingly)
		// take longer than a normal ovsdb operation. Give it a bit more time, so
		// we don't time out and enter a reconnect loop. In addition, it also enables
		// inactivity check on the ovsdb connection.
		client.WithInactivityCheck(inactivityTimeout, connectTimeout, &backoff.ZeroBackOff{}),
		client.WithLogger(&logger),
		client.WithEndpoint(endpoint),
	}

	client, err := client.NewOVSDBClient(dbModel, options...)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	err = client.Connect(ctx)
	if err != nil {
		return nil, err
	}

	return client, nil
}
