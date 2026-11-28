// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"fmt"
	"sync"
	"testing"
)

func TestNodeInfoMapSnapshots(t *testing.T) {
	c := &Controller{nodeName: "local"}
	local := nodeInfo{name: "local", chassisID: "old"}
	remote := nodeInfo{name: "remote", chassisID: "remote-chassis"}
	state := &networkState{}
	c.syncNodeInfoForNetwork(state, &local)

	original := c.nodeInfoMapForNetwork(state)
	published := state.nodeInfosByName
	if !c.updateNodeInfoForNetwork(state, remote.name, &remote) {
		t.Fatal("remote node addition was ignored")
	}
	if len(published) != 1 || len(state.nodeInfosByName) != 2 {
		t.Fatal("updating a node must preserve the previous snapshot")
	}
	if len(original) != 1 || state.nodeInfo == nil || state.nodeInfo.chassisID != "old" {
		t.Fatal("adding a remote node changed an earlier snapshot or the local node")
	}

	// Readers may change their map without changing the controller's snapshot.
	readCopy := c.nodeInfoMapForNetwork(state)
	delete(readCopy, local.name)
	readCopy[remote.name] = nodeInfo{name: remote.name, chassisID: "reader-only"}
	if len(state.nodeInfosByName) != 2 || state.nodeInfosByName[remote.name].chassisID != remote.chassisID {
		t.Fatal("reader map aliases the published map")
	}
	if c.updateNodeInfoForNetwork(state, remote.name, &remote) {
		t.Fatal("unchanged node should not publish a new snapshot")
	}
	if c.updateNodeInfoForNetwork(state, "missing", nil) {
		t.Fatal("removing an absent node should not publish a new snapshot")
	}

	local.chassisID = "new"
	if !c.updateNodeInfoForNetwork(state, local.name, &local) {
		t.Fatal("local node update was ignored")
	}
	if state.nodeInfo.chassisID != "new" || original[local.name].chassisID != "old" {
		t.Fatal("local node update did not preserve snapshot isolation")
	}
	if !c.updateNodeInfoForNetwork(state, local.name, nil) {
		t.Fatal("local node deletion was ignored")
	}
	if state.nodeInfo != nil || len(state.nodeInfosByName) != 1 {
		t.Fatal("local node deletion must clear local information and preserve remote nodes")
	}
	c.syncNodeInfoForNetwork(state, nil)
	if state.nodeInfo != nil || len(state.nodeInfosByName) != 0 {
		t.Fatal("resetting local node information must clear the map")
	}
}

func TestNodeInfoMapConcurrentReadersAndWriters(t *testing.T) {
	c := &Controller{nodeName: "local"}
	local := nodeInfo{name: c.nodeName}
	state := &networkState{}
	c.syncNodeInfoForNetwork(state, &local)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for writer := range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for i := range 25 {
				next := nodeInfo{name: fmt.Sprintf("remote-%d-%d", writer, i)}
				c.updateNodeInfoForNetwork(state, next.name, &next)
				snapshot := c.nodeInfoMapForNetwork(state)
				delete(snapshot, local.name)
				snapshot["reader-only"] = nodeInfo{name: "reader-only"}
			}
		}()
	}
	close(start)
	workers.Wait()
	if len(state.nodeInfosByName) != 101 || state.nodeInfo == nil || state.nodeInfo.name != local.name {
		t.Fatal("concurrent operations lost updates or changed the local node")
	}
	for writer := range 4 {
		for i := range 25 {
			name := fmt.Sprintf("remote-%d-%d", writer, i)
			if info, ok := state.nodeInfosByName[name]; !ok || info.name != name {
				t.Fatalf("concurrent operations lost node %s", name)
			}
		}
	}
}

func BenchmarkNodeInfoMapUpdate(b *testing.B) {
	for _, count := range []int{100, 1000, 3069} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			c := &Controller{nodeName: "node-0"}
			state := &networkState{nodeInfosByName: make(map[string]nodeInfo, count)}
			for i := range count {
				name := fmt.Sprintf("node-%d", i)
				state.nodeInfosByName[name] = nodeInfo{name: name}
			}
			next := nodeInfo{name: c.nodeName}
			b.ReportAllocs()
			for b.Loop() {
				if next.chassisID == "old" {
					next.chassisID = "new"
				} else {
					next.chassisID = "old"
				}
				if !c.updateNodeInfoForNetwork(state, next.name, &next) {
					b.Fatal("benchmark update did not change node information")
				}
			}
		})
	}
}
