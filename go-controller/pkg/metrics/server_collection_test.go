// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func newCollectionTestServer(interval time.Duration, appctl func(int, ...string) (string, string, error)) *MetricServer {
	registry := prometheus.NewRegistry()
	registry.MustRegister(metricOVNDBSessions, metricOVNDBMonitor)
	server := NewMetricServer(MetricServerOptions{
		CollectionInterval: interval,
		EnableOVNDBMetrics: true,
		Registerer:         registry,
	})
	server.ovsDbProperties = []*util.OvsDbProperties{{DbName: "collection-test", AppCtl: appctl}}
	return server
}

func startCollectionTestLoop(t *testing.T, server *MetricServer) <-chan struct{} {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.runCollectionLoop(stop)
	}()
	t.Cleanup(func() {
		close(stop)
		require.Eventually(t, func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		}, time.Second, time.Millisecond)
	})
	return done
}

func TestMetricServerCollectionModes(t *testing.T) {
	for _, interval := range []time.Duration{0, time.Hour, -time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			var calls atomic.Int32
			server := newCollectionTestServer(interval, func(_ int, _ ...string) (string, string, error) {
				value := calls.Add(1)
				return fmt.Sprintf("sessions:%d monitors:%d", value, value), "", nil
			})
			done := startCollectionTestLoop(t, server)
			if interval == 0 {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("on-demand mode started a background collection loop")
				}
				require.Zero(t, calls.Load(), "on-demand mode must not collect before a scrape")
			} else {
				require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
			}

			for scrape := int32(1); scrape <= 2; scrape++ {
				response := httptest.NewRecorder()
				server.mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				require.Equal(t, http.StatusOK, response.Code)
				if interval == 0 {
					require.Equal(t, scrape, calls.Load())
					require.Contains(t, response.Body.String(), fmt.Sprintf("ovn_db_jsonrpc_server_sessions{db_name=\"collection-test\"} %d\n", scrape))
					require.Contains(t, response.Body.String(), fmt.Sprintf("ovn_db_ovsdb_monitors{db_name=\"collection-test\"} %d\n", scrape))
				} else {
					require.EqualValues(t, 1, calls.Load(), "periodic scrapes must not trigger extraction")
				}
			}

			response := httptest.NewRecorder()
			before := calls.Load()
			server.mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/missing", nil))
			require.Equal(t, http.StatusNotFound, response.Code)
			require.Equal(t, before, calls.Load(), "non-metrics requests must not trigger extraction")
		})
	}
}

func TestMetricServerPeriodicCollection(t *testing.T) {
	var calls atomic.Int32
	server := newCollectionTestServer(10*time.Millisecond, func(_ int, _ ...string) (string, string, error) {
		calls.Add(1)
		return "", "", nil
	})
	startCollectionTestLoop(t, server)
	require.Eventually(t, func() bool { return calls.Load() >= 2 }, time.Second, time.Millisecond,
		"periodic mode must keep collecting without scrapes")
}

func TestMetricServerSerializesOnDemandCollection(t *testing.T) {
	var calls atomic.Int32
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()

	server := newCollectionTestServer(0, func(_ int, _ ...string) (string, string, error) {
		call := calls.Add(1)
		if call == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		return fmt.Sprintf("sessions:%d monitors:%d", call, call), "", nil
	})

	request := func(started chan<- struct{}, done chan<- struct{}, response *httptest.ResponseRecorder) {
		defer close(done)
		close(started)
		server.mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	}

	firstRequestStarted := make(chan struct{})
	firstDone := make(chan struct{})
	firstResponse := httptest.NewRecorder()
	go request(firstRequestStarted, firstDone, firstResponse)
	<-firstRequestStarted
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first collection did not start")
	}

	secondRequestStarted := make(chan struct{})
	secondDone := make(chan struct{})
	secondResponse := httptest.NewRecorder()
	go request(secondRequestStarted, secondDone, secondResponse)
	<-secondRequestStarted
	require.Never(t, func() bool { return calls.Load() > 1 }, 100*time.Millisecond, time.Millisecond,
		"on-demand collection calls must not overlap")

	release()
	for _, done := range []<-chan struct{}{firstDone, secondDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("metrics request did not complete")
		}
	}
	require.Equal(t, http.StatusOK, firstResponse.Code)
	require.Equal(t, http.StatusOK, secondResponse.Code)
	require.EqualValues(t, 2, calls.Load())
}

func TestStartMetricsServerOwnsLifecycle(t *testing.T) {
	for _, interval := range []time.Duration{0, time.Hour} {
		t.Run(interval.String(), func(t *testing.T) {
			stop := make(chan struct{})
			_, done := StartMetricsServer(MetricServerOptions{
				BindAddress:        "127.0.0.1:0",
				CollectionInterval: interval,
				Registerer:         prometheus.NewRegistry(),
			}, stop)

			close(stop)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("metrics server did not stop")
			}
		})
	}
}
