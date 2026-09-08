// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/google/uuid"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestGetRouterLogicalRouterStaticRoutesWithPredicate(t *testing.T) {
	route1 := &nbdb.LogicalRouterStaticRoute{
		UUID:        buildNamedUUID(),
		IPPrefix:    "10.0.0.0/24",
		Nexthop:     "100.64.0.2",
		ExternalIDs: map[string]string{"owner": "node1"},
	}
	route2 := &nbdb.LogicalRouterStaticRoute{
		UUID:        buildNamedUUID(),
		IPPrefix:    "10.0.1.0/24",
		Nexthop:     "100.64.0.3",
		ExternalIDs: map[string]string{"owner": "node1"},
	}
	nonMatchingRoute := &nbdb.LogicalRouterStaticRoute{
		UUID:        buildNamedUUID(),
		IPPrefix:    "10.0.2.0/24",
		Nexthop:     "100.64.0.4",
		ExternalIDs: map[string]string{"owner": "node2"},
	}
	otherRouterRoute := &nbdb.LogicalRouterStaticRoute{
		UUID:        buildNamedUUID(),
		IPPrefix:    "10.0.3.0/24",
		Nexthop:     "100.64.0.5",
		ExternalIDs: map[string]string{"owner": "node1"},
	}
	router := &nbdb.LogicalRouter{
		UUID:         buildNamedUUID(),
		Name:         "router1",
		StaticRoutes: []string{route2.UUID, nonMatchingRoute.UUID, route1.UUID},
	}
	otherRouter := &nbdb.LogicalRouter{
		UUID:         buildNamedUUID(),
		Name:         "router2",
		StaticRoutes: []string{otherRouterRoute.UUID},
	}

	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
		route1,
		route2,
		nonMatchingRoute,
		otherRouterRoute,
		router,
		otherRouter,
	}}, nil)
	if err != nil {
		t.Fatalf("failed to set up test harness: %v", err)
	}
	t.Cleanup(cleanup.Cleanup)

	tests := []struct {
		name         string
		predicate    logicalRouterStaticRoutePredicate
		wantPrefixes []string
		verifyClone  bool
	}{
		{
			name: "multiple matches preserve router order",
			predicate: func(route *nbdb.LogicalRouterStaticRoute) bool {
				return route.ExternalIDs["owner"] == "node1"
			},
			wantPrefixes: []string{route2.IPPrefix, route1.IPPrefix},
		},
		{
			name: "single match",
			predicate: func(route *nbdb.LogicalRouterStaticRoute) bool {
				return route.IPPrefix == route1.IPPrefix
			},
			wantPrefixes: []string{route1.IPPrefix},
			verifyClone:  true,
		},
		{
			name: "no match",
			predicate: func(*nbdb.LogicalRouterStaticRoute) bool {
				return false
			},
			wantPrefixes: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			predicateCalls := 0
			predicate := func(route *nbdb.LogicalRouterStaticRoute) bool {
				predicateCalls++
				return tt.predicate(route)
			}
			routes, err := GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, &nbdb.LogicalRouter{Name: router.Name}, predicate)
			if err != nil {
				t.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() error = %v", err)
			}
			if routes == nil {
				t.Fatal("GetRouterLogicalRouterStaticRoutesWithPredicate() returned a nil slice")
			}
			if predicateCalls != len(router.StaticRoutes) {
				t.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() predicate calls = %d, want %d", predicateCalls, len(router.StaticRoutes))
			}

			gotPrefixes := make([]string, 0, len(routes))
			for _, route := range routes {
				gotPrefixes = append(gotPrefixes, route.IPPrefix)
			}
			if !reflect.DeepEqual(gotPrefixes, tt.wantPrefixes) {
				t.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() prefixes = %v, want %v", gotPrefixes, tt.wantPrefixes)
			}

			if tt.verifyClone {
				routes[0].ExternalIDs["mutated"] = "true"
				freshRoutes, err := GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, &nbdb.LogicalRouter{Name: router.Name}, tt.predicate)
				if err != nil {
					t.Fatalf("second GetRouterLogicalRouterStaticRoutesWithPredicate() error = %v", err)
				}
				if _, found := freshRoutes[0].ExternalIDs["mutated"]; found {
					t.Fatal("mutating a returned route changed the cached route")
				}
			}
		})
	}
}

func TestGetRouterLogicalRouterStaticRoutesWithPredicateEmptyRouter(t *testing.T) {
	unrelatedRoute := &nbdb.LogicalRouterStaticRoute{
		UUID:     buildNamedUUID(),
		IPPrefix: "10.0.0.0/24",
		Nexthop:  "100.64.0.2",
	}
	router := &nbdb.LogicalRouter{
		UUID: buildNamedUUID(),
		Name: "empty-router",
	}
	otherRouter := &nbdb.LogicalRouter{
		UUID:         buildNamedUUID(),
		Name:         "other-router",
		StaticRoutes: []string{unrelatedRoute.UUID},
	}

	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
		unrelatedRoute,
		router,
		otherRouter,
	}}, nil)
	if err != nil {
		t.Fatalf("failed to set up test harness: %v", err)
	}
	t.Cleanup(cleanup.Cleanup)

	predicateCalls := 0
	routes, err := GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, &nbdb.LogicalRouter{Name: router.Name}, func(*nbdb.LogicalRouterStaticRoute) bool {
		predicateCalls++
		return true
	})
	if err != nil {
		t.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() error = %v", err)
	}
	if len(routes) != 0 {
		t.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() matches = %d, want 0", len(routes))
	}
	if routes == nil {
		t.Fatal("GetRouterLogicalRouterStaticRoutesWithPredicate() returned a nil slice, want an empty slice")
	}
	if predicateCalls != 0 {
		t.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() predicate calls = %d, want 0", predicateCalls)
	}
}

func TestGetRouterLogicalRouterStaticRoutesWithPredicateReturnsErrorForMissingReferences(t *testing.T) {
	route := &nbdb.LogicalRouterStaticRoute{
		UUID:     buildNamedUUID(),
		IPPrefix: "10.0.0.0/24",
		Nexthop:  "100.64.0.2",
	}
	router := &nbdb.LogicalRouter{
		UUID:         buildNamedUUID(),
		Name:         "router-with-missing-route",
		StaticRoutes: []string{route.UUID},
	}

	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{route, router},
	}, nil)
	if err != nil {
		t.Fatalf("failed to set up test harness: %v", err)
	}
	t.Cleanup(cleanup.Cleanup)

	// Strong references prevent this state in a healthy NBDB. Inject it only in
	// the client cache to verify that the batched lookup preserves the previous
	// per-UUID lookup's error behavior for a stale reference.
	cachedRouter, err := GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: router.Name})
	if err != nil {
		t.Fatalf("failed to get router: %v", err)
	}
	cachedRouter.StaticRoutes = append([]string{uuid.NewString()}, cachedRouter.StaticRoutes...)
	if _, err = nbClient.Cache().Table(nbdb.LogicalRouterTable).Update(cachedRouter.UUID, cachedRouter, false); err != nil {
		t.Fatalf("failed to inject missing route reference: %v", err)
	}

	predicateCalls := 0
	_, err = GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, &nbdb.LogicalRouter{Name: router.Name}, func(*nbdb.LogicalRouterStaticRoute) bool {
		predicateCalls++
		return true
	})
	if !errors.Is(err, libovsdbclient.ErrNotFound) {
		t.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() error = %v, want %v", err, libovsdbclient.ErrNotFound)
	}
	if predicateCalls != 1 {
		t.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() predicate calls = %d, want 1", predicateCalls)
	}
}

func BenchmarkGetRouterLogicalRouterStaticRoutesWithPredicate(b *testing.B) {
	const (
		routerRouteCount      = 512
		largeRouterRouteCount = 4096
		unrelatedRouteCount   = 4096
	)

	tests := []struct {
		name            string
		routerRoutes    int
		unrelatedRoutes int
		matchAll        bool
		matchNone       bool
		wantMatches     int
	}{
		{
			name:         "selective/R=512/U=0/K=1",
			routerRoutes: routerRouteCount,
			wantMatches:  1,
		},
		{
			name:         "selective/R=4096/U=0/K=1",
			routerRoutes: largeRouterRouteCount,
			wantMatches:  1,
		},
		{
			name:         "missing/R=512/U=0/K=0",
			routerRoutes: routerRouteCount,
			matchNone:    true,
		},
		{
			name:         "broad/R=512/U=0/K=512",
			routerRoutes: routerRouteCount,
			matchAll:     true,
			wantMatches:  routerRouteCount,
		},
		{
			name:            "selective/R=512/U=4096/K=1",
			routerRoutes:    routerRouteCount,
			unrelatedRoutes: unrelatedRouteCount,
			wantMatches:     1,
		},
		{
			name:            "empty/R=0/U=4096/K=0",
			unrelatedRoutes: unrelatedRouteCount,
		},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			data := make([]libovsdbtest.TestData, 0, tt.routerRoutes+tt.unrelatedRoutes+2)
			router := &nbdb.LogicalRouter{
				UUID: buildNamedUUID(),
				Name: "benchmark-router",
			}
			otherRouter := &nbdb.LogicalRouter{
				UUID: buildNamedUUID(),
				Name: "other-router",
			}

			targetPrefix := ""
			for i := 0; i < tt.routerRoutes; i++ {
				prefix := fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)
				route := &nbdb.LogicalRouterStaticRoute{
					UUID:     buildNamedUUID(),
					IPPrefix: prefix,
					Nexthop:  "100.64.0.2",
					ExternalIDs: map[string]string{
						"owner": "benchmark-router",
						"route": fmt.Sprintf("route-%d", i),
					},
				}
				data = append(data, route)
				router.StaticRoutes = append(router.StaticRoutes, route.UUID)
				targetPrefix = prefix
			}
			for i := 0; i < tt.unrelatedRoutes; i++ {
				prefix := fmt.Sprintf("172.%d.%d.0/24", i/256, i%256)
				route := &nbdb.LogicalRouterStaticRoute{
					UUID:     buildNamedUUID(),
					IPPrefix: prefix,
					Nexthop:  "100.64.0.3",
					ExternalIDs: map[string]string{
						"owner": "other-router",
						"route": fmt.Sprintf("route-%d", i),
					},
				}
				data = append(data, route)
				otherRouter.StaticRoutes = append(otherRouter.StaticRoutes, route.UUID)
			}
			data = append(data, router)
			if tt.unrelatedRoutes > 0 {
				data = append(data, otherRouter)
			}

			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
			if err != nil {
				b.Fatalf("failed to set up test harness: %v", err)
			}
			b.Cleanup(cleanup.Cleanup)

			predicate := func(route *nbdb.LogicalRouterStaticRoute) bool {
				return tt.matchAll || (!tt.matchNone && route.IPPrefix == targetPrefix)
			}
			routes, err := GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, &nbdb.LogicalRouter{Name: router.Name}, predicate)
			if err != nil {
				b.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() preflight error = %v", err)
			}
			if len(routes) != tt.wantMatches {
				b.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() preflight matches = %d, want %d", len(routes), tt.wantMatches)
			}
			if tt.wantMatches == 1 && routes[0].IPPrefix != targetPrefix {
				b.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() preflight prefix = %q, want %q", routes[0].IPPrefix, targetPrefix)
			}

			b.ReportAllocs()
			for b.Loop() {
				routes, err = GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, &nbdb.LogicalRouter{Name: router.Name}, predicate)
				if err != nil {
					b.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() error = %v", err)
				}
			}
			if len(routes) != tt.wantMatches {
				b.Fatalf("GetRouterLogicalRouterStaticRoutesWithPredicate() matches = %d, want %d", len(routes), tt.wantMatches)
			}
		})
	}
}

func TestFindNATsUsingPredicate(t *testing.T) {
	fakeNAT1 := &nbdb.NAT{
		UUID: buildNamedUUID(),
		Type: nbdb.NATTypeSNAT,
	}

	fakeNAT2 := &nbdb.NAT{
		UUID:        buildNamedUUID(),
		ExternalIDs: map[string]string{"name": "fakeNAT2"},
	}

	initialNbdb := libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			&nbdb.LogicalRouter{
				Nat: []string{fakeNAT1.UUID, fakeNAT2.UUID},
			},
			fakeNAT1,
			fakeNAT2,
		},
	}

	tests := []struct {
		desc       string
		predFunc   func(item *nbdb.NAT) bool
		expectedRc []*nbdb.NAT
	}{
		{
			desc: "find no nats",
			predFunc: func(*nbdb.NAT) bool {
				return false
			},
			expectedRc: []*nbdb.NAT{},
		},
		{
			desc: "find all nats",
			predFunc: func(*nbdb.NAT) bool {
				return true
			},
			expectedRc: []*nbdb.NAT{fakeNAT1, fakeNAT2},
		},
		{
			desc: "find nat2",
			predFunc: func(item *nbdb.NAT) bool {
				name := item.ExternalIDs["name"]
				return name == "fakeNAT2"
			},
			expectedRc: []*nbdb.NAT{fakeNAT2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			rc, err := FindNATsWithPredicate(nbClient, tt.predFunc)
			if err != nil {
				t.Fatal(fmt.Errorf("FindNATsUsingPredicate() error = %v", err))
			}

			if len(rc) != len(tt.expectedRc) {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match len expected %v with actual: %v", tt.desc, tt.expectedRc, rc))
			}

			var foundMatch bool
			for _, nat := range tt.expectedRc {
				foundMatch = false
				for _, rcNat := range rc {
					if isEquivalentNAT(rcNat, nat) {
						foundMatch = true
						break
					}
				}
				if !foundMatch {
					t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected nat %v", tt.desc, nat))

				}
			}
		})
	}
}

func TestBuildSNATWithAllowedExtIPs(t *testing.T) {
	externalIP := net.ParseIP("169.254.0.12")
	_, logicalIP, err := net.ParseCIDR("100.128.0.0/24")
	if err != nil {
		t.Fatalf("failed to parse logical IP: %v", err)
	}

	nat := BuildSNATWithAllowedExtIPs(
		&externalIP,
		logicalIP,
		"rtos-blue-node",
		map[string]string{"network": "blue"},
		"",
		"allowed-ext-ips-uuid",
	)

	if nat.AllowedExtIPs == nil || *nat.AllowedExtIPs != "allowed-ext-ips-uuid" {
		t.Fatalf("expected allowed_ext_ips to be set, got %v", nat.AllowedExtIPs)
	}
	if nat.ExemptedExtIPs != nil {
		t.Fatalf("expected exempted_ext_ips to be nil, got %v", nat.ExemptedExtIPs)
	}
}

func TestEquivalentNATChecksAllowedExtIPsButNotExemptedExtIPs(t *testing.T) {
	externalIP := net.ParseIP("169.254.0.12")
	_, logicalIP, err := net.ParseCIDR("100.128.0.0/24")
	if err != nil {
		t.Fatalf("failed to parse logical IP: %v", err)
	}
	externalIDs := map[string]string{"network": "blue"}

	existing := BuildSNATWithAllowedExtIPs(&externalIP, logicalIP, "rtos-blue-node", externalIDs, "", "node-ip-as-uuid")
	searched := BuildSNATWithAllowedExtIPs(&externalIP, logicalIP, "rtos-blue-node", externalIDs, "", "svc-ip-as-uuid")
	if isEquivalentNAT(existing, searched) {
		t.Fatal("expected SNATs with different allowed_ext_ips to be distinct")
	}

	broadSearch := BuildSNATWithMatch(&externalIP, logicalIP, "rtos-blue-node", externalIDs, "")
	if !isEquivalentNAT(existing, broadSearch) {
		t.Fatal("expected broad SNAT search without allowed_ext_ips to still match")
	}

	existing = BuildSNATWithExemptedExtIPs(&externalIP, logicalIP, "rtos-blue-node", externalIDs, "", "old-exempted-ext-ips-uuid")
	searched = BuildSNATWithExemptedExtIPs(&externalIP, logicalIP, "rtos-blue-node", externalIDs, "", "new-exempted-ext-ips-uuid")
	if !isEquivalentNAT(existing, searched) {
		t.Fatal("expected SNATs with different exempted_ext_ips to match for update")
	}
}

func TestDeleteNATsFromRouter(t *testing.T) {
	fakeNAT1 := &nbdb.NAT{
		UUID:       buildNamedUUID(),
		ExternalIP: "192.168.1.110",
		Type:       nbdb.NATTypeSNAT,
	}

	fakeNAT2 := &nbdb.NAT{
		UUID:       buildNamedUUID(),
		ExternalIP: "192.168.1.110",
		Type:       nbdb.NATTypeDNATAndSNAT,
	}

	fakeNAT3 := &nbdb.NAT{
		UUID:        buildNamedUUID(),
		ExternalIP:  "192.168.1.111",
		Type:        nbdb.NATTypeSNAT,
		ExternalIDs: map[string]string{"name": "fakeNAT3"},
	}

	fakeNAT4 := &nbdb.NAT{
		UUID:        buildNamedUUID(),
		ExternalIP:  "192.168.1.112",
		Type:        nbdb.NATTypeSNAT,
		ExternalIDs: map[string]string{"name": "fakeNAT4"},
	}

	fakeRouter1 := &nbdb.LogicalRouter{
		Name: "rtr1",
		UUID: buildNamedUUID(),
		Nat:  []string{fakeNAT1.UUID},
	}

	fakeRouter2 := &nbdb.LogicalRouter{
		Name: "rtr2",
		UUID: buildNamedUUID(),
		Nat:  []string{fakeNAT2.UUID, fakeNAT3.UUID},
	}

	initialNbdb := libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			fakeNAT1,
			fakeNAT2,
			fakeNAT3,
			fakeRouter1,
			fakeRouter2,
		},
	}

	tests := []struct {
		desc         string
		expectErr    bool
		routerName   string
		nats         []*nbdb.NAT
		expectedNbdb libovsdbtest.TestSetup
	}{
		{
			desc:         "no router",
			expectErr:    true,
			nats:         []*nbdb.NAT{fakeNAT1.DeepCopy(), fakeNAT2.DeepCopy(), fakeNAT3.DeepCopy(), fakeNAT4.DeepCopy()},
			expectedNbdb: initialNbdb,
		},
		{
			desc:         "no router -- with a name",
			routerName:   "doesNotExistRouter",
			expectErr:    false,
			nats:         []*nbdb.NAT{fakeNAT1.DeepCopy(), fakeNAT2.DeepCopy(), fakeNAT3.DeepCopy(), fakeNAT4.DeepCopy()},
			expectedNbdb: initialNbdb,
		},
		{
			desc:         "no deletes: no matching nats",
			routerName:   "rtr1",
			nats:         []*nbdb.NAT{fakeNAT2.DeepCopy(), fakeNAT3.DeepCopy(), fakeNAT4.DeepCopy()},
			expectedNbdb: initialNbdb,
		},
		{
			desc:       "remove nat 2 from router 2",
			routerName: "rtr2",
			nats:       []*nbdb.NAT{fakeNAT2.DeepCopy(), fakeNAT4.DeepCopy()},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeNAT1,
					fakeNAT3,
					fakeRouter1,
					&nbdb.LogicalRouter{
						Name: fakeRouter2.Name,
						UUID: fakeRouter2.UUID,
						Nat:  []string{fakeNAT3.UUID},
					},
				},
			},
		},
		{
			desc:       "remove nats from router2",
			routerName: "rtr2",
			nats:       []*nbdb.NAT{fakeNAT1.DeepCopy(), fakeNAT2.DeepCopy(), fakeNAT3.DeepCopy(), fakeNAT4.DeepCopy()},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeNAT1,
					fakeRouter1,
					&nbdb.LogicalRouter{
						Name: fakeRouter2.Name,
						UUID: fakeRouter2.UUID,
						Nat:  []string{},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			logicalRouter := nbdb.LogicalRouter{
				Name: tt.routerName,
			}
			err = DeleteNATs(nbClient, &logicalRouter, tt.nats...)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("DeleteNATsFromRouter() error = %v", err))
			}

			matcher := libovsdbtest.HaveData(tt.expectedNbdb.NBData)
			success, err := matcher.Match(nbClient)

			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(nbClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}

}

func TestDeleteRoutersWithPredicateOps(t *testing.T) {
	fakeRouter1 := nbdb.LogicalRouter{
		Name:        "rtr1",
		UUID:        buildNamedUUID(),
		ExternalIDs: map[string]string{"key": "a"},
	}

	fakeRouter2 := nbdb.LogicalRouter{
		Name:        "rtr2",
		UUID:        buildNamedUUID(),
		ExternalIDs: map[string]string{"key": "a"},
	}

	fakeRouter3 := nbdb.LogicalRouter{
		Name:        "rtr3",
		UUID:        buildNamedUUID(),
		ExternalIDs: map[string]string{"key": "b"},
	}

	tests := []struct {
		desc         string
		expectErr    bool
		initialNbdb  libovsdbtest.TestSetup
		expectedNbdb libovsdbtest.TestSetup
		p            logicalRouterPredicate
	}{
		{
			desc:      "remove routers of specified external_id key",
			expectErr: false,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeRouter1.DeepCopy(),
					fakeRouter2.DeepCopy(),
					fakeRouter3.DeepCopy(),
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeRouter3.DeepCopy(),
				},
			},
			p: func(item *nbdb.LogicalRouter) bool { return item.ExternalIDs["key"] == "a" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(tt.initialNbdb, nil)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			ops, err := DeleteLogicalRoutersWithPredicateOps(nbClient, nil, tt.p)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("DeleteLogicalRoutersWithPredicateOps() error = %v", err))
			}

			_, err = TransactAndCheck(nbClient, ops)
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("TransactAndCheck() error = %v", err))
			}

			matcher := libovsdbtest.HaveData(tt.expectedNbdb.NBData)
			success, err := matcher.Match(nbClient)

			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(nbClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}

func TestDeleteLogicalRouterStaticRoutes(t *testing.T) {
	fakeRouter1LRSR1 := &nbdb.LogicalRouterStaticRoute{
		UUID:        buildNamedUUID(),
		IPPrefix:    "192.168.1.0/24",
		Nexthop:     "192.168.1.0",
		ExternalIDs: map[string]string{"id": "v1"},
	}

	fakeRouter1LRSR2 := &nbdb.LogicalRouterStaticRoute{
		UUID:        buildNamedUUID(),
		IPPrefix:    "192.169.1.0/24",
		Nexthop:     "192.169.1.0",
		ExternalIDs: map[string]string{"id": "v2"},
	}

	fakeRouter2LRSR1 := &nbdb.LogicalRouterStaticRoute{
		UUID:        buildNamedUUID(),
		IPPrefix:    "192.170.1.0/24",
		Nexthop:     "192.170.1.0",
		ExternalIDs: map[string]string{"id": "v1"},
	}

	tests := []struct {
		desc         string
		expectErr    bool
		routerName   string
		lrsrs        []*nbdb.LogicalRouterStaticRoute
		initialNbdb  libovsdbtest.TestSetup
		expectedNbdb libovsdbtest.TestSetup
	}{
		{
			desc: "delete logical router static route with predicate will only delete static route from the specified router",
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeRouter1LRSR1,
					fakeRouter1LRSR2,
					fakeRouter2LRSR1,
					&nbdb.LogicalRouter{
						Name:         "rtr1",
						UUID:         buildNamedUUID(),
						StaticRoutes: []string{fakeRouter1LRSR1.UUID, fakeRouter1LRSR2.UUID},
					},
					&nbdb.LogicalRouter{
						Name:         "rtr2",
						UUID:         buildNamedUUID(),
						StaticRoutes: []string{fakeRouter2LRSR1.UUID},
					},
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					fakeRouter1LRSR2,
					fakeRouter2LRSR1,
					&nbdb.LogicalRouter{
						Name:         "rtr1",
						UUID:         buildNamedUUID(),
						StaticRoutes: []string{fakeRouter1LRSR2.UUID},
					},
					&nbdb.LogicalRouter{
						Name:         "rtr2",
						UUID:         buildNamedUUID(),
						StaticRoutes: []string{fakeRouter2LRSR1.UUID},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(tt.initialNbdb, nil)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			err = DeleteLogicalRouterStaticRoutesWithPredicate(nbClient, "rtr1", func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.ExternalIDs["id"] == "v1"
			})
			if err != nil && !tt.expectErr {
				t.Fatal(fmt.Errorf("DeleteLogicalRouterStaticRoutesWithPredicate() error = %v", err))
			}

			matcher := libovsdbtest.HaveData(tt.expectedNbdb.NBData)
			success, err := matcher.Match(nbClient)

			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(nbClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}
