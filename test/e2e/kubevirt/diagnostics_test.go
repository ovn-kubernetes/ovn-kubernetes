// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"strings"
	"testing"
)

func dualStackDiagnostics() IngressFlakeDiagnostics {
	return IngressFlakeDiagnostics{
		TestDirName: "spec-under-test",
		ClientIPs:   []string{"172.18.0.99", "fc00:f853:ccd:e793::99"},
		EntryIPs:    []string{"172.18.0.3", "fc00:f853:ccd:e793::3"},
		NodePort:    30666,
		VMIPs:       []string{"10.20.16.9", "fd10:0:14:1000::9"},
		VMName:      "worker-test1",
	}
}

func TestTCPTraceFlow(t *testing.T) {
	if flow := tcpTraceFlow("172.18.0.99", "172.18.0.3", "__SPORT__", "30666"); flow != "tcp,nw_src=172.18.0.99,nw_dst=172.18.0.3,tp_src=__SPORT__,tp_dst=30666" {
		t.Fatalf("unexpected IPv4 trace flow: %s", flow)
	}
	if flow := tcpTraceFlow("fd10:0:14:1000::9", "fc00:f853:ccd:e793::99", "5201", "__SPORT__"); flow != "tcp6,ipv6_src=fd10:0:14:1000::9,ipv6_dst=fc00:f853:ccd:e793::99,tp_src=5201,tp_dst=__SPORT__" {
		t.Fatalf("unexpected IPv6 trace flow: %s", flow)
	}
}

func TestMatchIPFamily(t *testing.T) {
	ips := []string{"172.18.0.99", "fc00:f853:ccd:e793::99"}
	if ip, found := matchIPFamily(ips, "10.20.16.9"); !found || ip != "172.18.0.99" {
		t.Fatalf("expected IPv4 match 172.18.0.99, got %q found=%v", ip, found)
	}
	if ip, found := matchIPFamily(ips, "fd10::9"); !found || ip != "fc00:f853:ccd:e793::99" {
		t.Fatalf("expected IPv6 match fc00:f853:ccd:e793::99, got %q found=%v", ip, found)
	}
	if ip, found := matchIPFamily([]string{"172.18.0.99"}, "fd10::9"); found {
		t.Fatalf("expected no IPv6 match from IPv4 only list, got %q", ip)
	}
	if ip, found := matchIPFamily(nil, "10.20.16.9"); found {
		t.Fatalf("expected no match from empty list, got %q", ip)
	}
}

func TestTraceScriptDualStack(t *testing.T) {
	script := dualStackDiagnostics().traceScript("breth0", "eth0")
	for _, expected := range []string{
		// Uplink and client source port resolution.
		`uplink=$(ovs-ofctl show breth0 | sed -n 's/^ *\([0-9]\+\)(eth0).*/\1/p' | head -1)`,
		`sport=$(ovs-appctl dpctl/dump-conntrack | grep -o "sport=[0-9]*,dport=30666"`,
		// Forward direction traces with forward conntrack state, per family.
		`trace breth0 'trk,est' "in_port=$uplink,tcp,nw_src=172.18.0.99,nw_dst=172.18.0.3,tp_src=__SPORT__,tp_dst=30666"`,
		`trace breth0 'trk,est' "in_port=$uplink,tcp6,ipv6_src=fc00:f853:ccd:e793::99,ipv6_dst=fc00:f853:ccd:e793::3,tp_src=__SPORT__,tp_dst=30666"`,
		// Reply direction traces with reply conntrack state, per family.
		`trace br-int 'trk,est,rpl' "in_port=$vmport,tcp,nw_src=10.20.16.9,nw_dst=172.18.0.99,tp_src=5201,tp_dst=__SPORT__"`,
		`trace br-int 'trk,est,rpl' "in_port=$vmport,tcp6,ipv6_src=fd10:0:14:1000::9,ipv6_dst=fc00:f853:ccd:e793::99,tp_src=5201,tp_dst=__SPORT__"`,
		// Reply traces only run on nodes hosting a virt-launcher interface.
		`/virt-launcher-worker-test1/`,
		// The trace function simulates both new and the passed conntrack state.
		`ovs-appctl ofproto/trace --ct-next "$ctstate" --ct-next "$ctstate" --ct-next "$ctstate" --ct-next "$ctstate" "$bridge" "$flow"`,
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("expected trace script to contain %q, got:\n%s", expected, script)
		}
	}
}

func TestTraceScriptUnmatchedIPFamilies(t *testing.T) {
	diag := dualStackDiagnostics()
	// IPv6 only client cannot trace against IPv4 entry/VM addresses.
	diag.ClientIPs = []string{"fc00:f853:ccd:e793::99"}
	diag.EntryIPs = []string{"172.18.0.3"}
	diag.VMIPs = []string{"10.20.16.9"}
	script := diag.traceScript("breth0", "eth0")
	if strings.Contains(script, "trace breth0 'trk,est' ") {
		t.Fatalf("expected no forward traces for unmatched families, got:\n%s", script)
	}
	if strings.Contains(script, "trace br-int 'trk,est,rpl' ") {
		t.Fatalf("expected no reply traces for unmatched families, got:\n%s", script)
	}
}

func TestHostScript(t *testing.T) {
	script := dualStackDiagnostics().hostScript("ovn-worker")
	for _, expected := range []string{
		"mkdir -p /var/log/openvswitch/e2e-kv-diagnostics/spec-under-test",
		"conntrack -L 2>&1",
		"conntrack -L -f ipv6 2>&1",
		"nft list ruleset",
		"> /var/log/openvswitch/e2e-kv-diagnostics/spec-under-test/ovn-worker-host.log 2>&1",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("expected host script to contain %q, got:\n%s", expected, script)
		}
	}
}

func TestOVNScript(t *testing.T) {
	script := dualStackDiagnostics().ovnScript("ovn-worker", "breth0", "eth0")
	for _, expected := range []string{
		"mkdir -p /var/log/ovn/e2e-kv-diagnostics/spec-under-test",
		"ovs-vsctl get Bridge br-int external_ids",
		"ovs-appctl dpctl/dump-conntrack -m",
		"ovs-ofctl dump-flows br-int",
		"ovs-ofctl dump-flows breth0",
		"ovn-appctl -t ovn-controller inc-engine/show-stats",
		"> /var/log/ovn/e2e-kv-diagnostics/spec-under-test/ovn-worker-ovn.log 2>&1",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("expected ovn script to contain %q, got:\n%s", expected, script)
		}
	}
}

func TestCollectClient(t *testing.T) {
	diag := dualStackDiagnostics()
	executedCmd := ""
	diag.ClientExec = func(cmd string) (string, error) {
		executedCmd = cmd
		return "ESTAB 0 0 172.18.0.99:33686 172.18.0.3:30666", nil
	}
	diag.collectClient()
	if executedCmd != "ss -tni 'dport = :30666'" {
		t.Fatalf("unexpected client command: %q", executedCmd)
	}
	// Without a client exec function nothing runs and nothing panics.
	diag.ClientExec = nil
	diag.collectClient()
}
