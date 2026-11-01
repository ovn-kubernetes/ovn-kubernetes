// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"net"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"

	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestPodAssignmentStateClone(t *testing.T) {
	state := &podAssignmentState{
		activeEgressIPs:      map[string]*podEgressIPAssignment{},
		standbyEgressIPNames: sets.New("standby"),
		podIPs:               []net.IP{net.ParseIP("10.128.0.2")},
		network:              &util.DefaultNetInfo{},
	}
	for _, name := range []string{"one", "two"} {
		state.activeEgressIPs[name] = &podEgressIPAssignment{
			egressStatuses: egressStatuses{statusMap{{Node: name, EgressIP: "192.0.2.10"}: egressStatusStatePending}},
			podIPs:         []net.IP{net.ParseIP("10.128.0.2")},
			network:        &util.DefaultNetInfo{},
		}
	}
	clone := state.Clone()
	if len(clone.activeEgressIPs) != 2 || !clone.standbyEgressIPNames.Has("standby") {
		t.Fatal("clone lost an owner or standby")
	}
	if clone.network != state.network || len(clone.podIPs) != 1 || !clone.podIPs[0].Equal(state.podIPs[0]) {
		t.Fatal("clone lost shared cleanup state")
	}
	clone.podIPs[0] = net.ParseIP("10.128.0.3")
	if clone.podIPs[0].Equal(state.podIPs[0]) {
		t.Fatal("clone shares its cleanup IP slice")
	}
	for name, original := range state.activeEgressIPs {
		copied := clone.activeEgressIPs[name]
		status := egressipv1.EgressIPStatusItem{Node: name, EgressIP: "192.0.2.10"}
		if copied == nil || copied.statusMap[status] != egressStatusStatePending || copied.network != original.network || !copied.podIPs[0].Equal(original.podIPs[0]) {
			t.Fatalf("clone changed the applied state of owner %s", name)
		}
		delete(copied.statusMap, status)
		copied.podIPs[0] = net.ParseIP("10.128.0.3")
		if !original.contains(status) || original.podIPs[0].Equal(copied.podIPs[0]) {
			t.Fatalf("clone shares mutable assignment state with owner %s", name)
		}
	}
	delete(clone.activeEgressIPs, "one")
	clone.standbyEgressIPNames.Delete("standby")
	if len(state.activeEgressIPs) != 2 || !state.standbyEgressIPNames.Has("standby") {
		t.Fatal("clone shares its owner map or standby set")
	}
}
