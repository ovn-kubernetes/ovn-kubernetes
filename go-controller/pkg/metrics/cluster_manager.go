package metrics

import (
	"errors"
	"runtime"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

var registerClusterManagerBaseMetrics sync.Once
var registerClusterManagerFunctionalMetrics sync.Once

// MetricClusterManagerLeader identifies whether this instance of ovnkube-cluster-manager is a leader or not
var MetricClusterManagerLeader = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "leader",
	Help:      "Identifies whether the instance of ovnkube-cluster-manager is a leader(1) or not(0).",
})

var MetricClusterManagerReadyDuration = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "ready_duration_seconds",
	Help:      "The duration for the cluster manager to get to ready state",
})

var metricV4HostSubnetCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "num_v4_host_subnets",
	Help:      "The total number of v4 host subnets possible per network"},
	[]string{
		"network_name",
	},
)

var metricV6HostSubnetCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "num_v6_host_subnets",
	Help:      "The total number of v6 host subnets possible per network"},
	[]string{
		"network_name",
	},
)

var metricV4AllocatedHostSubnetCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "allocated_v4_host_subnets",
	Help:      "The total number of v4 host subnets currently allocated per network"},
	[]string{
		"network_name",
	},
)

var metricV6AllocatedHostSubnetCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "allocated_v6_host_subnets",
	Help:      "The total number of v6 host subnets currently allocated per network"},
	[]string{
		"network_name",
	},
)

/** EgressIP metrics recorded from cluster-manager begins**/
var metricEgressIPCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "num_egress_ips",
	Help:      "The number of defined egress IP addresses",
})

var metricEgressIPNodeUnreacheableCount = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "egress_ips_node_unreachable_total",
	Help:      "The total number of times assigned egress IP(s) were unreachable"},
)

var metricEgressIPRebalanceCount = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "egress_ips_rebalance_total",
	Help:      "The total number of times assigned egress IP(s) needed to be moved to a different node"},
)

/** EgressIP metrics recorded from cluster-manager ends**/

var metricUDNCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "user_defined_networks",
	Help:      "The total number of UserDefinedNetworks in the cluster"},
	[]string{
		"role",
		"topology",
	},
)

var metricCUDNCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: types.MetricOvnkubeNamespace,
	Subsystem: types.MetricOvnkubeSubsystemClusterManager,
	Name:      "cluster_user_defined_networks",
	Help:      "The total number of ClusterUserDefinedNetworks in the cluster"},
	[]string{
		"role",
		"topology",
	},
)

var metricUDNNodesRendered = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: types.MetricOvnkubeNamespace,
		Subsystem: types.MetricOvnkubeSubsystemClusterManager,
		Name:      "udn_nodes_rendered",
		Help:      "Number of nodes on which a UserDefinedNetwork (UDN) or ClusterUserDefinedNetwork (CUDN) is currently rendered.",
	},
	[]string{
		"network_name",
	},
)

// RegisterClusterManagerBase registers ovnkube cluster manager base metrics with the Prometheus registry.
// This function should only be called once.
func RegisterClusterManagerBase() {
	registerClusterManagerBaseMetrics.Do(func() {
		prometheus.MustRegister(MetricClusterManagerLeader)
		prometheus.MustRegister(MetricClusterManagerReadyDuration)
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: types.MetricOvnkubeNamespace,
				Subsystem: types.MetricOvnkubeSubsystemClusterManager,
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
	})
}

// RegisterClusterManagerFunctional is a collection of metrics that help us understand ovnkube-cluster-manager functions. Call once after
// LE is won.
func RegisterClusterManagerFunctional() {
	registerClusterManagerFunctionalMetrics.Do(func() {
		prometheus.MustRegister(metricV4HostSubnetCount)
		prometheus.MustRegister(metricV6HostSubnetCount)
		prometheus.MustRegister(metricV4AllocatedHostSubnetCount)
		prometheus.MustRegister(metricV6AllocatedHostSubnetCount)
		if config.OVNKubernetesFeature.EnableEgressIP {
			prometheus.MustRegister(metricEgressIPNodeUnreacheableCount)
			prometheus.MustRegister(metricEgressIPRebalanceCount)
			prometheus.MustRegister(metricEgressIPCount)
		}
		prometheus.MustRegister(metricUDNCount)
		prometheus.MustRegister(metricCUDNCount)
		if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
			prometheus.MustRegister(metricUDNNodesRendered)
		}
		if err := prometheus.Register(MetricResourceRetryFailuresCount); err != nil {
			var alreadyRegistered prometheus.AlreadyRegisteredError
			if !errors.As(err, &alreadyRegistered) {
				panic(err)
			}
		}
	})
}

// RecordSubnetUsage records the number of subnets allocated for nodes
func RecordSubnetUsage(v4SubnetsAllocated, v6SubnetsAllocated float64, networkName string) {
	metricV4AllocatedHostSubnetCount.WithLabelValues(networkName).Set(v4SubnetsAllocated)
	metricV6AllocatedHostSubnetCount.WithLabelValues(networkName).Set(v6SubnetsAllocated)
}

// RecordSubnetCount records the number of available subnets per configuration
// for ovn-kubernetes
func RecordSubnetCount(v4SubnetCount, v6SubnetCount float64, networkName string) {
	metricV4HostSubnetCount.WithLabelValues(networkName).Set(v4SubnetCount)
	metricV6HostSubnetCount.WithLabelValues(networkName).Set(v6SubnetCount)
}

// RecordEgressIPReachableNode records how many times EgressIP detected an unuseable node.
func RecordEgressIPUnreachableNode() {
	metricEgressIPNodeUnreacheableCount.Inc()
}

// RecordEgressIPRebalance records how many EgressIPs had to move to a different egress node.
func RecordEgressIPRebalance(count int) {
	metricEgressIPRebalanceCount.Add(float64(count))
}

// RecordEgressIPCount records the total number of Egress IPs.
// This total may include multiple Egress IPs per EgressIP CR.
func RecordEgressIPCount(count float64) {
	metricEgressIPCount.Set(count)
}

// IncrementUDNCount increments the number of UserDefinedNetworks of the given type
func IncrementUDNCount(role, topology string) {
	metricUDNCount.WithLabelValues(role, topology).Inc()
}

// DecrementUDNCount decrements the number of UserDefinedNetworks of the given type
func DecrementUDNCount(role, topology string) {
	metricUDNCount.WithLabelValues(role, topology).Dec()
}

// IncrementCUDNCount increments the number of ClusterUserDefinedNetworks of the given type
func IncrementCUDNCount(role, topology string) {
	metricCUDNCount.WithLabelValues(role, topology).Inc()
}

// DecrementCUDNCount decrements the number of ClusterUserDefinedNetworks of the given type
func DecrementCUDNCount(role, topology string) {
	metricCUDNCount.WithLabelValues(role, topology).Dec()
}

// SetDynamicUDNNodeCount sets the number of nodes currently active with a CUDN/UDN.
func SetDynamicUDNNodeCount(networkName string, nodeCount float64) {
	metricUDNNodesRendered.WithLabelValues(networkName).Set(nodeCount)
}

// DeleteDynamicUDNNodeCount when CUDN/UDN is deleted.
func DeleteDynamicUDNNodeCount(networkName string) {
	metricUDNNodesRendered.DeleteLabelValues(networkName)
}
