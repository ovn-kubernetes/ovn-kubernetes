package ovn

import (
	"context"
	"net"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

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

	Context("with layer2 interconnect and multi-VTEP support", func() {

		var (
			layer2NAD *nadapi.NetworkAttachmentDefinition
			netInfo   util.NetInfo

			testNode1    corev1.Node
			testNode2    corev1.Node
			node1Chassis sbdb.Chassis
			node2Chassis sbdb.Chassis

			node1Encap1 sbdb.Encap
			node1Encap2 sbdb.Encap
			node2Encap1 sbdb.Encap
			node2Encap2 sbdb.Encap

			initialDB libovsdbtest.TestSetup
		)

		BeforeEach(func() {
			// Restore global default values before each testcase
			Expect(config.PrepareTestConfig()).To(Succeed())

			// Create layer2 NAD
			layer2NAD = ovntest.GenerateNAD("blue-l2-net", "blue-l2-net", "blue-ns",
				types.Layer2Topology, "10.1.0.0/16", types.NetworkRoleSecondary)

			// Convert NAD to NetInfo
			var err error
			netInfo, err = util.ParseNADInfo(layer2NAD)
			Expect(err).NotTo(HaveOccurred())

			node1ChassisID := "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
			node2ChassisID := "cb9ec8fa-b409-4ef3-9f42-d9283c47aac7"
			// node1 is a local zone node
			testNode1 = corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					Annotations: map[string]string{
						util.OvnNodeChassisID: node1ChassisID,
						util.OvnNodeID:        "2",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}},
				},
			}
			// node2 is a remote zone node
			testNode2 = corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node2",
					Annotations: map[string]string{
						util.OvnNodeChassisID: node2ChassisID,
						util.OvnNodeID:        "3",
					},
				},
				Status: corev1.NodeStatus{
					Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}},
				},
			}

			node1Encap1 = sbdb.Encap{ChassisName: node1ChassisID, IP: "10.0.0.11", UUID: "6b6216bc-b409-4ef3-9f42-d9283c47aac6", Type: "geneve"}
			node1Encap2 = sbdb.Encap{ChassisName: node1ChassisID, IP: "10.0.0.12", UUID: "6b6216bc-b409-4ef3-9f42-d9283c47aac7", Type: "geneve"}
			node2Encap1 = sbdb.Encap{ChassisName: node2ChassisID, IP: "10.0.0.21", UUID: "6b6216bc-b409-4ef3-9f42-d9283c47aac8", Type: "geneve"}
			node2Encap2 = sbdb.Encap{ChassisName: node2ChassisID, IP: "10.0.0.22", UUID: "6b6216bc-b409-4ef3-9f42-d9283c47aac9", Type: "geneve"}

			node1Chassis = sbdb.Chassis{Name: node1ChassisID, Hostname: testNode1.ObjectMeta.Name, UUID: node1ChassisID, Encaps: []string{node1Encap1.UUID, node1Encap2.UUID}}
			node2Chassis = sbdb.Chassis{Name: node2ChassisID, Hostname: testNode2.ObjectMeta.Name, UUID: node2ChassisID, Encaps: []string{node2Encap1.UUID, node2Encap2.UUID}}

			initialDB.SBData = []libovsdbtest.TestData{
				&node1Encap1, &node1Encap2, &node2Encap1, &node2Encap2,
				&node1Chassis, &node2Chassis,
			}

		})

		It("should update port binding for remote pod with encap IP", func() {
			// Enable interconnect for layer2 topology
			config.OVNKubernetesFeature.EnableInterconnect = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			fakeOVN := NewFakeOVN(true)

			fakeOVN.startWithDBSetup(initialDB, &testNode1, &testNode2)
			defer fakeOVN.shutdown()

			// Create the layer2 network controller
			Expect(fakeOVN.NewUserDefinedNetworkController(layer2NAD)).To(Succeed())
			controller, ok := fakeOVN.userDefinedNetworkControllers["blue-l2-net"]
			Expect(ok).To(BeTrue())

			// Get the full layer2 controller and call init() to set up logical switch
			fullL2Controller, ok := fakeOVN.fullL2UDNControllers["blue-l2-net"]
			Expect(ok).To(BeTrue())
			err := fullL2Controller.init()
			Expect(err).NotTo(HaveOccurred())

			podAnnotation := &util.PodAnnotation{
				IPs:      []*net.IPNet{ovntest.MustParseIPNet("10.1.0.3/24")},
				MAC:      ovntest.MustParseMAC("0a:58:0a:01:00:03"),
				Role:     types.NetworkRoleSecondary,
				TunnelID: 19,
			}

			// Create remote pod with encap IP annotation
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "blue-ns",
					Name:      "pod-1",
					Annotations: map[string]string{
						"k8s.v1.cni.cncf.io/networks": "blue-ns/blue-l2-net",
					},
				},
				Spec: corev1.PodSpec{
					NodeName: testNode2.ObjectMeta.Name,
				},
			}

			annotation, err := util.MarshalPodAnnotation(pod.Annotations, podAnnotation, "blue-ns/blue-l2-net")
			Expect(err).NotTo(HaveOccurred())
			pod.Annotations = annotation

			By("Verify the conditions that should trigger Port_Binding.encap field update")
			isLocalPod := controller.bnc.isPodScheduledinLocalZone(pod)
			isLayer2Interconnect := controller.bnc.isLayer2Interconnect()
			Expect(isLocalPod).To(BeFalse(), "Pod should be on remote node")
			Expect(isLayer2Interconnect).To(BeTrue(), "Layer2 interconnect should be enabled")

			By("Create Port_Binding in SB database to simulate ovn-northd behavior")
			// add the transit switch port bindings on behalf of ovn-northd so that
			// the node gets added successfully. This is required for test simulation
			// and not required in real world scenario.
			layer2NADName := util.GetNADName(layer2NAD.ObjectMeta.Namespace, layer2NAD.ObjectMeta.Name)
			transistSwitchPortName := controller.bnc.GetLogicalPortName(pod, layer2NADName)
			transistSwitchName := netInfo.GetNetworkScopedName(types.OVNLayer2Switch)
			err = libovsdbtest.CreateTransitSwitchPortBindings(fakeOVN.sbClient, transistSwitchName, transistSwitchPortName)
			Expect(err).NotTo(HaveOccurred())

			By("Calling ensurePodForUserDefinedNetwork for Pod creation event without encap IP")
			err = controller.bnc.ensurePodForUserDefinedNetwork(pod, true)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Port_Binding.encap field is empty when encap IP is not set")
			verifyPortBindingEncap(fakeOVN, transistSwitchPortName, "")

			newPod := pod.DeepCopy()
			podAnnotation.EncapIP = node2Encap2.IP

			annotation, err = util.MarshalPodAnnotation(newPod.Annotations, podAnnotation, "blue-ns/blue-l2-net")
			Expect(err).NotTo(HaveOccurred())
			newPod.Annotations = annotation

			By("Calling ensurePodForUserDefinedNetwork for Pod update event with encap IP")
			err = controller.bnc.ensurePodForUserDefinedNetwork(newPod, true)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Port_Binding.encap field was updated according to encap IP")
			verifyPortBindingEncap(fakeOVN, transistSwitchPortName, node2Encap2.UUID)
		})
	})
})

func verifyPortBindingEncap(fakeOVN *FakeOVN, transistSwitchPortName string, expectedEncapUUID string) {

	var portBindings []*sbdb.PortBinding
	Expect(fakeOVN.sbClient.List(context.Background(), &portBindings)).To(Succeed())

	actualEncapUUID := ""
	for _, pb := range portBindings {
		if pb.LogicalPort == transistSwitchPortName {
			// pb.Encap points to the UUID of the Encap object
			if pb.Encap != nil {
				actualEncapUUID = *pb.Encap
			}
			Expect(actualEncapUUID).To(Equal(expectedEncapUUID))
			return
		}
	}

	Fail("No Port_Binding found for transit switch port ")
}
