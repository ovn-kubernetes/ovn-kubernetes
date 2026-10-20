// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"maps"
	"testing"

	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func expectFRRMetrics(t *testing.T, ra string, configs, ipv4, ipv6 float64) {
	t.Helper()
	metrics.RegisterClusterManagerFunctional()
	for metric, want := range map[string]map[string]float64{
		"route_advertisement_frr_configurations":  {"": configs},
		"route_advertisement_advertised_prefixes": {"ipv4": ipv4, "ipv6": ipv6},
	} {
		name := prometheus.BuildFQName(types.MetricOvnkubeNamespace, types.MetricOvnkubeSubsystemClusterManager, metric)
		got := map[string]float64{}
		for _, sample := range ovntest.FindMetricFamily(name).GetMetric() {
			if ovntest.MetricLabelValue(sample.Label, "name") == ra {
				got[ovntest.MetricLabelValue(sample.Label, "family")] = sample.GetGauge().GetValue()
			}
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s for RA %s: got %v, want %v", metric, ra, got, want)
		}
	}
}

func TestCountAdvertisedPrefixes(t *testing.T) {
	config := func(routers ...frrtypes.Router) *frrtypes.FRRConfiguration {
		c := &frrtypes.FRRConfiguration{}
		c.Spec.BGP.Routers = routers
		return c
	}

	tests := []struct {
		name      string
		generated []*frrtypes.FRRConfiguration
		wantV4    int
		wantV6    int
	}{
		{
			name:      "no generated configurations",
			generated: nil,
		},
		{
			name:      "generated configuration with no routers",
			generated: []*frrtypes.FRRConfiguration{config()},
		},
		{
			name:      "router with no prefixes",
			generated: []*frrtypes.FRRConfiguration{config(frrtypes.Router{ASN: 1})},
		},
		{
			name: "one prefix per node, layer3 style",
			generated: []*frrtypes.FRRConfiguration{
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.244.0.0/24"}}),
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.244.1.0/24"}}),
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.244.2.0/24"}}),
			},
			wantV4: 3,
		},
		{
			name: "same prefix from every node counts once, layer2 anycast style",
			generated: []*frrtypes.FRRConfiguration{
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.210.0.0/16"}}),
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.210.0.0/16"}}),
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.210.0.0/16"}}),
			},
			wantV4: 1,
		},
		{
			name: "ipv6 only",
			generated: []*frrtypes.FRRConfiguration{
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"fd01::/64", "fd02::/64"}}),
			},
			wantV6: 2,
		},
		{
			name: "dual stack is split by family",
			generated: []*frrtypes.FRRConfiguration{
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.244.0.0/24", "fd01::/64"}}),
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"10.244.1.0/24", "fd02::/64"}}),
			},
			wantV4: 2,
			wantV6: 2,
		},
		{
			name: "host routes, as advertised for EgressIPs and EVPN VTEPs",
			generated: []*frrtypes.FRRConfiguration{
				config(frrtypes.Router{ASN: 1, Prefixes: []string{"172.18.0.3/32", "fd00::3/128"}}),
			},
			wantV4: 1,
			wantV6: 1,
		},
		{
			name: "prefixes are counted across all routers of a configuration",
			generated: []*frrtypes.FRRConfiguration{
				config(
					frrtypes.Router{ASN: 1, Prefixes: []string{"172.18.0.3/32"}},
					frrtypes.Router{ASN: 1, VRF: "red", Prefixes: []string{"10.210.0.0/16"}},
				),
			},
			wantV4: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v4, v6 := countAdvertisedPrefixes(tt.generated)
			if v4 != tt.wantV4 || v6 != tt.wantV6 {
				t.Errorf("countAdvertisedPrefixes() = (v4=%d, v6=%d), want (v4=%d, v6=%d)",
					v4, v6, tt.wantV4, tt.wantV6)
			}
		})
	}
}
