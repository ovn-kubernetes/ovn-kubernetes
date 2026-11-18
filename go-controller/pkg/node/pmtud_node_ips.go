// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package node

import (
	"fmt"
	"net"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/knftables"

	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

var nodeIPSets = []knftables.Object{
	&knftables.Set{Name: types.NFTRemoteNodeIPsv4},
	&knftables.Set{Name: types.NFTRemoteNodeIPsv6},
}

// pmtudNodeIPSet suppresses one duplicate add per address after bulk startup sync.
// The mutex keeps that snapshot consistent with updates and deletes; the nftables
// helpers still serialize their transactions with other nftables users.
type pmtudNodeIPSet struct {
	sync.Mutex
	initialIPs sets.Set[string]
}

func pmtudNodeIPElement(ip net.IP) knftables.Object {
	set := types.NFTRemoteNodeIPsv6
	if ip.To4() != nil {
		set = types.NFTRemoteNodeIPsv4
	}
	return &knftables.Element{Set: set, Key: []string{ip.String()}}
}

func (s *pmtudNodeIPSet) add(ips []net.IP) error {
	s.Lock()
	defer s.Unlock()

	var objects []knftables.Object
	for _, ip := range ips {
		if !s.initialIPs.Has(ip.String()) {
			objects = append(objects, pmtudNodeIPElement(ip))
		}
	}
	if len(objects) > 0 {
		if err := nodenft.AddObjects(objects); err != nil {
			return err
		}
	}
	// Consume only after success. Later events retain the normal write path,
	// rather than trusting a permanent shadow of the kernel's contents.
	for _, ip := range ips {
		s.initialIPs.Delete(ip.String())
	}
	if len(s.initialIPs) == 0 {
		s.initialIPs = nil
	}
	return nil
}

func (s *pmtudNodeIPSet) remove(ips []net.IP) error {
	s.Lock()
	defer s.Unlock()

	var objects []knftables.Object
	for _, ip := range ips {
		// Even a failed delete must not let a subsequent add trust the snapshot.
		s.initialIPs.Delete(ip.String())
		objects = append(objects, pmtudNodeIPElement(ip))
	}
	if len(s.initialIPs) == 0 {
		s.initialIPs = nil
	}
	if len(objects) > 0 {
		if err := nodenft.DeleteObjects(objects); err != nil {
			return fmt.Errorf("unable to remove PMTUD rules for nodes: %w", err)
		}
	}
	return nil
}

func (s *pmtudNodeIPSet) sync(ips []net.IP) error {
	s.Lock()
	defer s.Unlock()

	s.initialIPs = nil
	objects := make([]knftables.Object, 0, len(ips))
	for _, ip := range ips {
		objects = append(objects, pmtudNodeIPElement(ip))
	}
	if err := nodenft.SyncObjects(nodeIPSets, objects); err != nil {
		return err
	}
	s.initialIPs = sets.New[string]()
	for _, ip := range ips {
		s.initialIPs.Insert(ip.String())
	}
	return nil
}
