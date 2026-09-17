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
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%v", stale), func(t *testing.T) {
			g := gomega.NewWithT(t)
			client, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "router", Name: "router", StaticRoutes: []string{"foreign"}},
				&nbdb.LogicalRouterStaticRoute{UUID: "foreign", IPPrefix: "192.0.2.0/24", Nexthop: "192.0.2.1", ExternalIDs: map[string]string{"owner": "other"}},
				&nbdb.LogicalRouter{UUID: "other", Name: "other"},
			}}, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(cleanup.Cleanup)
			snapshot, err := GetLogicalRouter(client, &nbdb.LogicalRouter{Name: "router"})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			newRoutes := []*nbdb.LogicalRouterStaticRoute{
				{IPPrefix: "192.0.2.0/24", Nexthop: "192.0.2.1", ExternalIDs: map[string]string{"owner": "import"}},
				{IPPrefix: "2001:db8::/64", Nexthop: "fe80::1", ExternalIDs: map[string]string{"owner": "import"}},
			}
			ops, err := CreateLogicalRouterStaticRoutesOps(client, nil, snapshot, newRoutes...)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			mutations := 0
			for _, op := range ops {
				if op.Op == ovsdb.OperationMutate && op.Table == nbdb.LogicalRouterTable {
					mutations++
				}
			}
			g.Expect(mutations).To(gomega.Equal(1))
			g.Expect(snapshot.StaticRoutes).To(gomega.HaveLen(1), "snapshot must remain immutable")
			if stale {
				updated := &nbdb.LogicalRouter{UUID: snapshot.UUID, StaticRoutes: []string{}}
				change, updateErr := client.Where(updated).Update(updated, &updated.StaticRoutes)
				g.Expect(updateErr).NotTo(gomega.HaveOccurred())
				_, updateErr = TransactAndCheck(client, change)
				g.Expect(updateErr).NotTo(gomega.HaveOccurred())
			}
			_, err = TransactAndCheck(client, ops)
			if stale {
				g.Expect(err).To(gomega.HaveOccurred(), "stale snapshot must abort the entire batch")
				g.Eventually(func() int {
					routes, listErr := FindLogicalRouterStaticRoutesWithPredicate(client, func(r *nbdb.LogicalRouterStaticRoute) bool { return r.ExternalIDs["owner"] == "import" })
					g.Expect(listErr).NotTo(gomega.HaveOccurred())
					return len(routes)
				}).Should(gomega.BeZero())
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Eventually(func() int {
				router, getErr := GetLogicalRouter(client, &nbdb.LogicalRouter{Name: "router"})
				g.Expect(getErr).NotTo(gomega.HaveOccurred())
				return len(router.StaticRoutes)
			}).Should(gomega.Equal(3))
			other, err := GetLogicalRouter(client, &nbdb.LogicalRouter{Name: "other"})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(other.StaticRoutes).To(gomega.BeEmpty())
		})
	}
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
			snapshot, err := GetLogicalRouter(client, &nbdb.LogicalRouter{Name: "router"})
			if err != nil {
				b.Fatal(err)
			}
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
							ops, err = CreateLogicalRouterStaticRoutesOps(client, nil, snapshot, routes...)
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
