// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package cni

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// dpuRecoveryMu serializes recovery scans to prevent concurrent scans
// from racing to move/configure the same VF (e.g., lease flap + startup).
var dpuRecoveryMu sync.Mutex

// testable function variables for unit test injection
var (
	openNetNS      = ns.GetNS
	recoverSriovIf func(s *Server, pod *corev1.Pod, nadKey string, dcd util.DPUConnectionDetails, ifName string) error
)

const (
	dpuRecoveryRetryInterval = 30 * time.Second
	dpuRecoveryTimeout       = 5 * time.Minute
	dpuRecoverySlowInterval  = 2 * time.Minute
	dpuDefaultIfName         = "eth0"
)

// RecoverPodInterfaces scans all pods on this node that have DPU connection-details
// and re-configures any pod whose interface has disappeared. This is intended to be
// called when the DPU transitions from unhealthy to healthy.
func (s *Server) RecoverPodInterfaces(stopCh <-chan struct{}) {
	if !config.IsModeDPUHost() {
		return
	}

	ctx := wait.ContextForChannel(stopCh)
	poll := func(_ context.Context) (bool, error) {
		return s.recoverPodInterfacesScan() == 0, nil
	}
	err := wait.PollUntilContextTimeout(ctx, dpuRecoveryRetryInterval, dpuRecoveryTimeout, true, poll)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		klog.Errorf("DPU recovery: still failing after %s, continuing at %s interval: %v",
			dpuRecoveryTimeout, dpuRecoverySlowInterval, err)
		_ = wait.PollUntilContextCancel(ctx, dpuRecoverySlowInterval, false, poll)
	}
	klog.Infof("DPU recovery: all recoverable pod interfaces have been restored")
}

func (s *Server) recoverPodInterfacesScan() int {
	dpuRecoveryMu.Lock()
	defer dpuRecoveryMu.Unlock()

	klog.Infof("DPU recovery: starting pod interface recovery scan")

	pods, err := s.clientSet.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("DPU recovery: failed to list pods: %v", err)
		return 1
	}

	var failed, scanned int
	for _, pod := range pods {
		if pod.Spec.NodeName != s.nodeName {
			continue
		}
		scanned++
		if err := s.recoverPodInterface(pod); err != nil {
			klog.Errorf("DPU recovery: pod %s/%s: %v", pod.Namespace, pod.Name, err)
			failed++
		}
	}

	klog.Infof("DPU recovery: scan complete (pods_on_node=%d, failed=%d)", scanned, failed)
	return failed
}

func (s *Server) recoverPodInterface(pod *corev1.Pod) error {
	if util.PodWantsHostNetwork(pod) {
		klog.V(5).Infof("DPU recovery: pod %s/%s uses host networking, skipping", pod.Namespace, pod.Name)
		return nil
	}

	dcds, err := util.UnmarshalPodDPUConnDetailsAllNetworks(pod.Annotations)
	if err != nil {
		return fmt.Errorf("failed to parse DPU connection details: %v", err)
	}
	if len(dcds) == 0 {
		return nil
	}

	var errs []error
	for nadKey, dcd := range dcds {
		if nadKey != types.DefaultNetworkName {
			klog.V(5).Infof("DPU recovery: pod %s/%s NAD %s is not default network, skipping", pod.Namespace, pod.Name, nadKey)
			continue
		}
		if dcd.NetnsPath == "" {
			klog.Warningf("DPU recovery: pod %s/%s NAD %s missing netnsPath (created before DPU recovery support); recreate the pod", pod.Namespace, pod.Name, nadKey)
			continue
		}
		if dcd.VfNetdevName == "" {
			klog.Warningf("DPU recovery: pod %s/%s NAD %s has empty VfNetdevName; cannot recover VFIO or non-netdev interfaces", pod.Namespace, pod.Name, nadKey)
			continue
		}

		recoverFn := recoverSriovIf
		if recoverFn == nil {
			recoverFn = (*Server).recoverSriovInterface
		}
		klog.Infof("DPU recovery: pod %s/%s NAD %s: recovering interface %s", pod.Namespace, pod.Name, nadKey, dpuDefaultIfName)
		if err := recoverFn(s, pod, nadKey, dcd, dpuDefaultIfName); err != nil {
			klog.Errorf("DPU recovery: pod %s/%s NAD %s: recovery failed: %v", pod.Namespace, pod.Name, nadKey, err)
			errs = append(errs, fmt.Errorf("NAD %s: %w", nadKey, err))
		} else {
			klog.Infof("DPU recovery: pod %s/%s NAD %s: interface %s recovered successfully", pod.Namespace, pod.Name, nadKey, dpuDefaultIfName)
		}
	}
	return errors.Join(errs...)
}

// recoverSriovInterface restores the default-network VF into the pod netns and
// reconciles MAC, MTU, link state, addresses, and routes. It is idempotent so a
// partial previous attempt (VF already moved, renamed, or configured) can resume.
func (s *Server) recoverSriovInterface(pod *corev1.Pod, nadKey string, dcd util.DPUConnectionDetails, ifName string) error {
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if err != nil {
		return fmt.Errorf("failed to get pod annotation for NAD %s: %v", nadKey, err)
	}

	netNS, err := openNetNS(dcd.NetnsPath)
	if err != nil {
		return fmt.Errorf("failed to open netns %s: %v", dcd.NetnsPath, err)
	}
	defer netNS.Close()

	currentName, err := ensureVFInPodNetns(netNS, dcd, ifName)
	if err != nil {
		return err
	}

	return netNS.Do(func(_ ns.NetNS) error {
		if currentName != ifName {
			if err := renameLink(currentName, ifName); err != nil {
				return fmt.Errorf("failed to rename %s to %s: %v", currentName, ifName, err)
			}
		}
		link, err := util.GetNetLinkOps().LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to lookup %s: %v", ifName, err)
		}
		return reconcilePodLink(link, pod, podAnnotation)
	})
}

// ensureVFInPodNetns checks if the VF is already in the pod netns (as ifName
// or VfNetdevName from a partial previous recovery). If not, it looks up the
// VF on the host and moves it into the pod netns. Returns the current name of
// the link inside the pod netns.
func ensureVFInPodNetns(netNS ns.NetNS, dcd util.DPUConnectionDetails, ifName string) (string, error) {
	// Check inside the pod netns: the VF may already be there as ifName (fully
	// recovered) or as VfNetdevName (moved by a previous attempt but not renamed).
	var currentName string
	err := netNS.Do(func(_ ns.NetNS) error {
		for _, name := range []string{ifName, dcd.VfNetdevName} {
			if _, err := util.GetNetLinkOps().LinkByName(name); err == nil {
				currentName = name
				return nil
			} else if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to inspect pod netns %s: %v", dcd.NetnsPath, err)
	}
	if currentName != "" {
		return currentName, nil
	}

	// VF not in pod netns — look it up on the host and move it in.
	if _, err := util.GetNetLinkOps().LinkByName(dcd.VfNetdevName); err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return "", fmt.Errorf("VF %s not found on host or in pod netns (DPU may not be fully recovered)", dcd.VfNetdevName)
		}
		return "", fmt.Errorf("failed to lookup VF %s on host: %v", dcd.VfNetdevName, err)
	}

	name, moveErr := safeMoveIfToNetns(dcd.VfNetdevName, netNS, dcd.SandboxId)
	if moveErr != nil {
		return "", fmt.Errorf("failed to move VF %s to netns: %v", dcd.VfNetdevName, moveErr)
	}
	return name, nil
}

func reconcilePodLink(link netlink.Link, pod *corev1.Pod, podAnnotation *util.PodAnnotation) error {
	if err := util.GetNetLinkOps().LinkSetHardwareAddr(link, podAnnotation.MAC); err != nil {
		return fmt.Errorf("failed to set MAC: %v", err)
	}
	if err := util.GetNetLinkOps().LinkSetMTU(link, config.Default.MTU); err != nil {
		return fmt.Errorf("failed to set MTU: %v", err)
	}
	if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to set link up: %v", err)
	}
	if kubevirt.IsPodLiveMigratable(pod) {
		klog.Infof("DPU recovery: skipping IP/route configuration for live-migratable pod %s/%s", pod.Namespace, pod.Name)
		return nil
	}
	return setupNetworkRecovery(link, podAnnotation)
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
