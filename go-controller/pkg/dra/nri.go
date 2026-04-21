// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dra

import (
	"context"
	"fmt"

	"github.com/containerd/nri/pkg/api"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// Synchronize handles initial NRI synchronization.
func (k *NetworkDriver) Synchronize(_ context.Context, _ []*api.PodSandbox, _ []*api.Container) ([]*api.ContainerUpdate, error) {
	return nil, nil
}

// RunPodSandbox is called when a pod is created by the Container Runtime.
func (k *NetworkDriver) RunPodSandbox(_ context.Context, pod *api.PodSandbox) error {
	podUID := types.UID(pod.Uid)
	networkNamespace := getNetworkNamespace(pod)
	if networkNamespace == "" {
		return nil // No network namespace, nothing to configure
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	devices := k.sharedState.PodDeviceConfig[podUID]
	preparedData := k.sharedState.PreparedData[podUID]

	for _, device := range devices {
		if err := k.configureDeviceForPod(device, networkNamespace, pod, preparedData); err != nil {
			return err
		}
	}
	return nil
}

// StopPodSandbox is called when a pod is stopped by the Container Runtime.
func (k *NetworkDriver) StopPodSandbox(_ context.Context, pod *api.PodSandbox) error {
	podUID := types.UID(pod.Uid)
	networkNamespace := getNetworkNamespace(pod)

	k.mu.Lock()
	defer k.mu.Unlock()

	devices := k.sharedState.PodDeviceConfig[podUID]
	preparedData := k.sharedState.PreparedData[podUID]

	for _, device := range devices {
		if err := k.cleanupDeviceForPod(device, networkNamespace, pod, preparedData); err != nil {
			klog.Infof("Failed to cleanup device %s for pod %s: %v", device.Name, pod.Name, err)
		}
	}
	return nil
}

// RemovePodSandbox is called when a pod is removed by the Container Runtime.
func (k *NetworkDriver) RemovePodSandbox(_ context.Context, pod *api.PodSandbox) error {
	podUID := types.UID(pod.Uid)
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.sharedState.PodDeviceConfig, podUID)
	delete(k.sharedState.PreparedData, podUID)
	return nil
}

func (k *NetworkDriver) configureDeviceForPod(_ AllocatedDevice, networkNamespace string, _ *api.PodSandbox, preparedData interface{}) error {
	hostDeviceName, ok := preparedData.(string)
	if !ok {
		return fmt.Errorf("invalid prepared data type: expected string, got %T", preparedData)
	}

	podInterfaceName := hostDeviceName
	return nsAttachNetdev(hostDeviceName, networkNamespace, podInterfaceName)
}

func (k *NetworkDriver) cleanupDeviceForPod(_ AllocatedDevice, networkNamespace string, _ *api.PodSandbox, preparedData interface{}) error {
	hostDeviceName, ok := preparedData.(string)
	if !ok {
		return fmt.Errorf("invalid prepared data type: expected string, got %T", preparedData)
	}

	podInterfaceName := hostDeviceName
	return nsDetachNetdev(networkNamespace, podInterfaceName, hostDeviceName)
}

func getNetworkNamespace(pod *api.PodSandbox) string {
	if pod.GetLinux() == nil {
		return ""
	}
	for _, nsRef := range pod.GetLinux().GetNamespaces() {
		if nsRef.GetType() == "network" {
			return nsRef.GetPath()
		}
	}
	return ""
}

func nsAttachNetdev(hostDeviceName, targetNetnsPath, podInterfaceName string) error {
	targetNs, err := ns.GetNS(targetNetnsPath)
	if err != nil {
		return fmt.Errorf("open target netns %q: %w", targetNetnsPath, err)
	}
	defer targetNs.Close()

	link, err := netlink.LinkByName(hostDeviceName)
	if err != nil {
		return fmt.Errorf("find host device %q: %w", hostDeviceName, err)
	}

	if err := netlink.LinkSetNsFd(link, int(targetNs.Fd())); err != nil {
		return fmt.Errorf("move device %q to netns %q: %w", hostDeviceName, targetNetnsPath, err)
	}

	return targetNs.Do(func(_ ns.NetNS) error {
		movedLink, err := netlink.LinkByName(hostDeviceName)
		if err != nil {
			return fmt.Errorf("find moved device %q in target netns: %w", hostDeviceName, err)
		}
		if podInterfaceName != hostDeviceName {
			if err := netlink.LinkSetName(movedLink, podInterfaceName); err != nil {
				return fmt.Errorf("rename %q to %q in target netns: %w", hostDeviceName, podInterfaceName, err)
			}
			movedLink, err = netlink.LinkByName(podInterfaceName)
			if err != nil {
				return fmt.Errorf("find renamed device %q in target netns: %w", podInterfaceName, err)
			}
		}
		if err := netlink.LinkSetUp(movedLink); err != nil {
			return fmt.Errorf("set device %q up in target netns: %w", podInterfaceName, err)
		}
		return nil
	})
}

func nsDetachNetdev(podNetnsPath, podInterfaceName, hostDeviceName string) error {
	podNs, err := ns.GetNS(podNetnsPath)
	if err != nil {
		return fmt.Errorf("open pod netns %q: %w", podNetnsPath, err)
	}
	defer podNs.Close()

	hostNs, err := ns.GetCurrentNS()
	if err != nil {
		return fmt.Errorf("open host netns: %w", err)
	}
	defer hostNs.Close()

	if err := podNs.Do(func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(podInterfaceName)
		if err != nil {
			return fmt.Errorf("find pod device %q: %w", podInterfaceName, err)
		}
		if err := netlink.LinkSetNsFd(link, int(hostNs.Fd())); err != nil {
			return fmt.Errorf("move %q back to host netns: %w", podInterfaceName, err)
		}
		return nil
	}); err != nil {
		return err
	}

	return hostNs.Do(func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(podInterfaceName)
		if err != nil {
			return fmt.Errorf("find returned device %q in host netns: %w", podInterfaceName, err)
		}
		if podInterfaceName != hostDeviceName {
			if err := netlink.LinkSetName(link, hostDeviceName); err != nil {
				return fmt.Errorf("rename %q to %q in host netns: %w", podInterfaceName, hostDeviceName, err)
			}
			link, err = netlink.LinkByName(hostDeviceName)
			if err != nil {
				return fmt.Errorf("find renamed host device %q: %w", hostDeviceName, err)
			}
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("set device %q up in host netns: %w", hostDeviceName, err)
		}
		return nil
	})
}
