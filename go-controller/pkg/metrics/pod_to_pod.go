//go:build linux
// +build linux

package metrics

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// RESEARCH: Integration bridge name where pod-to-pod traffic flows
	integrationBridge = "br-int"
	// RESEARCH: Flow statistics collection interval
	podToPodMetricsInterval = 30 * time.Second
)

var (
	// RESEARCH: Prometheus counters for pod-to-pod traffic metrics
	metricPodToPodBytes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemNode,
			Name:      "pod_to_pod_bytes_total",
			Help:      "Total bytes of pod-to-pod traffic, labeled by traffic_type (same_node or cross_node)",
		},
		[]string{"node", "traffic_type"},
	)

	metricPodToPodPackets = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: types.MetricOvnkubeNamespace,
			Subsystem: types.MetricOvnkubeSubsystemNode,
			Name:      "pod_to_pod_packets_total",
			Help:      "Total packets of pod-to-pod traffic, labeled by traffic_type (same_node or cross_node)",
		},
		[]string{"node", "traffic_type"},
	)

	// RESEARCH: Flag to enable/disable pod-to-pod traffic instrumentation
	podToPodMetricsEnabled = false

	// RESEARCH: Mutex to protect metrics updates
	podToPodMetricsMutex sync.Mutex

	// RESEARCH: Last collected flow statistics to compute deltas
	lastFlowStats = make(map[string]flowStats)

	// RESEARCH: Regex to match OVS flow output with statistics
	flowStatsRegex = regexp.MustCompile(`n_packets=(\d+),n_bytes=(\d+)`)
)

type flowStats struct {
	packets uint64
	bytes   uint64
}

// RESEARCH: RegisterPodToPodMetrics registers the pod-to-pod traffic metrics with Prometheus
func RegisterPodToPodMetrics() {
	prometheus.MustRegister(metricPodToPodBytes)
	prometheus.MustRegister(metricPodToPodPackets)
}

// RESEARCH: EnablePodToPodMetrics enables pod-to-pod traffic instrumentation
func EnablePodToPodMetrics(enabled bool) {
	podToPodMetricsEnabled = enabled
	if enabled {
		klog.Info("RESEARCH: Pod-to-pod traffic metrics enabled")
	} else {
		klog.Info("RESEARCH: Pod-to-pod traffic metrics disabled")
	}
}

// RESEARCH: IsPodToPodMetricsEnabled returns whether pod-to-pod metrics are enabled
func IsPodToPodMetricsEnabled() bool {
	return podToPodMetricsEnabled
}

// RESEARCH: StartPodToPodMetricsCollector starts the periodic collection of pod-to-pod traffic metrics
func StartPodToPodMetricsCollector(nodeName string, stopChan <-chan struct{}) {
	if !podToPodMetricsEnabled {
		klog.V(5).Info("RESEARCH: Pod-to-pod metrics collector not started (disabled)")
		return
	}

	ticker := time.NewTicker(podToPodMetricsInterval)
	defer ticker.Stop()

	klog.Infof("RESEARCH: Starting pod-to-pod traffic metrics collector for node %s", nodeName)

	for {
		select {
		case <-ticker.C:
			if err := collectPodToPodMetrics(nodeName); err != nil {
				klog.Errorf("RESEARCH: Failed to collect pod-to-pod metrics: %v", err)
			}
		case <-stopChan:
			klog.Infof("RESEARCH: Stopping pod-to-pod traffic metrics collector for node %s", nodeName)
			return
		}
	}
}

// RESEARCH: collectPodToPodMetrics collects pod-to-pod traffic statistics from OVS flows
// It distinguishes same-node vs cross-node traffic by examining flow output ports:
// - Same-node: flows outputting to local pod ports (namespace_podname format)
// - Cross-node: flows outputting to gateway router ports (rtos-* format) or transit switch ports
func collectPodToPodMetrics(nodeName string) error {
	podToPodMetricsMutex.Lock()
	defer podToPodMetricsMutex.Unlock()

	// RESEARCH: Query OVS flows on br-int with statistics
	// We query table=0 which handles initial packet classification
	// Note: OVN uses multiple tables, but table=0 is where initial routing decisions are made
	stdout, stderr, err := util.RunOVSOfctl("-O", "OpenFlow13", "dump-flows", integrationBridge,
		"table=0", "--no-names")
	if err != nil {
		return fmt.Errorf("failed to dump flows from %s: %v (stderr: %s)", integrationBridge, err, stderr)
	}

	// RESEARCH: Parse flows and classify traffic
	sameNodeBytes := uint64(0)
	sameNodePackets := uint64(0)
	crossNodeBytes := uint64(0)
	crossNodePackets := uint64(0)

	flowLines := strings.Split(stdout, "\n")

	// RESEARCH: Process each flow line
	for _, line := range flowLines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// RESEARCH: Extract statistics from flow line
		matches := flowStatsRegex.FindStringSubmatch(line)
		if len(matches) != 3 {
			continue
		}

		packets, err := strconv.ParseUint(matches[1], 10, 64)
		if err != nil {
			continue
		}
		bytes, err := strconv.ParseUint(matches[2], 10, 64)
		if err != nil {
			continue
		}

		// RESEARCH: Classify flow as same-node or cross-node based on output port
		isSameNode, isCrossNode := classifyFlow(line, nodeName)
		if !isSameNode && !isCrossNode {
			continue
		}

		// RESEARCH: Create flow key for delta calculation
		flowKey := extractFlowKey(line)

		// RESEARCH: Calculate delta from last collection
		lastStats, hadLastStats := lastFlowStats[flowKey]
		var deltaPackets, deltaBytes uint64
		if hadLastStats {
			// RESEARCH: Handle counter wraparound
			if packets >= lastStats.packets {
				deltaPackets = packets - lastStats.packets
			} else {
				deltaPackets = packets // Counter wrapped, use current value
			}
			if bytes >= lastStats.bytes {
				deltaBytes = bytes - lastStats.bytes
			} else {
				deltaBytes = bytes // Counter wrapped, use current value
			}
		} else {
			// RESEARCH: First time seeing this flow, use current stats
			deltaPackets = packets
			deltaBytes = bytes
		}

		// RESEARCH: Update last seen statistics
		lastFlowStats[flowKey] = flowStats{packets: packets, bytes: bytes}

		if isSameNode {
			sameNodePackets += deltaPackets
			sameNodeBytes += deltaBytes
		} else if isCrossNode {
			crossNodePackets += deltaPackets
			crossNodeBytes += deltaBytes
		}
	}

	// RESEARCH: Update Prometheus metrics (always update, even if zero, to maintain counter continuity)
	metricPodToPodPackets.WithLabelValues(nodeName, "same_node").Add(float64(sameNodePackets))
	metricPodToPodBytes.WithLabelValues(nodeName, "same_node").Add(float64(sameNodeBytes))
	if sameNodePackets > 0 || sameNodeBytes > 0 {
		klog.V(5).Infof("RESEARCH: Same-node traffic: %d packets, %d bytes", sameNodePackets, sameNodeBytes)
	}

	metricPodToPodPackets.WithLabelValues(nodeName, "cross_node").Add(float64(crossNodePackets))
	metricPodToPodBytes.WithLabelValues(nodeName, "cross_node").Add(float64(crossNodeBytes))
	if crossNodePackets > 0 || crossNodeBytes > 0 {
		klog.V(5).Infof("RESEARCH: Cross-node traffic: %d packets, %d bytes", crossNodePackets, crossNodeBytes)
	}

	return nil
}

// RESEARCH: classifyFlow determines if a flow represents same-node or cross-node pod-to-pod traffic
// Returns: (isSameNode, isCrossNode)
// Classification logic:
// - Same-node: flows that output directly to pod ports (namespace_podname format)
// - Cross-node: flows that output to router ports (rtos-*, stor-*) or transit switch ports (tstor-*)
func classifyFlow(flow, nodeName string) (bool, bool) {
	// RESEARCH: Skip non-IP traffic (ARP, etc.)
	// Skip if ARP or if it doesn't contain either "ip" or "ipv6"
	if strings.Contains(flow, "arp") || (!strings.Contains(flow, "ip") && !strings.Contains(flow, "ipv6")) {
		return false, false
	}

	// RESEARCH: Extract output action from flow
	// Format: ... actions=output:"PORT_NAME" or actions=output:PORT_NUMBER
	// OVS flows can have multiple actions, so we look for output actions
	outputMatch := regexp.MustCompile(`actions=.*output:"([^"]+)"`)
	matches := outputMatch.FindStringSubmatch(flow)
	if len(matches) < 2 {
		// RESEARCH: Try numeric port format
		outputMatch = regexp.MustCompile(`actions=.*output:(\d+)`)
		matches = outputMatch.FindStringSubmatch(flow)
		if len(matches) < 2 {
			return false, false
		}
	}

	outputPort := matches[1]

	// RESEARCH: Same-node traffic: output to local pod port
	// Pod ports follow the format: namespace_podname (e.g., "default_my-pod")
	// These are direct outputs to pods on the same logical switch
	if strings.Contains(outputPort, "_") {
		// RESEARCH: Exclude router and switch ports
		if !strings.HasPrefix(outputPort, "rtos-") &&
			!strings.HasPrefix(outputPort, "stor-") &&
			!strings.HasPrefix(outputPort, "rtoj-") &&
			!strings.HasPrefix(outputPort, "tstor-") &&
			!strings.HasPrefix(outputPort, "rtoe-") {
			return true, false
		}
	}

	// RESEARCH: Cross-node traffic: output to router or transit switch ports
	// rtos-*: router-to-switch ports (traffic going to gateway router)
	// stor-*: switch-to-router ports
	// tstor-*: transit switch to router ports (interconnect)
	// rtoj-*: router to join switch ports
	crossNodePatterns := []string{
		"rtos-",  // Router to switch (gateway router)
		"stor-",  // Switch to router
		"tstor-", // Transit switch to router (interconnect)
		"rtoj-",  // Router to join switch
		"rtoe-",  // Router to external switch
	}

	for _, pattern := range crossNodePatterns {
		if strings.HasPrefix(outputPort, pattern) {
			return false, true
		}
	}

	// RESEARCH: If we can't classify, skip this flow
	return false, false
}

// RESEARCH: extractFlowKey creates a unique key for a flow based on its match criteria
// This is used to track flow statistics over time to calculate deltas
func extractFlowKey(flow string) string {
	// RESEARCH: Extract the match criteria and output port to create a unique key
	// Format: cookie=0x..., table=X, priority=Y, match... actions=output:PORT
	// We use table + priority + output port as the key
	tableMatch := regexp.MustCompile(`table=(\d+)`)
	priorityMatch := regexp.MustCompile(`priority=(\d+)`)
	outputMatch := regexp.MustCompile(`actions=.*output:"([^"]+)"`)

	table := ""
	priority := ""
	outputPort := ""

	if matches := tableMatch.FindStringSubmatch(flow); len(matches) >= 2 {
		table = matches[1]
	}
	if matches := priorityMatch.FindStringSubmatch(flow); len(matches) >= 2 {
		priority = matches[1]
	}
	if matches := outputMatch.FindStringSubmatch(flow); len(matches) >= 2 {
		outputPort = matches[1]
	} else {
		// RESEARCH: Try numeric port format
		outputMatch = regexp.MustCompile(`actions=.*output:(\d+)`)
		if matches := outputMatch.FindStringSubmatch(flow); len(matches) >= 2 {
			outputPort = matches[1]
		}
	}

	if table != "" && priority != "" && outputPort != "" {
		return fmt.Sprintf("%s:%s:%s", table, priority, outputPort)
	}

	// RESEARCH: Fallback: use a hash of the match criteria
	parts := strings.Split(flow, "actions=")
	if len(parts) >= 1 {
		return strings.TrimSpace(parts[0])
	}

	return flow
}
