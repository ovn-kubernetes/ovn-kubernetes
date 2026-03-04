//go:build linux
// +build linux

package nftables

import (
	"testing"

	"sigs.k8s.io/knftables"
)

func TestUpdateNFTElements(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial string
		elems   []*knftables.Element
		final   string
	}{
		{
			name:    "empty transaction",
			initial: "",
			elems:   []*knftables.Element{},
			final:   "add table inet ovn-kubernetes",
		},
		{
			name: "add to empty set",
			initial: `
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
			`,
			elems: []*knftables.Element{
				{
					Set: "testset",
					Key: []string{"1.2.3.4"},
				},
				{
					Set: "testset",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
				add element inet ovn-kubernetes testset { 5.6.7.8 }
			`,
		},
		{
			name: "re-add existing object",
			initial: `
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
			`,
			elems: []*knftables.Element{
				{
					Set: "testset",
					Key: []string{"1.2.3.4"},
				},
				{
					Set: "testset",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
				add element inet ovn-kubernetes testset { 5.6.7.8 }
			`,
		},
		{
			name: "add map elements, multiple containers",
			initial: `
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add map inet ovn-kubernetes testmap { type ipv4_addr : ipv4_addr ; }
			`,
			elems: []*knftables.Element{
				{
					Set: "testset",
					Key: []string{"1.2.3.4"},
				},
				{
					Map:   "testmap",
					Key:   []string{"10.0.0.1"},
					Value: []string{"9.9.9.9"},
				},
				{
					Set: "testset",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add map inet ovn-kubernetes testmap { type ipv4_addr : ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
				add element inet ovn-kubernetes testset { 5.6.7.8 }
				add element inet ovn-kubernetes testmap { 10.0.0.1 : 9.9.9.9 }
			`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := SetFakeNFTablesHelper()
			err := fake.ParseDump(tc.initial)
			if err != nil {
				t.Fatalf("unexpected error parsing initial state: %v", err)
			}
			err = UpdateNFTElements(tc.elems)
			if err != nil {
				t.Fatalf("unexpected error updating elements: %v", err)
			}
			err = MatchNFTRules(tc.final, fake.Dump())
			if err != nil {
				t.Fatalf("unexpected final result: %v", err)
			}
		})
	}
}

func TestDeleteNFTElements(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial string
		elems   []*knftables.Element
		final   string
	}{
		{
			name:    "empty transaction",
			initial: "",
			elems:   []*knftables.Element{},
			final:   "add table inet ovn-kubernetes",
		},
		{
			name: "delete existing objects",
			initial: `
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
				add element inet ovn-kubernetes testset { 5.6.7.8 }
			`,
			elems: []*knftables.Element{
				{
					Set: "testset",
					Key: []string{"1.2.3.4"},
				},
				{
					Set: "testset",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
			`,
		},
		{
			name: "delete non-existing object",
			initial: `
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
			`,
			elems: []*knftables.Element{
				{
					Set: "testset",
					Key: []string{"1.2.3.4"},
				},
				{
					Set: "testset",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
			`,
		},
		{
			name: "delete map elements, multiple containers",
			initial: `
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add map inet ovn-kubernetes testmap { type ipv4_addr : ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
				add element inet ovn-kubernetes testset { 5.6.7.8 }
				add element inet ovn-kubernetes testmap { 10.0.0.1 : 9.9.9.9 }
			`,
			elems: []*knftables.Element{
				{
					Set: "testset",
					Key: []string{"1.2.3.4"},
				},
				{
					Map:   "testmap",
					Key:   []string{"10.0.0.1"},
					Value: []string{"9.9.9.9"},
				},
				{
					Set: "testset",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add map inet ovn-kubernetes testmap { type ipv4_addr : ipv4_addr ; }
			`,
		},
		{
			name: "delete map elements without values",
			initial: `
				add map inet ovn-kubernetes testmap { type ipv4_addr : ipv4_addr ; }
				add element inet ovn-kubernetes testmap { 10.0.0.1 : 9.9.9.9 }
				add element inet ovn-kubernetes testmap { 10.0.0.2 : 8.8.8.8 }
				add element inet ovn-kubernetes testmap { 10.0.0.3 : 7.7.7.7 }
				add element inet ovn-kubernetes testmap { 10.0.0.4 : 6.6.6.6 }
			`,
			elems: []*knftables.Element{
				{
					Map: "testmap",
					Key: []string{"10.0.0.1"},
				},
				{
					Map: "testmap",
					Key: []string{"10.0.0.3"},
				},
				{
					Map: "testmap",
					Key: []string{"10.0.0.5"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add map inet ovn-kubernetes testmap { type ipv4_addr : ipv4_addr ; }
				add element inet ovn-kubernetes testmap { 10.0.0.2 : 8.8.8.8 }
				add element inet ovn-kubernetes testmap { 10.0.0.4 : 6.6.6.6 }
			`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := SetFakeNFTablesHelper()
			err := fake.ParseDump(tc.initial)
			if err != nil {
				t.Fatalf("unexpected error parsing initial state: %v", err)
			}
			err = DeleteNFTElements(tc.elems)
			if err != nil {
				t.Fatalf("unexpected error deleting objects: %v", err)
			}
			err = MatchNFTRules(tc.final, fake.Dump())
			if err != nil {
				t.Fatalf("unexpected final result: %v", err)
			}
		})
	}
}
