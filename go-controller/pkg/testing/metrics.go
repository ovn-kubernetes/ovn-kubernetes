// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testing

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// FindMetricFamily returns the MetricFamily with the given name from the
// default Prometheus gatherer. Returns nil if not found.
func FindMetricFamily(name string) *dto.MetricFamily {
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		panic(fmt.Sprintf("failed to gather metrics: %v", err))
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

// MetricLabelValue returns the value of the label with the given name from
// a slice of Prometheus label pairs. Returns empty string if not found.
func MetricLabelValue(labels []*dto.LabelPair, name string) string {
	for _, label := range labels {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
}

// GetConditionMetricValue looks up a condition gauge metric by name,
// condition, and status labels. Returns the gauge value and whether the
// metric was found.
func GetConditionMetricValue(metricName, nameLabel, conditionLabel, statusLabel string) (float64, bool) {
	mf := FindMetricFamily(metricName)
	if mf == nil {
		return 0, false
	}
	for _, metric := range mf.GetMetric() {
		if MetricLabelValue(metric.GetLabel(), "name") == nameLabel &&
			MetricLabelValue(metric.GetLabel(), "condition") == conditionLabel &&
			MetricLabelValue(metric.GetLabel(), "status") == statusLabel {
			if metric.GetGauge() != nil {
				return metric.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}
