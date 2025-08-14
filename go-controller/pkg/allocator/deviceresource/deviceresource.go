package deviceresource

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"k8s.io/klog/v2"
)

// Device resource manager, each reserved resource will be owned by a unique owner
type DeviceResourceManager struct {
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

// CreateManagePortResources tries to retrieve all resources associated with the configured management port resource.
func CreateDeviceResourceManager(resourceName string) (*DeviceResourceManager, error) {
	mgmtPortEnvName := getEnvNameFromResourceName(resourceName)
	deviceIds, err := getDeviceIdsFromEnv(mgmtPortEnvName)
	if err != nil {
		return nil, err
	}
	return createDeviceResourceManagerWithDeviceIDs(resourceName, deviceIds)
}

// createManagePortResources tries to retrieve all resources associated with the configured management port resource.
func createDeviceResourceManagerWithDeviceIDs(resourceName string, deviceIds []string) (*DeviceResourceManager, error) {
	if len(deviceIds) == 0 {
		return nil, fmt.Errorf("no device IDs for resource: %s", resourceName)
	}
	drm := &DeviceResourceManager{
		resourceName:     resourceName,
		deviceIds:        deviceIds,
		unUsedDeviceIds:  make(map[string]struct{}),
		deviceIdsByOwner: make(map[string]string),
	}
	for _, deviceID := range deviceIds {
		drm.unUsedDeviceIds[deviceID] = struct{}{}
	}
	return drm, nil
}

func (drm *DeviceResourceManager) GetResourceName() string {
	return drm.resourceName
}

func (drm *DeviceResourceManager) ReserveResourcesDeviceIDByIndex(owner string, index int) (string, error) {
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

func (drm *DeviceResourceManager) ReserveResourcesDeviceIDByDeviceID(owner, deviceId string) error {
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

func (drm *DeviceResourceManager) ReserveResourcesDeviceID(owner string) (string, error) {
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
	}
	drm.deviceIdsByOwner[owner] = deviceId
	klog.V(5).Infof("Reserved device %s of resource %s by %s", deviceId, drm.resourceName, owner)
	return deviceId, nil
}

func (drm *DeviceResourceManager) ReleaseResourcesDeviceID(owner string) error {
	klog.V(5).Infof("Release resource %s by %s", drm.resourceName, owner)
	drm.resourceLock.Lock()
	defer drm.resourceLock.Unlock()

	deviceId, ok := drm.deviceIdsByOwner[owner]
	if !ok {
		return fmt.Errorf("no resource reserved for resource %s by %s, nothing to release", drm.resourceName, owner)
	}
	_, ok = drm.unUsedDeviceIds[deviceId]
	if ok {
		return fmt.Errorf("to be released device %s already in unused device list for resource %s", deviceId, drm.resourceName)
	}
	drm.unUsedDeviceIds[deviceId] = struct{}{}
	klog.V(5).Infof("Released device %s of resource %s by %s", deviceId, drm.resourceName, owner)
	return nil
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
		return nil, fmt.Errorf("unexpected empty env variable: %s", envName)
	}
	deviceIds := strings.Split(envVar, ",")
	return deviceIds, nil
}
