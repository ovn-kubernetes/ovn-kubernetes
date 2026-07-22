// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"net"
	"sync"
	gotesting "testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("BaseUserDefinedNetworkController", func() {
	var (
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
	})

	type dhcpTest struct {
		vmName                string
		ips                   []string
		dns                   []string
		expectedDHCPv4Options *nbdb.DHCPOptions
		expectedDHCPv6Options *nbdb.DHCPOptions
	}
	DescribeTable("with layer2 primary UDN when configuring DHCP", func(t dhcpTest) {
		layer2NAD := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		fakeOVN := NewFakeOVN(true)
		lsp := &nbdb.LogicalSwitchPort{
			Name: "vm-port",
			UUID: "vm-port-UUID",
		}
		logicalSwitch := &nbdb.LogicalSwitch{
			UUID:  "layer2-switch-UUID",
			Name:  "layer2-switch",
			Ports: []string{lsp.UUID},
		}

		initialDB := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				logicalSwitch,
				lsp,
			},
		}
		fakeOVN.startWithDBSetup(
			initialDB,
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
			&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "kube-system",
					Name:      "kube-dns",
				},
				Spec: corev1.ServiceSpec{
					ClusterIPs: t.dns,
				},
			},
		)
		defer fakeOVN.shutdown()

		Expect(fakeOVN.NewUserDefinedNetworkController(layer2NAD)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "dummy",
				Labels: map[string]string{
					kubevirtv1.AppLabel:                "virt-launcher",
					kubevirtv1.VirtualMachineNameLabel: t.vmName,
				},
				Annotations: map[string]string{
					kubevirtv1.DomainAnnotation: t.vmName,
				},
			},
		}
		ips, err := util.ParseIPNets(t.ips)
		Expect(err).ToNot(HaveOccurred())
		podAnnotation := &util.PodAnnotation{
			IPs: ips,
		}
		Expect(controller.bnc.ensureDHCP(pod, podAnnotation, lsp)).To(Succeed())
		expectedDB := []libovsdbtest.TestData{}

		By("asserting the OVN entities provisioned in the NBDB are the expected ones")
		expectedLSP := lsp.DeepCopy()
		if t.expectedDHCPv4Options != nil {
			t.expectedDHCPv4Options.UUID = "vm1-dhcpv4-UUID"
			expectedLSP.Dhcpv4Options = &t.expectedDHCPv4Options.UUID
			expectedDB = append(expectedDB, t.expectedDHCPv4Options)
		}
		if t.expectedDHCPv6Options != nil {
			t.expectedDHCPv6Options.UUID = "vm1-dhcpv6-UUID"
			expectedLSP.Dhcpv6Options = &t.expectedDHCPv6Options.UUID
			expectedDB = append(expectedDB, t.expectedDHCPv6Options)
		}
		// Refresh logical switch to have the propert ports uuid
		obtainedLogicalSwitches := []*nbdb.LogicalSwitch{}
		Expect(fakeOVN.nbClient.List(context.Background(), &obtainedLogicalSwitches)).To(Succeed())
		expectedDB = append(expectedDB,
			obtainedLogicalSwitches[0],
			expectedLSP,
		)
		Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDB))

	},
		Entry("for ipv4 singlestack", dhcpTest{
			vmName: "vm1",
			dns:    []string{"10.96.0.100"},
			ips:    []string{"192.168.100.4/24"},
			expectedDHCPv4Options: &nbdb.DHCPOptions{
				Cidr: "192.168.100.0/24",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/cidr":             "192.168.100.0/24",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:192.168.100.0/24",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
					"k8s.ovn.org/name":             "foo/vm1",
				},
				Options: map[string]string{
					"lease_time": "3500",
					"server_mac": "0a:58:a9:fe:01:01",
					"hostname":   "\"vm1\"",
					"mtu":        "1300",
					"dns_server": "10.96.0.100",
					"server_id":  "169.254.1.1",
				},
			},
		}),
		Entry("for ipv6 singlestack", dhcpTest{
			vmName: "vm1",
			dns:    []string{"2015:100:200::10"},
			ips:    []string{"2010:100:200::2/60"},
			expectedDHCPv6Options: &nbdb.DHCPOptions{
				Cidr: "2010:100:200::/60",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/name":             "foo/vm1",
					"k8s.ovn.org/cidr":             "2010.100.200../60",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:2010.100.200../60",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
				},
				Options: map[string]string{
					"server_id":  "0a:58:6d:6d:c1:50",
					"fqdn":       "\"vm1\"",
					"dns_server": "2015:100:200::10",
				},
			},
		}),
		Entry("for dualstack", dhcpTest{
			vmName: "vm1",
			dns:    []string{"10.96.0.100", "2015:100:200::10"},
			ips:    []string{"192.168.100.4/24", "2010:100:200::2/60"},
			expectedDHCPv4Options: &nbdb.DHCPOptions{
				Cidr: "192.168.100.0/24",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/cidr":             "192.168.100.0/24",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:192.168.100.0/24",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
					"k8s.ovn.org/name":             "foo/vm1",
				},
				Options: map[string]string{
					"lease_time": "3500",
					"server_mac": "0a:58:a9:fe:01:01",
					"hostname":   "\"vm1\"",
					"mtu":        "1300",
					"dns_server": "10.96.0.100",
					"server_id":  "169.254.1.1",
				},
			},
			expectedDHCPv6Options: &nbdb.DHCPOptions{
				Cidr: "2010:100:200::/60",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/name":             "foo/vm1",
					"k8s.ovn.org/cidr":             "2010.100.200../60",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:2010.100.200../60",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
				},
				Options: map[string]string{
					"server_id":  "0a:58:6d:6d:c1:50",
					"fqdn":       "\"vm1\"",
					"dns_server": "2015:100:200::10",
				},
			},
		}),
	)
	Context("enableSourceLSPFailedLiveMigration", func() {
		const (
			vmName        = "test-vm"
			nadKey        = "awips/mgmt"
			localNodeName = "node-local"
		)

		newVirtLauncherPod := func(name, nodeName string, phase corev1.PodPhase, annotations map[string]string) *corev1.Pod {
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[kubevirtv1.DomainAnnotation] = vmName
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: "awips",
					Labels: map[string]string{
						kubevirtv1.AppLabel:                "virt-launcher",
						kubevirtv1.VirtualMachineNameLabel: vmName,
					},
					CreationTimestamp: metav1.Time{Time: time.Now()},
					Annotations:       annotations,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "kubevirt.io/v1",
						Kind:       "VirtualMachineInstance",
						Name:       vmName,
					}},
				},
				Spec: corev1.PodSpec{
					NodeName: nodeName,
				},
				Status: corev1.PodStatus{
					Phase: phase,
				},
			}
			return pod
		}

		setupControllerWithDBSetup := func(dbSetup *libovsdbtest.TestSetup, pods ...*corev1.Pod) (*BaseUserDefinedNetworkController, *FakeOVN) {
			localnetNAD := ovntest.GenerateNAD("mgmt", "mgmt", "awips",
				types.LocalnetTopology, "", types.NetworkRoleSecondary)

			fakeOVN := NewFakeOVN(false)
			objs := []runtime.Object{}
			for _, p := range pods {
				objs = append(objs, p)
			}
			if dbSetup != nil {
				fakeOVN.startWithDBSetup(*dbSetup, objs...)
			} else {
				fakeOVN.start(objs...)
			}
			DeferCleanup(fakeOVN.shutdown)

			Expect(fakeOVN.NewUserDefinedNetworkController(localnetNAD)).To(Succeed())
			controller, ok := fakeOVN.userDefinedNetworkControllers["mgmt"]
			Expect(ok).To(BeTrue())

			// Set local zone to only include localNodeName
			controller.bnc.localZoneNodes = &sync.Map{}
			controller.bnc.localZoneNodes.Store(localNodeName, true)

			return controller.bnc, fakeOVN
		}

		setupController := func(pods ...*corev1.Pod) *BaseUserDefinedNetworkController {
			bnc, _ := setupControllerWithDBSetup(nil, pods...)
			return bnc
		}

		It("should skip source LSP re-enable when source pod is on a remote node", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			sourcePod := newVirtLauncherPod("virt-launcher-"+vmName+"-source", "node-remote", corev1.PodRunning, nil)
			// Target pod is local, failed (completed) — triggers LiveMigrationFailed detection
			targetPod := newVirtLauncherPod("virt-launcher-"+vmName+"-target", localNodeName, corev1.PodFailed, nil)
			// Make target created after source so DiscoverLiveMigrationStatus picks it as target
			targetPod.CreationTimestamp = metav1.Time{Time: sourcePod.CreationTimestamp.Add(time.Second)}

			bnc := setupController(sourcePod, targetPod)

			// Call with empty IPs (IPAM-less localnet) — this would fail without the locality guard
			err := bnc.enableSourceLSPFailedLiveMigration(targetPod, nadKey, "", nil)
			Expect(err).NotTo(HaveOccurred(), "should not error when source pod is on a remote node")
		})

		It("should skip source LSP re-enable when source pod LSP is not local", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			sourcePod := newVirtLauncherPod("virt-launcher-"+vmName+"-source", "node-remote", corev1.PodRunning, nil)
			targetPod := newVirtLauncherPod("virt-launcher-"+vmName+"-target", localNodeName, corev1.PodSucceeded, nil)
			targetPod.CreationTimestamp = metav1.Time{Time: sourcePod.CreationTimestamp.Add(time.Second)}

			bnc := setupController(sourcePod, targetPod)

			err := bnc.enableSourceLSPFailedLiveMigration(targetPod, nadKey, "", nil)
			Expect(err).NotTo(HaveOccurred(), "should not error when source pod LSP is not local")
		})

		It("should re-enable source LSP when source pod is local and migration failed", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			sourcePodName := "virt-launcher-" + vmName + "-source"
			sourcePod := newVirtLauncherPod(sourcePodName, localNodeName, corev1.PodRunning, nil)
			targetPod := newVirtLauncherPod("virt-launcher-"+vmName+"-target", localNodeName, corev1.PodFailed, nil)
			targetPod.CreationTimestamp = metav1.Time{Time: sourcePod.CreationTimestamp.Add(time.Second)}

			// Build LSP and switch names matching what the controller will compute:
			//   LSP name: GetUserDefinedNetworkLogicalPortName(namespace, podName, nadKey)
			//   Switch name: GetNetworkScopedSwitchName(OVNLocalnetSwitch)
			sourceLSPName := util.GetUserDefinedNetworkLogicalPortName(sourcePod.Namespace, sourcePodName, nadKey)
			sourceLSP := &nbdb.LogicalSwitchPort{
				UUID:    sourceLSPName + "-UUID",
				Name:    sourceLSPName,
				Enabled: ptr.To(false),
			}
			switchName := util.GetUserDefinedNetworkPrefix("mgmt") + types.OVNLocalnetSwitch
			logicalSwitch := &nbdb.LogicalSwitch{
				UUID:  switchName + "-UUID",
				Name:  switchName,
				Ports: []string{sourceLSP.UUID},
			}

			dbSetup := &libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					logicalSwitch,
					sourceLSP,
				},
			}

			bnc, fakeOVN := setupControllerWithDBSetup(dbSetup, sourcePod, targetPod)

			mac := "0a:58:0a:80:00:05"
			ips := []string{"10.128.0.5/24"}
			err := bnc.enableSourceLSPFailedLiveMigration(targetPod, nadKey, mac, ips)
			Expect(err).NotTo(HaveOccurred(), "should re-enable source LSP without error")

			// Verify the LSP was updated: Enabled=true and addresses set
			expectedLSP := &nbdb.LogicalSwitchPort{
				UUID:      sourceLSP.UUID,
				Name:      sourceLSPName,
				Enabled:   ptr.To(true),
				Addresses: []string{mac + " 10.128.0.5"},
			}
			expectedSwitch := logicalSwitch.DeepCopy()
			Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedSwitch, expectedLSP))
		})
	})

	It("should not fail to sync pods if namespace is gone", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		fakeOVN := NewFakeOVN(false)
		fakeOVN.start(
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"other": "3"}`,
					},
				},
			},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		// inject a real networkManager instead of a fake one, so getActiveNetworkForNamespace will get called
		nadController, err := networkmanager.NewForZone("dummyZone", nil, fakeOVN.watcher)
		Expect(err).NotTo(HaveOccurred())
		controller.bnc.networkManager = nadController.Interface()

		// simulate that we listed the pod, but namespace was deleted after
		podWithNoNamespace := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "doesnotexist",
				Name:      "dummy",
			},
		}

		var initialPodList []interface{}
		initialPodList = append(initialPodList, podWithNoNamespace)

		err = controller.bnc.syncPodsForUserDefinedNetwork(initialPodList)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should not fail to sync pods if namespace has primary UDN label but NAD not ready", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		fakeOVN := NewFakeOVN(false)
		// Create namespace with primary UDN label but no NAD
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-namespace",
				Labels: map[string]string{
					types.RequiredUDNNamespaceLabel: "",
				},
			},
		}
		fakeOVN.start(
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"other": "3"}`,
					},
				},
			},
			namespace,
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		// inject a real networkManager so GetActiveNetworkForNamespace will get called
		nadController, err := networkmanager.NewForZone("dummyZone", nil, fakeOVN.watcher)
		Expect(err).NotTo(HaveOccurred())
		controller.bnc.networkManager = nadController.Interface()

		// Pod in namespace with primary UDN label but no NAD causes InvalidPrimaryNetworkError
		podInLabeledNamespace := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "test-namespace",
				Name:      "test-pod",
			},
		}

		var initialPodList []interface{}
		initialPodList = append(initialPodList, podInLabeledNamespace)

		// Should skip pod without error when GetActiveNetworkForNamespace returns InvalidPrimaryNetworkError
		err = controller.bnc.syncPodsForUserDefinedNetwork(initialPodList)
		Expect(err).NotTo(HaveOccurred())
	})

})

func TestAdvertisedSharedGatewaySNATUsesLiveAllowedExtIPSets(t *gotesting.T) {
	for _, outboundSNAT := range []string{types.NoOverlaySNATDisabled, types.NoOverlaySNATEnabled} {
		t.Run(outboundSNAT, func(t *gotesting.T) {
			bsnc, asf, localPodSubnets := newAdvertisedSNATTestController(t, outboundSNAT, config.GatewayModeShared)
			seedAdvertisedSNATAddressSets(t, asf)

			expectAdvertisedSNATUsesLiveAllowedExtIPs(t, bsnc, asf, localPodSubnets)
		})
	}
}

func TestAdvertisedSharedGatewaySNATFailsWithoutAllowedExtIPsForFamily(t *gotesting.T) {
	bsnc, asf, localPodSubnets := newAdvertisedSNATTestController(t, types.NoOverlaySNATDisabled, config.GatewayModeShared)
	config.IPv6Mode = false
	seedAdvertisedSNATAddressSets(t, asf)

	g := NewWithT(t)
	_, err := bsnc.buildUDNEgressSNAT(localPodSubnets, "rtos-bluenet-worker1", true)
	g.Expect(err).To(MatchError(ContainSubstring(
		"failed to build allowed_ext_ips SNAT for advertised network bluenet, subnet ae70::/64: no address set UUID for IPv6",
	)))
}

func TestAdvertisedLocalGatewaySNATUsesDestinationMatch(t *gotesting.T) {
	bsnc, asf, localPodSubnets := newAdvertisedSNATTestController(t, types.NoOverlaySNATDisabled, config.GatewayModeLocal)
	seedAdvertisedSNATAddressSets(t, asf)

	expectAdvertisedSNATUsesDestinationMatch(t, bsnc, asf, localPodSubnets)
}

func newAdvertisedSNATTestController(
	t *gotesting.T,
	outboundSNAT string,
	gatewayMode config.GatewayMode,
) (*BaseUserDefinedNetworkController, *addressset.FakeAddressSetFactory, []*net.IPNet) {
	t.Helper()
	return newAdvertisedSNATTestControllerForTopology(
		t,
		types.Layer3Topology,
		"100.128.0.0/16/24,ae70::/60/64",
		outboundSNAT,
		gatewayMode,
		ovntest.MustParseIPNets("100.128.0.0/24", "ae70::/64"),
	)
}

func newAdvertisedSNATTestControllerForTopology(
	t *gotesting.T,
	topology string,
	cidrs string,
	outboundSNAT string,
	gatewayMode config.GatewayMode,
	localPodSubnets []*net.IPNet,
) (*BaseUserDefinedNetworkController, *addressset.FakeAddressSetFactory, []*net.IPNet) {
	t.Helper()
	RegisterTestingT(t)
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = gatewayMode
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
	config.Gateway.V6MasqueradeSubnet = "fd69::/112"

	const (
		networkName = "bluenet"
		nadName     = "rednad"
		namespace   = "greenamespace"
	)
	nad := ovntest.GenerateNADWithConfig(nadName, namespace, fmt.Sprintf(`
{
        "cniVersion": "1.1.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology": %q,
        "subnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q,
        "role": %q,
        "transport": %q,
        "outboundSNAT": %q
}
`,
		networkName,
		topology,
		cidrs,
		fmt.Sprintf("%s/%s", namespace, nadName),
		types.NetworkRolePrimary,
		types.NetworkTransportNoOverlay,
		outboundSNAT,
	))
	ovntest.AnnotateNADWithNetworkID("3", nad)
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		t.Fatalf("failed to parse NAD: %v", err)
	}

	controllerName := getNetworkControllerName(netInfo.GetNetworkName())
	asf := addressset.NewFakeAddressSetFactory(controllerName)
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker1",
			Annotations: map[string]string{
				util.OVNNodeHostCIDRs: `["192.168.126.11/24","fd00::11/64"]`,
			},
		},
	}
	clientSet := util.GetOVNClientset(&corev1.NodeList{Items: []corev1.Node{node}}).GetOVNKubeControllerClientset()
	watchFactory, err := factory.NewOVNKubeControllerWatchFactory(clientSet, "test-node")
	if err != nil {
		t.Fatalf("failed to create watch factory: %v", err)
	}
	if err := watchFactory.Start(); err != nil {
		t.Fatalf("failed to start watch factory: %v", err)
	}
	t.Cleanup(watchFactory.Shutdown)

	nbClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{})
	if err != nil {
		t.Fatalf("failed to create libovsdb test harness: %v", err)
	}
	t.Cleanup(libovsdbCleanup.Cleanup)
	addressSetManager := addresssetmanager.NewAddressSetManager(
		watchFactory.PodCoreInformer(),
		watchFactory.NamespaceInformer(),
		watchFactory.NodeCoreInformer(),
		nbClient,
		networkmanager.Default().Interface().GetNetworkNameForNADKey,
	)
	return &BaseUserDefinedNetworkController{
			BaseNetworkController: BaseNetworkController{
				controllerName:      controllerName,
				ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
				addressSetFactory:   asf,
				addressSetManager:   addressSetManager,
			},
		},
		asf,
		localPodSubnets
}

func seedAdvertisedSNATAddressSets(t *gotesting.T, asf addressset.AddressSetFactory) {
	t.Helper()
	nodeIPsASIDs := getClusterNodeIPsAddrSetDbIDsForTest()
	if _, err := asf.NewAddressSet(nodeIPsASIDs, []string{"192.168.126.11", "fd00::11"}); err != nil {
		t.Fatalf("failed to create node IP address set: %v", err)
	}

	svcIPsASIDs := udnenabledsvc.GetAddressSetDBIDs()
	if _, err := asf.NewAddressSet(svcIPsASIDs, []string{"10.96.0.10", "fd02::10"}); err != nil {
		t.Fatalf("failed to create UDN-enabled service address set: %v", err)
	}
}

func expectAdvertisedSNATUsesLiveAllowedExtIPs(
	t *gotesting.T,
	bsnc *BaseUserDefinedNetworkController,
	asf addressset.AddressSetFactory,
	localPodSubnets []*net.IPNet,
) {
	t.Helper()
	g := NewWithT(t)

	snats, err := bsnc.buildUDNEgressSNAT(localPodSubnets, "rtos-bluenet-worker1", true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(snats).To(HaveLen(4))

	nodeIPsAS, err := asf.GetAddressSet(getClusterNodeIPsAddrSetDbIDsForTest())
	g.Expect(err).NotTo(HaveOccurred())
	nodeIPv4ASUUID, nodeIPv6ASUUID := nodeIPsAS.GetASUUID()
	svcIPsAS, err := asf.GetAddressSet(udnenabledsvc.GetAddressSetDBIDs())
	g.Expect(err).NotTo(HaveOccurred())
	svcIPv4ASUUID, svcIPv6ASUUID := svcIPsAS.GetASUUID()

	actualAllowedExtIPsByLogicalIP := map[string][]string{}
	for _, snat := range snats {
		g.Expect(snat.Match).To(Equal(""))
		g.Expect(snat.AllowedExtIPs).NotTo(BeNil())
		g.Expect(snat.ExemptedExtIPs).To(BeNil())
		actualAllowedExtIPsByLogicalIP[snat.LogicalIP] = append(
			actualAllowedExtIPsByLogicalIP[snat.LogicalIP],
			*snat.AllowedExtIPs,
		)
	}
	g.Expect(actualAllowedExtIPsByLogicalIP["100.128.0.0/24"]).To(ConsistOf(nodeIPv4ASUUID, svcIPv4ASUUID))
	g.Expect(actualAllowedExtIPsByLogicalIP["ae70::/64"]).To(ConsistOf(nodeIPv6ASUUID, svcIPv6ASUUID))
}

func expectAdvertisedSNATUsesDestinationMatch(
	t *gotesting.T,
	bsnc *BaseUserDefinedNetworkController,
	asf addressset.AddressSetFactory,
	localPodSubnets []*net.IPNet,
) {
	t.Helper()
	g := NewWithT(t)

	snats, err := bsnc.buildUDNEgressSNAT(localPodSubnets, "rtos-bluenet-worker1", true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(snats).To(HaveLen(2))

	nodeIPsAS, err := asf.GetAddressSet(getClusterNodeIPsAddrSetDbIDsForTest())
	g.Expect(err).NotTo(HaveOccurred())
	svcIPsAS, err := asf.GetAddressSet(udnenabledsvc.GetAddressSetDBIDs())
	g.Expect(err).NotTo(HaveOccurred())

	dstMac := util.IPAddrToHWAddr(bsnc.GetNodeManagementIP(localPodSubnets[0]).IP)
	dstMacMatch := getMasqueradeManagementIPSNATMatch(dstMac.String())
	v4Match := getClusterNodesDestinationBasedSNATMatch(utilnet.IPv4, nodeIPsAS, svcIPsAS)
	v6Match := getClusterNodesDestinationBasedSNATMatch(utilnet.IPv6, nodeIPsAS, svcIPsAS)

	g.Expect(snats[0].Match).To(Equal(fmt.Sprintf("%s && %s", dstMacMatch, v4Match)))
	g.Expect(snats[0].AllowedExtIPs).To(BeNil())
	g.Expect(snats[0].ExemptedExtIPs).To(BeNil())

	g.Expect(snats[1].Match).To(Equal(fmt.Sprintf("%s && %s", dstMacMatch, v6Match)))
	g.Expect(snats[1].AllowedExtIPs).To(BeNil())
	g.Expect(snats[1].ExemptedExtIPs).To(BeNil())
}

// TestDHCPPodNetworksChanged covers the pod-update re-trigger for DHCP IPAM
// networks: ovnkube-node patches the DHCP-learned IP into the pod-networks
// annotation during CNI ADD — after the LSP was first created — so an
// annotation-only pod update must reprocess the port for these networks
// (and only for them).
func TestDHCPPodNetworksChanged(t *gotesting.T) {
	newController := func(g Gomega, ipamType string) *BaseUserDefinedNetworkController {
		netconf := &ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				Name: "localnet-net",
				Type: "ovn-k8s-cni-overlay",
				IPAM: cnitypes.IPAM{Type: ipamType},
			},
			NADName:  "foo-ns/localnet-nad",
			Topology: types.LocalnetTopology,
		}
		netInfo, err := util.NewNetInfo(netconf)
		g.Expect(err).NotTo(HaveOccurred())
		return &BaseUserDefinedNetworkController{
			BaseNetworkController: BaseNetworkController{
				ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
				networkManager: &networkmanager.FakeNetworkManager{
					NADNetworks: map[string]util.NetInfo{"foo-ns/localnet-nad": netInfo},
				},
			},
		}
	}

	newPod := func(podNetworks string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "bar-pod",
				Namespace:   "foo-ns",
				Annotations: map[string]string{},
			},
		}
		if podNetworks != "" {
			pod.Annotations[types.OvnPodAnnotationName] = podNetworks
		}
		return pod
	}

	withoutIPs := `{"foo-ns/localnet-nad":{"mac_address":"0a:58:fd:98:00:01","role":"secondary","ipam_mode":"dhcp"}}`
	withIPs := `{"foo-ns/localnet-nad":{"ip_addresses":["10.1.192.102/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["10.1.192.1"],"role":"secondary","ipam_mode":"dhcp"}}`
	// this network's entry identical to withoutIPs; only the default
	// network's entry appears — the write every multi-homed pod gets
	withOtherNetworkEntry := `{"default":{"ip_addresses":["10.244.1.5/24"],"mac_address":"0a:58:0a:f4:01:05","role":"primary"},"foo-ns/localnet-nad":{"mac_address":"0a:58:fd:98:00:01","role":"secondary","ipam_mode":"dhcp"}}`
	// the realistic multi-homed timeline: by CNI-patch time the default
	// network's entry is already in the blob; the patch adds the lease
	// (IPs + gateway) to this network's entry and touches nothing else
	withOtherNetworkEntryAndIPs := `{"default":{"ip_addresses":["10.244.1.5/24"],"mac_address":"0a:58:0a:f4:01:05","role":"primary"},"foo-ns/localnet-nad":{"ip_addresses":["10.1.192.102/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["10.1.192.1"],"role":"secondary","ipam_mode":"dhcp"}}`

	t.Run("triggers when the annotation gains the DHCP-learned IPs", func(t *gotesting.T) {
		g := NewWithT(t)
		bsnc := newController(g, "dhcp")
		g.Expect(bsnc.dhcpPodNetworksChanged(newPod(withoutIPs), newPod(withIPs))).To(BeTrue())
	})

	t.Run("does not trigger when the annotation is unchanged", func(t *gotesting.T) {
		g := NewWithT(t)
		bsnc := newController(g, "dhcp")
		g.Expect(bsnc.dhcpPodNetworksChanged(newPod(withIPs), newPod(withIPs))).To(BeFalse())
	})

	t.Run("does not trigger on non-DHCP networks", func(t *gotesting.T) {
		g := NewWithT(t)
		bsnc := newController(g, "")
		g.Expect(bsnc.dhcpPodNetworksChanged(newPod(withoutIPs), newPod(withIPs))).To(BeFalse())
	})

	t.Run("does not trigger without an old pod", func(t *gotesting.T) {
		g := NewWithT(t)
		bsnc := newController(g, "dhcp")
		g.Expect(bsnc.dhcpPodNetworksChanged(nil, newPod(withIPs))).To(BeFalse())
	})

	t.Run("does not trigger when only another network's entry changes", func(t *gotesting.T) {
		g := NewWithT(t)
		bsnc := newController(g, "dhcp")
		g.Expect(bsnc.dhcpPodNetworksChanged(newPod(withoutIPs), newPod(withOtherNetworkEntry))).To(BeFalse())
	})

	t.Run("triggers when this network's entry is removed", func(t *gotesting.T) {
		g := NewWithT(t)
		bsnc := newController(g, "dhcp")
		g.Expect(bsnc.dhcpPodNetworksChanged(newPod(withIPs), newPod(`{}`))).To(BeTrue())
	})

	t.Run("triggers on the CNI lease patch of this network's entry in a multi-homed annotation", func(t *gotesting.T) {
		g := NewWithT(t)
		bsnc := newController(g, "dhcp")
		g.Expect(bsnc.dhcpPodNetworksChanged(newPod(withOtherNetworkEntry), newPod(withOtherNetworkEntryAndIPs))).To(BeTrue())
	})
}
