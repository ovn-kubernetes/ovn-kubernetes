package netlinkdevicemanager

import (
	"errors"
	"fmt"
	"net"

	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	nl "github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"

	"k8s.io/utils/ptr"

	controllerPkg "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NetlinkDeviceManager", func() {
	var (
		controller *Controller
		nlMock     *mocks.NetLinkOps
	)

	BeforeEach(func() {
		controller = NewController()
		nlMock = &mocks.NetLinkOps{}
		util.SetNetLinkOpMockInst(nlMock)
	})

	AfterEach(func() {
		util.ResetNetLinkOpMockInst()
	})

	Describe("Reconciling a new device", func() {
		It("creates a bridge and transitions to Ready", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			createdBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br0",
				Index: 10,
			}}

			// applyDeviceConfig: device doesn't exist
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			// createLink: LinkAdd + re-fetch + set alias
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil)
			nlMock.On("LinkByName", "br0").Return(createdBridge, nil)
			nlMock.On("LinkSetAlias", createdBridge, managedAliasPrefix+"bridge:br0").Return(nil)
			// ensureDeviceUp: idempotent, always called
			nlMock.On("LinkSetUp", createdBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("creates a VXLAN with master and bridge port settings", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					FlowBased: true,
					VniFilter: true,
				},
				Master:             "br0",
				BridgePortSettings: &BridgePortSettings{VLANTunnel: true, NeighSuppress: true, Learning: false},
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10}}
			createdVxlan := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{
				Name:  "vxlan0",
				Index: 20,
			}}

			// applyDeviceConfig: VXLAN doesn't exist
			nlMock.On("LinkByName", "vxlan0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			// master resolution
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			// createLink: LinkAdd + re-fetch + set alias
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vxlan")).Return(nil)
			nlMock.On("LinkByName", "vxlan0").Return(createdVxlan, nil)
			nlMock.On("LinkSetAlias", createdVxlan, managedAliasPrefix+"vxlan:vxlan0").Return(nil)
			// ensureMaster + bridge port settings
			nlMock.On("LinkSetMaster", createdVxlan, bridgeLink).Return(nil)
			nlMock.On("LinkSetVlanTunnel", createdVxlan, true).Return(nil)
			nlMock.On("LinkSetBrNeighSuppress", createdVxlan, true).Return(nil)
			nlMock.On("LinkSetLearning", createdVxlan, false).Return(nil)
			nlMock.On("LinkSetIsolated", createdVxlan, false).Return(nil)
			// ensureDeviceUp
			nlMock.On("LinkSetUp", createdVxlan).Return(nil)

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("creates a device with IP addresses", func() {
			desiredAddr := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}
			Expect(controller.EnsureLink(DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{desiredAddr},
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			createdDummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{
				Name:  "dummy0",
				Index: 5,
			}}

			nlMock.On("LinkByName", "dummy0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Dummy")).Return(nil)
			nlMock.On("LinkByName", "dummy0").Return(createdDummy, nil)
			nlMock.On("LinkSetAlias", createdDummy, managedAliasPrefix+"dummy:dummy0").Return(nil)
			nlMock.On("LinkSetUp", createdDummy).Return(nil)
			// syncAddresses: no current addresses -> add desired
			nlMock.On("AddrList", createdDummy, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", createdDummy, mock.MatchedBy(func(addr *netlink.Addr) bool {
				return addr.IPNet.String() == "10.0.0.1/32"
			})).Return(nil)

			Expect(controller.reconcileDeviceKey("dummy0")).To(Succeed())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("creates a VLAN with resolved VLANParent", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vlan100"},
					VlanId:    100,
				},
				VLANParent: "br0",
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			parentBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10}}
			createdVlan := &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{
				Name:        "vlan100",
				Index:       30,
				ParentIndex: 10,
			}, VlanId: 100}

			// applyDeviceConfig: VLAN doesn't exist
			nlMock.On("LinkByName", "vlan100").Return(nil, linkNotFoundErr).Once()
			// VLANParent resolution
			nlMock.On("LinkByName", "br0").Return(parentBridge, nil)
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			// createLink: LinkAdd + re-fetch + set alias
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vlan")).Return(nil)
			nlMock.On("LinkByName", "vlan100").Return(createdVlan, nil)
			nlMock.On("LinkSetAlias", createdVlan, managedAliasPrefix+"vlan:vlan100").Return(nil)
			// ensureDeviceUp
			nlMock.On("LinkSetUp", createdVlan).Return(nil)

			Expect(controller.reconcileDeviceKey("vlan100")).To(Succeed())
			nlMock.AssertExpectations(GinkgoT())
		})
	})

	Describe("Reconciling an existing device", func() {
		It("updates master when it changes", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					FlowBased: true,
					VniFilter: true,
				},
				Master:             "br-new",
				BridgePortSettings: &BridgePortSettings{VLANTunnel: true, NeighSuppress: true, Learning: false},
			})).To(Succeed())

			newBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br-new", Index: 20}}
			existingVxlan := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:        "vxlan0",
					Index:       10,
					MasterIndex: 5, // Currently attached to different master
					Alias:       managedAliasPrefix + "vxlan:vxlan0",
					Flags:       net.FlagUp,
				},
				FlowBased: true,
				VniFilter: true,
			}

			// applyDeviceConfig: device exists with our alias
			nlMock.On("LinkByName", "vxlan0").Return(existingVxlan, nil)
			// master resolution
			nlMock.On("LinkByName", "br-new").Return(newBridge, nil)
			// Master changed: MasterIndex(5) != newBridge.Index(20)
			nlMock.On("LinkSetMaster", existingVxlan, newBridge).Return(nil)
			// Bridge port settings: kernel defaults differ from desired -> apply
			nlMock.On("LinkGetProtinfo", existingVxlan).Return(netlink.Protinfo{Learning: true}, nil)
			nlMock.On("LinkSetVlanTunnel", existingVxlan, true).Return(nil)
			nlMock.On("LinkSetBrNeighSuppress", existingVxlan, true).Return(nil)
			nlMock.On("LinkSetLearning", existingVxlan, false).Return(nil)
			nlMock.On("LinkSetIsolated", existingVxlan, false).Return(nil)
			// ensureDeviceUp: idempotent, always called
			nlMock.On("LinkSetUp", existingVxlan).Return(nil)

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("detaches from master when no longer desired", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				// No Master — want detached
			})).To(Succeed())

			existingBridge := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:        "br0",
					Index:       10,
					MasterIndex: 5, // Currently attached to a master
					Alias:       managedAliasPrefix + "bridge:br0",
					Flags:       net.FlagUp,
				},
			}

			nlMock.On("LinkByName", "br0").Return(existingBridge, nil)
			nlMock.On("LinkSetNoMaster", existingBridge).Return(nil)
			nlMock.On("LinkSetUp", existingBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			nlMock.AssertCalled(GinkgoT(), "LinkSetNoMaster", existingBridge)
		})

		It("applies LinkModify when mutable attributes differ", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 9000}},
			})).To(Succeed())

			existingBridge := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 10,
					MTU:   1500, // Different from desired 9000
					Alias: managedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}

			nlMock.On("LinkByName", "br0").Return(existingBridge, nil)
			// linkMutableFieldsEqual -> false (MTU differs)
			nlMock.On("LinkModify", mock.AnythingOfType("*netlink.Bridge")).Return(nil)
			nlMock.On("LinkSetUp", existingBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			nlMock.AssertCalled(GinkgoT(), "LinkModify", mock.AnythingOfType("*netlink.Bridge"))
		})

		It("recreates device on critical mismatch (VRF table change)", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 200},
			})).To(Succeed())

			existingVrf := &netlink.Vrf{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "vrf0",
					Index: 10,
					Alias: managedAliasPrefix + "vrf:vrf0",
				},
				Table: 100, // Different -> critical mismatch
			}
			recreatedVrf := &netlink.Vrf{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "vrf0",
					Index: 11,
				},
				Table: 200,
			}

			// applyDeviceConfig: device exists with critical mismatch
			nlMock.On("LinkByName", "vrf0").Return(existingVrf, nil).Once()
			nlMock.On("LinkDelete", existingVrf).Return(nil)
			// createDevice after delete
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vrf")).Return(nil)
			nlMock.On("LinkByName", "vrf0").Return(recreatedVrf, nil)
			nlMock.On("LinkSetAlias", recreatedVrf, managedAliasPrefix+"vrf:vrf0").Return(nil)
			nlMock.On("LinkSetUp", recreatedVrf).Return(nil)

			Expect(controller.reconcileDeviceKey("vrf0")).To(Succeed())
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", existingVrf)
		})

		It("syncs addresses: adds missing and removes extra, preserves link-local", func() {
			desiredAddr := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}
			Expect(controller.EnsureLink(DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{desiredAddr},
			})).To(Succeed())

			existingDummy := &netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "dummy0",
					Index: 5,
					Alias: managedAliasPrefix + "dummy:dummy0",
					Flags: net.FlagUp,
				},
			}
			extraAddr := netlink.Addr{IPNet: mustParseIPNetWithIP("192.168.1.1/24")}
			linkLocalAddr := netlink.Addr{IPNet: mustParseIPNetWithIP("fe80::1/64")}

			nlMock.On("LinkByName", "dummy0").Return(existingDummy, nil)
			nlMock.On("LinkSetUp", existingDummy).Return(nil)
			nlMock.On("AddrList", existingDummy, netlink.FAMILY_ALL).Return(
				[]netlink.Addr{extraAddr, linkLocalAddr}, nil)
			nlMock.On("AddrAdd", existingDummy, mock.MatchedBy(func(addr *netlink.Addr) bool {
				return addr.IPNet.String() == "10.0.0.1/32"
			})).Return(nil)
			nlMock.On("AddrDel", existingDummy, mock.MatchedBy(func(addr *netlink.Addr) bool {
				return addr.IPNet.String() == "192.168.1.1/24"
			})).Return(nil)

			Expect(controller.reconcileDeviceKey("dummy0")).To(Succeed())
			nlMock.AssertNotCalled(GinkgoT(), "AddrDel", existingDummy, mock.MatchedBy(func(addr *netlink.Addr) bool {
				return addr.IPNet.String() == "fe80::1/64"
			}))
		})

		It("skips bridge port settings when they already match", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					FlowBased: true,
					VniFilter: true,
				},
				Master:             "br0",
				BridgePortSettings: &BridgePortSettings{VLANTunnel: true, NeighSuppress: true, Learning: false},
			})).To(Succeed())

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10}}
			existingVxlan := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:        "vxlan0",
					Index:       20,
					MasterIndex: 10, // Already attached to br0
					Alias:       managedAliasPrefix + "vxlan:vxlan0",
					Flags:       net.FlagUp,
				},
				FlowBased: true,
				VniFilter: true,
			}

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(existingVxlan, nil)
			// LinkGetProtinfo returns matching settings -> skip apply
			nlMock.On("LinkGetProtinfo", existingVxlan).Return(netlink.Protinfo{
				VlanTunnel:    true,
				NeighSuppress: true,
				Learning:      false,
			}, nil)
			nlMock.On("LinkSetUp", existingVxlan).Return(nil)

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed())
			nlMock.AssertNotCalled(GinkgoT(), "LinkSetVlanTunnel", mock.Anything, mock.Anything)
			nlMock.AssertNotCalled(GinkgoT(), "LinkSetLearning", mock.Anything, mock.Anything)
			nlMock.AssertNotCalled(GinkgoT(), "LinkSetIsolated", mock.Anything, mock.Anything)
		})
	})

	Describe("Reconciling a deleted device", func() {
		It("deletes owned device from kernel", func() {
			// EnsureLink then DeleteLink: device removed from store
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())
			Expect(controller.DeleteLink("br0")).To(Succeed())

			kernelBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br0",
				Index: 10,
				Alias: managedAliasPrefix + "bridge:br0",
			}}

			nlMock.On("LinkByName", "br0").Return(kernelBridge, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)
			nlMock.On("LinkDelete", kernelBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(controller.store["br0"]).To(BeNil())
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", kernelBridge)
		})

		It("swallows notOwnedError for foreign device and does not delete", func() {
			foreignBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br0",
				Index: 10,
				Alias: "some-other-system:br0",
			}}

			nlMock.On("LinkByName", "br0").Return(foreignBridge, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)

			// Device not in store -> delete path -> notOwnedError -> swallowed
			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			nlMock.AssertNotCalled(GinkgoT(), "LinkDelete", mock.Anything)
		})

		It("succeeds silently when device already gone from kernel", func() {
			linkNotFoundErr := errors.New("link not found")

			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr)
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
		})

		It("returns error when LinkByName fails with non-link-not-found error during delete", func() {
			permErr := errors.New("permission denied")

			nlMock.On("LinkByName", "br0").Return(nil, permErr)
			nlMock.On("IsLinkNotFoundError", permErr).Return(false)

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("permission denied"))
		})

		It("returns error when LinkDelete fails during delete path", func() {
			kernelBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br0",
				Index: 10,
				Alias: managedAliasPrefix + "bridge:br0",
			}}

			nlMock.On("LinkByName", "br0").Return(kernelBridge, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)
			nlMock.On("LinkDelete", kernelBridge).Return(errors.New("device busy"))

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("device busy"))
		})

		It("cleans up bridge self VLANs when deleting VXLAN with tunnel mappings", func() {
			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			kernelVxlan := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{
				Name:        "vxlan0",
				Index:       10,
				MasterIndex: 5,
				Alias:       managedAliasPrefix + "vxlan:vxlan0",
			}}

			nlMock.On("LinkByName", "vxlan0").Return(kernelVxlan, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)
			nlMock.On("LinkByIndex", 5).Return(bridgeLink, nil)
			nlMock.On("BridgeVlanTunnelShowDev", kernelVxlan).Return([]nl.TunnelInfo{
				{Vid: 10, TunId: 100},
				{Vid: 20, TunId: 200},
			}, nil)
			nlMock.On("BridgeVlanDel", bridgeLink, uint16(10), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanDel", bridgeLink, uint16(20), false, false, true, false).Return(nil)
			nlMock.On("LinkDelete", kernelVxlan).Return(nil)

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed())
			nlMock.AssertCalled(GinkgoT(), "BridgeVlanDel", bridgeLink, uint16(10), false, false, true, false)
			nlMock.AssertCalled(GinkgoT(), "BridgeVlanDel", bridgeLink, uint16(20), false, false, true, false)
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", kernelVxlan)
		})

		It("skips bridge self VLAN cleanup for VXLAN without master", func() {
			kernelVxlan := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{
				Name:  "vxlan0",
				Index: 10,
				Alias: managedAliasPrefix + "vxlan:vxlan0",
			}}

			nlMock.On("LinkByName", "vxlan0").Return(kernelVxlan, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)
			nlMock.On("LinkDelete", kernelVxlan).Return(nil)

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed())
			nlMock.AssertNotCalled(GinkgoT(), "BridgeVlanDel", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", kernelVxlan)
		})

		It("retries when bridge self VLAN cleanup fails (does not delete device)", func() {
			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			kernelVxlan := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{
				Name:        "vxlan0",
				Index:       10,
				MasterIndex: 5,
				Alias:       managedAliasPrefix + "vxlan:vxlan0",
			}}

			nlMock.On("LinkByName", "vxlan0").Return(kernelVxlan, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)
			nlMock.On("LinkByIndex", 5).Return(bridgeLink, nil)
			nlMock.On("BridgeVlanTunnelShowDev", kernelVxlan).Return([]nl.TunnelInfo{
				{Vid: 10, TunId: 100},
			}, nil)
			nlMock.On("BridgeVlanDel", bridgeLink, uint16(10), false, false, true, false).Return(errors.New("ENOMEM"))
			nlMock.On("IsEntryNotFoundError", mock.Anything).Return(false)

			err := controller.reconcileDeviceKey("vxlan0")
			Expect(err).To(HaveOccurred())
			nlMock.AssertNotCalled(GinkgoT(), "LinkDelete", mock.Anything)
		})
	})

	Describe("Dependency and ownership scenarios", func() {
		It("transitions to Pending when master doesn't exist", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master: "br0",
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")

			// applyDeviceConfig: device doesn't exist
			nlMock.On("LinkByName", "vxlan0").Return(nil, linkNotFoundErr)
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			// master resolution: not found -> dependencyError
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr)

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed()) // dependencyError -> nil
			Expect(controller.store["vxlan0"].state.getIfindex()).To(BeZero())
		})

		It("transitions to Pending when VLANParent doesn't exist", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:       &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}, VlanId: 100},
				VLANParent: "br0",
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")

			// applyDeviceConfig: device doesn't exist
			nlMock.On("LinkByName", "vlan100").Return(nil, linkNotFoundErr)
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			// VLANParent resolution: not found -> dependencyError
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr)

			Expect(controller.reconcileDeviceKey("vlan100")).To(Succeed())
			Expect(controller.store["vlan100"].state.getIfindex()).To(BeZero())
		})

		DescribeTable("transitions to Blocked when device exists with non-owned alias",
			func(alias string) {
				Expect(controller.EnsureLink(DeviceConfig{
					Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				})).To(Succeed())

				existingBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 10,
					Alias: alias,
				}}

				nlMock.On("LinkByName", "br0").Return(existingBridge, nil)

				Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
				Expect(controller.store["br0"].state.getIfindex()).To(BeZero())
			},
			Entry("foreign alias", "someone-else:br0"),
			Entry("empty alias", ""),
		)

		It("transitions to Failed on transient kernel error", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")

			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(errors.New("ENOSPC"))

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred()) // Transient error returned for workqueue retry
		})

		It("transitions Pending -> Ready when dependency appears", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Master: "vrf0",
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")

			// First reconcile: getLink("br0") not found, master missing -> Pending
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("LinkByName", "vrf0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(controller.store["br0"].state.getIfindex()).To(BeZero())

			// Dependency appears
			vrfLink := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0", Index: 50, Flags: net.FlagUp}}
			createdBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br0",
				Index: 10,
			}}

			nlMock.On("LinkByName", "vrf0").Return(vrfLink, nil)
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil)
			nlMock.On("LinkByName", "br0").Return(createdBridge, nil)
			nlMock.On("LinkSetAlias", createdBridge, managedAliasPrefix+"bridge:br0").Return(nil)
			nlMock.On("LinkSetMaster", createdBridge, vrfLink).Return(nil)
			nlMock.On("LinkSetUp", createdBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
		})

		It("master missing with existing device -> cleans up and transitions to Pending", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				},
				Master: "br0",
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			existingVxlan := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:        "vxlan0",
					Index:       10,
					MasterIndex: 5,
					Alias:       managedAliasPrefix + "vxlan:vxlan0",
					Flags:       net.FlagUp,
				},
			}

			// applyDeviceConfig: device exists
			nlMock.On("LinkByName", "vxlan0").Return(existingVxlan, nil)
			// master resolution: not found -> dependencyError
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr)
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			// defer cleanup: delete existing device
			nlMock.On("LinkDelete", existingVxlan).Return(nil)

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed()) // dependencyError -> nil
		})
	})

	Describe("syncVIDVNIMappings", func() {
		It("adds new mappings and removes stale ones", func() {
			cfg := &DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master: "br0",
				VIDVNIMappings: []VIDVNIMapping{
					{VID: 10, VNI: 100},
					{VID: 20, VNI: 200},
				},
			}

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}

			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{
				{Vid: 30, TunId: 300},
			}, nil)
			nlMock.On("BridgeVlanList").Return(map[int32][]*nl.BridgeVlanInfo{}, nil)
			nlMock.On("BridgeVniList").Return(map[int32][]*nl.BridgeVniInfo{}, nil)

			nlMock.On("BridgeVlanDelTunnelInfo", vxlanLink, uint16(30), uint32(300), false, true).Return(nil)
			nlMock.On("BridgeVniDel", vxlanLink, uint32(300)).Return(nil)
			nlMock.On("BridgeVlanDel", vxlanLink, uint16(30), false, false, false, true).Return(nil)
			nlMock.On("BridgeVlanDel", bridgeLink, uint16(30), false, false, true, false).Return(nil)

			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(10), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(100)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(10), uint32(100), false, true).Return(nil)

			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(20), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(20), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(200)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(20), uint32(200), false, true).Return(nil)

			Expect(syncVIDVNIMappings(vxlanLink, bridgeLink, cfg.VIDVNIMappings)).To(Succeed())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("skips when VIDVNIMappings is nil", func() {
			cfg := &DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master: "br0",
			}
			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}
			Expect(syncVIDVNIMappings(vxlanLink, bridgeLink, cfg.VIDVNIMappings)).To(Succeed())
		})

		It("skips fully present mappings — no adds when all four components exist", func() {
			cfg := &DeviceConfig{
				Link:           &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:         "br0",
				VIDVNIMappings: []VIDVNIMapping{{VID: 10, VNI: 100}},
			}

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}

			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{
				{Vid: 10, TunId: 100},
			}, nil)
			nlMock.On("BridgeVlanList").Return(map[int32][]*nl.BridgeVlanInfo{
				5:  {{Vid: 10}},
				10: {{Vid: 10}},
			}, nil)
			nlMock.On("BridgeVniList").Return(map[int32][]*nl.BridgeVniInfo{
				10: {{Vni: 100}},
			}, nil)

			Expect(syncVIDVNIMappings(vxlanLink, bridgeLink, cfg.VIDVNIMappings)).To(Succeed())

			nlMock.AssertNotCalled(GinkgoT(), "BridgeVlanAdd", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
			nlMock.AssertNotCalled(GinkgoT(), "BridgeVniAdd", mock.Anything, mock.Anything)
			nlMock.AssertNotCalled(GinkgoT(), "BridgeVlanAddTunnelInfo", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})

		DescribeTable("self-heals when any single component is externally removed",
			func(vlanList map[int32][]*nl.BridgeVlanInfo, vniList map[int32][]*nl.BridgeVniInfo, tunnelInfo []nl.TunnelInfo) {
				cfg := &DeviceConfig{
					Link:           &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
					Master:         "br0",
					VIDVNIMappings: []VIDVNIMapping{{VID: 10, VNI: 100}},
				}

				bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
				vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}
				eexistErr := errors.New("file exists")

				nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return(tunnelInfo, nil)
				nlMock.On("BridgeVlanList").Return(vlanList, nil)
				nlMock.On("BridgeVniList").Return(vniList, nil)

				nlMock.On("BridgeVlanAdd", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(eexistErr).Maybe()
				nlMock.On("IsAlreadyExistsError", eexistErr).Return(true).Maybe()
				nlMock.On("BridgeVniAdd", mock.Anything, mock.Anything).Return(eexistErr).Maybe()
				nlMock.On("BridgeVlanAddTunnelInfo", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(eexistErr).Maybe()

				nlMock.On("BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false).Return(nil).Maybe()
				nlMock.On("BridgeVlanAdd", vxlanLink, uint16(10), false, false, false, true).Return(nil).Maybe()
				nlMock.On("BridgeVniAdd", vxlanLink, uint32(100)).Return(nil).Maybe()
				nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(10), uint32(100), false, true).Return(nil).Maybe()

				Expect(syncVIDVNIMappings(vxlanLink, bridgeLink, cfg.VIDVNIMappings)).To(Succeed())

				nlMock.AssertCalled(GinkgoT(), "BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false)
				nlMock.AssertCalled(GinkgoT(), "BridgeVlanAdd", vxlanLink, uint16(10), false, false, false, true)
				nlMock.AssertCalled(GinkgoT(), "BridgeVniAdd", vxlanLink, uint32(100))
				nlMock.AssertCalled(GinkgoT(), "BridgeVlanAddTunnelInfo", vxlanLink, uint16(10), uint32(100), false, true)
			},
			Entry("bridge self VLAN missing",
				map[int32][]*nl.BridgeVlanInfo{
					10: {{Vid: 10}},
				},
				map[int32][]*nl.BridgeVniInfo{
					10: {{Vni: 100}},
				},
				[]nl.TunnelInfo{{Vid: 10, TunId: 100}},
			),
			Entry("VXLAN VID membership missing",
				map[int32][]*nl.BridgeVlanInfo{
					5: {{Vid: 10}},
				},
				map[int32][]*nl.BridgeVniInfo{
					10: {{Vni: 100}},
				},
				[]nl.TunnelInfo{{Vid: 10, TunId: 100}},
			),
			Entry("VNI filter entry missing",
				map[int32][]*nl.BridgeVlanInfo{
					5:  {{Vid: 10}},
					10: {{Vid: 10}},
				},
				map[int32][]*nl.BridgeVniInfo{},
				[]nl.TunnelInfo{{Vid: 10, TunId: 100}},
			),
			Entry("tunnel info missing",
				map[int32][]*nl.BridgeVlanInfo{
					5:  {{Vid: 10}},
					10: {{Vid: 10}},
				},
				map[int32][]*nl.BridgeVniInfo{
					10: {{Vni: 100}},
				},
				[]nl.TunnelInfo{},
			),
		)
	})

	Describe("Atomic ifindex/masterIfindex tracking", func() {
		It("addr events are dropped before reconciliation and match after", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:      &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Addresses: []netlink.Addr{},
			})).To(Succeed())

			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler

			// Unwanted address added → divergent event that should trigger reconciliation
			// once ifindex is set.
			divergentEvent := netlink.AddrUpdate{
				LinkIndex:   10,
				LinkAddress: *mustParseIPNet("10.0.0.1/32"),
				NewAddr:     true,
			}

			// Before reconciliation: ifindex is 0 (new pointer), addr event are dropped
			// no big deal since after the ifindex is set a subsequent address sync is performed during
			// the reconciliation process.
			controller.handleAddrUpdate(divergentEvent)
			Expect(fakeReconciler.Reconciles).To(BeEmpty(),
				"addr event must be dropped before reconciliation sets ifindex")

			existingBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br0",
				Index: 10,
				Alias: managedAliasPrefix + "bridge:br0",
				Flags: net.FlagUp,
			}}

			nlMock.On("LinkByName", "br0").Return(existingBridge, nil)
			nlMock.On("AddrList", existingBridge, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
			nlMock.On("LinkSetUp", existingBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())

			// After reconciliation: ifindex is set, divergent addr event must now trigger reconciliation.
			controller.handleAddrUpdate(divergentEvent)
			Expect(fakeReconciler.Reconciles).To(ContainElement("Reconcile:device/br0"),
				"addr event must match after reconciliation sets ifindex")
		})

		It("link events match after update path sets masterIfindex when master already correct", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Master: "vrf0",
			})).To(Succeed())

			vrfLink := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0", Index: 20}}
			existingBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:        "br0",
				Index:       10,
				MasterIndex: 20,
				Alias:       managedAliasPrefix + "bridge:br0",
				Flags:       net.FlagUp,
			}}

			nlMock.On("LinkByName", "vrf0").Return(vrfLink, nil)
			nlMock.On("LinkByName", "br0").Return(existingBridge, nil)
			nlMock.On("LinkSetUp", existingBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())

			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler

			// Simulate an external MTU drift — linkStateEquals should return true
			// (no drift) because master, flags, and link attrs all match.
			// Without the masterIfindex fix, masterIfindex would be 0 and
			// linkStateEquals would always return false, causing spurious reconciles.
			controller.handleLinkUpdate(netlink.LinkUpdate{
				Header: unix.NlMsghdr{Type: unix.RTM_NEWLINK},
				Link:   existingBridge,
			})
			Expect(fakeReconciler.Reconciles).To(BeEmpty(),
				"link event matching desired state must NOT trigger reconciliation — "+
					"without the masterIfindex fix this would loop infinitely")
		})
	})

	Describe("Pointer-swap staleness isolation", func() {
		It("isolates in-flight reconciler updates when EnsureLink swaps the pointer", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Master: "vrf-old",
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			vrfOld := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf-old", Index: 10}}
			createdBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br0",
				Index: 5,
				Alias: managedAliasPrefix + "bridge:br0",
			}}

			nlMock.On("LinkByName", "vrf-old").Return(vrfOld, nil)
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			nlMock.On("IsLinkNotFoundError", mock.MatchedBy(func(e error) bool { return e != linkNotFoundErr })).Return(false)

			// During LinkAdd, simulate concurrent EnsureLink replacing the pointer
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil).Once().Run(func(_ mock.Arguments) {
				controller.mu.Lock()
				controller.store["br0"] = &managedDevice{
					cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{
						Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
						Master: "vrf-new",
					}},
				}
				controller.mu.Unlock()
			})
			nlMock.On("LinkByName", "br0").Return(createdBridge, nil)
			nlMock.On("LinkSetAlias", createdBridge, managedAliasPrefix+"bridge:br0").Return(nil)
			nlMock.On("LinkSetMaster", mock.Anything, mock.Anything).Return(nil)
			nlMock.On("LinkSetUp", mock.Anything).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())

			// The reconciler updated ifindex on the OLD pointer (detached).
			// The NEW pointer in the store should have ifindex=0 (untouched).
			Expect(controller.store["br0"].state.getIfindex()).To(Equal(0),
				"new pointer should not be affected by in-flight reconciler")
		})
	})

	Describe("Orphan cleanup", func() {
		It("deletes our-aliased devices not in store, preserves foreign and desired", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br-desired"}},
			})).To(Succeed())

			desiredBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br-desired",
				Index: 1,
				Alias: managedAliasPrefix + "bridge:br-desired",
			}}
			orphanBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br-orphan",
				Index: 2,
				Alias: managedAliasPrefix + "bridge:br-orphan",
			}}
			foreignBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br-foreign",
				Index: 3,
				Alias: "someone-else:br-foreign",
			}}

			nlMock.On("LinkList").Return([]netlink.Link{desiredBridge, orphanBridge, foreignBridge}, nil)
			nlMock.On("LinkDelete", orphanBridge).Return(nil)

			Expect(controller.cleanupOrphanedDevices()).To(Succeed())
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", orphanBridge)
			nlMock.AssertNotCalled(GinkgoT(), "LinkDelete", desiredBridge)
			nlMock.AssertNotCalled(GinkgoT(), "LinkDelete", foreignBridge)
		})

		It("returns error when LinkList fails", func() {
			nlMock.On("LinkList").Return(nil, errors.New("netlink error"))

			err := controller.cleanupOrphanedDevices()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to list links"))
		})

		It("continues best-effort cleanup when individual LinkDelete fails", func() {
			orphan1 := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name: "br-orphan1", Index: 2, Alias: managedAliasPrefix + "bridge:br-orphan1",
			}}
			orphan2 := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name: "br-orphan2", Index: 3, Alias: managedAliasPrefix + "bridge:br-orphan2",
			}}

			nlMock.On("LinkList").Return([]netlink.Link{orphan1, orphan2}, nil)
			nlMock.On("LinkDelete", orphan1).Return(errors.New("device busy"))
			nlMock.On("LinkDelete", orphan2).Return(nil)

			Expect(controller.cleanupOrphanedDevices()).To(Succeed())
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", orphan1)
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", orphan2)
		})

		It("cleans up bridge self VLANs when deleting orphaned VXLAN device", func() {
			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			orphanVxlan := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{
				Name:        "vxlan-orphan",
				Index:       10,
				MasterIndex: 5,
				Alias:       managedAliasPrefix + "vxlan:vxlan-orphan",
			}}

			nlMock.On("LinkList").Return([]netlink.Link{orphanVxlan}, nil)
			nlMock.On("LinkByIndex", 5).Return(bridgeLink, nil)
			nlMock.On("BridgeVlanTunnelShowDev", orphanVxlan).Return([]nl.TunnelInfo{
				{Vid: 10, TunId: 100},
			}, nil)
			nlMock.On("BridgeVlanDel", bridgeLink, uint16(10), false, false, true, false).Return(nil)
			nlMock.On("LinkDelete", orphanVxlan).Return(nil)

			Expect(controller.cleanupOrphanedDevices()).To(Succeed())
			nlMock.AssertCalled(GinkgoT(), "BridgeVlanDel", bridgeLink, uint16(10), false, false, true, false)
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", orphanVxlan)
		})
	})

	Describe("Error handling during device creation", func() {
		It("transitions to Failed when LinkSetUp fails after create", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			createdBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10}}

			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil)
			nlMock.On("LinkByName", "br0").Return(createdBridge, nil)
			nlMock.On("LinkSetAlias", createdBridge, managedAliasPrefix+"bridge:br0").Return(nil)
			nlMock.On("LinkSetUp", createdBridge).Return(errors.New("ENOMEM"))
			// defer cleanup: delete device after error
			nlMock.On("LinkDelete", createdBridge).Return(nil)

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred())
		})

		DescribeTable("transitions to Failed when a bridge port setting fails during creation",
			func(setupMocks func(nlMock *mocks.NetLinkOps, bridgeLink *netlink.Bridge, createdVxlan *netlink.Vxlan)) {
				Expect(controller.EnsureLink(DeviceConfig{
					Link:               &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
					Master:             "br0",
					BridgePortSettings: &BridgePortSettings{VLANTunnel: true, NeighSuppress: true, Learning: false},
				})).To(Succeed())

				linkNotFoundErr := errors.New("link not found")
				bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10}}
				createdVxlan := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 20}}

				nlMock.On("LinkByName", "vxlan0").Return(nil, linkNotFoundErr).Once()
				nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
				nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
				nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vxlan")).Return(nil)
				nlMock.On("LinkByName", "vxlan0").Return(createdVxlan, nil)
				nlMock.On("LinkSetAlias", createdVxlan, managedAliasPrefix+"vxlan:vxlan0").Return(nil)
				nlMock.On("LinkSetMaster", createdVxlan, bridgeLink).Return(nil)
				// defer cleanup: removeBridgeSelfVLANs (no mappings yet) + delete device
				nlMock.On("BridgeVlanTunnelShowDev", createdVxlan).Return([]nl.TunnelInfo{}, nil)
				nlMock.On("LinkDelete", createdVxlan).Return(nil)
				setupMocks(nlMock, bridgeLink, createdVxlan)

				err := controller.reconcileDeviceKey("vxlan0")
				Expect(err).To(HaveOccurred())
			},
			Entry("LinkSetVlanTunnel fails", func(nlMock *mocks.NetLinkOps, _ *netlink.Bridge, vxlan *netlink.Vxlan) {
				nlMock.On("LinkSetVlanTunnel", vxlan, true).Return(errors.New("not supported"))
			}),
			Entry("LinkSetBrNeighSuppress fails", func(nlMock *mocks.NetLinkOps, _ *netlink.Bridge, vxlan *netlink.Vxlan) {
				nlMock.On("LinkSetVlanTunnel", vxlan, true).Return(nil)
				nlMock.On("LinkSetBrNeighSuppress", vxlan, true).Return(errors.New("not supported"))
			}),
			Entry("LinkSetLearning fails", func(nlMock *mocks.NetLinkOps, _ *netlink.Bridge, vxlan *netlink.Vxlan) {
				nlMock.On("LinkSetVlanTunnel", vxlan, true).Return(nil)
				nlMock.On("LinkSetBrNeighSuppress", vxlan, true).Return(nil)
				nlMock.On("LinkSetLearning", vxlan, false).Return(errors.New("not supported"))
			}),
		)

		It("cleans up bridge self VLANs in defer when ensureDeviceUp fails after VID/VNI sync", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:           &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:         "br0",
				VIDVNIMappings: []VIDVNIMapping{{VID: 10, VNI: 100}},
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			createdVxlan := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}

			// applyDeviceConfig: device doesn't exist
			nlMock.On("LinkByName", "vxlan0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			// createLink
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vxlan")).Return(nil)
			nlMock.On("LinkByName", "vxlan0").Return(createdVxlan, nil)
			nlMock.On("LinkSetAlias", createdVxlan, managedAliasPrefix+"vxlan:vxlan0").Return(nil)
			// ensureMaster
			nlMock.On("LinkSetMaster", createdVxlan, bridgeLink).Return(nil)
			// syncVIDVNIMappings: first call returns empty (no existing mappings)
			nlMock.On("BridgeVlanTunnelShowDev", createdVxlan).Return([]nl.TunnelInfo{}, nil).Once()
			nlMock.On("BridgeVlanList").Return(map[int32][]*nl.BridgeVlanInfo{}, nil)
			nlMock.On("BridgeVniList").Return(map[int32][]*nl.BridgeVniInfo{}, nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", createdVxlan, uint16(10), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", createdVxlan, uint32(100)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", createdVxlan, uint16(10), uint32(100), false, true).Return(nil)
			// ensureDeviceUp: FAILS → triggers defer cleanup
			nlMock.On("LinkSetUp", createdVxlan).Return(errors.New("ENOMEM"))
			// defer: removeBridgeSelfVLANs queries tunnel info (second call returns added mapping)
			nlMock.On("BridgeVlanTunnelShowDev", createdVxlan).Return([]nl.TunnelInfo{{Vid: 10, TunId: 100}}, nil)
			nlMock.On("BridgeVlanDel", bridgeLink, uint16(10), false, false, true, false).Return(nil)
			// defer: LinkDelete
			nlMock.On("LinkDelete", createdVxlan).Return(nil)

			err := controller.reconcileDeviceKey("vxlan0")
			Expect(err).To(HaveOccurred())
			nlMock.AssertCalled(GinkgoT(), "BridgeVlanDel", bridgeLink, uint16(10), false, false, true, false)
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", createdVxlan)
		})
	})

	Describe("Error handling during device update", func() {
		It("transitions to Failed when LinkModify fails", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 9000}},
			})).To(Succeed())

			existingBridge := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 10,
					MTU:   1500,
					Alias: managedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}

			nlMock.On("LinkByName", "br0").Return(existingBridge, nil)
			nlMock.On("LinkModify", mock.AnythingOfType("*netlink.Bridge")).Return(errors.New("permission denied"))
			// defer cleanup: delete device after error
			nlMock.On("LinkDelete", existingBridge).Return(nil)

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred())
		})

		It("fails when LinkGetProtinfo returns error", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					FlowBased: true,
					VniFilter: true,
				},
				Master:             "br0",
				BridgePortSettings: &BridgePortSettings{VLANTunnel: true, NeighSuppress: true, Learning: false},
			})).To(Succeed())

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10}}
			existingVxlan := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:        "vxlan0",
					Index:       20,
					MasterIndex: 10,
					Alias:       managedAliasPrefix + "vxlan:vxlan0",
					Flags:       net.FlagUp,
				},
				FlowBased: true,
				VniFilter: true,
			}

			nlMock.On("LinkByName", "vxlan0").Return(existingVxlan, nil)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkGetProtinfo", existingVxlan).Return(netlink.Protinfo{}, errors.New("no bridge port info"))
			// defer cleanup: removeBridgeSelfVLANs + delete device
			nlMock.On("BridgeVlanTunnelShowDev", existingVxlan).Return([]nl.TunnelInfo{}, nil)
			nlMock.On("LinkDelete", existingVxlan).Return(nil)

			err := controller.reconcileDeviceKey("vxlan0")
			Expect(err).To(HaveOccurred())
			nlMock.AssertNotCalled(GinkgoT(), "LinkSetVlanTunnel", mock.Anything, mock.Anything)
		})
	})

	Describe("VLAN validation in EnsureLink", func() {
		It("fails when VLANParent is set on a Bridge (type mismatch)", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link:       &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				VLANParent: "eth0",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("VLANParent set but Link is"))
		})

		It("fails when VLANParent is set on a Vxlan (type mismatch)", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link:       &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				VLANParent: "eth0",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("VLANParent set but Link is"))
		})

		It("fails when VLAN has no VLANParent", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vlan100"},
					VlanId:    100,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("VLAN requires VLANParent"))
		})
	})

	Describe("Master lookup error handling", func() {
		It("transitions to Failed on non-link-not-found error from master lookup", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Master: "vrf0",
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			permErr := errors.New("permission denied")

			// getLink: device doesn't exist
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr)
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			// master lookup: non-retriable error
			nlMock.On("LinkByName", "vrf0").Return(nil, permErr)
			nlMock.On("IsLinkNotFoundError", permErr).Return(false)

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("syncAddresses error handling through reconciler", func() {
		It("tolerates EEXIST when adding addresses (still Ready)", func() {
			desiredAddr := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}
			Expect(controller.EnsureLink(DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{desiredAddr},
			})).To(Succeed())

			existingDummy := &netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "dummy0",
					Index: 5,
					Alias: managedAliasPrefix + "dummy:dummy0",
					Flags: net.FlagUp,
				},
			}
			eexistErr := errors.New("file exists")

			nlMock.On("LinkByName", "dummy0").Return(existingDummy, nil)
			nlMock.On("LinkSetUp", existingDummy).Return(nil)
			nlMock.On("AddrList", existingDummy, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", existingDummy, mock.Anything).Return(eexistErr)
			nlMock.On("IsAlreadyExistsError", eexistErr).Return(true)

			Expect(controller.reconcileDeviceKey("dummy0")).To(Succeed())
		})

		It("tolerates ENOENT when removing addresses (still Ready)", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{},
			})).To(Succeed())

			existingDummy := &netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "dummy0",
					Index: 5,
					Alias: managedAliasPrefix + "dummy:dummy0",
					Flags: net.FlagUp,
				},
			}
			extraAddr := netlink.Addr{IPNet: mustParseIPNetWithIP("192.168.1.1/24")}
			enoentErr := errors.New("no such address")

			nlMock.On("LinkByName", "dummy0").Return(existingDummy, nil)
			nlMock.On("LinkSetUp", existingDummy).Return(nil)
			nlMock.On("AddrList", existingDummy, netlink.FAMILY_ALL).Return([]netlink.Addr{extraAddr}, nil)
			nlMock.On("AddrDel", existingDummy, mock.Anything).Return(enoentErr)
			nlMock.On("IsEntryNotFoundError", enoentErr).Return(true)

			Expect(controller.reconcileDeviceKey("dummy0")).To(Succeed())
		})

		It("transitions to Failed on non-retriable address add failure", func() {
			desiredAddr := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}
			Expect(controller.EnsureLink(DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{desiredAddr},
			})).To(Succeed())

			existingDummy := &netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "dummy0",
					Index: 5,
					Alias: managedAliasPrefix + "dummy:dummy0",
					Flags: net.FlagUp,
				},
			}
			permErr := errors.New("permission denied")

			nlMock.On("LinkByName", "dummy0").Return(existingDummy, nil)
			nlMock.On("AddrList", existingDummy, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", existingDummy, mock.Anything).Return(permErr)
			nlMock.On("IsAlreadyExistsError", permErr).Return(false)
			// defer cleanup: delete device after error
			nlMock.On("LinkDelete", existingDummy).Return(nil)

			err := controller.reconcileDeviceKey("dummy0")
			Expect(err).To(HaveOccurred())
		})

		It("transitions to Failed on non-retriable address delete failure", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{},
			})).To(Succeed())

			existingDummy := &netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "dummy0",
					Index: 5,
					Alias: managedAliasPrefix + "dummy:dummy0",
					Flags: net.FlagUp,
				},
			}
			extraAddr := netlink.Addr{IPNet: mustParseIPNetWithIP("192.168.1.1/24")}
			permErr := errors.New("permission denied")

			nlMock.On("LinkByName", "dummy0").Return(existingDummy, nil)
			nlMock.On("AddrList", existingDummy, netlink.FAMILY_ALL).Return([]netlink.Addr{extraAddr}, nil)
			nlMock.On("AddrDel", existingDummy, mock.Anything).Return(permErr)
			nlMock.On("IsEntryNotFoundError", permErr).Return(false)
			// defer cleanup: delete device after error
			nlMock.On("LinkDelete", existingDummy).Return(nil)

			err := controller.reconcileDeviceKey("dummy0")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Mapping error handling through syncVIDVNIMappings", func() {
		It("returns error when addVIDVNIMapping bridge self VLAN fails", func() {
			cfg := &DeviceConfig{
				Link:           &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:         "br0",
				VIDVNIMappings: []VIDVNIMapping{{VID: 10, VNI: 100}},
			}

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}

			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{}, nil)
			nlMock.On("BridgeVlanList").Return(map[int32][]*nl.BridgeVlanInfo{}, nil)
			nlMock.On("BridgeVniList").Return(map[int32][]*nl.BridgeVniInfo{}, nil)
			// Steps 1-3 succeed (VXLAN port VID, VNI filter, tunnel info)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(10), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(100)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(10), uint32(100), false, true).Return(nil)
			// Step 4: bridge self VLAN fails
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false).Return(errors.New("ENOMEM"))
			nlMock.On("IsAlreadyExistsError", mock.Anything).Return(false)

			err := syncVIDVNIMappings(vxlanLink, bridgeLink, cfg.VIDVNIMappings)
			Expect(err).To(HaveOccurred())
		})

		It("succeeds when removeVIDVNIMapping entries already gone (idempotent)", func() {
			cfg := &DeviceConfig{
				Link:           &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:         "br0",
				VIDVNIMappings: []VIDVNIMapping{},
			}

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}
			entryNotFoundErr := errors.New("entry not found")

			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{
				{Vid: 30, TunId: 300},
			}, nil)
			nlMock.On("BridgeVlanList").Return(map[int32][]*nl.BridgeVlanInfo{}, nil)
			nlMock.On("BridgeVniList").Return(map[int32][]*nl.BridgeVniInfo{}, nil)
			nlMock.On("BridgeVlanDelTunnelInfo", vxlanLink, uint16(30), uint32(300), false, true).Return(entryNotFoundErr)
			nlMock.On("IsEntryNotFoundError", entryNotFoundErr).Return(true)
			nlMock.On("BridgeVniDel", vxlanLink, uint32(300)).Return(entryNotFoundErr)
			nlMock.On("BridgeVlanDel", vxlanLink, uint16(30), false, false, false, true).Return(entryNotFoundErr)
			nlMock.On("BridgeVlanDel", bridgeLink, uint16(30), false, false, true, false).Return(entryNotFoundErr)

			Expect(syncVIDVNIMappings(vxlanLink, bridgeLink, cfg.VIDVNIMappings)).To(Succeed())
		})

		It("returns error when removeVIDVNIMapping has non-entry-not-found error", func() {
			cfg := &DeviceConfig{
				Link:           &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:         "br0",
				VIDVNIMappings: []VIDVNIMapping{},
			}

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}
			realErr := errors.New("device busy")

			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{
				{Vid: 30, TunId: 300},
			}, nil)
			nlMock.On("BridgeVlanList").Return(map[int32][]*nl.BridgeVlanInfo{}, nil)
			nlMock.On("BridgeVniList").Return(map[int32][]*nl.BridgeVniInfo{}, nil)
			nlMock.On("BridgeVlanDelTunnelInfo", vxlanLink, uint16(30), uint32(300), false, true).Return(realErr)
			nlMock.On("IsEntryNotFoundError", realErr).Return(false)
			nlMock.On("BridgeVniDel", vxlanLink, uint32(300)).Return(nil)
			nlMock.On("BridgeVlanDel", vxlanLink, uint16(30), false, false, false, true).Return(nil)
			nlMock.On("BridgeVlanDel", bridgeLink, uint16(30), false, false, true, false).Return(nil)

			err := syncVIDVNIMappings(vxlanLink, bridgeLink, cfg.VIDVNIMappings)
			Expect(err).To(HaveOccurred())
		})

		It("collects multiple errors from removeVIDVNIMapping (best-effort cleanup)", func() {
			cfg := &DeviceConfig{
				Link:           &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:         "br0",
				VIDVNIMappings: []VIDVNIMapping{},
			}

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}
			err1 := errors.New("error1")
			err2 := errors.New("error2")
			entryNotFoundErr := errors.New("entry not found")

			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{
				{Vid: 30, TunId: 300},
			}, nil)
			nlMock.On("BridgeVlanList").Return(map[int32][]*nl.BridgeVlanInfo{}, nil)
			nlMock.On("BridgeVniList").Return(map[int32][]*nl.BridgeVniInfo{}, nil)
			nlMock.On("BridgeVlanDelTunnelInfo", vxlanLink, uint16(30), uint32(300), false, true).Return(err1)
			nlMock.On("IsEntryNotFoundError", err1).Return(false)
			nlMock.On("BridgeVniDel", vxlanLink, uint32(300)).Return(entryNotFoundErr)
			nlMock.On("IsEntryNotFoundError", entryNotFoundErr).Return(true)
			nlMock.On("BridgeVlanDel", vxlanLink, uint16(30), false, false, false, true).Return(err2)
			nlMock.On("IsEntryNotFoundError", err2).Return(false)
			nlMock.On("BridgeVlanDel", bridgeLink, uint16(30), false, false, true, false).Return(nil)

			err := syncVIDVNIMappings(vxlanLink, bridgeLink, cfg.VIDVNIMappings)
			Expect(err).To(HaveOccurred())
		})

		It("returns aggregate error on partial mapping failure", func() {
			cfg := &DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master: "br0",
				VIDVNIMappings: []VIDVNIMapping{
					{VID: 10, VNI: 100},
					{VID: 20, VNI: 200},
				},
			}

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}

			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{}, nil)
			nlMock.On("BridgeVlanList").Return(map[int32][]*nl.BridgeVlanInfo{}, nil)
			nlMock.On("BridgeVniList").Return(map[int32][]*nl.BridgeVniInfo{}, nil)

			// VID 10: succeeds (new order: VXLAN VID, VNI, tunnel info, bridge self)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(10), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(100)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(10), uint32(100), false, true).Return(nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false).Return(nil)

			// VID 20: VXLAN-side steps succeed, bridge self VLAN fails
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(20), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(200)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(20), uint32(200), false, true).Return(nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(20), false, false, true, false).Return(errors.New("ENOMEM"))
			nlMock.On("IsAlreadyExistsError", mock.Anything).Return(false)

			err := syncVIDVNIMappings(vxlanLink, bridgeLink, cfg.VIDVNIMappings)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to apply"))
		})
	})

	Describe("VIDVNIMappings validation in EnsureLink", func() {
		It("rejects duplicate VIDs", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master: "br0",
				VIDVNIMappings: []VIDVNIMapping{
					{VID: 10, VNI: 100},
					{VID: 10, VNI: 200},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("duplicate VID"))
		})

		It("rejects duplicate VNIs", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master: "br0",
				VIDVNIMappings: []VIDVNIMapping{
					{VID: 10, VNI: 100},
					{VID: 20, VNI: 100},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("duplicate VNI"))
		})

		It("rejects VIDVNIMappings on non-VXLAN devices", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link:           &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				VIDVNIMappings: []VIDVNIMapping{{VID: 10, VNI: 100}},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("only valid for VXLAN"))
		})

		It("rejects VIDVNIMappings without a bridge master", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link:           &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				VIDVNIMappings: []VIDVNIMapping{{VID: 10, VNI: 100}},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("requires a bridge master"))
		})

		DescribeTable("link-local address validation",
			func(addrs []string, expectErr bool) {
				nlAddrs := make([]netlink.Addr, len(addrs))
				for i, a := range addrs {
					nlAddrs[i] = netlink.Addr{IPNet: mustParseIPNetWithIP(a)}
				}
				err := controller.EnsureLink(DeviceConfig{
					Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}},
					Addresses: nlAddrs,
				})
				if expectErr {
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("link-local address"))
					Expect(controller.store["dev0"]).To(BeNil())
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
			},
			Entry("rejects IPv6 link-local", []string{"fe80::1/64"}, true),
			Entry("rejects IPv4 link-local", []string{"169.254.1.1/16"}, true),
			Entry("accepts non-link-local addresses", []string{"10.0.0.1/24", "fd00::1/64"}, false),
		)
	})

	Describe("Loop prevention and idempotency", func() {
		It("causes no unnecessary operations when device is stable", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			stableBridge := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 10,
					Alias: managedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}
			nlMock.On("LinkByName", "br0").Return(stableBridge, nil)
			nlMock.On("LinkSetUp", stableBridge).Return(nil)

			for range 10 {
				Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			}

			nlMock.AssertNotCalled(GinkgoT(), "LinkModify", mock.Anything)
			nlMock.AssertNotCalled(GinkgoT(), "LinkAdd", mock.Anything)
		})

		It("applies bridge port settings exactly once per reconcile when they differ", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					FlowBased: true, VniFilter: true,
				},
				Master:             "br0",
				BridgePortSettings: &BridgePortSettings{VLANTunnel: true, NeighSuppress: true, Learning: false},
			})).To(Succeed())

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10}}
			existingVxlan := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{
					Name: "vxlan0", Index: 20, MasterIndex: 10,
					Alias: managedAliasPrefix + "vxlan:vxlan0", Flags: net.FlagUp,
				},
				FlowBased: true, VniFilter: true,
			}

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(existingVxlan, nil)
			// Persistent drift: protinfo always reports wrong values
			nlMock.On("LinkGetProtinfo", existingVxlan).Return(netlink.Protinfo{
				VlanTunnel: false, NeighSuppress: false, Learning: true,
			}, nil)
			nlMock.On("LinkSetVlanTunnel", existingVxlan, true).Return(nil)
			nlMock.On("LinkSetBrNeighSuppress", existingVxlan, true).Return(nil)
			nlMock.On("LinkSetLearning", existingVxlan, false).Return(nil)
			nlMock.On("LinkSetIsolated", existingVxlan, false).Return(nil)
			nlMock.On("LinkSetUp", existingVxlan).Return(nil)

			for range 100 {
				Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed())
			}

			// Count calls: exactly 100 per setting (one per reconcile, not exponential)
			vlanTunnelCalls := 0
			learningCalls := 0
			for _, call := range nlMock.Calls {
				switch call.Method {
				case "LinkSetVlanTunnel":
					vlanTunnelCalls++
				case "LinkSetLearning":
					learningCalls++
				}
			}
			Expect(vlanTunnelCalls).To(Equal(100))
			Expect(learningCalls).To(Equal(100))
		})

		It("interleaved sync and event handling does not cause duplicate operations", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			bridgeLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 10,
					Alias: managedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkList").Return([]netlink.Link{bridgeLink}, nil)
			nlMock.On("LinkGetProtinfo", bridgeLink).Return(netlink.Protinfo{}, nil)

			for range 20 {
				controller.handleLinkUpdate(netlink.LinkUpdate{
					Header: unix.NlMsghdr{Type: unix.RTM_NEWLINK},
					Link:   bridgeLink,
				})
				Expect(controller.reconcileVxlanSyncKey()).To(Succeed())
			}

			nlMock.AssertNotCalled(GinkgoT(), "LinkModify", mock.Anything)
			nlMock.AssertNotCalled(GinkgoT(), "LinkAdd", mock.Anything)
		})
	})

	Describe("EnsureLink", func() {
		It("stores device config in store", func() {
			cfg := DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0"},
				},
			}

			Expect(controller.EnsureLink(cfg)).To(Succeed())
			Expect(controller.store["br0"]).ToNot(BeNil())
		})

		It("returns error for config without name", func() {
			cfg := DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{},
				},
			}

			err := controller.EnsureLink(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("name is empty"))
		})

		It("returns error for unsupported link type", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link: &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported link type"))
		})

		It("tracks config updates for reconciliation", func() {
			// Tests that EnsureLink updates the desired state when config changes.
			// The reconciler uses this stored config to apply updates.
			cfg1 := DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0"},
				},
				Master: "vrf0",
			}

			Expect(controller.EnsureLink(cfg1)).To(Succeed())
			Expect(controller.store["br0"].cfg.Master).To(Equal("vrf0"))

			// Update config
			cfg2 := DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0"},
				},
				Master: "vrf1",
			}

			Expect(controller.EnsureLink(cfg2)).To(Succeed())

			Expect(controller.store["br0"].cfg.Master).To(Equal("vrf1"))
		})

		It("returns that non-existent device is not in store", func() {
			Expect(controller.store).ToNot(HaveKey("nonexistent"))
		})
	})

	Describe("DeleteLink", func() {
		It("succeeds for non-existent device", func() {
			Expect(controller.DeleteLink("nonexistent")).To(Succeed())
		})

		It("removes device from store", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())
			Expect(controller.store["br0"]).ToNot(BeNil())

			Expect(controller.DeleteLink("br0")).To(Succeed())
			Expect(controller.store["br0"]).To(BeNil())
		})

	})

	Describe("DeviceConfig.equal", func() {
		It("returns true for equal configs with all fields populated", func() {
			cfg1 := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   0,
					FlowBased: true,
					VniFilter: true,
					SrcAddr:   net.ParseIP("10.0.0.1"),
					Port:      4789,
				},
				Master:             "br0",
				BridgePortSettings: &BridgePortSettings{VLANTunnel: true, NeighSuppress: true, Learning: false},
				Addresses:          []netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   0,
					FlowBased: true,
					VniFilter: true,
					SrcAddr:   net.ParseIP("10.0.0.1"),
					Port:      4789,
				},
				Master:             "br0",
				BridgePortSettings: &BridgePortSettings{VLANTunnel: true, NeighSuppress: true, Learning: false},
				Addresses:          []netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			}

			m1 := &managedDeviceConfig{DeviceConfig: *cfg1}
			m2 := &managedDeviceConfig{DeviceConfig: *cfg2}
			Expect(m1.Equal(m2)).To(BeTrue())
		})

		DescribeTable("field comparison",
			func(cfg1, cfg2 *DeviceConfig, expected bool) {
				m1 := &managedDeviceConfig{DeviceConfig: *cfg1}
				m2 := &managedDeviceConfig{DeviceConfig: *cfg2}
				Expect(m1.Equal(m2)).To(Equal(expected))
				Expect(m2.Equal(m1)).To(Equal(expected), "Equal must be symmetric")
			},
			Entry("different Master",
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}, Master: "vrf0"},
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}, Master: "vrf1"},
				false),
			Entry("different BridgePortSettings",
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}}, BridgePortSettings: &BridgePortSettings{VLANTunnel: true}},
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}}, BridgePortSettings: &BridgePortSettings{VLANTunnel: false}},
				false),
			Entry("one BridgePortSettings nil",
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}}, BridgePortSettings: &BridgePortSettings{VLANTunnel: true}},
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}}, BridgePortSettings: nil},
				false),
			Entry("different link types",
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}}},
				&DeviceConfig{Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}, Table: 100}},
				false),
			Entry("different VXLAN FlowBased",
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, FlowBased: true}},
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, FlowBased: false}},
				false),
			Entry("different VXLAN VniFilter",
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VniFilter: true}},
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VniFilter: false}},
				false),
			Entry("different VXLAN Learning",
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: true}},
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false}},
				false),
			Entry("same VXLAN Learning",
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false}},
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false}},
				true),
			Entry("different VlanFiltering",
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)}},
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(false)}},
				false),
			Entry("different VlanDefaultPVID",
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](0)}},
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](1)}},
				false),
			Entry("one VlanDefaultPVID nil",
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](0)}},
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: nil}},
				false),
			Entry("both VlanDefaultPVID zero",
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](0)}},
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](0)}},
				true),
			Entry("different VLANParent",
				&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}}, VLANParent: "eth0"},
				&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}}, VLANParent: "eth1"},
				false),
			Entry("same VLANParent",
				&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}}, VLANParent: "eth0"},
				&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}}, VLANParent: "eth0"},
				true),
			Entry("nil vs nil Addresses",
				&DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}}, Addresses: nil},
				&DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}}, Addresses: nil},
				true),
			Entry("nil vs empty Addresses",
				&DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}}, Addresses: nil},
				&DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}}, Addresses: []netlink.Addr{}},
				false),
			Entry("same Addresses",
				&DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}}, Addresses: []netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}}},
				&DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}}, Addresses: []netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}}},
				true),
			Entry("different Addresses",
				&DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}}, Addresses: []netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}}},
				&DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}}, Addresses: []netlink.Addr{{IPNet: mustParseIPNet("10.0.0.2/32")}}},
				false),
			Entry("both MTU set to different values",
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 1500}}},
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 9000}}},
				false),
			Entry("MTU set vs unset",
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 1500}}},
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
				false),
			Entry("HardwareAddr set vs unset",
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0",
					HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}}},
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
				false),
			Entry("VXLAN SrcAddr 4-byte vs 16-byte (same IP, different representation)",
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, SrcAddr: net.IPv4(10, 0, 0, 1).To4()}},
				&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, SrcAddr: net.ParseIP("10.0.0.1")}},
				true),
			Entry("addresses in different order",
				&DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}},
					Addresses: []netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}, {IPNet: mustParseIPNet("10.0.0.2/32")}}},
				&DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}},
					Addresses: []netlink.Addr{{IPNet: mustParseIPNet("10.0.0.2/32")}, {IPNet: mustParseIPNet("10.0.0.1/32")}}},
				true),
			Entry("VLAN ParentIndex zero vs non-zero (0 = don't care, directional)",
				&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0", ParentIndex: 0}, VlanId: 10}, VLANParent: "br0"},
				&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0", ParentIndex: 0}, VlanId: 10}, VLANParent: "br0"},
				true),
			Entry("VLAN ParentIndex both non-zero and different (critical mismatch)",
				&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0", ParentIndex: 5}, VlanId: 10}, VLANParent: "br0"},
				&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0", ParentIndex: 6}, VlanId: 10}, VLANParent: "br0"},
				false),
		)
	})

	Describe("EnsureLink pointer-swap", func() {
		It("creates a new pointer on config change", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}},
				Addresses: nil,
			})).To(Succeed())
			ptr1 := controller.store["d0"]

			Expect(controller.EnsureLink(DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}},
				Addresses: []netlink.Addr{},
			})).To(Succeed())
			ptr2 := controller.store["d0"]
			Expect(ptr2).ToNot(BeIdenticalTo(ptr1),
				"config change should replace the pointer")
		})

		It("reuses the same pointer on identical config", func() {
			cfg := DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   100,
					SrcAddr:   net.ParseIP("10.0.0.1"),
				},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())
			ptr1 := controller.store["vxlan0"]

			Expect(controller.EnsureLink(cfg)).To(Succeed())
			ptr2 := controller.store["vxlan0"]
			Expect(ptr2).To(BeIdenticalTo(ptr1),
				"identical config should not replace the pointer")
		})
	})

	Describe("handleLinkUpdate", func() {
		It("enqueues pending device when master appears", func() {
			// Fresh mock needed: this test has a two-phase flow with different
			// expectations per phase that conflict with the outer BeforeEach mock.
			nlMock := &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
			defer util.ResetNetLinkOpMockInst()

			cfg := DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "svi0"}},
				Master: "vrf0",
			}

			vrfLink := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0", Index: 10}}
			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "svi0", Index: 5, Flags: net.FlagUp}}
			linkNotFoundErr := netlink.LinkNotFoundError{}

			// Phase 1: EnsureLink stores config as Pending, then reconcileDeviceKey
			nlMock.On("LinkByName", "svi0").Return(nil, linkNotFoundErr).Once() // getLink: device doesn't exist
			nlMock.On("LinkByName", "vrf0").Return(nil, linkNotFoundErr).Once() // master resolution: missing
			nlMock.On("IsLinkNotFoundError", mock.Anything).Return(true)

			Expect(controller.EnsureLink(cfg)).To(Succeed())
			Expect(controller.reconcileDeviceKey("svi0")).To(Succeed())
			Expect(controller.store["svi0"].state.ifindex.Load()).To(BeZero(),
				"device should be pending due to missing master")

			// Phase 2: Master appears -> handleLinkUpdate enqueues, then reconcileDeviceKey succeeds
			nlMock.On("LinkByName", "svi0").Return(nil, linkNotFoundErr).Once() // getLink: device doesn't exist
			nlMock.On("LinkByName", "vrf0").Return(vrfLink, nil)                // master resolution: exists
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil).Once()
			nlMock.On("LinkByName", "svi0").Return(bridgeLink, nil).Once() // createLink: fetch created link
			nlMock.On("LinkSetAlias", bridgeLink, managedAliasPrefix+"bridge:svi0").Return(nil).Once()
			nlMock.On("LinkSetMaster", bridgeLink, vrfLink).Return(nil).Once()
			nlMock.On("LinkSetUp", bridgeLink).Return(nil).Once()

			controller.handleLinkUpdate(netlink.LinkUpdate{
				Header: unix.NlMsghdr{Type: unix.RTM_NEWLINK},
				Link:   vrfLink,
			})
			Expect(controller.reconcileDeviceKey("svi0")).To(Succeed())
		})

		DescribeTable("reconciliation decisions",
			func(cfg DeviceConfig, update netlink.LinkUpdate, expectReconcile bool) {
				Expect(controller.EnsureLink(cfg)).To(Succeed())

				fakeReconciler := &controllerPkg.FakeController{}
				controller.reconciler = fakeReconciler

				controller.handleLinkUpdate(update)

				key := "Reconcile:device/" + cfg.Link.Attrs().Name
				if expectReconcile {
					Expect(fakeReconciler.Reconciles).To(ContainElement(key))
				} else {
					Expect(fakeReconciler.Reconciles).To(BeEmpty())
				}
			},
			Entry("skip: unrelated link name",
				DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
				netlink.LinkUpdate{
					Header: unix.NlMsghdr{Type: unix.RTM_NEWLINK},
					Link:   &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				},
				false),
			Entry("skip: non-delete event without our alias",
				DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
				netlink.LinkUpdate{
					Header: unix.NlMsghdr{Type: unix.RTM_NEWLINK},
					Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
						Name: "br0", Index: 10, Flags: net.FlagUp,
					}},
				},
				false),
			Entry("reconcile: delete event without our alias",
				DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
				netlink.LinkUpdate{
					Header: unix.NlMsghdr{Type: unix.RTM_DELLINK},
					Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
						Name: "br0", Index: 10,
					}},
				},
				true),
			Entry("reconcile: owned link with drifted MTU",
				DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 9000}}},
				netlink.LinkUpdate{
					Header: unix.NlMsghdr{Type: unix.RTM_NEWLINK},
					Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
						Name: "br0", Alias: managedAliasPrefix + "bridge:br0",
						Index: 10, MTU: 1500, Flags: net.FlagUp,
					}},
				},
				true),
			Entry("reconcile: unsupported link type with our alias",
				DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
				netlink.LinkUpdate{
					Header: unix.NlMsghdr{Type: unix.RTM_NEWLINK},
					Link: &netlink.Device{LinkAttrs: netlink.LinkAttrs{
						Name: "br0", Alias: managedAliasPrefix + "bridge:br0",
						Index: 10, Flags: net.FlagUp,
					}},
				},
				true),
			Entry("skip: owned link matching desired state",
				DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
				netlink.LinkUpdate{
					Header: unix.NlMsghdr{Type: unix.RTM_NEWLINK},
					Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
						Name: "br0", Alias: managedAliasPrefix + "bridge:br0",
						Index: 10, Flags: net.FlagUp,
					}},
				},
				false),
		)

	})

	Describe("handleAddrUpdate", func() {
		It("ignores addr update for untracked ifindex", func() {
			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler

			state := &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}}},
			}
			state.state.setIfindex(10)
			controller.store["br0"] = state

			controller.handleAddrUpdate(netlink.AddrUpdate{LinkIndex: 99})

			Expect(fakeReconciler.Reconciles).To(BeEmpty())
		})

		It("ignores addr update when ifindex is not yet populated", func() {
			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler

			controller.store["br0"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}}},
			}

			controller.handleAddrUpdate(netlink.AddrUpdate{LinkIndex: 10})

			Expect(fakeReconciler.Reconciles).To(BeEmpty())
		})

		DescribeTable("ignores link-local address updates",
			func(addr string) {
				fakeReconciler := &controllerPkg.FakeController{}
				controller.reconciler = fakeReconciler

				state := &managedDevice{
					cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{
						Link:      &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
						Addresses: []netlink.Addr{},
					}},
				}
				state.state.setIfindex(10)
				controller.store["br0"] = state

				controller.handleAddrUpdate(netlink.AddrUpdate{
					LinkIndex:   10,
					LinkAddress: *mustParseIPNet(addr),
					NewAddr:     true,
				})

				Expect(fakeReconciler.Reconciles).To(BeEmpty())
			},
			Entry("IPv6 link-local", "fe80::1/64"),
			Entry("IPv4 link-local", "169.254.1.1/16"),
		)

		DescribeTable("addr reconcile decisions",
			func(desiredAddrs []string, updateAddr string, newAddr bool, expectReconcile bool) {
				fakeReconciler := &controllerPkg.FakeController{}
				controller.reconciler = fakeReconciler

				var addresses []netlink.Addr
				if desiredAddrs != nil {
					addresses = make([]netlink.Addr, 0, len(desiredAddrs))
					for _, a := range desiredAddrs {
						addresses = append(addresses, netlink.Addr{IPNet: mustParseIPNet(a)})
					}
				}

				state := &managedDevice{
					cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{
						Link:      &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
						Addresses: addresses,
					}},
				}
				state.state.setIfindex(10)
				controller.store["br0"] = state

				controller.handleAddrUpdate(netlink.AddrUpdate{
					LinkIndex:   10,
					LinkAddress: *mustParseIPNet(updateAddr),
					NewAddr:     newAddr,
				})

				if expectReconcile {
					Expect(fakeReconciler.Reconciles).To(ContainElement("Reconcile:device/br0"))
				} else {
					Expect(fakeReconciler.Reconciles).To(BeEmpty())
				}
			},
			Entry("enqueues when update diverges from desired state",
				[]string{}, "10.0.0.1/32", true, true),
			Entry("skips when desired address is added (already converged)",
				[]string{"10.0.0.1/32"}, "10.0.0.1/32", true, false),
			Entry("reconciles when desired address is removed",
				[]string{"10.0.0.1/32"}, "10.0.0.1/32", false, true),
			Entry("reconciles when unexpected address is added",
				[]string{"10.0.0.1/32"}, "10.0.0.99/32", true, true),
			Entry("skips when unexpected address is removed (converging)",
				[]string{"10.0.0.1/32"}, "10.0.0.99/32", false, false),
			Entry("skips all events when Addresses is nil (no address management)",
				nil, "10.0.0.1/32", true, false),
		)
	})

	Describe("reconcileVxlanSyncKey", func() {
		It("only enqueues VXLAN devices", func() {
			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler

			bridgeAttrs := netlink.NewLinkAttrs()
			bridgeAttrs.Name = "br0"
			controller.store["br0"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Bridge{LinkAttrs: bridgeAttrs}}},
			}

			vxlanAttrs := netlink.NewLinkAttrs()
			vxlanAttrs.Name = "vxlan0"
			controller.store["vxlan0"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: vxlanAttrs}}},
			}

			vlanAttrs := netlink.NewLinkAttrs()
			vlanAttrs.Name = "vlan100"
			controller.store["vlan100"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Vlan{LinkAttrs: vlanAttrs}}},
			}

			vrfAttrs := netlink.NewLinkAttrs()
			vrfAttrs.Name = "vrf0"
			controller.store["vrf0"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Vrf{LinkAttrs: vrfAttrs}}},
			}

			dummyAttrs := netlink.NewLinkAttrs()
			dummyAttrs.Name = "dummy0"
			controller.store["dummy0"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Dummy{LinkAttrs: dummyAttrs}}},
			}

			Expect(controller.reconcileVxlanSyncKey()).To(Succeed())

			Expect(fakeReconciler.Reconciles).To(ConsistOf("After:device/vxlan0"))
		})

		It("enqueues multiple VXLAN devices", func() {
			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler

			vxlan1Attrs := netlink.NewLinkAttrs()
			vxlan1Attrs.Name = "vxlan0"
			controller.store["vxlan0"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: vxlan1Attrs}}},
			}

			vxlan2Attrs := netlink.NewLinkAttrs()
			vxlan2Attrs.Name = "vxlan1"
			controller.store["vxlan1"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: vxlan2Attrs}}},
			}

			bridgeAttrs := netlink.NewLinkAttrs()
			bridgeAttrs.Name = "br0"
			controller.store["br0"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Bridge{LinkAttrs: bridgeAttrs}}},
			}

			Expect(controller.reconcileVxlanSyncKey()).To(Succeed())

			Expect(fakeReconciler.Reconciles).To(ConsistOf(
				"After:device/vxlan0",
				"After:device/vxlan1",
			))
		})

		It("enqueues nothing when no VXLAN devices exist", func() {
			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler

			bridgeAttrs := netlink.NewLinkAttrs()
			bridgeAttrs.Name = "br0"
			controller.store["br0"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Bridge{LinkAttrs: bridgeAttrs}}},
			}

			Expect(controller.reconcileVxlanSyncKey()).To(Succeed())

			Expect(fakeReconciler.Reconciles).To(BeEmpty())
		})
	})

	Describe("EnsureLink Addresses defensive copy", func() {
		It("caller mutation after EnsureLink does not affect stored config", func() {
			addr1, _ := netlink.ParseAddr("10.0.0.1/32")
			addrs := []netlink.Addr{*addr1}

			Expect(controller.EnsureLink(DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}},
				Addresses: addrs,
			})).To(Succeed())

			// Mutate caller's slice
			addr2, _ := netlink.ParseAddr("10.0.0.99/32")
			addrs[0] = *addr2

			stored := controller.store["d0"].cfg.Addresses
			Expect(stored).To(HaveLen(1))
			Expect(stored[0].IP.String()).To(Equal("10.0.0.1"))
		})
	})

	DescribeTable("EnsureLink name validation edge cases",
		func(cfg DeviceConfig, expectErr bool) {
			err := controller.EnsureLink(cfg)
			if expectErr {
				Expect(err).To(HaveOccurred())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
		},
		Entry("accepts exactly 15-character name",
			DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "exactly15chars_"}}},
			false),
		Entry("rejects 16-character name",
			DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "exactly16chars__"}}},
			true),
		Entry("accepts 15-character master name",
			DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}}, Master: "exactly15chars_"},
			false),
		Entry("rejects 16-character master name",
			DeviceConfig{Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}}, Master: "exactly16chars__"},
			true),
		Entry("accepts 15-character VLANParent name",
			DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "v0"}, VlanId: 10}, VLANParent: "exactly15chars_"},
			false),
		Entry("rejects 16-character VLANParent name",
			DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "v0"}, VlanId: 10}, VLANParent: "exactly16chars__"},
			true),
	)

	Describe("isNotOwnedError", func() {
		It("returns true for notOwnedError", func() {
			err := &notOwnedError{deviceName: "br0", reason: "foreign alias"}
			Expect(isNotOwnedError(err)).To(BeTrue())
		})

		It("returns true for wrapped notOwnedError", func() {
			inner := &notOwnedError{deviceName: "br0", reason: "foreign alias"}
			wrapped := fmt.Errorf("outer: %w", inner)
			Expect(isNotOwnedError(wrapped)).To(BeTrue())
		})

		It("returns false for other errors", func() {
			Expect(isNotOwnedError(errors.New("some error"))).To(BeFalse())
		})

		It("returns false for nil", func() {
			Expect(isNotOwnedError(nil)).To(BeFalse())
		})
	})

	Describe("reconcileFullSyncKey", func() {
		It("continues sync even when orphan cleanup fails", func() {
			nlMock.On("LinkList").Return(nil, errors.New("netlink error"))
			Expect(controller.reconcileFullSyncKey()).To(Succeed())
		})

		It("enqueues all device types, not just VXLAN", func() {
			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler
			nlMock.On("LinkList").Return([]netlink.Link{}, nil)

			bridgeAttrs := netlink.NewLinkAttrs()
			bridgeAttrs.Name = "br0"
			controller.store["br0"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Bridge{LinkAttrs: bridgeAttrs}}},
			}

			vxlanAttrs := netlink.NewLinkAttrs()
			vxlanAttrs.Name = "vxlan0"
			controller.store["vxlan0"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: vxlanAttrs}}},
			}

			vlanAttrs := netlink.NewLinkAttrs()
			vlanAttrs.Name = "vlan100"
			controller.store["vlan100"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Vlan{LinkAttrs: vlanAttrs}}},
			}

			dummyAttrs := netlink.NewLinkAttrs()
			dummyAttrs.Name = "dummy0"
			controller.store["dummy0"] = &managedDevice{
				cfg: managedDeviceConfig{DeviceConfig: DeviceConfig{Link: &netlink.Dummy{LinkAttrs: dummyAttrs}}},
			}

			Expect(controller.reconcileFullSyncKey()).To(Succeed())

			Expect(fakeReconciler.Reconciles).To(ConsistOf(
				"After:device/br0",
				"After:device/vxlan0",
				"After:device/vlan100",
				"After:device/dummy0",
			))
		})
	})

})

// mustParseIPNet parses a CIDR string and panics on error (for test convenience).
// Note: net.ParseCIDR returns the network address (e.g., "10.0.0.1/24" -> "10.0.0.0/24")
func mustParseIPNet(cidr string) *net.IPNet {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return ipnet
}

// mustParseIPNetWithIP parses a CIDR string and preserves the IP (not network) address.
// Use this when you need the specific IP, not the network address.
// e.g., "192.168.1.1/24" -> IPNet{IP: 192.168.1.1, Mask: /24}
func mustParseIPNetWithIP(cidr string) *net.IPNet {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	ipnet.IP = ip
	return ipnet
}
