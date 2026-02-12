package deviceresource

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
)

// there is a case that even resource name is defined, it is not associated with any real device,
// ErrResourceNotDefined is to indicate this error
var ErrResourceNotDefined = errors.New("resource not defined")

// Device resource manager, each reserved resource will be owned by a unique owner
type DeviceResourceAllocator struct {
	resourceLock sync.RWMutex

	resourceName string
	// all deviceIDs allocated for this resource
	deviceIds []string
	// maps used for resource management
	// unUsedDeviceIds: map of devices that are not used, key is the deviceID, val is not used
	unUsedDeviceIds map[string]struct{}
	// deviceIdsByOwner: map of devices that are reserved,
	// key is the owner of the resource, and the value is the resource's deviceId
	deviceIdsByOwner map[string]string
}

type deviceResourcesManager struct {
	// key is resource name, value is DeviceResourceAllocator
	resourceAllocators *syncmap.SyncMap[*DeviceResourceAllocator]
}

var drManager *deviceResourcesManager

func init() {
	drManager = &deviceResourcesManager{
		resourceAllocators: syncmap.NewSyncMap[*DeviceResourceAllocator](),
	}
}

func DeviceResourceManager() *deviceResourcesManager {
	return drManager
}

// GetDeviceResourceAllocator get allocator to allocator/release devices for specified resource.
func (drm *deviceResourcesManager) GetDeviceResourceAllocator(resourceName string) (*DeviceResourceAllocator, error) {
	drm.resourceAllocators.LockKey(resourceName)
	defer drm.resourceAllocators.UnlockKey(resourceName)
	dra, ok := drm.resourceAllocators.Load(resourceName)
	if ok {
		return dra, nil
	}
	mgmtPortEnvName := getEnvNameFromResourceName(resourceName)
	deviceIds, err := getDeviceIdsFromEnv(mgmtPortEnvName)
	if err != nil {
		return nil, err
	}
	if len(deviceIds) == 0 {
		return nil, fmt.Errorf("no device IDs for resource: %s", resourceName)
	}
	dra = &DeviceResourceAllocator{
		resourceName:     resourceName,
		deviceIds:        deviceIds,
		unUsedDeviceIds:  make(map[string]struct{}),
		deviceIdsByOwner: make(map[string]string),
	}
	for _, deviceID := range deviceIds {
		dra.unUsedDeviceIds[deviceID] = struct{}{}
	}
	drm.resourceAllocators.Store(resourceName, dra)
	return dra, nil
}

func (drm *DeviceResourceAllocator) ResourceName() string {
	return drm.resourceName
}

func (drm *DeviceResourceAllocator) DeviceIDs() []string {
	drm.resourceLock.RLock()
	defer drm.resourceLock.RUnlock()

	ids := make([]string, len(drm.deviceIds))
	copy(ids, drm.deviceIds)
	return ids
}

func (drm *DeviceResourceAllocator) ReserveResourcesDeviceIDByIndex(owner string, index int) (string, error) {
	klog.V(5).Infof("Reserve resource %s by %s", drm.resourceName, owner)
	if index < 0 || index >= len(drm.deviceIds) {
		return "", fmt.Errorf("index %d is out of range for resource %s", index, drm.resourceName)
	}
	drm.resourceLock.Lock()
	defer drm.resourceLock.Unlock()
	deviceId := drm.deviceIds[index]
	reservedDeviceId, ok := drm.deviceIdsByOwner[owner]
	if ok {
		if reservedDeviceId != deviceId {
			return "", fmt.Errorf("owner %s has already reserved a different device %s for resource %s, expected %s",
				owner, reservedDeviceId, drm.resourceName, deviceId)
		}
		klog.V(5).Infof("Device %s of index %v of resource %s is already reserved by %s",
			deviceId, index, drm.resourceName, owner)
		return deviceId, nil
	}

	if _, ok := drm.unUsedDeviceIds[deviceId]; !ok {
		return "", fmt.Errorf("device %s of index %v of resource %s is already reserved", deviceId, index, drm.resourceName)
	}
	delete(drm.unUsedDeviceIds, deviceId)
	drm.deviceIdsByOwner[owner] = deviceId
	klog.V(5).Infof("Reserved device %s of resource %s by %s", deviceId, drm.resourceName, owner)
	return deviceId, nil
}

func (drm *DeviceResourceAllocator) ReserveResourcesDeviceIDByDeviceID(owner, deviceId string) error {
	klog.V(5).Infof("Reserve resource %s by %s", drm.resourceName, owner)
	drm.resourceLock.Lock()
	defer drm.resourceLock.Unlock()

	reservedDeviceId, ok := drm.deviceIdsByOwner[owner]
	if ok {
		if reservedDeviceId != deviceId {
			return fmt.Errorf("owner %s has already reserved a different device %s for resource %s, expected %s",
				owner, reservedDeviceId, drm.resourceName, deviceId)
		}
		klog.V(5).Infof("Device %s of resource %s is already reserved by %s",
			deviceId, drm.resourceName, owner)
		return nil
	}
	if len(drm.unUsedDeviceIds) == 0 {
		return fmt.Errorf("insufficient device IDs for resource: %s", drm.resourceName)
	}

	// get one from the unused map which can be later add to the deviceIdsByOwnerOfResource map
	if _, ok := drm.unUsedDeviceIds[deviceId]; !ok {
		return fmt.Errorf("requested device ID %s for resource %s not available", deviceId, drm.resourceName)
	}
	delete(drm.unUsedDeviceIds, deviceId)
	drm.deviceIdsByOwner[owner] = deviceId
	klog.V(5).Infof("Reserved device %s of resource %s by %s", deviceId, drm.resourceName, owner)
	return nil
}

func (drm *DeviceResourceAllocator) ReserveResourcesDeviceID(owner string) (string, error) {
	klog.V(5).Infof("Reserve resource %s by %s", drm.resourceName, owner)
	drm.resourceLock.Lock()
	defer drm.resourceLock.Unlock()

	deviceId, ok := drm.deviceIdsByOwner[owner]
	if ok {
		klog.V(5).Infof("Device %s of resource %s is already reserved by %s", deviceId, drm.resourceName, owner)
		return deviceId, nil
	}

	if len(drm.unUsedDeviceIds) == 0 {
		return "", fmt.Errorf("insufficient device IDs for resource: %s", drm.resourceName)
	}

	// get one from the unused map which can be later add to the deviceIdsByOwnerOfResource map
	for deviceId = range drm.unUsedDeviceIds {
		delete(drm.unUsedDeviceIds, deviceId)
		break
	}
	drm.deviceIdsByOwner[owner] = deviceId
	klog.V(5).Infof("Reserved device %s of resource %s by %s", deviceId, drm.resourceName, owner)
	return deviceId, nil
}

func (drm *DeviceResourceAllocator) ReleaseResourcesDeviceID(owner string) {
	klog.V(5).Infof("Release resource %s by %s", drm.resourceName, owner)
	drm.resourceLock.Lock()
	defer drm.resourceLock.Unlock()

	deviceId, ok := drm.deviceIdsByOwner[owner]
	if !ok {
		klog.Warningf("No resource reserved for resource %s by %s, nothing to release", drm.resourceName, owner)
		return
	}
	_, ok = drm.unUsedDeviceIds[deviceId]
	if ok {
		klog.Errorf("To be released device %s already in unused device list for resource %s", deviceId, drm.resourceName)
		delete(drm.deviceIdsByOwner, owner)
		return
	}
	drm.unUsedDeviceIds[deviceId] = struct{}{}
	delete(drm.deviceIdsByOwner, owner)
	klog.V(5).Infof("Released device %s of resource %s by %s", deviceId, drm.resourceName, owner)
}

// getEnvNameFromResourceName gets the device plugin env variable from the device plugin resource name.
func getEnvNameFromResourceName(resource string) string {
	res1 := strings.ReplaceAll(resource, ".", "_")
	res2 := strings.ReplaceAll(res1, "/", "_")
	return "PCIDEVICE_" + strings.ToUpper(res2)
}

// getDeviceIdsFromEnv gets the list of device IDs from the device plugin env variable.
func getDeviceIdsFromEnv(envName string) ([]string, error) {
	envVar := os.Getenv(envName)
	if len(envVar) == 0 {
		return nil, ErrResourceNotDefined
	}
	raw := strings.Split(envVar, ",")
	deviceIds := make([]string, 0, len(raw))
	for _, s := range raw {
		deviceId := strings.TrimSpace(s)
		if deviceId != "" {
			deviceIds = append(deviceIds, deviceId)
		}
	}
	return deviceIds, nil
}
