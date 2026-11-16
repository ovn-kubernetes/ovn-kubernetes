// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"fmt"
	"sync"
	"testing"
)

func TestNodeInfoMapUpdates(t *testing.T) {
	c := &Controller{nodeName: "local"}
	state := &networkState{}
	local := nodeInfo{name: c.nodeName, chassisID: "old"}
	c.syncNodeInfoForNetwork(state, &local)
	before := c.nodeInfoMapForNetwork(state)

	remote := nodeInfo{name: "remote", chassisID: "remote-chassis"}
	if !c.updateNodeInfoForNetwork(state, remote.name, &remote) {
		t.Fatal("remote addition was ignored")
	}
	if len(before) != 1 || state.nodeInfo == nil || state.nodeInfo.chassisID != "old" {
		t.Fatal("remote addition changed an earlier snapshot or local information")
	}
	reader := c.nodeInfoMapForNetwork(state)
	delete(reader, local.name)
	reader[remote.name] = nodeInfo{name: "reader-only"}
	if len(state.nodeInfosByName) != 2 || state.nodeInfosByName[remote.name].chassisID != remote.chassisID {
		t.Fatal("reader map aliases controller state")
	}
	if c.updateNodeInfoForNetwork(state, remote.name, &remote) || c.updateNodeInfoForNetwork(state, "missing", nil) {
		t.Fatal("unchanged updates must be ignored")
	}
	local.chassisID = "new"
	if !c.updateNodeInfoForNetwork(state, local.name, &local) || state.nodeInfo.chassisID != "new" {
		t.Fatal("local information was not refreshed")
	}
	if before[local.name].chassisID != "old" {
		t.Fatal("local update changed an earlier reader snapshot")
	}
	if !c.updateNodeInfoForNetwork(state, remote.name, nil) || len(state.nodeInfosByName) != 1 {
		t.Fatal("remote deletion did not preserve the local node")
	}
	if !c.updateNodeInfoForNetwork(state, local.name, nil) || state.nodeInfo != nil {
		t.Fatal("local deletion did not clear local information")
	}
	c.syncNodeInfoForNetwork(state, &remote)
	c.syncNodeInfoForNetwork(state, &local)
	if len(state.nodeInfosByName) != 1 || state.nodeInfo == nil || state.nodeInfo.name != local.name {
		t.Fatal("reset retained remote node information")
	}
	c.syncNodeInfoForNetwork(state, nil)
	if state.nodeInfo != nil || len(state.nodeInfosByName) != 0 {
		t.Fatal("empty reset retained node information")
	}
}

func TestNodeInfoMapConcurrentReadersAndWriters(t *testing.T) {
	c := &Controller{nodeName: "local"}
	state := &networkState{}
	local := nodeInfo{name: c.nodeName}
	c.syncNodeInfoForNetwork(state, &local)
	otherNetwork := &networkState{}
	c.syncNodeInfoForNetwork(otherNetwork, &local)
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Go(func() {
			for i := range 25 {
				info := nodeInfo{name: fmt.Sprintf("remote-%d-%d", worker, i)}
				c.updateNodeInfoForNetwork(state, info.name, &info)
				snapshot := c.nodeInfoMapForNetwork(state)
				delete(snapshot, local.name)
				snapshot["reader-only"] = nodeInfo{}
			}
		})
	}
	workers.Wait()
	if len(state.nodeInfosByName) != 101 || state.nodeInfo == nil || state.nodeInfo.name != local.name {
		t.Fatal("concurrent updates lost nodes or changed local information")
	}
	for worker := range 4 {
		for i := range 25 {
			name := fmt.Sprintf("remote-%d-%d", worker, i)
			if info, ok := state.nodeInfosByName[name]; !ok || info.name != name {
				t.Fatalf("missing node %s", name)
			}
		}
	}
	if len(otherNetwork.nodeInfosByName) != 1 {
		t.Fatal("updates affected a different network")
	}
}

func BenchmarkNodeInfoMapUpdate(b *testing.B) {
	for _, count := range []int{100, 1000, 4000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			c := &Controller{nodeName: "node-0"}
			state := &networkState{nodeInfosByName: make(map[string]nodeInfo, count)}
			for i := range count {
				name := fmt.Sprintf("node-%d", i)
				state.nodeInfosByName[name] = nodeInfo{name: name}
			}
			next := nodeInfo{name: c.nodeName}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if next.chassisID == "old" {
					next.chassisID = "new"
				} else {
					next.chassisID = "old"
				}
				if !c.updateNodeInfoForNetwork(state, next.name, &next) {
					b.Fatal("node update was ignored")
				}
			}
		})
	}
}
