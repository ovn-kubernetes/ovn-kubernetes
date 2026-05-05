// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dpu

import (
	"fmt"

	"github.com/stretchr/testify/mock"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	factorymocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory/mocks"
	kubemocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	linkMock "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	coreinformermocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	v1mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func genOVSFindCmd(timeout, table, column, condition string) string {
	return fmt.Sprintf("ovs-vsctl --timeout=%s --no-heading --format=csv --data=bare --columns=%s find %s %s",
		timeout, column, table, condition)
}

func genOVSAddPortCmd(hostIfaceName, ifaceID, mac, ip, sandboxID, podUID string) string {
	ipAddrExtID := ""
	if ip != "" {
		ipAddrExtID = fmt.Sprintf("external_ids:ip_addresses=%s ", ip)
	}
	return fmt.Sprintf("ovs-vsctl --timeout=30 --may-exist add-port br-int %s other_config:transient=true "+
		"-- set interface %s external_ids:attached_mac=%s external_ids:iface-id=%s external_ids:iface-id-ver=%s "+
		"%sexternal_ids:sandbox=%s external_ids:vf-netdev-name=%s "+
		"-- --if-exists remove interface %s external_ids k8s.ovn.org/network "+
		"-- --if-exists remove interface %s external_ids k8s.ovn.org/nad",
		hostIfaceName, hostIfaceName, mac, ifaceID, podUID, ipAddrExtID, sandboxID, hostIfaceName, hostIfaceName, hostIfaceName)
}

func genOVSGetCmd(table, record, column, key string) string {
	if key != "" {
		column = column + ":" + key
	}
	return fmt.Sprintf("ovs-vsctl --timeout=30 --if-exists get %s %s %s", table, record, column)
}

func genIfaceID(podNamespace, podName string) string {
	return fmt.Sprintf("%s_%s", podNamespace, podName)
}

func checkOVSPortPodInfo(execMock *ovntest.FakeExec, vfRep string, exists bool, timeout, sandbox string, nadName string) {
	output := ""
	if exists {
		output = fmt.Sprintf("sandbox=%s", sandbox)
		if nadName != types.DefaultNetworkName {
			output = output + " k8s.ovn.org/nad=" + nadName
		}
	}
	execMock.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    genOVSFindCmd(timeout, "Interface", "external_ids", "name="+vfRep),
		Output: output,
	})
}

func newFakeKubeClientWithPod(pod *corev1.Pod) *fake.Clientset {
	return fake.NewSimpleClientset(&corev1.PodList{Items: []corev1.Pod{*pod}})
}

var _ = Describe("Node DPU tests", func() {
	var sriovnetOpsMock utilMocks.SriovnetOps
	var netlinkOpsMock utilMocks.NetLinkOps
	var execMock *ovntest.FakeExec
	var kubeMock kubemocks.Interface
	var factoryMock factorymocks.NodeWatchFactory
	var pod corev1.Pod
	var ctrl *Controller
	var podInformer coreinformermocks.PodInformer
	var podLister v1mocks.PodLister
	var podNamespaceLister v1mocks.PodNamespaceLister
	var clientset *cni.ClientSet
	var libovsdbCleanup *libovsdbtest.Context

	origSriovnetOps := util.GetSriovnetOps()
	origNetlinkOps := util.GetNetLinkOps()

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		sriovnetOpsMock = utilMocks.SriovnetOps{}
		netlinkOpsMock = utilMocks.NetLinkOps{}
		execMock = ovntest.NewFakeExec()

		util.SetSriovnetOpsInst(&sriovnetOpsMock)
		util.SetNetLinkOpMockInst(&netlinkOpsMock)
		err := util.SetExec(execMock)
		Expect(err).NotTo(HaveOccurred())
		err = cni.SetExec(execMock)
		Expect(err).NotTo(HaveOccurred())

		kubeMock = kubemocks.Interface{}

		ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"bridge-br-int-uuid"}},
				&vswitchd.Bridge{UUID: "bridge-br-int-uuid", Name: "br-int", Ports: []string{"port-pf0vf9-uuid"}},
				&vswitchd.Port{UUID: "port-pf0vf9-uuid", Name: "pf0vf9", Interfaces: []string{"intf-pf0vf9-uuid"}},
				&vswitchd.Interface{UUID: "intf-pf0vf9-uuid", Name: "pf0vf9"},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		libovsdbCleanup = testCtx

		factoryMock = factorymocks.NodeWatchFactory{}
		ctrl = &Controller{
			kube:         &kubeMock,
			watchFactory: &factoryMock,
			ovsClient:    ovsClient,
		}

		podInformer = coreinformermocks.PodInformer{}
		podNamespaceLister = v1mocks.PodNamespaceLister{}
		podLister = v1mocks.PodLister{}
		podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)

		pod = corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:        "a-pod",
			Namespace:   "foo-ns",
			UID:         "a-pod",
			Annotations: map[string]string{},
		}}
	})

	AfterEach(func() {
		if libovsdbCleanup != nil {
			libovsdbCleanup.Cleanup()
		}
		util.SetSriovnetOpsInst(origSriovnetOps)
		util.SetNetLinkOpMockInst(origNetlinkOps)
		cni.ResetRunner()
		util.ResetRunner()
	})

	Context("addRepPort", func() {
		var vfRep string
		var vfPciAddress string
		var vfLink *linkMock.Link
		var ifInfo *cni.PodInterfaceInfo
		var state *dpuConnectionState

		BeforeEach(func() {
			vfRep = "pf0vf9"
			vfPciAddress = "0000:03:00.0"
			vfLink = &linkMock.Link{}
			ifInfo = &cni.PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				Ingress:       -1,
				Egress:        -1,
				IsDPUHostMode: true,
				NetName:       types.DefaultNetworkName,
				NADKey:        types.DefaultNetworkName,
				PodUID:        "a-pod",
			}

			fakeClient := newFakeKubeClientWithPod(&pod)
			clientset = cni.NewClientSet(fakeClient, &podLister)
			scd := util.DPUConnectionDetails{
				PfId:      "0",
				VfId:      "9",
				SandboxId: "a8d09931",
			}
			state = &dpuConnectionState{
				vfRepName: vfRep,
				sandboxId: scd.SandboxId,
			}
			podAnnot, err := util.MarshalPodDPUConnDetails(nil, &scd, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			pod.Annotations = podAnnot
		})

		It("Fails if GetPCIFromDeviceName fails", func() {
			sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return("", fmt.Errorf("could not find PCI Address"))
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

			err := ctrl.addRepPort(&pod, state, ifInfo, clientset)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("could not find PCI Address"))
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Fails if configure OVS fails", func() {
			sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return(vfPciAddress, nil)
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSGetCmd("bridge", "br-int", "datapath_type", ""),
			})
			// set ovs CMD output
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSFindCmd("30", "Interface", "name",
					"external-ids:iface-id="+genIfaceID(pod.Namespace, pod.Name)),
			})
			checkOVSPortPodInfo(execMock, vfRep, false, "30", "", "")
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOVSGetCmd("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping"),
				Output: "",
			})
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSAddPortCmd(vfRep, genIfaceID(pod.Namespace, pod.Name), "", "", "a8d09931", string(pod.UID)),
				Err: fmt.Errorf("failed to run ovs command"),
			})
			// Mock netlink/ovs calls for cleanup
			checkOVSPortPodInfo(execMock, vfRep, false, "15", "", "")

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

			err := ctrl.addRepPort(&pod, state, ifInfo, clientset)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to run ovs command"))
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Fails if configure OVS fails but OVS interface is added", func() {
			sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return(vfPciAddress, nil)
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSGetCmd("bridge", "br-int", "datapath_type", ""),
			})
			// set ovs CMD output
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSFindCmd("30", "Interface", "name",
					"external-ids:iface-id="+genIfaceID(pod.Namespace, pod.Name)),
			})
			checkOVSPortPodInfo(execMock, vfRep, false, "30", "", "")
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOVSGetCmd("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping"),
				Output: "",
			})
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSAddPortCmd(vfRep, genIfaceID(pod.Namespace, pod.Name), "", "", "a8d09931", string(pod.UID)),
				Err: fmt.Errorf("failed to run ovs command"),
			})
			checkOVSPortPodInfo(execMock, vfRep, true, "15", "a8d09931", "default")
			// Mock netlink/ovs calls for cleanup (ovs del-port now via libovsdb, not ovs-vsctl)
			netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

			err := ctrl.addRepPort(&pod, state, ifInfo, clientset)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to run ovs command"))
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		Context("After successfully calling ConfigureOVS", func() {
			BeforeEach(func() {
				sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return(vfPciAddress, nil)
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genOVSGetCmd("bridge", "br-int", "datapath_type", ""),
				})
				// set ovs CMD output so cni.ConfigureOVS passes without error
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genOVSFindCmd("30", "Interface", "name",
						"external-ids:iface-id="+genIfaceID(pod.Namespace, pod.Name)),
				})
				checkOVSPortPodInfo(execMock, vfRep, false, "30", "", "")
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genOVSGetCmd("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping"),
					Output: "",
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genOVSAddPortCmd(vfRep, genIfaceID(pod.Namespace, pod.Name), "", "", "a8d09931", string(pod.UID)),
				})
				// clearPodBandwidth
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genOVSFindCmd("30", "interface", "name",
						"external-ids:sandbox=a8d09931"),
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genOVSFindCmd("30", "qos", "_uuid",
						"external-ids:sandbox=a8d09931"),
				})
			})

			Context("Fails if link configuration fails on", func() {
				It("LinkByName()", func() {
					netlinkOpsMock.On("LinkByName", vfRep).Return(nil, fmt.Errorf("failed to get link"))
					// Mock ovs calls for cleanup (ovs del-port now via libovsdb)
					checkOVSPortPodInfo(execMock, vfRep, true, "15", "a8d09931", "default")

					podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

					err := ctrl.addRepPort(&pod, state, ifInfo, clientset)
					Expect(err).To(HaveOccurred())
					Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
				})

				It("LinkSetMTU()", func() {
					netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
					netlinkOpsMock.On("LinkSetMTU", vfLink, ifInfo.MTU).Return(fmt.Errorf("failed to set mtu"))
					// Mock netlink/ovs calls for cleanup (ovs del-port now via libovsdb)
					checkOVSPortPodInfo(execMock, vfRep, true, "15", "a8d09931", "default")
					netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

					podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

					err := ctrl.addRepPort(&pod, state, ifInfo, clientset)
					Expect(err).To(HaveOccurred())
					Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
				})

				It("LinkSetUp()", func() {
					netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
					netlinkOpsMock.On("LinkSetMTU", vfLink, ifInfo.MTU).Return(nil)
					netlinkOpsMock.On("LinkSetUp", vfLink).Return(fmt.Errorf("failed to set link up"))
					// Mock netlink/ovs calls for cleanup (ovs del-port now via libovsdb)
					checkOVSPortPodInfo(execMock, vfRep, true, "15", "a8d09931", "default")
					netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

					podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

					err := ctrl.addRepPort(&pod, state, ifInfo, clientset)
					Expect(err).To(HaveOccurred())
					Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
				})
			})

			It("Sets dpu.connection-status pod annotation on success", func() {
				var err error
				netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
				netlinkOpsMock.On("LinkSetMTU", vfLink, ifInfo.MTU).Return(nil)
				netlinkOpsMock.On("LinkSetUp", vfLink).Return(nil)
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genOVSGetCmd("Interface", "pf0vf9", "external-ids", "iface-id") + " external-ids:ovn-installed",
					Output: genIfaceID(pod.Namespace, pod.Name) + "\n" + "true",
				})
				dcs := util.DPUConnectionStatus{
					Status: "Ready",
				}
				cpod := pod.DeepCopy()
				cpod.Annotations, err = util.MarshalPodDPUConnStatus(cpod.Annotations, map[string]*util.DPUConnectionStatus{types.DefaultNetworkName: &dcs})
				Expect(err).ToNot(HaveOccurred())

				factoryMock.On("PodCoreInformer").Return(&podInformer)
				podInformer.On("Lister").Return(&podLister)
				podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)
				podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)
				kubeMock.On("PatchPodStatusAnnotations", &pod, cpod).Return(nil)

				err = ctrl.addRepPort(&pod, state, ifInfo, clientset)
				Expect(err).ToNot(HaveOccurred())
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
			})

			It("cleans up representor port if set pod annotation fails", func() {
				var err error
				netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
				netlinkOpsMock.On("LinkSetMTU", vfLink, ifInfo.MTU).Return(nil)
				netlinkOpsMock.On("LinkSetUp", vfLink).Return(nil)
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genOVSGetCmd("Interface", "pf0vf9", "external-ids", "iface-id") + " external-ids:ovn-installed",
					Output: genIfaceID(pod.Namespace, pod.Name) + "\n" + "true",
				})
				dcs := util.DPUConnectionStatus{
					Status: "Ready",
				}
				cpod := pod.DeepCopy()
				cpod.Annotations, err = util.MarshalPodDPUConnStatus(cpod.Annotations, map[string]*util.DPUConnectionStatus{types.DefaultNetworkName: &dcs})
				Expect(err).ToNot(HaveOccurred())
				checkOVSPortPodInfo(execMock, vfRep, true, "15", "a8d09931", "default")
				netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

				factoryMock.On("PodCoreInformer").Return(&podInformer)
				podInformer.On("Lister").Return(&podLister)
				podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)
				podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)
				kubeMock.On("PatchPodStatusAnnotations", &pod, cpod).Return(fmt.Errorf("failed to set pod annotations"))

				err = ctrl.addRepPort(&pod, state, ifInfo, clientset)
				Expect(err).To(HaveOccurred())
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
			})
		})
	})

	Context("delRepPort", func() {
		var vfRep string
		var vfLink *linkMock.Link
		var state *dpuConnectionState

		BeforeEach(func() {
			vfRep = "pf0vf9"
			vfLink = &linkMock.Link{}
			state = &dpuConnectionState{
				vfRepName: vfRep,
				sandboxId: "a8d09931",
			}
		})

		It("Sets link down for VF representor and removes VF representor from OVS", func() {
			checkOVSPortPodInfo(execMock, vfRep, true, "15", state.sandboxId, types.DefaultNetworkName)
			netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)
			err := ctrl.delRepPort(&pod, state, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Does not fail if LinkByName failed", func() {
			checkOVSPortPodInfo(execMock, vfRep, true, "15", state.sandboxId, types.DefaultNetworkName)
			netlinkOpsMock.On("LinkByName", vfRep).Return(nil, fmt.Errorf("failed to get link"))
			err := ctrl.delRepPort(&pod, state, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})
	})

	Context("bootstrapDPUPodMapFromOVS", func() {
		// Each test creates its own OVS harness with the specific interface data it needs,
		// because the shared BeforeEach sets up a plain interface without DPU external IDs.

		It("No-ops when no representor interfaces exist", func() {
			// The shared harness has an interface without sandbox/vf-netdev-name,
			// so FindInterfacesWithPredicate returns nothing.
			factoryMock.On("GetAllPods").Return([]*corev1.Pod{}, nil)

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			// Maps should remain empty
			count := 0
			ctrl.podNADToDPUCDMap.Range(func(_, _ interface{}) bool { count++; return true })
			Expect(count).To(Equal(0))
		})

		It("Populates state for an existing pod on default network", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb1", vfNetdevName: "pf0vf9", ifaceID: "foo-ns_a-pod", ifaceIDVer: "uid-1"},
			})
			defer cleanup()

			existingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "a-pod", Namespace: "foo-ns", UID: "uid-1",
			}}
			factoryMock.On("GetAllPods").Return([]*corev1.Pod{existingPod}, nil)

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			v, ok := ctrl.podNADToDPUCDMap.Load(k8stypes.UID("uid-1"))
			Expect(ok).To(BeTrue())
			ps := v.(*podDPUState)
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))
			Expect(ps.nadStates[types.DefaultNetworkName].vfRepName).To(Equal("pf0vf9"))
			Expect(ps.nadStates[types.DefaultNetworkName].sandboxId).To(Equal("sb1"))

			uidVal, ok := ctrl.podKeyToUID.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(uidVal).To(Equal(k8stypes.UID("uid-1")))
		})

		It("Populates state for an existing pod on a UDN", func() {
			udnNADKey := "ns1/nad1"
			udnPrefix := util.GetUserDefinedNetworkPrefix(udnNADKey)
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: "pf0vf10", sandbox: "sb2", vfNetdevName: "pf0vf10",
					ifaceID: udnPrefix + "foo-ns_a-pod", ifaceIDVer: "uid-2",
					nadKey: udnNADKey,
				},
			})
			defer cleanup()

			existingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "a-pod", Namespace: "foo-ns", UID: "uid-2",
			}}
			factoryMock.On("GetAllPods").Return([]*corev1.Pod{existingPod}, nil)

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			v, ok := ctrl.podNADToDPUCDMap.Load(k8stypes.UID("uid-2"))
			Expect(ok).To(BeTrue())
			ps := v.(*podDPUState)
			Expect(ps.nadStates).To(HaveKey(udnNADKey))
			Expect(ps.nadStates[udnNADKey].vfRepName).To(Equal("pf0vf10"))
			Expect(ps.nadStates[udnNADKey].sandboxId).To(Equal("sb2"))

			uidVal, ok := ctrl.podKeyToUID.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(uidVal).To(Equal(k8stypes.UID("uid-2")))
		})

		It("Deletes orphaned representor port when pod no longer exists", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb-old", vfNetdevName: "pf0vf9", ifaceID: "foo-ns_gone-pod", ifaceIDVer: "uid-gone"},
			})
			defer cleanup()

			// No pods exist
			factoryMock.On("GetAllPods").Return([]*corev1.Pod{}, nil)
			netlinkOpsMock.On("LinkByName", "pf0vf9").Return(nil, fmt.Errorf("no link"))

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			// Maps should be empty since the pod doesn't exist
			count := 0
			ctrl.podNADToDPUCDMap.Range(func(_, _ interface{}) bool { count++; return true })
			Expect(count).To(Equal(0))
		})

		It("Handles mix of existing and orphaned ports", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb1", vfNetdevName: "pf0vf9", ifaceID: "foo-ns_alive-pod", ifaceIDVer: "uid-alive"},
				{name: "pf0vf10", sandbox: "sb2", vfNetdevName: "pf0vf10", ifaceID: "foo-ns_dead-pod", ifaceIDVer: "uid-dead"},
			})
			defer cleanup()

			alivePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "alive-pod", Namespace: "foo-ns", UID: "uid-alive",
			}}
			factoryMock.On("GetAllPods").Return([]*corev1.Pod{alivePod}, nil)
			netlinkOpsMock.On("LinkByName", "pf0vf10").Return(nil, fmt.Errorf("no link"))

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			// Alive pod should be populated
			v, ok := ctrl.podNADToDPUCDMap.Load(k8stypes.UID("uid-alive"))
			Expect(ok).To(BeTrue())
			ps := v.(*podDPUState)
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))

			// Dead pod should not be in the map
			_, ok = ctrl.podNADToDPUCDMap.Load(k8stypes.UID("uid-dead"))
			Expect(ok).To(BeFalse())
		})

		It("Populates multi-NAD state for the same pod with indexed NAD key", func() {
			// Use an indexed NAD key ("ns1/nad1/1") to verify that the full
			// indexed key is stored in the map and podKeyFromIfaceID correctly
			// strips the prefix derived from it.
			indexedNADKey := "ns1/nad1/1"
			udnPrefix := util.GetUserDefinedNetworkPrefix(indexedNADKey)
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb1", vfNetdevName: "pf0vf9", ifaceID: "foo-ns_multi-pod", ifaceIDVer: "uid-multi"},
				{
					name: "pf0vf10", sandbox: "sb1", vfNetdevName: "pf0vf10",
					ifaceID: udnPrefix + "foo-ns_multi-pod", ifaceIDVer: "uid-multi",
					nadKey: indexedNADKey,
				},
			})
			defer cleanup()

			multiPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "multi-pod", Namespace: "foo-ns", UID: "uid-multi",
			}}
			factoryMock.On("GetAllPods").Return([]*corev1.Pod{multiPod}, nil)

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			v, ok := ctrl.podNADToDPUCDMap.Load(k8stypes.UID("uid-multi"))
			Expect(ok).To(BeTrue())
			ps := v.(*podDPUState)
			Expect(ps.nadStates).To(HaveLen(2))
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))
			Expect(ps.nadStates[types.DefaultNetworkName].vfRepName).To(Equal("pf0vf9"))
			Expect(ps.nadStates).To(HaveKey(indexedNADKey))
			Expect(ps.nadStates[indexedNADKey].vfRepName).To(Equal("pf0vf10"))
		})

		It("Skips interfaces with incomplete external IDs", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				// Missing iface-id
				{name: "pf0vf9", sandbox: "sb1", vfNetdevName: "pf0vf9", ifaceIDVer: "uid-1"},
				// Valid interface
				{name: "pf0vf10", sandbox: "sb2", vfNetdevName: "pf0vf10", ifaceID: "foo-ns_ok-pod", ifaceIDVer: "uid-ok"},
			})
			defer cleanup()

			okPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "ok-pod", Namespace: "foo-ns", UID: "uid-ok",
			}}
			factoryMock.On("GetAllPods").Return([]*corev1.Pod{okPod}, nil)

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			// Only the valid pod should be populated
			_, ok := ctrl.podNADToDPUCDMap.Load(k8stypes.UID("uid-1"))
			Expect(ok).To(BeFalse())

			v, ok := ctrl.podNADToDPUCDMap.Load(k8stypes.UID("uid-ok"))
			Expect(ok).To(BeTrue())
			ps := v.(*podDPUState)
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))
		})

		It("Populates existing pod state even when orphan cleanup runs", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb1", vfNetdevName: "pf0vf9", ifaceID: "foo-ns_orphan", ifaceIDVer: "uid-orphan"},
				{name: "pf0vf10", sandbox: "sb2", vfNetdevName: "pf0vf10", ifaceID: "foo-ns_alive-pod", ifaceIDVer: "uid-alive"},
			})
			defer cleanup()

			alivePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "alive-pod", Namespace: "foo-ns", UID: "uid-alive",
			}}
			factoryMock.On("GetAllPods").Return([]*corev1.Pod{alivePod}, nil)
			netlinkOpsMock.On("LinkByName", "pf0vf9").Return(nil, fmt.Errorf("no link"))

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			// Alive pod state should be populated
			v, ok := ctrl.podNADToDPUCDMap.Load(k8stypes.UID("uid-alive"))
			Expect(ok).To(BeTrue())
			ps := v.(*podDPUState)
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))

			// Orphan should not be in the map
			_, ok = ctrl.podNADToDPUCDMap.Load(k8stypes.UID("uid-orphan"))
			Expect(ok).To(BeFalse())
		})
	})

	Context("getNetInfoForNADKey", func() {
		var fakeNetMgr *networkmanager.FakeNetworkManager

		BeforeEach(func() {
			fakeNetMgr = &networkmanager.FakeNetworkManager{
				NADNetworks: map[string]util.NetInfo{
					"ns1/nad1": &util.DefaultNetInfo{},
				},
			}
			ctrl.networkMgr = fakeNetMgr
		})

		It("Returns NetInfo for default network", func() {
			ni := ctrl.getNetInfoForNADKey(types.DefaultNetworkName)
			Expect(ni).NotTo(BeNil())
		})

		It("Returns NetInfo for base NAD key", func() {
			ni := ctrl.getNetInfoForNADKey("ns1/nad1")
			Expect(ni).NotTo(BeNil())
		})

		It("Returns NetInfo for indexed NAD key", func() {
			ni := ctrl.getNetInfoForNADKey("ns1/nad1/1")
			Expect(ni).NotTo(BeNil())
		})

		It("Returns nil for unknown NAD key", func() {
			ni := ctrl.getNetInfoForNADKey("ns1/unknown")
			Expect(ni).To(BeNil())
		})
	})
})

// ovsInterfaceData holds the external IDs for setting up an OVS interface in tests.
type ovsInterfaceData struct {
	name         string
	sandbox      string
	vfNetdevName string
	ifaceID      string
	ifaceIDVer   string
	nadKey       string // empty = default network
}

// setupOVSHarnessWithInterfaces creates a fresh OVS test harness with the specified
// interfaces on br-int and updates ctrl.ovsClient. Returns a cleanup function.
func setupOVSHarnessWithInterfaces(ctrl *Controller, ifaces []ovsInterfaceData) func() {
	var portUUIDs []string
	var ovsData []libovsdbtest.TestData

	for _, iface := range ifaces {
		portUUID := fmt.Sprintf("port-%s-uuid", iface.name)
		intfUUID := fmt.Sprintf("intf-%s-uuid", iface.name)
		portUUIDs = append(portUUIDs, portUUID)

		extIDs := map[string]string{}
		if iface.sandbox != "" {
			extIDs["sandbox"] = iface.sandbox
		}
		if iface.vfNetdevName != "" {
			extIDs["vf-netdev-name"] = iface.vfNetdevName
		}
		if iface.ifaceID != "" {
			extIDs["iface-id"] = iface.ifaceID
		}
		if iface.ifaceIDVer != "" {
			extIDs["iface-id-ver"] = iface.ifaceIDVer
		}
		if iface.nadKey != "" {
			extIDs[types.NADExternalID] = iface.nadKey
		}

		ovsData = append(ovsData,
			&vswitchd.Port{UUID: portUUID, Name: iface.name, Interfaces: []string{intfUUID}},
			&vswitchd.Interface{UUID: intfUUID, Name: iface.name, ExternalIDs: extIDs},
		)
	}

	ovsData = append(ovsData,
		&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"bridge-br-int-uuid"}},
		&vswitchd.Bridge{UUID: "bridge-br-int-uuid", Name: "br-int", Ports: portUUIDs},
	)

	ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: ovsData,
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	ctrl.ovsClient = ovsClient
	return testCtx.Cleanup
}
