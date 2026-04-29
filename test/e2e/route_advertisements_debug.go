// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

// Diagnostic dumper for the "BGP: Pod to external server when CUDN network is
// advertised" tests — issue https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5952.
//
// The flake only reproduces in GitHub CI. This file adds a JustAfterEach
// (wired from route_advertisements.go) that, on failure, dumps everything
// that matters for the no-overlay BGP/CUDN datapath into
//     /var/log/debug-5952/<test>-<ns>/<node>/
// on each kind node. `kind export logs` sweeps /var/log/* out into the
// uploaded CI artifact, so the dumps land next to the existing e2e-dbs/
// directory with no extra wiring.

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

const (
	debug5952BaseDir      = "/var/log/debug-5952"
	debug5952FRRNamespace = "frr-k8s-system"
)

// dumpNoOverlayDebug is called from a ginkgo.JustAfterEach when a target test
// fails. It is fully defensive: every collection error is logged via
// framework.Logf and execution continues. This function must never fail the
// test further.
func dumpNoOverlayDebug(f *framework.Framework, nodes []corev1.Node, cudnName, clientPodName, clientPodNS string) {
	if !ginkgo.CurrentSpecReport().Failed() {
		return
	}
	if f == nil || f.ClientSet == nil {
		framework.Logf("debug-5952: framework not initialized, skipping dump")
		return
	}

	testName := ginkgo.CurrentSpecReport().LeafNodeText
	slug := sanitizeForPath(testName) + "-" + f.UniqueName
	framework.Logf("debug-5952: test failed, collecting diagnostics for %q (slug=%q, cudn=%q, clientPod=%s/%s)",
		testName, slug, cudnName, clientPodNS, clientPodName)

	// Per-node dumps ---------------------------------------------------------
	for i := range nodes {
		node := nodes[i]
		nodeDir := filepath.Join(debug5952BaseDir, slug, node.Name)
		dumpNodeKernelState(node.Name, nodeDir, clientPodNS, clientPodName)
	}

	// Shared dumps (K8s objects, ovnkube DBs, FRR, external containers) ------
	// Dump into every node's copy so the artifact is self-contained per-node:
	// a reviewer can open *any* node's dir first and find the README and
	// shared dumps there, not only on nodes[0].
	for i := range nodes {
		node := nodes[i]
		sharedDir := filepath.Join(debug5952BaseDir, slug, node.Name, "cluster")
		mkdirOnNode(node.Name, sharedDir)
		dumpOVNDatabases(node.Name, sharedDir)
		dumpRawOVNDBFiles(node.Name, sharedDir)
	}

	// Collect the cross-cluster data once on nodes[0], then replicate the
	// resulting text files into every node's cluster/ dir below so the
	// artifact is truly self-contained per-node.
	if len(nodes) > 0 {
		primaryNode := nodes[0].Name
		primarySharedDir := filepath.Join(debug5952BaseDir, slug, primaryNode, "cluster")
		dumpKubernetesObjects(f, primaryNode, primarySharedDir, cudnName, clientPodName, clientPodNS)
		dumpPodLogs(primaryNode, primarySharedDir)
		dumpFRRPods(f, primaryNode, primarySharedDir)
		dumpExternalContainers(primaryNode, primarySharedDir)
		writeReadme(primaryNode, primarySharedDir, testName, cudnName, clientPodNS, clientPodName, nodes, clientPodNodeName(clientPodName, clientPodNS, nodes))

		// Replicate the shared (non-NBDB/SBDB) files to other nodes so any
		// downloaded per-node artifact stands on its own.
		for i := range nodes {
			if nodes[i].Name == primaryNode {
				continue
			}
			dstShared := filepath.Join(debug5952BaseDir, slug, nodes[i].Name, "cluster")
			replicateSharedDumps(primaryNode, primarySharedDir, nodes[i].Name, dstShared)
		}

		// Also write a top-level "0-START-HERE.txt" at /var/log/debug-5952/
		// on each node so `ls` in the unzipped artifact surfaces it first
		// (digit sorts before letters in most filesystems/zip listings).
		startHere := buildStartHereText(testName, slug, cudnName, clientPodNS, clientPodName, nodes, clientPodNodeName(clientPodName, clientPodNS, nodes))
		for i := range nodes {
			writeToNodeFile(nodes[i].Name, filepath.Join(debug5952BaseDir, "0-START-HERE.txt"), startHere)
		}
	}

	framework.Logf("debug-5952: diagnostics collection complete for %q (slug=%q)", testName, slug)
}

// clientPodNodeName returns the name of the node hosting the client pod, used
// for guiding reviewers straight at the most interesting per-node directory.
func clientPodNodeName(clientPodName, clientPodNS string, nodes []corev1.Node) string {
	if clientPodName == "" || clientPodNS == "" {
		return ""
	}
	// Best-effort: the test always schedules echo-client-pod on nodes.Items[1].
	if len(nodes) >= 2 {
		return nodes[1].Name
	}
	return ""
}

// replicateSharedDumps tars+exfils the text dumps from primary's cluster/
// into another node's cluster/ so per-node artifacts are self-contained.
// OVN NBDB/SBDB outputs already live in each node's own cluster/ (they're
// node-local) so we skip those via a name-based filter.
func replicateSharedDumps(srcNode, srcDir, dstNode, dstDir string) {
	mkdirOnNode(dstNode, dstDir)
	// List files (not directories) to replicate; skip node-local content:
	// nbctl-*, sbctl-* are per-node DB queries, and rawdb/ holds per-node
	// NBDB/SBDB/OVSDB raw copies — every node already has its own.
	out, err := infraprovider.Get().ExecK8NodeCommand(srcNode, []string{
		"sh", "-c", fmt.Sprintf("cd %s && ls -p | grep -vE '^(nbctl-|sbctl-|rawdb/|.+/)'", shellSingleQuoteEscape(srcDir)),
	})
	if err != nil {
		framework.Logf("debug-5952: list shared dumps on %s failed: %v", srcNode, err)
		return
	}
	for _, name := range strings.Fields(out) {
		content, err := infraprovider.Get().ExecK8NodeCommand(srcNode, []string{
			"cat", filepath.Join(srcDir, name),
		})
		if err != nil {
			framework.Logf("debug-5952: read %s from %s failed: %v", name, srcNode, err)
			continue
		}
		writeToNodeFile(dstNode, filepath.Join(dstDir, name), content)
	}
}

// buildStartHereText returns the content for the top-level index file that
// lands at /var/log/debug-5952/0-START-HERE.txt on every node — the first
// file a reviewer sees when they `ls` the debug-5952 dir in the artifact.
func buildStartHereText(testName, slug, cudnName, ns, pod string, nodes []corev1.Node, clientPodNode string) string {
	var sb strings.Builder
	fmt.Fprintln(&sb, "debug-5952 — diagnostic dumps for BGP CUDN flake (issue #5952)")
	fmt.Fprintln(&sb, "================================================================")
	fmt.Fprintln(&sb)
	fmt.Fprintf(&sb, "Test that failed here: %s\n", testName)
	fmt.Fprintf(&sb, "Timestamp: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "CUDN: %s\n", cudnName)
	fmt.Fprintf(&sb, "Client pod: %s/%s (on node %s)\n", ns, pod, clientPodNode)
	fmt.Fprintln(&sb)
	fmt.Fprintln(&sb, "DIRECTORY LAYOUT INSIDE THIS ARTIFACT")
	fmt.Fprintln(&sb, "-------------------------------------")
	fmt.Fprintln(&sb, "  <node>/debug-5952/0-START-HERE.txt               (this file — same on every node)")
	fmt.Fprintf(&sb, "  <node>/debug-5952/%s/<node>/         per-node kernel / OVS / nft state at failure\n", slug)
	fmt.Fprintf(&sb, "  <node>/debug-5952/%s/<node>/cluster/  OVN NBDB/SBDB queries (nbctl-*.txt, sbctl-*.txt) +\n", slug)
	fmt.Fprintln(&sb, "                                                     K8s YAMLs, FRR runtime state,")
	fmt.Fprintln(&sb, "                                                     external-frr/bgpserver state")
	fmt.Fprintln(&sb, "                                                     (replicated on every node for convenience)")
	fmt.Fprintf(&sb, "  <node>/debug-5952/%s/<node>/cluster/rawdb/  raw NBDB/SBDB/OVSDB .db files for that node\n", slug)
	fmt.Fprintln(&sb, "                                                     (replay offline with ovsdb-server + ovn-nbctl)")
	fmt.Fprintln(&sb)
	fmt.Fprintln(&sb, "TO REPLAY A RAW NBDB FILE OFFLINE")
	fmt.Fprintln(&sb, "---------------------------------")
	fmt.Fprintln(&sb, "  cd <node>/debug-5952/<slug>/<node>/cluster/rawdb/")
	fmt.Fprintln(&sb, "  ovsdb-server --remote=punix:/tmp/nb.sock --detach --pidfile=/tmp/nb.pid ovnnb_db.db")
	fmt.Fprintln(&sb, "  ovn-nbctl --db=unix:/tmp/nb.sock show")
	fmt.Fprintln(&sb, "  ovn-nbctl --db=unix:/tmp/nb.sock lr-route-list <GR_name>")
	fmt.Fprintln(&sb, "  kill $(cat /tmp/nb.pid)")
	fmt.Fprintln(&sb)
	if clientPodNode != "" {
		fmt.Fprintf(&sb, "TIP: start with %s/debug-5952/%s/%s/ — that node hosted the failing client pod.\n", clientPodNode, slug, clientPodNode)
		fmt.Fprintln(&sb)
	}
	fmt.Fprintln(&sb, "HYPOTHESIS CHEAT-SHEET — which file to open first")
	fmt.Fprintln(&sb, "-------------------------------------------------")
	fmt.Fprintln(&sb, "  A) CUDN OVN GR missing static route to 172.26.0.0/16 (PR #6071 theory)")
	fmt.Fprintf(&sb, "     → %s/debug-5952/%s/%s/cluster/nbctl-per-lr.txt\n", firstOrEmpty(nodes), slug, firstOrEmpty(nodes))
	fmt.Fprintln(&sb, "         search for 'GR_cluster_udn_' then grep '172.26'")
	fmt.Fprintln(&sb, "  B) mp*-udn-vrf management-port race (node-subnets annotation not in place)")
	if clientPodNode != "" {
		fmt.Fprintf(&sb, "     → %s/debug-5952/%s/%s/ip-link-vrf.txt and ip-link.txt\n", clientPodNode, slug, clientPodNode)
		fmt.Fprintf(&sb, "         cross-check %s/debug-5952/%s/%s/cluster/nodes.yaml for k8s.ovn.org/node-subnets\n", clientPodNode, slug, clientPodNode)
	}
	fmt.Fprintln(&sb, "  C) no-overlay SNAT nftables rule missing for CUDN pod subnet")
	if clientPodNode != "" {
		fmt.Fprintf(&sb, "     → %s/debug-5952/%s/%s/nft-ruleset.txt\n", clientPodNode, slug, clientPodNode)
		fmt.Fprintln(&sb, "         look for 'inet ovn-kubernetes' table and rules matching the CUDN CIDR (103.x)")
	}
	fmt.Fprintln(&sb, "  D) BGP session flap / CUDN subnet not advertised to external FRR at curl time")
	fmt.Fprintf(&sb, "     → %s/debug-5952/%s/%s/cluster/frr-pod-*.txt (cluster-side FRR)\n", firstOrEmpty(nodes), slug, firstOrEmpty(nodes))
	fmt.Fprintf(&sb, "     → %s/debug-5952/%s/%s/cluster/external-frr.txt (external FRR)\n", firstOrEmpty(nodes), slug, firstOrEmpty(nodes))
	fmt.Fprintln(&sb)
	fmt.Fprintln(&sb, "See the full README at <node>/debug-5952/"+slug+"/"+firstOrEmpty(nodes)+"/cluster/README.txt")
	return sb.String()
}

func firstOrEmpty(nodes []corev1.Node) string {
	if len(nodes) == 0 {
		return ""
	}
	return nodes[0].Name
}

// ----------------------------------------------------------------------------
// Per-node kernel / nftables / OVS / conntrack dumps
// ----------------------------------------------------------------------------

func dumpNodeKernelState(nodeName, nodeDir, clientPodNS, clientPodName string) {
	mkdirOnNode(nodeName, nodeDir)

	// Network stack
	runAndSaveOnNode(nodeName, nodeDir, "ip-link.txt", "ip", "-d", "link", "show")
	runAndSaveOnNode(nodeName, nodeDir, "ip-link-vrf.txt", "ip", "-d", "link", "show", "type", "vrf")
	runAndSaveOnNode(nodeName, nodeDir, "ip-addr.txt", "ip", "-4", "addr", "show")
	runAndSaveOnNode(nodeName, nodeDir, "ip-addr-v6.txt", "ip", "-6", "addr", "show")
	runAndSaveOnNode(nodeName, nodeDir, "ip-route.txt", "ip", "route", "show", "table", "all")
	runAndSaveOnNode(nodeName, nodeDir, "ip-route-v6.txt", "ip", "-6", "route", "show", "table", "all")
	runAndSaveOnNode(nodeName, nodeDir, "ip-rule.txt", "ip", "rule", "show")
	runAndSaveOnNode(nodeName, nodeDir, "ip-rule-v6.txt", "ip", "-6", "rule", "show")
	runAndSaveOnNode(nodeName, nodeDir, "ip-neigh.txt", "ip", "neigh", "show")
	runAndSaveOnNode(nodeName, nodeDir, "ip-neigh-v6.txt", "ip", "-6", "neigh", "show")
	runAndSaveOnNode(nodeName, nodeDir, "bridge-fdb.txt", "bridge", "fdb", "show")

	// nftables. The nft binary shipped in the current kind images (nftables
	// 1.0.6 on Debian 12) reliably segfaults on `nft list ruleset` and any
	// family-level enumeration (`nft list tables`, `nft list tables ip`,
	// etc.), leaving coredumps that would otherwise trigger the test
	// framework's coredump-abort path in wrappedTestFramework (util.go).
	// We therefore probe only specific per-table listings that have been
	// observed safe on that image, and rely on `nft` being in the
	// skippedCoredumps list so any remaining crash (e.g. against
	// `inet ovn-kubernetes`) is tolerated.  We still attempt the OVN
	// no-overlay SNAT table (`inet ovn-kubernetes`) — on newer nft versions
	// in CI it may succeed; if it crashes, the crash is skipped.
	runAndSaveOnNode(nodeName, nodeDir, "nft-ruleset.txt", "sh", "-c", `
set +e
echo '### nft --version'
nft --version 2>&1
echo
for entry in \
  'inet ovn-kubernetes' \
  'ip filter' 'ip nat' 'ip mangle' 'ip raw' 'ip security' \
  'ip6 filter' 'ip6 nat' 'ip6 mangle' 'ip6 raw' 'ip6 security'; do
  out=$(nft list table $entry 2>&1)
  rc=$?
  if [ $rc -eq 0 ] && [ -n "$out" ]; then
    echo "### nft list table $entry"
    echo "$out"
    echo
  elif [ $rc -ne 0 ] && [ -n "$out" ] && ! echo "$out" | grep -q 'No such file'; then
    echo "### nft list table $entry (rc=$rc, partial/err output)"
    echo "$out"
    echo
  fi
done
`)
	runAndSaveOnNode(nodeName, nodeDir, "iptables-save.txt", "iptables-save")
	runAndSaveOnNode(nodeName, nodeDir, "ip6tables-save.txt", "ip6tables-save")

	// Conntrack (best effort — may not be installed)
	runAndSaveOnNode(nodeName, nodeDir, "conntrack.txt", "sh", "-c", "conntrack -L 2>&1 | head -n 2000 || true")

	// OVS state
	runAndSaveOnNode(nodeName, nodeDir, "ovs-vsctl-show.txt", "ovs-vsctl", "show")
	runAndSaveOnNode(nodeName, nodeDir, "ovs-ofctl-brint.txt", "sh", "-c", "ovs-ofctl dump-flows br-int 2>&1 | head -n 4000")
	runAndSaveOnNode(nodeName, nodeDir, "ovs-ofctl-breth0.txt", "sh", "-c", "ovs-ofctl dump-flows breth0 2>&1 || true")
	runAndSaveOnNode(nodeName, nodeDir, "ovs-dpctl-flows.txt", "sh", "-c", "ovs-appctl dpctl/dump-flows 2>&1 | head -n 4000")
	runAndSaveOnNode(nodeName, nodeDir, "ovs-dpctl-conntrack.txt", "sh", "-c", "ovs-appctl dpctl/dump-conntrack 2>&1 | head -n 4000 || true")
	runAndSaveOnNode(nodeName, nodeDir, "ovs-fdb.txt", "sh", "-c", "ovs-appctl fdb/show br-int 2>&1 || true")

	// From within the failing client pod, capture its own view (best effort
	// — the pod may already be terminating).
	if clientPodName != "" && clientPodNS != "" {
		runAndSaveKubectl(nodeName, nodeDir, "clientpod-ip-addr.txt",
			[]string{"exec", "-n", clientPodNS, clientPodName, "--", "ip", "addr"})
		runAndSaveKubectl(nodeName, nodeDir, "clientpod-ip-route.txt",
			[]string{"exec", "-n", clientPodNS, clientPodName, "--", "ip", "route"})
		runAndSaveKubectl(nodeName, nodeDir, "clientpod-ip-route-v6.txt",
			[]string{"exec", "-n", clientPodNS, clientPodName, "--", "ip", "-6", "route"})
	}
}

// ----------------------------------------------------------------------------
// OVN NB/SB database dumps via the ovnkube pods on the given node
// ----------------------------------------------------------------------------

// dumpRawOVNDBFiles copies the raw NBDB/SBDB/OVSDB .db files from their live
// locations into the per-test debug-5952 dir so reviewers can replay any
// ovn-nbctl/ovn-sbctl query offline against the exact state at failure. The
// pre-queried text dumps (nbctl-*.txt, sbctl-*.txt) are still produced for
// quick grepping; these raw files are the authoritative record.
//
// OVN maintainers replay these with:
//
//	ovsdb-server --remote=punix:/tmp/nb.sock ovnnb_db.db &
//	ovn-nbctl --db=unix:/tmp/nb.sock show
//
// Live paths are the same conventions the existing wrappedTestFramework
// JustAfterEach uses (util.go): /var/lib/openvswitch/ovn{nb,sb}_db.db and
// OVS conf.db at /etc/openvswitch/conf.db or /var/lib/openvswitch/conf.db.
func dumpRawOVNDBFiles(nodeName, sharedDir string) {
	dstDir := filepath.Join(sharedDir, "rawdb")
	mkdirOnNode(nodeName, dstDir)
	// Use a single shell to stat + copy each candidate path. Only files that
	// exist get copied; missing ones are silently skipped.
	script := `
set +e
copy_if() {
  [ -s "$1" ] && cp -f -- "$1" "$2/$(basename "$1")" && echo "copied $1 -> $2"
}
copy_if /var/lib/openvswitch/ovnnb_db.db ` + shellSingleQuoteEscape(dstDir) + `
copy_if /var/lib/openvswitch/ovnsb_db.db ` + shellSingleQuoteEscape(dstDir) + `
# OVS conf.db can live in either location depending on the image.
copy_if /etc/openvswitch/conf.db          ` + shellSingleQuoteEscape(dstDir) + `
copy_if /var/lib/openvswitch/conf.db      ` + shellSingleQuoteEscape(dstDir) + `
ls -la ` + shellSingleQuoteEscape(dstDir) + `
`
	out, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"sh", "-c", script})
	if err != nil {
		framework.Logf("debug-5952: copy raw DBs on %s failed: %v\n%s", nodeName, err, out)
		return
	}
	framework.Logf("debug-5952: raw DBs on %s:\n%s", nodeName, out)
}

func dumpOVNDatabases(nodeName, sharedDir string) {
	ovnkNamespace := deploymentconfig.Get().OVNKubernetesNamespace()

	// In single-node-zones IC mode (which the target lane uses), NB/SB DBs
	// live inside the ovnkube-node pod on each node. Find it.
	nbPod, nbContainer, nbErr := findNBDBPodOnNode(nodeName, ovnkNamespace)
	if nbErr != nil {
		framework.Logf("debug-5952: could not locate nbdb pod on node %s: %v", nodeName, nbErr)
	}

	nbCommands := [][]string{
		{"ovn-nbctl", "show"},
		{"ovn-nbctl", "list", "logical_router"},
		{"ovn-nbctl", "list", "logical_switch"},
		{"ovn-nbctl", "list", "logical_router_static_route"},
		{"ovn-nbctl", "list", "logical_router_policy"},
		{"ovn-nbctl", "list", "nat"},
		{"ovn-nbctl", "list", "address_set"},
		{"ovn-nbctl", "list", "load_balancer"},
		{"ovn-nbctl", "list", "gateway_chassis"},
	}
	if nbErr == nil {
		for _, cmd := range nbCommands {
			fileName := "nbctl-" + strings.Join(cmd[1:], "-") + ".txt"
			args := []string{"exec", nbPod, "-c", nbContainer, "--"}
			args = append(args, cmd...)
			runAndSaveKubectlNs(ovnkNamespace, nodeName, sharedDir, fileName, args)
		}

		// lr-route-list / lr-nat-list / lr-policy-list per logical router.
		// List LRs first then iterate — done via a single sh to avoid N round-trips.
		script := `for lr in $(ovn-nbctl --bare --columns=name list logical_router); do
  echo "=== lr-route-list $lr ==="; ovn-nbctl lr-route-list "$lr" 2>&1 || true;
  echo "=== lr-nat-list $lr ==="; ovn-nbctl lr-nat-list "$lr" 2>&1 || true;
  echo "=== lr-policy-list $lr ==="; ovn-nbctl lr-policy-list "$lr" 2>&1 || true;
done`
		args := []string{"exec", nbPod, "-c", nbContainer, "--", "sh", "-c", script}
		runAndSaveKubectlNs(ovnkNamespace, nodeName, sharedDir, "nbctl-per-lr.txt", args)
	}

	// SBDB — usually colocated with NBDB.
	sbPod, sbContainer, sbErr := findSBDBPodOnNode(nodeName, ovnkNamespace)
	if sbErr != nil {
		framework.Logf("debug-5952: could not locate sbdb pod on node %s: %v", nodeName, sbErr)
		return
	}
	sbCommands := [][]string{
		{"ovn-sbctl", "show"},
		{"ovn-sbctl", "list", "chassis"},
		{"ovn-sbctl", "list", "port_binding"},
		{"ovn-sbctl", "list", "mac_binding"},
		{"ovn-sbctl", "list", "datapath_binding"},
	}
	for _, cmd := range sbCommands {
		fileName := "sbctl-" + strings.Join(cmd[1:], "-") + ".txt"
		args := []string{"exec", sbPod, "-c", sbContainer, "--"}
		args = append(args, cmd...)
		runAndSaveKubectlNs(ovnkNamespace, nodeName, sharedDir, fileName, args)
	}
}

// findNBDBPodOnNode returns (podName, containerName). In IC single-zone +
// helm mode the DBs live inside the ovnkube-node pod as the nb-ovsdb / sb-ovsdb
// containers. Non-helm / non-IC layouts use different names — try common
// combinations and accept the first one whose pod+container both resolve.
func findNBDBPodOnNode(nodeName, ns string) (string, string, error) {
	return findDBPodOnNode(nodeName, ns, []dbCandidate{
		{"name=ovnkube-node", "nb-ovsdb"},
		{"app=ovnkube-node", "nb-ovsdb"},
		{"name=ovnkube-node", "nbdb"},
		{"app=ovnkube-node", "nbdb"},
		{"name=ovnkube-db", "nb-ovsdb"},
		{"app=ovnkube-db", "nb-ovsdb"},
	}, "nbdb")
}

func findSBDBPodOnNode(nodeName, ns string) (string, string, error) {
	return findDBPodOnNode(nodeName, ns, []dbCandidate{
		{"name=ovnkube-node", "sb-ovsdb"},
		{"app=ovnkube-node", "sb-ovsdb"},
		{"name=ovnkube-node", "sbdb"},
		{"app=ovnkube-node", "sbdb"},
		{"name=ovnkube-db", "sb-ovsdb"},
		{"app=ovnkube-db", "sb-ovsdb"},
	}, "sbdb")
}

type dbCandidate struct {
	labelSelector string
	container     string
}

func findDBPodOnNode(nodeName, ns string, candidates []dbCandidate, kind string) (string, string, error) {
	for _, c := range candidates {
		name, err := findPodOnNode(ns, c.labelSelector, nodeName)
		if err != nil || name == "" {
			continue
		}
		if !containerExistsInPod(ns, name, c.container) {
			continue
		}
		return name, c.container, nil
	}
	return "", "", fmt.Errorf("no %s pod found on node %s", kind, nodeName)
}

func containerExistsInPod(ns, podName, container string) bool {
	out, err := e2ekubectl.RunKubectl(ns,
		"get", "pod", podName,
		"-o", "jsonpath={.spec.containers[*].name}",
	)
	if err != nil {
		return false
	}
	for _, name := range strings.Fields(out) {
		if name == container {
			return true
		}
	}
	return false
}

func findPodOnNode(ns, labelSelector, nodeName string) (string, error) {
	out, err := e2ekubectl.RunKubectl(ns,
		"get", "pods",
		"-l", labelSelector,
		"--field-selector", "spec.nodeName="+nodeName,
		"-o", "jsonpath={.items[0].metadata.name}",
	)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("no pod matching %q on %s", labelSelector, nodeName)
	}
	return out, nil
}

// ----------------------------------------------------------------------------
// Kubernetes objects
// ----------------------------------------------------------------------------

func dumpKubernetesObjects(f *framework.Framework, hostNode, sharedDir, cudnName, clientPodName, clientPodNS string) {
	// Write to host-side tmpfs first (from the test process), then push to
	// the node filesystem so `kind export logs` collects them.
	type fetch struct {
		fileName string
		args     []string
	}
	fetches := []fetch{
		{"cudn.yaml", []string{"get", "clusteruserdefinednetworks.k8s.ovn.org", cudnName, "-o", "yaml"}},
		{"cudn-describe.txt", []string{"describe", "clusteruserdefinednetworks.k8s.ovn.org", cudnName}},
		{"routeadvertisements.yaml", []string{"get", "routeadvertisements.k8s.ovn.org", "-A", "-o", "yaml"}},
		{"frrconfigurations.yaml", []string{"get", "frrconfigurations.frrk8s.metallb.io", "-A", "-o", "yaml"}},
		{"nad.yaml", []string{"get", "network-attachment-definitions.k8s.cni.cncf.io", "-A", "-o", "yaml"}},
		{"nodes.yaml", []string{"get", "nodes", "-o", "yaml"}},
		{"events-ovnk.txt", []string{"get", "events", "-n", deploymentconfig.Get().OVNKubernetesNamespace(), "--sort-by=.lastTimestamp"}},
		{"events-frr.txt", []string{"get", "events", "-n", debug5952FRRNamespace, "--sort-by=.lastTimestamp"}},
		{"pods-ovnk.txt", []string{"get", "pods", "-n", deploymentconfig.Get().OVNKubernetesNamespace(), "-o", "wide"}},
		{"pods-frr.txt", []string{"get", "pods", "-n", debug5952FRRNamespace, "-o", "wide"}},
	}
	if clientPodNS != "" && clientPodName != "" {
		fetches = append(fetches,
			fetch{"clientpod.yaml", []string{"get", "pod", clientPodName, "-n", clientPodNS, "-o", "yaml"}},
			fetch{"clientpod-describe.txt", []string{"describe", "pod", clientPodName, "-n", clientPodNS}},
			fetch{"events-testns.txt", []string{"get", "events", "-n", clientPodNS, "--sort-by=.lastTimestamp"}},
		)
	}
	for _, ft := range fetches {
		out, err := e2ekubectl.RunKubectl("", ft.args...)
		if err != nil {
			out = fmt.Sprintf("kubectl %v: %v\n%s", ft.args, err, out)
		}
		writeToNodeFile(hostNode, filepath.Join(sharedDir, ft.fileName), out)
	}
}

// ----------------------------------------------------------------------------
// ovnkube, ovs, ovn-controller container logs
// ----------------------------------------------------------------------------

func dumpPodLogs(hostNode, sharedDir string) {
	ns := deploymentconfig.Get().OVNKubernetesNamespace()
	// Label selectors + container names. Tail to keep artifacts small.
	// We only tail *all* pods in the namespace once — kind export logs covers
	// the per-node container logs too, but this gives an easy, searchable
	// merged view dated at the moment of failure.
	logRequests := []struct {
		fileName  string
		selector  string
		container string
	}{
		{"logs-ovnkube-node.txt", "name=ovnkube-node", "ovnkube-controller"},
		{"logs-ovnkube-node-nbdb.txt", "name=ovnkube-node", "nbdb"},
		{"logs-ovnkube-node-sbdb.txt", "name=ovnkube-node", "sbdb"},
		{"logs-ovnkube-node-ovn-controller.txt", "name=ovnkube-node", "ovn-controller"},
		{"logs-ovnkube-control-plane.txt", "name=ovnkube-control-plane", "ovnkube-cluster-manager"},
		{"logs-ovs-node.txt", "app=ovs-node", ""},
	}
	for _, r := range logRequests {
		args := []string{"logs", "-l", r.selector, "-n", ns, "--tail=1000", "--prefix=true", "--timestamps=true"}
		if r.container != "" {
			args = append(args, "-c", r.container)
		}
		out, err := e2ekubectl.RunKubectl("", args...)
		if err != nil {
			out = fmt.Sprintf("kubectl %v: %v\n%s", args, err, out)
		}
		writeToNodeFile(hostNode, filepath.Join(sharedDir, r.fileName), out)
	}
}

// ----------------------------------------------------------------------------
// FRR daemons (cluster-side) + external FRR / server containers
// ----------------------------------------------------------------------------

func dumpFRRPods(f *framework.Framework, hostNode, sharedDir string) {
	pods, err := f.ClientSet.CoreV1().Pods(debug5952FRRNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=frr-k8s",
	})
	if err != nil || pods == nil {
		// Fall back to the daemonset name selector.
		pods, err = f.ClientSet.CoreV1().Pods(debug5952FRRNamespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			framework.Logf("debug-5952: list frr-k8s pods failed: %v", err)
			return
		}
	}
	vtyshCmds := []string{
		"show running-config",
		"show bgp ipv4 summary",
		"show bgp ipv4 all",
		"show bgp vrf all ipv4 unicast summary",
		"show bgp vrf all ipv4 unicast",
		"show bgp ipv6 summary",
		"show bgp ipv6 all",
		"show bgp vrf all ipv6 unicast summary",
		"show bgp vrf all ipv6 unicast",
		// show bgp vrfs json emits vrfId unconditionally for every BGP
		// instance, including those with empty RIB; -1 == VRF_UNKNOWN.
		// Needed to distinguish "BGP instance not linked to kernel VRF"
		// from "BGP instance linked but RIB empty".
		"show bgp vrfs json",
		"show vrf",
		"show ip route vrf all",
		"show ipv6 route vrf all",
		"show interface",
		"show bgp neighbor",
		"show bfd peer",
	}
	for i := range pods.Items {
		pod := pods.Items[i]
		if !strings.Contains(pod.Name, "frr") {
			continue
		}
		var sb strings.Builder
		for _, cmd := range vtyshCmds {
			fmt.Fprintf(&sb, "\n=== vtysh -c %q ===\n", cmd)
			out, err := e2ekubectl.RunKubectl(debug5952FRRNamespace,
				"exec", pod.Name, "-c", "frr", "--", "vtysh", "-c", cmd)
			if err != nil {
				fmt.Fprintf(&sb, "<error: %v>\n%s\n", err, out)
				continue
			}
			sb.WriteString(out)
		}
		// Grab the bgpd/frr config files (small — dump whole) and log
		// tails (larger — 5000 lines to cover cluster-boot events that
		// might be well before test-fail time). FRR container image
		// ships logs under /etc/frr, not /var/log/frr.
		for _, path := range []string{"/etc/frr/frr.conf", "/etc/frr/daemons"} {
			fmt.Fprintf(&sb, "\n=== cat %s ===\n", path)
			out, err := e2ekubectl.RunKubectl(debug5952FRRNamespace,
				"exec", pod.Name, "-c", "frr", "--", "sh", "-c",
				fmt.Sprintf("cat %s 2>&1 || true", path))
			if err != nil {
				fmt.Fprintf(&sb, "<error: %v>\n", err)
				continue
			}
			sb.WriteString(out)
		}
		for _, path := range []string{"/etc/frr/frr.log", "/etc/frr/bgpd.log"} {
			fmt.Fprintf(&sb, "\n=== cat %s (tail 5000) ===\n", path)
			out, err := e2ekubectl.RunKubectl(debug5952FRRNamespace,
				"exec", pod.Name, "-c", "frr", "--", "sh", "-c",
				fmt.Sprintf("tail -n 5000 %s 2>&1 || true", path))
			if err != nil {
				fmt.Fprintf(&sb, "<error: %v>\n", err)
				continue
			}
			sb.WriteString(out)
		}
		// frr-k8s controller log tail — explains WHY the rendered config
		// changed (e.g. which FRRConfiguration was reconciled).
		fmt.Fprintf(&sb, "\n=== logs frr-k8s container (tail 2000) ===\n")
		out, err := e2ekubectl.RunKubectl(debug5952FRRNamespace,
			"logs", pod.Name, "-c", "frr-k8s", "--tail=2000", "--timestamps=true")
		if err != nil {
			fmt.Fprintf(&sb, "<error: %v>\n%s\n", err, out)
		} else {
			sb.WriteString(out)
		}
		// reloader container log — the most valuable one for the #5952
		// reload-cycle hypothesis. frr-reload.py runs here and its stdout
		// lists every vtysh command it applies. If we see
		//     no router bgp <asn> vrf mp<N>-udn-vrf
		//     router bgp <asn> vrf mp<N>-udn-vrf
		// for the wedged VRF, that's direct evidence of the mechanism.
		fmt.Fprintf(&sb, "\n=== logs reloader container (tail 5000) ===\n")
		out, err = e2ekubectl.RunKubectl(debug5952FRRNamespace,
			"logs", pod.Name, "-c", "reloader", "--tail=5000", "--timestamps=true")
		if err != nil {
			// control-plane-scoped variant (cp-reloader) in some layouts
			cpOut, cpErr := e2ekubectl.RunKubectl(debug5952FRRNamespace,
				"logs", pod.Name, "-c", "cp-reloader", "--tail=5000", "--timestamps=true")
			if cpErr != nil {
				fmt.Fprintf(&sb, "<error (reloader): %v>\n<error (cp-reloader): %v>\n", err, cpErr)
			} else {
				fmt.Fprintf(&sb, "(from cp-reloader)\n")
				sb.WriteString(cpOut)
			}
		} else {
			sb.WriteString(out)
		}

		fileName := filepath.Join(sharedDir, fmt.Sprintf("frr-pod-%s.txt", sanitizeForPath(pod.Name)))
		writeToNodeFile(hostNode, fileName, sb.String())
	}
}

func dumpExternalContainers(hostNode, sharedDir string) {
	// The external FRR router.
	frrCmds := [][]string{
		{"vtysh", "-c", "show running-config"},
		{"vtysh", "-c", "show bgp ipv4 summary"},
		{"vtysh", "-c", "show bgp ipv4"},
		{"vtysh", "-c", "show bgp ipv6 summary"},
		{"vtysh", "-c", "show bgp ipv6"},
		{"vtysh", "-c", "show ip route"},
		{"vtysh", "-c", "show ipv6 route"},
		{"vtysh", "-c", "show interface"},
		{"vtysh", "-c", "show bgp neighbor"},
		{"ip", "addr"},
		{"ip", "route"},
	}
	var frrSB strings.Builder
	for _, cmd := range frrCmds {
		fmt.Fprintf(&frrSB, "\n=== %s ===\n", strings.Join(cmd, " "))
		out, err := infraprovider.Get().ExecExternalContainerCommand(
			infraapi.ExternalContainer{Name: routerContainerName}, cmd)
		if err != nil {
			fmt.Fprintf(&frrSB, "<error: %v>\n%s\n", err, out)
			continue
		}
		frrSB.WriteString(out)
	}
	writeToNodeFile(hostNode, filepath.Join(sharedDir, "external-frr.txt"), frrSB.String())

	// The external BGP server.
	serverCmds := [][]string{
		{"ip", "addr"},
		{"ip", "route"},
		{"ip", "-6", "route"},
		{"sh", "-c", "ss -tlnp 2>&1 || netstat -tlnp 2>&1 || true"},
	}
	var serverSB strings.Builder
	for _, cmd := range serverCmds {
		fmt.Fprintf(&serverSB, "\n=== %s ===\n", strings.Join(cmd, " "))
		out, err := infraprovider.Get().ExecExternalContainerCommand(
			infraapi.ExternalContainer{Name: serverContainerName}, cmd)
		if err != nil {
			fmt.Fprintf(&serverSB, "<error: %v>\n%s\n", err, out)
			continue
		}
		serverSB.WriteString(out)
	}
	writeToNodeFile(hostNode, filepath.Join(sharedDir, "external-bgpserver.txt"), serverSB.String())
}

// ----------------------------------------------------------------------------
// Small helpers for writing/exec'ing on node containers
// ----------------------------------------------------------------------------

func mkdirOnNode(nodeName, dir string) {
	if _, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"mkdir", "-p", dir}); err != nil {
		framework.Logf("debug-5952: mkdir -p %s on %s failed: %v", dir, nodeName, err)
	}
}

func runAndSaveOnNode(nodeName, dir, fileName, cmd string, args ...string) {
	full := append([]string{cmd}, args...)
	out, err := infraprovider.Get().ExecK8NodeCommand(nodeName, full)
	if err != nil {
		out = fmt.Sprintf("$ %s\n<error: %v>\n%s", strings.Join(full, " "), err, out)
	} else {
		out = fmt.Sprintf("$ %s\n%s", strings.Join(full, " "), out)
	}
	writeToNodeFile(nodeName, filepath.Join(dir, fileName), out)
}

func runAndSaveKubectl(nodeName, dir, fileName string, args []string) {
	runAndSaveKubectlNs("", nodeName, dir, fileName, args)
}

func runAndSaveKubectlNs(ns, nodeName, dir, fileName string, args []string) {
	out, err := e2ekubectl.RunKubectl(ns, args...)
	if err != nil {
		out = fmt.Sprintf("$ kubectl %v\n<error: %v>\n%s", args, err, out)
	} else {
		out = fmt.Sprintf("$ kubectl %v\n%s", args, out)
	}
	writeToNodeFile(nodeName, filepath.Join(dir, fileName), out)
}

// writeToNodeFile writes content into a file on the node container.
// Payload is base64-encoded on the host and decoded on the node to avoid
// quoting hell with arbitrary command output (quotes, backticks, nulls,
// etc.). The base64 alphabet contains no shell metacharacters.
//
// Large content is written in chunks because `docker exec` passes the shell
// script as a single argv entry, and Linux ARG_MAX (plus docker's own
// smaller limit in some versions) causes fork/exec failures at ~128 KB.
// Each chunk is ~64 KB of raw content (~87 KB base64), well under any limit.
// First chunk truncates, subsequent chunks append.
func writeToNodeFile(nodeName, path, content string) {
	mkdirOnNode(nodeName, filepath.Dir(path))
	escapedPath := shellSingleQuoteEscape(path)
	const chunkSize = 64 * 1024
	if len(content) == 0 {
		// Still create an empty file so its presence is unambiguous.
		script := fmt.Sprintf(": > '%s'", escapedPath)
		if _, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"sh", "-c", script}); err != nil {
			framework.Logf("debug-5952: truncate-to-empty %s on %s failed: %v", path, nodeName, err)
		}
		return
	}
	for i := 0; i < len(content); i += chunkSize {
		end := i + chunkSize
		if end > len(content) {
			end = len(content)
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(content[i:end]))
		// First chunk truncates (>); subsequent chunks append (>>).
		redir := ">>"
		if i == 0 {
			redir = ">"
		}
		script := fmt.Sprintf("printf '%%s' '%s' | base64 -d %s '%s'", encoded, redir, escapedPath)
		if _, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"sh", "-c", script}); err != nil {
			framework.Logf("debug-5952: write chunk [%d:%d] to %s on %s failed: %v", i, end, path, nodeName, err)
			return
		}
	}
}

// shellSingleQuoteEscape returns s safe for inclusion inside a single-quoted
// shell string. We don't expect the path to contain single quotes, but cheap
// defense.
func shellSingleQuoteEscape(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// ----------------------------------------------------------------------------
// Misc helpers
// ----------------------------------------------------------------------------

var debug5952SafeRE = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func sanitizeForPath(s string) string {
	s = strings.TrimSpace(s)
	s = debug5952SafeRE.ReplaceAllString(s, "_")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func writeReadme(hostNode, sharedDir, testName, cudnName, ns, pod string, nodes []corev1.Node, clientPodNode string) {
	var sb strings.Builder
	fmt.Fprintln(&sb, "debug-5952 diagnostic dump")
	fmt.Fprintln(&sb, "==========================")
	fmt.Fprintln(&sb)
	fmt.Fprintf(&sb, "Test: %s\n", testName)
	fmt.Fprintf(&sb, "Timestamp: %s\n", time.Now().UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(&sb, "CUDN: %s\n", cudnName)
	fmt.Fprintf(&sb, "Client pod: %s/%s (on node %s)\n", ns, pod, clientPodNode)
	fmt.Fprintf(&sb, "Nodes:\n")
	for _, n := range nodes {
		fmt.Fprintf(&sb, "  - %s\n", n.Name)
	}
	fmt.Fprintln(&sb)
	fmt.Fprintln(&sb, "Layout: /var/log/debug-5952/<slug>/<node>/...")
	fmt.Fprintln(&sb, "  ip-*.txt, nft-ruleset.txt, iptables-save.txt, ovs-*.txt, conntrack.txt — per-node kernel/OVS state")
	fmt.Fprintln(&sb, "  cluster/nbctl-*.txt, cluster/sbctl-*.txt — OVN NB/SB DB state")
	fmt.Fprintln(&sb, "  cluster/cudn.yaml, routeadvertisements.yaml, frrconfigurations.yaml, nad.yaml, clientpod.yaml, nodes.yaml")
	fmt.Fprintln(&sb, "  cluster/logs-*.txt — tailed container logs")
	fmt.Fprintln(&sb, "  cluster/frr-pod-*.txt — in-cluster FRR daemon state")
	fmt.Fprintln(&sb, "  cluster/external-frr.txt, external-bgpserver.txt — external container state")
	fmt.Fprintln(&sb)
	fmt.Fprintln(&sb, "Hypotheses cheat-sheet (issue #5952):")
	fmt.Fprintln(&sb, "  A) CUDN OVN GR missing static route to 172.26.0.0/16 (PR #6071 theory)")
	fmt.Fprintln(&sb, "     → open cluster/nbctl-per-lr.txt, search for the CUDN GR and 172.26")
	fmt.Fprintln(&sb, "  B) mp*-udn-vrf race / missing management port")
	fmt.Fprintln(&sb, "     → open <clientpod-node>/ip-link.txt and ip-link-vrf.txt")
	fmt.Fprintln(&sb, "  C) no-overlay SNAT nftables rule missing for CUDN subnet")
	fmt.Fprintln(&sb, "     → open <clientpod-node>/nft-ruleset.txt, search CUDN pod CIDR")
	fmt.Fprintln(&sb, "  D) BGP session flap / unstable advertisement")
	fmt.Fprintln(&sb, "     → open cluster/frr-pod-*.txt and cluster/external-frr.txt (show bgp summary uptime)")
	writeToNodeFile(hostNode, filepath.Join(sharedDir, "README.txt"), sb.String())
}
