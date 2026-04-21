// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dra

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/vishvananda/netlink"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
)

// PrepareResourceClaims implements the DRA prepare callback.
func (k *NetworkDriver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	results := make(map[types.UID]kubeletplugin.PrepareResult)
	for _, claim := range claims {
		preparedData, err := k.prepareDevice(ctx, claim)
		if err != nil {
			results[claim.UID] = kubeletplugin.PrepareResult{Err: err}
			continue
		}
		k.mu.Lock()
		k.sharedState.PreparedData[claim.UID] = preparedData
		k.mu.Unlock()
		results[claim.UID] = kubeletplugin.PrepareResult{}
	}
	return results, nil
}

// UnprepareResourceClaims implements the DRA unprepare callback.
func (k *NetworkDriver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	errs := make(map[types.UID]error)
	for _, claim := range claims {
		if err := k.unprepareDevice(ctx, claim); err != nil {
			errs[claim.UID] = err
		}
		k.mu.Lock()
		delete(k.sharedState.PreparedData, claim.UID)
		k.mu.Unlock()
	}
	return errs, nil
}

// HandleError is called for background errors.
func (k *NetworkDriver) HandleError(_ context.Context, err error, msg string) {
	runtime.HandleError(fmt.Errorf("%s: %w", msg, err))
}

func (k *NetworkDriver) prepareDevice(_ context.Context, claim *resourceapi.ResourceClaim) (interface{}, error) {
	if claim.Status.Allocation == nil || len(claim.Status.Allocation.Devices.Results) == 0 {
		return nil, fmt.Errorf("claim %s has no allocated devices", claim.Name)
	}

	return claim.Status.Allocation.Devices.Results[0].Device, nil
}

func (k *NetworkDriver) unprepareDevice(_ context.Context, _ kubeletplugin.NamespacedObject) error {
	return nil
}

func (k *NetworkDriver) publishResources(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			devices, err := k.getDevices()
			if err != nil {
				klog.Errorf("Failed to get devices: %v", err)
				continue
			}
			resources := resourceslice.DriverResources{
				Pools: map[string]resourceslice.Pool{
					k.nodeName: {Slices: []resourceslice.Slice{{Devices: devices}}},
				},
			}
			if err := k.draPlugin.PublishResources(ctx, resources); err != nil {
				klog.Errorf("Failed to publish resources: %v", err)
			}
		}
	}
}

func (k *NetworkDriver) getDevices() ([]resourceapi.Device, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list network interfaces: %w", err)
	}

	var devices []resourceapi.Device
	for _, link := range links {
		attrs := link.Attrs()

		if attrs.Flags&net.FlagLoopback != 0 || attrs.Flags&net.FlagUp == 0 {
			continue
		}
		if strings.HasPrefix(attrs.Name, "veth") || strings.HasPrefix(attrs.Name, "docker") || strings.HasPrefix(attrs.Name, "cni") {
			continue
		}

		mac := attrs.HardwareAddr.String()
		device := resourceapi.Device{
			Name: attrs.Name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"interface-name": {StringValue: &attrs.Name},
				"mac-address":    {StringValue: &mac},
			},
		}
		devices = append(devices, device)
	}
	return devices, nil
}
