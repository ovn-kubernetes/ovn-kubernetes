// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

const (
	// diagnosticsNodeBaseDir is where diagnostics are written as seen from
	// the node filesystem. It is intentionally a subdirectory of the OVS/OVN
	// log directory: `kind export logs` copies each node's /var/log into the
	// CI "kind-logs-*" artifact, so everything written here can be
	// downloaded from GitHub CI without any workflow changes.
	diagnosticsNodeBaseDir = "/var/log/openvswitch/e2e-kv-diagnostics"
	// diagnosticsOVNPodBaseDir is the same host directory as seen from the
	// ovnkube-node pod containers, which mount the node's
	// /var/log/openvswitch at /var/log/ovn (and /var/log/openvswitch).
	diagnosticsOVNPodBaseDir = "/var/log/ovn/e2e-kv-diagnostics"

	ovnControllerContainerName = "ovn-controller"
)

// IngressFlakeDiagnostics carries the parameters of the NodePort ingress
// traffic whose dataplane state should be collected on failure, see
// https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6581
type IngressFlakeDiagnostics struct {
	// TestDirName is the per spec directory name the diagnostics are
	// written to, it should be unique per spec run.
	TestDirName string
	// ClientIPs are the iperf3 client source addresses (the external
	// container addresses).
	ClientIPs []string
	// EntryIPs are the node addresses the iperf3 clients connect to (the
	// NodePort service entry points).
	EntryIPs []string
	// NodePort is the NodePort the iperf3 clients connect to.
	NodePort int32
	// VMIPs are the VM addresses backing the NodePort service.
	VMIPs []string
	// VMName is the VirtualMachine name, used to locate the virt-launcher
	// OVS interfaces for reply direction traces.
	VMName string
	// ClientExec optionally executes a command on the iperf3 client
	// (external container) to snapshot the client side TCP state.
	ClientExec func(cmd string) (string, error)
}

// CollectIngressFlakeDiagnostics dumps, for every cluster node, the
// dataplane state needed to debug NodePort ingress traffic disruptions:
// conntrack (host and OVS datapath with zones), the br-int ct-zone
// assignments, OpenFlow flows, ovn-controller incremental engine stats and
// ofproto/trace of the ingress flow in both directions. Everything is
// written under /var/log/openvswitch/e2e-kv-diagnostics on each node so it
// ends up in the CI kind-logs artifacts. All collection is best effort:
// errors are logged and never fail the test.
func CollectIngressFlakeDiagnostics(p infraapi.Provider, cs kubernetes.Interface, diag IngressFlakeDiagnostics) {
	if diag.TestDirName == "" {
		framework.Logf("ingress flake diagnostics: empty TestDirName, skipping collection")
		return
	}
	nodes, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		framework.Logf("ingress flake diagnostics: failed listing nodes: %v", err)
		return
	}
	framework.Logf("ingress flake diagnostics: collecting into %s/%s on every node", diagnosticsNodeBaseDir, diag.TestDirName)
	for _, node := range nodes.Items {
		if output, err := p.ExecK8NodeCommand(node.Name, []string{"bash", "-c", diag.hostScript(node.Name)}); err != nil {
			framework.Logf("ingress flake diagnostics: host collection failed on node %s: %s: %v", node.Name, output, err)
		}
		if err := diag.collectOVN(cs, node.Name); err != nil {
			framework.Logf("ingress flake diagnostics: ovn collection failed on node %s: %v", node.Name, err)
		}
	}
	diag.collectClient()
}

// hostScript composes the script collecting node (host network namespace)
// state, writing to the diagnostics directory as seen from the node
// filesystem.
func (diag IngressFlakeDiagnostics) hostScript(nodeName string) string {
	dir := fmt.Sprintf("%s/%s", diagnosticsNodeBaseDir, diag.TestDirName)
	return fmt.Sprintf(`
mkdir -p %[1]s
{
echo "### $(date -u +%%FT%%T.%%3NZ) host state for node %[2]s"
echo "=== conntrack -L (ipv4)"; conntrack -L 2>&1
echo "=== conntrack -L (ipv6)"; conntrack -L -f ipv6 2>&1
echo "=== nft list ruleset"; nft list ruleset 2>&1
echo "=== ip -br addr"; ip -br addr 2>&1
echo "=== ip route show table all"; ip route show table all 2>&1
echo "=== ip -6 route show table all"; ip -6 route show table all 2>&1
echo "=== ip rule"; ip rule 2>&1; ip -6 rule 2>&1
} > %[1]s/%[2]s-host.log 2>&1
`, dir, nodeName)
}

// collectOVN collects OVS/OVN state from the ovn-controller container of the
// ovnkube-node pod running on the given node; that container has the OVS and
// OVN tooling, the ovn-controller control socket and mounts the node's
// /var/log/openvswitch at /var/log/ovn, so its output lands on the node
// filesystem and hence in the CI artifacts.
func (diag IngressFlakeDiagnostics) collectOVN(cs kubernetes.Interface, nodeName string) error {
	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	pods, err := cs.CoreV1().Pods(ovnNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return fmt.Errorf("failed to locate ovnkube-node pod on %s: %w", nodeName, err)
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no ovnkube-node pod found on node %s", nodeName)
	}
	output, err := e2ekubectl.RunKubectl(ovnNamespace,
		"exec", pods.Items[0].Name, "-c", ovnControllerContainerName, "--",
		"bash", "-c", diag.ovnScript(nodeName, deploymentconfig.Get().ExternalBridgeName(), deploymentconfig.Get().PrimaryInterfaceName()))
	if err != nil {
		return fmt.Errorf("%s: %w", output, err)
	}
	return nil
}

// ovnScript composes the script collecting OVS/OVN state, writing to the
// diagnostics directory as seen from the ovnkube-node pod containers.
func (diag IngressFlakeDiagnostics) ovnScript(nodeName, externalBridge, primaryInterface string) string {
	dir := fmt.Sprintf("%s/%s", diagnosticsOVNPodBaseDir, diag.TestDirName)
	return fmt.Sprintf(`
mkdir -p %[1]s
{
echo "### $(date -u +%%FT%%T.%%3NZ) OVS/OVN state for node %[2]s"
echo "=== ct-zone map: ovs-vsctl get Bridge br-int external_ids"; ovs-vsctl get Bridge br-int external_ids
echo "=== ovs-appctl dpctl/dump-conntrack -m"; ovs-appctl dpctl/dump-conntrack -m
echo "=== ovs-ofctl show br-int"; ovs-ofctl show br-int
echo "=== ovs-ofctl show %[3]s"; ovs-ofctl show %[3]s
echo "=== ovs-vsctl list Interface"; ovs-vsctl --columns=name,ofport,external_ids list Interface
echo "=== ovn-appctl -t ovn-controller inc-engine/show-stats"; ovn-appctl -t ovn-controller inc-engine/show-stats
%[4]s
echo "=== ovs-ofctl dump-flows br-int"; ovs-ofctl dump-flows br-int
echo "=== ovs-ofctl dump-flows %[3]s"; ovs-ofctl dump-flows %[3]s
} > %[1]s/%[2]s-ovn.log 2>&1
`, dir, nodeName, externalBridge, diag.traceScript(externalBridge, primaryInterface))
}

// traceScript composes the ofproto/trace section of the OVS/OVN collection
// script. It traces the ingress NodePort flow in the forward direction
// (client to node, entering through the external bridge uplink) and, on the
// node(s) hosting a virt-launcher OVS interface of the VM, in the reply
// direction (VM to client, entering br-int through the VM interface). Each
// trace runs twice: once with conntrack lookups resolving to new and once
// resolving to established (plus rpl in the reply direction), to tell apart
// "flows dropped the packet" from "conntrack state was lost".
func (diag IngressFlakeDiagnostics) traceScript(externalBridge, primaryInterface string) string {
	script := []string{
		// Resolve the external bridge uplink ofport (the port named after
		// the primary interface, e.g. eth0 on kind).
		fmt.Sprintf(`uplink=$(ovs-ofctl show %[1]s | sed -n 's/^ *\([0-9]\+\)(%[2]s).*/\1/p' | head -1)
uplink=${uplink:-1}
# Client source port of the established iperf3 NodePort connection, from the
# datapath conntrack; falls back to a synthetic port if not found.
sport=$(ovs-appctl dpctl/dump-conntrack | grep -o "sport=[0-9]*,dport=%[3]d" | head -1 | sed 's/sport=\([0-9]*\).*/\1/')
sport=${sport:-12345}
echo "=== ofproto/trace parameters: uplink=$uplink sport=$sport"
trace() {
	local bridge=$1 ctstate=$2 flow=$3
	flow=${flow//__SPORT__/$sport}
	echo "--- ofproto/trace $bridge $flow (ct: new)"
	ovs-appctl ofproto/trace "$bridge" "$flow"
	echo "--- ofproto/trace $bridge $flow (ct: $ctstate)"
	ovs-appctl ofproto/trace --ct-next "$ctstate" --ct-next "$ctstate" --ct-next "$ctstate" --ct-next "$ctstate" "$bridge" "$flow"
}`, externalBridge, primaryInterface, diag.NodePort),
	}
	// Forward direction: client -> nodeIP:nodePort through the external
	// bridge uplink.
	for _, entryIP := range diag.EntryIPs {
		clientIP, found := matchIPFamily(diag.ClientIPs, entryIP)
		if !found {
			continue
		}
		script = append(script, fmt.Sprintf(`trace %s 'trk,est' "in_port=$uplink,%s"`,
			externalBridge, tcpTraceFlow(clientIP, entryIP, "__SPORT__", fmt.Sprintf("%d", diag.NodePort))))
	}
	// Reply direction: VM -> client entering br-int through the
	// virt-launcher interface, only on nodes hosting one. Conntrack lookups
	// resolve with the rpl flag since this is the reply direction of the
	// client initiated NodePort connection.
	script = append(script, fmt.Sprintf(`vmports=$(ovs-vsctl --columns=name,ofport,external_ids list Interface | awk -v RS='' '/virt-launcher-%s/' | sed -n 's/^ofport *: \([0-9]\+\).*/\1/p')
echo "=== virt-launcher ofports: $vmports"
for vmport in $vmports; do`, diag.VMName))
	for _, vmIP := range diag.VMIPs {
		clientIP, found := matchIPFamily(diag.ClientIPs, vmIP)
		if !found {
			continue
		}
		script = append(script, fmt.Sprintf(`	trace br-int 'trk,est,rpl' "in_port=$vmport,%s"`,
			tcpTraceFlow(vmIP, clientIP, fmt.Sprintf("%d", iperf3DefaultPort), "__SPORT__")))
	}
	script = append(script, "done")
	return strings.Join(script, "\n")
}

const iperf3DefaultPort = 5201

// tcpTraceFlow composes an ofproto/trace TCP flow for the right IP family.
func tcpTraceFlow(srcIP, dstIP, srcPort, dstPort string) string {
	if utilnet.IsIPv6String(srcIP) {
		return fmt.Sprintf("tcp6,ipv6_src=%s,ipv6_dst=%s,tp_src=%s,tp_dst=%s", srcIP, dstIP, srcPort, dstPort)
	}
	return fmt.Sprintf("tcp,nw_src=%s,nw_dst=%s,tp_src=%s,tp_dst=%s", srcIP, dstIP, srcPort, dstPort)
}

// matchIPFamily returns the first IP from ips with the same family as ip.
func matchIPFamily(ips []string, ip string) (string, bool) {
	for _, candidate := range ips {
		if utilnet.IsIPv6String(candidate) == utilnet.IsIPv6String(ip) {
			return candidate, true
		}
	}
	return "", false
}

// collectClient logs the client side TCP socket state of the iperf3
// connections, which shows zero-window/persist stance and retransmission
// timers from the client point of view.
func (diag IngressFlakeDiagnostics) collectClient() {
	if diag.ClientExec == nil {
		return
	}
	output, err := diag.ClientExec(fmt.Sprintf("ss -tni 'dport = :%d'", diag.NodePort))
	if err != nil {
		framework.Logf("ingress flake diagnostics: client ss failed: %s: %v", output, err)
		return
	}
	framework.Logf("ingress flake diagnostics: client TCP state for NodePort %d:\n%s", diag.NodePort, output)
}
