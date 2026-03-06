package netlinkdevicemanager

import (
	"errors"
	"fmt"
	"net"
	"reflect"

	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
	nl "github.com/vishvananda/netlink/nl"

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
			// createLink: LinkAdd + re-fetch + set alias
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vxlan")).Return(nil)
			nlMock.On("LinkByName", "vxlan0").Return(createdVxlan, nil)
			nlMock.On("LinkSetAlias", createdVxlan, managedAliasPrefix+"vxlan:vxlan0").Return(nil)
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
			nlMock.On("LinkSetAlias", createdDummy, managedAliasPrefix+"dummy:dummy0").Return(nil)
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
			// createLink: LinkAdd + re-fetch + set alias
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vlan")).Return(nil)
			nlMock.On("LinkByName", "vlan100").Return(createdVlan, nil)
			nlMock.On("LinkSetAlias", createdVlan, managedAliasPrefix+"vlan:vlan100").Return(nil)
			// ensureDeviceUp
			nlMock.On("LinkSetUp", createdVlan).Return(nil)

			Expect(controller.reconcileDeviceKey("vlan100")).To(Succeed())
			Expect(controller.GetDeviceState("vlan100")).To(Equal(DeviceStateReady))
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

			// resolveDependencies: new master exists
			nlMock.On("LinkByName", "br-new").Return(newBridge, nil)
			// applyDeviceConfig: device exists with our alias
			nlMock.On("LinkByName", "vxlan0").Return(existingVxlan, nil)
			// updateDevice: needsLinkModify -> false (alias matches, mutable fields match)
			// Master changed: MasterIndex(5) != newBridge.Index(20)
			nlMock.On("LinkSetDown", existingVxlan).Return(nil)
			nlMock.On("LinkSetMaster", existingVxlan, newBridge).Return(nil)
			// Bridge port settings: kernel defaults differ from desired -> apply
			nlMock.On("LinkGetProtinfo", existingVxlan).Return(netlink.Protinfo{Learning: true}, nil)
			nlMock.On("LinkSetVlanTunnel", existingVxlan, true).Return(nil)
			nlMock.On("LinkSetBrNeighSuppress", existingVxlan, true).Return(nil)
			nlMock.On("LinkSetLearning", existingVxlan, false).Return(nil)
			// ensureDeviceUp: idempotent, always called
			nlMock.On("LinkSetUp", existingVxlan).Return(nil)

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
					Alias:       managedAliasPrefix + "bridge:br0",
					Flags:       net.FlagUp,
				},
			}

			nlMock.On("LinkByName", "br0").Return(existingBridge, nil)
			nlMock.On("LinkSetNoMaster", existingBridge).Return(nil)
			nlMock.On("LinkSetUp", existingBridge).Return(nil)

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
					Alias: managedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}

			nlMock.On("LinkByName", "br0").Return(existingBridge, nil)
			// needsLinkModify -> true (MTU differs)
			nlMock.On("LinkModify", mock.AnythingOfType("*netlink.Bridge")).Return(nil)
			nlMock.On("LinkSetUp", existingBridge).Return(nil)

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
					Alias:       managedAliasPrefix + "vxlan:vxlan0",
					Flags:       net.FlagUp,
				},
				FlowBased: true,
				VniFilter: true,
			}

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(existingVxlan, nil)
			// getBridgePortSettings returns matching settings -> skip apply
			nlMock.On("LinkGetProtinfo", existingVxlan).Return(netlink.Protinfo{
				VlanTunnel:    true,
				NeighSuppress: true,
				Learning:      false,
			}, nil)
			nlMock.On("LinkSetUp", existingVxlan).Return(nil)

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
				Alias: managedAliasPrefix + "bridge:br0",
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
			nlMock.On("LinkSetAlias", createdBridge, managedAliasPrefix+"bridge:br0").Return(nil)
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
					Alias:       managedAliasPrefix + "vxlan:vxlan0",
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

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
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

			Expect(syncVIDVNIMappings(vxlanLink, cfg)).To(Succeed())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("skips when VIDVNIMappings is nil", func() {
			cfg := &DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master: "br0",
			}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}
			Expect(syncVIDVNIMappings(vxlanLink, cfg)).To(Succeed())
		})

		It("skips fully present mappings — no adds when all four components exist", func() {
			cfg := &DeviceConfig{
				Link:           &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:         "br0",
				VIDVNIMappings: []VIDVNIMapping{{VID: 10, VNI: 100}},
			}

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 10}}

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
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

			Expect(syncVIDVNIMappings(vxlanLink, cfg)).To(Succeed())

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

				nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
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

				Expect(syncVIDVNIMappings(vxlanLink, cfg)).To(Succeed())

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
			nlMock.On("LinkSetAlias", createdBridge, managedAliasPrefix+"bridge:br0").Return(nil)
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

		It("does NOT notify when state stays Ready and kernel state is unchanged", func() {
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
					Alias: managedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}
			nlMock.On("LinkByName", "br0").Return(brLink, nil)
			nlMock.On("LinkSetUp", brLink).Return(nil)

			// First reconcile: Pending -> Ready (notified)
			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(notifyCount).To(Equal(1))
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateReady))

			// Second reconcile: Ready -> Ready, no kernel change (should NOT notify)
			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(notifyCount).To(Equal(1)) // Still 1, not 2
		})

		It("notifies when state stays Ready but kernel state was modified (self-heal)", func() {
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

			// First reconcile: device exists, Pending -> Ready
			brLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 10,
					Alias: managedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}
			nlMock.On("LinkByName", "br0").Return(brLink, nil).Once()
			nlMock.On("LinkSetUp", brLink).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(notifyCount).To(Equal(1))
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateReady))

			// Second reconcile: device was deleted externally, NDM recreates it.
			// State stays Ready but kernel state was modified — subscriber must be notified
			// so that components depending on the device (e.g., OVS port attached to this
			// bridge) can re-apply their side-configuration.
			linkNotFoundErr := errors.New("link not found")
			recreatedBridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 20}}

			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr).Once()
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil)
			nlMock.On("LinkByName", "br0").Return(recreatedBridge, nil)
			nlMock.On("LinkSetAlias", recreatedBridge, managedAliasPrefix+"bridge:br0").Return(nil)
			nlMock.On("LinkSetUp", recreatedBridge).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(notifyCount).To(Equal(2)) // Notified again despite Ready -> Ready
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateReady))
		})

		It("notifies when state stays Ready but master was re-attached (attribute drift)", func() {
			notifyCount := 0
			controller.RegisterDeviceReconciler(&mockReconciler{
				fn: func(_ string) error {
					notifyCount++
					return nil
				},
			})

			Expect(controller.EnsureLink(DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Master: "vrf0",
			})).To(Succeed())

			vrfLink := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0", Index: 20}}
			nlMock.On("LinkByName", "vrf0").Return(vrfLink, nil)

			// First reconcile: device exists with correct master. Pending -> Ready.
			brLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:        "br0",
					Index:       10,
					MasterIndex: 20,
					Alias:       managedAliasPrefix + "bridge:br0",
					Flags:       net.FlagUp,
				},
			}
			nlMock.On("LinkByName", "br0").Return(brLink, nil).Once()
			nlMock.On("LinkSetUp", brLink).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(notifyCount).To(Equal(1))

			// Second reconcile: master was externally detached (MasterIndex=0).
			// updateDevice re-attaches it — modified=true, state stays Ready.
			brLinkDetached := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:        "br0",
					Index:       10,
					MasterIndex: 0,
					Alias:       managedAliasPrefix + "bridge:br0",
					Flags:       net.FlagUp,
				},
			}
			nlMock.On("LinkByName", "br0").Return(brLinkDetached, nil).Once()
			nlMock.On("LinkSetDown", brLinkDetached).Return(nil)
			nlMock.On("LinkSetMaster", brLinkDetached, vrfLink).Return(nil)
			nlMock.On("LinkSetUp", brLinkDetached).Return(nil)

			Expect(controller.reconcileDeviceKey("br0")).To(Succeed())
			Expect(notifyCount).To(Equal(2)) // Notified: master re-attached
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
				Alias: managedAliasPrefix + "bridge:br0",
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
			nlMock.On("LinkSetAlias", createdBridge, managedAliasPrefix+"bridge:br0").Return(nil)
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
				nlMock.On("LinkSetAlias", createdVxlan, managedAliasPrefix+"vxlan:vxlan0").Return(nil)
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
					Alias: managedAliasPrefix + "bridge:br0",
					Flags: net.FlagUp,
				},
			}

			nlMock.On("LinkByName", "br0").Return(existingBridge, nil)
			nlMock.On("LinkModify", mock.AnythingOfType("*netlink.Bridge")).Return(errors.New("permission denied"))

			err := controller.reconcileDeviceKey("br0")
			Expect(err).To(HaveOccurred())
			Expect(controller.GetDeviceState("br0")).To(Equal(DeviceStateFailed))
		})

		It("fails when getBridgePortSettings returns error", func() {
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

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(existingVxlan, nil)
			nlMock.On("LinkGetProtinfo", existingVxlan).Return(netlink.Protinfo{}, errors.New("no bridge port info"))

			err := controller.reconcileDeviceKey("vxlan0")
			Expect(err).To(HaveOccurred())
			Expect(controller.GetDeviceState("vxlan0")).To(Equal(DeviceStateFailed))
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

		It("fails when VLAN has no VLANParent", func() {
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
					Alias: managedAliasPrefix + "dummy:dummy0",
					Flags: net.FlagUp,
				},
			}
			permErr := errors.New("permission denied")

			nlMock.On("LinkByName", "dummy0").Return(existingDummy, nil)
			nlMock.On("LinkSetUp", existingDummy).Return(nil)
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
					Alias: managedAliasPrefix + "dummy:dummy0",
					Flags: net.FlagUp,
				},
			}
			extraAddr := netlink.Addr{IPNet: mustParseIPNetWithIP("192.168.1.1/24")}
			permErr := errors.New("permission denied")

			nlMock.On("LinkByName", "dummy0").Return(existingDummy, nil)
			nlMock.On("LinkSetUp", existingDummy).Return(nil)
			nlMock.On("AddrList", existingDummy, netlink.FAMILY_ALL).Return([]netlink.Addr{extraAddr}, nil)
			nlMock.On("AddrDel", existingDummy, mock.Anything).Return(permErr)
			nlMock.On("IsEntryNotFoundError", permErr).Return(false)

			err := controller.reconcileDeviceKey("dummy0")
			Expect(err).To(HaveOccurred())
			Expect(controller.GetDeviceState("dummy0")).To(Equal(DeviceStateFailed))
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

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{}, nil)
			nlMock.On("BridgeVlanList").Return(map[int32][]*nl.BridgeVlanInfo{}, nil)
			nlMock.On("BridgeVniList").Return(map[int32][]*nl.BridgeVniInfo{}, nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false).Return(errors.New("ENOMEM"))
			nlMock.On("IsAlreadyExistsError", mock.Anything).Return(false)

			err := syncVIDVNIMappings(vxlanLink, cfg)
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

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
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

			Expect(syncVIDVNIMappings(vxlanLink, cfg)).To(Succeed())
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

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
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

			err := syncVIDVNIMappings(vxlanLink, cfg)
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

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
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

			err := syncVIDVNIMappings(vxlanLink, cfg)
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

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("BridgeVlanTunnelShowDev", vxlanLink).Return([]nl.TunnelInfo{}, nil)
			nlMock.On("BridgeVlanList").Return(map[int32][]*nl.BridgeVlanInfo{}, nil)
			nlMock.On("BridgeVniList").Return(map[int32][]*nl.BridgeVniInfo{}, nil)

			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(10), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(100)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(10), uint32(100), false, true).Return(nil)

			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(20), false, false, true, false).Return(errors.New("ENOMEM"))
			nlMock.On("IsAlreadyExistsError", mock.Anything).Return(false)

			err := syncVIDVNIMappings(vxlanLink, cfg)
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
					Expect(controller.Has("dev0")).To(BeFalse())
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

		It("removes device from store", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())
			Expect(controller.Has("br0")).To(BeTrue())

			Expect(controller.DeleteLink("br0")).To(Succeed())
			Expect(controller.Has("br0")).To(BeFalse())
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
			Entry("VLAN ParentIndex differs (ignored, resolved at apply time)",
				&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0", ParentIndex: 5}, VlanId: 10}, VLANParent: "br0"},
				&DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0", ParentIndex: 6}, VlanId: 10}, VLANParent: "br0"},
				true),
		)
	})

	Describe("EnsureLink generation counter", func() {
		It("increments generation on config change", func() {
			Expect(controller.EnsureLink(DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}},
				Addresses: nil,
			})).To(Succeed())
			gen1 := controller.store["d0"].generation

			Expect(controller.EnsureLink(DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "d0"}},
				Addresses: []netlink.Addr{},
			})).To(Succeed())
			gen2 := controller.store["d0"].generation
			Expect(gen2).To(BeNumerically(">", gen1))
		})

		It("does not increment generation on identical config", func() {
			cfg := DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   100,
					SrcAddr:   net.ParseIP("10.0.0.1"),
				},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())
			gen1 := controller.store["vxlan0"].generation

			Expect(controller.EnsureLink(cfg)).To(Succeed())
			gen2 := controller.store["vxlan0"].generation
			Expect(gen2).To(Equal(gen1))
		})
	})

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
			nlMock.On("LinkSetAlias", bridgeLink, managedAliasPrefix+"bridge:svi0").Return(nil).Once()
			nlMock.On("LinkSetMaster", bridgeLink, vrfLink).Return(nil).Once()
			nlMock.On("LinkSetUp", bridgeLink).Return(nil).Once()

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

	})

	Describe("handleAddrUpdate", func() {
		It("enqueues device when addr update matches a tracked ifindex", func() {
			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler

			controller.store["br0"] = &managedDevice{
				cfg:     DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
				state:   DeviceStateReady,
				ifindex: 10,
			}

			controller.handleAddrUpdate(netlink.AddrUpdate{LinkIndex: 10})

			Expect(fakeReconciler.Reconciles).To(ContainElement("Reconcile:device/br0"))
		})

		It("ignores addr update for untracked ifindex", func() {
			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler

			controller.store["br0"] = &managedDevice{
				cfg:     DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
				state:   DeviceStateReady,
				ifindex: 10,
			}

			controller.handleAddrUpdate(netlink.AddrUpdate{LinkIndex: 99})

			Expect(fakeReconciler.Reconciles).To(BeEmpty())
		})

		It("ignores addr update when ifindex is not yet populated", func() {
			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler

			controller.store["br0"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}}},
				state: DeviceStatePending,
			}

			controller.handleAddrUpdate(netlink.AddrUpdate{LinkIndex: 10})

			Expect(fakeReconciler.Reconciles).To(BeEmpty())
		})
	})

	Describe("reconcileSyncKey", func() {
		It("only enqueues VXLAN devices", func() {
			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler

			bridgeAttrs := netlink.NewLinkAttrs()
			bridgeAttrs.Name = "br0"
			controller.store["br0"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Bridge{LinkAttrs: bridgeAttrs}},
				state: DeviceStateReady,
			}

			vxlanAttrs := netlink.NewLinkAttrs()
			vxlanAttrs.Name = "vxlan0"
			controller.store["vxlan0"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: vxlanAttrs}},
				state: DeviceStateReady,
			}

			vlanAttrs := netlink.NewLinkAttrs()
			vlanAttrs.Name = "vlan100"
			controller.store["vlan100"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Vlan{LinkAttrs: vlanAttrs}},
				state: DeviceStateReady,
			}

			vrfAttrs := netlink.NewLinkAttrs()
			vrfAttrs.Name = "vrf0"
			controller.store["vrf0"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Vrf{LinkAttrs: vrfAttrs}},
				state: DeviceStateReady,
			}

			dummyAttrs := netlink.NewLinkAttrs()
			dummyAttrs.Name = "dummy0"
			controller.store["dummy0"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Dummy{LinkAttrs: dummyAttrs}},
				state: DeviceStateReady,
			}

			Expect(controller.reconcileSyncKey()).To(Succeed())

			Expect(fakeReconciler.Reconciles).To(ConsistOf("After:device/vxlan0"))
		})

		It("enqueues multiple VXLAN devices", func() {
			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler

			vxlan1Attrs := netlink.NewLinkAttrs()
			vxlan1Attrs.Name = "vxlan0"
			controller.store["vxlan0"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: vxlan1Attrs}},
				state: DeviceStateReady,
			}

			vxlan2Attrs := netlink.NewLinkAttrs()
			vxlan2Attrs.Name = "vxlan1"
			controller.store["vxlan1"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: vxlan2Attrs}},
				state: DeviceStateReady,
			}

			bridgeAttrs := netlink.NewLinkAttrs()
			bridgeAttrs.Name = "br0"
			controller.store["br0"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Bridge{LinkAttrs: bridgeAttrs}},
				state: DeviceStateReady,
			}

			Expect(controller.reconcileSyncKey()).To(Succeed())

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
				cfg:   DeviceConfig{Link: &netlink.Bridge{LinkAttrs: bridgeAttrs}},
				state: DeviceStateReady,
			}

			Expect(controller.reconcileSyncKey()).To(Succeed())

			Expect(fakeReconciler.Reconciles).To(BeEmpty())
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

	DescribeTable("addressesEqual",
		func(a, b []netlink.Addr, expected bool) {
			Expect(addressesEqual(a, b)).To(Equal(expected))
		},
		Entry("both nil", nil, nil, true),
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

	// Managed field coverage invariants
	//
	// These tests enforce the coupling between linkMutableFieldsMatch, linkImmutableFieldsEqual,
	// hasCriticalMismatch, and prepareLinkForModify. Together these functions must be exhaustive
	// over all type-specific fields NDM manages. Adding a field to one without the others causes
	// a test failure here.
	//
	// configsEqual delegates to linkMutableFieldsEqual and linkImmutableFieldsEqual, so it
	// automatically covers any field added to either. No configsEqual-specific updates needed.

	Describe("managed field exhaustiveness", func() {
		type managedField struct {
			name    string
			current netlink.Link
			desired *DeviceConfig
		}

		allFields := []managedField{
			// Common mutable (LinkAttrs) — tested via Bridge
			{name: "MTU",
				current: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 1500}},
				desired: &DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 9000}}}},
			{name: "TxQLen",
				current: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", TxQLen: 500}},
				desired: &DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", TxQLen: 1000}}}},
			{name: "HardwareAddr",
				current: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0",
					HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}},
				desired: &DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0",
					HardwareAddr: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}}}},

			// VXLAN
			{name: "VXLAN/Learning",
				current: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: true},
				desired: &DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false}}},
			{name: "VXLAN/VxlanId",
				current: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 100},
				desired: &DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VxlanId: 200}}},
			{name: "VXLAN/SrcAddr",
				current: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, SrcAddr: net.ParseIP("10.0.0.1")},
				desired: &DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, SrcAddr: net.ParseIP("10.0.0.2")}}},
			{name: "VXLAN/Port",
				current: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Port: 4789},
				desired: &DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Port: 4790}}},
			{name: "VXLAN/FlowBased",
				current: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, FlowBased: true},
				desired: &DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, FlowBased: false}}},
			{name: "VXLAN/VniFilter",
				current: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VniFilter: true},
				desired: &DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VniFilter: false}}},

			// Bridge
			{name: "Bridge/VlanFiltering",
				current: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(false)},
				desired: &DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: ptr.To(true)}}},
			{name: "Bridge/VlanDefaultPVID",
				current: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](1)},
				desired: &DeviceConfig{Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: ptr.To[uint16](0)}}},

			// VRF
			{name: "VRF/Table",
				current: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 100},
				desired: &DeviceConfig{Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}, Table: 200}}},

			// VLAN
			{name: "VLAN/VlanId",
				current: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0"}, VlanId: 100},
				desired: &DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0"}, VlanId: 200}}},
			{name: "VLAN/VlanProtocol",
				current: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0"}, VlanId: 10, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q},
				desired: &DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0"}, VlanId: 10, VlanProtocol: netlink.VLAN_PROTOCOL_8021AD}}},
			{name: "VLAN/HardwareAddr",
				current: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0", ParentIndex: 5,
					HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}}, VlanId: 10},
				desired: &DeviceConfig{Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan0", ParentIndex: 5,
					HardwareAddr: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}, VlanId: 10}}},
		}

		It("each field is covered by the correct functions", func() {
			for _, f := range allFields {
				isMutable := !linkMutableFieldsMatch(f.current, f.desired.Link)
				isImmutable := !linkImmutableFieldsEqual(f.current, f.desired.Link)
				isCritical := hasCriticalMismatch(f.current, f.desired)

				Expect(isMutable || isCritical).To(BeTrue(),
					"field %s is detected by neither linkMutableFieldsMatch nor hasCriticalMismatch", f.name)

				Expect(isMutable || isImmutable).To(BeTrue(),
					"field %s is detected by neither linkMutableFieldsMatch nor linkImmutableFieldsEqual — configsEqual would miss it", f.name)

				if isMutable {
					result := prepareLinkForModify(f.current, f.desired)
					Expect(linkMutableFieldsMatch(result, f.desired.Link)).To(BeTrue(),
						"prepareLinkForModify does not carry mutable field %s", f.name)
				}
			}
		})
	})

	Describe("upstream field audit", func() {
		// These tests use reflection to detect new fields added to vendored netlink
		// types. When a new field appears, the test fails and the developer must
		// classify it as managed (mutable/immutable) or explicitly ignored.

		vxlanManaged := map[string]bool{
			"VxlanId": true, "SrcAddr": true, "Port": true,
			"FlowBased": true, "VniFilter": true, "Learning": true,
		}
		vxlanIgnored := map[string]string{
			"VtepDevIndex":   "resolved by kernel at creation time",
			"Group":          "multicast group, not used by NDM",
			"TTL":            "not managed by NDM",
			"TOS":            "not managed by NDM",
			"Proxy":          "not managed by NDM",
			"RSC":            "not managed by NDM",
			"L2miss":         "not managed by NDM",
			"L3miss":         "not managed by NDM",
			"UDPCSum":        "not managed by NDM",
			"UDP6ZeroCSumTx": "not managed by NDM",
			"UDP6ZeroCSumRx": "not managed by NDM",
			"NoAge":          "not managed by NDM",
			"GBP":            "not managed by NDM",
			"Age":            "runtime state, not a config knob",
			"Limit":          "not managed by NDM",
			"PortLow":        "not managed by NDM",
			"PortHigh":       "not managed by NDM",
		}

		bridgeManaged := map[string]bool{
			"VlanFiltering": true, "VlanDefaultPVID": true,
		}
		bridgeIgnored := map[string]string{
			"MulticastSnooping": "not managed by NDM",
			"AgeingTime":        "not managed by NDM",
			"HelloTime":         "not managed by NDM",
			"GroupFwdMask":      "not managed by NDM",
		}

		vlanManaged := map[string]bool{
			"VlanId": true, "VlanProtocol": true,
		}
		vlanIgnored := map[string]string{
			"IngressQosMap": "not managed by NDM",
			"EgressQosMap":  "not managed by NDM",
			"ReorderHdr":    "not managed by NDM",
			"Gvrp":          "not managed by NDM",
			"LooseBinding":  "not managed by NDM",
			"Mvrp":          "not managed by NDM",
			"BridgeBinding": "not managed by NDM",
		}

		vrfManaged := map[string]bool{
			"Table": true,
		}

		auditLinkType := func(name string, typ reflect.Type, managed map[string]bool, ignored map[string]string) {
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				if field.Name == "LinkAttrs" {
					continue
				}
				_, isManaged := managed[field.Name]
				_, isIgnored := ignored[field.Name]
				Expect(isManaged || isIgnored).To(BeTrue(),
					"%s field %q is not accounted for — add to managed or ignored with justification", name, field.Name)
			}
		}

		It("all Vxlan fields are accounted for", func() {
			auditLinkType("Vxlan", reflect.TypeFor[netlink.Vxlan](), vxlanManaged, vxlanIgnored)
		})

		It("all Bridge fields are accounted for", func() {
			auditLinkType("Bridge", reflect.TypeFor[netlink.Bridge](), bridgeManaged, bridgeIgnored)
		})

		It("all Vlan fields are accounted for", func() {
			auditLinkType("Vlan", reflect.TypeFor[netlink.Vlan](), vlanManaged, vlanIgnored)
		})

		It("all Vrf fields are accounted for", func() {
			auditLinkType("Vrf", reflect.TypeFor[netlink.Vrf](), vrfManaged, map[string]string{})
		})
	})

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

		It("enqueues all device types, not just VXLAN", func() {
			fakeReconciler := &controllerPkg.FakeController{}
			controller.reconciler = fakeReconciler
			nlMock.On("LinkList").Return([]netlink.Link{}, nil)

			bridgeAttrs := netlink.NewLinkAttrs()
			bridgeAttrs.Name = "br0"
			controller.store["br0"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Bridge{LinkAttrs: bridgeAttrs}},
				state: DeviceStateReady,
			}

			vxlanAttrs := netlink.NewLinkAttrs()
			vxlanAttrs.Name = "vxlan0"
			controller.store["vxlan0"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Vxlan{LinkAttrs: vxlanAttrs}},
				state: DeviceStateReady,
			}

			vlanAttrs := netlink.NewLinkAttrs()
			vlanAttrs.Name = "vlan100"
			controller.store["vlan100"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Vlan{LinkAttrs: vlanAttrs}},
				state: DeviceStateReady,
			}

			dummyAttrs := netlink.NewLinkAttrs()
			dummyAttrs.Name = "dummy0"
			controller.store["dummy0"] = &managedDevice{
				cfg:   DeviceConfig{Link: &netlink.Dummy{LinkAttrs: dummyAttrs}},
				state: DeviceStateReady,
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
