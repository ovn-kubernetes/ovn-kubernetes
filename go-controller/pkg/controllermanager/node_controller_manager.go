package controllermanager

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
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
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/managementport"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// NodeControllerManager structure is the object manages all controllers for all networks for ovnkube-node
type NodeControllerManager struct {
	name          string
	node          *corev1.Node
	ovnNodeClient *util.OVNNodeClientset
	Kube          kube.Interface
	watchFactory  factory.NodeWatchFactory
	stopChan      chan struct{}
	wg            *sync.WaitGroup
	recorder      record.EventRecorder
	nodeAnnotator kube.Annotator

	// manages default and primary network management ports
	mgmtPortDeviceManager *deviceresource.DeviceResourceManager
	mgmtPortMutex         sync.RWMutex
	mgmtPortDetails       map[string]*util.NetworkDeviceDetails

	defaultNodeNetworkController *node.DefaultNodeNetworkController

	// networkManager creates and deletes secondary network controllers
	networkManager networkmanager.Controller
	// vrf manager that creates and manages vrfs for all UDNs
	vrfManager *vrfmanager.Controller
	// route manager that creates and manages routes
	routeManager *routemanager.Controller
	// iprule manager that creates and manages iprules for all UDNs
	ruleManager *iprulemanager.Controller
	// ovs client that allows to read ovs info
	ovsClient client.Client
}

// NewNetworkController create secondary node network controllers for the given NetInfo
func (ncm *NodeControllerManager) NewNetworkController(nInfo util.NetInfo) (networkmanager.NetworkController, error) {
	topoType := nInfo.TopologyType()
	switch topoType {
	case ovntypes.Layer3Topology, ovntypes.Layer2Topology, ovntypes.LocalnetTopology:
		if ncm.mgmtPortDeviceManager != nil && nInfo.IsPrimaryNetwork() {
			ncm.mgmtPortMutex.Lock()
			_, ok := ncm.mgmtPortDetails[nInfo.GetNetworkName()]
			if !ok {
				deviceId, err := ncm.mgmtPortDeviceManager.ReserveResourcesDeviceID(nInfo.GetNetworkName())
				if err != nil {
					return nil, fmt.Errorf("failed to get manage port device of resource %s for network %s: %v",
						ncm.mgmtPortDeviceManager.GetResourceName(), nInfo.GetNetworkName(), err)
				}
				mgmtPortDetails, err := util.GetNetworkDeviceDetails(deviceId)
				if err != nil {
					return nil, fmt.Errorf("failed to get network manage port device details for device %s: %v", deviceId, err)
				}
				ncm.mgmtPortDetails[nInfo.GetNetworkName()] = mgmtPortDetails
				err = util.UpdateNodeManagementPortAnnotation(ncm.nodeAnnotator, ncm.mgmtPortDetails)
				if err != nil {
					return nil, fmt.Errorf("failed to update node management port annotation: %v", err)
				}
			}
		}

		// Pass a shallow clone of the watch factory, this allows multiplexing
		// informers for secondary networks.
		return node.NewSecondaryNodeNetworkController(ncm.newCommonNetworkControllerInfo(ncm.watchFactory.(*factory.WatchFactory).ShallowClone()),
			nInfo, ncm.networkManager.Interface(), ncm.vrfManager, ncm.ruleManager, ncm.defaultNodeNetworkController.Gateway)
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

func (ncm *NodeControllerManager) GetDefaultNetworkController() networkmanager.ReconcilableNetworkController {
	return ncm.defaultNodeNetworkController
}

// syncManagementPorts deletes stale management port entities for networks that deleted during reboot.
func (ncm *NodeControllerManager) syncManagementPorts(validNetworks ...util.NetInfo) {
	// Delete management ports for delete networks
	// first try to find all expected primary UDN mgmtPortIfName (types.K8sMgmtIntfNamePrefix + <networkID>)
	expectedMgmtPortIfNames := map[string]struct{}{}
	for _, network := range validNetworks {
		if !network.IsPrimaryNetwork() {
			continue
		}
		expectedMgmtPortIfNames[util.GetNetworkScopedK8sMgmtHostIntfName(uint(network.GetNetworkID()))] = struct{}{}
	}
	// add management port name for default networks to expected name map
	expectedMgmtPortIfNames[ovntypes.K8sMgmtIntfName] = struct{}{}
	expectedMgmtPortIfNames[ovntypes.K8sMgmtIntfName+"_0"] = struct{}{}

	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPUHost {
		// then get all existing management ports for primary UDNs
		// internal management port map, key is managementPortIfName (types.K8sMgmtIntfNamePrefix + <networkID>), value is management port OVS interface name
		internalMgmtPorts := make(map[string]string)

		// representor management port map, key is managementPortIfName (types.K8sMgmtIntfNamePrefix + <networkID>), value is management port OVS interface name and network name
		type repInfo struct {
			name    string // name of OVS interface name
			netName string // network name
		}
		repMgmtPorts := make(map[string]repInfo)

		// first internal management port OVS interface
		stdout, _, _ := util.RunOVSVsctl("--no-headings",
			"--data", "bare",
			"--format", "csv",
			"--columns", "name",
			"find", "Interface", "type=internal")
		if stdout != "" {
			mgmtPortNames := strings.Split(stdout, "\n")
			for _, mgmtPortIfName := range mgmtPortNames {
				if strings.HasPrefix(mgmtPortIfName, ovntypes.K8sMgmtIntfNamePrefix) {
					internalMgmtPorts[mgmtPortIfName] = mgmtPortIfName
				}
			}
		}
		// then management port OVS interface for representor
		stdout, _, _ = util.RunOVSVsctl("--no-headings",
			"--data", "bare",
			"--format", "csv",
			"--columns", "name,external_ids",
			"find", "Interface", fmt.Sprintf("external_ids:%s!=\"\"", ovntypes.OvnManagementPortNameExternalId))
		if stdout != "" {
			nameAndAllExternalIDs := strings.Split(stdout, "\n")
			for _, nameAndAllExternalID := range nameAndAllExternalIDs {
				fields := strings.Split(nameAndAllExternalID, ",")
				name := fields[0]
				externalIDs := fields[1]
				repMgmtPorts[util.GetExternalIDValByKey(externalIDs, ovntypes.OvnManagementPortNameExternalId)] =
					repInfo{name: name, netName: util.GetExternalIDValByKey(externalIDs, ovntypes.NetworkExternalID)}
			}
		}

		// delete stale internal management port OVS interface
		for mgmtPortIfName, mgmtPortOVSIfName := range internalMgmtPorts {
			if _, ok := expectedMgmtPortIfNames[mgmtPortIfName]; !ok {
				stdout, stderr, err := util.RunOVSVsctl("--", "--if-exists", "del-port", "br-int", mgmtPortOVSIfName)
				if err != nil {
					klog.Errorf("Failed to delete port %s from br-int, stdout: %q, stderr: %q, error: %v",
						mgmtPortOVSIfName, stdout, stderr, err)
				}
			}
		}

		// delete stale representor management port OVS interface
		for mgmtPortIfName, repInfo := range repMgmtPorts {
			if _, ok := expectedMgmtPortIfNames[mgmtPortIfName]; !ok {
				err := managementport.DeleteManagementPortOVSInterface(repInfo.netName, mgmtPortIfName)
				if err != nil {
					klog.Error(err.Error())
				}
			}
			link, err := util.GetNetLinkOps().LinkByName(repInfo.name)
			if err == nil {
				err = managementport.TearDownManagementPortLink(repInfo.netName, link, "")
				if err != nil {
					klog.Error(err.Error())
				}
			}
		}
	}

	// cleanup stale management port netdev
	if config.OvnKubeNode.Mode != ovntypes.NodeModeDPU {
		links, err := util.GetNetLinkOps().LinkList()
		if err != nil {
			for _, link := range links {
				linkName := link.Attrs().Name
				if !strings.HasPrefix(linkName, ovntypes.K8sMgmtIntfNamePrefix) {
					continue
				}
				if _, ok := expectedMgmtPortIfNames[linkName]; ok {
					continue
				}
				err = managementport.TearDownManagementPortLink("unknownNetwork", link, "")
				if err != nil {
					klog.Error(err.Error())
				}
			}
		}
	}

	// Record the list of networks that requires management ports. This is used to delete
	// stale management port reservation during reboot. We cannot get current management port details yet
	// from the annotation as watch factory is not started
	if config.OvnKubeNode.MgmtPortDPResourceName == "" || config.OvnKubeNode.Mode == ovntypes.NodeModeDPU {
		return
	}
	// initialization, no lock needed, just set the map key to indicate all potential valid networks needs management port
	// device allocation after reboot
	for _, validNetwork := range validNetworks {
		if validNetwork.IsDefault() || (util.IsNetworkSegmentationSupportEnabled() && validNetwork.IsPrimaryNetwork()) {
			ncm.mgmtPortDetails[validNetwork.GetNetworkName()] = nil
		}
	}
}

// CleanupStaleNetworks cleans up all stale entities giving list of all existing secondary network controllers
func (ncm *NodeControllerManager) CleanupStaleNetworks(validNetworks ...util.NetInfo) error {
	if !util.IsNetworkSegmentationSupportEnabled() {
		return nil
	}

	ncm.syncManagementPorts(validNetworks...)

	validVRFDevices := make(sets.Set[string])
	for _, network := range validNetworks {
		if !network.IsPrimaryNetwork() {
			continue
		}
		validVRFDevices.Insert(util.GetNetworkVRFName(network))
	}
	return ncm.vrfManager.Repair(validVRFDevices)
}

// newCommonNetworkControllerInfo creates and returns the base node network controller info
func (ncm *NodeControllerManager) newCommonNetworkControllerInfo(wf factory.NodeWatchFactory) *node.CommonNodeNetworkControllerInfo {
	return node.NewCommonNodeNetworkControllerInfo(ncm.ovnNodeClient.KubeClient, ncm.ovnNodeClient.AdminPolicyRouteClient, wf, ncm.recorder, ncm.name, ncm.routeManager)
}

// isNetworkManagerRequiredForNode checks if network manager should be started
// on the node side, which requires any of the following conditions:
// (1) dpu mode is enabled when secondary networks feature is enabled
// (2) primary user defined networks is enabled (all modes)
func isNetworkManagerRequiredForNode() bool {
	return (config.OVNKubernetesFeature.EnableMultiNetwork && config.OvnKubeNode.Mode == ovntypes.NodeModeDPU) ||
		util.IsNetworkSegmentationSupportEnabled() ||
		util.IsRouteAdvertisementsEnabled()
}

// NewNodeControllerManager creates a new OVN controller manager to manage all the controller for all networks
func NewNodeControllerManager(ovnClient *util.OVNClientset, wf factory.NodeWatchFactory, name string,
	wg *sync.WaitGroup, eventRecorder record.EventRecorder, routeManager *routemanager.Controller, ovsClient client.Client) (*NodeControllerManager, error) {
	ncm := &NodeControllerManager{
		name:            name,
		ovnNodeClient:   &util.OVNNodeClientset{KubeClient: ovnClient.KubeClient, AdminPolicyRouteClient: ovnClient.AdminPolicyRouteClient},
		Kube:            &kube.Kube{KClient: ovnClient.KubeClient},
		watchFactory:    wf,
		stopChan:        make(chan struct{}),
		wg:              wg,
		recorder:        eventRecorder,
		routeManager:    routeManager,
		ovsClient:       ovsClient,
		mgmtPortMutex:   sync.RWMutex{},
		mgmtPortDetails: map[string]*util.NetworkDeviceDetails{},
	}

	// need to configure OVS interfaces for Pods on secondary networks in the DPU mode
	// need to start NAD controller on node side for programming gateway pieces for UDNs
	// need to start NAD controller on node side for VRF awareness with BGP
	var err error
	ncm.networkManager = networkmanager.Default()
	ncm.nodeAnnotator = kube.NewNodeAnnotator(ncm.Kube, name)

	if isNetworkManagerRequiredForNode() {
		ncm.networkManager, err = networkmanager.NewForNode(name, ncm, wf)
		if err != nil {
			return nil, err
		}
	}
	if util.IsNetworkSegmentationSupportEnabled() {
		ncm.vrfManager = vrfmanager.NewController(ncm.routeManager)
		ncm.ruleManager = iprulemanager.NewController(config.IPv4Mode, config.IPv6Mode)
	}
	return ncm, nil
}

// initDefaultNodeNetworkController creates the controller for default network
func (ncm *NodeControllerManager) initDefaultNodeNetworkController(ctx context.Context) error {
	if ncm.mgmtPortDeviceManager != nil {
		ncm.mgmtPortMutex.Lock()
		mgmtPortDetails, ok := ncm.mgmtPortDetails[ovntypes.DefaultNetworkName]
		if !ok {
			deviceId, err := ncm.mgmtPortDeviceManager.ReserveResourcesDeviceIDByIndex(ovntypes.DefaultNetworkName, 0)
			if err != nil {
				return fmt.Errorf("failed to get manage port device of resource %s for default network: %v",
					ncm.mgmtPortDeviceManager.GetResourceName(), err)
			}
			mgmtPortDetails, err = util.GetNetworkDeviceDetails(deviceId)
			if err != nil {
				return fmt.Errorf("failed to get network manage port device details for device %s: %v", deviceId, err)
			}
			ncm.mgmtPortDetails[ovntypes.DefaultNetworkName] = mgmtPortDetails
			err = util.UpdateNodeManagementPortAnnotation(ncm.nodeAnnotator, ncm.mgmtPortDetails)
			if err != nil {
				return fmt.Errorf("failed to update node management port annotation: %v", err)
			}
		}
		netdevice, err := util.GetNetdevNameFromDeviceId(mgmtPortDetails.DeviceId, v1.DeviceInfo{})
		if err != nil {
			return fmt.Errorf("failed to get netdev name for device %s allocated for default network: %v", mgmtPortDetails.DeviceId, err)
		}

		if config.OvnKubeNode.MgmtPortNetdev != "" && config.OvnKubeNode.MgmtPortNetdev != netdevice {
			klog.Warningf("MgmtPortNetdev is set explicitly (%s), overriding with resource...",
				config.OvnKubeNode.MgmtPortNetdev)
		}
		config.OvnKubeNode.MgmtPortNetdev = netdevice
		klog.V(5).Infof("Using MgmtPortNetdev (Netdev %s) passed via resource %s",
			config.OvnKubeNode.MgmtPortNetdev, ncm.mgmtPortDeviceManager.GetResourceName())
		ncm.mgmtPortMutex.Unlock()
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

// initMgmtPortDeviceManager initializes management port device manager and validate and reserve management port devices
// for all existing default/primary networks
func (ncm *NodeControllerManager) initMgmtPortDeviceManager() error {
	var annotationNeedUpdate bool
	var err error

	if config.OvnKubeNode.MgmtPortDPResourceName == "" || config.OvnKubeNode.Mode == ovntypes.NodeModeDPU {
		return nil
	}
	ncm.mgmtPortDeviceManager, err = deviceresource.CreateDeviceResourceManager(config.OvnKubeNode.MgmtPortDPResourceName)
	if err != nil {
		return fmt.Errorf("failed to create manage port resources manager for resource %s: %v",
			config.OvnKubeNode.MgmtPortDPResourceName, err)
	}

	node, err := ncm.watchFactory.GetNode(ncm.name)
	if err != nil {
		return err
	}
	ncm.node = node

	// initialization phase, no need to hold lock to access ncm.mgmtPortDetails
	annotatedMgmtPortDetailsMap, err := util.ParseNodeManagementPortAnnotation(ncm.node)
	if err != nil {
		if !util.IsAnnotationNotSetError(err) {
			return fmt.Errorf("failed to parse node network management port annotation %q: %v",
				node.Annotations, err)
		}
	} else {
		for network := range annotatedMgmtPortDetailsMap {
			if _, ok := ncm.mgmtPortDetails[network]; !ok {
				// network has already been deleted during reboot
				delete(annotatedMgmtPortDetailsMap, network)
				annotationNeedUpdate = true
			}
		}
	}

	// validate the existing management port reservations:
	for network, annotatedMgmtPortDetails := range annotatedMgmtPortDetailsMap {
		var deviceId string
		// default network, for backward compatibilty, always reserve the first device
		if network == ovntypes.DefaultNetworkName {
			deviceId, err = ncm.mgmtPortDeviceManager.ReserveResourcesDeviceIDByIndex(network, 0)
			if annotatedMgmtPortDetails.DeviceId == "" {
				// legacy annotation
				annotatedMgmtPortDetails.DeviceId = deviceId
				annotationNeedUpdate = true
			}
		} else {
			deviceId = annotatedMgmtPortDetails.DeviceId
			err = ncm.mgmtPortDeviceManager.ReserveResourcesDeviceIDByDeviceID(network, deviceId)
		}
		if err != nil {
			return fmt.Errorf("failed to reserve manage port device of resource %s for network %s: %v",
				ncm.mgmtPortDeviceManager.GetResourceName(), network, err)
		}
		curMgmtPortDetails, err := util.GetNetworkDeviceDetails(deviceId)
		if err != nil {
			return fmt.Errorf("failed to get network manage port device details for device %s network %s: %v", deviceId, network, err)
		}
		if annotatedMgmtPortDetails.FuncId != curMgmtPortDetails.FuncId || annotatedMgmtPortDetails.PfId != curMgmtPortDetails.PfId {
			return fmt.Errorf("mismatched management port details for network %s. Annotated: %v, Current: %v", network, annotatedMgmtPortDetails, curMgmtPortDetails)
		}
	}
	if annotationNeedUpdate {
		err = util.UpdateNodeManagementPortAnnotation(ncm.nodeAnnotator, annotatedMgmtPortDetailsMap)
		if err != nil {
			return fmt.Errorf("failed to update node management port annotation: %v", err)
		}
	}
	return nil
}

// Start the node network controller manager
func (ncm *NodeControllerManager) Start(ctx context.Context) (err error) {
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
			ncm.Stop()
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

	err = ncm.initMgmtPortDeviceManager()
	if err != nil {
		return fmt.Errorf("failed to init management port device manager: %v", err)
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
	return nil
}

// Stop gracefully stops all managed controllers
func (ncm *NodeControllerManager) Stop() {
	// stop stale ovs ports cleanup
	close(ncm.stopChan)

	if ncm.defaultNodeNetworkController != nil {
		ncm.defaultNodeNetworkController.Stop()
	}

	// stop the NAD controller
	if ncm.networkManager != nil {
		ncm.networkManager.Stop()
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
