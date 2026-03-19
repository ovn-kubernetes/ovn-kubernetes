package ovn

import (
	"context"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	fakenadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
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
					kubevirtv1.VirtualMachineNameLabel: t.vmName,
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
	It("should not delete localnet LSPs for pods whose NADs are not yet registered", func() {
		// This test reproduces a race condition where multiple NADs map to the same
		// localnet network. When the network controller starts (triggered by the first
		// NAD), syncPodsForUserDefinedNetwork only sees pods whose NADs are registered.
		// Pods using later-arriving NADs are skipped, and their existing LSPs get
		// deleted as stale.
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableInterconnect = true

		const (
			networkName = "localnet1"
			namespace   = "myvms"
			nodeName    = "worker1"
		)

		// Create 4 ipamless localnet NADs all for the same network.
		makeNAD := func(name string) *nadapi.NetworkAttachmentDefinition {
			nad := ovntest.GenerateNAD(networkName, name, namespace,
				types.LocalnetTopology, "", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("1", nad)
			if nad.Annotations == nil {
				nad.Annotations = map[string]string{}
			}
			nad.Annotations[types.OvnNetworkNameAnnotation] = networkName
			return nad
		}
		nad0 := makeNAD("vm0")
		nad1 := makeNAD("vm1")
		nad2 := makeNAD("vm2")
		nad3 := makeNAD("vm3")

		// Compute localnet switch name and LSP names using the real helpers
		nInfo, err := util.ParseNADInfo(nad0)
		Expect(err).NotTo(HaveOccurred())
		localnetSwitchName := nInfo.GetNetworkScopedName(types.OVNLocalnetSwitch)

		// LSP names
		lspName := func(nadNS, nadName, podNS, podName string) string {
			return util.GetUserDefinedNetworkLogicalPortName(podNS, podName, nadNS+"/"+nadName)
		}
		lsp0Name := lspName(namespace, "vm0", namespace, "pod-0")
		lsp1Name := lspName(namespace, "vm1", namespace, "pod-1")
		lsp2Name := lspName(namespace, "vm2", namespace, "pod-2")
		lsp3Name := lspName(namespace, "vm3", namespace, "pod-3")

		// Pre-populate OVN DB with all 4 LSPs on the localnet switch
		lsp0 := &nbdb.LogicalSwitchPort{UUID: "lsp0-UUID", Name: lsp0Name, ExternalIDs: map[string]string{"pod": "true", "namespace": namespace}}
		lsp1 := &nbdb.LogicalSwitchPort{UUID: "lsp1-UUID", Name: lsp1Name, ExternalIDs: map[string]string{"pod": "true", "namespace": namespace}}
		lsp2 := &nbdb.LogicalSwitchPort{UUID: "lsp2-UUID", Name: lsp2Name, ExternalIDs: map[string]string{"pod": "true", "namespace": namespace}}
		lsp3 := &nbdb.LogicalSwitchPort{UUID: "lsp3-UUID", Name: lsp3Name, ExternalIDs: map[string]string{"pod": "true", "namespace": namespace}}

		localnetSwitch := &nbdb.LogicalSwitch{
			UUID:  localnetSwitchName + "-UUID",
			Name:  localnetSwitchName,
			Ports: []string{lsp0.UUID, lsp1.UUID, lsp2.UUID, lsp3.UUID},
		}

		initialDB := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{localnetSwitch, lsp0, lsp1, lsp2, lsp3},
		}

		fakeOVN := NewFakeOVN(false)
		fakeOVN.startWithDBSetup(
			initialDB,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						"kubernetes.io/hostname": nodeName,
					},
					Annotations: map[string]string{
						"k8s.ovn.org/zone":        nodeName,
						"k8s.ovn.org/network-ids": `{"localnet1": "1"}`,
					},
				},
			},
			// Add only nad0 via the standard mechanism
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad0}},
		)
		defer fakeOVN.shutdown()

		// Add nad1-3 directly to the watch factory's NAD informer cache.
		// They exist in the API server but the NAD controller hasn't registered
		// them with the network controller yet (simulating the race).
		for _, nad := range []*nadapi.NetworkAttachmentDefinition{nad1, nad2, nad3} {
			_, err = fakeOVN.watcher.GetNAD(nad.Namespace, nad.Name)
			if err != nil {
				nadCopy := nad.DeepCopy()
				tracker := fakeOVN.fakeClient.NetworkAttchDefClient.(*fakenadclient.Clientset).Tracker()
				Expect(tracker.Create(schema.GroupVersionResource{
					Group: "k8s.cni.cncf.io", Version: "v1", Resource: "network-attachment-definitions",
				}, nadCopy, nadCopy.Namespace)).To(Succeed())
			}
		}
		// Wait for the informer to pick up the NADs
		Eventually(func() error {
			for _, nad := range []*nadapi.NetworkAttachmentDefinition{nad1, nad2, nad3} {
				_, err := fakeOVN.watcher.GetNAD(nad.Namespace, nad.Name)
				if err != nil {
					return err
				}
			}
			return nil
		}).Should(Succeed())

		// Register ONLY the first NAD (vm0) with the controller.
		Expect(fakeOVN.NewUserDefinedNetworkController(nad0)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers[networkName]
		Expect(ok).To(BeTrue())

		// Replace the networkManager with a fake that only knows about vm0.
		// This simulates the race: the NAD controller has only processed the
		// first NAD when syncPodsForUserDefinedNetwork runs.
		controller.bnc.networkManager = &networkmanager.FakeNetworkManager{
			NADNetworks: map[string]util.NetInfo{
				namespace + "/vm0": nInfo,
			},
		}

		By("verifying only vm0 NAD is registered")
		Expect(controller.bnc.networkManager.GetNetworkNameForNADKey(namespace + "/vm0")).To(Equal(networkName))
		Expect(controller.bnc.networkManager.GetNetworkNameForNADKey(namespace + "/vm1")).To(BeEmpty())
		Expect(controller.bnc.networkManager.GetNetworkNameForNADKey(namespace + "/vm2")).To(BeEmpty())
		Expect(controller.bnc.networkManager.GetNetworkNameForNADKey(namespace + "/vm3")).To(BeEmpty())

		By("building pod list with all 4 pods referencing their respective NADs")
		makePod := func(name, nadName string) *corev1.Pod {
			nadKey := namespace + "/" + nadName
			return &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      name,
					Annotations: map[string]string{
						"k8s.v1.cni.cncf.io/networks": `[{"name":"` + nadName + `"}]`,
						"k8s.ovn.org/pod-networks":    `{"` + nadKey + `":{"mac_address":"0a:58:01:02:03:04","role":"secondary"}}`,
					},
				},
				Spec:   corev1.PodSpec{NodeName: nodeName},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}
		}
		podList := []interface{}{
			makePod("pod-0", "vm0"),
			makePod("pod-1", "vm1"),
			makePod("pod-2", "vm2"),
			makePod("pod-3", "vm3"),
		}

		By("calling syncPodsForUserDefinedNetwork (this is where the race manifests)")
		err = controller.bnc.syncPodsForUserDefinedNetwork(podList)
		Expect(err).NotTo(HaveOccurred())

		By("checking how many LSPs remain on the localnet switch")
		obtainedSwitches := []*nbdb.LogicalSwitch{}
		Expect(fakeOVN.nbClient.List(context.Background(), &obtainedSwitches)).To(Succeed())

		var localnetSW *nbdb.LogicalSwitch
		for _, sw := range obtainedSwitches {
			if sw.Name == localnetSwitchName {
				localnetSW = sw
				break
			}
		}
		Expect(localnetSW).NotTo(BeNil(), "localnet switch should exist")

		// With the fix, all 4 LSPs should be preserved because the sync function
		// falls back to checking the NAD informer cache for unregistered NADs.
		Expect(localnetSW.Ports).To(HaveLen(4), "all 4 localnet LSPs should be preserved when multiple NADs map to the same network")
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

})
