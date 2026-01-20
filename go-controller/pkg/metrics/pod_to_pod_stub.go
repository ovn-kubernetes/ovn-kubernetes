//go:build !linux
// +build !linux

package metrics

// RESEARCH: Stub implementations for non-Linux platforms
// These functions do nothing on non-Linux platforms where OVS is not available

// RegisterPodToPodMetrics is a no-op on non-Linux platforms
func RegisterPodToPodMetrics() {
	// No-op: OVS not available on this platform
}

// EnablePodToPodMetrics is a no-op on non-Linux platforms
func EnablePodToPodMetrics(enabled bool) {
	// No-op: OVS not available on this platform
}

// IsPodToPodMetricsEnabled always returns false on non-Linux platforms
func IsPodToPodMetricsEnabled() bool {
	return false
}

// StartPodToPodMetricsCollector is a no-op on non-Linux platforms
func StartPodToPodMetricsCollector(nodeName string, stopChan <-chan struct{}) {
	// No-op: OVS not available on this platform
}
