// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovntls "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/tls"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// metricsScrapeBudgetFallback bounds metric collection when Prometheus does not
	// advertise its scrape timeout. Kept below the typical 10s scrape_timeout so the
	// handler still returns in time to serve the last known values (up stays 1).
	metricsScrapeBudgetFallback = 8 * time.Second

	// metricsScrapeBudgetSlack is subtracted from the advertised scrape timeout to
	// leave promhttp room to serialize and write the response before Prometheus'
	// deadline; without it a cached write can lose the race and flap up to 0.
	metricsScrapeBudgetSlack = 500 * time.Millisecond
)

// scrapeBudget returns how long metric collection may run before the handler gives
// up and serves the last known values. It honors Prometheus'
// X-Prometheus-Scrape-Timeout-Seconds header (minus slack) when present, otherwise
// falls back to metricsScrapeBudgetFallback.
func scrapeBudget(r *http.Request) time.Duration {
	if v := r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			// Honor the advertised deadline minus slack, never the larger fallback.
			// If that leaves no room, use the full advertised value rather than skip
			// collection entirely.
			advertised := time.Duration(secs * float64(time.Second))
			if budget := advertised - metricsScrapeBudgetSlack; budget > 0 {
				return budget
			}
			return advertised
		}
	}
	// No header, or an unparseable/non-positive one: fall back.
	return metricsScrapeBudgetFallback
}

// MetricServerOptions defines the configuration options for the new MetricServer
type MetricServerOptions struct {
	// Server configuration
	BindAddress string

	// TLS configuration
	CertFile        string
	KeyFile         string
	ApplyTLSOptions ovntls.ApplyConfigOptions

	// Feature flags
	EnableOVSMetrics           bool
	EnableOVNDBMetrics         bool
	EnableOVNControllerMetrics bool
	EnableOVNNorthdMetrics     bool
	EnablePprof                bool

	OVSDBClient libovsdbclient.Client

	// OnFatalError is called when an unrecoverable error occurs (e.g., failed to bind to address).
	// If set, it allows the caller to trigger a graceful shutdown.
	OnFatalError func()

	// Prometheus plumbing
	Registerer prometheus.Registerer

	dbFoundViaPath bool
}

// MetricServer represents the new unified metrics server
type MetricServer struct {
	// Configuration
	opts MetricServerOptions

	ovsDbProperties []*util.OvsDbProperties

	// HTTP server
	server *http.Server
	mux    *http.ServeMux

	// Prometheus registry
	registerer prometheus.Registerer
}

// NewMetricServer creates a new MetricServer instance
func NewMetricServer(opts MetricServerOptions) *MetricServer {
	registerer := opts.Registerer
	if registerer == nil {
		registerer = prometheus.NewRegistry()
	}

	server := &MetricServer{
		opts:       opts,
		registerer: registerer,
	}

	server.mux = http.NewServeMux()
	tg := prometheus.ToTransactionalGatherer(server.registerer.(prometheus.Gatherer))
	metricsHandler := promhttp.HandlerForTransactional(tg, promhttp.HandlerOpts{})

	server.mux.Handle("/metrics", promhttp.InstrumentMetricHandler(
		server.registerer,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Update metrics in the registry before emitting them.
			server.handleMetrics(r)
			metricsHandler.ServeHTTP(w, r)
		}),
	))

	if opts.EnablePprof {
		server.mux.HandleFunc("/debug/pprof/", pprof.Index)
		server.mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		server.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		server.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		server.mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

		// Allow changes to log level at runtime
		server.mux.HandleFunc("/debug/flags/v", stringFlagPutHandler(klogSetter))
	}

	return server
}

// registerMetrics registers the metrics to the OVN registry
func (s *MetricServer) registerMetrics() {
	if s.opts.EnableOVSMetrics {
		klog.Infof("MetricServer registers OVS metrics")
		registerOvsMetrics(s.opts.OVSDBClient, s.registerer)
	}
	if s.opts.EnableOVNDBMetrics {
		klog.Infof("MetricServer registers OVN DB metrics")
		s.ovsDbProperties, s.opts.dbFoundViaPath = RegisterOvnDBMetrics(s.registerer)
	}
	if s.opts.EnableOVNControllerMetrics {
		klog.Infof("MetricServer registers OVN Controller metrics")
		RegisterOvnControllerMetrics(s.opts.OVSDBClient, s.registerer)
	}
	if s.opts.EnableOVNNorthdMetrics {
		klog.Infof("MetricServer registers OVN Northd metrics")
		RegisterOvnNorthdMetrics(s.registerer)
	}
}

// updateOvsMetrics updates the OVS metrics
func (s *MetricServer) updateOvsMetrics() {
	ovsDatapathMetricsUpdate()
	if err := updateOvsBridgeMetrics(s.opts.OVSDBClient, util.RunOVSOfctl); err != nil {
		klog.Errorf("Updating ovs bridge metrics failed: %s", err.Error())
	}
	if err := updateOvsInterfaceMetrics(s.opts.OVSDBClient); err != nil {
		klog.Errorf("Updating ovs interface metrics failed: %s", err.Error())
	}
	if err := setOvsMemoryMetrics(func(args ...string) (string, string, error) {
		return util.RunOvsVswitchdAppCtlWithTimeout(metricsAppctlTimeout, args...)
	}); err != nil {
		klog.Errorf("Updating ovs memory metrics failed: %s", err.Error())
	}
	if err := setOvsHwOffloadMetrics(s.opts.OVSDBClient); err != nil {
		klog.Errorf("Updating ovs hardware offload metrics failed: %s", err.Error())
	}
	coverageShowMetricsUpdate(ovsVswitchd)
}

// updateOvnControllerMetrics updates the OVN Controller metrics
func (s *MetricServer) updateOvnControllerMetrics() {
	if err := setOvnControllerConfigurationMetrics(s.opts.OVSDBClient); err != nil {
		klog.Errorf("Setting ovn controller config metrics failed: %s", err.Error())
	}

	coverageShowMetricsUpdate(ovnController)
	stopwatchShowMetricsUpdate(ovnController)
	updateSBDBConnectionMetric(func(args ...string) (string, string, error) {
		return util.RunOVNControllerAppCtlWithTimeout(metricsAppctlTimeout, args...)
	})

}

// updateOvnNorthdMetrics updates the OVN Northd metrics
func (s *MetricServer) updateOvnNorthdMetrics() {
	coverageShowMetricsUpdate(ovnNorthd)
	stopwatchShowMetricsUpdate(ovnNorthd)
}

// updateOvnDBMetrics updates the OVN DB metrics
func (s *MetricServer) updateOvnDBMetrics() {
	if s.opts.dbFoundViaPath {
		resetOvnDbSizeMetric()
	}
	resetOvnDbMemoryMetrics()

	for _, dbProperty := range s.ovsDbProperties {
		if s.opts.dbFoundViaPath {
			updateOvnDBSizeMetrics(dbProperty)
		}
		updateOvnDBMemoryMetrics(dbProperty)
	}
}

// handleMetrics refreshes the OVS/OVN metrics before they are emitted. Collection
// execs ovs-appctl/ovn-appctl subprocesses (each bounded by metricsAppctlTimeout)
// in a separate goroutine under a scrape budget: if the daemons are slow and it
// overruns, the handler returns and promhttp serves the last known values, keeping
// the scrape under Prometheus' scrape_timeout (up stays 1). The goroutine keeps
// running to refresh the registry for the next scrape.
func (s *MetricServer) handleMetrics(r *http.Request) {
	klog.V(5).Infof("MetricServer starts to handle metrics request from %s", r.RemoteAddr)

	ctx, cancel := context.WithTimeout(r.Context(), scrapeBudget(r))
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Nothing recovers this detached goroutine, so recover here: a panic in a
		// metric path must only fail this scrape, not crash ovnkube-node.
		defer func() {
			if r := recover(); r != nil {
				klog.Errorf("MetricServer recovered from panic during metric collection: %v\n%s", r, debug.Stack())
			}
		}()

		if s.opts.EnableOVSMetrics {
			s.updateOvsMetrics()
		}
		if s.opts.EnableOVNDBMetrics {
			s.updateOvnDBMetrics()
		}
		if s.opts.EnableOVNControllerMetrics {
			s.updateOvnControllerMetrics()
		}
		if s.opts.EnableOVNNorthdMetrics {
			s.updateOvnNorthdMetrics()
		}
	}()

	select {
	case <-done:
	case <-ctx.Done():
		klog.Errorf("MetricServer metric collection exceeded the scrape budget for request from %s; "+
			"serving last known values (%v)", r.RemoteAddr, ctx.Err())
	}
}

// Run runs the metrics server and blocks until graceful shutdown
func (s *MetricServer) Run(stopChan <-chan struct{}) {
	utilwait.Until(func() {
		s.server = &http.Server{
			Addr:    s.opts.BindAddress,
			Handler: s.mux,
		}
		listenAndServe := func() error { return s.server.ListenAndServe() }
		if s.opts.CertFile != "" && s.opts.KeyFile != "" {
			s.server.TLSConfig = &tls.Config{
				MinVersion: tls.VersionTLS12,
				GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
					cert, err := tls.LoadX509KeyPair(s.opts.CertFile, s.opts.KeyFile)
					if err != nil {
						return nil, fmt.Errorf("error generating x509 certs for metrics TLS endpoint: %v", err)
					}
					return &cert, nil
				},
			}

			if s.opts.ApplyTLSOptions != nil {
				s.opts.ApplyTLSOptions(s.server.TLSConfig)
			}

			listenAndServe = func() error { return s.server.ListenAndServeTLS("", "") }
		}

		errCh := make(chan error)
		go func() {
			klog.Infof("Metric Server starts to listen on %s", s.opts.BindAddress)
			errCh <- listenAndServe()
		}()

		select {
		case err := <-errCh:
			if !errors.Is(err, http.ErrServerClosed) {
				utilruntime.HandleError(fmt.Errorf("failed while running metrics server at address %q: %w", s.opts.BindAddress, err))
				if s.opts.OnFatalError != nil {
					s.opts.OnFatalError()
				}
			}
		case <-stopChan:
			klog.Infof("Stopping metrics server at address %q", s.opts.BindAddress)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.server.Shutdown(shutdownCtx); err != nil {
				klog.Errorf("Error stopping metrics server at address %q: %v", s.opts.BindAddress, err)
			}
		}
	}, 5*time.Second, stopChan)
}
