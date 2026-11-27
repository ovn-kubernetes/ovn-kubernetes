// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"

	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	deploymentconfigapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	utilnet "k8s.io/utils/net"
)

// These tests cover the MACBindingController (OKEP-6691), which is only active
// when ovnkube-node runs with --disable-udn-arp-ndp-flood.
//
// With that flag set, ARP replies / neighbour advertisements carrying the shared
// breth0 MAC are no longer flooded to every CUDN patch port: they reach only the
// default cluster network (CDN) gateway router. UDN gateway routers therefore
// cannot learn external neighbours themselves. Instead the controller mirrors
// the SBDB MAC_Binding rows learned on the CDN external port
// (rtoe-GR_<node>) onto every tracked primary UDN external port
// (rtoe-GR_cluster_udn_<cudn>_<node>) in the same zone.
//
// Node IPs are deliberately excluded from that mirroring; they are written as
// NBDB Static_MAC_Binding rows instead (see the "node IPs" spec below).
//
// Nothing ever deletes a mirrored row explicitly: it expires through the OVN
// logical router option mac_binding_age_threshold, which ovn-kubernetes sets to
// types.GRMACBindingAgeThreshold (300) on every gateway router.

const (
	noFloodHostnameKey = "kubernetes.io/hostname"

	// pid files for the background pings started inside the client pods.
	noFloodPingPidFileV4   = "/tmp/noflood-ping4.pid"
	noFloodPingPidFileV6   = "/tmp/noflood-ping6.pid"
	noFloodPingPidFileGlob = "/tmp/noflood-ping*.pid"

	// The lifetime these specs run with, in seconds. ovn-kubernetes ships
	// types.GRMACBindingAgeThreshold (300s), which makes every spec that has to
	// observe a binding expire, or be refreshed, cost minutes. The specs below
	// are about the controller's mirroring, refreshing and reaping, not about
	// the shipped value - that one is asserted at rest, in BeforeEach - so
	// the suites lower it on their own gateway routers and restore it on the
	// way out. OVN probes a neighbour in the last 3/16 of the lifetime, i.e.
	// ~22s here, which is still far longer than an ARP round trip on kind.
	noFloodTestAgeThreshold = 120

	// Ceiling, not an expected wait. With noFloodTestAgeThreshold applied and
	// the external container silenced first, a row goes stale within ~60s of
	// the last refresh. The ceiling stays wide because ovn-kubernetes rewrites
	// the logical router options on every gatewayInit: if an override is lost,
	// the spec falls back to the shipped 300s lifetime - measured on kind, the
	// last refresh landed 259s after the pings stopped and the row disappeared
	// at 573s - and must still pass, only slower.
	noFloodAgeOutTimeout = 900 * time.Second

	// How long we hold the bindings under sustained traffic to prove the
	// controller does not churn or prematurely drop them. Comfortably longer
	// than the ~22s refresh interval of noFloodTestAgeThreshold, so the
	// controller's refresh-mirror path is exercised several times.
	noFloodStabilityDuration = 60 * time.Second

	// After an ovnkube-node restart the databases in that pod come back empty
	// and everything has to be relearned and re-mirrored.
	noFloodReconvergeTimeout = 300 * time.Second

	// Ceiling, not an expected wait. A gateway router only notices a
	// neighbour's new MAC on its next refresh probe, and in the worst case the
	// binding was refreshed just before the change - so this is one full
	// lifetime. Wide for the same reason as noFloodAgeOutTimeout: ~60s with the
	// override in place, up to 300s without it.
	noFloodMACChangeTimeout = 600 * time.Second
)

// macBindingRow is one row of either the SBDB MAC_Binding table or the NBDB
// Static_MAC_Binding table.
type macBindingRow struct {
	logicalPort        string
	ip                 string
	mac                string
	overrideDynamicMAC bool
}

// listMACBindingRows runs an ovn-{n,s}bctl list command that emits bare CSV and
// parses the rows. Filtering happens in Go rather than through `find ip=...` so
// the test does not have to deal with quoting IPv6 addresses inside a shell
// string, which is error prone.
func listMACBindingRows(ovnkPod *v1.Pod, cmd string, columns int) ([]macBindingRow, error) {
	out, err := e2epodoutput.RunHostCmdWithRetries(ovnkPod.Namespace, ovnkPod.Name, cmd, framework.Poll, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%q failed: %w", cmd, err)
	}
	var rows []macBindingRow
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < columns {
			return nil, fmt.Errorf("unexpected output line %q from %q", line, cmd)
		}
		row := macBindingRow{logicalPort: fields[0], ip: fields[1], mac: fields[2]}
		if columns > 3 {
			row.overrideDynamicMAC = fields[3] == "true"
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// listDynamicMACBindings returns the SBDB MAC_Binding rows of the zone that
// ovnkPod belongs to.
func listDynamicMACBindings(ovnkPod *v1.Pod) ([]macBindingRow, error) {
	return listMACBindingRows(ovnkPod,
		"ovn-sbctl --format=csv --no-headings --data=bare --columns=logical_port,ip,mac list MAC_Binding", 3)
}

// listStaticMACBindings returns the NBDB Static_MAC_Binding rows of the zone
// that ovnkPod belongs to.
func listStaticMACBindings(ovnkPod *v1.Pod) ([]macBindingRow, error) {
	return listMACBindingRows(ovnkPod,
		"ovn-nbctl --format=csv --no-headings --data=bare --columns=logical_port,ip,mac,override_dynamic_mac list Static_MAC_Binding", 4)
}

// gatewayRouterExtPorts returns every gateway router external port (rtoe-*)
// present in the zone of the given ovnkube-node pod: the live port inventory,
// as opposed to any test's expected set. Foreign primary networks present on
// the node have their ports in it; routers deleted with their networks do not.
func gatewayRouterExtPorts(ovnkPod *v1.Pod) ([]string, error) {
	out, err := e2epodoutput.RunHostCmdWithRetries(ovnkPod.Namespace, ovnkPod.Name,
		"ovn-nbctl --bare --columns=name list logical_router_port", framework.Poll, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("listing logical router ports failed: %w", err)
	}
	var ports []string
	for _, line := range strings.Split(out, "\n") {
		if port := strings.TrimSpace(line); strings.HasPrefix(port, ovntypes.GWRouterToExtSwitchPrefix) {
			ports = append(ports, port)
		}
	}
	return ports, nil
}

// purgeDynamicMACBindings destroys every MAC_Binding row holding one of ips.
// Dynamic rows only ever disappear through aging, and the infra provider recycles
// external container IPs, so a run started within mac_binding_age_threshold of the
// previous one would otherwise inherit its bindings.
func purgeDynamicMACBindings(ovnkPod *v1.Pod, ips []string) {
	conds := make([]string, 0, len(ips))
	for _, ip := range ips {
		conds = append(conds, fmt.Sprintf("$2==%q", ip))
	}
	cmd := fmt.Sprintf(`for u in $(ovn-sbctl --format=csv --no-headings --data=bare --columns=_uuid,ip list MAC_Binding | awk -F, '%s{print $1}'); do ovn-sbctl --if-exists destroy MAC_Binding $u; done`,
		strings.Join(conds, "||"))
	_, err := e2epodoutput.RunHostCmdWithRetries(ovnkPod.Namespace, ovnkPod.Name, cmd, framework.Poll, 30*time.Second)
	Expect(err).NotTo(HaveOccurred(), "must purge stale MAC_Bindings for %v", ips)
}

// macBindingAgeThresholds returns router -> mac_binding_age_threshold for every
// logical router that currently exists in ovnkPod's zone. Values are strings so
// the shipped constant can be compared verbatim.
func macBindingAgeThresholds(ovnkPod *v1.Pod) map[string]string {
	out, err := e2epodoutput.RunHostCmdWithRetries(ovnkPod.Namespace, ovnkPod.Name,
		"ovn-nbctl --format=csv --no-headings --data=bare --columns=name,options list logical_router",
		framework.Poll, 10*time.Second)
	Expect(err).NotTo(HaveOccurred(), "must list the logical routers and their options")
	thresholds := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, options, found := strings.Cut(strings.TrimSpace(line), ",")
		if !found {
			continue
		}
		for _, option := range strings.Fields(options) {
			if value, ok := strings.CutPrefix(option, "mac_binding_age_threshold="); ok {
				thresholds[name] = value
			}
		}
	}
	return thresholds
}

// gatewayRoutersOf maps external (rtoe-) logical router ports to the gateway
// routers owning them, which is how the suites name the routers they assert on.
func gatewayRoutersOf(ports []string) []string {
	routers := make([]string, 0, len(ports))
	for _, port := range ports {
		routers = append(routers, strings.TrimPrefix(port, ovntypes.GWRouterToExtSwitchPrefix))
	}
	return routers
}

// setMACBindingAgeThreshold overrides mac_binding_age_threshold on the named
// gateway routers, so the specs that wait for a binding to expire or to be
// refreshed do not have to sit through the shipped 300s lifetime.
//
// Only the routers a suite asserts on are touched, never every GR_* in the zone:
// the lifetime is observable behaviour for anything else sharing the cluster, and
// a run killed outright - a step timeout, say - never gets to restore it.
//
// The override is not sticky: createGWRouter writes the option through
// CreateOrUpdateLogicalRouter with Options as a mutate field, so any gatewayInit
// puts 300 back. Callers therefore re-apply it before every spec whose runtime
// depends on it, and the matching Eventually ceilings stay wide enough for the
// shipped value - a lost override makes a spec slower, never wrong.
//
// --if-exists because the suites delete CUDNs, and with them gateway routers,
// before the restore runs.
func setMACBindingAgeThreshold(ovnkPod *v1.Pod, routers []string, seconds int) error {
	if len(routers) == 0 {
		return nil
	}
	var cmd strings.Builder
	for _, router := range routers {
		fmt.Fprintf(&cmd, "ovn-nbctl --if-exists set logical_router %s options:mac_binding_age_threshold=%d; ",
			router, seconds)
	}
	_, err := e2epodoutput.RunHostCmdWithRetries(ovnkPod.Namespace, ovnkPod.Name, cmd.String(), framework.Poll, 30*time.Second)
	return err
}

// withTestMACBindingAgeThreshold applies the shortened lifetime to the routers
// the suite owns and restores the shipped one on the way out. Both the pod and
// the router set are read when they are needed rather than captured: the pod
// changes across an ovnkube-node restart, and the suites add and remove gateway
// routers while they run.
//
// The restore is read back rather than trusted: --if-exists turns a router name
// that is wrong, rather than merely gone, into a silent no-op, so the ovn-nbctl
// exit status alone proves nothing.
//
// A router left on the short lifetime aborts the suite instead of only failing
// this node. The damage is not local to these specs - every spec that would run
// afterwards, here and in the invocations that follow on the same cluster, would
// be measured against a gateway router that expires neighbour entries five times
// faster than production. Those results would be worth less than no results, so
// the run stops and says why.
func withTestMACBindingAgeThreshold(ovnkPod **v1.Pod, routers func() []string) {
	// Registered before the override is applied, not after. setMACBindingAgeThreshold
	// asserts, and a failed assertion panics out of this function, so registering
	// afterwards would mean the one case that most needs a restore - the override
	// applied to some routers and then failed - is the one case that never gets one.
	// Restoring routers that were never changed is a no-op.
	DeferCleanup(func() {
		err := setMACBindingAgeThreshold(*ovnkPod, routers(), ovntypes.GRMACBindingAgeThresholdInt)
		if err != nil {
			AbortSuite(fmt.Sprintf(
				"could not restore the MAC binding lifetime of %s to the shipped %s. Everything that runs "+
					"next on this cluster would be measured against a gateway router these specs left "+
					"expiring neighbour entries %dx faster than production, so the suite is aborted rather "+
					"than reporting results from it. Repair with `ovn-nbctl set logical_router <router> "+
					"options:mac_binding_age_threshold=%s` in the ovnkube-node pod, or rebuild the cluster",
				strings.Join(routers(), ", "), ovntypes.GRMACBindingAgeThreshold,
				ovntypes.GRMACBindingAgeThresholdInt/noFloodTestAgeThreshold,
				ovntypes.GRMACBindingAgeThreshold))
		}
		live := macBindingAgeThresholds(*ovnkPod)
		var leaked []string
		for _, router := range routers() {
			// Routers deleted along with their CUDN have nothing to restore.
			if got, exists := live[router]; exists && got != ovntypes.GRMACBindingAgeThreshold {
				leaked = append(leaked, fmt.Sprintf("%s=%s", router, got))
			}
		}
		if len(leaked) > 0 {
			AbortSuite(fmt.Sprintf(
				"could not restore the MAC binding lifetime of %s to the shipped %s. Everything that runs "+
					"next on this cluster would be measured against a gateway router these specs left "+
					"expiring neighbour entries %dx faster than production, so the suite is aborted rather "+
					"than reporting results from it. Repair with `ovn-nbctl set logical_router <router> "+
					"options:mac_binding_age_threshold=%s` in the ovnkube-node pod, or rebuild the cluster",
				strings.Join(leaked, ", "), ovntypes.GRMACBindingAgeThreshold,
				ovntypes.GRMACBindingAgeThresholdInt/noFloodTestAgeThreshold,
				ovntypes.GRMACBindingAgeThreshold))
		}
	})
	err := setMACBindingAgeThreshold(*ovnkPod, routers(), noFloodTestAgeThreshold)
	Expect(err).NotTo(HaveOccurred(), "must set mac_binding_age_threshold=%d on %v", noFloodTestAgeThreshold, routers())
}

// silenceExternalContainer takes the container's only interface down so it stops
// answering the ARP/NDP probe OVN sends in the last 3/16 of a binding's lifetime.
// Without this the row is refreshed for roughly one more full lifetime after the
// traffic stops. Nothing deletes the row - OVN still has to expire it - so the
// aging path, and the controller reaping the mirrors behind it, are still what
// the age-out specs exercise.
func silenceExternalContainer(container infraapi.ExternalContainer) {
	iface := infraprovider.Get().ExternalContainerPrimaryInterfaceName()
	_, err := infraprovider.Get().ExecExternalContainerCommand(container,
		[]string{"ip", "link", "set", "dev", iface, "down"})
	Expect(err).NotTo(HaveOccurred(), "must bring %s down in external container %s", iface, container.GetName())
}

// macBindingPortsForIP returns logical port -> MAC for every row holding ip.
func macBindingPortsForIP(rows []macBindingRow, ip string) map[string]string {
	ports := map[string]string{}
	for _, row := range rows {
		if row.ip == ip {
			ports[row.logicalPort] = row.mac
		}
	}
	return ports
}

// cudnGatewayRouterExtPortName returns the external (rtoe-) logical router port
// of the CUDN gateway router on the given node - the mirroring target.
func cudnGatewayRouterExtPortName(cudnName, nodeName string) string {
	return ovntypes.GWRouterToExtSwitchPrefix + cudnGatewayRouterName(cudnName, nodeName)
}

// defaultGatewayRouterExtPortName returns the external (rtoe-) logical router
// port of the default cluster network gateway router - the mirroring source.
func defaultGatewayRouterExtPortName(nodeName string) string {
	return ovntypes.GWRouterToExtSwitchPrefix + ovntypes.GWRouterPrefix + nodeName
}

// externalContainerMAC returns the MAC of the external container's primary
// interface, which is the MAC the gateway routers are expected to learn.
func externalContainerMAC(c infraapi.ExternalContainer) string {
	iface := infraprovider.Get().ExternalContainerPrimaryInterfaceName()
	out, err := infraprovider.Get().ExecExternalContainerCommand(c, []string{"cat", "/sys/class/net/" + iface + "/address"})
	Expect(err).NotTo(HaveOccurred(), "must read MAC of external container %s", c.GetName())
	mac := strings.ToLower(strings.TrimSpace(out))
	Expect(mac).NotTo(BeEmpty(), "external container %s must have a MAC", c.GetName())
	return mac
}

// startBackgroundPing starts a detached ping inside the pod and records its pid
// so it can be stopped later. The correct binary is picked per IP family.
func startBackgroundPing(namespace, podName, ip, pidFile string) {
	ping := "ping"
	if utilnet.IsIPv6String(ip) {
		ping = "ping6"
	}
	_, err := e2ekubectl.RunKubectl(namespace, "exec", podName, "--", "/bin/sh", "-c",
		fmt.Sprintf("%s -q -i 1 %s >/dev/null 2>&1 & echo $! > %s", ping, ip, pidFile))
	Expect(err).NotTo(HaveOccurred(), "must start background ping to %s from %s/%s", ip, namespace, podName)
}

// stopBackgroundPings kills every ping started by startBackgroundPing in the pod
// and asserts none survives - a surviving ping would keep the MAC bindings
// refreshed and silently invalidate the age-out specs. Both families are stopped
// together because the survivor check counts "ping" and "ping6" alike.
func stopBackgroundPings(namespace, podName string) {
	_, err := e2ekubectl.RunKubectl(namespace, "exec", podName, "--", "/bin/sh", "-c",
		fmt.Sprintf("kill $(cat %s) 2>/dev/null; rm -f %s; true", noFloodPingPidFileGlob, noFloodPingPidFileGlob))
	Expect(err).NotTo(HaveOccurred(), "must stop background pings in %s/%s", namespace, podName)
	Eventually(func() string {
		out, _ := e2ekubectl.RunKubectl(namespace, "exec", podName, "--", "/bin/sh", "-c",
			"ps -o args= -A 2>/dev/null | grep -c '^ping' || true")
		return strings.TrimSpace(out)
	}, 30*time.Second, 2*time.Second).Should(Equal("0"), "no ping must survive in %s/%s", namespace, podName)
}

// noFloodGatewayConfig is the subset of the k8s.ovn.org/l3-gateway-config node
// annotation this test needs.
type noFloodGatewayConfig struct {
	MACAddress  string   `json:"mac-address"`
	IPAddresses []string `json:"ip-addresses"`
}

func noFloodGatewayConfigOf(node *v1.Node) noFloodGatewayConfig {
	raw, ok := node.Annotations["k8s.ovn.org/l3-gateway-config"]
	Expect(ok).To(BeTrue(), "node %s must have a l3-gateway-config annotation", node.Name)
	var cfg map[string]noFloodGatewayConfig
	Expect(json.Unmarshal([]byte(raw), &cfg)).To(Succeed(), "l3-gateway-config of node %s must parse", node.Name)
	def, ok := cfg["default"]
	Expect(ok).To(BeTrue(), "node %s must have a default l3-gateway-config entry", node.Name)
	return def
}

var _ = Describe("Network Segmentation: MAC binding mirroring with ARP/NDP flood disabled",
	feature.NetworkSegmentation, func() {

		f := wrappedTestFramework("noflood-macbinding")

		// The container is nested so that the framework's own BeforeEach -
		// which populates f.ClientSet and creates f.Namespace - runs before
		// ours.
		//
		// Every spec builds its own fixture in BeforeEach and starts the traffic
		// it asserts on, so each one can run on its own, selected by name, and
		// in any order.
		//
		// Serial because these specs are not parallel safe: they restart
		// ovnkube-node on the traffic node and retime the MAC bindings of the
		// default network's gateway router, both of which are visible to every
		// other spec running on that node.
		//
		// Ginkgo matches Serial in label filters, so the skip_label "Serial" that
		// PARALLEL=true installs in test/scripts/e2e-cp.sh would drop this whole
		// container. The lane that sets DISABLE_UDN_ARP_NDP_FLOOD is the only one
		// running these specs, so it runs with PARALLEL=false - see the PARALLEL
		// expression in .github/workflows/test.yml. Keep the two in step: marking
		// a container Serial without that is the same as deleting its coverage.
		//
		// TODO: PARALLEL=false is a lane-wide hammer - it also unskips every other
		// Serial spec on that lane. The narrower fix is for e2e-cp.sh to run the
		// serial specs in a second pass rather than skip them, at which point the
		// PARALLEL override here can go away.
		Context("", Serial, func() {

			var (
				cs          clientset.Interface
				providerCtx infraapi.Context

				trafficNode v1.Node
				ovnkPod     *v1.Pod

				extA, extB       infraapi.ExternalContainer
				extAMAC, extBMAC string
				// IPs actually exercised, one per supported family.
				extAIPs, extBIPs []string

				// every schedulable node other than trafficNode; their zones must
				// stay untouched.
				otherNodes []v1.Node

				cdnNamespace string
				// udnNamespace holds the pinging UDN pod; it is selected by cudnNames[0].
				udnNamespace string
				cudnNames    []string
				// the mirroring source plus the three mirroring targets.
				expectedPorts []string
				// the static bindings the CDN external port carries before any
				// test traffic - the gateway masquerade IPs. The controller must
				// never add to them.
				cdnStaticIPs []string

				// toleratedHolders records the foreign (ip, port) MAC_Binding holders
				// already logged, so polling windows do not repeat them on every
				// poll. See expectMirroredEverywhere.
				toleratedHolders = sets.New[string]()

				cdnPodName = "noflood-cdn-client"
				udnPodName = "noflood-udn-client"
			)

			// shortenAging re-applies the test lifetime to the gateway routers
			// this suite asserts on - the mirroring source plus its CUDNs - and
			// nothing else in the zone. Called again before every spec whose
			// runtime depends on it, because gatewayInit keeps putting 300 back.
			shortenAging := func() {
				err := setMACBindingAgeThreshold(ovnkPod, gatewayRoutersOf(expectedPorts), noFloodTestAgeThreshold)
				Expect(err).NotTo(HaveOccurred(), "must set mac_binding_age_threshold=%d on %v", noFloodTestAgeThreshold, gatewayRoutersOf(expectedPorts))
			}

			// macBindingPorts returns logical port -> MAC for ip in the traffic
			// node's zone. The read error is reported rather than silenced: a nil
			// map would satisfy the emptiness assertions vacuously.
			macBindingPorts := func(ip string) (map[string]string, error) {
				rows, err := listDynamicMACBindings(ovnkPod)
				if err != nil {
					return nil, err
				}
				return macBindingPortsForIP(rows, ip), nil
			}

			// expectMirroredEverywhere asserts that every port of ports holds ip
			// with mac, and that every other holder of ip carries the same mac.
			//
			// The mirror controller's follower set is open world by design: every
			// primary network's gateway router on the node is a legitimate mirror
			// target, so a foreign primary UDN - another suite's leftover, or a
			// real workload - may correctly hold the binding too. The exact-set
			// assertion this replaces failed the specs on exactly that correct
			// mirroring. What must never happen is a missing expected port or a
			// wrong MAC on any holder: those still fail. Tolerated foreign holders
			// are logged once per (ip, port) per spec so they are visible in the
			// output; the boundaries the exact set used to pin - secondary
			// networks and deleted CUDNs must not hold the test IPs - are
			// asserted explicitly by the specs that care about them.
			expectMirroredEverywhere := func(g Gomega, ip, mac string, ports []string) {
				got, err := macBindingPorts(ip)
				g.Expect(err).NotTo(HaveOccurred(), "must list MAC_Bindings for %s", ip)
				for _, port := range ports {
					g.Expect(got).To(HaveKeyWithValue(port, mac),
						"MAC_Binding for %s must exist on %s with MAC %s, got %v", ip, port, mac, got)
				}
				for port, gotMAC := range got {
					g.Expect(gotMAC).To(Equal(mac), "MAC_Binding for %s on %s must carry MAC %s", ip, port, mac)
					if !slices.Contains(ports, port) {
						if key := ip + "/" + port; !toleratedHolders.Has(key) {
							toleratedHolders.Insert(key)
							framework.Logf("tolerating MAC_Binding for %s on foreign follower %s (MAC %s)", ip, port, gotMAC)
						}
					}
				}
			}

			// ovnkPodOnNode returns the ovnkube-node pod owning the given node's
			// zone, which is how this test reaches that zone's NB/SB databases.
			ovnkPodOnNode := func(nodeName string) *v1.Pod {
				pods, err := cs.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(context.TODO(), metav1.ListOptions{
					LabelSelector: "app=ovnkube-node",
					FieldSelector: "spec.nodeName=" + nodeName,
				})
				framework.ExpectNoError(err)
				Expect(pods.Items).To(HaveLen(1), "expected exactly one ovnkube-node pod on %s", nodeName)
				return &pods.Items[0]
			}

			// staticMACBindingIPsOn returns the IPs statically bound on one
			// logical port.
			staticMACBindingIPsOn := func(port string) []string {
				static, err := listStaticMACBindings(ovnkPod)
				Expect(err).NotTo(HaveOccurred())
				var ips []string
				for _, row := range static {
					if row.logicalPort == port {
						ips = append(ips, row.ip)
					}
				}
				return ips
			}

			// deleteNamespaceAndWait removes a namespace and blocks until it is
			// really gone. A ClusterUserDefinedNetwork cannot be deleted while a
			// namespace it selects still exists, so every CUDN teardown has to
			// go through this first.
			deleteNamespaceAndWait := func(name string) {
				err := cs.CoreV1().Namespaces().Delete(context.TODO(), name, metav1.DeleteOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					framework.ExpectNoError(err, "must delete namespace %s", name)
				}
				Eventually(func() bool {
					_, err := cs.CoreV1().Namespaces().Get(context.TODO(), name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, 120*time.Second, 2*time.Second).Should(BeTrue(), "namespace %s must go away", name)
			}

			createCUDN := func(namespace, topology, cidr, role string) string {
				name := randomNetworkMetaName()
				manifest := generateClusterUserDefinedNetworkManifest(&networkAttachmentConfigParams{
					name:      name,
					namespace: namespace,
					topology:  topology,
					cidr:      cidr,
					role:      role,
				}, cs)
				cleanup, err := createManifest("", manifest)
				Expect(err).NotTo(HaveOccurred(), "must create CUDN %s", name)
				DeferCleanup(func() {
					deleteNamespaceAndWait(namespace)
					cleanup()
					_, _ = e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", name, "--wait", "--timeout=120s")
				})
				Eventually(clusterUserDefinedNetworkReadyFunc(context.TODO(), f.DynamicClient, name), 60*time.Second, time.Second).
					Should(Succeed(), "CUDN %s must become ready", name)
				return name
			}

			// createNamespace creates a namespace the way the framework creates
			// its own. The framework deletes the namespace after every spec; the CUDN
			// cleanups below wait for that deletion before deleting their network.
			createNamespace := func(labels map[string]string) *v1.Namespace {
				ns, err := f.CreateNamespace(context.TODO(), f.BaseName, labels)
				Expect(err).NotTo(HaveOccurred(), "must create namespace")
				return ns
			}

			createPod := func(namespace, name string, attachments ...nadapi.NetworkSelectionElement) {
				e2epod.PodClientNS(f, namespace).CreateSync(context.TODO(), generatePodSpec(podConfiguration{
					name:         name,
					namespace:    namespace,
					containerCmd: []string{"pause"},
					nodeSelector: map[string]string{noFloodHostnameKey: trafficNode.Name},
					isPrivileged: true,
					attachments:  attachments,
				}))
			}

			BeforeEach(func() {
				cs = f.ClientSet
				if !deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.UDNARPNDPFloodDisabled) {
					Skip("requires ovnkube-node to run with --disable-udn-arp-ndp-flood (DISABLE_UDN_ARP_NDP_FLOOD=true)")
				}
				// Appended to below: start from scratch, not from the previous spec.
				extAIPs, extBIPs, cudnNames = nil, nil, nil
				toleratedHolders = sets.New[string]()

				By("selecting the node that will carry all the test traffic")
				nodes, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
				framework.ExpectNoError(err)
				Expect(nodes.Items).NotTo(BeEmpty(), "need at least one schedulable node")
				trafficNode = nodes.Items[0]
				otherNodes = nodes.Items[1:]
				ovnkPod = ovnkPodOnNode(trafficNode.Name)

				By("asserting the gateway routers ship with the expected MAC binding lifetime")
				Expect(macBindingAgeThresholds(ovnkPod)).
					To(HaveKeyWithValue(ovntypes.GWRouterPrefix+trafficNode.Name, ovntypes.GRMACBindingAgeThreshold),
						"ovn-kubernetes must set mac_binding_age_threshold=%s on the gateway routers. "+
							"If it reads %d, a previous run of these specs was killed before its cleanup could "+
							"restore it: run `ovn-nbctl set logical_router %s options:mac_binding_age_threshold=%s` "+
							"in the ovnkube-node pod on %s, or rebuild the cluster",
						ovntypes.GRMACBindingAgeThreshold, noFloodTestAgeThreshold,
						ovntypes.GWRouterPrefix+trafficNode.Name, ovntypes.GRMACBindingAgeThreshold, trafficNode.Name)

				By("creating the two external containers")
				providerCtx = infraprovider.Get().NewTestContext()
				providerNetwork, err := infraprovider.Get().PrimaryNetwork()
				framework.ExpectNoError(err, "provider primary network must be available")
				for i, target := range []*infraapi.ExternalContainer{&extA, &extB} {
					port := infraprovider.Get().GetExternalContainerPort()
					container, err := providerCtx.CreateExternalContainer(infraapi.ExternalContainer{
						Name:    fmt.Sprintf("noflood-ext-%d-%s", i, rand.String(5)),
						Image:   images.AgnHost(),
						Network: providerNetwork,
						CmdArgs: httpServerContainerCmd(port),
						ExtPort: port,
					})
					framework.ExpectNoError(err, "must create external container %d", i)
					*target = container
				}
				extAMAC, extBMAC = externalContainerMAC(extA), externalContainerMAC(extB)
				for _, spec := range []struct {
					c   infraapi.ExternalContainer
					ips *[]string
				}{{extA, &extAIPs}, {extB, &extBIPs}} {
					if isIPv4Supported(cs) {
						Expect(spec.c.GetIPv4()).NotTo(BeEmpty())
						*spec.ips = append(*spec.ips, spec.c.GetIPv4())
					}
					if isIPv6Supported(cs) {
						Expect(spec.c.GetIPv6()).NotTo(BeEmpty())
						*spec.ips = append(*spec.ips, spec.c.GetIPv6())
					}
				}
				Expect(extAIPs).NotTo(BeEmpty(), "no supported IP family")
				framework.Logf("external container A: ips=%v mac=%s", extAIPs, extAMAC)
				framework.Logf("external container B: ips=%v mac=%s", extBIPs, extBMAC)
				extIPs := append(append([]string{}, extAIPs...), extBIPs...)
				purgeDynamicMACBindings(ovnkPod, extIPs)
				for i := range otherNodes {
					purgeDynamicMACBindings(ovnkPodOnNode(otherNodes[i].Name), extIPs)
				}
				By("using the framework's own namespace for the CDN and creating its client pod")
				cdnNamespace = f.Namespace.Name
				// The framework deletes its namespaces after every spec without
				// waiting for them to go away; wait here so the CDN pod - and any
				// ping it still runs - is gone before the external containers are
				// deleted and their IPs reused by the next spec.
				DeferCleanup(func() {
					deleteNamespaceAndWait(cdnNamespace)
				})
				createPod(cdnNamespace, cdnPodName)

				By("creating three primary CUDNs, one layer3 and two layer2")
				// Four non overlapping subnet pairs: three for the CUDNs created
				// here and a fourth for the late joining CUDN.
				v4, v6 := allocators.GetNthFirstUDNSubnets(4)
				topologies := []struct {
					topology string
					cidr     string
				}{
					{"layer3", joinStrings(v4[0]+"/24", v6[0]+"/64")},
					{"layer2", joinStrings(v4[1], v6[1])},
					{"layer2", joinStrings(v4[2], v6[2])},
				}
				for i, t := range topologies {
					ns := createNamespace(map[string]string{
						"e2e-framework":           f.BaseName,
						RequiredUDNNamespaceLabel: "",
					}).Name
					name := createCUDN(ns, t.topology, t.cidr, "primary")
					cudnNames = append(cudnNames, name)
					// Every CUDN gets a pod on the traffic node so that its
					// topology - and therefore its rtoe- port - exists there even
					// with dynamic UDN allocation enabled. The first one doubles as
					// the pinging UDN client.
					podName := fmt.Sprintf("noflood-udn-pause-%d", i)
					if i == 0 {
						podName = udnPodName
						udnNamespace = ns
					}
					createPod(ns, podName)
				}

				expectedPorts = []string{defaultGatewayRouterExtPortName(trafficNode.Name)}
				for _, name := range cudnNames {
					expectedPorts = append(expectedPorts, cudnGatewayRouterExtPortName(name, trafficNode.Name))
				}
				framework.Logf("expecting MAC bindings on: %v", expectedPorts)

				By("shortening the MAC binding lifetime on those gateway routers for the duration of this spec")
				withTestMACBindingAgeThreshold(&ovnkPod, func() []string { return gatewayRoutersOf(expectedPorts) })

				cdnStaticIPs = staticMACBindingIPsOn(defaultGatewayRouterExtPortName(trafficNode.Name))
				framework.Logf("CDN gateway router static bindings at rest: %v", cdnStaticIPs)

				// Safety net for the specs that leave their pings running on
				// purpose and for the failure paths: no ping may outlive its spec,
				// or it could keep learning into the next spec's external
				// containers, which may reuse the same IPs. Registered last so it
				// runs before every other cleanup of the spec.
				DeferCleanup(func() {
					stopBackgroundPings(cdnNamespace, cdnPodName)
					stopBackgroundPings(udnNamespace, udnPodName)
				})
			})

			It("has no MAC bindings for the external containers before any traffic", func() {
				// IPv4 only: an IPv6 container announces itself on the shared
				// link as soon as it comes up (DAD / unsolicited NA), so the CDN
				// gateway router learns it - and the controller mirrors it -
				// before any test traffic exists. IPv4 has no such chatter.
				if !isIPv4Supported(cs) {
					Skip("baseline requires IPv4: IPv6 neighbour discovery populates the bindings on container start")
				}
				static, err := listStaticMACBindings(ovnkPod)
				Expect(err).NotTo(HaveOccurred())
				for _, ip := range []string{extA.GetIPv4(), extB.GetIPv4()} {
					got, err := macBindingPorts(ip)
					Expect(err).NotTo(HaveOccurred(), "must list MAC_Bindings for %s", ip)
					Expect(got).To(BeEmpty(),
						"no MAC_Binding must exist for untouched external container IP %s", ip)
					Expect(macBindingPortsForIP(static, ip)).To(BeEmpty(),
						"no Static_MAC_Binding must exist for external container IP %s", ip)
				}
			})

			It("mirrors the binding learned by the CDN gateway router to every UDN", func() {
				By("starting a sustained ping from the CDN pod to external container A")
				for _, ip := range extAIPs {
					startBackgroundPing(cdnNamespace, cdnPodName, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extAIPs {
					By(fmt.Sprintf("asserting %s is bound to %s on all %d gateway routers", ip, extAMAC, len(expectedPorts)))
					Eventually(func(g Gomega) {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}, 90*time.Second, 2*time.Second).Should(Succeed())
				}

				By("creating a primary UDN outside the fixture and asserting the mirror reaches it too")
				// The controller's follower set is open world by design: every
				// primary network's gateway router on the node is mirrored to, not
				// just the fixture's. Pin that contract with a deliberate foreign
				// NAD-backed UDN - the shape a leftover from another suite or a
				// real workload takes - whose pod stays quiet: its gateway router
				// must still be backfilled with the binding already learned by the
				// CDN gateway router, and with the correct MAC.
				foreignNs := createNamespace(map[string]string{
					"e2e-framework":           f.BaseName,
					RequiredUDNNamespaceLabel: "",
				})
				nadClient, err := nadclient.NewForConfig(f.ClientConfig())
				Expect(err).NotTo(HaveOccurred())
				v4, v6 := allocators.GetNthFirstUDNSubnets(5)
				foreignConfig := newNetworkAttachmentConfig(networkAttachmentConfigParams{
					name:      "noflood-foreign",
					namespace: foreignNs.Name,
					topology:  "layer2",
					cidr:      joinStrings(v4[4], v6[4]),
					role:      "primary",
				})
				_, err = nadClient.NetworkAttachmentDefinitions(foreignNs.Name).Create(context.TODO(),
					generateNAD(foreignConfig, f.ClientSet), metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred(), "must create the foreign primary UDN's NAD")
				// The NAD is namespaced: deleting the namespace takes the network
				// with it. Waited, so no trace reaches the next spec.
				DeferCleanup(func() {
					deleteNamespaceAndWait(foreignNs.Name)
				})
				createPod(foreignNs.Name, "noflood-foreign-client")
				// ovn-k sanitizes OVN entity names - the network name's hyphens become
				// dots - so locate the port through the same prefix the other specs
				// use, not the raw network name.
				foreignPrefix := util.GetUserDefinedNetworkPrefix(foreignConfig.networkName)
				for _, ip := range extAIPs {
					Eventually(func(g Gomega) {
						got, err := macBindingPorts(ip)
						g.Expect(err).NotTo(HaveOccurred(), "must list MAC_Bindings for %s", ip)
						var foreign string
						for port := range got {
							if strings.Contains(port, foreignPrefix) {
								foreign = port
							}
						}
						g.Expect(foreign).NotTo(BeEmpty(),
							"the foreign primary UDN %s must hold the mirrored binding for %s", foreignConfig.networkName, ip)
						g.Expect(got[foreign]).To(Equal(extAMAC),
							"the foreign primary UDN's binding for %s must carry MAC %s", ip, extAMAC)
					}, 120*time.Second, 5*time.Second).Should(Succeed())
				}
			})

			It("mirrors a binding triggered by a UDN pod to the CDN and every other UDN", func() {
				// The UDN gateway router sends the ARP/NS itself, but with flooding
				// disabled the reply only reaches the CDN gateway router. The UDN
				// can therefore only obtain the binding through the controller.
				By("starting a sustained ping from the UDN pod to external container B")
				for _, ip := range extBIPs {
					startBackgroundPing(udnNamespace, udnPodName, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extBIPs {
					By(fmt.Sprintf("asserting %s is bound to %s on all %d gateway routers", ip, extBMAC, len(expectedPorts)))
					Eventually(func(g Gomega) {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}, 90*time.Second, 2*time.Second).Should(Succeed())
				}
			})

			It("binds node IPs statically on the UDNs instead of mirroring them", func() {
				gwCfg := noFloodGatewayConfigOf(&trafficNode)
				var extSubnets []*net.IPNet
				for _, addr := range gwCfg.IPAddresses {
					_, subnet, err := net.ParseCIDR(addr)
					Expect(err).NotTo(HaveOccurred(), "l3-gateway-config ip-address %q must parse", addr)
					extSubnets = append(extSubnets, subnet)
				}

				By("collecting the node IPs that are on-link on the traffic node's external subnets")
				nodes, err := cs.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
				framework.ExpectNoError(err)
				// node IP -> expected MAC (the owning node's gateway MAC)
				onLinkNodeIPs := map[string]string{}
				for i := range nodes.Items {
					node := &nodes.Items[i]
					mac := strings.ToLower(noFloodGatewayConfigOf(node).MACAddress)
					for _, addr := range node.Status.Addresses {
						if addr.Type != v1.NodeInternalIP {
							continue
						}
						ip := net.ParseIP(addr.Address)
						for _, subnet := range extSubnets {
							if subnet.Contains(ip) {
								onLinkNodeIPs[addr.Address] = mac
							}
						}
					}
				}
				Expect(onLinkNodeIPs).NotTo(BeEmpty(), "expected at least one on-link node IP")
				framework.Logf("on-link node IPs: %v", onLinkNodeIPs)

				cdnPort := defaultGatewayRouterExtPortName(trafficNode.Name)
				udnPorts := expectedPorts[1:]

				By("asserting every UDN external port has a static binding for every on-link node IP")
				Eventually(func(g Gomega) {
					static, err := listStaticMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					for ip, mac := range onLinkNodeIPs {
						got := map[string]string{}
						for _, row := range static {
							if row.ip != ip {
								continue
							}
							g.Expect(row.overrideDynamicMAC).To(BeTrue(),
								"static binding for %s on %s must set override_dynamic_mac", ip, row.logicalPort)
							got[row.logicalPort] = row.mac
						}
						g.Expect(macBindingPortNames(got)).To(ContainElements(udnPorts),
							"node IP %s must be statically bound on exactly the UDN ports %v, got %v", ip, udnPorts, got)
						for port, gotMAC := range got {
							g.Expect(gotMAC).To(Equal(mac), "static binding for %s on %s must carry MAC %s", ip, port, mac)
						}
					}
				}, 120*time.Second, 5*time.Second).Should(Succeed())

				By("asserting the CDN external port only carries the gateway masquerade bindings")
				static, err := listStaticMACBindings(ovnkPod)
				Expect(err).NotTo(HaveOccurred())
				for _, row := range static {
					if row.logicalPort != cdnPort {
						continue
					}
					Expect(onLinkNodeIPs).NotTo(HaveKey(row.ip),
						"the CDN gateway router must never be a static binding target, found %s on %s", row.ip, cdnPort)
				}

				By("asserting node IPs are not mirrored as dynamic bindings on the UDNs")
				dynamic, err := listDynamicMACBindings(ovnkPod)
				Expect(err).NotTo(HaveOccurred())
				for _, row := range dynamic {
					if _, isNodeIP := onLinkNodeIPs[row.ip]; !isNodeIP {
						continue
					}
					Expect(udnPorts).NotTo(ContainElement(row.logicalPort),
						"node IP %s must not be dynamically mirrored onto UDN port %s", row.ip, row.logicalPort)
				}
			})

			It("never adds a static MAC binding to the default network gateway router", func() {
				By("starting a sustained ping from the CDN pod to A and from the UDN pod to B")
				for _, ip := range extAIPs {
					startBackgroundPing(cdnNamespace, cdnPodName, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extBIPs {
					startBackgroundPing(udnNamespace, udnPodName, ip, noFloodPingPidFile(ip))
				}
				Eventually(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}
				}, 90*time.Second, 2*time.Second).Should(Succeed())

				// The controller owns the UDN external ports only; the CDN port
				// must keep exactly the masquerade bindings the gateway itself
				// programs (config.IsGatewayStaticMACBindingIP).
				got := staticMACBindingIPsOn(defaultGatewayRouterExtPortName(trafficNode.Name))
				Expect(got).To(ConsistOf(cdnStaticIPs),
					"the static bindings of the CDN gateway router must not change while the controller runs")
				for _, ip := range append(append([]string{}, extAIPs...), extBIPs...) {
					Expect(got).NotTo(ContainElement(ip),
						"external container IP %s must never be statically bound on the CDN gateway router", ip)
				}
			})

			It("does not mirror onto a secondary CUDN", func() {
				// shouldTrackNetwork only accepts the default network and primary
				// layer2/layer3 UDNs, so a secondary network must never become a
				// mirroring follower.
				By("starting a sustained ping from the CDN pod to A and from the UDN pod to B")
				for _, ip := range extAIPs {
					startBackgroundPing(cdnNamespace, cdnPodName, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extBIPs {
					startBackgroundPing(udnNamespace, udnPodName, ip, noFloodPingPidFile(ip))
				}
				Eventually(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}
				}, 90*time.Second, 2*time.Second).Should(Succeed())

				v4, v6 := allocators.GetNthFirstUDNSubnets(5)
				ns := createNamespace(map[string]string{"e2e-framework": f.BaseName}).Name
				secName := createCUDN(ns, "layer2", joinStrings(v4[4], v6[4]), "secondary")
				createPod(ns, "noflood-secondary", nadapi.NetworkSelectionElement{Name: secName, Namespace: ns})
				secPrefix := util.GetUserDefinedNetworkPrefix(ovntypes.CUDNPrefix + secName)
				testIPs := append(append([]string{}, extAIPs...), extBIPs...)

				Consistently(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}
					static, err := listStaticMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					for _, row := range static {
						g.Expect(row.logicalPort).NotTo(ContainSubstring(secPrefix),
							"the secondary network must not get static MAC bindings either")
					}
					// A secondary network is not a legitimate follower, so exclude it
					// explicitly.
					dynamic, err := listDynamicMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					for _, row := range dynamic {
						if strings.Contains(row.logicalPort, secPrefix) {
							g.Expect(row.ip).NotTo(BeElementOf(testIPs),
								"the secondary network must not get dynamic MAC bindings for the test IPs either")
						}
					}
				}, 30*time.Second, 5*time.Second).Should(Succeed())
			})

			It("does not leak the bindings into the other nodes' zones", func() {
				// IPv4 only, for the same reason the baseline spec is: an IPv6
				// container announces itself on the shared link, so every node's
				// CDN gateway router legitimately learns it and mirrors it within
				// its own zone. Nothing makes another node resolve the container's
				// IPv4 address - always_learn_from_arp_request is false, so the
				// broadcast ARP requests this test generates teach nobody.
				if len(otherNodes) == 0 {
					Skip("needs more than one schedulable node")
				}
				if !isIPv4Supported(cs) {
					Skip("cross-zone isolation is only asserted for IPv4")
				}
				By("starting a sustained ping from the CDN pod to A and from the UDN pod to B")
				for _, ip := range extAIPs {
					startBackgroundPing(cdnNamespace, cdnPodName, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extBIPs {
					startBackgroundPing(udnNamespace, udnPodName, ip, noFloodPingPidFile(ip))
				}
				Eventually(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}
				}, 90*time.Second, 2*time.Second).Should(Succeed())

				for i := range otherNodes {
					node := &otherNodes[i]
					rows, err := listDynamicMACBindings(ovnkPodOnNode(node.Name))
					Expect(err).NotTo(HaveOccurred())
					for _, ip := range []string{extA.GetIPv4(), extB.GetIPv4()} {
						Expect(macBindingPortsForIP(rows, ip)).To(BeEmpty(),
							"the controller is zone local: node %s must hold no MAC binding for %s", node.Name, ip)
					}
				}
			})

			It("keeps the mirrored bindings stable while the traffic lasts", func() {
				By("starting a sustained ping from the CDN pod to A and from the UDN pod to B")
				for _, ip := range extAIPs {
					startBackgroundPing(cdnNamespace, cdnPodName, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extBIPs {
					startBackgroundPing(udnNamespace, udnPodName, ip, noFloodPingPidFile(ip))
				}
				Eventually(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}
				}, 90*time.Second, 2*time.Second).Should(Succeed())

				// OVN refreshes a binding's timestamp inside the last 3/16 of the
				// age threshold, i.e. roughly every 22s here, so this window both
				// proves the controller does not churn or prematurely drop rows
				// and puts the refresh-mirror path through several cycles.
				shortenAging()
				Consistently(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}
				}, noFloodStabilityDuration, 10*time.Second).Should(Succeed())
			})

			It("backfills a CUDN created after the bindings were learned, and reaps it on delete", func() {
				By("starting a sustained ping from the CDN pod to A and from the UDN pod to B")
				for _, ip := range extAIPs {
					startBackgroundPing(cdnNamespace, cdnPodName, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extBIPs {
					startBackgroundPing(udnNamespace, udnPodName, ip, noFloodPingPidFile(ip))
				}
				Eventually(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}
				}, 90*time.Second, 2*time.Second).Should(Succeed())

				v4, v6 := allocators.GetNthFirstUDNSubnets(4)
				ns := createNamespace(map[string]string{
					"e2e-framework":           f.BaseName,
					RequiredUDNNamespaceLabel: "",
				}).Name
				lateName := createCUDN(ns, "layer3", joinStrings(v4[3]+"/24", v6[3]+"/64"), "primary")
				createPod(ns, "noflood-udn-late")
				latePort := cudnGatewayRouterExtPortName(lateName, trafficNode.Name)
				withLate := append(append([]string{}, expectedPorts...), latePort)

				By("asserting the late CUDN is backfilled with the already learned bindings")
				for _, spec := range []struct {
					ips []string
					mac string
				}{{extAIPs, extAMAC}, {extBIPs, extBMAC}} {
					for _, ip := range spec.ips {
						Eventually(func(g Gomega) {
							expectMirroredEverywhere(g, ip, spec.mac, withLate)
						}, 120*time.Second, 5*time.Second).Should(Succeed())
					}
				}

				By("deleting the late CUDN and asserting its bindings are reaped")
				deleteNamespaceAndWait(ns)
				_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", lateName, "--wait", "--timeout=120s")
				Expect(err).NotTo(HaveOccurred())

				for _, spec := range []struct {
					ips []string
					mac string
				}{{extAIPs, extAMAC}, {extBIPs, extBMAC}} {
					for _, ip := range spec.ips {
						Eventually(func(g Gomega) {
							expectMirroredEverywhere(g, ip, spec.mac, expectedPorts)
							got, err := macBindingPorts(ip)
							g.Expect(err).NotTo(HaveOccurred(), "must list MAC_Bindings for %s", ip)
							g.Expect(got).NotTo(HaveKey(latePort),
								"the deleted CUDN's binding for %s must be reaped from %s", ip, latePort)
						}, 120*time.Second, 5*time.Second).Should(Succeed())
					}
				}
				Eventually(func(g Gomega) {
					static, err := listStaticMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					for _, row := range static {
						g.Expect(row.logicalPort).NotTo(Equal(latePort),
							"static bindings of the deleted CUDN must be reaped")
					}
				}, 120*time.Second, 5*time.Second).Should(Succeed())
			})

			It("reconverges after ovnkube-node restarts, without leaving orphan static bindings", func() {
				By("starting a sustained ping from the CDN pod to A and from the UDN pod to B")
				for _, ip := range extAIPs {
					startBackgroundPing(cdnNamespace, cdnPodName, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extBIPs {
					startBackgroundPing(udnNamespace, udnPodName, ip, noFloodPingPidFile(ip))
				}
				Eventually(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}
				}, 90*time.Second, 2*time.Second).Should(Succeed())

				framework.ExpectNoError(restartOVNKubeNodePod(cs, deploymentconfig.Get().OVNKubernetesNamespace(), trafficNode.Name))
				// The pod name changed: the rest of this spec, and the cleanup
				// restoring the MAC binding lifetime, read the databases through it.
				ovnkPod = ovnkPodOnNode(trafficNode.Name)
				// gatewayInit ran again as the pod came back, so the gateway
				// routers are on the shipped lifetime once more.
				shortenAging()

				By("asserting the mirrored bindings come back on every gateway router")
				for _, spec := range []struct {
					ips []string
					mac string
				}{{extAIPs, extAMAC}, {extBIPs, extBMAC}} {
					for _, ip := range spec.ips {
						Eventually(func(g Gomega) {
							expectMirroredEverywhere(g, ip, spec.mac, expectedPorts)
						}, noFloodReconvergeTimeout, 5*time.Second).Should(Succeed())
					}
				}

				By("asserting repairStaticMacBindings left no orphans behind")
				Eventually(func(g Gomega) {
					// The live port inventory, not the fixture's expected set: a
					// foreign primary UDN's static rows are as legitimate as its
					// mirrored dynamic ones, and only a row surviving on a port
					// that no longer exists is an orphan.
					live, err := gatewayRouterExtPorts(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred(), "must list the zone's gateway router ports")
					static, err := listStaticMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					for _, row := range static {
						g.Expect(live).To(ContainElement(row.logicalPort),
							"static binding for %s survives on %s, which is not a live gateway router port", row.ip, row.logicalPort)
					}
				}, noFloodReconvergeTimeout, 5*time.Second).Should(Succeed())
			})

			It("converges every gateway router onto the new MAC when the external container changes it", func() {
				// The ping keeps running so the gateway routers keep probing A.
				By("starting a sustained ping from the CDN pod to external container A")
				for _, ip := range extAIPs {
					startBackgroundPing(cdnNamespace, cdnPodName, ip, noFloodPingPidFile(ip))
				}
				Eventually(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}
				}, 90*time.Second, 2*time.Second).Should(Succeed())

				shortenAging()
				iface := infraprovider.Get().ExternalContainerPrimaryInterfaceName()
				newMAC := "02:00:00:00:00:a1"
				// Changed in place: bouncing the link would flush the container's
				// global IPv6 address and the test would then be chasing a
				// neighbour that no longer answers.
				_, err := infraprovider.Get().ExecExternalContainerCommand(extA,
					[]string{"ip", "link", "set", "dev", iface, "address", newMAC})
				Expect(err).NotTo(HaveOccurred(), "must change the MAC of external container A")
				Expect(externalContainerMAC(extA)).To(Equal(newMAC))
				extAMAC = newMAC

				// The gateway routers do not learn from the gratuitous ARP - they
				// run with always_learn_from_arp_request=false - so they pick the
				// new MAC up on their next refresh probe, at most one lifetime
				// away, and the controller has to update the followers in place
				// rather than add a second row.
				for _, ip := range extAIPs {
					By(fmt.Sprintf("asserting %s converges to %s on all %d gateway routers", ip, newMAC, len(expectedPorts)))
					Eventually(func(g Gomega) {
						expectMirroredEverywhere(g, ip, newMAC, expectedPorts)
					}, noFloodMACChangeTimeout, 5*time.Second).Should(Succeed())
				}
			})

			It("ages out the CDN-triggered binding on every gateway router once traffic stops", func() {
				By("starting a sustained ping from the CDN pod to external container A")
				for _, ip := range extAIPs {
					startBackgroundPing(cdnNamespace, cdnPodName, ip, noFloodPingPidFile(ip))
				}
				Eventually(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}
				}, 90*time.Second, 2*time.Second).Should(Succeed())

				shortenAging()

				By("stopping all background pings")
				stopBackgroundPings(cdnNamespace, cdnPodName)

				By("silencing external container A so it stops answering the refresh probe")
				silenceExternalContainer(extA)

				// The row set is zone-wide, foreign followers included: their mirror
				// rows carry the source's timestamp - the controller copies it - so
				// they age out with it once the traffic stops and the container
				// stops answering the refresh probe.
				for _, ip := range extAIPs {
					By(fmt.Sprintf("waiting for %s to age out of all gateway routers", ip))
					Eventually(func(g Gomega) {
						rows, err := listDynamicMACBindings(ovnkPod)
						g.Expect(err).NotTo(HaveOccurred(), "must list MAC_Bindings")
						g.Expect(macBindingPortsForIP(rows, ip)).To(BeEmpty(),
							"MAC_Bindings for %s must age out after mac_binding_age_threshold", ip)
					}, noFloodAgeOutTimeout, 10*time.Second).Should(Succeed())
				}
			})

			It("ages out the UDN-triggered binding on every gateway router once traffic stops", func() {
				By("starting a sustained ping from the UDN pod to external container B")
				for _, ip := range extBIPs {
					startBackgroundPing(udnNamespace, udnPodName, ip, noFloodPingPidFile(ip))
				}
				Eventually(func(g Gomega) {
					for _, ip := range extBIPs {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}
				}, 90*time.Second, 2*time.Second).Should(Succeed())

				shortenAging()

				By("stopping the background pings")
				stopBackgroundPings(udnNamespace, udnPodName)

				By("silencing external container B so it stops answering the refresh probe")
				silenceExternalContainer(extB)

				// Foreign followers age out with the source the same way: their
				// mirror rows carry the source's timestamp.
				for _, ip := range extBIPs {
					By(fmt.Sprintf("waiting for %s to age out of all gateway routers", ip))
					Eventually(func(g Gomega) {
						rows, err := listDynamicMACBindings(ovnkPod)
						g.Expect(err).NotTo(HaveOccurred(), "must list MAC_Bindings")
						g.Expect(macBindingPortsForIP(rows, ip)).To(BeEmpty(),
							"MAC_Bindings for %s must age out after mac_binding_age_threshold", ip)
					}, noFloodAgeOutTimeout, 10*time.Second).Should(Succeed())

				}
			})
		})
	})

// noFloodPingPidFile returns the pid file to use for a background ping to ip,
// one per IP family so both can run at the same time.
func noFloodPingPidFile(ip string) string {
	if utilnet.IsIPv6String(ip) {
		return noFloodPingPidFileV6
	}
	return noFloodPingPidFileV4
}

// macBindingPortNames returns the logical port names held by m.
func macBindingPortNames(m map[string]string) []string {
	names := slices.Collect(maps.Keys(m))
	return names
}

// On a dedicated uplink there is no default network to resolve ARP/NDP on
// behalf of the group. The openflow manager designates the first CUDN rendered
// on the uplink OVS bridge as the group's source: the priority 52 steering flows
// send every unicast ARP reply / NA / NS / RA addressed to the shared bridge MAC
// to that CUDN's patch port only, so only the source gateway router ever learns
// an external neighbour. The MACBindingController mirrors the source's SBDB
// MAC_Binding rows onto the other CUDNs of the uplink (the followers), which is
// the only way a follower can reach an external host.
//
// Every spec below builds its own fixture - uplink, CUDNs, external
// containers - and starts the traffic it asserts on, so each one can run on
// its own, selected by name, and in any order. The pings a spec starts are
// stopped by the deferred teardown, with a survivor check, so none outlives
// its spec and reaches the next spec's external containers, which may reuse
// the same IPs.

const (
	// How long the bindings are held under sustained traffic. Longer than the
	// ~22s OVN refresh interval of noFloodTestAgeThreshold, so the
	// refresh-mirror path runs several times.
	uplinkStabilityDuration = 60 * time.Second

	// Time allowed for a CUDN joining or leaving the uplink to be reflected in
	// the follower set and in the MAC_Binding table.
	uplinkMembershipTimeout = 120 * time.Second
)

// noFloodUplinkCUDN is one CUDN sharing the uplink, with the namespace and the
// client pod created for it and its gateway router external port.
type noFloodUplinkCUDN struct {
	name      string
	namespace string
	podName   string
	port      string
}

// noFloodPingPidFileForIP returns a pid file unique per target IP so one pod can
// ping several targets of the same family at once. It matches the glob used by
// stopBackgroundPings.
func noFloodPingPidFileForIP(ip string) string {
	return "/tmp/noflood-ping-" + strings.NewReplacer(":", "-", ".", "-").Replace(ip) + ".pid"
}

// noFloodPingOnce pings ip from the pod a few times and returns an error if no
// reply came back: the end-to-end proof that the gateway router of the pod's
// network holds a usable binding for ip.
func noFloodPingOnce(namespace, podName, ip string) error {
	ping := "ping"
	if utilnet.IsIPv6String(ip) {
		ping = "ping6"
	}
	_, err := e2ekubectl.RunKubectl(namespace, "exec", podName, "--", ping, "-c", "3", "-W", "3", ip)
	if err != nil {
		return fmt.Errorf("%s/%s cannot reach %s: %w", namespace, podName, ip, err)
	}
	return nil
}

var noFloodSteeringOutputRe = regexp.MustCompile(`actions=output:(\d+),NORMAL`)

// noFloodUplinkSourcePort returns the OVS interface the uplink bridge steers
// external ARP/NDP to, i.e. the patch port of the designated source CUDN. It
// also asserts the pre-no-flood table 0 fan-out flow (priority 10, dl_dst=bridge
// MAC) is gone, without which "mirrored" rows could just as well have been
// learned by every gateway router on its own.
func noFloodUplinkSourcePort(ovnkPod *v1.Pod, bridgeName string) (string, error) {
	out, err := e2epodoutput.RunHostCmd(ovnkPod.Namespace, ovnkPod.Name,
		fmt.Sprintf("ovs-ofctl --no-stats dump-flows %s table=0 | grep 'priority=52,'", bridgeName))
	if err != nil {
		return "", fmt.Errorf("no priority 52 ARP/NDP steering flows on %s yet: %w", bridgeName, err)
	}
	ofports := sets.New[string]()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		m := noFloodSteeringOutputRe.FindStringSubmatch(line)
		if m == nil {
			return "", fmt.Errorf("unexpected steering flow on %s: %q", bridgeName, line)
		}
		ofports.Insert(m[1])
	}
	if ofports.Len() != 1 {
		return "", fmt.Errorf("steering flows on %s must target a single port, got %v", bridgeName, ofports.UnsortedList())
	}
	ofport := ofports.UnsortedList()[0]
	out, err = e2epodoutput.RunHostCmd(ovnkPod.Namespace, ovnkPod.Name,
		fmt.Sprintf("ovs-ofctl --no-stats dump-flows %s table=0 | grep -c 'priority=10,.*dl_dst=' || true", bridgeName))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) != "0" {
		return "", fmt.Errorf("the ARP/NDP fan-out flow must not be rendered on %s with the flood disabled", bridgeName)
	}
	// ofport numbers are per bridge, so keep only the interface that the
	// bridge itself lists among the ones carrying that number.
	out, err = e2epodoutput.RunHostCmd(ovnkPod.Namespace, ovnkPod.Name,
		fmt.Sprintf("ovs-vsctl --timeout=15 --bare --columns=name find interface ofport=%s", ofport))
	if err != nil {
		return "", err
	}
	candidates := sets.New(strings.Fields(out)...)
	out, err = e2epodoutput.RunHostCmd(ovnkPod.Namespace, ovnkPod.Name,
		fmt.Sprintf("ovs-vsctl --timeout=15 list-ports %s", bridgeName))
	if err != nil {
		return "", err
	}
	matches := candidates.Intersection(sets.New(strings.Fields(out)...))
	if matches.Len() != 1 {
		return "", fmt.Errorf("ofport %s on bridge %s resolves to %v, expected one port", ofport, bridgeName, matches.UnsortedList())
	}
	return matches.UnsortedList()[0], nil
}

// ipInAnyCIDR reports whether ip falls inside any of the given CIDR strings.
func ipInAnyCIDR(ip string, cidrs []string) bool {
	parsed := net.ParseIP(ip)
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err == nil && ipNet.Contains(parsed) {
			return true
		}
	}
	return false
}

var _ = Describe("Network Segmentation: MAC binding mirroring for multiple UDNs on same uplink",
	feature.NetworkSegmentation, feature.Uplink, func() {

		f := wrappedTestFramework("noflood-uplink")
		// The suite has no use for the framework's own default-network
		// namespace: every pod lives in a member namespace, created through
		// f.CreateNamespace in addMember, which the framework deletes after
		// every spec all the same.
		f.SkipNamespaceCreation = true

		Context("", Serial, func() {

			var (
				cs          kubernetes.Interface
				ictx        infraapi.Context
				ipFamilySet sets.Set[utilnet.IPFamily]

				trafficNode      v1.Node
				ovnkPod          *v1.Pod
				uplinkName       string
				bridgeName       string
				uplinkNetwork    infraapi.Network
				extA, extB       infraapi.ExternalContainer
				extAMAC, extBMAC string
				extAIPs, extBIPs []string

				members []noFloodUplinkCUDN
				source  noFloodUplinkCUDN
				// pings records the background pings started so far as
				// "namespace/pod/ip", so a spec never starts the same one twice
				// and orphans the first.
				pings = sets.New[string]()
			)

			memberPorts := func() []string {
				ports := make([]string, 0, len(members))
				for _, m := range members {
					ports = append(ports, m.port)
				}
				return ports
			}

			// shortenAging re-applies the test lifetime to the member gateway
			// routers only. The uplink group's bindings live on those and nowhere
			// else - the specs below assert exactly that - so the default network
			// and everything else sharing the cluster keep the shipped lifetime.
			shortenAging := func() {
				err := setMACBindingAgeThreshold(ovnkPod, gatewayRoutersOf(memberPorts()), noFloodTestAgeThreshold)
				Expect(err).NotTo(HaveOccurred(), "must set mac_binding_age_threshold=%d on %v", noFloodTestAgeThreshold, gatewayRoutersOf(memberPorts()))
			}

			followers := func() []noFloodUplinkCUDN {
				var out []noFloodUplinkCUDN
				for _, m := range members {
					if m.name != source.name {
						out = append(out, m)
					}
				}
				return out
			}

			macBindingPorts := func(ip string) map[string]string {
				rows, err := listDynamicMACBindings(ovnkPod)
				if err != nil {
					framework.Logf("failed to list MAC_Bindings: %v", err)
					return nil
				}
				return macBindingPortsForIP(rows, ip)
			}

			// expectBoundOnExactly asserts ip is bound to mac on exactly ports:
			// every member of the group and nothing else in the zone.
			expectBoundOnExactly := func(g Gomega, ip, mac string, ports []string) {
				got := macBindingPorts(ip)
				g.Expect(macBindingPortNames(got)).To(ConsistOf(ports), "MAC_Bindings for %s must be on exactly %v, got %v", ip, ports, got)
				for port, gotMAC := range got {
					g.Expect(gotMAC).To(Equal(mac), "MAC_Binding for %s on %s must carry MAC %s", ip, port, mac)
				}
			}

			expectContainerBoundEverywhere := func(g Gomega, ips []string, mac string) {
				for _, ip := range ips {
					expectBoundOnExactly(g, ip, mac, memberPorts())
				}
			}

			ensureBackgroundPing := func(m noFloodUplinkCUDN, ip string) {
				key := m.namespace + "/" + m.podName + "/" + ip
				if pings.Has(key) {
					return
				}
				// One deferred stop per started ping, not per pod: duplicates are
				// idempotent - the first kills the pings, the rest count zero
				// survivors. A pod already gone with its member's namespace
				// counts as stopped.
				DeferCleanup(func() {
					if _, err := cs.CoreV1().Pods(m.namespace).Get(context.TODO(), m.podName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
						return
					}
					stopBackgroundPings(m.namespace, m.podName)
				})
				startBackgroundPing(m.namespace, m.podName, ip, noFloodPingPidFileForIP(ip))
				pings.Insert(key)
			}

			expectReachable := func(m noFloodUplinkCUDN, ips []string) {
				for _, ip := range ips {
					Eventually(func() error {
						return noFloodPingOnce(m.namespace, m.podName, ip)
					}, 60*time.Second, 5*time.Second).Should(Succeed(), "CUDN %s must reach %s", m.name, ip)
				}
			}

			// sourceCUDN resolves the steering flows of the uplink bridge on the
			// traffic node to one of the members.
			sourceCUDN := func() (noFloodUplinkCUDN, error) {
				patch, err := noFloodUplinkSourcePort(ovnkPod, bridgeName)
				if err != nil {
					return noFloodUplinkCUDN{}, err
				}
				for _, m := range members {
					scoped := util.GetUserDefinedNetworkPrefix(ovntypes.CUDNPrefix+m.name) + trafficNode.Name
					if patch == util.GetPatchPortName(bridgeName, scoped) {
						return m, nil
					}
				}
				return noFloodUplinkCUDN{}, fmt.Errorf("steering target %q is not the patch port of a member CUDN", patch)
			}

			// startGroupTraffic starts the sustained pings the specs assert on -
			// container A pinged from the source CUDN's pod, container B from a
			// follower's - and does not return until both are bound on every
			// member, the state the specs below build their assertions on.
			startGroupTraffic := func() {
				for _, ip := range extAIPs {
					ensureBackgroundPing(source, ip)
				}
				for _, ip := range extBIPs {
					ensureBackgroundPing(followers()[0], ip)
				}
				Eventually(func(g Gomega) {
					expectContainerBoundEverywhere(g, extAIPs, extAMAC)
					expectContainerBoundEverywhere(g, extBIPs, extBMAC)
				}, 90*time.Second, 2*time.Second).Should(Succeed())
			}

			createExternalContainer := func(name string) (infraapi.ExternalContainer, string, []string) {
				port := infraprovider.Get().GetExternalContainerPort()
				container, err := ictx.CreateExternalContainer(infraapi.ExternalContainer{
					Name:    fmt.Sprintf("%s-%s", name, framework.RandomSuffix()),
					Image:   images.AgnHost(),
					Network: uplinkNetwork,
					CmdArgs: httpServerContainerCmd(port),
					ExtPort: port,
				})
				framework.ExpectNoError(err, "must create external container %s", name)
				var ips []string
				if isIPv4Supported(cs) {
					Expect(container.GetIPv4()).NotTo(BeEmpty())
					ips = append(ips, container.GetIPv4())
				}
				if isIPv6Supported(cs) {
					Expect(container.GetIPv6()).NotTo(BeEmpty())
					ips = append(ips, container.GetIPv6())
				}
				mac := externalContainerMAC(container)
				framework.Logf("external container %s: ips=%v mac=%s", container.GetName(), ips, mac)
				return container, mac, ips
			}

			deleteNamespaceAndWait := func(name string) {
				err := cs.CoreV1().Namespaces().Delete(context.TODO(), name, metav1.DeleteOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					framework.ExpectNoError(err, "must delete namespace %s", name)
				}
				Eventually(func() bool {
					_, err := cs.CoreV1().Namespaces().Get(context.TODO(), name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, uplinkMembershipTimeout, 2*time.Second).Should(BeTrue(), "namespace %s must go away", name)
			}

			// addMember creates a primary layer3 CUDN bound to the uplink, its
			// namespace and a client pod on the traffic node, and tracks it.
			addMember := func(ctx context.Context, name string) noFloodUplinkCUDN {
				// f.CreateNamespace gives the namespace the framework's pod
				// security labels - privileged for wrappedTestFramework, which
				// the client pods need - and hands it to the framework, which
				// deletes it after every spec.
				ns, err := f.CreateNamespace(ctx, f.BaseName, map[string]string{
					"e2e-framework":           f.BaseName,
					RequiredUDNNamespaceLabel: "",
				})
				framework.ExpectNoError(err, "must create namespace for CUDN %s", name)
				// The member's removal is registered as soon as the namespace
				// exists, before the allocation, CUDN and pod setup - each of
				// those can fail after creating its resource, and their cleanup
				// must not be skipped. It is idempotent, so it also no-ops for a
				// member that removeMember already removed (the deletes tolerate
				// NotFound), and retries a removal that failed halfway. The order
				// matters: live pods using the NAD and the active network
				// controller can hold the CUDN's finalization, and the member
				// pods must be gone before the infra cleanups dismantle the
				// bridge and the external network. Registered here, after the
				// infra context, so LIFO runs it before those; waiting also hands
				// the framework an already-drained namespace to delete.
				DeferCleanup(func() {
					deleteNamespaceAndWait(ns.Name)
					_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", name,
						"--ignore-not-found=true", "--wait", "--timeout=120s")
					framework.ExpectNoError(err, "must delete CUDN %s", name)
				})
				alloc, err := allocators.AllocateBGP(f, ictx)
				framework.ExpectNoError(err)
				Expect(createUplinkCUDN(ctx, f, ictx, ns, name,
					uplinkLayer3NetworkSpec(ipFamilySet, alloc.UDNSubnet, alloc.UDNSubnet6), nil, uplinkName)).To(Succeed())
				m := noFloodUplinkCUDN{
					name:      name,
					namespace: ns.Name,
					podName:   name + "-client",
					port:      cudnGatewayRouterExtPortName(name, trafficNode.Name),
				}
				createUplinkNetexecPod(ctx, f, m.namespace, m.podName, trafficNode.Name)
				members = append(members, m)
				return m
			}

			// removeMember deletes the member's namespace (and so its pod and
			// any ping it ran), then the CUDN, and stops tracking it.
			removeMember := func(m noFloodUplinkCUDN) {
				deleteNamespaceAndWait(m.namespace)
				_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", m.name, "--wait", "--timeout=120s")
				Expect(err).NotTo(HaveOccurred(), "must delete CUDN %s", m.name)
				kept := members[:0]
				for _, o := range members {
					if o.name != m.name {
						kept = append(kept, o)
					}
				}
				members = kept
				for key := range pings {
					if strings.HasPrefix(key, m.namespace+"/") {
						pings.Delete(key)
					}
				}
			}

			BeforeEach(func(ctx SpecContext) {
				cs = f.ClientSet
				if !deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.UDNARPNDPFloodDisabled) {
					Skip("requires ovnkube-node to run with --disable-udn-arp-ndp-flood (DISABLE_UDN_ARP_NDP_FLOOD=true)")
				}
				if IsGatewayModeLocal(cs) {
					Skip("Uplink CUDN gateway plumbing is only supported in shared gateway mode")
				}
				if isDPUUplinkE2E() {
					Skip("the DPU Uplink e2e environment provisions its own uplink bridges")
				}
				ipFamilySet = sets.New(getSupportedIPFamiliesSlice(cs)...)

				// Start from scratch, not from the previous spec.
				members, source, pings = nil, noFloodUplinkCUDN{}, sets.New[string]()

				By("selecting the node that will carry all the test traffic")
				allNodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
				framework.ExpectNoError(err)
				schedulable, err := e2enode.GetReadySchedulableNodes(ctx, cs)
				framework.ExpectNoError(err)
				Expect(len(schedulable.Items)).To(BeNumerically(">=", 2), "uplink tests need at least 2 schedulable nodes")
				// Take the last schedulable node, away from the one the sibling
				// default-group suite pins its traffic to and restarts ovnkube-node
				// on, so this suite's zone databases are not the ones reconverging
				// after those restarts.
				trafficNode = schedulable.Items[len(schedulable.Items)-1]
				ovnkPods, err := cs.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(ctx, metav1.ListOptions{
					LabelSelector: "app=ovnkube-node",
					FieldSelector: "spec.nodeName=" + trafficNode.Name,
				})
				framework.ExpectNoError(err)
				Expect(ovnkPods.Items).To(HaveLen(1), "expected exactly one ovnkube-node pod on %s", trafficNode.Name)
				ovnkPod = &ovnkPods.Items[0]

				ictx = infraprovider.Get().NewTestContext()
				// The infra cleanups - CUDN and Uplink deletes, allocation
				// releases, bridge and external network teardown - run with the
				// DeferCleanup of this spec, in LIFO order against everything
				// registered after the context here. The framework registers its
				// own teardown even earlier, so f.ClientSet outlives all of them.

				By("setting up the uplink network, bridge and Uplink CR on every node")
				suffix := framework.RandomSuffix()
				uplinkAlloc, err := allocators.AllocateBGP(f, ictx)
				framework.ExpectNoError(err)
				var nodeIfaces map[string]infraapi.NetworkInterface
				uplinkNetwork, nodeIfaces = setupUplinkNetwork(ictx, allNodes.Items, ipFamilySet,
					"upnet"+suffix, []string{uplinkAlloc.BGPPeerSubnet, uplinkAlloc.BGPPeerSubnet6})
				bridgeName = uplinkBridgeName("upflood" + suffix)
				Expect(configureUplinkBridge(ctx, f, ictx, bridgeName, nodeIfaces)).To(Succeed())
				Expect(configureUplinkBridgeDefaultRoutes(ictx, bridgeName, nodeIfaces)).To(Succeed())
				uplinkName = "upflood" + suffix
				createUplink(ctx, f, ictx, uplinkName, allNodes.Items, nodeIfaces, bridgeName)
				waitForUplinkStatesResolved(ctx, f, uplinkName, bridgeName, allNodes.Items)
				waitForUplinkStatesDefaultGateways(ctx, f, uplinkName, allNodes.Items, ipFamilySet)

				By("creating the two external containers on the uplink network")
				extA, extAMAC, extAIPs = createExternalContainer("upflood-ext-a")
				extB, extBMAC, extBIPs = createExternalContainer("upflood-ext-b")
				// The provider recycles container IPs and dynamic rows only
				// go away by aging, so drop whatever a previous run left.
				purgeDynamicMACBindings(ovnkPod, append(append([]string{}, extAIPs...), extBIPs...))

				By("creating three primary CUDNs on the uplink, each with a client pod on the traffic node")
				for i := 1; i <= 3; i++ {
					addMember(ctx, fmt.Sprintf("upflood-cudn%d-%s", i, suffix))
				}
				framework.Logf("CUDN gateway router ports on the uplink: %v", memberPorts())

				By("resolving which CUDN gateway router is the ARP/NDP source of the uplink")
				// sourceCUDN only reads OVS state, so resolving here does not prime
				// the no-traffic baseline of the first spec.
				Eventually(func() error {
					var err error
					source, err = sourceCUDN()
					return err
				}, 60*time.Second, 2*time.Second).Should(Succeed())

				By("shortening the MAC binding lifetime on the member gateway routers for the duration of this spec")
				withTestMACBindingAgeThreshold(&ovnkPod, func() []string { return gatewayRoutersOf(memberPorts()) })
			})

			It("designates exactly one CUDN gateway router as the ARP/NDP source of the uplink", func() {
				// The fixture resolved and asserted the designation in BeforeEach:
				// sourceCUDN fails unless the uplink bridge steers ARP/NDP to a
				// single port, that port is one member's patch port, and the
				// pre-no-flood fan-out flow is gone. Here, restate it and assert
				// the follower set and the quiet baseline.
				Expect(followers()).To(HaveLen(len(members) - 1))
				framework.Logf("source: %s (%s), followers: %v", source.name, source.port, followers())

				By("asserting nothing is bound for the external containers before any traffic")
				if isIPv4Supported(cs) {
					// IPv6 containers announce themselves on the link as soon as
					// they come up, so only IPv4 is quiet until the first ping.
					for _, ip := range []string{extA.GetIPv4(), extB.GetIPv4()} {
						Expect(macBindingPorts(ip)).To(BeEmpty(), "no MAC_Binding must exist for untouched %s", ip)
					}
				}
			})

			It("mirrors a binding resolved by the source CUDN onto every follower", func() {
				By("starting a sustained ping from the source CUDN pod to external container A")
				for _, ip := range extAIPs {
					ensureBackgroundPing(source, ip)
				}
				By(fmt.Sprintf("asserting container A is bound to %s on the source and on every follower", extAMAC))
				Eventually(func(g Gomega) {
					expectContainerBoundEverywhere(g, extAIPs, extAMAC)
				}, 90*time.Second, 2*time.Second).Should(Succeed())

				By("asserting the followers can reach container A with the mirrored binding")
				for _, m := range followers() {
					expectReachable(m, extAIPs)
				}
			})

			It("mirrors a binding resolved on behalf of a follower CUDN onto the source and the other followers", func() {
				// The follower's gateway router sends the ARP/NS itself, but the
				// reply is steered to the source only. The follower can therefore
				// resolve container B exclusively through the controller.
				prober := followers()[0]
				By(fmt.Sprintf("starting a sustained ping from follower %s to external container B", prober.name))
				for _, ip := range extBIPs {
					ensureBackgroundPing(prober, ip)
				}
				By(fmt.Sprintf("asserting container B is bound to %s on the source and on every follower", extBMAC))
				Eventually(func(g Gomega) {
					expectContainerBoundEverywhere(g, extBIPs, extBMAC)
				}, 90*time.Second, 2*time.Second).Should(Succeed())

				By("asserting the prober and the other members can reach container B")
				for _, m := range members {
					expectReachable(m, extBIPs)
				}
			})

			It("keeps the uplink group's bindings stable and isolated while the traffic lasts", func() {
				By("starting sustained pings to both containers and waiting for the bindings")
				startGroupTraffic()

				shortenAging()
				cdnPort := defaultGatewayRouterExtPortName(trafficNode.Name)
				Consistently(func(g Gomega) {
					expectContainerBoundEverywhere(g, extAIPs, extAMAC)
					expectContainerBoundEverywhere(g, extBIPs, extBMAC)

					// What the default group resolves on breth0 must never be
					// mirrored onto the uplink group: the two share the zone's
					// SBDB but not an uplink.
					dynamic, err := listDynamicMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					defaultGroupIPs := sets.New[string]()
					for _, row := range dynamic {
						if row.logicalPort == cdnPort {
							defaultGroupIPs.Insert(row.ip)
						}
					}
					for _, row := range dynamic {
						for _, m := range members {
							if row.logicalPort == m.port {
								g.Expect(defaultGroupIPs).NotTo(HaveKey(row.ip),
									"default group binding for %s must not be mirrored onto uplink port %s", row.ip, m.port)
							}
						}
					}

					// Node IP static bindings are only managed for the default
					// uplink; on a dedicated uplink the only static rows are the
					// masquerade IPs every gateway router programs for itself.
					static, err := listStaticMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					for _, row := range static {
						if !sets.New(memberPorts()...).Has(row.logicalPort) {
							continue
						}
						g.Expect(ipInAnyCIDR(row.ip, uplinkMasqueradeCIDRs)).To(BeTrue(),
							"only masquerade IPs may be statically bound on dedicated uplink port %s, found %s", row.logicalPort, row.ip)
					}
				}, uplinkStabilityDuration, 10*time.Second).Should(Succeed())
			})

			It("backfills a CUDN that joins the uplink after the bindings were learned, and reaps it when it leaves", func(ctx SpecContext) {
				By("starting sustained pings to both containers and waiting for the bindings")
				startGroupTraffic()

				late := addMember(ctx, "upflood-cudn4-"+framework.RandomSuffix())

				By("asserting the late CUDN is backfilled with both containers' bindings")
				Eventually(func(g Gomega) {
					expectContainerBoundEverywhere(g, extAIPs, extAMAC)
					expectContainerBoundEverywhere(g, extBIPs, extBMAC)
				}, uplinkMembershipTimeout, 5*time.Second).Should(Succeed())

				By("asserting the late CUDN can reach both containers without resolving them itself")
				expectReachable(late, extAIPs)
				expectReachable(late, extBIPs)

				By("deleting the late CUDN and asserting the group shrinks back")
				removeMember(late)
				Eventually(func(g Gomega) {
					expectContainerBoundEverywhere(g, extAIPs, extAMAC)
					expectContainerBoundEverywhere(g, extBIPs, extBMAC)
				}, uplinkMembershipTimeout, 5*time.Second).Should(Succeed())
			})

			It("re-designates the source and keeps the followers resolved when the source CUDN leaves the uplink", func() {
				By("starting sustained pings to both containers and waiting for the bindings")
				startGroupTraffic()

				former := source
				By(fmt.Sprintf("deleting the source CUDN %s", former.name))
				removeMember(former)

				By("asserting another member becomes the source and the bindings survive on the remaining members")
				Eventually(func(g Gomega) {
					current, err := sourceCUDN()
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(current.name).NotTo(Equal(former.name))
					source = current
					expectContainerBoundEverywhere(g, extAIPs, extAMAC)
					expectContainerBoundEverywhere(g, extBIPs, extBMAC)
				}, uplinkMembershipTimeout, 5*time.Second).Should(Succeed())
				framework.Logf("new source: %s, followers: %v", source.name, followers())

				By("keeping traffic to both containers flowing from every remaining member")
				for _, m := range members {
					for _, ip := range append(append([]string{}, extAIPs...), extBIPs...) {
						ensureBackgroundPing(m, ip)
					}
					expectReachable(m, extAIPs)
					expectReachable(m, extBIPs)
				}

				By("asserting the new source resolves a neighbour on behalf of its follower")
				// Dropping container B's rows forces a fresh resolution. The
				// follower's own ARP/NS is still steered to the source only, so
				// the rows can only come back through the new source and the
				// controller.
				purgeDynamicMACBindings(ovnkPod, extBIPs)
				Eventually(func(g Gomega) {
					expectContainerBoundEverywhere(g, extBIPs, extBMAC)
				}, 90*time.Second, 2*time.Second).Should(Succeed())
				for _, m := range followers() {
					expectReachable(m, extBIPs)
				}

				Consistently(func(g Gomega) {
					expectContainerBoundEverywhere(g, extAIPs, extAMAC)
					expectContainerBoundEverywhere(g, extBIPs, extBMAC)
				}, 30*time.Second, 5*time.Second).Should(Succeed())
			})

			It("ages out every binding on every member once the traffic stops", func() {
				By("starting sustained pings to both containers and waiting for the bindings")
				startGroupTraffic()

				shortenAging()

				By("stopping the background pings in every remaining pod")
				for _, m := range members {
					stopBackgroundPings(m.namespace, m.podName)
				}
				pings.Clear()

				// Nothing below needs the containers reachable.
				By("silencing both external containers so they stop answering the refresh probe")
				silenceExternalContainer(extA)
				silenceExternalContainer(extB)

				for _, ip := range append(append([]string{}, extAIPs...), extBIPs...) {
					By(fmt.Sprintf("waiting for %s to age out of every gateway router", ip))
					Eventually(func(g Gomega) {
						rows, err := listDynamicMACBindings(ovnkPod)
						g.Expect(err).NotTo(HaveOccurred(), "must list MAC_Bindings")
						g.Expect(macBindingPortsForIP(rows, ip)).To(BeEmpty(),
							"MAC_Bindings for %s must age out after mac_binding_age_threshold", ip)
					}, noFloodAgeOutTimeout, 10*time.Second).Should(Succeed())
				}
			})
		})
	})
