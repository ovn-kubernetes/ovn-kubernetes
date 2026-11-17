// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

// transactMetricsClient answers every transaction with a canned outcome so the
// test controls which path TransactAndCheckWithContext takes.
type transactMetricsClient struct {
	libovsdbclient.Client
	results []ovsdb.OperationResult
	err     error
}

func (c *transactMetricsClient) Transact(context.Context, ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	return c.results, c.err
}

func (c *transactMetricsClient) Schema() ovsdb.DatabaseSchema {
	return nbdb.Schema()
}

// seriesCount returns how many series a collector currently exposes.
func seriesCount(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric, 64)
	c.Collect(ch)
	close(ch)
	n := 0
	for range ch {
		n++
	}
	return n
}

// histogramSamples returns the observation count and sum of one histogram series.
func histogramSamples(t *testing.T, observer prometheus.Observer) (uint64, float64) {
	t.Helper()
	m := &dto.Metric{}
	require.NoError(t, observer.(prometheus.Histogram).Write(m))
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

func TestTransactAndCheckWithContextMetrics(t *testing.T) {
	ops := []ovsdb.Operation{{Op: ovsdb.OperationComment}, {Op: ovsdb.OperationComment}}
	succeeded := make([]ovsdb.OperationResult, len(ops))
	failed := make([]ovsdb.OperationResult, len(ops))
	failed[1].Error = "constraint violation"

	tests := []struct {
		name       string
		client     *transactMetricsClient
		wantErr    bool
		wantResult string
	}{
		{
			name:       "success",
			client:     &transactMetricsClient{results: succeeded},
			wantResult: txnResultSuccess,
		},
		{
			name:       "transact error",
			client:     &transactMetricsClient{err: errors.New("connection reset")},
			wantErr:    true,
			wantResult: txnResultError,
		},
		{
			name:       "operation result error",
			client:     &transactMetricsClient{results: failed},
			wantErr:    true,
			wantResult: txnResultError,
		},
	}

	db := nbdb.Schema().Name
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metricTxnDuration.Reset()
			metricTxnOps.Reset()

			_, err := TransactAndCheckWithContext(context.Background(), tt.client, ops)
			require.Equal(t, tt.wantErr, err != nil, "unexpected error outcome: %v", err)

			// exactly one series per family, so a wrong label would show up as
			// a second series rather than go unnoticed
			require.Equal(t, 1, seriesCount(metricTxnOps), "txn_ops must have exactly one series, labelled by db")
			require.Equal(t, 1, seriesCount(metricTxnDuration), "txn_duration_seconds must have exactly one series, labelled by db and result")

			count, sum := histogramSamples(t, metricTxnOps.WithLabelValues(db))
			require.Equal(t, uint64(1), count, "txn_ops must observe the transaction once")
			require.InDelta(t, float64(len(ops)), sum, 0, "txn_ops must observe the operation count")

			count, _ = histogramSamples(t, metricTxnDuration.WithLabelValues(db, tt.wantResult))
			require.Equal(t, uint64(1), count, "txn_duration_seconds must observe result=%s", tt.wantResult)
		})
	}
}

func TestTransactAndCheckWithContextNoOpsRecordsNothing(t *testing.T) {
	metricTxnDuration.Reset()
	metricTxnOps.Reset()

	_, err := TransactAndCheckWithContext(context.Background(), &transactMetricsClient{}, nil)
	require.NoError(t, err)
	require.Equal(t, 0, seriesCount(metricTxnOps), "an empty transaction is not submitted and must not be counted")
	require.Equal(t, 0, seriesCount(metricTxnDuration), "an empty transaction must not be timed")
}
