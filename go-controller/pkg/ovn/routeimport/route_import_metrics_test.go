// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeimport

import (
	"context"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntesting "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
)

// routeImportValues returns the value of every series of a route import metric
// that carries the network label, keyed by the given secondary label. A
// histogram contributes its sample count.
func routeImportValues(name, network, label string) map[string]float64 {
	values := map[string]float64{}
	family := ovntesting.FindMetricFamily(prometheus.BuildFQName(types.MetricOvnkubeNamespace, types.MetricOvnkubeSubsystemController, name))
	if family == nil {
		return values
	}
	for _, m := range family.GetMetric() {
		if ovntesting.MetricLabelValue(m.GetLabel(), "network") != network {
			continue
		}
		values[ovntesting.MetricLabelValue(m.GetLabel(), label)] = metricValue(m)
	}
	return values
}

func metricValue(m *dto.Metric) float64 {
	switch {
	case m.GetHistogram() != nil:
		return float64(m.GetHistogram().GetSampleCount())
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	default:
		return m.GetGauge().GetValue()
	}
}

func Test_controller_syncNetwork_metrics(t *testing.T) {
	g := gomega.NewWithT(t)
	node := "testnode"
	const network = "udn"

	// registration is process-wide and gated on the same flags ovnkube-node uses
	origScale := config.Metrics.EnableScaleMetrics
	origRouteAdvertisements := config.OVNKubernetesFeature.EnableRouteAdvertisements
	config.Metrics.EnableScaleMetrics = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	t.Cleanup(func() {
		config.Metrics.EnableScaleMetrics = origScale
		config.OVNKubernetesFeature.EnableRouteAdvertisements = origRouteAdvertisements
	})
	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	metrics.RegisterNodeMetrics(stopCh)

	udn := &multinetworkmocks.NetInfo{}
	udn.On("IsDefault").Return(false)
	udn.On("GetNetworkName").Return(network)
	udn.On("GetNetworkID").Return(1)
	udn.On("Subnets").Return(nil)
	udn.On("GetNetworkScopedGWRouterName", node).Return("router")
	udn.On("Transport").Return("")
	udn.On("Uplink").Return("")

	routes := []netlink.Route{
		{Dst: ovntesting.MustParseIPNet("1.1.1.0/24"), Gw: ovntesting.MustParseIP("1.1.1.1")},
	}

	// newController tracks the network with a VRF holding one BGP route and
	// starts every scenario from a clean set of series
	newController := func(t *testing.T, initial []libovsdb.TestData) *controller {
		t.Helper()
		nlmock := &mocks.NetLinkOps{}
		nlmock.On("IsLinkNotFoundError", mock.Anything).Return(false)
		nlmock.On("LinkByName", util.GetNetworkVRFName(udn)).Return(&netlink.Vrf{Table: 1000}, nil)
		nlmock.On("RouteListFilteredIter", netlink.FAMILY_ALL, mock.Anything, netlink.RT_FILTER_PROTOCOL|netlink.RT_FILTER_TABLE, mock.Anything).
			Run(func(args mock.Arguments) {
				for _, route := range routes {
					if !args.Get(3).(func(netlink.Route) bool)(route) {
						break
					}
				}
			}).Return(nil)

		client, ctx, err := libovsdb.NewNBTestHarness(libovsdb.TestSetup{NBData: initial}, nil)
		g.Expect(err).ToNot(gomega.HaveOccurred())
		t.Cleanup(ctx.Cleanup)
		t.Cleanup(func() { metrics.DeleteRouteImportMetrics(network) })

		return &controller{
			nbClient:   client,
			node:       node,
			log:        testr.New(t),
			networkIDs: map[int]string{1: network},
			networks:   map[string]util.NetInfo{network: udn},
			tables:     map[int]int{},
			netlink:    nlmock,
		}
	}
	router := []libovsdb.TestData{&nbdb.LogicalRouter{Name: "router"}}

	t.Run("a successful sync records its duration, both route counts and the added routes", func(t *testing.T) {
		g := gomega.NewWithT(t)
		c := newController(t, router)

		g.Expect(c.syncNetwork(context.Background(), network)).To(gomega.Succeed())

		g.Expect(routeImportValues("route_import_sync_duration_seconds", network, "result")).To(gomega.Equal(map[string]float64{"success": 1}))
		g.Expect(routeImportValues("route_import_routes", network, "source")).To(gomega.Equal(map[string]float64{
			metrics.RouteSourceBGP: 1,
			metrics.RouteSourceOVN: 1,
		}), "after the transaction OVN holds what the kernel holds")
		g.Expect(routeImportValues("route_import_operations_total", network, "op")).To(gomega.Equal(map[string]float64{"add": 1}))
	})

	t.Run("a sync that cannot read the gateway router records an error and no route counts", func(t *testing.T) {
		g := gomega.NewWithT(t)
		c := newController(t, nil)

		g.Expect(c.syncNetwork(context.Background(), network)).ToNot(gomega.Succeed(), "the sync must fail without the gateway router")

		g.Expect(routeImportValues("route_import_sync_duration_seconds", network, "result")).To(gomega.Equal(map[string]float64{"error": 1}))
		g.Expect(routeImportValues("route_import_routes", network, "source")).To(gomega.BeEmpty(), "the diff was never computed")
		g.Expect(routeImportValues("route_import_operations_total", network, "op")).To(gomega.BeEmpty(), "nothing was written")
	})

	t.Run("forgetting a network removes every series it had", func(t *testing.T) {
		g := gomega.NewWithT(t)
		c := newController(t, router)
		g.Expect(c.syncNetwork(context.Background(), network)).To(gomega.Succeed())
		g.Expect(routeImportValues("route_import_sync_duration_seconds", network, "result")).ToNot(gomega.BeEmpty(), "the sync must have recorded series before the network is forgotten")

		c.ForgetNetwork(network)

		for _, name := range []string{"route_import_sync_duration_seconds", "route_import_routes", "route_import_operations_total"} {
			g.Expect(routeImportValues(name, network, "")).To(gomega.BeEmpty(), "%s must not keep series for a forgotten network", name)
		}
	})

	t.Run("a reconcile queued before the network was forgotten records nothing", func(t *testing.T) {
		g := gomega.NewWithT(t)
		c := newController(t, router)
		c.ForgetNetwork(network)

		g.Expect(c.syncNetwork(context.Background(), network)).To(gomega.Succeed(), "a forgotten network is skipped, not an error")

		g.Expect(routeImportValues("route_import_sync_duration_seconds", network, "result")).To(gomega.BeEmpty(), "observing the duration would recreate the series")
	})
}
