// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package macbinding implements the MAC Binding controller for OKEP-6691
// (Scalable ARP and NDP Broadcast Handling for UDN).
//
// This controller watches host neighbor table entries and makes the resolved
// MAC addresses available to UDN (User Defined Network) Gateway Routers. This
// avoids the need for ARP replies/Neighbor Advertisements to be flooded to
// each UDN GR which does not scale as the number of UDNs grows.
//
// Two distinct sync actions are possible, abstracted behind the
// [macBindingSyncOps] interface:
//
//   - ARP responder flows: Program ARP responder flows on br-ex
//     via the openflowManager. This allows br-ex to send ARP replies
//     specifically to the port which the request came from. This mechanism
//     is primarily intended for IPv4 address family and not supported for
//     IPv6 address family.
//
//   - MAC_Binding mirroring: Mirror entries as MAC_Bindings to
//     UDN GR external ports in SBDB. This prevents UDN GRs from sending
//     ARP requests in the first place. This mechanism is primarily intended
//     for IPv6 address family but is also supported for the IPv4 address family.
//
// The controller uses a [sync.Map] as a local cache of MAC/IP binding entries,
// populated by an event handler that reacts to adds, deletes, and MAC changes
// notifications from a netlink subscription. Updates on the default bridge link
// index are tracked, secondary bridges are not supported.
//
// For entries using MAC_Binding mirroring, a periodic scan iterates the cache
// and enqueues any entry that may expire before the next scan in order to
// refresh its timestamp. To avoid timestamp refreshes clogging a single queue
// preventing new entries or mac updates from being synced, there are two
// reconcilers with two separate queues, a bootstrapReconciler handles new
// entries or mac updates at runtime and a refreshReconciler handles timestamp
// refreshes and deletes at runtime and initial sync. A separate [sync.Map]
// tracks known UDNs their datapath UUIDs re-syncing the appropriate entries on
// changes.
package macbinding

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	libovsdbcache "github.com/ovn-kubernetes/libovsdb/cache"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// macBindingSyncOps abstracts the operations used to propagate a CDN
// Gateway Router MAC_Binding entry to UDN Gateway Routers. A single
// implementation provides both ARP flow and MAC_Binding methods.
//
// The reconciler passes a fresh timestamp (time.Now) to MAC_Binding
// methods, not the CDN entry's timestamp, to avoid moving UDN
// timestamps backwards.
//
// There is no DeleteMACBinding: UDN entries age out naturally via
// OVN's mac_binding_age_threshold.
//
// The implementation must be safe for concurrent use from multiple
// reconciler workers.
type macBindingSyncOps interface {
	// EnsureARPFlow programs an OpenFlow ARP responder rule on br-ex via
	// the openflowManager for the given (IP, MAC) pair. When a UDN GR
	// sends an ARP request for this IP, br-ex replies directly with
	// the known MAC, avoiding broadcast.
	EnsureARPFlow(ip, mac string) error

	SyncARPFlows(iptomac map[string]string) error

	// DeleteARPFlow removes the ARP responder flow for the given IP.
	DeleteARPFlow(ip string) error

	// AddMACBinding inserts a MAC_Binding row in SBDB for each UDN GR
	// external port in ports. Used when syncedTimestamp == 0 (entry
	// never synced).
	AddMACBinding(ip, mac string, timestamp int, ports []portInfo) error

	// UpdateMACBinding conditionally updates existing MAC_Binding rows
	// for each UDN GR external port in ports. The OVSDB update must
	// include a timestamp < new_timestamp condition to handle the
	// residual race between computing time.Now and transaction commit:
	// the UDN GR's own statctrl may refresh the timestamp in that
	// window. A no-op update (0 rows, condition not met) is not an
	// error and must not trigger recovery.
	UpdateMACBinding(ip, mac string, timestamp int, ports []portInfo) error

	// DeleteAndAddMACBinding performs a conditional delete followed by
	// a create in a single OVSDB transaction. Used for error recovery
	// when the controller's cache has diverged from SBDB state (e.g.
	// AddMACBinding failed with duplicate, or UpdateMACBinding failed
	// with row missing). Like UpdateMACBinding, the delete must be
	// conditioned on timestamp < new_timestamp to avoid replacing an
	// entry that the UDN's own statctrl has already refreshed to a
	// newer value. A no-op (condition not met) is not an error.
	DeleteAndAddMACBinding(ip, mac string, timestamp int, ports []portInfo) error
}

// cacheEntry holds IP and MAC.
type cacheEntry struct {
	ip  string
	mac atomic.Pointer[string]
}

// getMAC returns the current MAC address.
func (e *cacheEntry) getMAC() string {
	return *e.mac.Load()
}

// setMAC atomically updates the MAC address.
func (e *cacheEntry) setMAC(mac *string) {
	e.mac.Store(mac)
}

// macBindingCacheEntry extends [cacheEntry] with sync state for MAC_Binding
// mirroring.
type macBindingCacheEntry struct {
	cacheEntry

	// syncedTimestamp is the timestamp at last attempted sync.
	syncedTimestamp atomic.Int64

	// syncPending indicates the entry is enqueued for sync.
	// Ownership is claimed by the reconciler via
	// [macBindingCacheEntry.claimSync]; only the goroutine that
	// succeeds performs the sync.
	syncPending atomic.Bool

	// fullSyncPending indicates the next sync must use
	// DeleteAndAddMACBinding. Set on sync errors and when re-syncing
	// to newly added ports (where SBDB state is unknown).
	fullSyncPending atomic.Bool
}

// getSyncTime returns the time at last successful sync.
// Zero time means never synced.
func (e *macBindingCacheEntry) getSyncTime() time.Time {
	ms := e.syncedTimestamp.Load()
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// markSync marks the entry as pending sync if not already pending.
// Returns true if newly marked; callers should only enqueue a
// reconcile when true.
func (e *macBindingCacheEntry) markSync() bool {
	return e.syncPending.CompareAndSwap(false, true)
}

// markFullSync marks the entry for a full sync (DeleteAndAddMACBinding).
func (e *macBindingCacheEntry) markFullSync() {
	e.fullSyncPending.Store(true)
	e.syncPending.Store(true)
}

// claimSync atomically clears syncPending, claiming ownership of the
// sync work. Returns true if claimed; only the caller that gets
// true should proceed with sync.
func (e *macBindingCacheEntry) claimSync() bool {
	return e.syncPending.CompareAndSwap(true, false)
}

// completeSync records a successful sync.
func (e *macBindingCacheEntry) completeSync(t time.Time) {
	e.syncedTimestamp.Store(t.UnixMilli())
	e.fullSyncPending.Store(false)
}

const (
	// enqueueMACBindings is a sentinel reconcile key that triggers a
	// full cache iteration, enqueuing all [macBindingCacheEntry] entries
	// for sync. Used when a structural change (new port, datapath update)
	// requires re-syncing all entries without blocking the caller's
	// thread.
	enqueueMACBindings = "*"

	// reconcileAll is a sentinel reconcile key that triggers a full
	// re-sync.
	reconcileAll = "**"
)

// scanPeriod returns the timestamp refresh interval for SBDB MAC_Binding
// entries.
func scanPeriod() time.Duration {
	ageThreshold, _ := strconv.Atoi(types.GRMACBindingAgeThreshold)
	return time.Duration((3*ageThreshold)/16) * time.Second
}

// staggerSyncTime returns a time randomly spread across the age threshold
// window, leaving a margin of one scan period. This avoids refreshing the
// timestamp of all entries at the same time.
func staggerSyncTime(now time.Time, scanPeriod time.Duration) time.Time {
	ageThreshold, _ := strconv.Atoi(types.GRMACBindingAgeThreshold)
	window := time.Duration(ageThreshold)*time.Second - scanPeriod
	return now.Add(-time.Duration(rand.Int64N(window.Milliseconds())) * time.Millisecond)
}

// openFlowManager abstracts the openflow manager methods needed to
// program ARP responder flows on br-ex.
type openFlowManager interface {
	UpdateFlowCacheEntry(key string, flows []string)
	DeleteFlowsByKey(key string)
	RequestFlowSync()
	Flowskeys() []string
}

// portInfo identifies a UDN GR external port and its datapath.
type portInfo struct {
	LogicalPort  string
	DatapathUUID string
	Network      string
}

// networkRefReconcilerFunc adapts a function to the
// networkmanager.NetworkRefReconciler interface.
type networkRefReconcilerFunc func(node, networkName string)

func (f networkRefReconcilerFunc) Reconcile(node, networkName string) { f(node, networkName) }

// MACBindingController propagates CDN Gateway Router MAC_Binding
// entries to UDN Gateway Routers. One instance per node.
type MACBindingController struct {
	sbClient       libovsdbclient.Client
	networkManager networkmanager.Interface
	syncOps        macBindingSyncOps

	// cache stores *cacheEntry (ARP flow) or *macBindingCacheEntry
	// (MAC_Binding mirroring) values keyed by IP. Pointer is swapped
	// atomically on bridge recreation; in-flight event handler
	// writes to the old map are harmless.
	cache atomic.Pointer[sync.Map]

	// ports tracks UDN GR external ports keyed by logical port name.
	// Add/remove is done by the network reconciler; the PortBinding
	// event handler only updates the datapath UUID. Cleanup of
	// MAC_Binding rows for deleted ports is handled by OVN via
	// strong-reference cascade from Datapath_Binding.
	ports sync.Map

	ipv4Enabled     bool
	ipv4UseARPFlows bool
	ipv6Enabled     bool

	cdnGatewayPort  string
	bridgeName      string
	bridgeLinkIndex atomic.Int32
	nodeName        string

	// bootstrapReconciler handles new entries and MAC changes.
	bootstrapReconciler controller.Reconciler
	// refreshReconciler handles timestamp refreshes, deletes, and initial sync.
	refreshReconciler controller.Reconciler

	nadReconciler controller.Reconciler
}

func (c *MACBindingController) getBridgeLinkIndex() int {
	return int(c.bridgeLinkIndex.Load())
}

func (c *MACBindingController) setBridgeLinkIndex(index int) {
	c.bridgeLinkIndex.Store(int32(index))
}

func (c *MACBindingController) getCache() *sync.Map {
	return c.cache.Load()
}

// NewMACBindingController creates a new MACBindingController.
func NewMACBindingController(
	sbClient libovsdbclient.Client,
	networkManager networkmanager.Interface,
	ofm openFlowManager,
	nodeName string,
	bridgeName string,
	ipv4Enabled bool,
	ipv4UseARPFlows bool,
	ipv6Enabled bool,
) *MACBindingController {
	c := &MACBindingController{
		sbClient:        sbClient,
		networkManager:  networkManager,
		syncOps:         newMACBindingSyncOps(sbClient, ofm),
		nodeName:        nodeName,
		bridgeName:      bridgeName,
		ipv4Enabled:     ipv4Enabled,
		ipv4UseARPFlows: ipv4UseARPFlows,
		ipv6Enabled:     ipv6Enabled,
		cdnGatewayPort:  types.GWRouterToExtSwitchPrefix + (&util.DefaultNetInfo{}).GetNetworkScopedGWRouterName(nodeName),
	}
	c.cache.Store(&sync.Map{})
	c.bootstrapReconciler = controller.NewReconciler(
		"mac-binding-bootstrap",
		&controller.ReconcilerConfig{
			Reconcile:   c.reconcile,
			Threadiness: 1,
		},
	)
	c.refreshReconciler = controller.NewReconciler(
		"mac-binding-refresh",
		&controller.ReconcilerConfig{
			Reconcile:   c.reconcile,
			Threadiness: 1,
		},
	)
	c.nadReconciler = controller.NewReconciler(
		"mac-binding-nads",
		&controller.ReconcilerConfig{
			Reconcile:   c.reconcileNAD,
			Threadiness: 1,
		})
	return c
}

// Run starts the controller and blocks until stopCh is closed.
func (c *MACBindingController) Run(stopCh <-chan struct{}) error {
	if c.reconcilesMACBindings() {
		c.networkManager.RegisterNetworkRefReconciler(networkRefReconcilerFunc(func(node, networkName string) {
			c.reconcileNetwork(node, networkName)
		}))
		c.networkManager.RegisterNADReconciler(c.nadReconciler)
		c.registerPortEventHandler()
	}
	// c.registerNeighEventHandler(stopCh)
	c.registerMACBindingEventHandler()

	if err := controller.Start(
		c.bootstrapReconciler,
		c.refreshReconciler,
		c.nadReconciler,
	); err != nil {
		return err
	}
	c.bootstrapReconciler.Reconcile(reconcileAll)

	period := scanPeriod()
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.scan()
		case <-stopCh:
			controller.Stop(c.bootstrapReconciler, c.refreshReconciler, c.nadReconciler)
			return nil
		}
	}
}

// registerMACBindingEventHandler registers libovsdb event handlers for
// the MAC_Binding table, filtered to this node's CDN GR port.
func (c *MACBindingController) registerMACBindingEventHandler() {
	handleEntryAndEnqueue := func(ip string, mac *string, reconciler controller.Reconciler, create bool) {
		old, loaded := c.getCache().Load(ip)
		if !loaded {
			if !create {
				return
			}
			c.getCache().Store(ip, c.newCacheEntry(ip, mac))
			reconciler.Reconcile(ip)
			return
		}

		switch entry := old.(type) {
		case *cacheEntry:
			entry.setMAC(mac)
			reconciler.Reconcile(ip)
		case *macBindingCacheEntry:
			entry.setMAC(mac)
			if entry.markSync() {
				reconciler.Reconcile(ip)
			}
		}
	}

	c.sbClient.Cache().AddEventHandler(&libovsdbcache.EventHandlerFuncs{
		AddFunc: func(table string, m model.Model) {
			if table != sbdb.MACBindingTable {
				return
			}
			mb := m.(*sbdb.MACBinding)
			if !c.isCDNGatewayPort(mb.LogicalPort) || !c.isIPFamilyEnabled(mb.IP) {
				return
			}
			handleEntryAndEnqueue(mb.IP, &mb.MAC, c.bootstrapReconciler, true)
		},
		UpdateFunc: func(table string, old model.Model, new model.Model) {
			if table != sbdb.MACBindingTable {
				return
			}
			oldMB := old.(*sbdb.MACBinding)
			newMB := new.(*sbdb.MACBinding)
			if !c.isCDNGatewayPort(newMB.LogicalPort) || !c.isIPFamilyEnabled(newMB.IP) {
				return
			}
			if oldMB.MAC == newMB.MAC {
				return
			}
			handleEntryAndEnqueue(newMB.IP, &newMB.MAC, c.bootstrapReconciler, false)
		},
		DeleteFunc: func(table string, m model.Model) {
			if table != sbdb.MACBindingTable {
				return
			}
			mb := m.(*sbdb.MACBinding)
			if !c.isCDNGatewayPort(mb.LogicalPort) || !c.isIPFamilyEnabled(mb.IP) {
				return
			}
			c.getCache().Delete(mb.IP)
			c.refreshReconciler.Reconcile(mb.IP)
		},
	})
}

// registerPortEventHandler registers libovsdb event handlers for the
// PortBinding table to track datapath UUID changes.
func (c *MACBindingController) registerPortEventHandler() {
	updateDatapath := func(logicalPort, datapath string) {
		old, loaded := c.ports.Load(logicalPort)
		if !loaded {
			return
		}
		pi := old.(portInfo)
		if pi.DatapathUUID == datapath {
			return
		}
		klog.V(5).Infof("GR Datapath UUID changed: %s -> %s (was %s)", pi.Network, datapath, pi.DatapathUUID)
		pi.DatapathUUID = datapath
		c.ports.CompareAndSwap(logicalPort, old, pi)
		c.bootstrapReconciler.Reconcile(enqueueMACBindings)
	}

	c.sbClient.Cache().AddEventHandler(&libovsdbcache.EventHandlerFuncs{
		AddFunc: func(table string, m model.Model) {
			if table == sbdb.PortBindingTable {
				pb := m.(*sbdb.PortBinding)
				updateDatapath(pb.LogicalPort, pb.Datapath)
			}
		},
		UpdateFunc: func(table string, _ model.Model, new model.Model) {
			if table == sbdb.PortBindingTable {
				pb := new.(*sbdb.PortBinding)
				updateDatapath(pb.LogicalPort, pb.Datapath)
			}
		},
		DeleteFunc: func(table string, m model.Model) {
			if table == sbdb.PortBindingTable {
				pb := m.(*sbdb.PortBinding)
				updateDatapath(pb.LogicalPort, "")
			}
		},
	})
}

func (c *MACBindingController) reconcileNAD(nad string) error {
	if !c.reconcilesMACBindings() {
		return nil
	}
	netInfo := c.networkManager.GetNetInfoForNADKey(nad)
	if netInfo == nil {
		// network might have been deleted, find out which and remove the port
		// from the cache
		c.ports.Range(func(key, value any) bool {
			portName := key.(string)
			pi := value.(portInfo)
			if len(c.networkManager.GetNADKeysForNetwork(pi.Network)) == 0 {
				klog.Infof("Removed UDN GR port %s as network %s is no longer available", portName, pi.Network)
				c.ports.Delete(portName)
			}
			return true
		})
		return nil
	}

	networkName := netInfo.GetNetworkName()
	var trackNetwork bool
	switch {
	case !netInfo.IsPrimaryNetwork():
		fallthrough
	case netInfo.TopologyType() != types.Layer2Topology && netInfo.TopologyType() != types.Layer3Topology:
		fallthrough
	case netInfo.Uplink() != "":
		fallthrough
	case !c.networkManager.NodeHasNetwork(c.nodeName, networkName):
		trackNetwork = false
	default:
		trackNetwork = true
	}

	portName := types.GWRouterToExtSwitchPrefix + netInfo.GetNetworkScopedGWRouterName(c.nodeName)
	if trackNetwork {
		pi := portInfo{LogicalPort: portName, Network: networkName}
		pb, _ := ops.GetPortBinding(c.sbClient, &sbdb.PortBinding{LogicalPort: portName})
		if pb != nil {
			pi.DatapathUUID = pb.Datapath
		}
		if _, loaded := c.ports.LoadOrStore(portName, pi); !loaded {
			klog.Infof("Added UDN GR port %s (datapath %s), re-syncing all MAC bindings", portName, pi.DatapathUUID)
			c.bootstrapReconciler.Reconcile(enqueueMACBindings)
		}
	} else if _, loaded := c.ports.LoadAndDelete(portName); loaded {
		klog.Infof("Removed UDN GR port %s as network %s is no longer available", portName, networkName)
	}

	return nil
}

func (c *MACBindingController) reconcileNetwork(node, networkName string) {
	if node != c.nodeName {
		return
	}
	if !c.reconcilesMACBindings() {
		return
	}
	nads := c.networkManager.GetNADKeysForNetwork(networkName)
	if len(nads) == 0 {
		return
	}
	c.nadReconciler.Reconcile(nads[0])
}

// HandleLinkEvent should be called from the linkManager callback when
// the external bridge link changes.
func (c *MACBindingController) HandleLinkEvent(link netlink.Link) {
	if link.Attrs().Name != c.bridgeName {
		return
	}
	if link.Attrs().Index == c.getBridgeLinkIndex() {
		return
	}
	c.bootstrapReconciler.Reconcile(reconcileAll)
}

// reconcile dispatches to the appropriate handler based on IP family
// and sentinel keys. Shared by both reconcilers.
func (c *MACBindingController) reconcile(ip string) error {
	if ip == enqueueMACBindings {
		return c.enqueueMACBindings()
	}
	if ip == reconcileAll {
		return c.reconcileAll()
	}

	isIPv6 := utilnet.IsIPv6String(ip)
	if isIPv6 && !c.ipv6Enabled {
		return nil
	}
	if !isIPv6 && !c.ipv4Enabled {
		return nil
	}
	if !isIPv6 && c.ipv4UseARPFlows {
		return c.reconcileARPFlow(ip)
	}
	return c.reconcileMACBinding(ip)
}

func (c *MACBindingController) reconcileAll() error {
	return c.reconcileMACBindings()
	// return c.reconcileNeighbors()
}

// reconcileNeighbors handles bridge discovery and recreation by
// clearing the cache and re-syncing from the host neighbor table.
func (c *MACBindingController) reconcileNeighbors() error {
	nlOps := util.GetNetLinkOps()
	link, err := nlOps.LinkByName(c.bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", c.bridgeName, err)
	}
	newIndex := link.Attrs().Index
	if newIndex == c.getBridgeLinkIndex() {
		return nil
	}
	klog.Infof("Bridge %s link index changed from %d to %d, re-syncing neighbors",
		c.bridgeName, c.getBridgeLinkIndex(), newIndex)
	c.setBridgeLinkIndex(0)
	c.cache.Store(&sync.Map{})
	if err := util.SetAcceptUnsolicitedNeighborForInterface(c.bridgeName); err != nil {
		klog.Warningf("Failed to set accept unsolicited neighbor for %s: %v", c.bridgeName, err)
	}
	c.setBridgeLinkIndex(newIndex)
	return c.neighborSync(newIndex)
}

// neighborSync lists all existing neighbors on the external bridge
// and populates the cache via [MACBindingController.syncEntries].
func (c *MACBindingController) neighborSync(linkIndex int) error {
	nlOps := util.GetNetLinkOps()
	neighs, err := nlOps.NeighList(linkIndex, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to list neighbors: %w", err)
	}

	entries := make(map[string]string)
	for i := range neighs {
		neigh := &neighs[i]
		if !isValidNeighState(neigh.State) || len(neigh.HardwareAddr) == 0 {
			continue
		}
		ip := neigh.IP.String()
		if !c.isIPFamilyEnabled(ip) {
			continue
		}
		entries[ip] = neigh.HardwareAddr.String()
	}

	klog.Infof("Neighbor sync on link index %d: %d entries", linkIndex, len(entries))
	return c.syncEntries(entries)
}

// reconcileMACBindings lists all existing CDN GR MAC_Binding rows from SBDB
// and populates the cache.
func (c *MACBindingController) reconcileMACBindings() error {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()

	var mbs []*sbdb.MACBinding
	err := c.sbClient.WhereCache(func(mb *sbdb.MACBinding) bool {
		return c.isCDNGatewayPort(mb.LogicalPort) && c.isIPFamilyEnabled(mb.IP)
	}).List(ctx, &mbs)
	if err != nil {
		return err
	}

	entries := make(map[string]string, len(mbs))
	for _, mb := range mbs {
		entries[mb.IP] = mb.MAC
	}

	return c.syncEntries(entries)
}

// syncEntries populates the cache from a map of IP→MAC entries.
func (c *MACBindingController) syncEntries(entries map[string]string) error {
	arpFlows := make(map[string]string)
	hasMACBindings := false

	for ip, mac := range entries {
		entry := c.newCacheEntry(ip, &mac)
		if mbe, ok := entry.(*macBindingCacheEntry); ok {
			// State is unknown from a previous run; use full sync
			mbe.markFullSync()
			hasMACBindings = true
		} else {
			arpFlows[ip] = mac
		}
		c.getCache().Store(ip, entry)
	}

	if len(arpFlows) > 0 {
		klog.Infof("Syncing %d ARP flows", len(arpFlows))
		if err := c.syncOps.SyncARPFlows(arpFlows); err != nil {
			return err
		}
	}

	if hasMACBindings {
		klog.Infof("Enqueuing MAC binding resync")
		c.refreshReconciler.Reconcile(enqueueMACBindings)
	}

	return nil
}

// registerNeighEventHandler subscribes to netlink neighbor events on the
// default bridge. Resubscribes automatically on failure or unexpected
// channel close.
func (c *MACBindingController) registerNeighEventHandler(stopCh <-chan struct{}) {
	neighCh := c.neighSubscribe(stopCh)
	subscribeTicker := time.NewTicker(1 * time.Second)

	go func() {
		defer subscribeTicker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-subscribeTicker.C:
				if neighCh != nil {
					continue
				}
				neighCh = c.neighSubscribe(stopCh)
			case update, open := <-neighCh:
				if !open {
					neighCh = c.neighSubscribe(stopCh)
					c.bootstrapReconciler.Reconcile(reconcileAll)
					continue
				}
				c.handleNeighUpdate(update)
			}
		}
	}()
}

// neighSubscribe creates a new netlink neighbor subscription.
func (c *MACBindingController) neighSubscribe(stopCh <-chan struct{}) chan netlink.NeighUpdate {
	neighCh := make(chan netlink.NeighUpdate, 100)
	nlOps := util.GetNetLinkOps()
	if err := nlOps.NeighSubscribeWithOptions(neighCh, stopCh, netlink.NeighSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.Warningf("Neighbor subscribe error: %v", err)
		},
	}); err != nil {
		klog.Errorf("Failed to subscribe to neighbor events: %v", err)
		return nil
	}
	return neighCh
}

// handleNeighUpdate processes a single neighbor update event.
func (c *MACBindingController) handleNeighUpdate(update netlink.NeighUpdate) {
	if update.LinkIndex != c.getBridgeLinkIndex() {
		return
	}
	ip := update.IP.String()
	if !c.isIPFamilyEnabled(ip) {
		return
	}

	if update.Type == unix.RTM_DELNEIGH || !isValidNeighState(update.State) {
		klog.V(5).Infof("Neighbor deleted: %s", ip)
		c.getCache().Delete(ip)
		c.refreshReconciler.Reconcile(ip)
		return
	}

	if len(update.HardwareAddr) == 0 {
		return
	}

	mac := update.HardwareAddr.String()
	old, loaded := c.getCache().Load(ip)
	if !loaded {
		klog.V(5).Infof("New neighbor: %s -> %s", ip, mac)
		c.getCache().Store(ip, c.newCacheEntry(ip, &mac))
		c.bootstrapReconciler.Reconcile(ip)
		return
	}

	switch entry := old.(type) {
	case *cacheEntry:
		if entry.getMAC() == mac {
			return
		}
		klog.V(5).Infof("Neighbor MAC changed: %s -> %s (was %s)", ip, mac, entry.getMAC())
		entry.setMAC(&mac)
		c.bootstrapReconciler.Reconcile(ip)
	case *macBindingCacheEntry:
		if entry.getMAC() == mac {
			return
		}
		klog.V(5).Infof("Neighbor MAC changed: %s -> %s (was %s)", ip, mac, entry.getMAC())
		entry.setMAC(&mac)
		if entry.markSync() {
			c.bootstrapReconciler.Reconcile(ip)
		}
	}
}

// scan enqueues MAC_Binding entries whose SBDB timestamp may expire
// before the next scan.
func (c *MACBindingController) scan() {
	ageThreshold, _ := strconv.Atoi(types.GRMACBindingAgeThreshold)
	ageThresholdDur := time.Duration(ageThreshold) * time.Second
	scan := scanPeriod()
	now := time.Now()

	c.getCache().Range(func(_, value any) bool {
		entry, ok := value.(*macBindingCacheEntry)
		if !ok {
			return true
		}

		if now.Add(scan).After(entry.getSyncTime().Add(ageThresholdDur)) {
			if entry.markSync() {
				c.refreshReconciler.Reconcile(entry.ip)
			}
		}

		return true
	})
}

// enqueueMACBindings enqueues all MAC_Binding entries for a full
// sync (DeleteAndAddMACBinding). Full sync is needed because a new
// port may not have MAC_Binding rows yet, and UpdateMACBinding only
// updates existing rows.
func (c *MACBindingController) enqueueMACBindings() error {
	c.getCache().Range(func(_, value any) bool {
		if entry, ok := value.(*macBindingCacheEntry); ok {
			entry.markFullSync()
			c.bootstrapReconciler.Reconcile(entry.ip)
		}
		return true
	})
	return nil
}

// reconcileARPFlow ensures the ARP responder flow on br-ex matches
// the cache. Deletes the flow if the entry is absent.
func (c *MACBindingController) reconcileARPFlow(ip string) error {
	old, ok := c.getCache().Load(ip)
	if !ok {
		klog.V(5).Infof("Deleting ARP flow for %s", ip)
		return c.syncOps.DeleteARPFlow(ip)
	}
	entry := old.(*cacheEntry)
	klog.V(5).Infof("Ensuring ARP flow %s -> %s", ip, entry.getMAC())
	return c.syncOps.EnsureARPFlow(ip, entry.getMAC())
}

// reconcileMACBinding syncs a MAC_Binding entry to all UDN GR
// external ports.
func (c *MACBindingController) reconcileMACBinding(ip string) error {
	old, ok := c.getCache().Load(ip)
	if !ok {
		return nil
	}

	entry := old.(*macBindingCacheEntry)
	if !entry.claimSync() {
		return nil
	}

	mac := entry.getMAC()
	fullSync := entry.fullSyncPending.Load()
	syncTime := entry.getSyncTime()

	var ports []portInfo
	c.ports.Range(func(_, value any) bool {
		pi := value.(portInfo)
		if pi.DatapathUUID != "" {
			ports = append(ports, pi)
		}
		return true
	})

	if len(ports) == 0 {
		return nil
	}

	now := time.Now()
	nowMs := int(now.UnixMilli())
	var err error
	if fullSync {
		klog.V(5).Infof("Full sync MAC binding %s -> %s to %d ports", ip, mac, len(ports))
		err = c.syncOps.DeleteAndAddMACBinding(ip, mac, nowMs, ports)
	} else if syncTime.IsZero() {
		klog.V(5).Infof("Adding MAC binding %s -> %s to %d ports", ip, mac, len(ports))
		err = c.syncOps.AddMACBinding(ip, mac, nowMs, ports)
	} else {
		klog.V(5).Infof("Refreshing MAC binding %s -> %s to %d ports", ip, mac, len(ports))
		err = c.syncOps.UpdateMACBinding(ip, mac, nowMs, ports)
	}

	if err != nil {
		entry.markFullSync()
		return err
	}

	if syncTime.IsZero() {
		entry.completeSync(staggerSyncTime(now, scanPeriod()))
	} else {
		entry.completeSync(now)
	}
	return nil
}

// newCacheEntry creates the appropriate cache entry type based on IP
// family configuration.
func (c *MACBindingController) newCacheEntry(ip string, mac *string) any {
	isIPv6 := utilnet.IsIPv6String(ip)
	if !isIPv6 && c.ipv4UseARPFlows {
		e := &cacheEntry{ip: ip}
		e.mac.Store(mac)
		return e
	}
	e := &macBindingCacheEntry{
		cacheEntry: cacheEntry{ip: ip},
	}
	e.mac.Store(mac)
	e.markSync()
	return e
}

// isIPFamilyEnabled returns true if the controller is configured to
// handle the IP family of the given address.
func (c *MACBindingController) isIPFamilyEnabled(ip string) bool {
	if utilnet.IsIPv6String(ip) {
		return c.ipv6Enabled
	}
	return c.ipv4Enabled
}

// isCDNGatewayPort returns true if the logical port name matches the
// CDN Gateway Router external port for this node (e.g. "rtoe-GR_node1").
// UDN GR ports include a network name segment
// (e.g. "rtoe-GR_blue_node1") and are excluded.
func (c *MACBindingController) isCDNGatewayPort(logicalPort string) bool {
	return logicalPort == c.cdnGatewayPort
}

func (c *MACBindingController) reconcilesMACBindings() bool {
	return c.ipv6Enabled || (c.ipv4Enabled && !c.ipv4UseARPFlows)
}

// isValidNeighState returns true if the neighbor state indicates a
// resolved entry with a valid MAC address.
func isValidNeighState(state int) bool {
	return state&(netlink.NUD_REACHABLE) != 0
}
