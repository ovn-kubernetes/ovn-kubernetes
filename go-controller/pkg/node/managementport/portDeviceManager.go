package managementport

import (
	"fmt"
	"sync"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/deviceresource"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// MgmtPortDeviceManager manages the mapping between network name and its management port VF device details information
// it calls into device allocator to allocate/release VF device for networks (default network and primary networks)
type MgmtPortDeviceManager struct {
	nodeName     string
	Kube         kube.Interface
	watchFactory factory.NodeWatchFactory

	// manages default and primary network management ports VF allocation,
	// it will be nil if no VF resource is defined
	deviceAllocator *deviceresource.DeviceResourceAllocator
	mgmtPortMutex   sync.Mutex
	mgmtPortDetails util.NetworkDeviceDetailsMap
}

func NewMgmtPortDeviceManager(Kube kube.Interface, wf factory.NodeWatchFactory, nodeName string, deviceAllocator *deviceresource.DeviceResourceAllocator) *MgmtPortDeviceManager {
	return &MgmtPortDeviceManager{
		nodeName:        nodeName,
		Kube:            Kube,
		watchFactory:    wf,
		deviceAllocator: deviceAllocator,
		mgmtPortMutex:   sync.Mutex{},
		mgmtPortDetails: util.NetworkDeviceDetailsMap{},
	}
}

func (mpdm *MgmtPortDeviceManager) Init() error {
	var annotationNeedUpdate bool

	node, err := mpdm.watchFactory.GetNode(mpdm.nodeName)
	if err != nil {
		return fmt.Errorf("failed to get node %s: %v", mpdm.nodeName, err)
	}

	mpdm.mgmtPortMutex.Lock()
	defer mpdm.mgmtPortMutex.Unlock()
	annotatedMgmtPortDetailsMap, err := util.ParseNodeManagementPortAnnotation(node)
	if err != nil {
		if !util.IsAnnotationNotSetError(err) {
			return fmt.Errorf("failed to parse node network management port annotation %q: %v",
				node.Annotations, err)
		}
		annotatedMgmtPortDetailsMap = util.NetworkDeviceDetailsMap{}
	}

	// validate the existing management port reservations:
	for network, annotatedMgmtPortDetails := range annotatedMgmtPortDetailsMap {
		deviceId := annotatedMgmtPortDetails.DeviceId
		if deviceId == "" {
			// this must be legacyManagementPortDetails annotation for default network, try to find its deviceId.
			// luckily this is one time thing
			allDeviceIDs := mpdm.deviceAllocator.DeviceIDs()
			for _, d := range allDeviceIDs {
				mgmtDetails, err := util.GetNetworkDeviceDetails(d)
				if err == nil && mgmtDetails.PfId == annotatedMgmtPortDetails.PfId && mgmtDetails.FuncId == annotatedMgmtPortDetails.FuncId {
					deviceId = d
					break
				}
			}
			if deviceId == "" {
				return fmt.Errorf("failed to find match manage port device %v of resource %s for network %s",
					annotatedMgmtPortDetails, mpdm.deviceAllocator.ResourceName(), network)
			}
			err = mpdm.deviceAllocator.ReserveResourcesDeviceIDByDeviceID(network, deviceId)
			if err != nil {
				return fmt.Errorf("failed to reserve manage port device %v of resource %s for network %s: %v",
					deviceId, mpdm.deviceAllocator.ResourceName(), network, err)
			}
			annotatedMgmtPortDetails.DeviceId = deviceId
			annotationNeedUpdate = true
		} else {
			err = mpdm.deviceAllocator.ReserveResourcesDeviceIDByDeviceID(network, deviceId)
			if err != nil {
				return fmt.Errorf("failed to reserve manage port device %v of resource %s for network %s: %v",
					deviceId, mpdm.deviceAllocator.ResourceName(), network, err)
			}
			curMgmtPortDetails, err := util.GetNetworkDeviceDetails(deviceId)
			if err != nil {
				return fmt.Errorf("failed to get network manage port device details for device %s network %s: %v", deviceId, network, err)
			}
			if annotatedMgmtPortDetails.FuncId != curMgmtPortDetails.FuncId || annotatedMgmtPortDetails.PfId != curMgmtPortDetails.PfId {
				return fmt.Errorf("mismatched management port details for network %s. Annotated: %v, Current: %v", network, annotatedMgmtPortDetails, curMgmtPortDetails)
			}
		}
	}
	if annotationNeedUpdate {
		err = util.UpdateNodeManagementPortAnnotation(mpdm.Kube, mpdm.nodeName, annotatedMgmtPortDetailsMap)
		if err != nil {
			return fmt.Errorf("failed to update node management port annotation: %v", err)
		}
	}
	mpdm.mgmtPortDetails = annotatedMgmtPortDetailsMap
	klog.V(5).Infof("Initializing management port device details %v", annotatedMgmtPortDetailsMap)
	return nil
}

func (mpdm *MgmtPortDeviceManager) SyncManagementPorts(validNetworks ...util.NetInfo) error {
	mpdm.mgmtPortMutex.Lock()
	defer mpdm.mgmtPortMutex.Unlock()
	needUpdate := false
	validPrimaryNetwork := make(map[string]interface{})
	for _, validNetwork := range validNetworks {
		if util.IsNetworkSegmentationSupportEnabled() && validNetwork.IsPrimaryNetwork() {
			validPrimaryNetwork[validNetwork.GetNetworkName()] = nil
		}
	}
	for network := range mpdm.mgmtPortDetails {
		if network == ovntypes.DefaultNetworkName {
			continue
		}
		if _, ok := validPrimaryNetwork[network]; !ok {
			klog.V(5).Infof("Release management port device reserved for stale network %s deleted during reboot", network)
			mpdm.deviceAllocator.ReleaseResourcesDeviceID(network)
			delete(mpdm.mgmtPortDetails, network)
			needUpdate = true
		}
	}
	if needUpdate {
		err := util.UpdateNodeManagementPortAnnotation(mpdm.Kube, mpdm.nodeName, mpdm.mgmtPortDetails)
		if err != nil {
			return fmt.Errorf("failed to update management port devices annotation after deleting stale network node management port %v", err)
		}
	}
	return nil
}

func (mpdm *MgmtPortDeviceManager) AllocateDeviceIDForNetwork(network string) error {
	var err error
	var deviceId string

	mpdm.mgmtPortMutex.Lock()
	defer mpdm.mgmtPortMutex.Unlock()

	mgmtPortDetails, ok := mpdm.mgmtPortDetails[network]
	if !ok {
		deviceId, err = mpdm.deviceAllocator.ReserveResourcesDeviceID(network)
		if err != nil {
			return fmt.Errorf("failed to get manage port device of resource %s for network %s: %v",
				mpdm.deviceAllocator.ResourceName(), network, err)
		}
		mgmtPortDetails, err = util.GetNetworkDeviceDetails(deviceId)
		if err != nil {
			mpdm.deviceAllocator.ReleaseResourcesDeviceID(network)
			return fmt.Errorf("failed to get network manage port device details for device %s: %v", deviceId, err)
		}
		mpdm.mgmtPortDetails[network] = mgmtPortDetails
		err = util.UpdateNodeManagementPortAnnotation(mpdm.Kube, mpdm.nodeName, mpdm.mgmtPortDetails)
		if err != nil {
			mpdm.deviceAllocator.ReleaseResourcesDeviceID(network)
			delete(mpdm.mgmtPortDetails, network)
			return fmt.Errorf("failed to update node management port annotation: %v", err)
		}
		klog.V(5).Infof("Management port device %v allocated for network %s", mgmtPortDetails, network)
	} else {
		klog.V(5).Infof("Management port device %v already allocated for network %s", mgmtPortDetails, network)
	}
	return nil
}

func (mpdm *MgmtPortDeviceManager) AllocateDeviceIDForDefaultNetwork() (*util.NetworkDeviceDetails, error) {
	mpdm.mgmtPortMutex.Lock()
	defer mpdm.mgmtPortMutex.Unlock()
	mgmtPortDetails, ok := mpdm.mgmtPortDetails[ovntypes.DefaultNetworkName]
	if !ok {
		deviceId, err := mpdm.deviceAllocator.ReserveResourcesDeviceIDByIndex(ovntypes.DefaultNetworkName, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to get manage port device of resource %s for default network: %v",
				mpdm.deviceAllocator.ResourceName(), err)
		}
		mgmtPortDetails, err = util.GetNetworkDeviceDetails(deviceId)
		if err != nil {
			mpdm.deviceAllocator.ReleaseResourcesDeviceID(ovntypes.DefaultNetworkName)
			return nil, fmt.Errorf("failed to get network manage port device details for device %s: %v", deviceId, err)
		}
		mpdm.mgmtPortDetails[ovntypes.DefaultNetworkName] = mgmtPortDetails
		err = util.UpdateNodeManagementPortAnnotation(mpdm.Kube, mpdm.nodeName, mpdm.mgmtPortDetails)
		if err != nil {
			mpdm.deviceAllocator.ReleaseResourcesDeviceID(ovntypes.DefaultNetworkName)
			delete(mpdm.mgmtPortDetails, ovntypes.DefaultNetworkName)
			return nil, fmt.Errorf("failed to update node management port annotation: %v", err)
		}
		klog.V(5).Infof("Management port device %v allocated for default network", mgmtPortDetails)
	} else {
		klog.V(5).Infof("Management port device %v already allocated for default network", mgmtPortDetails)
	}
	return mgmtPortDetails, nil
}

func (mpdm *MgmtPortDeviceManager) ReleaseDeviceIDForNetwork(network string) error {
	mpdm.mgmtPortMutex.Lock()
	defer mpdm.mgmtPortMutex.Unlock()

	klog.V(5).Infof("Release management device allocated for network %s", network)
	mgmtPortDetails, ok := mpdm.mgmtPortDetails[network]
	if ok {
		delete(mpdm.mgmtPortDetails, network)
		err := util.UpdateNodeManagementPortAnnotation(mpdm.Kube, mpdm.nodeName, mpdm.mgmtPortDetails)
		if err != nil {
			mpdm.mgmtPortDetails[network] = mgmtPortDetails
			return fmt.Errorf("error updating node management port annotation for network %s: %v", network, err)
		}
	}
	mpdm.deviceAllocator.ReleaseResourcesDeviceID(network)
	return nil
}
