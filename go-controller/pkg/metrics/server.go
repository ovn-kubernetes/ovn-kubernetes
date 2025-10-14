package metrics

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// MetricServerOptions defines the configuration options for the new MetricServer
type MetricServerOptions struct {
	// Server configuration
	BindAddress string

	// TLS configuration
	CertFile string
	KeyFile  string

	// Feature flags
	EnableOVSMetrics           bool
	EnableOVNDBMetrics         bool
	EnableOVNControllerMetrics bool
	EnableOVNNorthdMetrics     bool

	// Kubernetes integration
	K8sClient   kubernetes.Interface
	K8sNodeName string
	OVSDBClient libovsdbclient.Client

	dbIsClustered  bool
	dbFoundViaPath bool
}

// MetricServer represents the new unified metrics server
type MetricServer struct {
	// Configuration
	opts MetricServerOptions

	ovsDBClient libovsdbclient.Client
	kubeClient  kubernetes.Interface

	ovsDbProperties []*util.OvsDbProperties

	// HTTP server
	server *http.Server
	mux    *http.ServeMux

	// Prometheus registries
	prometheusRegistry prometheus.Gatherer
	ovnRegistry        *prometheus.Registry
}

// NewMetricServer creates a new MetricServer instance
func NewMetricServer(opts MetricServerOptions, ovsDBClient libovsdbclient.Client, kubeClient kubernetes.Interface) (*MetricServer, error) {
	// Create server instance
	server := &MetricServer{
		opts:               opts,
		prometheusRegistry: prometheus.DefaultGatherer,
		ovnRegistry:        prometheus.NewRegistry(),
		ovsDBClient:        ovsDBClient,
		kubeClient:         kubeClient,
	}

	server.mux = http.NewServeMux()
	if err := server.setupRoutes(); err != nil {
		return nil, fmt.Errorf("failed to setup routes: %w", err)
	}

	return server, nil
}

// setupRoutes configures all HTTP routes
func (s *MetricServer) setupRoutes() error {
	// Metrics endpoints
	s.mux.HandleFunc("/metrics", s.handleMetrics)

	return nil
}

const FmtText = `text/plain; version=` + expfmt.TextVersion + `; charset=utf-8`

// registerMetrics registers the metrics to the OVN registry
func (s *MetricServer) registerMetrics() {
	if s.opts.EnableOVSMetrics {
		klog.Infof("Metric Server registers OVS metrics")
		registerOvsMetrics(s.ovsDBClient, s.ovnRegistry)
	}
	if s.opts.EnableOVNDBMetrics {
		klog.Infof("MetricServer registers OVN DB metrics")
		s.ovsDbProperties, s.opts.dbIsClustered, s.opts.dbFoundViaPath = RegisterOvnDBMetrics(s.ovnRegistry)
	}
	if s.opts.EnableOVNControllerMetrics {
		klog.Infof("MetricServer registers OVN Controller metrics")
		RegisterOvnControllerMetrics(s.ovsDBClient, s.ovnRegistry)
	}
	if s.opts.EnableOVNNorthdMetrics {
		klog.Infof("MetricServer registers OVN Northd metrics")
		RegisterOvnNorthdMetrics(s.ovnRegistry)
	}
}

func (s *MetricServer) EnableOVNNorthdMetrics() {
	s.opts.EnableOVNNorthdMetrics = true
	klog.Infof("MetricServer registers OVN Northd metrics")
	RegisterOvnNorthdMetrics(s.ovnRegistry)
}

func (s *MetricServer) EnableOVNDBMetrics() {
	s.opts.EnableOVNDBMetrics = true
	klog.Infof("MetricServer registers OVN DB metrics")
	s.ovsDbProperties, s.opts.dbIsClustered, s.opts.dbFoundViaPath = RegisterOvnDBMetrics(s.ovnRegistry)
}

// writeRegisteredMetrics writes the registered metrics to the /metrics response.
func (s *MetricServer) writeRegisteredMetrics(w io.Writer) error {
	mfs, err := s.ovnRegistry.Gather()
	if err != nil {
		return err
	}
	enc := expfmt.NewEncoder(w, FmtText)
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return err
		}
	}
	return nil
}

// updateOvsMetrics updates the OVS metrics
func (s *MetricServer) updateOvsMetrics() {
	ovsDatapathMetricsUpdate()
	if err := updateOvsBridgeMetrics(s.ovsDBClient, util.RunOVSOfctl); err != nil {
		klog.Errorf("Updating ovs bridge metrics failed: %s", err.Error())
	}
	if err := updateOvsInterfaceMetrics(s.ovsDBClient); err != nil {
		klog.Errorf("Updating ovs interface metrics failed: %s", err.Error())
	}
	if err := setOvsMemoryMetrics(util.RunOvsVswitchdAppCtl); err != nil {
		klog.Errorf("Updating ovs memory metrics failed: %s", err.Error())
	}
	if err := setOvsHwOffloadMetrics(s.ovsDBClient); err != nil {
		klog.Errorf("Updating ovs hardware offload metrics failed: %s", err.Error())
	}
	coverageShowMetricsUpdate(ovsVswitchd)
}

// updateOvnControllerMetrics updates the OVN Controller metrics
func (s *MetricServer) updateOvnControllerMetrics() {
	if err := setOvnControllerConfigurationMetrics(s.ovsDBClient); err != nil {
		klog.Errorf("Setting ovn controller config metrics failed: %s", err.Error())
	}

	coverageShowMetricsUpdate(ovnController)
	stopwatchShowMetricsUpdate(ovnController)
	updateSBDBConnectionMetric(util.RunOVNControllerAppCtl)

}

// updateOvnNorthdMetrics updates the OVN Northd metrics
func (s *MetricServer) updateOvnNorthdMetrics() {
	coverageShowMetricsUpdate(ovnNorthd)
	stopwatchShowMetricsUpdate(ovnNorthd)
}

// updateOvnDBMetrics updates the OVN DB metrics
func (s *MetricServer) updateOvnDBMetrics() {
	if s.opts.dbIsClustered {
		resetOvnDbClusterMetrics()
	}
	if s.opts.dbFoundViaPath {
		resetOvnDbSizeMetric()
	}
	resetOvnDbMemoryMetrics()

	for _, dbProperty := range s.ovsDbProperties {
		if s.opts.dbIsClustered {
			ovnDBClusterStatusMetricsUpdater(dbProperty)
		}
		if s.opts.dbFoundViaPath {
			updateOvnDBSizeMetrics(dbProperty)
		}
		updateOvnDBMemoryMetrics(dbProperty)
	}
}

// handleMetrics handles the /metrics request
func (s *MetricServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	klog.V(5).Infof("MetricServer starts to handle metrics request from %s", r.RemoteAddr)

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

	// write out the registered metrics
	w.Header().Set("Content-Type", FmtText)
	if err := s.writeRegisteredMetrics(io.Writer(w)); err != nil {
		klog.Errorf("Failed to write registered metrics: %v", err)
		return
	}
}

// Run runs the metrics server and blocks until graceful shutdown
func (s *MetricServer) Run(stopChan <-chan struct{}) error {
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
			listenAndServe = func() error { return s.server.ListenAndServeTLS("", "") }
		}

		errCh := make(chan error)
		go func() {
			errCh <- listenAndServe()
		}()

		var err error
		select {
		case err = <-errCh:
			err = fmt.Errorf("failed while running metrics server at address %q: %w", s.opts.BindAddress, err)
			utilruntime.HandleError(err)
		case <-stopChan:
			klog.Infof("Stopping metrics server at address %q", s.opts.BindAddress)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.server.Shutdown(shutdownCtx); err != nil {
				klog.Errorf("Error stopping metrics server at address %q: %v", s.opts.BindAddress, err)
			}
		}
	}, 5*time.Second, stopChan)

	return nil
}

// GetOVNRegistry returns the OVN registry for external use
func (s *MetricServer) GetOVNRegistry() *prometheus.Registry {
	return s.ovnRegistry
}

// SetOVNRegistry sets the OVN registry (for integration with existing code)
func (s *MetricServer) SetOVNRegistry(registry *prometheus.Registry) {
	s.ovnRegistry = registry
}

// GetPrometheusRegistry returns the Prometheus registry for external use
func (s *MetricServer) GetPrometheusRegistry() prometheus.Gatherer {
	return s.prometheusRegistry
}
