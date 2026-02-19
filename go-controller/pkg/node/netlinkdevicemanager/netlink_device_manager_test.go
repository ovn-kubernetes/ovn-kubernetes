package netlinkdevicemanager

import (
	"errors"
	"fmt"
	"net"

	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	nl "github.com/vishvananda/netlink/nl"

	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

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
			// createLink: LinkAdd + re-fetch
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil)
			nlMock.On("LinkByName", "br0").Return(createdBridge, nil)
			nlMock.On("LinkSetAlias", createdBridge, ManagedAliasPrefix+"bridge:br0").Return(nil)
			// ensureDeviceUp: re-fetch, not up -> bring up
			nlMock.On("LinkSetUp", createdBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateReady))
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

			// resolveDependencies: master exists
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			// applyDeviceConfig: VXLAN doesn't exist
			nlMock.On("LinkByName", "vxlan0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			// createLink: LinkAdd + re-fetch
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vxlan")).Return(nil)
			nlMock.On("LinkByName", "vxlan0").Return(createdVxlan, nil)
			nlMock.On("LinkSetAlias", createdVxlan, ManagedAliasPrefix+"vxlan:vxlan0").Return(nil)
			// setMaster + bridge port settings
			nlMock.On("LinkSetMaster", createdVxlan, bridgeLink).Return(nil)
			nlMock.On("LinkSetVlanTunnel", createdVxlan, true).Return(nil)
			nlMock.On("LinkSetBrNeighSuppress", createdVxlan, true).Return(nil)
			nlMock.On("LinkSetLearning", createdVxlan, false).Return(nil)
			// ensureDeviceUp
			nlMock.On("LinkSetUp", createdVxlan).Return(nil)

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed())
			Expect(controller.GetDeviceState("vxlan0")).To(Equal(DeviceStateReady))
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
			nlMock.On("LinkSetAlias", createdDummy, ManagedAliasPrefix+"dummy:dummy0").Return(nil)
			nlMock.On("LinkSetUp", createdDummy).Return(nil)
			// syncAddresses: no current addresses -> add desired
			nlMock.On("AddrList", createdDummy, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", createdDummy, mock.MatchedBy(func(addr *netlink.Addr) bool {
				return addr.IPNet.String() == "10.0.0.1/32"
			})).Return(nil)

			Expect(controller.reconcileDeviceKey("dummy0")).To(Succeed())
			Expect(controller.GetDeviceState("dummy0")).To(Equal(DeviceStateReady))
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

			// resolveDependencies: VLANParent exists
			nlMock.On("LinkByName", "br0").Return(parentBridge, nil)
			// applyDeviceConfig: VLAN doesn't exist
			nlMock.On("LinkByName", "vlan100").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			// createLink: LinkAdd + re-fetch
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vlan")).Return(nil)
			nlMock.On("LinkByName", "vlan100").Return(createdVlan, nil)
			nlMock.On("LinkSetAlias", createdVlan, ManagedAliasPrefix+"vlan:vlan100").Return(nil)
			// ensureDeviceUp
			nlMock.On("LinkSetUp", createdVlan).Return(nil)

			Expect(controller.reconcileDeviceKey("vlan100")).To(Succeed())
			Expect(controller.GetDeviceState("vlan100")).To(Equal(DeviceStateReady))
			nlMock.AssertExpectations(GinkgoT())
		})

		It("rolls back on alias failure", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			createdBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10}}

			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil)
			nlMock.On("LinkByName", "br0").Return(createdBridge, nil)
			nlMock.On("LinkSetAlias", createdBridge, mock.Anything).Return(errors.New("alias failed"))
			// Rollback: should delete the partially created device
			nlMock.On("LinkDelete", createdBridge).Return(nil)

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred())
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateFailed))
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", createdBridge)
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
					Alias:       ManagedAliasPrefix + "vxlan:vxlan0",
					Flags:       net.FlagUp,
				},
				FlowBased: true,
				VniFilter: true,
			}

			// resolveDependencies: new master exists
			nlMock.On("LinkByName", "br-new").Return(newBridge, nil)
			// applyDeviceConfig: device exists with our alias
			nlMock.On("LinkByName", "vxlan0").Return(existingVxlan, nil)
			// updateDevice: needsLinkModify -> false (alias matches, mutable fields match)
			// Master changed: MasterIndex(5) != newBridge.Index(20)
			nlMock.On("LinkSetMaster", existingVxlan, newBridge).Return(nil)
			// Bridge port settings: masterChanged=true -> always apply
			nlMock.On("LinkSetVlanTunnel", existingVxlan, true).Return(nil)
			nlMock.On("LinkSetBrNeighSuppress", existingVxlan, true).Return(nil)
			nlMock.On("LinkSetLearning", existingVxlan, false).Return(nil)
			// ensureDeviceUp: already up (FlagUp set)

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed())
			Expect(controller.GetDeviceState("vxlan0")).To(Equal(DeviceStateReady))
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
					Alias:       ManagedAliasPrefix + "bridge:br0",
					Flags:       net.FlagUp,
				},
			}

			nlMock.On("LinkByName", "br0").Return(existingBridge, nil)
			nlMock.On("LinkSetNoMaster", existingBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateReady))
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
					Alias: ManagedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}

			nlMock.On("LinkByName", "br0").Return(existingBridge, nil)
			// needsLinkModify -> true (MTU differs)
			nlMock.On("LinkModify", mock.AnythingOfType("*netlink.Bridge")).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateReady))
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
					Alias: ManagedAliasPrefix + "vrf:vrf0",
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
			nlMock.On("LinkSetAlias", recreatedVrf, ManagedAliasPrefix+"vrf:vrf0").Return(nil)
			nlMock.On("LinkSetUp", recreatedVrf).Return(nil)

			Expect(controller.reconcileDeviceKey("vrf0")).To(Succeed())
			Expect(controller.GetDeviceState("vrf0")).To(Equal(DeviceStateReady))
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
					Alias: ManagedAliasPrefix + "dummy:dummy0",
					Flags: net.FlagUp,
				},
			}
			extraAddr := netlink.Addr{IPNet: mustParseIPNetWithIP("192.168.1.1/24")}
			linkLocalAddr := netlink.Addr{IPNet: mustParseIPNetWithIP("fe80::1/64")}

			nlMock.On("LinkByName", "dummy0").Return(existingDummy, nil)
			nlMock.On("AddrList", existingDummy, netlink.FAMILY_ALL).Return(
				[]netlink.Addr{extraAddr, linkLocalAddr}, nil)
			nlMock.On("AddrAdd", existingDummy, mock.MatchedBy(func(addr *netlink.Addr) bool {
				return addr.IPNet.String() == "10.0.0.1/32"
			})).Return(nil)
			nlMock.On("AddrDel", existingDummy, mock.MatchedBy(func(addr *netlink.Addr) bool {
				return addr.IPNet.String() == "192.168.1.1/24"
			})).Return(nil)

			Expect(controller.reconcileDeviceKey("dummy0")).To(Succeed())
			Expect(controller.GetDeviceState("dummy0")).To(Equal(DeviceStateReady))
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
					Alias:       ManagedAliasPrefix + "vxlan:vxlan0",
					Flags:       net.FlagUp,
				},
				FlowBased: true,
				VniFilter: true,
			}

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(existingVxlan, nil)
			// Master unchanged (MasterIndex matches) -> masterChanged=false
			// getBridgePortSettings returns matching settings -> skip apply
			nlMock.On("LinkGetProtinfo", existingVxlan).Return(netlink.Protinfo{
				VlanTunnel:    true,
				NeighSuppress: true,
				Learning:      false,
			}, nil)

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed())
			Expect(controller.GetDeviceState("vxlan0")).To(Equal(DeviceStateReady))
			nlMock.AssertNotCalled(GinkgoT(), "LinkSetVlanTunnel", mock.Anything, mock.Anything)
			nlMock.AssertNotCalled(GinkgoT(), "LinkSetLearning", mock.Anything, mock.Anything)
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
				Alias: ManagedAliasPrefix + "bridge:br0",
			}}

			nlMock.On("LinkByName", "br0").Return(kernelBridge, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)
			nlMock.On("LinkDelete", kernelBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(controller.Has("br0")).To(BeFalse())
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", kernelBridge)
		})

		It("swallows NotOwnedError for foreign device and does not delete", func() {
			foreignBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br0",
				Index: 10,
				Alias: "some-other-system:br0",
			}}

			nlMock.On("LinkByName", "br0").Return(foreignBridge, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)

			// Device not in store -> delete path -> NotOwnedError -> swallowed
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
	})

	Describe("Dependency and ownership scenarios", func() {
		It("transitions to Pending when master doesn't exist", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master: "br0",
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")

			// resolveDependencies: master not found -> DependencyError
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr)
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed()) // DependencyError -> nil
			Expect(controller.GetDeviceState("vxlan0")).To(Equal(DeviceStatePending))
		})

		It("transitions to Pending when VLANParent doesn't exist", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:       &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}, VlanId: 100},
				VLANParent: "br0",
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")

			// resolveDependencies: VLANParent not found -> DependencyError
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr)
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

			Expect(controller.reconcileDeviceKey("vlan100")).To(Succeed())
			Expect(controller.GetDeviceState("vlan100")).To(Equal(DeviceStatePending))
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
				Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateBlocked))
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
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateFailed))
		})

		It("transitions Pending -> Ready when dependency appears", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Master: "vrf0",
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")

			// First reconcile: master missing -> Pending
			nlMock.On("LinkByName", "vrf0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStatePending))

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
			nlMock.On("LinkSetAlias", createdBridge, ManagedAliasPrefix+"bridge:br0").Return(nil)
			nlMock.On("LinkSetMaster", createdBridge, vrfLink).Return(nil)
			nlMock.On("LinkSetUp", createdBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateReady))
		})

		It("master deleted during update -> transitions to Pending", func() {
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
					Alias:       ManagedAliasPrefix + "vxlan:vxlan0",
					Flags:       net.FlagUp,
				},
			}

			// resolveDependencies: master exists
			nlMock.On("LinkByName", "br0").Return(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}, nil).Once()
			// applyDeviceConfig: device exists
			nlMock.On("LinkByName", "vxlan0").Return(existingVxlan, nil)
			// updateDevice: master lookup -> deleted between resolve and update
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed()) // DependencyError -> nil
			Expect(controller.GetDeviceState("vxlan0")).To(Equal(DeviceStatePending))
		})
	})

	Describe("Reconciling VID/VNI mappings", func() {
		It("adds new mappings and removes stale ones", func() {
			Expect(controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{
				{VID: 10, VNI: 100},
				{VID: 20, VNI: 200},
			})).To(Succeed())

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}

			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			// Current: only VID=30/VNI=300 (stale)
			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{
				{Vid: 30, TunId: 300},
			}, nil)

			// Remove stale: VID=30/VNI=300
			nlMock.On("BridgeVlanDelTunnelInfo", vxlanLink, uint16(30), uint32(300), false, true).Return(nil)
			nlMock.On("BridgeVniDel", vxlanLink, uint32(300)).Return(nil)
			nlMock.On("BridgeVlanDel", vxlanLink, uint16(30), false, false, false, true).Return(nil)

			// Add desired: all 4 steps per mapping (bridge self VLAN, VXLAN VID, VNI filter, tunnel-info)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(10), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(100)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(10), uint32(100), false, true).Return(nil)

			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(20), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(20), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(200)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(20), uint32(200), false, true).Return(nil)

			// addVIDVNIMapping doesn't call IsAlreadyExistsError when the main call succeeds
			Expect(controller.reconcileMappingsKey("vxlan0")).To(Succeed())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("returns nil when VXLAN not yet created (dependency pending)", func() {
			Expect(controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{{VID: 10, VNI: 100}})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			nlMock.On("LinkByName", "vxlan0").Return(nil, linkNotFoundErr)
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

			Expect(controller.reconcileMappingsKey("vxlan0")).To(Succeed())
		})

		It("returns nil when mappings not in store", func() {
			Expect(controller.reconcileMappingsKey("nonexistent")).To(Succeed())
		})

		It("handles idempotent adds (EEXIST) — self-healing re-applies even when tunnel-info matches", func() {
			Expect(controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{{VID: 10, VNI: 100}})).To(Succeed())

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}
			eexistErr := errors.New("file exists")

			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{
				{Vid: 10, TunId: 100}, // Already exists in tunnel-info
			}, nil)

			// All adds return EEXIST -> should be tolerated
			nlMock.On("BridgeVlanAdd", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(eexistErr)
			nlMock.On("IsAlreadyExistsError", eexistErr).Return(true)
			nlMock.On("BridgeVniAdd", mock.Anything, mock.Anything).Return(eexistErr)
			nlMock.On("BridgeVlanAddTunnelInfo", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(eexistErr)

			Expect(controller.reconcileMappingsKey("vxlan0")).To(Succeed())

			// Self-healing contract: all 4 components are re-applied even when
			// tunnel-info reports the mapping as present.
			nlMock.AssertCalled(GinkgoT(), "BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false)
			nlMock.AssertCalled(GinkgoT(), "BridgeVlanAdd", vxlanLink, uint16(10), false, false, false, true)
			nlMock.AssertCalled(GinkgoT(), "BridgeVniAdd", vxlanLink, uint32(100))
			nlMock.AssertCalled(GinkgoT(), "BridgeVlanAddTunnelInfo", vxlanLink, uint16(10), uint32(100), false, true)
		})
	})

	Describe("State transitions and subscriber notifications", func() {
		It("notifies subscriber on Pending -> Ready transition", func() {
			var notifiedDevices []string
			controller.RegisterDeviceReconciler(&mockReconciler{
				fn: func(key string) error {
					notifiedDevices = append(notifiedDevices, key)
					return nil
				},
			})

			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			createdBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10}}

			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil)
			nlMock.On("LinkByName", "br0").Return(createdBridge, nil)
			nlMock.On("LinkSetAlias", createdBridge, mock.Anything).Return(nil)
			nlMock.On("LinkSetUp", createdBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(notifiedDevices).To(ContainElement("br0"))
		})

		It("notifies subscriber on Pending -> Blocked transition", func() {
			var notifiedDevices []string
			controller.RegisterDeviceReconciler(&mockReconciler{
				fn: func(key string) error {
					notifiedDevices = append(notifiedDevices, key)
					return nil
				},
			})

			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			foreignBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br0",
				Index: 10,
				Alias: "foreign:br0",
			}}
			nlMock.On("LinkByName", "br0").Return(foreignBridge, nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(notifiedDevices).To(ContainElement("br0"))
		})

		It("does NOT notify when state stays the same (Ready -> Ready)", func() {
			notifyCount := 0
			controller.RegisterDeviceReconciler(&mockReconciler{
				fn: func(_ string) error {
					notifyCount++
					return nil
				},
			})

			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			// Both reconciles see device already existing with our alias and UP.
			// This exercises the update-idempotency path directly.
			brLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 10,
					Alias: ManagedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}
			nlMock.On("LinkByName", "br0").Return(brLink, nil)

			// First reconcile: Pending -> Ready (notified)
			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(notifyCount).To(Equal(1))
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateReady))

			// Second reconcile: Ready -> Ready (should NOT notify again)
			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(notifyCount).To(Equal(1)) // Still 1, not 2
		})

		It("notifies subscriber on delete path", func() {
			var notifiedDevices []string
			controller.RegisterDeviceReconciler(&mockReconciler{
				fn: func(key string) error {
					notifiedDevices = append(notifiedDevices, key)
					return nil
				},
			})

			linkNotFoundErr := errors.New("link not found")
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr)
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

			// Device not in store -> delete path -> notifies
			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(notifiedDevices).To(ContainElement("br0"))
		})

		It("skips state update when config changed during I/O (staleness guard)", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Master: "vrf-old",
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			vrfOld := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf-old", Index: 10}}
			createdBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br0",
				Index: 5,
				Alias: ManagedAliasPrefix + "bridge:br0",
			}}

			nlMock.On("LinkByName", "vrf-old").Return(vrfOld, nil)
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			nlMock.On("IsLinkNotFoundError", mock.MatchedBy(func(e error) bool { return e != linkNotFoundErr })).Return(false)

			// During LinkAdd, simulate concurrent EnsureLink changing the config
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil).Once().Run(func(_ mock.Arguments) {
				controller.mu.Lock()
				controller.store["br0"] = &managedDevice{
					cfg: DeviceConfig{
						Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
						Master: "vrf-new", // Config changed!
					},
					state: DeviceStatePending,
				}
				controller.mu.Unlock()
			})
			nlMock.On("LinkByName", "br0").Return(createdBridge, nil)
			nlMock.On("LinkSetAlias", mock.Anything, mock.Anything).Return(nil)
			nlMock.On("LinkSetMaster", mock.Anything, mock.Anything).Return(nil)
			nlMock.On("LinkSetUp", mock.Anything).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())

			// State should NOT be updated to Ready because config was stale
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStatePending))
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
				Alias: ManagedAliasPrefix + "bridge:br-desired",
			}}
			orphanBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br-orphan",
				Index: 2,
				Alias: ManagedAliasPrefix + "bridge:br-orphan",
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
				Name: "br-orphan1", Index: 2, Alias: ManagedAliasPrefix + "bridge:br-orphan1",
			}}
			orphan2 := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name: "br-orphan2", Index: 3, Alias: ManagedAliasPrefix + "bridge:br-orphan2",
			}}

			nlMock.On("LinkList").Return([]netlink.Link{orphan1, orphan2}, nil)
			nlMock.On("LinkDelete", orphan1).Return(errors.New("device busy"))
			nlMock.On("LinkDelete", orphan2).Return(nil)

			Expect(controller.cleanupOrphanedDevices()).To(Succeed())
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", orphan1)
			nlMock.AssertCalled(GinkgoT(), "LinkDelete", orphan2)
		})
	})

	Describe("Error handling during device creation", func() {
		It("returns error when LinkDelete fails during delete path", func() {
			kernelBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
				Name:  "br0",
				Index: 10,
				Alias: ManagedAliasPrefix + "bridge:br0",
			}}

			nlMock.On("LinkByName", "br0").Return(kernelBridge, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)
			nlMock.On("LinkDelete", kernelBridge).Return(errors.New("device busy"))

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("device busy"))
		})

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
			nlMock.On("LinkSetAlias", createdBridge, mock.Anything).Return(nil)
			nlMock.On("LinkSetUp", createdBridge).Return(errors.New("ENOMEM"))

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred())
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateFailed))
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

				nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
				nlMock.On("LinkByName", "vxlan0").Return(nil, linkNotFoundErr).Once()
				nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
				nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vxlan")).Return(nil)
				nlMock.On("LinkByName", "vxlan0").Return(createdVxlan, nil)
				nlMock.On("LinkSetAlias", createdVxlan, mock.Anything).Return(nil)
				nlMock.On("LinkSetMaster", createdVxlan, bridgeLink).Return(nil)
				setupMocks(nlMock, bridgeLink, createdVxlan)

				err := controller.reconcileDeviceKey("vxlan0")
				Expect(err).To(HaveOccurred())
				Expect(controller.GetDeviceState("vxlan0")).To(Equal(DeviceStateFailed))
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
					Alias: ManagedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}

			nlMock.On("LinkByName", "br0").Return(existingBridge, nil)
			nlMock.On("LinkModify", mock.AnythingOfType("*netlink.Bridge")).Return(errors.New("permission denied"))

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred())
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateFailed))
		})

		It("skips bridge port settings when getBridgePortSettings returns error", func() {
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
					Alias:       ManagedAliasPrefix + "vxlan:vxlan0",
					Flags:       net.FlagUp,
				},
				FlowBased: true,
				VniFilter: true,
			}

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(existingVxlan, nil)
			// getBridgePortSettings fails -> ensureBridgePortSettings skips (logs, no error)
			nlMock.On("LinkGetProtinfo", existingVxlan).Return(netlink.Protinfo{}, errors.New("no bridge port info"))

			Expect(controller.reconcileDeviceKey("vxlan0")).To(Succeed())
			Expect(controller.GetDeviceState("vxlan0")).To(Equal(DeviceStateReady))
			nlMock.AssertNotCalled(GinkgoT(), "LinkSetVlanTunnel", mock.Anything, mock.Anything)
		})
	})

	Describe("resolveDependencies validation through reconciler", func() {
		It("fails when VLANParent is set on a Bridge (type mismatch)", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:       &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				VLANParent: "eth0",
			})).To(Succeed())

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred())
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateFailed))
		})

		It("fails when VLANParent is set on a Vxlan (type mismatch)", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:       &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				VLANParent: "eth0",
			})).To(Succeed())

			err := controller.reconcileDeviceKey("vxlan0")
			Expect(err).To(HaveOccurred())
			Expect(controller.GetDeviceState("vxlan0")).To(Equal(DeviceStateFailed))
		})

		It("fails when VLAN has neither VLANParent nor ParentIndex", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vlan100"},
					VlanId:    100,
				},
			})).To(Succeed())

			err := controller.reconcileDeviceKey("vlan100")
			Expect(err).To(HaveOccurred())
			Expect(controller.GetDeviceState("vlan100")).To(Equal(DeviceStateFailed))
		})

		It("transitions to Pending when legacy ParentIndex parent not found", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vlan100", ParentIndex: 999},
					VlanId:    100,
				},
			})).To(Succeed())

			linkNotFoundErr := errors.New("link not found")
			nlMock.On("LinkByIndex", 999).Return(nil, linkNotFoundErr)
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

			Expect(controller.reconcileDeviceKey("vlan100")).To(Succeed())
			Expect(controller.GetDeviceState("vlan100")).To(Equal(DeviceStatePending))
		})

		It("creates device when legacy ParentIndex parent exists", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vlan100", ParentIndex: 5},
					VlanId:    100,
				},
			})).To(Succeed())

			parentLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			linkNotFoundErr := errors.New("link not found")
			createdVlan := &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{
				Name:        "vlan100",
				Index:       30,
				ParentIndex: 5,
			}, VlanId: 100}

			nlMock.On("LinkByIndex", 5).Return(parentLink, nil)
			nlMock.On("LinkByName", "vlan100").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vlan")).Return(nil)
			nlMock.On("LinkByName", "vlan100").Return(createdVlan, nil)
			nlMock.On("LinkSetAlias", createdVlan, ManagedAliasPrefix+"vlan:vlan100").Return(nil)
			nlMock.On("LinkSetUp", createdVlan).Return(nil)

			Expect(controller.reconcileDeviceKey("vlan100")).To(Succeed())
			Expect(controller.GetDeviceState("vlan100")).To(Equal(DeviceStateReady))
		})

		It("transitions to Failed on non-link-not-found error from master lookup", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Master: "vrf0",
			})).To(Succeed())

			permErr := errors.New("permission denied")
			nlMock.On("LinkByName", "vrf0").Return(nil, permErr)
			nlMock.On("IsLinkNotFoundError", permErr).Return(false)

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred())
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateFailed))
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
					Alias: ManagedAliasPrefix + "dummy:dummy0",
					Flags: net.FlagUp,
				},
			}
			eexistErr := errors.New("file exists")

			nlMock.On("LinkByName", "dummy0").Return(existingDummy, nil)
			nlMock.On("AddrList", existingDummy, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", existingDummy, mock.Anything).Return(eexistErr)
			nlMock.On("IsAlreadyExistsError", eexistErr).Return(true)

			Expect(controller.reconcileDeviceKey("dummy0")).To(Succeed())
			Expect(controller.GetDeviceState("dummy0")).To(Equal(DeviceStateReady))
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
					Alias: ManagedAliasPrefix + "dummy:dummy0",
					Flags: net.FlagUp,
				},
			}
			extraAddr := netlink.Addr{IPNet: mustParseIPNetWithIP("192.168.1.1/24")}
			enoentErr := errors.New("no such address")

			nlMock.On("LinkByName", "dummy0").Return(existingDummy, nil)
			nlMock.On("AddrList", existingDummy, netlink.FAMILY_ALL).Return([]netlink.Addr{extraAddr}, nil)
			nlMock.On("AddrDel", existingDummy, mock.Anything).Return(enoentErr)
			nlMock.On("IsEntryNotFoundError", enoentErr).Return(true)

			Expect(controller.reconcileDeviceKey("dummy0")).To(Succeed())
			Expect(controller.GetDeviceState("dummy0")).To(Equal(DeviceStateReady))
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
					Alias: ManagedAliasPrefix + "dummy:dummy0",
					Flags: net.FlagUp,
				},
			}
			permErr := errors.New("permission denied")

			nlMock.On("LinkByName", "dummy0").Return(existingDummy, nil)
			nlMock.On("AddrList", existingDummy, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", existingDummy, mock.Anything).Return(permErr)
			nlMock.On("IsAlreadyExistsError", permErr).Return(false)

			err := controller.reconcileDeviceKey("dummy0")
			Expect(err).To(HaveOccurred())
			Expect(controller.GetDeviceState("dummy0")).To(Equal(DeviceStateFailed))
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
					Alias: ManagedAliasPrefix + "dummy:dummy0",
					Flags: net.FlagUp,
				},
			}
			extraAddr := netlink.Addr{IPNet: mustParseIPNetWithIP("192.168.1.1/24")}
			permErr := errors.New("permission denied")

			nlMock.On("LinkByName", "dummy0").Return(existingDummy, nil)
			nlMock.On("AddrList", existingDummy, netlink.FAMILY_ALL).Return([]netlink.Addr{extraAddr}, nil)
			nlMock.On("AddrDel", existingDummy, mock.Anything).Return(permErr)
			nlMock.On("IsEntryNotFoundError", permErr).Return(false)

			err := controller.reconcileDeviceKey("dummy0")
			Expect(err).To(HaveOccurred())
			Expect(controller.GetDeviceState("dummy0")).To(Equal(DeviceStateFailed))
		})
	})

	Describe("Mapping error handling through reconciler", func() {
		It("returns error when addVIDVNIMapping bridge self VLAN fails", func() {
			Expect(controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{{VID: 10, VNI: 100}})).To(Succeed())

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}

			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{}, nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false).Return(errors.New("ENOMEM"))
			nlMock.On("IsAlreadyExistsError", mock.Anything).Return(false)

			err := controller.reconcileMappingsKey("vxlan0")
			Expect(err).To(HaveOccurred())
		})

		It("succeeds when removeVIDVNIMapping entries already gone (idempotent)", func() {
			Expect(controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{})).To(Succeed())

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}
			entryNotFoundErr := errors.New("entry not found")

			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{
				{Vid: 30, TunId: 300},
			}, nil)
			nlMock.On("BridgeVlanDelTunnelInfo", vxlanLink, uint16(30), uint32(300), false, true).Return(entryNotFoundErr)
			nlMock.On("IsEntryNotFoundError", entryNotFoundErr).Return(true)
			nlMock.On("BridgeVniDel", vxlanLink, uint32(300)).Return(entryNotFoundErr)
			nlMock.On("BridgeVlanDel", vxlanLink, uint16(30), false, false, false, true).Return(entryNotFoundErr)

			Expect(controller.reconcileMappingsKey("vxlan0")).To(Succeed())
		})

		It("returns error when removeVIDVNIMapping has non-entry-not-found error", func() {
			Expect(controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{})).To(Succeed())

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}
			realErr := errors.New("device busy")

			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{
				{Vid: 30, TunId: 300},
			}, nil)
			nlMock.On("BridgeVlanDelTunnelInfo", vxlanLink, uint16(30), uint32(300), false, true).Return(realErr)
			nlMock.On("IsEntryNotFoundError", realErr).Return(false)
			nlMock.On("BridgeVniDel", vxlanLink, uint32(300)).Return(nil)
			nlMock.On("BridgeVlanDel", vxlanLink, uint16(30), false, false, false, true).Return(nil)

			err := controller.reconcileMappingsKey("vxlan0")
			Expect(err).To(HaveOccurred())
		})

		It("collects multiple errors from removeVIDVNIMapping (best-effort cleanup)", func() {
			Expect(controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{})).To(Succeed())

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}
			err1 := errors.New("error1")
			err2 := errors.New("error2")
			entryNotFoundErr := errors.New("entry not found")

			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{
				{Vid: 30, TunId: 300},
			}, nil)
			nlMock.On("BridgeVlanDelTunnelInfo", vxlanLink, uint16(30), uint32(300), false, true).Return(err1)
			nlMock.On("IsEntryNotFoundError", err1).Return(false)
			nlMock.On("BridgeVniDel", vxlanLink, uint32(300)).Return(entryNotFoundErr)
			nlMock.On("IsEntryNotFoundError", entryNotFoundErr).Return(true)
			nlMock.On("BridgeVlanDel", vxlanLink, uint16(30), false, false, false, true).Return(err2)
			nlMock.On("IsEntryNotFoundError", err2).Return(false)

			err := controller.reconcileMappingsKey("vxlan0")
			Expect(err).To(HaveOccurred())
		})

		It("returns aggregate error on partial mapping failure", func() {
			Expect(controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{
				{VID: 10, VNI: 100},
				{VID: 20, VNI: 200},
			})).To(Succeed())

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}

			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{}, nil)

			// First mapping succeeds
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(10), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(100)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(10), uint32(100), false, true).Return(nil)

			// Second mapping fails at bridge self VLAN
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(20), false, false, true, false).Return(errors.New("ENOMEM"))
			nlMock.On("IsAlreadyExistsError", mock.Anything).Return(false)

			err := controller.reconcileMappingsKey("vxlan0")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to apply"))
		})
	})

	Describe("validateMappings", func() {
		It("rejects duplicate VIDs", func() {
			err := controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{
				{VID: 10, VNI: 100},
				{VID: 10, VNI: 200},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("duplicate VID"))
		})

		It("rejects duplicate VNIs", func() {
			err := controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{
				{VID: 10, VNI: 100},
				{VID: 20, VNI: 100},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("duplicate VNI"))
		})

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
					Alias: ManagedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}
			nlMock.On("LinkByName", "br0").Return(stableBridge, nil)

			for range 10 {
				Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			}

			nlMock.AssertNotCalled(GinkgoT(), "LinkModify", mock.Anything)
			nlMock.AssertNotCalled(GinkgoT(), "LinkSetUp", mock.Anything)
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
					Alias: ManagedAliasPrefix + "vxlan:vxlan0", Flags: net.FlagUp,
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
					Alias: ManagedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkList").Return([]netlink.Link{bridgeLink}, nil)
			nlMock.On("LinkGetProtinfo", bridgeLink).Return(netlink.Protinfo{}, nil)

			for range 20 {
				controller.handleLinkUpdate(bridgeLink)
				Expect(controller.reconcileSyncKey()).To(Succeed())
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
			Expect(controller.Has("br0")).To(BeTrue())
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
			Expect(controller.GetConfig("br0").Master).To(Equal("vrf0"))

			// Update config
			cfg2 := DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0"},
				},
				Master: "vrf1",
			}

			Expect(controller.EnsureLink(cfg2)).To(Succeed())

			Expect(controller.GetConfig("br0").Master).To(Equal("vrf1"))
		})

		It("returns nil config for non-existent device", func() {
			Expect(controller.GetConfig("nonexistent")).To(BeNil())
		})
	})

	Describe("DeleteLink", func() {
		It("succeeds for non-existent device", func() {
			Expect(controller.DeleteLink("nonexistent")).To(Succeed())
		})

		It("cleans up mappingStore when deleting a VXLAN device", func() {
			cfg := DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())
			Expect(controller.EnsureBridgeMappings("br0", "vxlan0",
				[]VIDVNIMapping{{VID: 10, VNI: 100}})).To(Succeed())

			Expect(controller.DeleteLink("vxlan0")).To(Succeed())
			Expect(controller.Has("vxlan0")).To(BeFalse())
		})

	})

	Describe("configsEqual", func() {
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

			Expect(configsEqual(cfg1, cfg2)).To(BeTrue())
		})

		DescribeTable("field comparison",
			func(cfg1, cfg2 *DeviceConfig, expected bool) {
				Expect(configsEqual(cfg1, cfg2)).To(Equal(expected))
				Expect(configsEqual(cfg2, cfg1)).To(Equal(expected), "configsEqual must be symmetric")
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
			Entry("TxQLen set vs unset",
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", TxQLen: 1000}}},
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
				false),
			Entry("HardwareAddr set vs unset",
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0",
					HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}}},
				&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
				false),
		)

	})

	DescribeTable("diffMappings",
		func(current, desired, expectedAdd, expectedRemove []VIDVNIMapping) {
			toAdd, toRemove := diffMappings(current, desired)
			if len(expectedAdd) == 0 {
				Expect(toAdd).To(BeEmpty())
			} else {
				Expect(toAdd).To(ConsistOf(expectedAdd))
			}
			if len(expectedRemove) == 0 {
				Expect(toRemove).To(BeEmpty())
			} else {
				Expect(toRemove).To(ConsistOf(expectedRemove))
			}
		},
		Entry("equal mappings",
			[]VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}},
			[]VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}},
			[]VIDVNIMapping{}, []VIDVNIMapping{}),
		Entry("new mappings to add",
			[]VIDVNIMapping{{VID: 10, VNI: 100}},
			[]VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}},
			[]VIDVNIMapping{{VID: 20, VNI: 200}}, []VIDVNIMapping{}),
		Entry("stale mappings to remove",
			[]VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}},
			[]VIDVNIMapping{{VID: 10, VNI: 100}},
			[]VIDVNIMapping{}, []VIDVNIMapping{{VID: 20, VNI: 200}}),
		Entry("empty current",
			[]VIDVNIMapping{},
			[]VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}},
			[]VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}}, []VIDVNIMapping{}),
		Entry("empty desired",
			[]VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}},
			[]VIDVNIMapping{},
			[]VIDVNIMapping{}, []VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}}),
	)

	DescribeTable("isOurDevice ownership check",
		func(link netlink.Link, expected bool) {
			Expect(isOurDevice(link)).To(Equal(expected))
		},
		Entry("our alias prefix (bridge)",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Alias: "ovn-k8s-ndm:bridge:br0"}}, true),
		Entry("our alias prefix (vxlan)",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Alias: "ovn-k8s-ndm:vxlan:vxlan0"}}, true),
		Entry("empty alias",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Alias: ""}}, false),
		Entry("foreign alias",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Alias: "external-system:some-device"}}, false),
		Entry("partial prefix",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Alias: "ovn-k8s:bridge:br0"}}, false),
	)

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
			// encounters missing master -> DependencyError -> state = Pending
			nlMock.On("LinkByName", "vrf0").Return(nil, linkNotFoundErr).Once() // resolveDependencies: master missing
			nlMock.On("IsLinkNotFoundError", mock.Anything).Return(true)

			Expect(controller.EnsureLink(cfg)).To(Succeed())
			Expect(controller.reconcileDeviceKey("svi0")).To(Succeed())
			Expect(controller.GetDeviceState("svi0")).To(Equal(DeviceStatePending),
				"device should be pending due to missing master")

			// Phase 2: Master appears -> handleLinkUpdate enqueues, then reconcileDeviceKey succeeds
			nlMock.On("LinkByName", "vrf0").Return(vrfLink, nil)                // resolveDependencies: master exists
			nlMock.On("LinkByName", "svi0").Return(nil, linkNotFoundErr).Once() // device doesn't exist
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil).Once()
			nlMock.On("LinkByName", "svi0").Return(bridgeLink, nil).Once() // createLink: fetch created link
			nlMock.On("LinkSetAlias", bridgeLink, "ovn-k8s-ndm:bridge:svi0").Return(nil).Once()
			nlMock.On("LinkSetMaster", bridgeLink, vrfLink).Return(nil).Once()
			nlMock.On("LinkByName", "svi0").Return(bridgeLink, nil).Once() // ensureDeviceUp: re-fetch link

			controller.handleLinkUpdate(vrfLink)
			Expect(controller.reconcileDeviceKey("svi0")).To(Succeed())

			Expect(controller.GetDeviceState("svi0")).To(Equal(DeviceStateReady),
				"state should be Ready after successful retry")
		})

		It("does not retry non-pending devices for unrelated links", func() {
			cfg := DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())
			controller.store["br0"].state = DeviceStateReady

			// Simulate unrelated link update
			dummyLink := &netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{Name: "dummy0"},
			}
			controller.handleLinkUpdate(dummyLink)

			// Device should still be Ready
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateReady))
		})

		It("enqueues VXLAN mappings when parent bridge receives update", func() {
			Expect(controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{
				{VID: 10, VNI: 100},
			})).To(Succeed())

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}

			// handleLinkUpdate should traverse vidVNIMappingStore and find
			// vxlan0's mappings reference br0, enqueuing mapping reconciliation.
			controller.handleLinkUpdate(bridgeLink)

			// Verify enqueue had effect: manually reconcile and check mappings are applied.
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return(nil, nil)
			nlMock.On("BridgeVlanAdd", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
			nlMock.On("BridgeVniAdd", mock.Anything, mock.Anything).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

			Expect(controller.reconcileMappingsKey("vxlan0")).To(Succeed())
			nlMock.AssertCalled(GinkgoT(), "BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false)
		})
	})

	DescribeTable("hasCriticalMismatch",
		func(existing netlink.Link, cfg *DeviceConfig, expected bool) {
			Expect(hasCriticalMismatch(existing, cfg)).To(Equal(expected))
		},
		// VRF
		Entry("VRF table ID mismatch",
			&netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 100},
			&DeviceConfig{Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 200}},
			true),
		Entry("VRF matching table ID",
			&netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 100},
			&DeviceConfig{Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 100}},
			false),
		// VXLAN basic
		Entry("VXLAN VNI mismatch",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 200}},
			true),
		Entry("VXLAN src addr mismatch",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, SrcAddr: net.ParseIP("10.0.0.1")},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, SrcAddr: net.ParseIP("10.0.0.2")}},
			true),
		Entry("VXLAN port mismatch",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, Port: 4789},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, Port: 4790}},
			true),
		Entry("VXLAN matching critical attrs",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, SrcAddr: net.ParseIP("10.0.0.1"), Port: 4789},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, SrcAddr: net.ParseIP("10.0.0.1"), Port: 4789}},
			false),
		// VXLAN EVPN (FlowBased / VniFilter)
		Entry("VXLAN FlowBased true-to-false downgrade",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 0, FlowBased: true},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 0, FlowBased: false}},
			true),
		Entry("VXLAN FlowBased false-to-true upgrade",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, FlowBased: false},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100, FlowBased: true}},
			true),
		Entry("VXLAN VniFilter false-to-true",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 0, FlowBased: true, VniFilter: false},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 0, FlowBased: true, VniFilter: true}},
			true),
		Entry("VXLAN matching external with vnifilter",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 0, FlowBased: true, VniFilter: true, SrcAddr: net.ParseIP("10.0.0.1"), Port: 4789},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 0, FlowBased: true, VniFilter: true, SrcAddr: net.ParseIP("10.0.0.1"), Port: 4789}},
			false),
		// VLAN
		Entry("VLAN ID mismatch",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}, VlanId: 100},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}, VlanId: 200}},
			true),
		Entry("VLAN protocol mismatch",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10"}, VlanId: 10, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10"}, VlanId: 10, VlanProtocol: netlink.VLAN_PROTOCOL_8021AD}},
			true),
		Entry("VLAN parent index mismatch",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5}, VlanId: 10},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 6}, VlanId: 10}},
			true),
		Entry("VLAN matching configuration",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5}, VlanId: 10, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5}, VlanId: 10, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q}},
			false),
		Entry("VLAN HardwareAddr mismatch",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5,
				HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}, VlanId: 10},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5,
				HardwareAddr: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}, VlanId: 10}},
			true),
		Entry("VLAN nil HardwareAddr in desired (not critical)",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5,
				HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}, VlanId: 10},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5}, VlanId: 10}},
			false),
		Entry("VLAN matching HardwareAddr",
			&netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5,
				HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}, VlanId: 10},
			&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.10", ParentIndex: 5,
				HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}, VlanId: 10}},
			false),
		// Bridge (SVD)
		Entry("bridge VlanDefaultPVID mismatch",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](1)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](0)}},
			true),
		Entry("bridge nil VlanDefaultPVID in desired",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](1)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: nil}},
			false),
		Entry("bridge matching VlanDefaultPVID",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](0)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](0)}},
			false),
		Entry("bridge VlanFiltering mismatch",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(false)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)}},
			true),
		Entry("bridge nil VlanFiltering in desired (not critical)",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: nil}},
			false),
		Entry("bridge matching VlanFiltering",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)}},
			false),
		// Generic
		Entry("type mismatch",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}}},
			true),
		Entry("nil config link",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			&DeviceConfig{Link: nil},
			false),
	)

	DescribeTable("needsLinkModify",
		func(current netlink.Link, cfg *DeviceConfig, expected bool) {
			Expect(needsLinkModify(current, cfg)).To(Equal(expected))
		},
		Entry("all attributes match",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1, Alias: "ovn-k8s-ndm:bridge:br0", MTU: 1500}},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 1500}}},
			false),
		Entry("VXLAN attributes match",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 1, Alias: "ovn-k8s-ndm:vxlan:vxlan0"}, Learning: false},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false}},
			false),
		Entry("alias differs",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1, Alias: ""}},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
			true),
		Entry("MTU differs",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1, Alias: "ovn-k8s-ndm:bridge:br0", MTU: 1500}},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 9000}}},
			true),
		Entry("TxQLen differs",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1, Alias: "ovn-k8s-ndm:bridge:br0", TxQLen: 500}},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", TxQLen: 1000}}},
			true),
		Entry("HardwareAddr differs",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1, Alias: "ovn-k8s-ndm:bridge:br0",
				HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0",
				HardwareAddr: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}}},
			true),
		Entry("VXLAN Learning differs",
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 1, Alias: "ovn-k8s-ndm:vxlan:vxlan0"}, Learning: true},
			&DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false}},
			true),
		Entry("Bridge VlanFiltering differs",
			&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1, Alias: "ovn-k8s-ndm:bridge:br0"}, VlanFiltering: ptr.To(false)},
			&DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)}},
			true),
	)

	Describe("ApplyBridgePortVLAN", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("adds VLAN with pvid and untagged flags", func() {
			link := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "port0", Index: 1}}
			vlan := BridgePortVLAN{VID: 10, PVID: true, Untagged: true}

			nlMock.On("LinkByName", "port0").Return(link, nil)
			// BridgeVlanAdd(link, vid, pvid, untagged, self, master)
			nlMock.On("BridgeVlanAdd", link, uint16(10), true, true, false, true).Return(nil)

			Expect(ApplyBridgePortVLAN("port0", vlan)).To(Succeed())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("adds VLAN without pvid flag", func() {
			link := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "port0", Index: 1}}
			vlan := BridgePortVLAN{VID: 20, PVID: false, Untagged: false}

			nlMock.On("LinkByName", "port0").Return(link, nil)
			nlMock.On("BridgeVlanAdd", link, uint16(20), false, false, false, true).Return(nil)

			Expect(ApplyBridgePortVLAN("port0", vlan)).To(Succeed())
		})

		It("succeeds when VLAN already exists (idempotent)", func() {
			link := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "port0", Index: 1}}
			vlan := BridgePortVLAN{VID: 10, PVID: true, Untagged: true}

			alreadyExistsErr := errors.New("already exists")
			nlMock.On("LinkByName", "port0").Return(link, nil)
			nlMock.On("BridgeVlanAdd", link, uint16(10), true, true, false, true).Return(alreadyExistsErr)
			nlMock.On("IsAlreadyExistsError", alreadyExistsErr).Return(true)

			Expect(ApplyBridgePortVLAN("port0", vlan)).To(Succeed())
		})

		It("fails when device not found", func() {
			vlan := BridgePortVLAN{VID: 10}

			nlMock.On("LinkByName", "port0").Return(nil, errors.New("link not found"))

			err := ApplyBridgePortVLAN("port0", vlan)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get link"))
		})

		It("fails on non-already-exists error", func() {
			link := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "port0", Index: 1}}
			vlan := BridgePortVLAN{VID: 10}

			permissionErr := errors.New("permission denied")
			nlMock.On("LinkByName", "port0").Return(link, nil)
			nlMock.On("BridgeVlanAdd", link, uint16(10), false, false, false, true).Return(permissionErr)
			nlMock.On("IsAlreadyExistsError", permissionErr).Return(false)

			err := ApplyBridgePortVLAN("port0", vlan)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to add VLAN"))
		})
	})

	DescribeTable("addressesEqual",
		func(a, b []netlink.Addr, expected bool) {
			Expect(addressesEqual(a, b)).To(Equal(expected))
		},
		Entry("both nil", nil, nil, true),
		// nil means "no address management", empty means "manage but want none"
		Entry("nil vs empty", nil, []netlink.Addr{}, false),
		Entry("empty vs nil", []netlink.Addr{}, nil, false),
		Entry("both empty", []netlink.Addr{}, []netlink.Addr{}, true),
		Entry("same addresses",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			true),
		Entry("different addresses",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.2/32")}},
			false),
		Entry("different lengths",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}, {IPNet: mustParseIPNet("10.0.0.2/32")}},
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			false),
		Entry("ignores order",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}, {IPNet: mustParseIPNet("10.0.0.2/32")}},
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.2/32")}, {IPNet: mustParseIPNet("10.0.0.1/32")}},
			true),
		Entry("same IP different prefix length",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/24")}},
			false),
		Entry("compares by IPNet string, ignoring other fields",
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32"), Flags: 0}},
			[]netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32"), Flags: 128}},
			true),
	)

	DescribeTable("isLinkLocalAddress",
		func(ip net.IP, expected bool) {
			Expect(isLinkLocalAddress(ip)).To(Equal(expected))
		},
		Entry("IPv6 link-local", net.ParseIP("fe80::1"), true),
		Entry("IPv4 link-local (169.254.x.x)", net.ParseIP("169.254.1.1"), true),
		Entry("regular IPv6", net.ParseIP("2001:db8::1"), false),
		Entry("regular IPv4", net.ParseIP("10.0.0.1"), false),
		Entry("nil", nil, false),
	)

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

			stored := controller.GetConfig("d0").Addresses
			Expect(stored).To(HaveLen(1))
			Expect(stored[0].IP.String()).To(Equal("10.0.0.1"))
		})
	})

	Describe("ListDevicesByVLANParent", func() {
		It("returns only devices with matching VLANParent", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:       &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.100"}, VlanId: 100},
				VLANParent: "br0",
			})).To(Succeed())
			Expect(controller.EnsureLink(DeviceConfig{
				Link:       &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.200"}, VlanId: 200},
				VLANParent: "br0",
			})).To(Succeed())
			Expect(controller.EnsureLink(DeviceConfig{
				Link:       &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br1.100"}, VlanId: 100},
				VLANParent: "br1",
			})).To(Succeed())
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			result := controller.ListDevicesByVLANParent("br0")
			Expect(result).To(HaveLen(2))

			names := []string{result[0].Link.Attrs().Name, result[1].Link.Attrs().Name}
			Expect(names).To(ContainElements("br0.100", "br0.200"))
		})

		It("returns empty for no matches", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())
			result := controller.ListDevicesByVLANParent("nonexistent")
			Expect(result).To(BeEmpty())
		})
	})

	Describe("EnsureLink name validation edge cases", func() {
		It("accepts exactly 15-character name", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "exactly15chars_"}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.Has("exactly15chars_")).To(BeTrue())
		})

		It("rejects 16-character name", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link: &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "exactly16chars__"}},
			})
			Expect(err).To(HaveOccurred())
			Expect(controller.Has("exactly16chars__")).To(BeFalse())
		})

		It("accepts 15-character master name", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}},
				Master: "exactly15chars_",
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects 16-character master name", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}},
				Master: "exactly16chars__",
			})
			Expect(err).To(HaveOccurred())
			Expect(controller.Has("dev0")).To(BeFalse())
		})

		It("accepts 15-character VLANParent name", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link:       &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "v0"}, VlanId: 10},
				VLANParent: "exactly15chars_",
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects 16-character VLANParent name", func() {
			err := controller.EnsureLink(DeviceConfig{
				Link:       &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "v0"}, VlanId: 10},
				VLANParent: "exactly16chars__",
			})
			Expect(err).To(HaveOccurred())
			Expect(controller.Has("v0")).To(BeFalse())
		})
	})

	Describe("IsDeviceReady", func() {
		It("returns true for Ready device", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			controller.store["br0"].state = DeviceStateReady
			Expect(controller.IsDeviceReady("br0")).To(BeTrue())
		})

		It("returns false for Pending device", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			Expect(controller.IsDeviceReady("br0")).To(BeFalse())
		})

		It("returns false for non-existent device", func() {
			Expect(controller.IsDeviceReady("nonexistent")).To(BeFalse())
		})
	})

	Describe("IsNotOwnedError", func() {
		It("returns true for NotOwnedError", func() {
			err := &NotOwnedError{DeviceName: "br0", Reason: "foreign alias"}
			Expect(IsNotOwnedError(err)).To(BeTrue())
		})

		It("returns true for wrapped NotOwnedError", func() {
			inner := &NotOwnedError{DeviceName: "br0", Reason: "foreign alias"}
			wrapped := fmt.Errorf("outer: %w", inner)
			Expect(IsNotOwnedError(wrapped)).To(BeTrue())
		})

		It("returns false for other errors", func() {
			Expect(IsNotOwnedError(errors.New("some error"))).To(BeFalse())
		})

		It("returns false for nil", func() {
			Expect(IsNotOwnedError(nil)).To(BeFalse())
		})
	})

	Describe("reconcileFullSyncKey", func() {
		It("continues sync even when orphan cleanup fails", func() {
			nlMock.On("LinkList").Return(nil, errors.New("netlink error"))

			Expect(controller.reconcileFullSyncKey()).To(Succeed())
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

// mockReconciler is a test helper for subscriber notifications
type mockReconciler struct {
	fn func(key string) error
}

func (r *mockReconciler) ReconcileDevice(key string) error {
	if r.fn != nil {
		return r.fn(key)
	}
	return nil
}
