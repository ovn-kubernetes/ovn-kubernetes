//go:build linux
// +build linux

package managementport

import (
	"fmt"
	"net"

	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	kubeMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func genOVSAddMgmtPortCmd(nodeName, repName string) string {
	return fmt.Sprintf("ovs-vsctl --timeout=15 -- --may-exist add-port br-int %s -- set interface %s external-ids:iface-id=%s"+
		" external-ids:%s=%s external-ids:ovn-orig-mgmt-port-rep-name=%s",
		types.K8sMgmtIntfName+"_0", types.K8sMgmtIntfName+"_0", types.K8sPrefix+nodeName, types.OvnManagementPortNameExternalID, types.K8sMgmtIntfName, repName)
}

func mockOVSListInterfaceMgmtPortNotExistCmd(execMock *ovntest.FakeExec, mgmtPortName string) {
	execMock.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
	})
}

func genGetOvsEntry(table, record, column, key string) string {
	if key != "" {
		column = column + ":" + key
	}
	return fmt.Sprintf("ovs-vsctl --timeout=15 --if-exists get %s %s %s", table, record, column)
}

var _ = Describe("Mananagement port DPU tests", func() {
	origNetlinkOps := util.GetNetLinkOps()
	var netlinkOpsMock *utilMocks.NetLinkOps
	var execMock *ovntest.FakeExec
	var nodeAnnotatorMock *kubeMocks.Annotator

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		util.ResetRunner()

		netlinkOpsMock = &utilMocks.NetLinkOps{}
		nodeAnnotatorMock = &kubeMocks.Annotator{}
		execMock = ovntest.NewFakeExec()
		err := util.SetExec(execMock)
		Expect(err).NotTo(HaveOccurred())
		util.SetNetLinkOpMockInst(netlinkOpsMock)
		nftables.SetFakeNFTablesHelper()
	})

	AfterEach(func() {
		util.SetNetLinkOpMockInst(origNetlinkOps)
	})

	Context("Create Management port DPU", func() {
		It("Fails if representor link lookup failed with error", func() {
			mgmtPortDpu := managementPortRepresentor{
				repDevName: "non-existent-netdev",
			}

			netlinkOpsMock.On("LinkByName", "non-existent-netdev").Return(nil, fmt.Errorf("netlink mock error"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(false)

			err := mgmtPortDpu.create()
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			Expect(err).To(HaveOccurred())
		})

		It("Fails if representor and ovn-k8s-mp0 netdev is not found", func() {
			mgmtPortDpu := managementPortRepresentor{
				repDevName: "non-existent-netdev",
			}
			netlinkOpsMock.On("LinkByName", "non-existent-netdev").Return(
				nil, fmt.Errorf("failed to get interface"))
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName).Return(
				nil, fmt.Errorf("failed to get interface"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)

			err := mgmtPortDpu.create()
			Expect(err).To(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
		})

		It("Fails if set Name to ovn-k8s-mp0 fails", func() {
			mgmtPortDpu := newManagementPortRepresentor(types.K8sMgmtIntfName+"_0", "enp3s0f0v0", nil)
			linkMock := &mocks.Link{}
			linkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: "enp3s0f0v0", MTU: 1400})

			netlinkOpsMock.On("LinkByName", "enp3s0f0v0").Return(
				linkMock, nil)
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName+"_0").Return(
				nil, fmt.Errorf("link not found"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
			netlinkOpsMock.On("LinkSetDown", linkMock).Return(nil)
			netlinkOpsMock.On("LinkSetName", linkMock, types.K8sMgmtIntfName+"_0").Return(fmt.Errorf("failed to set name"))
			mockOVSListInterfaceMgmtPortNotExistCmd(execMock, types.K8sMgmtIntfName+"_0")

			err := mgmtPortDpu.create()
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			Expect(err).To(HaveOccurred())
		})

		It("Configures VF representor and connects it to OVS bridge", func() {
			_, ipnet, err := net.ParseCIDR("192.168.0.1/24")
			Expect(err).ToNot(HaveOccurred())
			expectedMgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipnet).IP)
			config.Default.MTU = 1400
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "k8s-worker0",
					Annotations: map[string]string{
						"k8s.ovn.org/node-mgmt-port-mac-addresses": fmt.Sprintf(`{"default":"%s"}`, expectedMgmtPortMac.String()),
					},
				},
			}
			cfg := &managementPortConfig{
				nodeName:    "k8s-worker0",
				hostSubnets: []*net.IPNet{ipnet},
			}
			mgmtPortDpu := newManagementPortRepresentor(types.K8sMgmtIntfName+"_0", "enp3s0f0v0", cfg)
			nodeAnnotatorMock.On("Set", mock.Anything, map[string]string{"default": expectedMgmtPortMac.String()}).Return(nil)
			linkMock := &mocks.Link{}
			linkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: "enp3s0f0v0", MTU: 1500})

			netlinkOpsMock.On("LinkByName", "enp3s0f0v0").Return(
				linkMock, nil)
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName+"_0").Return(
				nil, fmt.Errorf("link not found"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
			netlinkOpsMock.On("LinkSetDown", linkMock).Return(nil)
			netlinkOpsMock.On("LinkSetName", linkMock, types.K8sMgmtIntfName+"_0").Return(nil)
			netlinkOpsMock.On("LinkSetAlias", linkMock, "enp3s0f0v0").Return(nil)
			netlinkOpsMock.On("LinkSetMTU", linkMock, config.Default.MTU).Return(nil)
			netlinkOpsMock.On("LinkSetUp", linkMock).Return(nil)
			mockOVSListInterfaceMgmtPortNotExistCmd(execMock, types.K8sMgmtIntfName+"_0")
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genGetOvsEntry("bridge", "br-int", "datapath_type", ""),
			})
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSAddMgmtPortCmd(cfg.nodeName, mgmtPortDpu.repDevName),
			})
			fakeClient := fake.NewSimpleClientset(&corev1.NodeList{
				Items: []corev1.Node{*node},
			})
			fakeNodeClient := &util.OVNNodeClientset{
				KubeClient: fakeClient,
			}
			watchFactory, err := factory.NewNodeWatchFactory(fakeNodeClient, node.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(watchFactory.Start()).To(Succeed())

			err = mgmtPortDpu.create()
			Expect(err).ToNot(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			Expect(mgmtPortDpu.link).To(Equal(linkMock))
		})

		It("Brings interface up and attemps to add ovn-k8s-mp0 to OVS if interface already configured", func() {
			_, ipnet, err := net.ParseCIDR("192.168.0.1/24")
			Expect(err).ToNot(HaveOccurred())
			expectedMgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipnet).IP)
			config.Default.MTU = 1400
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "k8s-worker0",
					Annotations: map[string]string{
						"k8s.ovn.org/node-mgmt-port-mac-addresses": fmt.Sprintf(`{"default":"%s"}`, expectedMgmtPortMac.String()),
					},
				},
			}
			cfg := &managementPortConfig{
				nodeName:    "k8s-worker0",
				hostSubnets: []*net.IPNet{ipnet},
			}
			mgmtPortDpu := newManagementPortRepresentor(types.K8sMgmtIntfName+"_0", "enp3s0f0v0", cfg)
			nodeAnnotatorMock.On("Set", mock.Anything, map[string]string{"default": expectedMgmtPortMac.String()}).Return(nil)
			linkMock := &mocks.Link{}
			linkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: types.K8sMgmtIntfName + "_0", MTU: config.Default.MTU})

			netlinkOpsMock.On("LinkByName", "enp3s0f0v0").Return(
				linkMock, nil)
			netlinkOpsMock.On("LinkSetUp", linkMock).Return(nil)
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genGetOvsEntry("bridge", "br-int", "datapath_type", ""),
			})
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSAddMgmtPortCmd(cfg.nodeName, mgmtPortDpu.repDevName),
			})

			fakeClient := fake.NewSimpleClientset(&corev1.NodeList{
				Items: []corev1.Node{*node},
			})
			fakeNodeClient := &util.OVNNodeClientset{
				KubeClient: fakeClient,
			}
			watchFactory, err := factory.NewNodeWatchFactory(fakeNodeClient, node.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(watchFactory.Start()).To(Succeed())

			err = mgmtPortDpu.create()
			Expect(err).ToNot(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			Expect(mgmtPortDpu.link).To(Equal(linkMock))
		})
	})

	Context("findNetdevByDeviceID", func() {
		It("Returns error when deviceID is empty", func() {
			mgmtPortDpuHost := &managementPortNetdev{
				netdevDevName: "enp3s0f0v0",
				deviceID:      "",
			}

			link, err := mgmtPortDpuHost.findNetdevByDeviceID()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no device ID available"))
			Expect(link).To(BeNil())
		})
	})

	Context("Create Management port DPU host", func() {
		It("Fails if netdev link lookup failed", func() {
			mgmtPortDpuHost := managementPortNetdev{
				netdevDevName: "non-existent-netdev",
			}
			netlinkOpsMock.On("LinkByName", "non-existent-netdev").Return(nil, fmt.Errorf("netlink mock error"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(false)

			err := mgmtPortDpuHost.create()
			Expect(err).To(HaveOccurred())
		})

		It("Fails if netdev does not exist", func() {
			mgmtPortDpuHost := managementPortNetdev{
				netdevDevName: "non-existent-netdev",
			}
			netlinkOpsMock.On("LinkByName", "non-existent-netdev").Return(
				nil, fmt.Errorf("failed to get interface"))
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName).Return(
				nil, fmt.Errorf("failed to get interface"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)

			err := mgmtPortDpuHost.create()
			Expect(err).To(HaveOccurred())
		})

		It("Configures VF and calls createPlatformManagementPort", func() {
			_, ipnet, err := net.ParseCIDR("192.168.0.1/24")
			Expect(err).ToNot(HaveOccurred())
			expectedMgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipnet).IP)
			currentMgmtPortMac, err := net.ParseMAC("00:bb:cc:dd:ee:11")
			Expect(err).ToNot(HaveOccurred())
			config.Default.MTU = 1400
			cfg := &managementPortConfig{
				hostSubnets: []*net.IPNet{ipnet},
			}
			mgmtPortDpuHost := newManagementPortNetdev("enp3s0f0v0", "", cfg, nil)
			linkMock := &mocks.Link{}
			linkMock.On("Attrs").Return(&netlink.LinkAttrs{
				Name: "enp3s0f0v0", MTU: 1500, HardwareAddr: currentMgmtPortMac})

			netlinkOpsMock.On("LinkByName", "enp3s0f0v0").Return(
				linkMock, nil)
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
			netlinkOpsMock.On("LinkSetDown", linkMock).Return(nil)
			netlinkOpsMock.On("LinkSetHardwareAddr", linkMock, expectedMgmtPortMac).Return(nil)
			netlinkOpsMock.On("LinkSetName", linkMock, types.K8sMgmtIntfName).Return(nil)
			netlinkOpsMock.On("LinkSetAlias", linkMock, "enp3s0f0v0").Return(nil)
			netlinkOpsMock.On("LinkSetMTU", linkMock, config.Default.MTU).Return(nil)
			netlinkOpsMock.On("LinkSetUp", linkMock).Return(nil, nil)
			mockOVSListInterfaceMgmtPortNotExistCmd(execMock, types.K8sMgmtIntfName)
			execMock.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15 set Open_vSwitch . external-ids:ovn-orig-mgmt-port-netdev-name=" + mgmtPortDpuHost.netdevDevName,
			})

			// mock createPlatformManagementPort, we fail it as it covers what we want to test without the
			// need to mock the entire flow down to routes and iptable rules.
			netlinkOpsMock.On("LinkByName", mock.Anything).Return(nil, fmt.Errorf(
				"createPlatformManagementPort error"))

			err = mgmtPortDpuHost.create()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("createPlatformManagementPort error"))
		})

		It("Does not configure VF if already configured", func() {
			_, ipnet, err := net.ParseCIDR("192.168.0.1/24")
			Expect(err).ToNot(HaveOccurred())
			_, clusterCidr, err := net.ParseCIDR("192.168.0.0/16")
			Expect(err).ToNot(HaveOccurred())
			expectedMgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipnet).IP)
			config.Default.MTU = 1400
			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: clusterCidr, HostSubnetLength: 8}}
			cfg := &managementPortConfig{
				hostSubnets: []*net.IPNet{ipnet},
			}
			mgmtPortDpuHost := newManagementPortNetdev("enp3s0f0v0", "", cfg, nil)
			linkMock := &mocks.Link{}
			linkMock.On("Attrs").Return(&netlink.LinkAttrs{
				Name: types.K8sMgmtIntfName, MTU: 1400, HardwareAddr: expectedMgmtPortMac})

			// VF already configured: LinkByName returns the link with the final name, correct MTU & MAC.
			// Since all attributes already match, bringupManagementPortLink only calls LinkSetUp
			// (no LinkSetDown, rename, MAC or MTU changes).
			netlinkOpsMock.On("LinkByName", "enp3s0f0v0").Return(linkMock, nil).Once()
			netlinkOpsMock.On("LinkSetUp", linkMock).Return(nil)
			execMock.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15 set Open_vSwitch . external-ids:ovn-orig-mgmt-port-netdev-name=" + mgmtPortDpuHost.netdevDevName,
			})

			// mock createPlatformManagementPort, we fail it as it covers what we want to test without the
			// need to mock the entire flow down to routes and iptable rules.
			netlinkOpsMock.On("LinkByName", mock.Anything).Return(nil, fmt.Errorf(
				"createPlatformManagementPort error"))

			err = mgmtPortDpuHost.create()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(
				"createPlatformManagementPort error"))
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
		})
	})
})
