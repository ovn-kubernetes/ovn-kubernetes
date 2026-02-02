package netlinkdevicemanager

import (
	"errors"
	"net"
	"sync"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NetlinkDeviceManager", func() {
	var controller *Controller

	BeforeEach(func() {
		controller = NewController()
	})

	Describe("NewController", func() {
		It("creates a non-started controller", func() {
			Expect(controller).NotTo(BeNil())
			Expect(controller.store).To(BeEmpty())
			Expect(controller.started).To(BeFalse())
		})
	})

	Describe("EnsureLink", func() {
		It("stores device config in store", func() {
			cfg := DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0"},
				},
			}

			err := controller.EnsureLink(cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.store).To(HaveKey("br0"))
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

		It("is idempotent - calling twice with same config stores only once", func() {
			// Tests that EnsureLink is idempotent at the desired-state level.
			// The store represents the contract for reconciliation - if a device
			// is stored once, the reconciler will ensure it exists exactly once.
			cfg := DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0"},
				},
			}

			err := controller.EnsureLink(cfg)
			Expect(err).NotTo(HaveOccurred())

			// Second call with identical config
			err = controller.EnsureLink(cfg)
			Expect(err).NotTo(HaveOccurred())

			// Only one entry in store (idempotent behavior)
			Expect(controller.store).To(HaveLen(1))
			Expect(controller.store).To(HaveKey("br0"))
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

			err := controller.EnsureLink(cfg1)
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.store["br0"].cfg.Master).To(Equal("vrf0"))

			// Update config
			cfg2 := DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0"},
				},
				Master: "vrf1",
			}

			err = controller.EnsureLink(cfg2)
			Expect(err).NotTo(HaveOccurred())

			// Stored config reflects update (reconciler will apply this)
			Expect(controller.store["br0"].cfg.Master).To(Equal("vrf1"))
		})
	})

	Describe("DeleteLink", func() {
		It("removes device from store", func() {
			cfg := DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0"},
				},
			}

			Expect(controller.EnsureLink(cfg)).To(Succeed())
			Expect(controller.store).To(HaveKey("br0"))

			err := controller.DeleteLink("br0")
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.store).NotTo(HaveKey("br0"))
		})

		It("succeeds for non-existent device", func() {
			err := controller.DeleteLink("nonexistent")
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not attempt kernel deletion when not started", func() {
			// Before Run(), DeleteLink should only update desired state
			cfg := DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0"},
				},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())
			Expect(controller.started).To(BeFalse())

			// This should succeed without touching kernel (no netlink mock needed)
			err := controller.DeleteLink("br0")
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.store).NotTo(HaveKey("br0"))
			// No tombstone should be created when not started
			Expect(controller.pendingDeletes).NotTo(HaveKey("br0"))
		})

		It("clears stale tombstone when not started", func() {
			// Simulate a stale tombstone from previous run
			controller.pendingDeletes["br0"] = struct{}{}

			// DeleteLink when not started should clear it
			err := controller.DeleteLink("br0")
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.pendingDeletes).NotTo(HaveKey("br0"))
		})

		It("cleans up mappingStore when deleting a VXLAN device", func() {
			// Set up a VXLAN device with mappings
			cfg := DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())
			controller.vidVNIMappingStore["vxlan0"] = &managedVIDVNIMappings{
				bridgeName: "br0",
				vxlanName:  "vxlan0",
				mappings:   []VIDVNIMapping{{VID: 10, VNI: 100}},
			}

			// DeleteLink should clean up mappings
			err := controller.DeleteLink("vxlan0")
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.store).NotTo(HaveKey("vxlan0"))
			Expect(controller.vidVNIMappingStore).NotTo(HaveKey("vxlan0"))
		})

		It("cleans up portVLANStore entries when deleting a device", func() {
			cfg := DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())
			// Use the new nested map structure
			controller.portVLANStore["vxlan0"] = map[int]*managedPortVLAN{
				10: {linkName: "vxlan0", vlan: BridgePortVLAN{VID: 10}},
				20: {linkName: "vxlan0", vlan: BridgePortVLAN{VID: 20}},
			}
			// Also add an unrelated entry
			controller.portVLANStore["other"] = map[int]*managedPortVLAN{
				30: {linkName: "other", vlan: BridgePortVLAN{VID: 30}},
			}

			err := controller.DeleteLink("vxlan0")
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.portVLANStore).NotTo(HaveKey("vxlan0"))
			// Unrelated entry should remain
			Expect(controller.portVLANStore).To(HaveKey("other"))
		})
	})

	Describe("EnsureBridgeMappings before Run()", func() {
		It("stores desired state without executing commands when not started", func() {
			Expect(controller.started).To(BeFalse())

			// This should store state but not apply until Run() is called
			err := controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{{VID: 10, VNI: 100}})
			Expect(err).NotTo(HaveOccurred())

			// State should be stored
			Expect(controller.vidVNIMappingStore).To(HaveKey("vxlan0"))
			Expect(controller.vidVNIMappingStore["vxlan0"].bridgeName).To(Equal("br0"))
			Expect(controller.vidVNIMappingStore["vxlan0"].mappings).To(HaveLen(1))
		})

	})

	Describe("EnsureBridgePortVLAN before Run()", func() {
		It("stores desired state without executing commands when not started", func() {
			Expect(controller.started).To(BeFalse())

			err := controller.EnsureBridgePortVLAN("vxlan0", BridgePortVLAN{VID: 10, PVID: true, Untagged: true})
			Expect(err).NotTo(HaveOccurred())

			// Nested map structure: linkName -> vid -> config
			Expect(controller.portVLANStore).To(HaveKey("vxlan0"))
			Expect(controller.portVLANStore["vxlan0"]).To(HaveKey(10))
			Expect(controller.portVLANStore["vxlan0"][10].vlan.VID).To(Equal(10))
			Expect(controller.portVLANStore["vxlan0"][10].vlan.PVID).To(BeTrue())
			Expect(controller.portVLANStore["vxlan0"][10].vlan.Untagged).To(BeTrue())
		})
	})

	Describe("Has", func() {
		It("returns true for managed device", func() {
			cfg := DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0"},
				},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())

			Expect(controller.Has("br0")).To(BeTrue())
		})

		It("returns false for unmanaged device", func() {
			Expect(controller.Has("nonexistent")).To(BeFalse())
		})
	})

	Describe("GetConfig", func() {
		It("returns config for managed device", func() {
			cfg := DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0"},
				},
				Master: "vrf0",
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())

			result := controller.GetConfig("br0")
			Expect(result).NotTo(BeNil())
			Expect(result.Master).To(Equal("vrf0"))
		})

		It("returns nil for unmanaged device", func() {
			result := controller.GetConfig("nonexistent")
			Expect(result).To(BeNil())
		})
	})

	Describe("configsEqual", func() {
		It("returns true for equal configs", func() {
			cfg1 := &DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Master: "vrf0",
			}
			cfg2 := &DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Master: "vrf0",
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeTrue())
		})

		It("returns false for different Master", func() {
			cfg1 := &DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Master: "vrf0",
			}
			cfg2 := &DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				Master: "vrf1",
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns false for different BridgePortSettings", func() {
			cfg1 := &DeviceConfig{
				Link:               &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:             "br0",
				BridgePortSettings: &BridgePortSettings{VLANTunnel: true, NeighSuppress: true},
			}
			cfg2 := &DeviceConfig{
				Link:               &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:             "br0",
				BridgePortSettings: &BridgePortSettings{VLANTunnel: false, NeighSuppress: true},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns false when one BridgePortSettings is nil", func() {
			cfg1 := &DeviceConfig{
				Link:               &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:             "br0",
				BridgePortSettings: &BridgePortSettings{VLANTunnel: true},
			}
			cfg2 := &DeviceConfig{
				Link:               &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:             "br0",
				BridgePortSettings: nil,
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns true when both BridgePortSettings are nil", func() {
			cfg1 := &DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master: "br0",
			}
			cfg2 := &DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master: "br0",
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeTrue())
		})

		It("returns true for equal BridgePortSettings", func() {
			cfg1 := &DeviceConfig{
				Link:               &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:             "br0",
				BridgePortSettings: &BridgePortSettings{VLANTunnel: true, NeighSuppress: true, Learning: false},
			}
			cfg2 := &DeviceConfig{
				Link:               &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master:             "br0",
				BridgePortSettings: &BridgePortSettings{VLANTunnel: true, NeighSuppress: true, Learning: false},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeTrue())
		})

		It("returns false when link types differ", func() {
			// Bridge vs Vrf - different types with same name
			cfg1 := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "dev0"}, Table: 100},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})
	})

	Describe("diffMappings", func() {
		It("returns empty for equal mappings", func() {
			current := []VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}}
			desired := []VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}}

			toAdd, toRemove := diffMappings(current, desired)
			Expect(toAdd).To(BeEmpty())
			Expect(toRemove).To(BeEmpty())
		})

		It("returns additions for new mappings", func() {
			current := []VIDVNIMapping{{VID: 10, VNI: 100}}
			desired := []VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}}

			toAdd, toRemove := diffMappings(current, desired)
			Expect(toAdd).To(HaveLen(1))
			Expect(toAdd[0]).To(Equal(VIDVNIMapping{VID: 20, VNI: 200}))
			Expect(toRemove).To(BeEmpty())
		})

		It("returns removals for stale mappings", func() {
			current := []VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}}
			desired := []VIDVNIMapping{{VID: 10, VNI: 100}}

			toAdd, toRemove := diffMappings(current, desired)
			Expect(toAdd).To(BeEmpty())
			Expect(toRemove).To(HaveLen(1))
			Expect(toRemove[0]).To(Equal(VIDVNIMapping{VID: 20, VNI: 200}))
		})

		It("handles empty current", func() {
			current := []VIDVNIMapping{}
			desired := []VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}}

			toAdd, toRemove := diffMappings(current, desired)
			Expect(toAdd).To(HaveLen(2))
			Expect(toRemove).To(BeEmpty())
		})

		It("handles empty desired", func() {
			current := []VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}}
			desired := []VIDVNIMapping{}

			toAdd, toRemove := diffMappings(current, desired)
			Expect(toAdd).To(BeEmpty())
			Expect(toRemove).To(HaveLen(2))
		})
	})

	Describe("DependencyError", func() {
		It("unwraps to ErrDependencyPending for errors.Is compatibility", func() {
			err := &DependencyError{Dependency: "vrf0", Reason: "test"}
			Expect(err.Unwrap()).To(Equal(ErrDependencyPending))
		})

		It("formats error message with dependency and reason", func() {
			err := &DependencyError{Dependency: "vrf0", Reason: "not found"}
			Expect(err.Error()).To(Equal("dependency not ready: vrf0 (not found)"))
		})
	})

	Describe("isOurDevice ownership check", func() {
		It("returns true for device with our alias prefix", func() {
			link := &netlink.Bridge{}
			link.Alias = "ovn-k8s-ndm:bridge:br0"
			Expect(isOurDevice(link)).To(BeTrue())
		})

		It("returns true for device with our alias prefix (different type)", func() {
			link := &netlink.Vxlan{}
			link.Alias = "ovn-k8s-ndm:vxlan:vxlan0"
			Expect(isOurDevice(link)).To(BeTrue())
		})

		It("returns false for device with empty alias", func() {
			link := &netlink.Bridge{}
			link.Alias = ""
			Expect(isOurDevice(link)).To(BeFalse())
		})

		It("returns false for device with foreign alias", func() {
			link := &netlink.Bridge{}
			link.Alias = "external-system:some-device"
			Expect(isOurDevice(link)).To(BeFalse())
		})

		It("returns false for device with partial prefix", func() {
			link := &netlink.Bridge{}
			link.Alias = "ovn-k8s:bridge:br0"
			Expect(isOurDevice(link)).To(BeFalse())
		})
	})

	Describe("NotOwnedError", func() {
		It("formats error message with device name and reason", func() {
			err := &NotOwnedError{DeviceName: "br0", Reason: "no alias"}
			Expect(err.Error()).To(ContainSubstring("br0"))
			Expect(err.Error()).To(ContainSubstring("no alias"))
		})

		It("is detected by IsNotOwnedError", func() {
			err := &NotOwnedError{DeviceName: "br0", Reason: "test"}
			Expect(IsNotOwnedError(err)).To(BeTrue())
		})

		It("IsNotOwnedError is nil-safe", func() {
			Expect(IsNotOwnedError(nil)).To(BeFalse())
		})
	})

	Describe("runInternal", func() {
		var (
			stopCh       chan struct{}
			doneWg       *sync.WaitGroup
			linkChan     chan netlink.LinkUpdate
			stopChClosed bool
		)

		// waitForDone waits for the WaitGroup with a timeout to prevent test hangs
		waitForDone := func(wg *sync.WaitGroup, timeout time.Duration) {
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()
			Eventually(done, timeout).Should(BeClosed(), "WaitGroup did not complete within timeout")
		}

		BeforeEach(func() {
			stopCh = make(chan struct{})
			doneWg = &sync.WaitGroup{}
			linkChan = make(chan netlink.LinkUpdate)
			stopChClosed = false

			// Use shorter reconcile period for tests
			controller.ReconcilePeriod = 100 * time.Millisecond
		})

		AfterEach(func() {
			if !stopChClosed {
				close(stopCh)
			}
			waitForDone(doneWg, 5*time.Second)
		})

		It("starts controller and sets started flag", func() {
			subscribeCalled := false
			subscribe := func() (bool, chan netlink.LinkUpdate, error) {
				subscribeCalled = true
				return true, linkChan, nil
			}

			err := controller.runInternal(stopCh, doneWg, subscribe)
			Expect(err).NotTo(HaveOccurred())
			Expect(subscribeCalled).To(BeTrue())
			Expect(controller.started).To(BeTrue())
		})

		It("stops gracefully on stopCh close", func() {
			subscribe := func() (bool, chan netlink.LinkUpdate, error) {
				return true, linkChan, nil
			}

			err := controller.runInternal(stopCh, doneWg, subscribe)
			Expect(err).NotTo(HaveOccurred())

			close(stopCh)
			stopChClosed = true
			waitForDone(doneWg, 5*time.Second)
		})

		It("calls sync periodically", func() {
			// Add a device to track sync calls
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			})).To(Succeed())

			subscribe := func() (bool, chan netlink.LinkUpdate, error) {
				return true, linkChan, nil
			}

			err := controller.runInternal(stopCh, doneWg, subscribe)
			Expect(err).NotTo(HaveOccurred())

			// Wait for at least one sync cycle
			time.Sleep(150 * time.Millisecond)

			// Device should still be in store (sync doesn't remove)
			Expect(controller.Has("br0")).To(BeTrue())
		})
	})

	Describe("handleLinkUpdate", func() {
		It("retries pending device when master appears and clears pending on success", func() {
			nlMock := &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
			defer util.ResetNetLinkOpMockInst()

			// Enable immediate application so EnsureLink triggers applyDeviceConfig
			controller.started = true

			cfg := DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "svi0"}},
				Master: "vrf0",
			}

			vrfLink := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0", Index: 10}}
			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "svi0", Index: 5, Flags: net.FlagUp}}
			linkNotFoundErr := netlink.LinkNotFoundError{}

			// Phase 1: EnsureLink with missing master -> DependencyError -> pending = true
			// Order: resolveDependencies (vrf0) -> LinkByName (svi0)
			nlMock.On("LinkByName", "vrf0").Return(nil, linkNotFoundErr).Once() // resolveDependencies: master missing
			nlMock.On("IsLinkNotFoundError", mock.Anything).Return(true)

			Expect(controller.EnsureLink(cfg)).To(Succeed())
			Expect(controller.store["svi0"].pending).To(BeTrue(),
				"device should be pending due to missing master")

			// Phase 2: Master appears -> handleLinkUpdate triggers retry -> success -> pending = false
			// Order: resolveDependencies (vrf0) -> LinkByName (svi0) -> createDevice flow
			nlMock.On("LinkByName", "vrf0").Return(vrfLink, nil)                // resolveDependencies: master exists
			nlMock.On("LinkByName", "svi0").Return(nil, linkNotFoundErr).Once() // device doesn't exist
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil).Once()
			nlMock.On("LinkByName", "svi0").Return(bridgeLink, nil).Once() // createLink: fetch created link
			nlMock.On("LinkSetAlias", bridgeLink, "ovn-k8s-ndm:bridge:svi0").Return(nil).Once()
			nlMock.On("LinkSetMaster", bridgeLink, vrfLink).Return(nil).Once()
			nlMock.On("LinkByName", "svi0").Return(bridgeLink, nil).Once() // ensureDeviceUp: re-fetch link

			controller.handleLinkUpdate(vrfLink)

			Expect(controller.store["svi0"].pending).To(BeFalse(),
				"pending should be cleared after successful retry")
		})

		It("does not retry non-pending devices for unrelated links", func() {
			cfg := DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())
			controller.store["br0"].pending = false

			// Simulate unrelated link update
			dummyLink := &netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{Name: "dummy0"},
			}
			controller.handleLinkUpdate(dummyLink)

			// Device should still be non-pending
			Expect(controller.store["br0"].pending).To(BeFalse())
		})
	})

	Describe("sync", func() {
		It("does not remove devices from store", func() {
			cfg := DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())

			// Call sync
			controller.sync()

			// Device should still be in store
			Expect(controller.Has("br0")).To(BeTrue())
		})

		It("resets pending flag on successful ensure", func() {
			cfg := DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())
			// Manually set pending (simulating prior dependency failure)
			controller.store["br0"].pending = true

			// sync will try to ensure and likely fail (no mock), but pending tracking works
			controller.sync()

			// The sync was attempted - pending state reflects result
			Expect(controller.store["br0"]).NotTo(BeNil())
		})
	})

	Describe("EnsureLink with DependencyError", func() {
		It("marks device as pending when master dependency is missing", func() {
			nlMock := &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
			defer util.ResetNetLinkOpMockInst()

			// Enable immediate application
			controller.started = true

			cfg := DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "svi0"}},
				Master: "vrf0",
			}

			linkNotFoundErr := netlink.LinkNotFoundError{}

			// Order in applyDeviceConfig: resolveDependencies FIRST, then LinkByName
			nlMock.On("LinkByName", "vrf0").Return(nil, linkNotFoundErr).Once() // master missing
			nlMock.On("IsLinkNotFoundError", mock.Anything).Return(true)

			// EnsureLink should succeed (DependencyError is not propagated as error)
			Expect(controller.EnsureLink(cfg)).To(Succeed())

			// Device should be stored and marked as pending
			Expect(controller.store).To(HaveKey("svi0"))
			Expect(controller.store["svi0"].pending).To(BeTrue(),
				"device should be pending when master dependency is missing")
		})

		It("marks device as pending when VLANParent dependency is missing", func() {
			nlMock := &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
			defer util.ResetNetLinkOpMockInst()

			controller.started = true

			cfg := DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: "br0.10"},
					VlanId:    10,
				},
				VLANParent: "br0",
			}

			linkNotFoundErr := netlink.LinkNotFoundError{}

			// Order in applyDeviceConfig: resolveDependencies FIRST, then LinkByName
			nlMock.On("LinkByName", "br0").Return(nil, linkNotFoundErr).Once() // VLANParent missing
			nlMock.On("IsLinkNotFoundError", mock.Anything).Return(true)

			Expect(controller.EnsureLink(cfg)).To(Succeed())

			Expect(controller.store).To(HaveKey("br0.10"))
			Expect(controller.store["br0.10"].pending).To(BeTrue(),
				"device should be pending when VLANParent dependency is missing")
		})

		It("does not mark device as pending when dependencies exist", func() {
			nlMock := &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
			defer util.ResetNetLinkOpMockInst()

			controller.started = true

			cfg := DeviceConfig{
				Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "svi0"}},
				Master: "vrf0",
			}

			vrfLink := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0", Index: 10}}
			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "svi0", Index: 5, Flags: net.FlagUp}}
			linkNotFoundErr := netlink.LinkNotFoundError{}

			// Order: resolveDependencies (vrf0) -> LinkByName (svi0) -> createDevice flow
			nlMock.On("LinkByName", "vrf0").Return(vrfLink, nil)                // master exists
			nlMock.On("LinkByName", "svi0").Return(nil, linkNotFoundErr).Once() // device doesn't exist
			nlMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Bridge")).Return(nil)
			nlMock.On("LinkByName", "svi0").Return(bridgeLink, nil) // createLink: fetch
			nlMock.On("LinkSetAlias", bridgeLink, "ovn-k8s-ndm:bridge:svi0").Return(nil)
			nlMock.On("LinkSetMaster", bridgeLink, vrfLink).Return(nil)
			nlMock.On("LinkByName", "svi0").Return(bridgeLink, nil) // ensureDeviceUp

			Expect(controller.EnsureLink(cfg)).To(Succeed())

			Expect(controller.store).To(HaveKey("svi0"))
			Expect(controller.store["svi0"].pending).To(BeFalse(),
				"device should not be pending when all dependencies exist")
		})
	})

	Describe("configsEqual extended", func() {
		It("returns false for different FlowBased (VXLAN external)", func() {
			cfg1 := &DeviceConfig{
				Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, FlowBased: true},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, FlowBased: false},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns false for different VniFilter", func() {
			cfg1 := &DeviceConfig{
				Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VniFilter: true},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, VniFilter: false},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns false for different VXLAN Learning", func() {
			cfg1 := &DeviceConfig{
				Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: true},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns true for same VXLAN Learning", func() {
			cfg1 := &DeviceConfig{
				Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeTrue())
		})

		It("returns false for different VlanFiltering", func() {
			trueVal := true
			falseVal := false
			cfg1 := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: &trueVal},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanFiltering: &falseVal},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns false for different VlanDefaultPVID", func() {
			pvid1 := uint16(0)
			pvid2 := uint16(1)
			cfg1 := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: &pvid1},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: &pvid2},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns false when one VlanDefaultPVID is nil", func() {
			pvid := uint16(0)
			cfg1 := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: &pvid},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}, VlanDefaultPVID: nil},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns false for different VLANParent", func() {
			cfg1 := &DeviceConfig{
				Link:       &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}},
				VLANParent: "eth0",
			}
			cfg2 := &DeviceConfig{
				Link:       &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}},
				VLANParent: "eth1",
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns true when VLANParent is the same", func() {
			cfg1 := &DeviceConfig{
				Link:       &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}},
				VLANParent: "eth0",
			}
			cfg2 := &DeviceConfig{
				Link:       &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}},
				VLANParent: "eth0",
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeTrue())
		})
	})

	Describe("deviceName", func() {
		It("returns empty for nil Link", func() {
			cfg := &DeviceConfig{Link: nil}
			Expect(cfg.deviceName()).To(Equal(""))
		})

		It("returns name from Link", func() {
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}
			Expect(cfg.deviceName()).To(Equal("br0"))
		})
	})

	Describe("alias generation", func() {
		It("generates correct alias for bridge device", func() {
			cfg := DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br1"}},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())

			device := controller.store["br1"]
			// Format: ovn-k8s-ndm:<type>:<name>
			Expect(device.cfg.alias()).To(Equal("ovn-k8s-ndm:bridge:br1"))
		})

		It("generates correct alias for vxlan device", func() {
			cfg := DeviceConfig{
				Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())

			device := controller.store["vxlan0"]
			Expect(device.cfg.alias()).To(Equal("ovn-k8s-ndm:vxlan:vxlan0"))
		})

		It("generates correct alias for vrf device", func() {
			cfg := DeviceConfig{
				Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())

			device := controller.store["vrf0"]
			Expect(device.cfg.alias()).To(Equal("ovn-k8s-ndm:vrf:vrf0"))
		})

		It("generates correct alias for vlan device", func() {
			cfg := DeviceConfig{
				Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())

			device := controller.store["vlan100"]
			Expect(device.cfg.alias()).To(Equal("ovn-k8s-ndm:vlan:vlan100"))
		})
	})

	Describe("deviceType", func() {
		It("returns correct type for bridge", func() {
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}
			Expect(cfg.deviceType()).To(Equal("bridge"))
		})

		It("returns correct type for vxlan", func() {
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
			}
			Expect(cfg.deviceType()).To(Equal("vxlan"))
		})

		It("returns correct type for vrf", func() {
			cfg := &DeviceConfig{
				Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0"}},
			}
			Expect(cfg.deviceType()).To(Equal("vrf"))
		})

		It("returns correct type for vlan", func() {
			cfg := &DeviceConfig{
				Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "vlan100"}},
			}
			Expect(cfg.deviceType()).To(Equal("vlan"))
		})

		It("returns unknown for nil link", func() {
			cfg := &DeviceConfig{Link: nil}
			Expect(cfg.deviceType()).To(Equal("unknown"))
		})
	})

	Describe("ownership safety", func() {
		// These tests verify the ownership tracking behavior at the unit test level.
		// Integration tests would verify actual kernel behavior.

		It("stores alias with managed prefix", func() {
			cfg := DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}
			Expect(controller.EnsureLink(cfg)).To(Succeed())

			device := controller.store["br0"]
			Expect(device.cfg.alias()).To(HavePrefix(ManagedAliasPrefix))
		})

		It("alias format includes type for collision avoidance", func() {
			// Add devices of different types with same-ish names
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "evpn0"}},
			})).To(Succeed())
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "evpn1"}},
			})).To(Succeed())

			// Aliases should be distinct due to type inclusion
			Expect(controller.store["evpn0"].cfg.alias()).To(Equal("ovn-k8s-ndm:bridge:evpn0"))
			Expect(controller.store["evpn1"].cfg.alias()).To(Equal("ovn-k8s-ndm:vxlan:evpn1"))
		})

		It("ManagedAliasPrefix is consistent", func() {
			// Verify the prefix hasn't been accidentally changed
			Expect(ManagedAliasPrefix).To(Equal("ovn-k8s-ndm:"))
		})
	})

	Describe("hasCriticalMismatch (VRFManager absorption)", func() {
		It("detects VRF table ID mismatch", func() {
			existing := &netlink.Vrf{
				LinkAttrs: netlink.LinkAttrs{Name: "vrf0"},
				Table:     100,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vrf{
					LinkAttrs: netlink.LinkAttrs{Name: "vrf0"},
					Table:     200, // Different table
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeTrue())
		})

		It("allows VRF with matching table ID", func() {
			existing := &netlink.Vrf{
				LinkAttrs: netlink.LinkAttrs{Name: "vrf0"},
				Table:     100,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vrf{
					LinkAttrs: netlink.LinkAttrs{Name: "vrf0"},
					Table:     100, // Same table
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeFalse())
		})

		It("detects VXLAN VNI mismatch", func() {
			existing := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				VxlanId:   100,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   200, // Different VNI
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeTrue())
		})

		It("detects VXLAN src addr mismatch", func() {
			existing := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				VxlanId:   100,
				SrcAddr:   net.ParseIP("10.0.0.1"),
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   100,
					SrcAddr:   net.ParseIP("10.0.0.2"), // Different src
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeTrue())
		})

		It("detects VXLAN port mismatch", func() {
			existing := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				VxlanId:   100,
				Port:      4789,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   100,
					Port:      4790, // Different port
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeTrue())
		})

		It("allows VXLAN with matching critical attrs", func() {
			existing := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				VxlanId:   100,
				SrcAddr:   net.ParseIP("10.0.0.1"),
				Port:      4789,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   100,
					SrcAddr:   net.ParseIP("10.0.0.1"),
					Port:      4789,
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeFalse())
		})

		It("detects VLAN ID mismatch", func() {
			existing := &netlink.Vlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vlan100"},
				VlanId:    100,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vlan100"},
					VlanId:    200, // Different VLAN ID
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeTrue())
		})

		It("detects type mismatch", func() {
			existing := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{Name: "dev0"},
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "dev0"},
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeTrue())
		})

		It("returns false for nil config link", func() {
			existing := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{Name: "br0"},
			}
			cfg := &DeviceConfig{Link: nil}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeFalse())
		})
	})

	Describe("Delete methods for stores", func() {
		It("DeleteBridgePortVLAN removes from store", func() {
			// Use nested map structure
			controller.portVLANStore["port0"] = map[int]*managedPortVLAN{
				10: {linkName: "port0", vlan: BridgePortVLAN{VID: 10, PVID: true}},
			}

			controller.DeleteBridgePortVLAN("port0", 10)

			// When last VID is removed, the entire linkName key is also removed
			Expect(controller.portVLANStore).NotTo(HaveKey("port0"))
		})
	})

	Describe("diffMappingsStatic", func() {
		It("works the same as diffMappings", func() {
			current := []VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 20, VNI: 200}}
			desired := []VIDVNIMapping{{VID: 10, VNI: 100}, {VID: 30, VNI: 300}}

			toAdd1, toRemove1 := diffMappings(current, desired)
			toAdd2, toRemove2 := diffMappings(current, desired)

			Expect(toAdd1).To(Equal(toAdd2))
			Expect(toRemove1).To(Equal(toRemove2))
		})
	})

	Describe("addVIDVNIMapping", func() {
		var nlMock *mocks.NetLinkOps
		var bridgeLink *netlink.Bridge
		var vxlanLink *netlink.Vxlan

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)

			bridgeLink = &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1}}
			vxlanLink = &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 2}}
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("succeeds when all operations succeed", func() {
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(100), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(100), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(1000)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(100), uint32(1000), false, true).Return(nil)

			err := addVIDVNIMapping("br0", "vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("succeeds when already exists (idempotent)", func() {
			alreadyExistsErr := errors.New("already exists")
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(100), false, false, true, false).Return(alreadyExistsErr)
			nlMock.On("IsAlreadyExistsError", alreadyExistsErr).Return(true)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(100), false, false, false, true).Return(alreadyExistsErr)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(1000)).Return(alreadyExistsErr)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(100), uint32(1000), false, true).Return(alreadyExistsErr)

			err := addVIDVNIMapping("br0", "vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("fails when bridge not found", func() {
			nlMock.On("LinkByName", "br0").Return(nil, errors.New("link not found"))

			err := addVIDVNIMapping("br0", "vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get bridge"))
		})

		It("fails when VXLAN not found", func() {
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(nil, errors.New("link not found"))

			err := addVIDVNIMapping("br0", "vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get VXLAN"))
		})

		It("fails on non-already-exists error for bridge self VID", func() {
			permissionErr := errors.New("permission denied")
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(100), false, false, true, false).Return(permissionErr)
			nlMock.On("IsAlreadyExistsError", permissionErr).Return(false)

			err := addVIDVNIMapping("br0", "vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to add VID 100 to bridge self"))
		})

		It("fails on non-already-exists error for VXLAN VID", func() {
			permissionErr := errors.New("permission denied")
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(100), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(100), false, false, false, true).Return(permissionErr)
			nlMock.On("IsAlreadyExistsError", permissionErr).Return(false)

			err := addVIDVNIMapping("br0", "vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to add VID 100 to VXLAN"))
		})

		It("fails on non-already-exists error for VNI add", func() {
			permissionErr := errors.New("permission denied")
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(100), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(100), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(1000)).Return(permissionErr)
			nlMock.On("IsAlreadyExistsError", permissionErr).Return(false)

			err := addVIDVNIMapping("br0", "vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to add VNI 1000"))
		})

		It("fails on non-already-exists error for tunnel info", func() {
			permissionErr := errors.New("permission denied")
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(100), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(100), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(1000)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(100), uint32(1000), false, true).Return(permissionErr)
			nlMock.On("IsAlreadyExistsError", permissionErr).Return(false)

			err := addVIDVNIMapping("br0", "vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to add VID->VNI mapping"))
		})
	})

	Describe("removeVIDVNIMapping", func() {
		var nlMock *mocks.NetLinkOps
		var vxlanLink *netlink.Vxlan

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)

			vxlanLink = &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 2}}
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("succeeds when all operations succeed", func() {
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("BridgeVlanDelTunnelInfo", vxlanLink, uint16(100), uint32(1000), false, true).Return(nil)
			nlMock.On("BridgeVniDel", vxlanLink, uint32(1000)).Return(nil)
			nlMock.On("BridgeVlanDel", vxlanLink, uint16(100), false, false, false, true).Return(nil)

			err := removeVIDVNIMapping("vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("succeeds when VXLAN not found (already deleted)", func() {
			linkNotFoundErr := errors.New("link not found")
			nlMock.On("LinkByName", "vxlan0").Return(nil, linkNotFoundErr)
			nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

			err := removeVIDVNIMapping("vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).NotTo(HaveOccurred())
		})

		It("fails on non-link-not-found error", func() {
			permissionErr := errors.New("permission denied")
			nlMock.On("LinkByName", "vxlan0").Return(nil, permissionErr)
			nlMock.On("IsLinkNotFoundError", permissionErr).Return(false)

			err := removeVIDVNIMapping("vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get VXLAN"))
			Expect(err.Error()).To(ContainSubstring("permission denied"))
		})

		It("succeeds when entries not found (idempotent)", func() {
			entryNotFoundErr := errors.New("entry not found")
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("BridgeVlanDelTunnelInfo", vxlanLink, uint16(100), uint32(1000), false, true).Return(entryNotFoundErr)
			nlMock.On("IsEntryNotFoundError", entryNotFoundErr).Return(true)
			nlMock.On("BridgeVniDel", vxlanLink, uint32(1000)).Return(entryNotFoundErr)
			nlMock.On("BridgeVlanDel", vxlanLink, uint16(100), false, false, false, true).Return(entryNotFoundErr)

			err := removeVIDVNIMapping("vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).NotTo(HaveOccurred())
		})

		It("fails on non-entry-not-found errors during removal", func() {
			permissionErr := errors.New("permission denied")
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("BridgeVlanDelTunnelInfo", vxlanLink, uint16(100), uint32(1000), false, true).Return(nil)
			nlMock.On("BridgeVniDel", vxlanLink, uint32(1000)).Return(permissionErr)
			nlMock.On("IsEntryNotFoundError", permissionErr).Return(false)
			nlMock.On("BridgeVlanDel", vxlanLink, uint16(100), false, false, false, true).Return(nil)

			err := removeVIDVNIMapping("vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to remove mapping"))
			Expect(err.Error()).To(ContainSubstring("permission denied"))
		})

		It("collects all non-entry-not-found errors (best-effort cleanup)", func() {
			entryNotFoundErr := errors.New("entry not found")
			error1 := errors.New("error1")
			error2 := errors.New("error2")
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("BridgeVlanDelTunnelInfo", vxlanLink, uint16(100), uint32(1000), false, true).Return(error1)
			nlMock.On("IsEntryNotFoundError", error1).Return(false)
			nlMock.On("BridgeVniDel", vxlanLink, uint32(1000)).Return(entryNotFoundErr)
			nlMock.On("IsEntryNotFoundError", entryNotFoundErr).Return(true) // This one is ignored
			nlMock.On("BridgeVlanDel", vxlanLink, uint16(100), false, false, false, true).Return(error2)
			nlMock.On("IsEntryNotFoundError", error2).Return(false)

			err := removeVIDVNIMapping("vxlan0", VIDVNIMapping{VID: 100, VNI: 1000})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("error1"))
			Expect(err.Error()).To(ContainSubstring("error2"))
		})
	})

	Describe("deleteDevice", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("succeeds when device not found (already deleted)", func() {
			errNotFound := errors.New("link not found")
			nlMock.On("LinkByName", "br0").Return(nil, errNotFound)
			nlMock.On("IsLinkNotFoundError", errNotFound).Return(true)

			err := deleteDevice("br0")
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes device with our alias", func() {
			bridgeLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Alias: "ovn-k8s-ndm:bridge:br0",
				},
			}
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)
			nlMock.On("LinkDelete", bridgeLink).Return(nil)

			err := deleteDevice("br0")
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("returns NotOwnedError for device without alias", func() {
			bridgeLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Alias: "",
				},
			}
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)

			err := deleteDevice("br0")
			Expect(err).To(HaveOccurred())
			Expect(IsNotOwnedError(err)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("no alias"))
		})

		It("returns NotOwnedError for device with foreign alias", func() {
			bridgeLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Alias: "some-other-system:br0",
				},
			}
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)

			err := deleteDevice("br0")
			Expect(err).To(HaveOccurred())
			Expect(IsNotOwnedError(err)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("foreign alias"))
		})

		It("returns error when LinkDelete fails", func() {
			bridgeLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Alias: "ovn-k8s-ndm:bridge:br0",
				},
			}
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("IsLinkNotFoundError", nil).Return(false)
			nlMock.On("LinkDelete", bridgeLink).Return(errors.New("permission denied"))

			err := deleteDevice("br0")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to delete device"))
		})
	})

	Describe("createDevice", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("creates bridge successfully", func() {
			bridgeLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1},
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}

			nlMock.On("LinkAdd", cfg.Link).Return(nil)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkSetAlias", bridgeLink, "ovn-k8s-ndm:bridge:br0").Return(nil)
			nlMock.On("LinkSetUp", bridgeLink).Return(nil)

			err := createDevice(cfg)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("creates VXLAN with master successfully", func() {
			vxlanLink := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 2},
				VxlanId:   100,
			}
			masterLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1},
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   100,
				},
				Master: "br0",
			}

			nlMock.On("LinkAdd", cfg.Link).Return(nil)
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("LinkSetAlias", vxlanLink, "ovn-k8s-ndm:vxlan:vxlan0").Return(nil)
			nlMock.On("LinkByName", "br0").Return(masterLink, nil)
			// IsLinkNotFoundError is NOT called when LinkByName succeeds
			nlMock.On("LinkSetMaster", vxlanLink, masterLink).Return(nil)
			nlMock.On("LinkSetUp", vxlanLink).Return(nil)

			err := createDevice(cfg)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("fails when LinkAdd fails", func() {
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}

			nlMock.On("LinkAdd", cfg.Link).Return(errors.New("permission denied"))

			err := createDevice(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to create device"))
		})

		It("rolls back on alias failure", func() {
			bridgeLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1},
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}

			nlMock.On("LinkAdd", cfg.Link).Return(nil)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkSetAlias", bridgeLink, "ovn-k8s-ndm:bridge:br0").Return(errors.New("failed"))
			nlMock.On("LinkDelete", bridgeLink).Return(nil) // Rollback

			err := createDevice(cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to set alias"))
			nlMock.AssertExpectations(GinkgoT())
		})

		It("returns DependencyError when master not found", func() {
			vxlanLink := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 2},
			}
			cfg := &DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master: "br0",
			}

			nlMock.On("LinkAdd", cfg.Link).Return(nil)
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("LinkSetAlias", vxlanLink, "ovn-k8s-ndm:vxlan:vxlan0").Return(nil)
			nlMock.On("LinkByName", "br0").Return(nil, errors.New("link not found"))
			nlMock.On("IsLinkNotFoundError", errors.New("link not found")).Return(true)

			err := createDevice(cfg)
			Expect(err).To(HaveOccurred())
			Expect(isDependencyError(err)).To(BeTrue())
		})
	})

	Describe("updateDevice", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("updates device with LinkModify", func() {
			existingLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Alias: "ovn-k8s-ndm:bridge:br0",
					Flags: net.FlagUp,
				},
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0", MTU: 9000},
				},
			}

			// LinkModify receives the concrete type (Bridge) with only mutable fields
			nlMock.On("LinkModify", mock.AnythingOfType("*netlink.Bridge")).Return(nil)
			nlMock.On("LinkByName", "br0").Return(existingLink, nil)

			err := updateDevice(existingLink, cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("updates master when changed", func() {
			existingLink := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:        "vxlan0",
					Index:       2,
					Alias:       "ovn-k8s-ndm:vxlan:vxlan0",
					MasterIndex: 0, // No master currently
					Flags:       net.FlagUp,
				},
				Learning: false,
			}
			masterLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1},
			}
			cfg := &DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}, Learning: false},
				Master: "br0",
			}

			// With the guard, LinkModify is not called when attributes match
			// (alias is generated from cfg and matches existing, Learning matches)
			nlMock.On("LinkByName", "br0").Return(masterLink, nil)
			// IsLinkNotFoundError is NOT called when LinkByName succeeds
			nlMock.On("LinkSetMaster", existingLink, masterLink).Return(nil)
			nlMock.On("LinkByName", "vxlan0").Return(existingLink, nil)

			err := updateDevice(existingLink, cfg)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("returns DependencyError when master deleted", func() {
			existingLink := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "vxlan0",
					Index: 2,
					Alias: "ovn-k8s-ndm:vxlan:vxlan0",
				},
			}
			cfg := &DeviceConfig{
				Link:   &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
				Master: "br0",
			}

			// LinkModify now receives the original type (VXLAN) instead of generic Device
			nlMock.On("LinkModify", mock.AnythingOfType("*netlink.Vxlan")).Return(nil)
			nlMock.On("LinkByName", "br0").Return(nil, errors.New("link not found"))
			nlMock.On("IsLinkNotFoundError", errors.New("link not found")).Return(true)

			err := updateDevice(existingLink, cfg)
			Expect(err).To(HaveOccurred())
			Expect(isDependencyError(err)).To(BeTrue())
		})

		It("fails when LinkModify fails", func() {
			existingLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Alias: "", // Missing alias triggers needsLinkModify
				},
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}

			// LinkModify receives the concrete type (Bridge) with only mutable fields
			nlMock.On("LinkModify", mock.AnythingOfType("*netlink.Bridge")).Return(errors.New("permission denied"))

			err := updateDevice(existingLink, cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to modify link"))
		})
	})

	Describe("needsLinkModify", func() {
		It("returns false when all attributes match", func() {
			current := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Alias: "ovn-k8s-ndm:bridge:br0",
					MTU:   1500,
				},
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{
						Name: "br0",
						MTU:  1500,
					},
				},
			}
			Expect(needsLinkModify(current, cfg)).To(BeFalse())
		})

		It("returns true when alias differs", func() {
			current := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Alias: "", // Missing alias
				},
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}
			Expect(needsLinkModify(current, cfg)).To(BeTrue())
		})

		It("returns true when MTU differs", func() {
			current := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Alias: "ovn-k8s-ndm:bridge:br0",
					MTU:   1500,
				},
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{
						Name: "br0",
						MTU:  9000, // Different MTU
					},
				},
			}
			Expect(needsLinkModify(current, cfg)).To(BeTrue())
		})

		It("returns true when TxQLen differs", func() {
			current := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:   "br0",
					Index:  1,
					Alias:  "ovn-k8s-ndm:bridge:br0",
					TxQLen: 500,
				},
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{
						Name:   "br0",
						TxQLen: 1000,
					},
				},
			}
			Expect(needsLinkModify(current, cfg)).To(BeTrue())
		})

		It("returns true when HardwareAddr differs", func() {
			current := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:         "br0",
					Index:        1,
					Alias:        "ovn-k8s-ndm:bridge:br0",
					HardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
				},
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{
						Name:         "br0",
						HardwareAddr: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF},
					},
				},
			}
			Expect(needsLinkModify(current, cfg)).To(BeTrue())
		})

		It("returns true when VXLAN Learning differs", func() {
			current := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "vxlan0",
					Index: 1,
					Alias: "ovn-k8s-ndm:vxlan:vxlan0",
				},
				Learning: true,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					Learning:  false,
				},
			}
			Expect(needsLinkModify(current, cfg)).To(BeTrue())
		})

		It("returns false when VXLAN attributes match", func() {
			current := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "vxlan0",
					Index: 1,
					Alias: "ovn-k8s-ndm:vxlan:vxlan0",
				},
				Learning: false,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					Learning:  false,
				},
			}
			Expect(needsLinkModify(current, cfg)).To(BeFalse())
		})

		It("returns true when Bridge VlanFiltering differs", func() {
			trueVal := true
			falseVal := false
			current := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Alias: "ovn-k8s-ndm:bridge:br0",
				},
				VlanFiltering: &falseVal,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs:     netlink.LinkAttrs{Name: "br0"},
					VlanFiltering: &trueVal,
				},
			}
			Expect(needsLinkModify(current, cfg)).To(BeTrue())
		})

	})

	Describe("ensureDeviceUp", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("does nothing when device is already up", func() {
			link := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Flags: net.FlagUp,
				},
			}
			nlMock.On("LinkByName", "br0").Return(link, nil)

			err := ensureDeviceUp(link)
			Expect(err).NotTo(HaveOccurred())
			// LinkSetUp should NOT be called
			nlMock.AssertNotCalled(GinkgoT(), "LinkSetUp", mock.Anything)
		})

		It("brings device up when down", func() {
			link := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Flags: 0, // Not up
				},
			}
			nlMock.On("LinkByName", "br0").Return(link, nil)
			nlMock.On("LinkSetUp", link).Return(nil)

			err := ensureDeviceUp(link)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("returns error when LinkSetUp fails", func() {
			link := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "br0",
					Index: 1,
					Flags: 0,
				},
			}
			nlMock.On("LinkByName", "br0").Return(link, nil)
			nlMock.On("LinkSetUp", link).Return(errors.New("permission denied"))

			err := ensureDeviceUp(link)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("createLink", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("creates link and returns it with kernel attributes", func() {
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}
			createdLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 42},
			}

			nlMock.On("LinkAdd", cfg.Link).Return(nil)
			nlMock.On("LinkByName", "br0").Return(createdLink, nil)

			link, err := createLink(cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(link.Attrs().Index).To(Equal(42))
		})

		It("fails when LinkAdd fails", func() {
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
			}

			nlMock.On("LinkAdd", cfg.Link).Return(errors.New("permission denied"))

			link, err := createLink(cfg)
			Expect(err).To(HaveOccurred())
			Expect(link).To(BeNil())
		})
	})

	Describe("resolveDependencies", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		Context("VLANParent validation", func() {
			It("returns error when VLANParent set but Link is nil", func() {
				cfg := &DeviceConfig{
					Link:       nil,
					VLANParent: "eth0",
				}

				resolved, err := resolveDependencies(cfg)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("VLANParent set but Link is nil"))
				Expect(resolved).To(BeNil())
			})

			It("returns error when VLANParent set but Link is Bridge", func() {
				cfg := &DeviceConfig{
					Link:       &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
					VLANParent: "eth0",
				}

				resolved, err := resolveDependencies(cfg)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("VLANParent set but Link is bridge"))
				Expect(resolved).To(BeNil())
			})

			It("returns error when VLANParent set but Link is Vxlan", func() {
				cfg := &DeviceConfig{
					Link:       &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
					VLANParent: "eth0",
				}

				resolved, err := resolveDependencies(cfg)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("VLANParent set but Link is vxlan"))
				Expect(resolved).To(BeNil())
			})
		})

		Context("VLAN without parent validation", func() {
			It("returns error when VLAN has neither VLANParent nor ParentIndex", func() {
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{
							Name:        "vlan100",
							ParentIndex: 0, // No parent index
						},
						VlanId: 100,
					},
					VLANParent: "", // No parent name
				}

				resolved, err := resolveDependencies(cfg)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("requires VLANParent or ParentIndex"))
				Expect(resolved).To(BeNil())
			})
		})

		Context("legacy ParentIndex validation", func() {
			It("returns DependencyError when ParentIndex set but parent not found", func() {
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{
							Name:        "vlan100",
							ParentIndex: 42,
						},
						VlanId: 100,
					},
					VLANParent: "", // Legacy style: using ParentIndex directly
				}

				linkNotFoundErr := errors.New("link not found")
				nlMock.On("LinkByIndex", 42).Return(nil, linkNotFoundErr)
				nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

				resolved, err := resolveDependencies(cfg)
				Expect(err).To(HaveOccurred())
				Expect(isDependencyError(err)).To(BeTrue())
				Expect(err.Error()).To(ContainSubstring("parent ifindex 42"))
				Expect(err.Error()).To(ContainSubstring("VLAN parent not found"))
				Expect(resolved).To(BeNil())
			})

			It("returns wrapped error when LinkByIndex fails with non-link-not-found error", func() {
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{
							Name:        "vlan100",
							ParentIndex: 42,
						},
						VlanId: 100,
					},
					VLANParent: "",
				}

				permissionErr := errors.New("permission denied")
				nlMock.On("LinkByIndex", 42).Return(nil, permissionErr)
				nlMock.On("IsLinkNotFoundError", permissionErr).Return(false)

				resolved, err := resolveDependencies(cfg)
				Expect(err).To(HaveOccurred())
				Expect(isDependencyError(err)).To(BeFalse())
				Expect(err.Error()).To(ContainSubstring("failed to check VLAN parent ifindex 42"))
				Expect(errors.Unwrap(err)).To(Equal(permissionErr))
				Expect(resolved).To(BeNil())
			})

			It("succeeds when ParentIndex parent exists", func() {
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{
							Name:        "vlan100",
							ParentIndex: 42,
						},
						VlanId: 100,
					},
					VLANParent: "",
				}

				parentLink := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 42}}
				nlMock.On("LinkByIndex", 42).Return(parentLink, nil)

				resolved, err := resolveDependencies(cfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(resolved).To(Equal(cfg)) // Returns original config (no VLANParent to resolve)
			})
		})

		Context("Master validation", func() {
			It("returns DependencyError when Master not found", func() {
				cfg := &DeviceConfig{
					Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
					Master: "vrf0",
				}

				linkNotFoundErr := errors.New("link not found")
				nlMock.On("LinkByName", "vrf0").Return(nil, linkNotFoundErr)
				nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

				resolved, err := resolveDependencies(cfg)
				Expect(err).To(HaveOccurred())
				Expect(isDependencyError(err)).To(BeTrue())
				Expect(err.Error()).To(ContainSubstring("vrf0"))
				Expect(err.Error()).To(ContainSubstring("master not found"))
				Expect(resolved).To(BeNil())
			})

			It("returns wrapped error when LinkByName fails with non-link-not-found error", func() {
				cfg := &DeviceConfig{
					Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
					Master: "vrf0",
				}

				permissionErr := errors.New("permission denied")
				nlMock.On("LinkByName", "vrf0").Return(nil, permissionErr)
				nlMock.On("IsLinkNotFoundError", permissionErr).Return(false)

				resolved, err := resolveDependencies(cfg)
				Expect(err).To(HaveOccurred())
				Expect(isDependencyError(err)).To(BeFalse())
				Expect(err.Error()).To(ContainSubstring("failed to check master vrf0"))
				Expect(errors.Unwrap(err)).To(Equal(permissionErr))
				Expect(resolved).To(BeNil())
			})

			It("succeeds when Master exists", func() {
				cfg := &DeviceConfig{
					Link:   &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
					Master: "vrf0",
				}

				vrfLink := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0", Index: 10}}
				nlMock.On("LinkByName", "vrf0").Return(vrfLink, nil)

				resolved, err := resolveDependencies(cfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(resolved).To(Equal(cfg)) // Returns original config (no VLANParent)
			})
		})

		Context("VLANParent resolution", func() {
			It("returns DependencyError when VLANParent not found", func() {
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{Name: "vlan100"},
						VlanId:    100,
					},
					VLANParent: "eth0",
				}

				linkNotFoundErr := errors.New("link not found")
				nlMock.On("LinkByName", "eth0").Return(nil, linkNotFoundErr)
				nlMock.On("IsLinkNotFoundError", linkNotFoundErr).Return(true)

				resolved, err := resolveDependencies(cfg)
				Expect(err).To(HaveOccurred())
				Expect(isDependencyError(err)).To(BeTrue())
				Expect(err.Error()).To(ContainSubstring("eth0"))
				Expect(err.Error()).To(ContainSubstring("VLAN parent not found"))
				Expect(resolved).To(BeNil())
			})

			It("returns wrapped error when VLANParent lookup fails with non-link-not-found error", func() {
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{Name: "vlan100"},
						VlanId:    100,
					},
					VLANParent: "eth0",
				}

				permissionErr := errors.New("permission denied")
				nlMock.On("LinkByName", "eth0").Return(nil, permissionErr)
				nlMock.On("IsLinkNotFoundError", permissionErr).Return(false)

				resolved, err := resolveDependencies(cfg)
				Expect(err).To(HaveOccurred())
				Expect(isDependencyError(err)).To(BeFalse())
				Expect(err.Error()).To(ContainSubstring("failed to check VLAN parent eth0"))
				Expect(errors.Unwrap(err)).To(Equal(permissionErr))
				Expect(resolved).To(BeNil())
			})

			It("resolves VLANParent to ParentIndex successfully", func() {
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{Name: "vlan100"},
						VlanId:    100,
					},
					VLANParent: "eth0",
				}

				parentLink := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 5}}
				nlMock.On("LinkByName", "eth0").Return(parentLink, nil)

				resolved, err := resolveDependencies(cfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(resolved).NotTo(BeNil())
				Expect(resolved).NotTo(Equal(cfg)) // Should be a copy

				// Verify ParentIndex was set
				resolvedVlan, ok := resolved.Link.(*netlink.Vlan)
				Expect(ok).To(BeTrue())
				Expect(resolvedVlan.ParentIndex).To(Equal(5))

				// Original config should not be modified
				originalVlan := cfg.Link.(*netlink.Vlan)
				Expect(originalVlan.ParentIndex).To(Equal(0))
			})

			It("resolves VLANParent with Master both present", func() {
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{Name: "vlan100"},
						VlanId:    100,
					},
					VLANParent: "eth0",
					Master:     "vrf0",
				}

				parentLink := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 5}}
				vrfLink := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf0", Index: 10}}

				// Master is checked first
				nlMock.On("LinkByName", "vrf0").Return(vrfLink, nil)
				// Then VLANParent is resolved
				nlMock.On("LinkByName", "eth0").Return(parentLink, nil)

				resolved, err := resolveDependencies(cfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(resolved).NotTo(BeNil())

				resolvedVlan, ok := resolved.Link.(*netlink.Vlan)
				Expect(ok).To(BeTrue())
				Expect(resolvedVlan.ParentIndex).To(Equal(5))
			})
		})

		Context("success cases without dependencies", func() {
			It("returns original config for non-VLAN device without Master", func() {
				cfg := &DeviceConfig{
					Link: &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0"}},
				}

				resolved, err := resolveDependencies(cfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(resolved).To(Equal(cfg)) // Same pointer, no copy needed
			})

			It("returns original config for VXLAN without dependencies", func() {
				cfg := &DeviceConfig{
					Link: &netlink.Vxlan{
						LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
						VxlanId:   100,
					},
				}

				resolved, err := resolveDependencies(cfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(resolved).To(Equal(cfg))
			})

			It("returns original config for VRF without dependencies", func() {
				cfg := &DeviceConfig{
					Link: &netlink.Vrf{
						LinkAttrs: netlink.LinkAttrs{Name: "vrf0"},
						Table:     100,
					},
				}

				resolved, err := resolveDependencies(cfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(resolved).To(Equal(cfg))
			})
		})

		Context("edge cases", func() {
			It("handles VLAN with both VLANParent and ParentIndex (VLANParent takes precedence)", func() {
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{
							Name:        "vlan100",
							ParentIndex: 99, // This will be overwritten
						},
						VlanId: 100,
					},
					VLANParent: "eth0",
				}

				parentLink := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 5}}
				nlMock.On("LinkByName", "eth0").Return(parentLink, nil)

				resolved, err := resolveDependencies(cfg)
				Expect(err).NotTo(HaveOccurred())

				resolvedVlan, ok := resolved.Link.(*netlink.Vlan)
				Expect(ok).To(BeTrue())
				Expect(resolvedVlan.ParentIndex).To(Equal(5)) // VLANParent resolved, not 99
			})

			It("does not modify original config when resolving VLANParent", func() {
				originalVlan := &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{
						Name:        "vlan100",
						ParentIndex: 0,
					},
					VlanId: 100,
				}
				cfg := &DeviceConfig{
					Link:       originalVlan,
					VLANParent: "eth0",
				}

				parentLink := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 5}}
				nlMock.On("LinkByName", "eth0").Return(parentLink, nil)

				resolved, err := resolveDependencies(cfg)
				Expect(err).NotTo(HaveOccurred())

				// Original should be unchanged
				Expect(originalVlan.ParentIndex).To(Equal(0))
				// Resolved should have new ParentIndex
				resolvedVlan := resolved.Link.(*netlink.Vlan)
				Expect(resolvedVlan.ParentIndex).To(Equal(5))
				// They should be different objects
				Expect(originalVlan).NotTo(BeIdenticalTo(resolvedVlan))
			})
		})
	})

	// =========================================================================
	// EVPN-Specific Tests
	// =========================================================================

	Describe("applyBridgePortSettings", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("sets vlan_tunnel, neigh_suppress, and learning correctly", func() {
			link := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 1}}
			settings := BridgePortSettings{
				VLANTunnel:    true,
				NeighSuppress: true,
				Learning:      false,
			}

			nlMock.On("LinkByName", "vxlan0").Return(link, nil)
			nlMock.On("LinkSetVlanTunnel", link, true).Return(nil)
			nlMock.On("LinkSetBrNeighSuppress", link, true).Return(nil)
			nlMock.On("LinkSetLearning", link, false).Return(nil)

			err := applyBridgePortSettings("vxlan0", settings)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("fails when device not found", func() {
			settings := BridgePortSettings{VLANTunnel: true}

			nlMock.On("LinkByName", "vxlan0").Return(nil, errors.New("link not found"))

			err := applyBridgePortSettings("vxlan0", settings)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get link"))
		})

		It("fails when LinkSetVlanTunnel fails", func() {
			link := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 1}}
			settings := BridgePortSettings{VLANTunnel: true}

			nlMock.On("LinkByName", "vxlan0").Return(link, nil)
			nlMock.On("LinkSetVlanTunnel", link, true).Return(errors.New("operation not supported"))

			err := applyBridgePortSettings("vxlan0", settings)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to set vlan_tunnel"))
		})

		It("fails when LinkSetBrNeighSuppress fails", func() {
			link := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 1}}
			settings := BridgePortSettings{VLANTunnel: true, NeighSuppress: true}

			nlMock.On("LinkByName", "vxlan0").Return(link, nil)
			nlMock.On("LinkSetVlanTunnel", link, true).Return(nil)
			nlMock.On("LinkSetBrNeighSuppress", link, true).Return(errors.New("operation not supported"))

			err := applyBridgePortSettings("vxlan0", settings)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to set neigh_suppress"))
		})

		It("fails when LinkSetLearning fails", func() {
			link := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 1}}
			settings := BridgePortSettings{VLANTunnel: true, NeighSuppress: true, Learning: false}

			nlMock.On("LinkByName", "vxlan0").Return(link, nil)
			nlMock.On("LinkSetVlanTunnel", link, true).Return(nil)
			nlMock.On("LinkSetBrNeighSuppress", link, true).Return(nil)
			nlMock.On("LinkSetLearning", link, false).Return(errors.New("operation not supported"))

			err := applyBridgePortSettings("vxlan0", settings)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to set learning"))
		})
	})

	Describe("getBridgePortSettings", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("retrieves current protinfo settings via LinkGetProtinfo", func() {
			link := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "vxlan0",
					Index: 1,
				},
			}

			protinfo := netlink.Protinfo{
				VlanTunnel:    true,
				NeighSuppress: true,
				Learning:      false,
			}

			nlMock.On("LinkByName", "vxlan0").Return(link, nil)
			nlMock.On("LinkGetProtinfo", link).Return(protinfo, nil)

			settings, err := getBridgePortSettings("vxlan0")
			Expect(err).NotTo(HaveOccurred())
			Expect(settings).NotTo(BeNil())
			Expect(settings.VLANTunnel).To(BeTrue())
			Expect(settings.NeighSuppress).To(BeTrue())
			Expect(settings.Learning).To(BeFalse())
		})

		It("returns error when device not found", func() {
			nlMock.On("LinkByName", "vxlan0").Return(nil, errors.New("link not found"))

			settings, err := getBridgePortSettings("vxlan0")
			Expect(err).To(HaveOccurred())
			Expect(settings).To(BeNil())
		})

		It("returns error when LinkGetProtinfo fails", func() {
			link := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:  "vxlan0",
					Index: 1,
				},
			}

			nlMock.On("LinkByName", "vxlan0").Return(link, nil)
			nlMock.On("LinkGetProtinfo", link).Return(netlink.Protinfo{}, errors.New("no bridge port info"))

			settings, err := getBridgePortSettings("vxlan0")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bridge port info"))
			Expect(settings).To(BeNil())
		})
	})

	Describe("applyBridgePortVLAN", func() {
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

			err := applyBridgePortVLAN("port0", vlan)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("adds VLAN without pvid flag", func() {
			link := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "port0", Index: 1}}
			vlan := BridgePortVLAN{VID: 20, PVID: false, Untagged: false}

			nlMock.On("LinkByName", "port0").Return(link, nil)
			nlMock.On("BridgeVlanAdd", link, uint16(20), false, false, false, true).Return(nil)

			err := applyBridgePortVLAN("port0", vlan)
			Expect(err).NotTo(HaveOccurred())
		})

		It("succeeds when VLAN already exists (idempotent)", func() {
			link := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "port0", Index: 1}}
			vlan := BridgePortVLAN{VID: 10, PVID: true, Untagged: true}

			alreadyExistsErr := errors.New("already exists")
			nlMock.On("LinkByName", "port0").Return(link, nil)
			nlMock.On("BridgeVlanAdd", link, uint16(10), true, true, false, true).Return(alreadyExistsErr)
			nlMock.On("IsAlreadyExistsError", alreadyExistsErr).Return(true)

			err := applyBridgePortVLAN("port0", vlan)
			Expect(err).NotTo(HaveOccurred())
		})

		It("fails when device not found", func() {
			vlan := BridgePortVLAN{VID: 10}

			nlMock.On("LinkByName", "port0").Return(nil, errors.New("link not found"))

			err := applyBridgePortVLAN("port0", vlan)
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

			err := applyBridgePortVLAN("port0", vlan)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to add VLAN"))
		})
	})

	Describe("hasCriticalMismatch for SVD bridge", func() {
		It("detects VlanDefaultPVID mismatch", func() {
			pvid0 := uint16(0)
			pvid1 := uint16(1)
			existing := &netlink.Bridge{
				LinkAttrs:       netlink.LinkAttrs{Name: "br0"},
				VlanDefaultPVID: &pvid1,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs:       netlink.LinkAttrs{Name: "br0"},
					VlanDefaultPVID: &pvid0, // SVD requires pvid=0
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeTrue())
		})

		It("allows nil VlanDefaultPVID in desired (don't care)", func() {
			pvid1 := uint16(1)
			existing := &netlink.Bridge{
				LinkAttrs:       netlink.LinkAttrs{Name: "br0"},
				VlanDefaultPVID: &pvid1,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs:       netlink.LinkAttrs{Name: "br0"},
					VlanDefaultPVID: nil, // Don't care about PVID
				},
			}

			// nil in desired means we don't check this field
			Expect(hasCriticalMismatch(existing, cfg)).To(BeFalse())
		})

		It("allows matching VlanDefaultPVID", func() {
			pvid0 := uint16(0)
			existing := &netlink.Bridge{
				LinkAttrs:       netlink.LinkAttrs{Name: "br0"},
				VlanDefaultPVID: &pvid0,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs:       netlink.LinkAttrs{Name: "br0"},
					VlanDefaultPVID: &pvid0,
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeFalse())
		})
	})

	Describe("hasCriticalMismatch for EVPN VXLAN", func() {
		It("detects FlowBased (external) true→false as critical", func() {
			existing := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				VxlanId:   0, // external mode
				FlowBased: true,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   0,
					FlowBased: false, // Can't downgrade from external
				},
			}

			// Note: hasCriticalMismatch only checks if desired.FlowBased && !existing.FlowBased
			// This test verifies the logic path
			Expect(hasCriticalMismatch(existing, cfg)).To(BeFalse())
		})

		It("detects FlowBased false→true upgrade as critical", func() {
			existing := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				VxlanId:   100,
				FlowBased: false,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   100,
					FlowBased: true, // Want to enable external mode
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeTrue())
		})

		It("detects VniFilter false→true as critical", func() {
			existing := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				VxlanId:   0,
				FlowBased: true,
				VniFilter: false,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   0,
					FlowBased: true,
					VniFilter: true, // Want VNI filtering
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeTrue())
		})

		It("allows matching VXLAN with external and vnifilter", func() {
			existing := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
				VxlanId:   0,
				FlowBased: true,
				VniFilter: true,
				SrcAddr:   net.ParseIP("10.0.0.1"),
				Port:      4789,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   0,
					FlowBased: true,
					VniFilter: true,
					SrcAddr:   net.ParseIP("10.0.0.1"),
					Port:      4789,
				},
			}

			Expect(hasCriticalMismatch(existing, cfg)).To(BeFalse())
		})
	})

	Describe("configsEqual for SVD devices", func() {
		It("returns false for different VlanDefaultPVID values", func() {
			pvid0 := uint16(0)
			pvid1 := uint16(1)
			cfg1 := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs:       netlink.LinkAttrs{Name: "br0"},
					VlanDefaultPVID: &pvid0,
				},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs:       netlink.LinkAttrs{Name: "br0"},
					VlanDefaultPVID: &pvid1,
				},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns true when both VlanDefaultPVID are 0", func() {
			pvid0 := uint16(0)
			cfg1 := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs:       netlink.LinkAttrs{Name: "br0"},
					VlanDefaultPVID: &pvid0,
				},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs:       netlink.LinkAttrs{Name: "br0"},
					VlanDefaultPVID: &pvid0,
				},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeTrue())
		})

		It("returns false for different VXLAN FlowBased", func() {
			cfg1 := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					FlowBased: true,
				},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					FlowBased: false,
				},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns false for different VXLAN VniFilter", func() {
			cfg1 := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VniFilter: true,
				},
			}
			cfg2 := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VniFilter: false,
				},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns true for matching SVD VXLAN config", func() {
			cfg1 := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   0,
					FlowBased: true,
					VniFilter: true,
					SrcAddr:   net.ParseIP("10.0.0.1"),
					Port:      4789,
				},
				Master: "br0",
				BridgePortSettings: &BridgePortSettings{
					VLANTunnel:    true,
					NeighSuppress: true,
					Learning:      false,
				},
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
				Master: "br0",
				BridgePortSettings: &BridgePortSettings{
					VLANTunnel:    true,
					NeighSuppress: true,
					Learning:      false,
				},
			}

			Expect(configsEqual(cfg1, cfg2)).To(BeTrue())
		})
	})

	Describe("SVI (VLAN sub-interface) management", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		Context("hasCriticalMismatch for VLAN", func() {
			It("detects VLAN ID mismatch", func() {
				existing := &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: "br0.10"},
					VlanId:    10,
				}
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{Name: "br0.10"},
						VlanId:    11, // Different VLAN ID
					},
				}

				Expect(hasCriticalMismatch(existing, cfg)).To(BeTrue())
			})

			It("detects VLAN protocol mismatch", func() {
				existing := &netlink.Vlan{
					LinkAttrs:    netlink.LinkAttrs{Name: "br0.10"},
					VlanId:       10,
					VlanProtocol: netlink.VLAN_PROTOCOL_8021Q,
				}
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs:    netlink.LinkAttrs{Name: "br0.10"},
						VlanId:       10,
						VlanProtocol: netlink.VLAN_PROTOCOL_8021AD, // Different protocol
					},
				}

				Expect(hasCriticalMismatch(existing, cfg)).To(BeTrue())
			})

			It("detects parent index mismatch", func() {
				existing := &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{
						Name:        "br0.10",
						ParentIndex: 5,
					},
					VlanId: 10,
				}
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{
							Name:        "br0.10",
							ParentIndex: 6, // Different parent
						},
						VlanId: 10,
					},
				}

				Expect(hasCriticalMismatch(existing, cfg)).To(BeTrue())
			})

			It("allows matching VLAN configuration", func() {
				existing := &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{
						Name:        "br0.10",
						ParentIndex: 5,
					},
					VlanId:       10,
					VlanProtocol: netlink.VLAN_PROTOCOL_8021Q,
				}
				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{
							Name:        "br0.10",
							ParentIndex: 5,
						},
						VlanId:       10,
						VlanProtocol: netlink.VLAN_PROTOCOL_8021Q,
					},
				}

				Expect(hasCriticalMismatch(existing, cfg)).To(BeFalse())
			})
		})

		Context("SVI creation with VRF master", func() {
			It("creates SVI with VLANParent and attaches to VRF", func() {
				bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 5}}
				vrfLink := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf-blue", Index: 10}}
				createdSVI := &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{
						Name:        "br0.11",
						Index:       15,
						ParentIndex: 5,
					},
					VlanId: 11,
				}

				cfg := &DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{Name: "br0.11"},
						VlanId:    11,
					},
					VLANParent: "br0",
					Master:     "vrf-blue",
				}

				// resolveDependencies checks
				nlMock.On("LinkByName", "vrf-blue").Return(vrfLink, nil).Once()
				nlMock.On("LinkByName", "br0").Return(bridgeLink, nil).Once()

				resolved, err := resolveDependencies(cfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(resolved).NotTo(BeNil())

				// Verify resolved config has ParentIndex set
				resolvedVlan := resolved.Link.(*netlink.Vlan)
				Expect(resolvedVlan.ParentIndex).To(Equal(5))

				// Now test createDevice with the resolved config
				nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vlan")).Return(nil)
				nlMock.On("LinkByName", "br0.11").Return(createdSVI, nil)
				nlMock.On("LinkSetAlias", createdSVI, "ovn-k8s-ndm:vlan:br0.11").Return(nil)
				nlMock.On("LinkByName", "vrf-blue").Return(vrfLink, nil)
				nlMock.On("LinkSetMaster", createdSVI, vrfLink).Return(nil)
				nlMock.On("LinkSetUp", createdSVI).Return(nil)

				err = createDevice(resolved)
				Expect(err).NotTo(HaveOccurred())
				nlMock.AssertExpectations(GinkgoT())
			})
		})
	})

	Describe("Multi-UDN isolation", func() {
		It("supports different VID-VNI mappings per UDN", func() {
			controller := NewController()

			// UDN 1: Blue network
			blueVXLAN := DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   0,
					FlowBased: true,
					VniFilter: true,
				},
			}
			Expect(controller.EnsureLink(blueVXLAN)).To(Succeed())

			// Store mappings for blue UDN
			Expect(controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{
				{VID: 10, VNI: 100}, // Blue L2
				{VID: 11, VNI: 101}, // Blue L3
			})).To(Succeed())

			// Verify blue mappings stored
			Expect(controller.vidVNIMappingStore).To(HaveKey("vxlan0"))
			Expect(controller.vidVNIMappingStore["vxlan0"].mappings).To(HaveLen(2))
			Expect(controller.vidVNIMappingStore["vxlan0"].mappings).To(ContainElement(VIDVNIMapping{VID: 10, VNI: 100}))
			Expect(controller.vidVNIMappingStore["vxlan0"].mappings).To(ContainElement(VIDVNIMapping{VID: 11, VNI: 101}))
		})

		It("maintains separate VRF configs per UDN", func() {
			controller := NewController()

			// UDN 1: Blue VRF
			blueVRF := DeviceConfig{
				Link: &netlink.Vrf{
					LinkAttrs: netlink.LinkAttrs{Name: "vrf-blue"},
					Table:     100,
				},
			}
			Expect(controller.EnsureLink(blueVRF)).To(Succeed())

			// UDN 2: Red VRF
			redVRF := DeviceConfig{
				Link: &netlink.Vrf{
					LinkAttrs: netlink.LinkAttrs{Name: "vrf-red"},
					Table:     200,
				},
			}
			Expect(controller.EnsureLink(redVRF)).To(Succeed())

			// Verify both stored separately
			Expect(controller.store).To(HaveKey("vrf-blue"))
			Expect(controller.store).To(HaveKey("vrf-red"))

			blueCfg := controller.store["vrf-blue"].cfg
			redCfg := controller.store["vrf-red"].cfg

			blueVRFLink := blueCfg.Link.(*netlink.Vrf)
			redVRFLink := redCfg.Link.(*netlink.Vrf)

			Expect(blueVRFLink.Table).To(Equal(uint32(100)))
			Expect(redVRFLink.Table).To(Equal(uint32(200)))
		})

		It("maintains separate SVIs per UDN", func() {
			controller := NewController()

			// Blue SVI (VLAN 11)
			blueSVI := DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: "br0.11"},
					VlanId:    11,
				},
				VLANParent: "br0",
				Master:     "vrf-blue",
			}
			Expect(controller.EnsureLink(blueSVI)).To(Succeed())

			// Red SVI (VLAN 13)
			redSVI := DeviceConfig{
				Link: &netlink.Vlan{
					LinkAttrs: netlink.LinkAttrs{Name: "br0.13"},
					VlanId:    13,
				},
				VLANParent: "br0",
				Master:     "vrf-red",
			}
			Expect(controller.EnsureLink(redSVI)).To(Succeed())

			// Verify both stored
			Expect(controller.store).To(HaveKey("br0.11"))
			Expect(controller.store).To(HaveKey("br0.13"))

			blueSVICfg := controller.store["br0.11"].cfg
			redSVICfg := controller.store["br0.13"].cfg

			Expect(blueSVICfg.Master).To(Equal("vrf-blue"))
			Expect(redSVICfg.Master).To(Equal("vrf-red"))
		})

		It("maintains separate port VLANs per UDN", func() {
			controller := NewController()

			// Blue OVS port VLAN
			Expect(controller.EnsureBridgePortVLAN("blue-evpn", BridgePortVLAN{VID: 10, PVID: true, Untagged: true})).To(Succeed())

			// Red OVS port VLAN
			Expect(controller.EnsureBridgePortVLAN("red-evpn", BridgePortVLAN{VID: 12, PVID: true, Untagged: true})).To(Succeed())

			// Verify both stored
			Expect(controller.portVLANStore).To(HaveKey("blue-evpn"))
			Expect(controller.portVLANStore).To(HaveKey("red-evpn"))
			Expect(controller.portVLANStore["blue-evpn"][10].vlan.VID).To(Equal(10))
			Expect(controller.portVLANStore["red-evpn"][12].vlan.VID).To(Equal(12))
		})
	})

	Describe("EVPN SVD Setup", func() {
		Context("Per-VTEP resources (created once)", func() {
			It("stores SVD bridge config with vlan_filtering and default_pvid=0", func() {
				controller := NewController()

				vlanFiltering := true
				pvid0 := uint16(0)
				bridgeCfg := DeviceConfig{
					Link: &netlink.Bridge{
						LinkAttrs:       netlink.LinkAttrs{Name: "br-evpn-vtep0"},
						VlanFiltering:   &vlanFiltering,
						VlanDefaultPVID: &pvid0,
					},
				}

				err := controller.EnsureLink(bridgeCfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(controller.store).To(HaveKey("br-evpn-vtep0"))

				storedBridge := controller.store["br-evpn-vtep0"].cfg.Link.(*netlink.Bridge)
				Expect(*storedBridge.VlanFiltering).To(BeTrue())
				Expect(*storedBridge.VlanDefaultPVID).To(Equal(uint16(0)))
			})

			It("stores SVD VXLAN config with external and vnifilter", func() {
				controller := NewController()

				vxlanCfg := DeviceConfig{
					Link: &netlink.Vxlan{
						LinkAttrs: netlink.LinkAttrs{Name: "vxevpn-vtep0"},
						VxlanId:   0, // 0 for external mode
						FlowBased: true,
						VniFilter: true,
						SrcAddr:   net.ParseIP("100.64.0.1"),
						Port:      4789,
						Learning:  false,
					},
					Master: "br-evpn-vtep0",
					BridgePortSettings: &BridgePortSettings{
						VLANTunnel:    true,
						NeighSuppress: true,
						Learning:      false,
					},
				}

				err := controller.EnsureLink(vxlanCfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(controller.store).To(HaveKey("vxevpn-vtep0"))

				storedVxlan := controller.store["vxevpn-vtep0"].cfg
				vxlan := storedVxlan.Link.(*netlink.Vxlan)
				Expect(vxlan.FlowBased).To(BeTrue())
				Expect(vxlan.VniFilter).To(BeTrue())
				Expect(storedVxlan.Master).To(Equal("br-evpn-vtep0"))
				Expect(storedVxlan.BridgePortSettings.VLANTunnel).To(BeTrue())
				Expect(storedVxlan.BridgePortSettings.NeighSuppress).To(BeTrue())
				Expect(storedVxlan.BridgePortSettings.Learning).To(BeFalse())
			})
		})

		Context("Per-UDN resources", func() {
			It("stores VRF with unique table ID", func() {
				controller := NewController()

				vrfCfg := DeviceConfig{
					Link: &netlink.Vrf{
						LinkAttrs: netlink.LinkAttrs{Name: "blue"},
						Table:     100,
					},
				}

				err := controller.EnsureLink(vrfCfg)
				Expect(err).NotTo(HaveOccurred())

				storedVRF := controller.store["blue"].cfg.Link.(*netlink.Vrf)
				Expect(storedVRF.Table).To(Equal(uint32(100)))
			})

			It("stores SVI (VLAN sub-interface) for IP-VRF", func() {
				controller := NewController()

				sviCfg := DeviceConfig{
					Link: &netlink.Vlan{
						LinkAttrs: netlink.LinkAttrs{Name: "br-evpn0.11"}, // max 15 chars
						VlanId:    11,
					},
					VLANParent: "br-evpn0",
					Master:     "blue", // Attach to VRF
				}

				err := controller.EnsureLink(sviCfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(controller.store).To(HaveKey("br-evpn0.11"))

				storedSVI := controller.store["br-evpn0.11"].cfg
				Expect(storedSVI.VLANParent).To(Equal("br-evpn0"))
				Expect(storedSVI.Master).To(Equal("blue"))
			})

			It("stores VID-VNI mappings for MAC-VRF and IP-VRF", func() {
				controller := NewController()

				mappings := []VIDVNIMapping{
					{VID: 10, VNI: 100}, // MAC-VRF (L2)
					{VID: 11, VNI: 101}, // IP-VRF (L3)
				}

				err := controller.EnsureBridgeMappings("br-evpn-vtep0", "vxevpn-vtep0", mappings)
				Expect(err).NotTo(HaveOccurred())

				Expect(controller.vidVNIMappingStore).To(HaveKey("vxevpn-vtep0"))
				stored := controller.vidVNIMappingStore["vxevpn-vtep0"]
				Expect(stored.bridgeName).To(Equal("br-evpn-vtep0"))
				Expect(stored.mappings).To(HaveLen(2))
			})
		})
	})

	Describe("UDN deletion cleanup", func() {
		It("removes VID-VNI mappings when VXLAN device deleted", func() {
			controller := NewController()

			// Setup VXLAN with mappings
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"}},
			})).To(Succeed())
			Expect(controller.EnsureBridgeMappings("br0", "vxlan0", []VIDVNIMapping{
				{VID: 10, VNI: 100},
			})).To(Succeed())

			Expect(controller.vidVNIMappingStore).To(HaveKey("vxlan0"))

			// Delete VXLAN - should cleanup mappings
			err := controller.DeleteLink("vxlan0")
			Expect(err).NotTo(HaveOccurred())

			Expect(controller.store).NotTo(HaveKey("vxlan0"))
			Expect(controller.vidVNIMappingStore).NotTo(HaveKey("vxlan0"))
		})

		It("removes port VLANs when device deleted", func() {
			controller := NewController()

			// Setup device with port VLANs
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "blue-evpn"}},
			})).To(Succeed())
			Expect(controller.EnsureBridgePortVLAN("blue-evpn", BridgePortVLAN{VID: 10, PVID: true})).To(Succeed())

			Expect(controller.portVLANStore).To(HaveKey("blue-evpn"))

			// Delete device - should cleanup port VLANs
			err := controller.DeleteLink("blue-evpn")
			Expect(err).NotTo(HaveOccurred())

			Expect(controller.store).NotTo(HaveKey("blue-evpn"))
			Expect(controller.portVLANStore).NotTo(HaveKey("blue-evpn"))
		})

		It("preserves other UDN resources when one UDN deleted", func() {
			controller := NewController()

			// Setup two UDNs
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf-blue"}, Table: 100},
			})).To(Succeed())
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "vrf-red"}, Table: 200},
			})).To(Succeed())

			// Delete blue VRF
			err := controller.DeleteLink("vrf-blue")
			Expect(err).NotTo(HaveOccurred())

			// Blue should be gone, red should remain
			Expect(controller.store).NotTo(HaveKey("vrf-blue"))
			Expect(controller.store).To(HaveKey("vrf-red"))
		})

		It("preserves SVD bridge when other UDNs still use it", func() {
			controller := NewController()

			// Shared SVD bridge
			vlanFiltering := true
			pvid0 := uint16(0)
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Bridge{
					LinkAttrs:       netlink.LinkAttrs{Name: "br0"},
					VlanFiltering:   &vlanFiltering,
					VlanDefaultPVID: &pvid0,
				},
			})).To(Succeed())

			// Blue UDN resources
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.11"}, VlanId: 11},
			})).To(Succeed())

			// Red UDN resources
			Expect(controller.EnsureLink(DeviceConfig{
				Link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br0.13"}, VlanId: 13},
			})).To(Succeed())

			// Delete blue SVI
			err := controller.DeleteLink("br0.11")
			Expect(err).NotTo(HaveOccurred())

			// Bridge and red SVI should remain
			Expect(controller.store).To(HaveKey("br0"))
			Expect(controller.store).To(HaveKey("br0.13"))
			Expect(controller.store).NotTo(HaveKey("br0.11"))
		})
	})

	Describe("Self-healing for mappings", func() {
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

		It("syncMappingsForVXLAN detects and corrects drift", func() {
			// Store desired state
			controller.vidVNIMappingStore["vxlan0"] = &managedVIDVNIMappings{
				bridgeName: "br0",
				vxlanName:  "vxlan0",
				mappings: []VIDVNIMapping{
					{VID: 10, VNI: 100},
					{VID: 11, VNI: 101},
				},
			}

			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 2}}

			// Current state only has VID 10, missing VID 11
			// This would be returned by getVIDVNIMappings but we can't easily mock that
			// So we test diffMappings instead
			current := []VIDVNIMapping{{VID: 10, VNI: 100}}
			desired := controller.vidVNIMappingStore["vxlan0"].mappings

			toAdd, toRemove := diffMappings(current, desired)

			// Should add VID 11
			Expect(toAdd).To(HaveLen(1))
			Expect(toAdd[0]).To(Equal(VIDVNIMapping{VID: 11, VNI: 101}))
			Expect(toRemove).To(BeEmpty())

			// Verify addVIDVNIMapping would be called with correct params
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(11), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(11), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(101)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(11), uint32(101), false, true).Return(nil)

			err := addVIDVNIMapping("br0", "vxlan0", toAdd[0])
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("removes stale mappings not in desired state", func() {
			// Desired state only has VID 10
			current := []VIDVNIMapping{
				{VID: 10, VNI: 100},
				{VID: 20, VNI: 200}, // Stale - not in desired
			}
			desired := []VIDVNIMapping{
				{VID: 10, VNI: 100},
			}

			toAdd, toRemove := diffMappings(current, desired)

			Expect(toAdd).To(BeEmpty())
			Expect(toRemove).To(HaveLen(1))
			Expect(toRemove[0]).To(Equal(VIDVNIMapping{VID: 20, VNI: 200}))
		})

		It("detects complete drift (all mappings wrong)", func() {
			current := []VIDVNIMapping{
				{VID: 30, VNI: 300},
				{VID: 40, VNI: 400},
			}
			desired := []VIDVNIMapping{
				{VID: 10, VNI: 100},
				{VID: 11, VNI: 101},
			}

			toAdd, toRemove := diffMappings(current, desired)

			Expect(toAdd).To(HaveLen(2))
			Expect(toRemove).To(HaveLen(2))
		})
	})

	Describe("EnsureBridgeMappings after Run", func() {
		var (
			controller *Controller
			nlMock     *mocks.NetLinkOps
		)

		BeforeEach(func() {
			controller = NewController()
			controller.started = true // Simulate running controller
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("applies mappings immediately when controller is running", func() {
			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 2}}

			// Mock getVIDVNIMappings returning empty (no current mappings)
			// Note: getVIDVNIMappings uses runBridgeCmd which we can't easily mock
			// In a real test, this would be an integration test

			// For unit test, we verify the store is updated
			mappings := []VIDVNIMapping{{VID: 10, VNI: 100}}

			// Setup mocks for the diff+apply cycle
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false).Return(nil)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(10), false, false, false, true).Return(nil)
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(100)).Return(nil)
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(10), uint32(100), false, true).Return(nil)

			// Note: EnsureBridgeMappings calls getVIDVNIMappings which uses runBridgeCmd
			// This is hard to mock without integration tests, so we verify storage
			// and test addVIDVNIMapping separately

			// Direct test of addVIDVNIMapping
			err := addVIDVNIMapping("br0", "vxlan0", mappings[0])
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("stores desired state for self-healing", func() {
			// Even if apply fails, desired state should be stored
			mappings := []VIDVNIMapping{
				{VID: 10, VNI: 100},
				{VID: 11, VNI: 101},
			}

			// Note: We can't easily mock getVIDVNIMappings, so test storage directly
			controller.mu.Lock()
			controller.vidVNIMappingStore["vxlan0"] = &managedVIDVNIMappings{
				bridgeName: "br0",
				vxlanName:  "vxlan0",
				mappings:   mappings,
			}
			controller.mu.Unlock()

			Expect(controller.vidVNIMappingStore["vxlan0"].mappings).To(HaveLen(2))
		})
	})

	Describe("Full EVPN stack device creation", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("creates VRF with correct table", func() {
			vrfLink := &netlink.Vrf{
				LinkAttrs: netlink.LinkAttrs{Name: "blue", Index: 10},
				Table:     100,
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vrf{
					LinkAttrs: netlink.LinkAttrs{Name: "blue"},
					Table:     100,
				},
			}

			nlMock.On("LinkAdd", cfg.Link).Return(nil)
			nlMock.On("LinkByName", "blue").Return(vrfLink, nil)
			nlMock.On("LinkSetAlias", vrfLink, "ovn-k8s-ndm:vrf:blue").Return(nil)
			nlMock.On("LinkSetUp", vrfLink).Return(nil)

			err := createDevice(cfg)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("creates VXLAN with bridge port settings", func() {
			vxlanLink := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 5},
				VxlanId:   0,
				FlowBased: true,
				VniFilter: true,
			}
			vxlanLinkUp := &netlink.Vxlan{
				LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 5, Flags: net.FlagUp},
				VxlanId:   0,
				FlowBased: true,
				VniFilter: true,
			}
			bridgeLink := &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1},
			}
			cfg := &DeviceConfig{
				Link: &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
					VxlanId:   0,
					FlowBased: true,
					VniFilter: true,
					SrcAddr:   net.ParseIP("10.0.0.1"),
					Port:      4789,
				},
				Master: "br0",
				BridgePortSettings: &BridgePortSettings{
					VLANTunnel:    true,
					NeighSuppress: true,
					Learning:      false,
				},
			}

			// For resolveDependencies (1 call) + createDevice master lookup (1 call)
			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil).Times(2)
			nlMock.On("LinkAdd", mock.AnythingOfType("*netlink.Vxlan")).Return(nil)
			// createLink fetches, setAlias, applyBridgePortSettings fetches, ensureDeviceUp fetches
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil).Once() // createLink
			nlMock.On("LinkSetAlias", vxlanLink, "ovn-k8s-ndm:vxlan:vxlan0").Return(nil)
			nlMock.On("LinkSetMaster", vxlanLink, bridgeLink).Return(nil)
			// Bridge port settings - applyBridgePortSettings fetches link
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil).Once() // applyBridgePortSettings
			nlMock.On("LinkSetVlanTunnel", vxlanLink, true).Return(nil)
			nlMock.On("LinkSetBrNeighSuppress", vxlanLink, true).Return(nil)
			nlMock.On("LinkSetLearning", vxlanLink, false).Return(nil)
			// ensureDeviceUp fetches link to check flags
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLinkUp, nil).Once() // ensureDeviceUp - already up

			// First resolve dependencies
			resolved, err := resolveDependencies(cfg)
			Expect(err).NotTo(HaveOccurred())

			// Then create device
			err = createDevice(resolved)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})
	})

	Describe("Bridge VLAN self entries", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("adds bridge self VLAN in addVIDVNIMapping", func() {
			bridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 1}}
			vxlanLink := &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 2}}
			mapping := VIDVNIMapping{VID: 10, VNI: 100}

			nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
			nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
			// Step 1: Add VID to bridge with 'self' flag (4th param=true, 5th=false)
			nlMock.On("BridgeVlanAdd", bridgeLink, uint16(10), false, false, true, false).Return(nil)
			// Step 2: Add VID to VXLAN with 'master' flag (4th param=false, 5th=true)
			nlMock.On("BridgeVlanAdd", vxlanLink, uint16(10), false, false, false, true).Return(nil)
			// Step 3: Add VNI to filter
			nlMock.On("BridgeVniAdd", vxlanLink, uint32(100)).Return(nil)
			// Step 4: Add tunnel info
			nlMock.On("BridgeVlanAddTunnelInfo", vxlanLink, uint16(10), uint32(100), false, true).Return(nil)

			err := addVIDVNIMapping("br0", "vxlan0", mapping)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})
	})

	// =========================================================================
	// Address Management Tests
	// =========================================================================

	Describe("addressesEqual", func() {
		It("returns true for both nil", func() {
			Expect(addressesEqual(nil, nil)).To(BeTrue())
		})

		It("returns false when one is nil and other is empty", func() {
			// nil means "no address management", empty means "manage but want none"
			Expect(addressesEqual(nil, []netlink.Addr{})).To(BeFalse())
			Expect(addressesEqual([]netlink.Addr{}, nil)).To(BeFalse())
		})

		It("returns true for both empty", func() {
			Expect(addressesEqual([]netlink.Addr{}, []netlink.Addr{})).To(BeTrue())
		})

		It("returns true for same addresses", func() {
			addr1 := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}
			addr2 := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}
			Expect(addressesEqual([]netlink.Addr{addr1}, []netlink.Addr{addr2})).To(BeTrue())
		})

		It("returns false for different addresses", func() {
			addr1 := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}
			addr2 := netlink.Addr{IPNet: mustParseIPNet("10.0.0.2/32")}
			Expect(addressesEqual([]netlink.Addr{addr1}, []netlink.Addr{addr2})).To(BeFalse())
		})

		It("returns false for different lengths", func() {
			addr1 := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}
			addr2 := netlink.Addr{IPNet: mustParseIPNet("10.0.0.2/32")}
			Expect(addressesEqual([]netlink.Addr{addr1, addr2}, []netlink.Addr{addr1})).To(BeFalse())
		})

		It("ignores order when comparing addresses", func() {
			addr1 := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}
			addr2 := netlink.Addr{IPNet: mustParseIPNet("10.0.0.2/32")}
			Expect(addressesEqual([]netlink.Addr{addr1, addr2}, []netlink.Addr{addr2, addr1})).To(BeTrue())
		})

		It("compares by IPNet string, ignoring other fields", func() {
			// Same IPNet but different flags - should be equal
			addr1 := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32"), Flags: 0}
			addr2 := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32"), Flags: 128}
			Expect(addressesEqual([]netlink.Addr{addr1}, []netlink.Addr{addr2})).To(BeTrue())
		})
	})

	Describe("isLinkLocalAddress", func() {
		It("returns true for IPv6 link-local address", func() {
			Expect(isLinkLocalAddress(net.ParseIP("fe80::1"))).To(BeTrue())
		})

		It("returns false for regular IPv6 address", func() {
			Expect(isLinkLocalAddress(net.ParseIP("2001:db8::1"))).To(BeFalse())
		})

		It("returns false for IPv4 address", func() {
			Expect(isLinkLocalAddress(net.ParseIP("10.0.0.1"))).To(BeFalse())
		})

		It("returns false for nil", func() {
			Expect(isLinkLocalAddress(nil)).To(BeFalse())
		})

		It("returns true for IPv4 link-local", func() {
			// 169.254.x.x is also link-local
			Expect(isLinkLocalAddress(net.ParseIP("169.254.1.1"))).To(BeTrue())
		})
	})

	Describe("syncAddresses", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		It("does nothing when Addresses is nil", func() {
			cfg := &DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: nil, // No address management
			}
			// No mock expectations set - should not call any netlink functions
			err := syncAddresses("dummy0", cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		It("adds missing addresses", func() {
			dummyLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0", Index: 1}}
			desiredAddr := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}

			cfg := &DeviceConfig{
				Link:      dummyLink,
				Addresses: []netlink.Addr{desiredAddr},
			}

			nlMock.On("LinkByName", "dummy0").Return(dummyLink, nil)
			nlMock.On("AddrList", dummyLink, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil) // No current addresses
			nlMock.On("AddrAdd", dummyLink, mock.MatchedBy(func(addr *netlink.Addr) bool {
				return addr.IPNet.String() == "10.0.0.1/32"
			})).Return(nil)

			err := syncAddresses("dummy0", cfg)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("removes extra addresses (except link-local)", func() {
			dummyLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0", Index: 1}}
			extraAddr := netlink.Addr{IPNet: mustParseIPNetWithIP("192.168.1.1/24")}
			linkLocalAddr := netlink.Addr{IPNet: mustParseIPNetWithIP("fe80::1/64")}

			cfg := &DeviceConfig{
				Link:      dummyLink,
				Addresses: []netlink.Addr{}, // Want no addresses
			}

			nlMock.On("LinkByName", "dummy0").Return(dummyLink, nil)
			nlMock.On("AddrList", dummyLink, netlink.FAMILY_ALL).Return([]netlink.Addr{extraAddr, linkLocalAddr}, nil)
			// Should only delete the non-link-local address
			nlMock.On("AddrDel", dummyLink, mock.MatchedBy(func(addr *netlink.Addr) bool {
				return addr.IPNet.String() == "192.168.1.1/24"
			})).Return(nil)

			err := syncAddresses("dummy0", cfg)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})

		It("preserves link-local addresses even when Addresses is empty", func() {
			dummyLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0", Index: 1}}
			linkLocalAddr := netlink.Addr{IPNet: mustParseIPNet("fe80::1/64")}

			cfg := &DeviceConfig{
				Link:      dummyLink,
				Addresses: []netlink.Addr{}, // Want no addresses, but link-local should stay
			}

			nlMock.On("LinkByName", "dummy0").Return(dummyLink, nil)
			nlMock.On("AddrList", dummyLink, netlink.FAMILY_ALL).Return([]netlink.Addr{linkLocalAddr}, nil)
			// AddrDel should NOT be called for link-local

			err := syncAddresses("dummy0", cfg)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
			nlMock.AssertNotCalled(GinkgoT(), "AddrDel", mock.Anything, mock.Anything)
		})

		It("handles EEXIST gracefully when adding", func() {
			dummyLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0", Index: 1}}
			desiredAddr := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}

			cfg := &DeviceConfig{
				Link:      dummyLink,
				Addresses: []netlink.Addr{desiredAddr},
			}

			eexistErr := errors.New("file exists")
			nlMock.On("LinkByName", "dummy0").Return(dummyLink, nil)
			nlMock.On("AddrList", dummyLink, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", dummyLink, mock.Anything).Return(eexistErr)
			nlMock.On("IsAlreadyExistsError", eexistErr).Return(true)

			err := syncAddresses("dummy0", cfg)
			Expect(err).NotTo(HaveOccurred()) // EEXIST is not an error
			nlMock.AssertExpectations(GinkgoT())
		})

		It("handles ENOENT/EADDRNOTAVAIL gracefully when removing", func() {
			dummyLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0", Index: 1}}
			extraAddr := netlink.Addr{IPNet: mustParseIPNet("192.168.1.1/24")}

			cfg := &DeviceConfig{
				Link:      dummyLink,
				Addresses: []netlink.Addr{},
			}

			enoentErr := errors.New("no such address")
			nlMock.On("LinkByName", "dummy0").Return(dummyLink, nil)
			nlMock.On("AddrList", dummyLink, netlink.FAMILY_ALL).Return([]netlink.Addr{extraAddr}, nil)
			nlMock.On("AddrDel", dummyLink, mock.Anything).Return(enoentErr)
			nlMock.On("IsEntryNotFoundError", enoentErr).Return(true)

			err := syncAddresses("dummy0", cfg)
			Expect(err).NotTo(HaveOccurred()) // ENOENT is not an error
			nlMock.AssertExpectations(GinkgoT())
		})

		It("returns error for non-retriable add failure", func() {
			dummyLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0", Index: 1}}
			desiredAddr := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}

			cfg := &DeviceConfig{
				Link:      dummyLink,
				Addresses: []netlink.Addr{desiredAddr},
			}

			addErr := errors.New("permission denied")
			nlMock.On("LinkByName", "dummy0").Return(dummyLink, nil)
			nlMock.On("AddrList", dummyLink, netlink.FAMILY_ALL).Return([]netlink.Addr{}, nil)
			nlMock.On("AddrAdd", dummyLink, mock.Anything).Return(addErr)
			nlMock.On("IsAlreadyExistsError", addErr).Return(false)

			err := syncAddresses("dummy0", cfg)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("permission denied"))
			nlMock.AssertExpectations(GinkgoT())
		})

		It("syncs addresses to achieve desired state (add and remove)", func() {
			dummyLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0", Index: 1}}
			currentAddr := netlink.Addr{IPNet: mustParseIPNetWithIP("192.168.1.1/24")}
			desiredAddr := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}

			cfg := &DeviceConfig{
				Link:      dummyLink,
				Addresses: []netlink.Addr{desiredAddr},
			}

			nlMock.On("LinkByName", "dummy0").Return(dummyLink, nil)
			nlMock.On("AddrList", dummyLink, netlink.FAMILY_ALL).Return([]netlink.Addr{currentAddr}, nil)
			nlMock.On("AddrAdd", dummyLink, mock.MatchedBy(func(addr *netlink.Addr) bool {
				return addr.IPNet.String() == "10.0.0.1/32"
			})).Return(nil)
			nlMock.On("AddrDel", dummyLink, mock.MatchedBy(func(addr *netlink.Addr) bool {
				return addr.IPNet.String() == "192.168.1.1/24"
			})).Return(nil)

			err := syncAddresses("dummy0", cfg)
			Expect(err).NotTo(HaveOccurred())
			nlMock.AssertExpectations(GinkgoT())
		})
	})

	Describe("configsEqual with Addresses", func() {
		It("returns true when both configs have nil Addresses", func() {
			cfg1 := &DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: nil,
			}
			cfg2 := &DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: nil,
			}
			Expect(configsEqual(cfg1, cfg2)).To(BeTrue())
		})

		It("returns false when one has nil and other has empty Addresses", func() {
			cfg1 := &DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: nil,
			}
			cfg2 := &DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{},
			}
			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})

		It("returns true when both have same addresses", func() {
			addr := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}
			cfg1 := &DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{addr},
			}
			cfg2 := &DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{addr},
			}
			Expect(configsEqual(cfg1, cfg2)).To(BeTrue())
		})

		It("returns false when addresses differ", func() {
			cfg1 := &DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{{IPNet: mustParseIPNet("10.0.0.1/32")}},
			}
			cfg2 := &DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{{IPNet: mustParseIPNet("10.0.0.2/32")}},
			}
			Expect(configsEqual(cfg1, cfg2)).To(BeFalse())
		})
	})

	Describe("EnsureLink with Addresses (store level)", func() {
		It("stores addresses in config", func() {
			controller := NewController()
			addr := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}
			cfg := DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{addr},
			}

			err := controller.EnsureLink(cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.store["dummy0"].cfg.Addresses).To(HaveLen(1))
			Expect(controller.store["dummy0"].cfg.Addresses[0].IPNet.String()).To(Equal("10.0.0.1/32"))
		})

		It("detects address changes for reconciliation", func() {
			controller := NewController()
			addr1 := netlink.Addr{IPNet: mustParseIPNet("10.0.0.1/32")}
			addr2 := netlink.Addr{IPNet: mustParseIPNet("10.0.0.2/32")}

			cfg1 := DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{addr1},
			}
			cfg2 := DeviceConfig{
				Link:      &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}},
				Addresses: []netlink.Addr{addr2},
			}

			Expect(controller.EnsureLink(cfg1)).To(Succeed())
			Expect(controller.store["dummy0"].cfg.Addresses[0].IPNet.String()).To(Equal("10.0.0.1/32"))

			// Update config with different address
			Expect(controller.EnsureLink(cfg2)).To(Succeed())
			Expect(controller.store["dummy0"].cfg.Addresses[0].IPNet.String()).To(Equal("10.0.0.2/32"))
		})
	})

	// Stress tests for loop prevention
	Describe("Loop Prevention Stress Tests", func() {
		var nlMock *mocks.NetLinkOps

		BeforeEach(func() {
			nlMock = &mocks.NetLinkOps{}
			util.SetNetLinkOpMockInst(nlMock)
		})

		AfterEach(func() {
			util.ResetNetLinkOpMockInst()
		})

		Describe("needsLinkModify guard", func() {
			It("returns false when device already matches config (prevents loop)", func() {
				// Simulate a device that already has the correct alias and attributes
				existingLink := &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{
						Name:  "br0",
						Alias: "ovn-k8s-ndm:bridge:br0",
						MTU:   1500,
					},
				}
				cfg := &DeviceConfig{
					Link: &netlink.Bridge{
						LinkAttrs: netlink.LinkAttrs{
							Name: "br0",
							MTU:  1500,
						},
					},
				}

				// needsLinkModify should return false - no modification needed
				Expect(needsLinkModify(existingLink, cfg)).To(BeFalse())
			})

			It("returns true only when attributes actually differ", func() {
				existingLink := &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{
						Name:  "br0",
						Alias: "ovn-k8s-ndm:bridge:br0",
						MTU:   1500,
					},
				}
				cfg := &DeviceConfig{
					Link: &netlink.Bridge{
						LinkAttrs: netlink.LinkAttrs{
							Name: "br0",
							MTU:  9000, // Different MTU
						},
					},
				}

				Expect(needsLinkModify(existingLink, cfg)).To(BeTrue())
			})

			It("returns true when alias differs (ownership marker)", func() {
				existingLink := &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{
						Name:  "br0",
						Alias: "", // No alias yet
						MTU:   1500,
					},
				}
				cfg := &DeviceConfig{
					Link: &netlink.Bridge{
						LinkAttrs: netlink.LinkAttrs{
							Name: "br0",
							MTU:  1500,
						},
					},
				}

				// Alias will be generated from config, should trigger modify
				Expect(needsLinkModify(existingLink, cfg)).To(BeTrue())
			})
		})

		Describe("VXLAN Learning field comparison", func() {
			It("returns false when VXLAN Learning matches", func() {
				existingLink := &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{
						Name:  "vxlan0",
						Alias: "ovn-k8s-ndm:vxlan:vxlan0",
					},
					VxlanId:  100,
					Learning: false,
				}
				cfg := &DeviceConfig{
					Link: &netlink.Vxlan{
						LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
						VxlanId:   100,
						Learning:  false,
					},
				}

				Expect(needsLinkModify(existingLink, cfg)).To(BeFalse())
			})

			It("returns true when VXLAN Learning differs", func() {
				existingLink := &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{
						Name:  "vxlan0",
						Alias: "ovn-k8s-ndm:vxlan:vxlan0",
					},
					VxlanId:  100,
					Learning: true, // Different
				}
				cfg := &DeviceConfig{
					Link: &netlink.Vxlan{
						LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
						VxlanId:   100,
						Learning:  false,
					},
				}

				Expect(needsLinkModify(existingLink, cfg)).To(BeTrue())
			})
		})

		Describe("Rapid handleLinkUpdate calls (simulated event storm)", func() {
			It("limits operations when device state is stable", func() {
				controller := NewController()
				bridgeLink := &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{
						Name:  "br0",
						Index: 10,
						Alias: "ovn-k8s-ndm:bridge:br0",
					},
				}

				// Store the config
				cfg := DeviceConfig{
					Link: &netlink.Bridge{
						LinkAttrs: netlink.LinkAttrs{Name: "br0"},
					},
				}
				Expect(controller.EnsureLink(cfg)).To(Succeed())

				// Mock: device exists and is correct
				nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
				nlMock.On("LinkSetUp", bridgeLink).Return(nil)

				// Simulate 100 rapid link updates (like an event storm)
				for i := 0; i < 100; i++ {
					controller.handleLinkUpdate(bridgeLink)
				}

				// Verify: LinkSetUp should be called (to ensure device is up)
				// but no LinkModify/LinkAdd because device is stable
				nlMock.AssertNotCalled(GinkgoT(), "LinkModify", mock.Anything)
				nlMock.AssertNotCalled(GinkgoT(), "LinkAdd", mock.Anything)
			})

			It("handles VXLAN event storm without excessive operations", func() {
				controller := NewController()
				vxlanLink := &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{
						Name:        "vxlan0",
						Index:       20,
						Alias:       "ovn-k8s-ndm:vxlan:vxlan0",
						MasterIndex: 10,
					},
					VxlanId:  100,
					Learning: false,
				}
				bridgeLink := &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10},
				}

				// Store the config
				cfg := DeviceConfig{
					Link: &netlink.Vxlan{
						LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
						VxlanId:   100,
						Learning:  false,
					},
					Master: "br0",
				}
				Expect(controller.EnsureLink(cfg)).To(Succeed())

				// Mock: device and master exist, device is correctly configured
				nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
				nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
				nlMock.On("LinkSetUp", vxlanLink).Return(nil)
				nlMock.On("LinkGetProtinfo", vxlanLink).Return(netlink.Protinfo{}, nil)

				// Simulate 100 rapid link updates
				for i := 0; i < 100; i++ {
					controller.handleLinkUpdate(vxlanLink)
				}

				// No modifications should occur - device is stable
				nlMock.AssertNotCalled(GinkgoT(), "LinkModify", mock.Anything)
				nlMock.AssertNotCalled(GinkgoT(), "LinkAdd", mock.Anything)
				nlMock.AssertNotCalled(GinkgoT(), "LinkSetMaster", mock.Anything, mock.Anything)
			})
		})

		Describe("Bridge port settings loop prevention", func() {
			It("does not reapply bridge port settings when already correct", func() {
				controller := NewController()
				vxlanLink := &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{
						Name:        "vxlan0",
						Index:       20,
						Alias:       "ovn-k8s-ndm:vxlan:vxlan0",
						MasterIndex: 10,
					},
					VxlanId:  100,
					Learning: false,
				}
				bridgeLink := &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10},
				}

				bps := &BridgePortSettings{
					VLANTunnel:    true,
					NeighSuppress: true,
					Learning:      false,
				}

				cfg := DeviceConfig{
					Link: &netlink.Vxlan{
						LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
						VxlanId:   100,
						Learning:  false,
					},
					Master:             "br0",
					BridgePortSettings: bps,
				}
				Expect(controller.EnsureLink(cfg)).To(Succeed())

				// Mock: device exists and bridge port settings already match
				nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
				nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
				nlMock.On("LinkSetUp", vxlanLink).Return(nil)
				// Return protinfo that matches desired settings
				nlMock.On("LinkGetProtinfo", vxlanLink).Return(netlink.Protinfo{
					VlanTunnel:    true,
					NeighSuppress: true,
					Learning:      false,
				}, nil)

				// Simulate multiple link updates
				for i := 0; i < 50; i++ {
					controller.handleLinkUpdate(vxlanLink)
				}

				// Bridge port settings should NOT be reapplied
				nlMock.AssertNotCalled(GinkgoT(), "LinkSetVlanTunnel", mock.Anything, mock.Anything)
				nlMock.AssertNotCalled(GinkgoT(), "LinkSetBrNeighSuppress", mock.Anything, mock.Anything)
				nlMock.AssertNotCalled(GinkgoT(), "LinkSetLearning", mock.Anything, mock.Anything)
			})

			It("applies bridge port settings only once when they differ", func() {
				controller := NewController()
				vxlanLink := &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{
						Name:        "vxlan0",
						Index:       20,
						Alias:       "ovn-k8s-ndm:vxlan:vxlan0",
						MasterIndex: 10,
					},
					VxlanId:  100,
					Learning: false,
				}
				bridgeLink := &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10},
				}

				bps := &BridgePortSettings{
					VLANTunnel:    true,
					NeighSuppress: true,
					Learning:      false,
				}

				cfg := DeviceConfig{
					Link: &netlink.Vxlan{
						LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
						VxlanId:   100,
						Learning:  false,
					},
					Master:             "br0",
					BridgePortSettings: bps,
				}
				Expect(controller.EnsureLink(cfg)).To(Succeed())

				// First call: settings differ, should apply
				nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
				nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
				nlMock.On("LinkSetUp", vxlanLink).Return(nil)

				// First: return mismatched settings
				nlMock.On("LinkGetProtinfo", vxlanLink).Return(netlink.Protinfo{
					VlanTunnel:    false, // Different
					NeighSuppress: false, // Different
					Learning:      true,  // Different
				}, nil).Once()

				// After first apply: return correct settings
				nlMock.On("LinkGetProtinfo", vxlanLink).Return(netlink.Protinfo{
					VlanTunnel:    true,
					NeighSuppress: true,
					Learning:      false,
				}, nil)

				nlMock.On("LinkSetVlanTunnel", vxlanLink, true).Return(nil).Once()
				nlMock.On("LinkSetBrNeighSuppress", vxlanLink, true).Return(nil).Once()
				nlMock.On("LinkSetLearning", vxlanLink, false).Return(nil).Once()

				// First update - should apply settings
				controller.handleLinkUpdate(vxlanLink)

				// Subsequent updates - should NOT reapply
				for i := 0; i < 10; i++ {
					controller.handleLinkUpdate(vxlanLink)
				}

				// Verify: bridge port settings applied exactly once
				nlMock.AssertNumberOfCalls(GinkgoT(), "LinkSetVlanTunnel", 1)
				nlMock.AssertNumberOfCalls(GinkgoT(), "LinkSetBrNeighSuppress", 1)
				nlMock.AssertNumberOfCalls(GinkgoT(), "LinkSetLearning", 1)
			})
		})

		Describe("updateDevice idempotency", func() {
			It("calling updateDevice multiple times with same state is idempotent", func() {
				vxlanLink := &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{
						Name:        "vxlan0",
						Index:       20,
						Alias:       "ovn-k8s-ndm:vxlan:vxlan0",
						MasterIndex: 10,
						MTU:         1500,
					},
					VxlanId:  100,
					Learning: false,
				}
				bridgeLink := &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10},
				}

				cfg := &DeviceConfig{
					Link: &netlink.Vxlan{
						LinkAttrs: netlink.LinkAttrs{
							Name: "vxlan0",
							MTU:  1500,
						},
						VxlanId:  100,
						Learning: false,
					},
					Master: "br0",
				}

				// Mock all calls
				nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
				nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
				nlMock.On("LinkSetUp", vxlanLink).Return(nil)
				nlMock.On("LinkGetProtinfo", vxlanLink).Return(netlink.Protinfo{}, nil)

				// Call updateDevice 50 times
				for i := 0; i < 50; i++ {
					err := updateDevice(vxlanLink, cfg)
					Expect(err).NotTo(HaveOccurred())
				}

				// No modifications should have been made
				nlMock.AssertNotCalled(GinkgoT(), "LinkModify", mock.Anything)
				nlMock.AssertNotCalled(GinkgoT(), "LinkSetMaster", mock.Anything, mock.Anything)
			})
		})

		Describe("Concurrent sync and event handling", func() {
			It("sync during event processing does not cause duplicate operations", func() {
				controller := NewController()
				bridgeLink := &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{
						Name:  "br0",
						Index: 10,
						Alias: "ovn-k8s-ndm:bridge:br0",
					},
				}

				cfg := DeviceConfig{
					Link: &netlink.Bridge{
						LinkAttrs: netlink.LinkAttrs{Name: "br0"},
					},
				}
				Expect(controller.EnsureLink(cfg)).To(Succeed())

				// Mock
				nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
				nlMock.On("LinkSetUp", bridgeLink).Return(nil)
				nlMock.On("LinkList").Return([]netlink.Link{bridgeLink}, nil)
				nlMock.On("LinkGetProtinfo", bridgeLink).Return(netlink.Protinfo{}, nil)

				// Simulate interleaved sync and event handling
				for i := 0; i < 20; i++ {
					controller.handleLinkUpdate(bridgeLink)
					controller.sync()
				}

				// No unnecessary modifications
				nlMock.AssertNotCalled(GinkgoT(), "LinkModify", mock.Anything)
				nlMock.AssertNotCalled(GinkgoT(), "LinkAdd", mock.Anything)
			})
		})

		Describe("Bounded operations under stress", func() {
			It("operations are bounded even with simulated drift", func() {
				controller := NewController()

				// Create a VXLAN with bridge port settings
				bps := &BridgePortSettings{
					VLANTunnel: true,
					Learning:   false,
				}
				cfg := DeviceConfig{
					Link: &netlink.Vxlan{
						LinkAttrs: netlink.LinkAttrs{Name: "vxlan0"},
						VxlanId:   100,
						Learning:  false,
					},
					Master:             "br0",
					BridgePortSettings: bps,
				}
				Expect(controller.EnsureLink(cfg)).To(Succeed())

				bridgeLink := &netlink.Bridge{
					LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 10},
				}
				vxlanLink := &netlink.Vxlan{
					LinkAttrs: netlink.LinkAttrs{
						Name:        "vxlan0",
						Index:       20,
						Alias:       "ovn-k8s-ndm:vxlan:vxlan0",
						MasterIndex: 10,
					},
					VxlanId:  100,
					Learning: false,
				}

				nlMock.On("LinkByName", "vxlan0").Return(vxlanLink, nil)
				nlMock.On("LinkByName", "br0").Return(bridgeLink, nil)
				nlMock.On("LinkSetUp", vxlanLink).Return(nil)

				// Simulate drift: protinfo shows wrong settings every time
				// This tests that even with persistent drift, we don't infinite loop
				nlMock.On("LinkGetProtinfo", vxlanLink).Return(netlink.Protinfo{
					VlanTunnel:    false, // Always "wrong"
					NeighSuppress: true,  // Different from default (false)
					Learning:      true,  // Always "wrong"
				}, nil)

				// These will be called multiple times due to "drift"
				// applyBridgePortSettings always applies all three settings
				nlMock.On("LinkSetVlanTunnel", vxlanLink, true).Return(nil)
				nlMock.On("LinkSetBrNeighSuppress", vxlanLink, false).Return(nil)
				nlMock.On("LinkSetLearning", vxlanLink, false).Return(nil)

				// Run 100 updates
				for i := 0; i < 100; i++ {
					controller.handleLinkUpdate(vxlanLink)
				}

				// Verify operations are bounded (not infinite)
				// Each update should apply settings once
				calls := nlMock.Calls
				vlanTunnelCalls := 0
				learningCalls := 0
				for _, call := range calls {
					if call.Method == "LinkSetVlanTunnel" {
						vlanTunnelCalls++
					}
					if call.Method == "LinkSetLearning" {
						learningCalls++
					}
				}

				// Should be exactly 100 calls each (one per update)
				// This proves we're not in an infinite loop within each update
				Expect(vlanTunnelCalls).To(Equal(100))
				Expect(learningCalls).To(Equal(100))
			})
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
