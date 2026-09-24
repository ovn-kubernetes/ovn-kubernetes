// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"fmt"
	"testing"

	"github.com/onsi/gomega"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestCreateLogicalRouterStaticRoutesBatch(t *testing.T) {
	g := gomega.NewWithT(t)
	outport, otherOutport := "rtoe-router", "rtoe-other"
	client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
		&nbdb.LogicalRouter{UUID: "router", Name: "router", StaticRoutes: []string{"foreign"}},
		&nbdb.LogicalRouterStaticRoute{UUID: "foreign", IPPrefix: "192.0.2.0/24", Nexthop: "192.0.2.1", ExternalIDs: map[string]string{"owner": "other"}},
		&nbdb.LogicalRouter{UUID: "other", Name: "other", StaticRoutes: []string{"other-route"}},
		&nbdb.LogicalRouterStaticRoute{UUID: "other-route", IPPrefix: "192.0.2.0/24", Nexthop: "192.0.2.1", OutputPort: &otherOutport, ExternalIDs: map[string]string{"owner": "import"}},
	}}, nil)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(cleanup.Cleanup)
	newRoutes := []*nbdb.LogicalRouterStaticRoute{
		{IPPrefix: "192.0.2.0/24", Nexthop: "192.0.2.1", OutputPort: &outport, ExternalIDs: map[string]string{"owner": "import"}},
		{IPPrefix: "2001:db8::/64", Nexthop: "fe80::1", OutputPort: &outport, ExternalIDs: map[string]string{"owner": "import"}},
	}
	ops, err := CreateLogicalRouterStaticRoutesOps(client, nil, "router", newRoutes...)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ops).To(gomega.HaveLen(3), "two route inserts and one router mutation")
	mutations := 0
	for _, op := range ops {
		if op.Op == ovsdb.OperationMutate && op.Table == nbdb.LogicalRouterTable {
			mutations++
		}
	}
	g.Expect(mutations).To(gomega.Equal(1))
	_, err = TransactAndCheck(client, ops)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(client).Should(libovsdbtest.HaveData(
		&nbdb.LogicalRouter{UUID: "router", Name: "router", StaticRoutes: []string{"foreign", newRoutes[0].UUID, newRoutes[1].UUID}},
		&nbdb.LogicalRouterStaticRoute{UUID: "foreign", IPPrefix: "192.0.2.0/24", Nexthop: "192.0.2.1", ExternalIDs: map[string]string{"owner": "other"}},
		&nbdb.LogicalRouter{UUID: "other", Name: "other", StaticRoutes: []string{"other-route"}},
		&nbdb.LogicalRouterStaticRoute{UUID: "other-route", IPPrefix: "192.0.2.0/24", Nexthop: "192.0.2.1", OutputPort: &otherOutport, ExternalIDs: map[string]string{"owner": "import"}},
		newRoutes[0], newRoutes[1],
	))
}

func BenchmarkStaticRouteAddBatch(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprintf("existing=%d", count), func(b *testing.B) {
			data := make([]libovsdbtest.TestData, 0, count+1)
			router := &nbdb.LogicalRouter{UUID: "router", Name: "router"}
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("route-%d", i)
				router.StaticRoutes = append(router.StaticRoutes, id)
				data = append(data, &nbdb.LogicalRouterStaticRoute{UUID: id, IPPrefix: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256), Nexthop: "192.0.2.1"})
			}
			data = append(data, router)
			client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(cleanup.Cleanup)
			for _, batch := range []bool{false, true} {
				b.Run(fmt.Sprintf("batch=%v", batch), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						routes := make([]*nbdb.LogicalRouterStaticRoute, 50)
						for i := range routes {
							routes[i] = &nbdb.LogicalRouterStaticRoute{IPPrefix: fmt.Sprintf("172.16.%d.0/24", i), Nexthop: "192.0.2.1"}
						}
						var ops []ovsdb.Operation
						if batch {
							ops, err = CreateLogicalRouterStaticRoutesOps(client, nil, "router", routes...)
						} else {
							for _, route := range routes {
								ops, err = CreateOrReplaceLogicalRouterStaticRouteWithPredicateOps(client, ops, "router", route, func(r *nbdb.LogicalRouterStaticRoute) bool {
									return r.IPPrefix == route.IPPrefix && r.Nexthop == route.Nexthop
								})
								if err != nil {
									b.Fatal(err)
								}
							}
						}
						if err != nil || len(ops) == 0 {
							b.Fatalf("ops=%d err=%v", len(ops), err)
						}
					}
				})
			}
		})
	}
}
