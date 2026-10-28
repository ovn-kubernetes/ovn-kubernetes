// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package annotate

import (
	// ensure all the ginkgo tests are loaded
	_ "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e"
)

var (
	// LabelToLabelMaps label -> label (ginkgo label)
	// E2E tests are written with the support of ginkgo. ginkgo tests may contain Labels.
	LabelToLabelMaps = map[string][]string{
		"[Disabled:Unimplemented]": {
			`[Feature:Service]`,
			`[Feature:NetworkPolicy]`,
			`[Feature:AdminNetworkPolicy]`,
			`[Feature:BaselineNetworkPolicy]`,
			`[Feature:EgressService]`,
			`[Feature:EgressFirewall]`,
			`[Feature:EgressQos]`,
			`[Feature:ExternalGateway]`,
			`[Feature:DisablePacketMTUCheck]`,
			`[Feature:Interconnect]`,
			`[Feature:Multicast]`,
			`[Feature:MultiHoming]`,
			`[Feature:NetworkConnect]`,
			`[Feature:NodeIPMACMigration]`,
			`[Feature:OVSCPUPin]`,
			`[Feature:Unidle]`,
		},
		"[Suite:ovn-kubernetes/conformance/serial/virtualization]": {
			`[Feature:VirtualMachineSupport]`,
		},
	}
	// if a test name partially or fully contains one of the map value strings, then add the label to the test
	// label -> partial or full test name or regex to match a test name
	LabelToTestNameMatchMaps = map[string][]string{
		// alpha features that are not gated
		"[Disabled:Alpha]": {},
		// tests for features that are not implemented in openshift
		"[Disabled:Unimplemented]": {
			`Creating a static pod on a node Should successfully create then remove a static pod`,
			`Pod to external server PMTUD`,
			`Pod to pod TCP with low MTU`,
			`blocking ICMP needs frag`,
			// UDN test requires egress
			`pod2Egress on a user defined primary network`,
			`is isolated from the default network`,
			// requires host net port collision avoidance
			`EndpointSlices mirroring`,
			// reference kind nodes
			`Should validate connectivity within a namespace of pods on separate nodes`,
			// tied to KinD / container runtime
			`e2e delete databases`,
			`test e2e inter-node connectivity between worker nodes`,
			`e2e control plane`,
			`test e2e pod connectivity to host addresses`,
			`e2e br-int flow monitoring export validation`,
			`e2e ingress to host-networked pods traffic validation`,
			`e2e ingress traffic validation`,
			// pods dont drop privileges
			`Should validate the hairpinned traffic is always allowed`,
			// refactor to give pod sufficient privs for tcpdump
			`should be able to receive multicast IGMP query`,
			// refactor to give pod sufficient privs
			`UDN Pod should react to k8s.ovn.org/open-default-ports annotations changes`,
			// load balancer isn't becoming available from the cloud services. Ensure, we provider the correct provider to the k8 api which spawns the ext LB.
			`services on a user defined primary network should be reachable through their cluster IP, node port and load balancer L3 primary UDN, cluster-networked pods, NodePort service`,
			`services on a user defined primary network should be reachable through their cluster IP, node port and load balancer L2 primary UDN, cluster-networked pods, NodePort service`,
			// test gets interrupted when test timeout expires while testing pod and lack of pod connectivity.
			`a user defined primary network created using ClusterUserDefinedNetwork isolates overlapping CIDRs with L3 primary UDN`,
			`a user defined primary network created using NetworkAttachmentDefinitions isolates overlapping CIDRs with L3 primary UDN`,
			// tests are tied to KinD deployment and select nodes based on KinD deployment. Needs refactoring.
			`allow ingress traffic to one pod from a particular namespace`,
			// private image in upstream test & need privs for tcpdump
			`e2e NetworkQoS validation`,
			// ClusterNetworkConnect CR is not in downstream yet
			`ClusterNetworkConnect: API validations`,
			// unknown rc 7 code
			`Network Segmentation: API validations`,
			// 'Network allocation failed for at least one node'
			`Network Segmentation UserDefinedNetwork CRD Controller should correctly report subsystem error on node subnet allocation`,
			// requires implementation of overlay method (provider API)
			`Network Segmentation: Localnet using ClusterUserDefinedNetwork CR, pods in different namespaces, should communicate over localnet topology`,
			// requires implementation of the SetupUnderlay() method
			`Network Segmentation: Localnet should preserve LSPs for IPAM-less localnet pods after ovnkube-node restart`,
			// pods dont drop privileges
			`should be able to send multicast UDP traffic between nodes`,
			// TODO: fix the flakiness with net-seg overlapping CIDRs test in downstream.
			"isolates overlapping CIDRs",
			// tied to only kind cluster
			"Node Shutdown and Startup",
			// TODO: Fix flakiness in this test. Pod connectivity checks may need
			// to be wrapped in an Eventually block.
			"perform east/west traffic between nodes following OVN Kube node pod restart",
			// TODO: this test perma failing panicing at ipalloc.(*primaryIPAllocator).AllocateNextV4.
			// Fix it later.
			"integration should recover ovnkube pods after restart with primary and secondary UDN resources",
			// Disable BGP and VRF Lite tests.
			"VRF-Lite",
			"BGP: When default podNetwork is advertised",
			"BGP: Pod to external server when CUDN network is advertised",
			"BGP: isolation between advertised networks",
			// Disable EVPN isolation test with other CUDN advertised network because
			// it doesn't have required FRR configuration yet.
			"When there is other network Of type Layer 3 CUDN advertised",
			"When there is other network Of type Layer 2 CUDN advertised",
			// These tests need bgpServerNetwork "bgpnet" which is not configured
			// for downstream tests, so disable it.
			"It cannot reach an external server on a different network",
			// MAC security pod tests not implemented yet
			"Network Segmentation: MAC security",
			// Pending to implement virt tests
			"Kubevirt Virtual Machines with user defined networks and persistent ips",
			"Kubevirt Virtual Machines IP family validation",
			"Kubevirt Virtual Machines duplicate addresses validation",
			"Kubevirt Virtual Machines with default pod network",
			"Kubevirt Virtual Machines ipv4 subnet exhaustion",
			"Kubevirt Virtual Machines with kubevirt VM using layer2 UDPN should configure IPv4 and IPv6 using DHCP and NDP",
			"Kubevirt Virtual Machines with user defined networks with ipamless localnet",
			"Kubevirt Virtual Machines user-defined network port-security.*localnet",
		},
		// tests that rely on special configuration that we do not yet support
		"[Disabled:SpecialConfig]": {},
		// tests that are known broken and need to be fixed upstream or in openshift
		// always add an issue here
		"[Disabled:Broken]": {},
		// tests that need to be temporarily disabled while the rebase is in progress.
		"[Disabled:RebaseInProgress]": {},
		// tests that may work, but we don't support them
		"[Disabled:Unsupported]": {},
		// tests too slow to be part of conformance
		"[Slow]": {},
		// tests that are known flaky
		"[Flaky]": {},
		// tests that must be run without competition
		"[Serial]": {
			"e2e egress IP validation",
		},
		// Tests that don't pass on disconnected, either due to requiring
		// internet access for GitHub (e.g. many of the s2i builds), or
		// because of pullthrough not supporting ICSP (https://bugzilla.redhat.com/show_bug.cgi?id=1918376)
		"[Skipped:Disconnected]": {},
		"[Skipped:alibabacloud]": {},
		"[Skipped:aws]":          {},
		"[Skipped:azure]":        {},
		"[Skipped:baremetal]":    {},
		"[Skipped:gce]":          {},
		"[Skipped:ibmcloud]":     {},
		"[Skipped:kubevirt]":     {},
		"[Skipped:nutanix]":      {},
		"[Skipped:openstack]":    {},
		"[Skipped:ovirt]":        {},
		"[Skipped:vsphere]":      {},
		// These tests are skipped when openshift-tests needs to use a proxy to reach the
		// cluster -- either because the test won't work while proxied, or because the test
		// itself is testing a functionality using it's own proxy.
		"[Skipped:Proxy]":                 {},
		"[Skipped:SingleReplicaTopology]": {},
		// Tests which can't be run/don't make sense to run against a cluster with all optional capabilities disabled
		"[Skipped:NoOptionalCapabilities]": {},
		"[Skipped:ibmroks]":                {},
	}

	ExcludedTests = []string{
		`\[Disabled:`,
		`\[Disruptive\]`,
		`\[Skipped\]`,
		`\[Slow\]`,
		`\[Flaky\]`,
		`\[Local\]`,
	}
)
