package metrics

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	kapimtypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	klog "k8s.io/klog/v2"

	"github.com/ovn-kubernetes/libovsdb/cache"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// metricNbE2eTimestamp is the UNIX timestamp value set to NB DB. Northd will eventually copy this
// timestamp from NB DB to SB DB. The metric 'sb_e2e_timestamp' stores the timestamp that is
// read from SB DB. This is registered within func RunTimestamp in order to allow gathering this
// metric on the fly when metrics are scraped.
var metricNbE2eTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "nb_e2e_timestamp",
	Help:      "The current e2e-timestamp value as written to the northbound database"},
)

// metricDbTimestamp is the UNIX timestamp seen in NB and SB DBs.
var metricDbTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnNamespace,
	Subsystem: types.MetricOvnSubsystemDB,
	Name:      "e2e_timestamp",
	Help:      "The current e2e-timestamp value as observed in this instance of the database"},
	[]string{
		"db_name",
	},
)

// metricPodCreationLatency is the time between a pod being scheduled and
// completing its logical switch port configuration.
var metricPodCreationLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "pod_creation_latency_seconds",
	Help:      "The duration between a pod being scheduled and completing its logical switch port configuration",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15),
})

// MetricResourceUpdateCount is the number of times a particular resource's UpdateFunc has been called.
var MetricResourceUpdateCount = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "resource_update_total",
	Help:      "The number of times a given resource event (add, update, or delete) has been handled"},
	[]string{
		"name",
		"event",
	},
)

// MetricResourceAddLatency is the time taken to complete resource update by an handler.
// This measures the latency for all of the handlers for a given resource.
var MetricResourceAddLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "resource_add_latency_seconds",
	Help:      "The duration to process all handlers for a given resource event - add.",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
)

// MetricResourceUpdateLatency is the time taken to complete resource update by an handler.
// This measures the latency for all of the handlers for a given resource.
var MetricResourceUpdateLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "resource_update_latency_seconds",
	Help:      "The duration to process all handlers for a given resource event - update.",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
)

// MetricResourceDeleteLatency is the time taken to complete resource update by an handler.
// This measures the latency for all of the handlers for a given resource.
var MetricResourceDeleteLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "resource_delete_latency_seconds",
	Help:      "The duration to process all handlers for a given resource event - delete.",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
)

// MetricRequeueServiceCount is the number of times a particular service has been requeued.
var MetricRequeueServiceCount = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "requeue_service_total",
	Help:      "A metric that captures the number of times a service is requeued after failing to sync with OVN"},
)

// MetricSyncServiceCount is the number of times a particular service has been synced.
var MetricSyncServiceCount = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "sync_service_total",
	Help:      "A metric that captures the number of times a service is synced with OVN load balancers"},
)

// MetricSyncServiceLatency is the time taken to sync a service with the OVN load balancers.
var MetricSyncServiceLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "sync_service_latency_seconds",
	Help:      "The latency of syncing a service with the OVN load balancers",
	Buckets:   prometheus.ExponentialBuckets(.1, 2, 15)},
)

var MetricOVNKubeControllerReadyDuration = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "ready_duration_seconds",
	Help:      "The duration for the ovnkube-controller to get to ready state",
})

// MetricOVNKubeControllerSyncDuration is the time taken to complete initial Watch for different resource.
// Resource name is in the label.
var MetricOVNKubeControllerSyncDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "sync_duration_seconds",
	Help:      "The duration to sync and setup all handlers for a given resource"},
	[]string{
		"resource_name",
	})

// MetricOVNKubeControllerLeader identifies whether this instance of ovnkube-controller is a leader or not
var MetricOVNKubeControllerLeader = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "leader",
	Help:      "Identifies whether the instance of ovnkube-controller is a leader(1) or not(0).",
})

var metricOvnKubeControllerLogFileSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "logfile_size_bytes",
	Help:      "The size of ovnkube-controller log file."},
	[]string{
		"logfile_name",
	},
)

var metricEgressIPAssignLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "egress_ips_assign_latency_seconds",
	Help:      "The latency of egress IP assignment to ovn nb database",
	Buckets:   prometheus.ExponentialBuckets(.001, 2, 15),
})

var metricEgressIPUnassignLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "egress_ips_unassign_latency_seconds",
	Help:      "The latency of egress IP unassignment from ovn nb database",
	Buckets:   prometheus.ExponentialBuckets(.001, 2, 15),
})

var metricNetpolEventLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "network_policy_event_latency_seconds",
	Help:      "The latency of full network policy event handling (create, delete)",
	Buckets:   prometheus.ExponentialBuckets(.004, 2, 15)},
	[]string{
		"event",
	})

var metricNetpolLocalPodEventLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "network_policy_local_pod_event_latency_seconds",
	Help:      "The latency of local pod events handling (add, delete)",
	Buckets:   prometheus.ExponentialBuckets(.002, 2, 15)},
	[]string{
		"event",
	})

var metricNetpolPeerNamespaceEventLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "network_policy_peer_namespace_event_latency_seconds",
	Help:      "The latency of peer namespace events handling (add, delete)",
	Buckets:   prometheus.ExponentialBuckets(.002, 2, 15)},
	[]string{
		"event",
	})

var metricPodSelectorAddrSetPodEventLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "pod_selector_address_set_pod_event_latency_seconds",
	Help:      "The latency of peer pod events handling (add, delete)",
	Buckets:   prometheus.ExponentialBuckets(.002, 2, 15)},
	[]string{
		"event",
	})

var metricPodSelectorAddrSetNamespaceEventLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "pod_selector_address_set_namespace_event_latency_seconds",
	Help:      "The latency of peer namespace events handling (add, delete)",
	Buckets:   prometheus.ExponentialBuckets(.002, 2, 15)},
	[]string{
		"event",
	})

var metricPodEventLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "pod_event_latency_seconds",
	Help:      "The latency of pod events handling (add, update, delete)",
	Buckets:   prometheus.ExponentialBuckets(.002, 2, 15)},
	[]string{
		"event",
	})

var metricEgressFirewallRuleCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "num_egress_firewall_rules",
	Help:      "The number of egress firewall rules defined"},
)

var metricIPsecEnabled = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      types.MetricIPsecEnabled,
	Help:      "Specifies whether IPSec is enabled for this cluster(1) or not enabled for this cluster(0)",
})

var metricIPsecTunnelIKEChildSAState = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      types.MetricIPsecTunnelIKEChildSAState,
	Help:      "Specifies whether IPSec Child SAs are established for the Geneve tunnels(1) or not(0)",
})

var metricEgressRoutingViaHost = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "egress_routing_via_host",
	Help:      "Specifies whether egress gateway mode is via host networking stack(1) or not(0)",
})

var metricEgressFirewallCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "num_egress_firewalls",
	Help:      "The number of egress firewall policies",
})

/** AdminNetworkPolicyMetrics Begin**/
var metricANPCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "admin_network_policies",
	Help:      "The total number of admin network policies in the cluster",
})

var metricBANPCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "baseline_admin_network_policies",
	Help:      "The total number of baseline admin network policies in the cluster",
})

var metricANPDBObjects = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "admin_network_policies_db_objects",
	Help:      "The total number of OVN NBDB objects (table_name) owned by AdminNetworkPolicy controller in the cluster"},
	[]string{
		"table_name",
	},
)

var metricBANPDBObjects = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "baseline_admin_network_policies_db_objects",
	Help:      "The total number of OVN NBDB objects (table_name) owned by BaselineAdminNetworkPolicy controller in the cluster"},
	[]string{
		"table_name",
	},
)

/** AdminNetworkPolicyMetrics End**/

// metricFirstSeenLSPLatency is the time between a pod first seen in OVN-Kubernetes and its Logical Switch Port is created
var metricFirstSeenLSPLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "pod_first_seen_lsp_created_duration_seconds",
	Help:      "The duration between a pod first observed in OVN-Kubernetes and Logical Switch Port created",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 15),
})

var metricLSPPortBindingLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "pod_lsp_created_port_binding_duration_seconds",
	Help:      "The duration between a pods Logical Switch Port created and port binding observed in cache",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 15),
})

var metricPortBindingChassisLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "pod_port_binding_port_binding_chassis_duration_seconds",
	Help:      "The duration between a pods port binding observed and port binding chassis update observed in cache",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 15),
})

var metricPortBindingUpLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemController,
	Name:      "pod_port_binding_chassis_port_binding_up_duration_seconds",
	Help:      "The duration between a pods port binding chassis update and port binding up observed in cache",
	Buckets:   prometheus.ExponentialBuckets(.01, 2, 15),
})

// GaugeVecInterface interface for GaugeVec metric so that it can be mocked for
// unit testing. This can be replaced once prometheus library provides such
// interface for GaugeVec as similar to prometheus.ObserverVec for HistogramVec.
type GaugeVecInterface interface {
	GetMetricWithLabelValues(lvs ...string) (prometheus.Gauge, error)
	GetMetricWith(labels prometheus.Labels) (prometheus.Gauge, error)
	WithLabelValues(lvs ...string) prometheus.Gauge
	With(labels prometheus.Labels) prometheus.Gauge
	MustCurryWith(labels prometheus.Labels) *prometheus.GaugeVec
	DeleteLabelValues(lvs ...string) bool
	Delete(labels prometheus.Labels) bool
	DeletePartialMatch(labels prometheus.Labels) int
	Describe(chan<- *prometheus.Desc)
	Collect(chan<- prometheus.Metric)
	Reset()
}

const (
	globalOptionsTimestampField     = "e2e_timestamp"
	globalOptionsProbeIntervalField = "northd_probe_interval"
)

// RegisterOVNKubeControllerBase registers ovnkube controller base metrics with the Prometheus registry.
// This function should only be called once.
func RegisterOVNKubeControllerBase() {
	prometheus.MustRegister(MetricOVNKubeControllerLeader)
	prometheus.MustRegister(MetricOVNKubeControllerReadyDuration)
	prometheus.MustRegister(MetricOVNKubeControllerSyncDuration)
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "build_info",
			Help: "A metric with a constant '1' value labeled by version, revision, branch, " +
				"and go version from which ovnkube was built and when and who built it",
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
}

// RegisterOVNKubeControllerPerformance registers metrics that help us understand ovnkube-controller performance. Call once after LE is won.
func RegisterOVNKubeControllerPerformance(nbClient libovsdbclient.Client) {
	// No need to unregister because process exits when leadership is lost.
	prometheus.MustRegister(metricPodCreationLatency)
	prometheus.MustRegister(MetricResourceUpdateCount)
	prometheus.MustRegister(MetricResourceAddLatency)
	prometheus.MustRegister(MetricResourceUpdateLatency)
	prometheus.MustRegister(MetricResourceDeleteLatency)
	prometheus.MustRegister(MetricRequeueServiceCount)
	prometheus.MustRegister(MetricSyncServiceCount)
	prometheus.MustRegister(MetricSyncServiceLatency)
	registerWorkqueueMetrics(types.MetricOvnkubeNamespace, types.MetricOvnkubeSubsystemController)
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: types.MetricOvnNamespace,
			Subsystem: types.MetricOvnSubsystemNorthd,
			Name:      "northd_probe_interval",
			Help: "The maximum number of milliseconds of idle time on connection to the OVN SB " +
				"and NB DB before sending an inactivity probe message",
		}, func() float64 {
			return getGlobalOptionsValue(nbClient, globalOptionsProbeIntervalField)
		},
	))
}

// RegisterOVNKubeControllerFunctional is a collection of metrics that help us understand ovnkube-controller functions. Call once after
// LE is won.
func RegisterOVNKubeControllerFunctional(stopChan <-chan struct{}) {
	// No need to unregister because process exits when leadership is lost.
	if config.Metrics.EnableScaleMetrics {
		klog.Infof("Scale metrics are enabled")
		prometheus.MustRegister(metricEgressIPAssignLatency)
		prometheus.MustRegister(metricEgressIPUnassignLatency)
		prometheus.MustRegister(metricNetpolEventLatency)
		prometheus.MustRegister(metricNetpolLocalPodEventLatency)
		prometheus.MustRegister(metricNetpolPeerNamespaceEventLatency)
		prometheus.MustRegister(metricPodSelectorAddrSetPodEventLatency)
		prometheus.MustRegister(metricPodSelectorAddrSetNamespaceEventLatency)
		prometheus.MustRegister(metricPodEventLatency)
	}
	prometheus.MustRegister(metricEgressFirewallRuleCount)
	prometheus.MustRegister(metricEgressFirewallCount)
	prometheus.MustRegister(metricEgressRoutingViaHost)
	prometheus.MustRegister(metricANPCount)
	prometheus.MustRegister(metricBANPCount)
	if err := prometheus.Register(MetricResourceRetryFailuresCount); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			panic(err)
		}
	}
	// ovnkube-controller logfile size metric
	prometheus.MustRegister(metricOvnKubeControllerLogFileSize)
	go ovnKubeLogFileSizeMetricsUpdater(metricOvnKubeControllerLogFileSize, stopChan)
}

func registerOVNKubeFeatureDBObjectsMetrics() {
	prometheus.MustRegister(metricANPDBObjects)
	prometheus.MustRegister(metricBANPDBObjects)
}

func RunOVNKubeFeatureDBObjectsMetricsUpdater(ovnNBClient libovsdbclient.Client, controllerName string, tickPeriod time.Duration, stopChan <-chan struct{}) {
	registerOVNKubeFeatureDBObjectsMetrics()
	go func() {
		ticker := time.NewTicker(tickPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				updateOVNKubeFeatureNBDBObjectMetrics(ovnNBClient, controllerName)
			case <-stopChan:
				return
			}
		}
	}()
}

func updateOVNKubeFeatureNBDBObjectMetrics(ovnNBClient libovsdbclient.Client, controllerName string) {
	if config.OVNKubernetesFeature.EnableAdminNetworkPolicy {
		// ANP Feature
		// 1. ACL 2. AddressSet (TODO: Add PG once indexing is done)
		aclCount := libovsdbutil.GetACLCount(ovnNBClient, libovsdbops.ACLAdminNetworkPolicy, controllerName)
		metricANPDBObjects.WithLabelValues(nbdb.ACLTable).Set(float64(aclCount))
		addressSetCount := libovsdbutil.GetAddressSetCount(ovnNBClient, libovsdbops.AddressSetAdminNetworkPolicy, controllerName)
		metricANPDBObjects.WithLabelValues(nbdb.AddressSetTable).Set(float64(addressSetCount))

		// BANP Feature
		// 1. ACL 2. AddressSet (TODO: Add PG once indexing is done)
		aclCount = libovsdbutil.GetACLCount(ovnNBClient, libovsdbops.ACLBaselineAdminNetworkPolicy, controllerName)
		metricBANPDBObjects.WithLabelValues(nbdb.ACLTable).Set(float64(aclCount))
		addressSetCount = libovsdbutil.GetAddressSetCount(ovnNBClient, libovsdbops.AddressSetBaselineAdminNetworkPolicy, controllerName)
		metricBANPDBObjects.WithLabelValues(nbdb.AddressSetTable).Set(float64(addressSetCount))
	}
}

// RunTimestamp adds a goroutine that registers and updates timestamp metrics.
// This is so we can determine 'freshness' of the components NB/SB DB and northd.
// Function must be called once.
func RunTimestamp(stopChan <-chan struct{}, sbClient, nbClient libovsdbclient.Client) {
	// Metric named nb_e2e_timestamp is the UNIX timestamp this instance wrote to NB DB. Updated every 30s with the
	// current timestamp.
	prometheus.MustRegister(metricNbE2eTimestamp)

	// Metric named sb_e2e_timestamp is the UNIX timestamp observed in SB DB. The value is read from the SB DB
	// cache when metrics HTTP endpoint is scraped.
	prometheus.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemController,
			Name:      "sb_e2e_timestamp",
			Help:      "The current e2e-timestamp value as observed in the southbound database",
		}, func() float64 {
			return getGlobalOptionsValue(sbClient, globalOptionsTimestampField)
		}))

	// Metric named e2e_timestamp is the UNIX timestamp observed in NB and SB DBs cache with the DB name
	// (OVN_Northbound|OVN_Southbound) set as a label. Updated every 30s.
	prometheus.MustRegister(metricDbTimestamp)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				currentTime := time.Now().Unix()
				if setNbE2eTimestamp(nbClient, currentTime) {
					metricNbE2eTimestamp.Set(float64(currentTime))
				} else {
					metricNbE2eTimestamp.Set(0)
				}

				metricDbTimestamp.WithLabelValues(nbClient.Schema().Name).Set(getGlobalOptionsValue(nbClient, globalOptionsTimestampField))
				metricDbTimestamp.WithLabelValues(sbClient.Schema().Name).Set(getGlobalOptionsValue(sbClient, globalOptionsTimestampField))
			case <-stopChan:
				return
			}
		}
	}()
}

// RecordPodCreated extracts the scheduled timestamp and records how long it took
// us to notice this and set up the pod's scheduling.
func RecordPodCreated(pod *corev1.Pod, netInfo util.NetInfo) {
	if netInfo.IsUserDefinedNetwork() {
		// TBD: noop for UDN for now
		return
	}
	t := time.Now()

	// Find the scheduled timestamp
	for _, cond := range pod.Status.Conditions {
		if cond.Type != corev1.PodScheduled {
			continue
		}
		if cond.Status != corev1.ConditionTrue {
			return
		}
		creationLatency := t.Sub(cond.LastTransitionTime.Time).Seconds()
		metricPodCreationLatency.Observe(creationLatency)
		return
	}
}

// RecordEgressIPAssign records how long it took EgressIP to configure OVN.
func RecordEgressIPAssign(duration time.Duration) {
	metricEgressIPAssignLatency.Observe(duration.Seconds())
}

// RecordEgressIPUnassign records how long it took EgressIP to unconfigure OVN.
func RecordEgressIPUnassign(duration time.Duration) {
	metricEgressIPUnassignLatency.Observe(duration.Seconds())
}

func RecordNetpolEvent(eventName string, duration time.Duration) {
	metricNetpolEventLatency.WithLabelValues(eventName).Observe(duration.Seconds())
}

func RecordNetpolLocalPodEvent(eventName string, duration time.Duration) {
	metricNetpolLocalPodEventLatency.WithLabelValues(eventName).Observe(duration.Seconds())
}

func RecordNetpolPeerNamespaceEvent(eventName string, duration time.Duration) {
	metricNetpolPeerNamespaceEventLatency.WithLabelValues(eventName).Observe(duration.Seconds())
}

func RecordPodSelectorAddrSetPodEvent(eventName string, duration time.Duration) {
	metricPodSelectorAddrSetPodEventLatency.WithLabelValues(eventName).Observe(duration.Seconds())
}

func RecordPodSelectorAddrSetNamespaceEvent(eventName string, duration time.Duration) {
	metricPodSelectorAddrSetNamespaceEventLatency.WithLabelValues(eventName).Observe(duration.Seconds())
}

func RecordPodEvent(eventName string, duration time.Duration) {
	metricPodEventLatency.WithLabelValues(eventName).Observe(duration.Seconds())
}

// UpdateEgressFirewallRuleCount records the number of Egress firewall rules.
func UpdateEgressFirewallRuleCount(count float64) {
	metricEgressFirewallRuleCount.Add(count)
}

// RecordEgressRoutingViaHost records the egress gateway mode of the cluster
// The values are:
// 0: If it is shared gateway mode
// 1: If it is local gateway mode
// 2: invalid mode
func RecordEgressRoutingViaHost() {
	if config.Gateway.Mode == config.GatewayModeLocal {
		// routingViaHost is enabled
		metricEgressRoutingViaHost.Set(1)
	} else if config.Gateway.Mode == config.GatewayModeShared {
		// routingViaOVN is enabled
		metricEgressRoutingViaHost.Set(0)
	} else {
		// invalid mode
		metricEgressRoutingViaHost.Set(2)
	}
}

// MonitorIPSec will register a metric to determine if IPSec is enabled/disabled. It will also add a handler
// to NB libovsdb cache to update the IPSec metric.
// This function should only be called once.
func MonitorIPSec(ovnNBClient libovsdbclient.Client) {
	prometheus.MustRegister(metricIPsecEnabled)
	ovnNBClient.Cache().AddEventHandler(&cache.EventHandlerFuncs{
		AddFunc: func(table string, model model.Model) {
			ipsecMetricHandler(table, model)
		},
		UpdateFunc: func(table string, _, new model.Model) {
			ipsecMetricHandler(table, new)
		},
		DeleteFunc: func(table string, model model.Model) {
			ipsecMetricHandler(table, model)
		},
	})
}

func ipsecMetricHandler(table string, model model.Model) {
	if table != "NB_Global" {
		return
	}
	entry := model.(*nbdb.NBGlobal)
	if entry.Ipsec {
		metricIPsecEnabled.Set(1)
	} else {
		metricIPsecEnabled.Set(0)
	}
}

// subscribeFn defines the function signature for subscribing to xfrm events.
type xfrmSubscribeFn func() (bool, chan []syscall.NetlinkMessage, *nl.NetlinkSocket, error)

// MonitorIPsecTunnelsState will register a metric for the ipsec tunnel ike child sa establishment status.
// It will then subscribe to xfrm netlink events and update the metric with ike child sa establishment up
// or down status.
func MonitorIPsecTunnelsState(stopChan <-chan struct{}, wg *sync.WaitGroup, ovsVsctl ovsClient, ipsec ipsecClient) error {
	if err := prometheus.Register(metricIPsecTunnelIKEChildSAState); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			return err
		}
	}
	subscribe := func() (bool, chan []syscall.NetlinkMessage, *nl.NetlinkSocket, error) {
		groups, err := getXfrmMcastGroups([]nl.XfrmMsgType{nl.XFRM_MSG_NEWSA, nl.XFRM_MSG_DELSA, nl.XFRM_MSG_UPDSA,
			nl.XFRM_MSG_NEWPOLICY, nl.XFRM_MSG_DELPOLICY, nl.XFRM_MSG_UPDPOLICY})
		if err != nil {
			return false, nil, nil, fmt.Errorf("error creating XFRM groups: %v", err)
		}
		sock, err := nl.SubscribeAt(netns.None(), netns.None(), unix.NETLINK_XFRM, groups...)
		if err != nil {
			return false, nil, nil, fmt.Errorf("error subscribing XFRM events: %v", err)
		}
		msgCh := make(chan []syscall.NetlinkMessage)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(msgCh)
			for {
				msgs, from, err := sock.Receive()
				if err != nil {
					klog.Infof("XFRM reader exiting: %v", err)
					return
				}
				if from.Pid != nl.PidKernel {
					klog.Warningf("Unexpected XFRM sender pid %d (expected %d)", from.Pid, nl.PidKernel)
					continue
				}
				msgCh <- msgs
			}
		}()
		// Initial reconciliation
		updateIPsecTunnelStateMetric(ovsVsctl, ipsec)
		return true, msgCh, sock, nil
	}

	return runInternalIPsec(stopChan, wg, ovsVsctl, ipsec, subscribe)
}

// reconcile period for xfrm event handler, this would kick in for every 60 seconds if
// there is no explicit xfrm events. when xfrm event occurs, reconcile period is
// automatically extended by another 60 seconds.
var xfrmHandlerReconcilePeriod = 60 * time.Second

// runInternalIPsec runs the main monitoring loop with subscription handling.
func runInternalIPsec(stopChan <-chan struct{}, wg *sync.WaitGroup,
	ovsVsctl ovsClient, ipsec ipsecClient, subscribe xfrmSubscribeFn) error {
	// Get the current network namespace handle
	currentNs, err := ns.GetCurrentNS()
	if err != nil {
		return fmt.Errorf("error retrieving current net namespace, err: %v", err)
	}
	subscribed, xfrmCh, sock, err := subscribe()
	if err != nil {
		return err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err = currentNs.Do(func(_ ns.NetNS) error {
			xfrmSyncTimer := time.NewTicker(xfrmHandlerReconcilePeriod)
			defer xfrmSyncTimer.Stop()
			for {
				select {
				case msgs, ok := <-xfrmCh:
					xfrmSyncTimer.Reset(xfrmHandlerReconcilePeriod)
					if !ok {
						// Channel closed (i.e. netlink socket reader is exited)
						// Check if shutdown is requested.
						select {
						case <-stopChan:
							return nil
						default:
							if subscribed, xfrmCh, sock, err = subscribe(); err != nil {
								klog.Errorf("Error during XFRM re-subscribe due to channel closing: %v", err)
							}
						}
						continue
					}
					if len(msgs) > 0 {
						updateIPsecTunnelStateMetric(ovsVsctl, ipsec)
					}
				case <-xfrmSyncTimer.C:
					// Nothing else to do other than re-subscribe.
					if !subscribed {
						if subscribed, xfrmCh, sock, err = subscribe(); err != nil {
							klog.Errorf("Error during XFRM events re-subscribe: %v", err)
						}
					}
				case <-stopChan:
					// Trigger xfrm netlink socket reader exit.
					if sock != nil {
						sock.Close()
					}
					return nil
				}
			}
		})
		if err != nil {
			klog.Errorf("Failed to run XFRM event handler goroutine, err: %v", err)
		}
	}()
	return nil
}

func getXfrmMcastGroups(types []nl.XfrmMsgType) ([]uint, error) {
	groups := make([]uint, 0)
	if len(types) == 0 {
		return nil, fmt.Errorf("no xfrm msg type specified")
	}
	for _, t := range types {
		var group uint
		switch t {
		case nl.XFRM_MSG_ACQUIRE:
			group = nl.XFRMNLGRP_ACQUIRE
		case nl.XFRM_MSG_EXPIRE:
			group = nl.XFRMNLGRP_EXPIRE
		case nl.XFRM_MSG_NEWSA, nl.XFRM_MSG_DELSA, nl.XFRM_MSG_UPDSA:
			group = nl.XFRMNLGRP_SA
		case nl.XFRM_MSG_NEWPOLICY, nl.XFRM_MSG_DELPOLICY, nl.XFRM_MSG_UPDPOLICY:
			group = nl.XFRMNLGRP_POLICY
		default:
			return nil, fmt.Errorf("unsupported xfrm group: %x", t)
		}
		groups = append(groups, group)
	}
	return groups, nil
}

func updateIPsecTunnelStateMetric(ovsVsctl ovsClient, ipsec ipsecClient) {
	geneveTunnels, err := listGeneveTunnels(ovsVsctl)
	if err != nil {
		klog.Errorf("Error in retrieving Geneve tunnels, err: %v", err)
		return
	}
	if len(geneveTunnels) == 0 {
		metricIPsecTunnelIKEChildSAState.Set(0)
		return
	}
	ipsecTunnels, err := listEstablishedIPsecTunnels(ipsec)
	if err != nil {
		klog.Errorf("Error in retrieving established IPsec tunnels, err: %v", err)
		metricIPsecTunnelIKEChildSAState.Set(0)
		return
	}
	ipsecEstablished := true
	for _, geneveTunnel := range geneveTunnels {
		if geneveTunnel.AdminState == vswitchd.InterfaceAdminStateDown ||
			geneveTunnel.OperState == vswitchd.InterfaceLinkStateDown {
			ipsecEstablished = false
			break
		}
		if !ipsecTunnels.Has(fmt.Sprintf("%s-in-1", geneveTunnel.Name)) ||
			!ipsecTunnels.Has(fmt.Sprintf("%s-out-1", geneveTunnel.Name)) {
			ipsecEstablished = false
			break
		}
	}
	if ipsecEstablished {
		metricIPsecTunnelIKEChildSAState.Set(1)
	} else {
		metricIPsecTunnelIKEChildSAState.Set(0)
	}
}

// IncrementEgressFirewallCount increments the number of Egress firewalls
func IncrementEgressFirewallCount() {
	metricEgressFirewallCount.Inc()
}

// DecrementEgressFirewallCount decrements the number of Egress firewalls
func DecrementEgressFirewallCount() {
	metricEgressFirewallCount.Dec()
}

// IncrementANPCount increments the number of Admin Network Policies
func IncrementANPCount() {
	metricANPCount.Inc()
}

// DecrementANPCount decrements the number of Admin Network Policies
func DecrementANPCount() {
	metricANPCount.Dec()
}

// IncrementBANPCount increments the number of Baseline Admin Network Policies
func IncrementBANPCount() {
	metricBANPCount.Inc()
}

// DecrementBANPCount decrements the number of Baseline Admin Network Policies
func DecrementBANPCount() {
	metricBANPCount.Dec()
}

type (
	timestampType int
	operation     int
)

const (
	// pod event first handled by OVN-Kubernetes control plane
	firstSeen timestampType = iota
	// OVN-Kubernetes control plane created Logical Switch Port in northbound database
	logicalSwitchPort
	// port binding seen in OVN-Kubernetes control plane southbound database libovsdb cache
	portBinding
	// port binding with updated chassis seen in OVN-Kubernetes control plane southbound database libovsdb cache
	portBindingChassis
	// queue operations
	addPortBinding operation = iota
	updatePortBinding
	addPod
	cleanPod
	addLogicalSwitchPort
	queueCheckPeriod = time.Millisecond * 50
	// prevent OOM by limiting queue size
	queueLimit       = 10000
	portBindingTable = "Port_Binding"
)

type record struct {
	timestamp time.Time
	timestampType
}

type item struct {
	op        operation
	timestamp time.Time
	old       model.Model
	new       model.Model
	uid       kapimtypes.UID
}

type PodRecorder struct {
	records map[kapimtypes.UID]*record
	queue   workqueue.TypedInterface[*item]
}

func NewPodRecorder() PodRecorder {
	return PodRecorder{}
}

var podRecorderRegOnce sync.Once

// Run monitors pod setup latency
func (pr *PodRecorder) Run(sbClient libovsdbclient.Client, stop <-chan struct{}) {
	podRecorderRegOnce.Do(func() {
		prometheus.MustRegister(metricFirstSeenLSPLatency)
		prometheus.MustRegister(metricLSPPortBindingLatency)
		prometheus.MustRegister(metricPortBindingUpLatency)
		prometheus.MustRegister(metricPortBindingChassisLatency)
	})

	pr.queue = workqueue.NewTyped[*item]()
	pr.records = make(map[kapimtypes.UID]*record)

	sbClient.Cache().AddEventHandler(&cache.EventHandlerFuncs{
		AddFunc: func(table string, model model.Model) {
			if table != portBindingTable {
				return
			}
			if !pr.queueFull() {
				pr.queue.Add(&item{op: addPortBinding, old: model, timestamp: time.Now()})
			}
		},
		UpdateFunc: func(table string, old model.Model, new model.Model) {
			if table != portBindingTable {
				return
			}
			oldRow := old.(*sbdb.PortBinding)
			newRow := new.(*sbdb.PortBinding)
			// chassis assigned
			if oldRow.Chassis == nil && newRow.Chassis != nil {
				if !pr.queueFull() {
					pr.queue.Add(&item{op: updatePortBinding, old: old, new: new, timestamp: time.Now()})
				}
				// port binding up
			} else if oldRow.Up != nil && !*oldRow.Up && newRow.Up != nil && *newRow.Up {
				if !pr.queueFull() {
					pr.queue.Add(&item{op: updatePortBinding, old: old, new: new, timestamp: time.Now()})
				}
			}
		},
		DeleteFunc: func(_ string, _ model.Model) {
		},
	})

	go func() {
		wait.Until(pr.runWorker, queueCheckPeriod, stop)
		pr.queue.ShutDown()
	}()
}

func (pr *PodRecorder) AddPod(podUID kapimtypes.UID) {
	if pr.queue != nil && !pr.queueFull() {
		pr.queue.Add(&item{op: addPod, uid: podUID, timestamp: time.Now()})
	}
}

func (pr *PodRecorder) CleanPod(podUID kapimtypes.UID) {
	if pr.queue != nil && !pr.queueFull() {
		pr.queue.Add(&item{op: cleanPod, uid: podUID})
	}
}

func (pr *PodRecorder) AddLSP(podUID kapimtypes.UID, netInfo util.NetInfo) {
	if netInfo.IsUserDefinedNetwork() {
		// TBD: noop for UDN for now
		return
	}
	if pr.queue != nil && !pr.queueFull() {
		pr.queue.Add(&item{op: addLogicalSwitchPort, uid: podUID, timestamp: time.Now()})
	}
}

func (pr *PodRecorder) addLSP(podUID kapimtypes.UID, t time.Time) {
	var r *record
	if r = pr.getRecord(podUID); r == nil {
		klog.V(5).Infof("Add Logical Switch Port event expected pod with UID %q in cache", podUID)
		return
	}
	if r.timestampType != firstSeen {
		klog.V(5).Infof("Unexpected last event type (%d) in cache for pod with UID %q", r.timestampType, podUID)
		return
	}
	metricFirstSeenLSPLatency.Observe(t.Sub(r.timestamp).Seconds())
	r.timestamp = t
	r.timestampType = logicalSwitchPort
}

func (pr *PodRecorder) addPortBinding(m model.Model, t time.Time) {
	var r *record
	row := m.(*sbdb.PortBinding)
	podUID := getPodUIDFromPortBinding(row)
	if podUID == "" {
		return
	}
	if r = pr.getRecord(podUID); r == nil {
		klog.V(5).Infof("Add port binding event expected pod with UID %q in cache", podUID)
		return
	}
	if r.timestampType != logicalSwitchPort {
		klog.V(5).Infof("Unexpected last event entry (%d) in cache for pod with UID %q", r.timestampType, podUID)
		return
	}
	metricLSPPortBindingLatency.Observe(t.Sub(r.timestamp).Seconds())
	r.timestamp = t
	r.timestampType = portBinding
}

func (pr *PodRecorder) updatePortBinding(old, new model.Model, t time.Time) {
	var r *record
	oldRow := old.(*sbdb.PortBinding)
	newRow := new.(*sbdb.PortBinding)
	podUID := getPodUIDFromPortBinding(newRow)
	if podUID == "" {
		return
	}
	if r = pr.getRecord(podUID); r == nil {
		klog.V(5).Infof("Port binding update expected pod with UID %q in cache", podUID)
		return
	}

	if oldRow.Chassis == nil && newRow.Chassis != nil && r.timestampType == portBinding {
		metricPortBindingChassisLatency.Observe(t.Sub(r.timestamp).Seconds())
		r.timestamp = t
		r.timestampType = portBindingChassis

	}

	if oldRow.Up != nil && !*oldRow.Up && newRow.Up != nil && *newRow.Up && r.timestampType == portBindingChassis {
		metricPortBindingUpLatency.Observe(t.Sub(r.timestamp).Seconds())
		delete(pr.records, podUID)
	}
}

func (pr *PodRecorder) queueFull() bool {
	return pr.queue.Len() >= queueLimit
}

func (pr *PodRecorder) runWorker() {
	for pr.processNextItem() {
	}
}

func (pr *PodRecorder) processNextItem() bool {
	i, term := pr.queue.Get()
	if term {
		return false
	}
	pr.processItem(i)
	pr.queue.Done(i)
	return true
}

func (pr *PodRecorder) processItem(i *item) {
	switch i.op {
	case addPortBinding:
		pr.addPortBinding(i.old, i.timestamp)
	case updatePortBinding:
		pr.updatePortBinding(i.old, i.new, i.timestamp)
	case addPod:
		pr.records[i.uid] = &record{timestamp: i.timestamp, timestampType: firstSeen}
	case cleanPod:
		delete(pr.records, i.uid)
	case addLogicalSwitchPort:
		pr.addLSP(i.uid, i.timestamp)
	}
}

// getRecord returns record from map with func argument as the key
func (pr *PodRecorder) getRecord(podUID kapimtypes.UID) *record {
	r, ok := pr.records[podUID]
	if !ok {
		klog.V(5).Infof("Cache entry expected pod with UID %q but failed to find it", podUID)
		return nil
	}
	return r
}

func getPodUIDFromPortBinding(row *sbdb.PortBinding) kapimtypes.UID {
	if isPod, ok := row.ExternalIDs["pod"]; !ok || isPod != "true" {
		return ""
	}
	podUID, ok := row.Options["iface-id-ver"]
	if !ok {
		return ""
	}
	return kapimtypes.UID(podUID)
}

// setNbE2eTimestamp return true if setting timestamp to NB global options is successful
func setNbE2eTimestamp(ovnNBClient libovsdbclient.Client, timestamp int64) bool {
	// assumption that only first row is relevant in NB_Global table
	nbGlobal := nbdb.NBGlobal{
		Options: map[string]string{globalOptionsTimestampField: fmt.Sprintf("%d", timestamp)},
	}
	if err := libovsdbops.UpdateNBGlobalSetOptions(ovnNBClient, &nbGlobal); err != nil {
		klog.Errorf("Unable to update NB global options E2E timestamp metric err: %v", err)
		return false
	}
	return true
}

func getGlobalOptionsValue(client libovsdbclient.Client, field string) float64 {
	var options map[string]string
	dbName := client.Schema().Name
	nbGlobal := nbdb.NBGlobal{}
	sbGlobal := sbdb.SBGlobal{}

	if dbName == "OVN_Northbound" {
		if nbGlobal, err := libovsdbops.GetNBGlobal(client, &nbGlobal); err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
			klog.Errorf("Failed to get NB_Global table err: %v", err)
			return 0
		} else {
			options = nbGlobal.Options
		}
	}

	if dbName == "OVN_Southbound" {
		if sbGlobal, err := libovsdbops.GetSBGlobal(client, &sbGlobal); err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
			klog.Errorf("Failed to get SB_Global table err: %v", err)
			return 0
		} else {
			options = sbGlobal.Options
		}
	}

	if v, ok := options[field]; !ok {
		klog.V(5).Infof("Failed to find %q from %s options. This may occur at startup.", field, dbName)
		return 0
	} else {
		if value, err := strconv.ParseFloat(v, 64); err != nil {
			klog.Errorf("Failed to parse %q value to float64 err: %v", field, err)
			return 0
		} else {
			return value
		}
	}
}
