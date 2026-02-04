package controllermanager

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/deviceresource"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovsops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops/ovs"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/controllers/evpn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/managementport"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// NodeControllerManager structure is the object manages all controllers for all networks for ovnkube-node
type NodeControllerManager struct {
	name          string
	ovnNodeClient *util.OVNNodeClientset
	Kube          kube.Interface
	watchFactory  factory.NodeWatchFactory
	stopChan      chan struct{}
	wg            *sync.WaitGroup
	recorder      record.EventRecorder

	// management port device manager
	mpdm *managementport.MgmtPortDeviceManager

	// manages default and primary network management ports VF allocation,
	// it will be nil if no VF resource is defined
	deviceAllocator *deviceresource.DeviceResourceAllocator

	defaultNodeNetworkController *node.DefaultNodeNetworkController

	// networkManager creates and deletes user-defined network controllers
	networkManager networkmanager.Controller
	// vrf manager that creates and manages vrfs for all UDNs
	vrfManager *vrfmanager.Controller
	// route manager that creates and manages routes
	routeManager *routemanager.Controller
	// iprule manager that creates and manages iprules for all UDNs
	ruleManager *iprulemanager.Controller
	// ovs client that allows to read ovs info
	ovsClient client.Client
	// evpn controller that manages EVPN datapath
	evpnController *evpn.Controller
}

// NewNetworkController create node user-defined network controllers for the given NetInfo
func (ncm *NodeControllerManager) NewNetworkController(nInfo util.NetInfo) (networkmanager.NetworkController, error) {
	topoType := nInfo.TopologyType()
	switch topoType {
	case ovntypes.Layer3Topology, ovntypes.Layer2Topology, ovntypes.LocalnetTopology:
		if ncm.mpdm != nil && util.IsNetworkSegmentationSupportEnabled() && nInfo.IsPrimaryNetwork() {
			if err := ncm.mpdm.AllocateDeviceIDForNetwork(nInfo.GetNetworkName()); err != nil {
				return nil, err
			}
		}

		// Pass a shallow clone of the watch factory, this allows multiplexing
		// informers for UDNs.
		udnc, err := node.NewUserDefinedNodeNetworkController(ncm.newCommonNetworkControllerInfo(ncm.watchFactory.(*factory.WatchFactory).ShallowClone()),
			nInfo, ncm.networkManager.Interface(), ncm.vrfManager, ncm.ruleManager, ncm.mpdm, ncm.defaultNodeNetworkController.Gateway)
		if err != nil && ncm.mpdm != nil && util.IsNetworkSegmentationSupportEnabled() && nInfo.IsPrimaryNetwork() {
			_ = ncm.mpdm.ReleaseDeviceIDForNetwork(nInfo.GetNetworkName())
		}
		return udnc, err
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

func (ncm *NodeControllerManager) GetDefaultNetworkController() networkmanager.ReconcilableNetworkController {
	return ncm.defaultNodeNetworkController
}

// syncManagementPorts deletes stale management port entities for networks that deleted during reboot.
func (ncm *NodeControllerManager) syncManagementPorts(validNetworks ...util.NetInfo) error {
	var errs []error

	// build valid set of mpx interfaces
	validMpx := sets.New[string]()
	for _, netInfo := range validNetworks {
		if netInfo.IsPrimaryNetwork() {
			validMpx.Insert(util.GetNetworkScopedK8sMgmtHostIntfName(uint(netInfo.GetNetworkID())))
		}
	}
	// insert default network mp0 port
	validMpx.Insert(util.GetNetworkScopedK8sMgmtHostIntfName(0))
	validMpx.Insert(util.GetNetworkScopedK8sMgmtHostIntfName(0) + "_0")

	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPUHost && ncm.ovsClient != nil {
		// then get all existing management ports for primary UDNs
		// internal management port map, key is managementPortIfName (types.K8sMgmtIntfNamePrefix + <networkID>), value is management port OVS interface name
		internalMgmtPorts := make(map[string]string)

		// representor management port map, key is managementPortIfName (types.K8sMgmtIntfNamePrefix + <networkID>), value is management port OVS interface name and network name
		type repInfo struct {
			name    string // name of OVS interface name
			netName string // network name
		}
		repMgmtPorts := make(map[string]repInfo)

		// first find all internal management ports
		p := func(item *vswitchd.Interface) bool {
			if item.Type == "internal" && strings.HasPrefix(item.Name, ovntypes.K8sMgmtIntfNamePrefix) {
				return true
			}
			return false
		}
		ovsIfaces, err := ovsops.FindInterfacesWithPredicate(ncm.ovsClient, p)
		if err == nil {
			for _, ovsIface := range ovsIfaces {
				internalMgmtPorts[ovsIface.Name] = ovsIface.Name
			}
		}

		// then find all management port OVS interface for representor
		p = func(item *vswitchd.Interface) bool {
			if item.Type == "internal" {
				return false
			}
			_, ok := item.ExternalIDs[ovntypes.OvnManagementPortNameExternalID]
			return ok
		}
		ovsReps, err := ovsops.FindInterfacesWithPredicate(ncm.ovsClient, p)
		if err == nil {
			for _, ovsIface := range ovsReps {
				repMgmtPorts[ovsIface.ExternalIDs[ovntypes.OvnManagementPortNameExternalID]] = repInfo{name: ovsIface.Name, netName: ovsIface.ExternalIDs[ovntypes.NetworkExternalID]}
			}
		}

		// delete stale internal management port OVS interface
		for mgmtPortIfName, mgmtPortOVSIfName := range internalMgmtPorts {
			if !validMpx.Has(mgmtPortIfName) {
				err := managementport.DeleteManagementPortInternalOVSInterface("unknownNetwork", mgmtPortOVSIfName)
				if err != nil {
					errs = append(errs, fmt.Errorf("failed to delete stale OVS management port %s for unknown network: %w", mgmtPortOVSIfName, err))
				}
			}
		}

		// delete stale representor management port OVS interface
		for mgmtPortIfName, repInfo := range repMgmtPorts {
			if !validMpx.Has(mgmtPortIfName) {
				err := managementport.DeleteManagementPortRepInterface(repInfo.netName, repInfo.name, repInfo.name)
				if err != nil {
					errs = append(errs, fmt.Errorf("failed to delete stale OVS representor management port %s: %w", mgmtPortIfName, err))
				}
			}
		}
	}

	// cleanup stale management port netdev
	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPU {
		links, err := util.GetNetLinkOps().LinkList()
		if err == nil {
			for _, link := range links {
				linkName := link.Attrs().Name
				if !strings.HasPrefix(linkName, ovntypes.K8sMgmtIntfNamePrefix) {
					continue
				}
				if validMpx.Has(linkName) {
					continue
				}
				err = managementport.TearDownManagementPortLink("unknownNetwork", link, "")
				if err != nil {
					errs = append(errs, fmt.Errorf("failed to tear down stale management port intrface %s for unknown network: %w", linkName, err))
				}
			}
		}
	}

	// best efforts, log error if failed to delete/tear down stale management ports
	if len(errs) > 0 {
		klog.Error(kerrors.NewAggregate(errs))
	}

	// delete stale management port reservation during reboot.
	if ncm.mpdm == nil {
		return nil
	}

	return ncm.mpdm.SyncManagementPorts(validNetworks...)
}

// CleanupStaleNetworks cleans up all stale entities giving list of all existing node UDN controllers
func (ncm *NodeControllerManager) CleanupStaleNetworks(validNetworks ...util.NetInfo) error {
	var errs []error
	if !util.IsNetworkSegmentationSupportEnabled() {
		return nil
	}

	err := ncm.syncManagementPorts(validNetworks...)
	if err != nil {
		errs = append(errs, err)
	}

	// in DPU mode, vrfManager would be nil
	if ncm.vrfManager != nil {
		validVRFDevices := make(sets.Set[string])
		for _, network := range validNetworks {
			if !network.IsPrimaryNetwork() {
				continue
			}
			validVRFDevices.Insert(util.GetNetworkVRFName(network))
		}
		if err := ncm.vrfManager.Repair(validVRFDevices); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.Join(errs...)
}

// newCommonNetworkControllerInfo creates and returns the base node network controller info
func (ncm *NodeControllerManager) newCommonNetworkControllerInfo(wf factory.NodeWatchFactory) *node.CommonNodeNetworkControllerInfo {
	return node.NewCommonNodeNetworkControllerInfo(ncm.ovnNodeClient.KubeClient, ncm.ovnNodeClient.AdminPolicyRouteClient, wf, ncm.recorder, ncm.name, ncm.routeManager)
}

// isNetworkManagerRequiredForNode checks if network manager should be started
// on the node side, which requires any of the following conditions:
// (1) dpu mode is enabled when multiple networks feature is enabled
// (2) primary user-defined networks is enabled (all modes)
func isNetworkManagerRequiredForNode() bool {
	return (config.OVNKubernetesFeature.EnableMultiNetwork && config.OvnKubeNode.Mode == ovntypes.NodeModeDPU) ||
		util.IsNetworkSegmentationSupportEnabled() ||
		util.IsRouteAdvertisementsEnabled()
}

// NewNodeControllerManager creates a new OVN controller manager to manage all the controller for all networks
func NewNodeControllerManager(ovnClient *util.OVNClientset, wf factory.NodeWatchFactory, name string,
	wg *sync.WaitGroup, eventRecorder record.EventRecorder, routeManager *routemanager.Controller, ovsClient client.Client) (*NodeControllerManager, error) {
	ncm := &NodeControllerManager{
		name:          name,
		ovnNodeClient: &util.OVNNodeClientset{KubeClient: ovnClient.KubeClient, AdminPolicyRouteClient: ovnClient.AdminPolicyRouteClient},
		Kube:          &kube.Kube{KClient: ovnClient.KubeClient},
		watchFactory:  wf,
		stopChan:      make(chan struct{}),
		wg:            wg,
		recorder:      eventRecorder,
		routeManager:  routeManager,
		ovsClient:     ovsClient,
	}

	// need to configure OVS interfaces for Pods on UDNs in the DPU mode
	// need to start NAD controller on node side for programming gateway pieces for UDNs
	// need to start NAD controller on node side for VRF awareness with BGP
	var err error
	ncm.networkManager = networkmanager.Default()
	if isNetworkManagerRequiredForNode() {
		ncm.networkManager, err = networkmanager.NewForNode(name, ncm, wf)
		if err != nil {
			return nil, err
		}
	}

	if config.OvnKubeNode.MgmtPortDPResourceName != "" && config.OvnKubeNode.Mode != ovntypes.NodeModeDPU {
		ncm.deviceAllocator, err = deviceresource.DeviceResourceManager().GetDeviceResourceAllocator(config.OvnKubeNode.MgmtPortDPResourceName)
		if err != nil {
			if err.Error() != deviceresource.ErrResourceNotDefined.Error() {
				return nil, fmt.Errorf("failed to create manage port resources manager for resource %s: %v",
					config.OvnKubeNode.MgmtPortDPResourceName, err)
			}
			// the MgmtPortDPResourceName is not associated with any VF resources
			config.OvnKubeNode.MgmtPortDPResourceName = ""
		} else {
			klog.Infof("Allocated management port resource devices: %v", ncm.deviceAllocator.DeviceIDs())
		}
	}

	if util.IsNetworkSegmentationSupportEnabled() && config.OvnKubeNode.Mode != ovntypes.NodeModeDPU {
		ncm.vrfManager = vrfmanager.NewController(ncm.routeManager)
		ncm.ruleManager = iprulemanager.NewController(config.IPv4Mode, config.IPv6Mode)
	}

	if util.IsEVPNEnabled() {
		ncm.evpnController, err = evpn.NewController(name, wf, ncm.Kube, ncm.networkManager.Interface())
		if err != nil {
			return nil, fmt.Errorf("failed to create EVPN controller: %w", err)
		}
	}

	return ncm, nil
}

// initDefaultNodeNetworkController creates the controller for default network
func (ncm *NodeControllerManager) initDefaultNodeNetworkController(ctx context.Context) error {
	if ncm.mpdm != nil {
		mgmtPortDetails, err := ncm.mpdm.AllocateDeviceIDForDefaultNetwork()
		if err != nil {
			return err
		}
		netdevice, err := util.GetNetdevNameFromDeviceId(mgmtPortDetails.DeviceId, v1.DeviceInfo{})
		if err != nil {
			relErr := ncm.mpdm.ReleaseDeviceIDForNetwork(ovntypes.DefaultNetworkName)
			if relErr != nil {
				klog.Warningf("Failed to release management port device reserved for default network: %v", relErr)
			}
			return fmt.Errorf("failed to get netdev name for device %s allocated for default network: %v", mgmtPortDetails.DeviceId, err)
		}

		if config.OvnKubeNode.MgmtPortNetdev != "" && config.OvnKubeNode.MgmtPortNetdev != netdevice {
			klog.Warningf("MgmtPortNetdev is set explicitly (%s), overriding with resource...",
				config.OvnKubeNode.MgmtPortNetdev)
		}
		config.OvnKubeNode.MgmtPortNetdev = netdevice
		klog.V(5).Infof("Using MgmtPortNetdev (Netdev %s) passed via resource %s",
			config.OvnKubeNode.MgmtPortNetdev, ncm.deviceAllocator.ResourceName())
	}

	defaultNodeNetworkController, err := node.NewDefaultNodeNetworkController(ncm.newCommonNetworkControllerInfo(ncm.watchFactory), ncm.networkManager.Interface(), ncm.ovsClient)
	if err != nil {
		return err
	}
	// Make sure we only set defaultNodeNetworkController in case of no error,
	// otherwise we would initialize the interface with a nil implementation
	// which is not the same as nil interface.
	ncm.defaultNodeNetworkController = defaultNodeNetworkController

	return ncm.defaultNodeNetworkController.Init(ctx) // partial gateway init + OpenFlow Manager
}

// Start the node network controller manager
func (ncm *NodeControllerManager) Start(ctx context.Context, isOVNKubeControllerSyncd *atomic.Bool) (err error) {
	klog.Infof("Starting the node network controller manager, Mode: %s", config.OvnKubeNode.Mode)

	// Initialize OVS exec runner; find OVS binaries that the CNI code uses.
	// Must happen before calling any OVS exec from pkg/cni to prevent races.
	// Not required in DPUHost mode as OVS is not present there.
	if err = cni.SetExec(kexec.New()); err != nil {
		return err
	}

	err = ncm.watchFactory.Start()
	if err != nil {
		return err
	}

	// make sure we clean up after ourselves on failure
	defer func() {
		if err != nil {
			klog.Errorf("Stopping node network controller manager, err=%v", err)
			ncm.Stop(isOVNKubeControllerSyncd)
		}
	}()

	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPUHost {
		// start health check to ensure there are no stale OVS internal ports
		go wait.Until(func() {
			checkForStaleOVSInternalPorts()
			ncm.checkForStaleOVSRepresentorInterfaces()
		}, time.Minute, ncm.stopChan)
	}

	// Let's create Route manager that will manage routes.
	ncm.wg.Add(1)
	go func() {
		defer ncm.wg.Done()
		ncm.routeManager.Run(ncm.stopChan, 2*time.Minute)
	}()

	if config.OvnKubeNode.MgmtPortDPResourceName != "" && config.OvnKubeNode.Mode != ovntypes.NodeModeDPU {
		ncm.mpdm = managementport.NewMgmtPortDeviceManager(ncm.Kube, ncm.watchFactory, ncm.name, ncm.deviceAllocator)
		err = ncm.mpdm.Init()
		if err != nil {
			return fmt.Errorf("failed to init management port device manager: %w", err)
		}
	}

	err = ncm.initDefaultNodeNetworkController(ctx)
	if err != nil {
		return fmt.Errorf("failed to init default node network controller: %v", err)
	}

	if ncm.networkManager != nil {
		err = ncm.networkManager.Start()
		if err != nil {
			return fmt.Errorf("failed to start NAD controller: %w", err)
		}
	}

	err = ncm.defaultNodeNetworkController.Start(ctx)
	if err != nil {
		return fmt.Errorf("failed to start default node network controller: %v", err)
	}

	if ncm.vrfManager != nil {
		// Let's create VRF manager that will manage VRFs for all UDNs
		err = ncm.vrfManager.Run(ncm.stopChan, ncm.wg)
		if err != nil {
			return fmt.Errorf("failed to run VRF Manager: %w", err)
		}
	}

	if ncm.ruleManager != nil {
		// Let's create rule manager that will manage rules on the vrfs for all UDNs
		ncm.wg.Add(1)
		go func() {
			defer ncm.wg.Done()
			ncm.ruleManager.Run(ncm.stopChan, 5*time.Minute)
		}()
		// Tell rule manager that we want to fully own all rules at a particular priority.
		// Any rules created with this priority that we do not recognize it, will be
		// removed by relevant manager.
		if err := ncm.ruleManager.OwnPriority(node.UDNMasqueradeIPRulePriority); err != nil {
			return fmt.Errorf("failed to own priority %d for IP rules: %v", node.UDNMasqueradeIPRulePriority, err)
		}
	}

	if ncm.evpnController != nil {
		if err := ncm.evpnController.Start(); err != nil {
			return fmt.Errorf("failed to start EVPN controller: %w", err)
		}
	}

	// start workaround and remove when ovn has native support for silencing GARPs for LRPs
	// https://issues.redhat.com/browse/FDP-1537
	// when in mode ovnkube controller with node, wait until ovnkube controller is syncd before removing drop flows for GARPs
waitForControllerSyncLoop:
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			if isOVNKubeControllerSyncd != nil && !isOVNKubeControllerSyncd.Load() {
				klog.V(5).Infof("Waiting for ovnkube controller to start before removing GARP drop flows")
				time.Sleep(200 * time.Millisecond)
				continue
			}
			klog.Infof("Removing flows to drop GARP")
			ncm.defaultNodeNetworkController.Gateway.SetDefaultBridgeGARPDropFlows(false)
			if err := ncm.defaultNodeNetworkController.Gateway.Reconcile(); err != nil {
				return fmt.Errorf("failed to reconcile gateway after removing GARP drop flows for ext bridge: %v", err)
			}
			break waitForControllerSyncLoop
		}
	}
	// end workaround

	return nil
}

// Stop gracefully stops all managed controllers
func (ncm *NodeControllerManager) Stop(isOVNKubeControllerSyncd *atomic.Bool) {
	// stop stale ovs ports cleanup
	close(ncm.stopChan)

	if ncm.defaultNodeNetworkController != nil {
		if isOVNKubeControllerSyncd != nil && ncm.defaultNodeNetworkController.Gateway != nil {
			ncm.defaultNodeNetworkController.Gateway.SetDefaultBridgeGARPDropFlows(true)
			if err := ncm.defaultNodeNetworkController.Gateway.Reconcile(); err != nil {
				klog.Errorf("Failed to reconcile gateway after attempting to add flows to the external bridge to drop GARPs: %v", err)
			}
		}
		ncm.defaultNodeNetworkController.Stop()
	}

	// stop the NAD controller
	if ncm.networkManager != nil {
		ncm.networkManager.Stop()
	}

	if ncm.evpnController != nil {
		ncm.evpnController.Stop()
	}
}

// checkForStaleOVSRepresentorInterfaces checks for stale OVS ports backed by Repreresentor interfaces,
// derive iface-id from pod name and namespace then remove any interfaces assoicated with a sandbox that are
// not scheduled to the node.
func (ncm *NodeControllerManager) checkForStaleOVSRepresentorInterfaces() {
	// Get all representor interfaces. these are OVS interfaces that have their external_ids:sandbox and vf-netdev-name set.
	out, stderr, err := util.RunOVSVsctl("--columns=name,external_ids", "--data=bare", "--no-headings",
		"--format=csv", "find", "Interface", "external_ids:sandbox!=\"\"", "external_ids:vf-netdev-name!=\"\"")
	if err != nil {
		klog.Errorf("Failed to list ovn-k8s OVS interfaces:, stderr: %q, error: %v", stderr, err)
		return
	}

	if out == "" {
		return
	}

	// parse this data into local struct
	type interfaceInfo struct {
		Name   string
		PodUID string
	}

	lines := strings.Split(out, "\n")
	interfaceInfos := make([]*interfaceInfo, 0, len(lines))
	for _, line := range lines {
		cols := strings.Split(line, ",")
		// Note: There are exactly 2 column entries as requested in the ovs query
		// Col 0: interface name
		// Col 1: space separated key=val pairs of external_ids attributes
		if len(cols) < 2 {
			// should never happen
			klog.Errorf("Unexpected output: %s, expect \"<name>,<external_ids>\"", line)
			continue
		}

		if cols[1] != "" {
			for _, attr := range strings.Split(cols[1], " ") {
				keyVal := strings.SplitN(attr, "=", 2)
				if len(keyVal) != 2 {
					// should never happen
					klog.Errorf("Unexpected output: %s, expect \"<key>=<value>\"", attr)
					continue
				} else if keyVal[0] == "iface-id-ver" {
					ifcInfo := interfaceInfo{Name: strings.TrimSpace(cols[0]), PodUID: keyVal[1]}
					interfaceInfos = append(interfaceInfos, &ifcInfo)
					break
				}
			}
		}
	}

	if len(interfaceInfos) == 0 {
		return
	}

	// list Pods and calculate the expected iface-ids.
	// Note: we do this after scanning ovs interfaces to avoid deleting ports of pods that where just scheduled
	// on the node.
	pods, err := ncm.watchFactory.GetPods("")
	if err != nil {
		klog.Errorf("Failed to list pods. %v", err)
		return
	}
	expectedPodUIDs := make(map[string]struct{})
	for _, pod := range pods {
		if pod.Spec.NodeName == ncm.name && !util.PodWantsHostNetwork(pod) {
			// Note: wf (WatchFactory) *usually* returns pods assigned to this node, however we dont rely on it
			// and add this check to filter out pods assigned to other nodes. (e.g when ovnkube master and node
			// share the same process)
			expectedPodUIDs[string(pod.UID)] = struct{}{}
		}
	}

	// Remove any stale representor ports
	for _, ifaceInfo := range interfaceInfos {
		if _, ok := expectedPodUIDs[ifaceInfo.PodUID]; !ok {
			klog.Warningf("Found stale OVS Interface %s with iface-id-ver %s, deleting it", ifaceInfo.Name, ifaceInfo.PodUID)
			_, stderr, err := util.RunOVSVsctl("--if-exists", "--with-iface", "del-port", ifaceInfo.Name)
			if err != nil {
				klog.Errorf("Failed to delete interface %q . stderr: %q, error: %v",
					ifaceInfo.Name, stderr, err)
			}
		}
	}
}

// checkForStaleOVSInternalPorts checks for OVS internal ports without any ofport assigned,
// they are stale ports that must be deleted
func checkForStaleOVSInternalPorts() {
	// Track how long scrubbing stale interfaces takes
	start := time.Now()
	defer func() {
		klog.V(5).Infof("CheckForStaleOVSInternalPorts took %v", time.Since(start))
	}()

	stdout, _, err := util.RunOVSVsctl("--data=bare", "--no-headings", "--columns=name", "find",
		"interface", "ofport=-1")
	if err != nil {
		klog.Errorf("Failed to list OVS interfaces with ofport set to -1")
		return
	}
	if len(stdout) == 0 {
		return
	}
	// Batched command length overload shouldn't be a worry here since the number
	// of interfaces per node should never be very large
	// TODO: change this to use libovsdb
	staleInterfaceArgs := []string{}
	values := strings.Split(stdout, "\n\n")
	for _, val := range values {
		if val == ovntypes.K8sMgmtIntfName || val == ovntypes.K8sMgmtIntfName+"_0" {
			klog.Errorf("Management port %s is missing. Perhaps the host rebooted "+
				"or SR-IOV VFs were disabled on the host.", val)
			continue
		}
		klog.Warningf("Found stale interface %s, so queuing it to be deleted", val)
		if len(staleInterfaceArgs) > 0 {
			staleInterfaceArgs = append(staleInterfaceArgs, "--")
		}

		staleInterfaceArgs = append(staleInterfaceArgs, "--if-exists", "--with-iface", "del-port", val)
	}

	// Don't call ovs if all interfaces were skipped in the loop above
	if len(staleInterfaceArgs) == 0 {
		return
	}

	_, stderr, err := util.RunOVSVsctl(staleInterfaceArgs...)
	if err != nil {
		klog.Errorf("Failed to delete OVS port/interfaces: stderr: %s (%v)",
			stderr, err)
	}
}

func (ncm *NodeControllerManager) Reconcile(_ string, _, _ util.NetInfo) error {
	return nil
}
