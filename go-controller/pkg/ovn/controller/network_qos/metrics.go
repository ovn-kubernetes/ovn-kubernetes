package networkqos

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

// Metrics to be exposed
var (
	nqosCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "num_network_qoses",
			Help:      "The total number of network qoses in the cluster",
		},
		[]string{"network"},
	)

	nqosOvnOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "nqos_ovn_operation_duration_ms",
			Help:      "Time spent on reconciling a NetworkQoS event",
			Buckets:   prometheus.ExponentialBuckets(.1, 2, 15),
		},
		[]string{"operation"},
	)

	nqosReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "nqos_creation_duration_ms",
			Help:      "Time spent on reconciling a NetworkQoS event",
			Buckets:   prometheus.ExponentialBuckets(.1, 2, 15),
		},
		[]string{"network"},
	)

	nqosPodReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "nqos_deletion_duration_ms",
			Help:      "Time spent on reconciling a Pod event",
			Buckets:   prometheus.ExponentialBuckets(.1, 2, 15),
		},
		[]string{"network"},
	)

	nqosNamespaceReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "nqos_ns_reconcile_duration_ms",
			Help:      "Time spent on reconciling Namespace change for all Pods related to NetworkQoSes",
			Buckets:   prometheus.ExponentialBuckets(.1, 2, 15),
		},
		[]string{"network"},
	)

	nqosStatusPatchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "nqos_status_patch_duration_ms",
			Help:      "Time spent on patching the status of a NetworkQoS",
		},
		[]string{"network"},
	)
)

func init() {
	prometheus.MustRegister(
		nqosCount,
		nqosOvnOperationDuration,
		nqosReconcileDuration,
		nqosPodReconcileDuration,
		nqosNamespaceReconcileDuration,
		nqosStatusPatchDuration,
	)
}

func (c *Controller) teardownMetricsCollector() {
	prometheus.Unregister(nqosCount)
}

// records the number of networkqos.
func updateNetworkQoSCount(network string, count int) {
	nqosCount.WithLabelValues(network).Set(float64(count))
}

// records the reconciliation duration for networkqos
func recordNetworkQoSReconcileDuration(network string, duration int64) {
	nqosReconcileDuration.WithLabelValues(network).Observe(float64(duration))
}

// records time spent on adding/removing a pod to/from networkqos rules
func recordPodReconcileDuration(network string, duration int64) {
	nqosPodReconcileDuration.WithLabelValues(network).Observe(float64(duration))
}

// records time spent on handling a namespace event which is involved in networkqos
func recordNamespaceReconcileDuration(network string, duration int64) {
	nqosNamespaceReconcileDuration.WithLabelValues(network).Observe(float64(duration))
}

// records time spent on an ovn operation
func recordOvnOperationDuration(operationType string, duration int64) {
	nqosOvnOperationDuration.WithLabelValues(operationType).Observe(float64(duration))
}

// records time spent on patching the status of a NetworkQoS
func recordStatusPatchDuration(network string, duration int64) {
	nqosStatusPatchDuration.WithLabelValues(network).Observe(float64(duration))
}
