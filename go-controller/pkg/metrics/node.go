package metrics

import (
	"runtime"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// RESEARCH: registerPodToPodMetricsIfAvailable registers pod-to-pod metrics if available
// This function checks at runtime if the Linux-specific functions are available
func registerPodToPodMetricsIfAvailable() {
	// Check if we're on Linux where OVS is available
	if runtime.GOOS != "linux" {
		return
	}
	// The functions will be available via build tags on Linux
	// This is a compile-time check, but we add runtime check for safety
	RegisterPodToPodMetrics()
	EnablePodToPodMetrics(true)
}

// MetricCNIRequestDuration is a prometheus metric that tracks the duration
// of CNI requests
var MetricCNIRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "cni_request_duration_seconds",
	Help:      "The duration of CNI server requests.",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
	//labels
	[]string{"command", "err"},
)

var MetricNodeReadyDuration = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "ready_duration_seconds",
	Help:      "The duration for the node to get to ready state.",
})

var metricOvnNodePortEnabled = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "nodeport_enabled",
	Help:      "Specifies if the node port is enabled on this node(1) or not(0).",
})

// metric to get the size of ovnkube.log file
var metricOvnKubeNodeLogFileSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemNode,
	Name:      "logfile_size_bytes",
	Help:      "The size of ovnkube logfile on the node."},
	[]string{
		"logfile_name",
	},
)

var registerNodeMetricsOnce sync.Once

func RegisterNodeMetrics(stopChan <-chan struct{}) {
	registerNodeMetricsOnce.Do(func() {
		// ovnkube-node metrics
		prometheus.MustRegister(MetricCNIRequestDuration)
		prometheus.MustRegister(MetricNodeReadyDuration)
		prometheus.MustRegister(metricOvnNodePortEnabled)
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: types.MetricOvnkubeNamespace,
				Subsystem: types.MetricOvnkubeSubsystemNode,
				Name:      "build_info",
				Help: "A metric with a constant '1' value labeled by version, revision, branch, " +
					"and go version from which ovnkube was built and when and who built it.",
				ConstLabels: prometheus.Labels{
					"version":    "0.0",
					"revision":   config.Commit,
					"branch":     config.Branch,
					"build_user": config.BuildUser,
					"build_date": config.BuildDate,
					"goversion":  runtime.Version(),
				},
			},
			func() float64 { return 1 },
		))
		registerWorkqueueMetrics(types.MetricOvnkubeNamespace, types.MetricOvnkubeSubsystemNode)
		if err := prometheus.Register(MetricResourceRetryFailuresCount); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				panic(err)
			}
		}
		prometheus.MustRegister(metricOvnKubeNodeLogFileSize)
		go ovnKubeLogFileSizeMetricsUpdater(metricOvnKubeNodeLogFileSize, stopChan)

		// RESEARCH: Register pod-to-pod traffic metrics if enabled
		// Note: These functions are only available on Linux (see pod_to_pod.go)
		if config.Metrics.EnablePodToPodMetrics {
			registerPodToPodMetricsIfAvailable()
		}
	})
}
