// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package cni

import (
	"fmt"
	"net"
	"os"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// isInterfacePresentInNetns checks whether the given interface exists inside the
// network namespace at netnsPath.
func isInterfacePresentInNetns(netnsPath, ifName string) (bool, error) {
	netNS, err := ns.GetNS(netnsPath)
	if err != nil {
		return false, fmt.Errorf("failed to open netns %s: %v", netnsPath, err)
	}
	defer netNS.Close()

	var exists bool
	err = netNS.Do(func(_ ns.NetNS) error {
		_, linkErr := netlink.LinkByName(ifName)
		if linkErr == nil {
			exists = true
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to check interface in netns %s: %v", netnsPath, err)
	}
	return exists, nil
}

// RecoverPodInterfaces scans all pods on this node that have DPU connection-details
// and re-configures any pod whose interface has disappeared. This is intended to be
// called when the DPU transitions from unhealthy to healthy.
func (s *Server) RecoverPodInterfaces() {
	if !config.IsModeDPUHost() {
		return
	}

	klog.Infof("DPU recovery: starting pod interface recovery scan")

	pods, err := s.clientSet.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("DPU recovery: failed to list pods: %v", err)
		return
	}

	var failed int
	for _, pod := range pods {
		if err := s.recoverPodInterface(pod); err != nil {
			klog.Errorf("DPU recovery: pod %s/%s: %v", pod.Namespace, pod.Name, err)
			failed++
		}
	}

	klog.Infof("DPU recovery: scan complete (pods=%d, failed=%d)", len(pods), failed)
}

func (s *Server) recoverPodInterface(pod *corev1.Pod) error {
	dcds, err := util.UnmarshalPodDPUConnDetailsAllNetworks(pod.Annotations)
	if err != nil {
		return fmt.Errorf("failed to parse DPU connection details: %v", err)
	}
	if len(dcds) == 0 {
		return nil
	}

	for nadKey, dcd := range dcds {
		if dcd.NetnsPath == "" {
			klog.V(5).Infof("DPU recovery: pod %s/%s NAD %s has no netnsPath, skipping", pod.Namespace, pod.Name, nadKey)
			continue
		}

		ifName := "eth0"
		if nadKey != types.DefaultNetworkName {
			ifName = ""
		}
		if ifName == "" {
			klog.V(5).Infof("DPU recovery: pod %s/%s NAD %s is not default network, skipping for now", pod.Namespace, pod.Name, nadKey)
			continue
		}

		present, err := isInterfacePresentInNetns(dcd.NetnsPath, ifName)
		if err != nil {
			klog.Warningf("DPU recovery: pod %s/%s NAD %s: failed to check interface: %v", pod.Namespace, pod.Name, nadKey, err)
			continue
		}
		if present {
			klog.V(5).Infof("DPU recovery: pod %s/%s NAD %s: interface %s already present, skipping", pod.Namespace, pod.Name, nadKey, ifName)
			continue
		}

		klog.Infof("DPU recovery: pod %s/%s NAD %s: interface %s missing, recovering", pod.Namespace, pod.Name, nadKey, ifName)
		if err := s.recoverSriovInterface(pod, nadKey, dcd, ifName); err != nil {
			klog.Errorf("DPU recovery: pod %s/%s NAD %s: recovery failed: %v", pod.Namespace, pod.Name, nadKey, err)
		} else {
			klog.Infof("DPU recovery: pod %s/%s NAD %s: interface %s recovered successfully", pod.Namespace, pod.Name, nadKey, ifName)
		}
	}
	return nil
}

func (s *Server) recoverSriovInterface(pod *corev1.Pod, nadKey string, dcd util.DPUConnectionDetails, ifName string) error {
	netNS, err := ns.GetNS(dcd.NetnsPath)
	if err != nil {
		return fmt.Errorf("failed to open netns %s: %v", dcd.NetnsPath, err)
	}
	defer netNS.Close()

	vfNetdev := dcd.VfNetdevName
	if vfNetdev == "" {
		return fmt.Errorf("VfNetdevName is empty in DPU connection details")
	}

	// Verify the VF is available on the host
	if _, err := netlink.LinkByName(vfNetdev); err != nil {
		return fmt.Errorf("VF netdev %s not found on host (DPU may not be fully recovered): %v", vfNetdev, err)
	}

	// Move VF to pod netns
	newNetdevName, err := safeMoveIfToNetns(vfNetdev, netNS, dcd.SandboxId)
	if err != nil {
		return fmt.Errorf("failed to move VF %s to netns: %v", vfNetdev, err)
	}

	// Inside the netns: rename to ifName, set MAC/IP/routes
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if err != nil {
		return fmt.Errorf("failed to get pod annotation for NAD %s: %v", nadKey, err)
	}

	err = netNS.Do(func(_ ns.NetNS) error {
		if err := renameLink(newNetdevName, ifName); err != nil {
			return fmt.Errorf("failed to rename %s to %s: %v", newNetdevName, ifName, err)
		}

		link, err := util.GetNetLinkOps().LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to lookup %s: %v", ifName, err)
		}

		if err := util.GetNetLinkOps().LinkSetHardwareAddr(link, podAnnotation.MAC); err != nil {
			return fmt.Errorf("failed to set MAC: %v", err)
		}

		if err := util.GetNetLinkOps().LinkSetMTU(link, config.Default.MTU); err != nil {
			return fmt.Errorf("failed to set MTU: %v", err)
		}

		if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to set link up: %v", err)
		}

		return setupNetworkRecovery(link, podAnnotation)
	})
	if err != nil {
		return fmt.Errorf("failed to configure interface in netns: %v", err)
	}

	return nil
}

// setupNetworkRecovery configures IPs and routes on a recovered interface.
// Unlike setupNetwork, it tolerates existing state (EEXIST) to handle
// partial recovery retries gracefully.
func setupNetworkRecovery(link netlink.Link, podAnnotation *util.PodAnnotation) error {
	for _, ip := range podAnnotation.IPs {
		addr := &netlink.Addr{IPNet: ip}
		if err := util.GetNetLinkOps().AddrAdd(link, addr); err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to add IP addr %s to %s: %v", ip, link.Attrs().Name, err)
		}
	}

	for _, gw := range podAnnotation.Gateways {
		if err := addRouteIdempotent(nil, gw, link, config.Default.RoutableMTU); err != nil {
			return fmt.Errorf("failed to add gateway route: %v", err)
		}
	}

	for _, route := range podAnnotation.Routes {
		if err := addRouteIdempotent(route.Dest, route.NextHop, link, config.Default.RoutableMTU); err != nil {
			return fmt.Errorf("failed to add route %v via %v: %v", route.Dest, route.NextHop, err)
		}
	}

	return nil
}

func addRouteIdempotent(ipn *net.IPNet, gw net.IP, dev netlink.Link, mtu int) error {
	return util.GetNetLinkOps().RouteReplace(&netlink.Route{
		LinkIndex: dev.Attrs().Index,
		Scope:     netlink.SCOPE_UNIVERSE,
		Dst:       ipn,
		Gw:        gw,
		MTU:       mtu,
	})
}
