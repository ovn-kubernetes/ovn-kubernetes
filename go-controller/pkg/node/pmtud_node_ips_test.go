// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package node

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"sigs.k8s.io/knftables"

	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func newPMTUDNodeIPTest(t *testing.T) (*pmtudNodeIPSet, *knftables.Fake) {
	t.Helper()
	nft := nodenft.SetFakeNFTablesHelper()
	tx := nft.NewTransaction()
	tx.Add(&knftables.Set{Name: types.NFTRemoteNodeIPsv4, Type: "ipv4_addr"})
	tx.Add(&knftables.Set{Name: types.NFTRemoteNodeIPsv6, Type: "ipv6_addr"})
	if err := nft.Run(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	return &pmtudNodeIPSet{}, nft
}

func pmtudTestIPs(addresses ...string) []net.IP {
	ips := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		ips = append(ips, net.ParseIP(address))
	}
	return ips
}

func TestPMTUDNodeIPReplay(t *testing.T) {
	for _, addresses := range [][]string{{"192.0.2.1"}, {"2001:db8::1"}, {"192.0.2.1", "2001:db8::1"}} {
		t.Run(fmt.Sprint(addresses), func(t *testing.T) {
			s, nft := newPMTUDNodeIPTest(t)
			ips := pmtudTestIPs(addresses...)
			if err := s.sync(ips); err != nil {
				t.Fatal(err)
			}
			bulk := nft.LastTransaction
			if err := s.add(ips); err != nil {
				t.Fatal(err)
			}
			if nft.LastTransaction != bulk || len(s.initialIPs) != 0 {
				t.Fatal("initial replay should consume the snapshot without another transaction")
			}
			// The optimization is one-shot: a later event must still repair drift.
			tx := nft.NewTransaction()
			for _, set := range nodeIPSets {
				tx.Flush(set)
			}
			if err := nft.Run(context.Background(), tx); err != nil {
				t.Fatal(err)
			}
			if err := s.add(ips); err != nil {
				t.Fatal(err)
			}
			for _, ip := range ips {
				element := pmtudNodeIPElement(ip).(*knftables.Element)
				if nft.Table.Sets[element.Set].FindElement(ip.String()) == nil {
					t.Fatalf("later add did not restore %s", ip)
				}
			}
		})
	}
}

func TestPMTUDNodeIPDeleteBeforeReplay(t *testing.T) {
	s, nft := newPMTUDNodeIPTest(t)
	ips := pmtudTestIPs("192.0.2.1", "2001:db8::1")
	if err := s.sync(ips); err != nil {
		t.Fatal(err)
	}
	if err := s.remove(ips); err != nil {
		t.Fatal(err)
	}
	deleted := nft.LastTransaction
	if err := s.add(ips); err != nil {
		t.Fatal(err)
	}
	if nft.LastTransaction == deleted || len(nft.Table.Sets[types.NFTRemoteNodeIPsv4].Elements) != 1 ||
		len(nft.Table.Sets[types.NFTRemoteNodeIPsv6].Elements) != 1 {
		t.Fatal("recreated node must re-add deleted addresses")
	}
}

func TestPMTUDNodeIPUpdateBeforeReplay(t *testing.T) {
	s, nft := newPMTUDNodeIPTest(t)
	old := pmtudTestIPs("192.0.2.1", "2001:db8::1")
	if err := s.sync(old); err != nil {
		t.Fatal(err)
	}
	if err := s.remove(old[:1]); err != nil {
		t.Fatal(err)
	}
	if err := s.add(pmtudTestIPs("192.0.2.2", "2001:db8::1")); err != nil {
		t.Fatal(err)
	}
	v4 := nft.Table.Sets[types.NFTRemoteNodeIPsv4]
	if v4.FindElement("192.0.2.1") != nil || v4.FindElement("192.0.2.2") == nil {
		t.Fatal("update must replace the obsolete IPv4 address")
	}
	if len(s.initialIPs) != 0 || nft.Table.Sets[types.NFTRemoteNodeIPsv6].FindElement("2001:db8::1") == nil {
		t.Fatal("unchanged IPv6 address must be retained and its replay consumed")
	}
}

func TestPMTUDNodeIPFailedAddRetry(t *testing.T) {
	s, nft := newPMTUDNodeIPTest(t)
	if err := s.sync(pmtudTestIPs("192.0.2.1")); err != nil {
		t.Fatal(err)
	}
	tx := nft.NewTransaction()
	tx.Delete(&knftables.Set{Name: types.NFTRemoteNodeIPsv6})
	if err := nft.Run(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	ips := pmtudTestIPs("192.0.2.1", "2001:db8::1")
	if err := s.add(ips); err == nil {
		t.Fatal("expected an add failure for the missing IPv6 set")
	}
	if !s.initialIPs.Has("192.0.2.1") {
		t.Fatal("failed transaction consumed the pending replay")
	}
	tx = nft.NewTransaction()
	tx.Add(&knftables.Set{Name: types.NFTRemoteNodeIPsv6, Type: "ipv6_addr"})
	if err := nft.Run(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if err := s.add(ips); err != nil {
		t.Fatal(err)
	}
	if len(s.initialIPs) != 0 || nft.Table.Sets[types.NFTRemoteNodeIPsv6].FindElement("2001:db8::1") == nil {
		t.Fatal("retry did not install the new address")
	}
}

func TestPMTUDNodeIPFailedSync(t *testing.T) {
	s, nft := newPMTUDNodeIPTest(t)
	ips := pmtudTestIPs("192.0.2.1")
	if err := s.sync(ips); err != nil {
		t.Fatal(err)
	}
	tx := nft.NewTransaction()
	tx.Delete(&knftables.Set{Name: types.NFTRemoteNodeIPsv6})
	if err := nft.Run(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if err := s.sync(pmtudTestIPs("192.0.2.1", "2001:db8::1")); err == nil {
		t.Fatal("expected bulk sync to fail")
	}
	before := nft.LastTransaction
	if err := s.add(ips); err != nil {
		t.Fatal(err)
	}
	if nft.LastTransaction == before || len(s.initialIPs) != 0 {
		t.Fatal("failed bulk sync must not suppress later writes")
	}
}

func TestPMTUDNodeIPFailedDelete(t *testing.T) {
	s, nft := newPMTUDNodeIPTest(t)
	ips := pmtudTestIPs("192.0.2.1")
	if err := s.sync(ips); err != nil {
		t.Fatal(err)
	}
	tx := nft.NewTransaction()
	tx.Delete(&knftables.Set{Name: types.NFTRemoteNodeIPsv4})
	if err := nft.Run(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if err := s.remove(ips); err == nil {
		t.Fatal("expected delete to fail for the missing IPv4 set")
	}
	tx = nft.NewTransaction()
	tx.Add(&knftables.Set{Name: types.NFTRemoteNodeIPsv4, Type: "ipv4_addr"})
	if err := nft.Run(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if err := s.add(ips); err != nil {
		t.Fatal(err)
	}
	if nft.Table.Sets[types.NFTRemoteNodeIPsv4].FindElement("192.0.2.1") == nil {
		t.Fatal("failed delete must invalidate the initial snapshot")
	}
}

func TestPMTUDNodeIPResync(t *testing.T) {
	s, nft := newPMTUDNodeIPTest(t)
	if err := s.sync(pmtudTestIPs("192.0.2.1")); err != nil {
		t.Fatal(err)
	}
	if err := s.sync(pmtudTestIPs("192.0.2.2")); err != nil {
		t.Fatal(err)
	}
	if s.initialIPs.Has("192.0.2.1") || !s.initialIPs.Has("192.0.2.2") {
		t.Fatal("full resync must replace the snapshot")
	}
	if err := s.add(pmtudTestIPs("192.0.2.1")); err != nil {
		t.Fatal(err)
	}
	if nft.Table.Sets[types.NFTRemoteNodeIPsv4].FindElement("192.0.2.1") == nil {
		t.Fatal("address omitted by resync must not be skipped")
	}
}

func TestPMTUDNodeIPConcurrentReplay(t *testing.T) {
	s, nft := newPMTUDNodeIPTest(t)
	var ips []net.IP
	for i := 0; i < 4000; i++ {
		ips = append(ips, net.IPv4(10, byte(i>>8), byte(i), 1))
	}
	if err := s.sync(ips); err != nil {
		t.Fatal(err)
	}
	bulk := nft.LastTransaction
	var wg sync.WaitGroup
	for worker := 0; worker < 30; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := worker; i < len(ips); i += 30 {
				if err := s.add(ips[i : i+1]); err != nil {
					t.Error(err)
				}
			}
		}(worker)
	}
	wg.Wait()
	if nft.LastTransaction != bulk || len(s.initialIPs) != 0 || len(nft.Table.Sets[types.NFTRemoteNodeIPsv4].Elements) != len(ips) {
		t.Fatal("4000-node replay must retain all IPs without another transaction")
	}
}
