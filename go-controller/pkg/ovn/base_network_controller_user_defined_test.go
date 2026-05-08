// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"sync"
	"time"

	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

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
	Context("enableSourceLSPFailedLiveMigration", func() {
		const (
			vmName        = "test-vm"
			nadKey        = "awips/mgmt"
			localNodeName = "node-local"
		)

		newVirtLauncherPod := func(name, nodeName string, phase corev1.PodPhase, annotations map[string]string) *corev1.Pod {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:              name,
					Namespace:         "awips",
					Labels:            map[string]string{kubevirtv1.VirtualMachineNameLabel: vmName},
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
			config.OVNKubernetesFeature.EnableInterconnect = true
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
			config.OVNKubernetesFeature.EnableInterconnect = true
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
			config.OVNKubernetesFeature.EnableInterconnect = true
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
