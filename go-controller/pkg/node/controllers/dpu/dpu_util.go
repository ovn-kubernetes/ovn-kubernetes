// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dpu

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// dpuConnectionState tracks the per-NAD DPU connection state for a pod stored
// in podNADToDPUCDMap. It uses the resolved VF representor name directly instead
// of PfId/VfId, allowing state to be bootstrapped from OVS external_ids on restart.
type dpuConnectionState struct {
	vfRepName string
	sandboxId string
}

// podDPUState wraps the per-NAD state map with a mutex to protect concurrent
// access from different reconcilers (e.g. pod and NAD reconciler).
type podDPUState struct {
	sync.Mutex
	nadStates map[string]*dpuConnectionState
}

func (c *Controller) addDPUPodForNAD(pod *corev1.Pod, state *dpuConnectionState,
	netInfo util.NetInfo, nadKey string, getter cni.PodInfoGetter) error {
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)
	klog.Infof("Adding %s on DPU", podDesc)
	podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations, nil,
		string(pod.UID), "", nadKey, netInfo.GetNetworkName(), netInfo.MTU())
	if err != nil {
		return fmt.Errorf("failed to get pod interface information of %s: %v. retrying", podDesc, err)
	}
	err = c.addRepPort(pod, state, podInterfaceInfo, getter)
	if err != nil {
		return fmt.Errorf("failed to add rep port for %s, %v. retrying", podDesc, err)
	}
	return nil
}

func dpuConnectionStateChanged(oldState *dpuConnectionState, newState *dpuConnectionState) bool {
	if oldState == nil && newState == nil {
		return false
	}
	if (oldState != nil && newState == nil) || (oldState == nil && newState != nil) {
		return true
	}
	return oldState.vfRepName != newState.vfRepName || oldState.sandboxId != newState.sandboxId
}

// getNetInfoForNADKey returns the NetInfo for a NAD key. The nadKey may be
// indexed (e.g. "ns/nad/1" when a pod references the same NAD multiple times),
// but networkMgr only tracks the base "ns/nad" key, so we strip the index.
// For the default network, GetNetInfoForNADKey doesn't work because the default
// network has no NAD resource, so we fall back to GetNetwork.
func (c *Controller) getNetInfoForNADKey(nadKey string) util.NetInfo {
	if nadKey == types.DefaultNetworkName {
		return c.networkMgr.GetNetwork(nadKey)
	}
	nadName, _, err := util.GetNadFromIndexedNADKey(nadKey)
	if err != nil {
		klog.Warningf("Failed to parse NAD key %s: %v", nadKey, err)
		return nil
	}
	return c.networkMgr.GetNetInfoForNADKey(nadName)
}

// handleAddOrUpdateDPUPod reconciles DPU state for a pod
func (c *Controller) handleAddOrUpdateDPUPod(podKey string, pod *corev1.Pod, clientSet cni.PodInfoGetter) error {
	allDPUCDs, err := util.UnmarshalPodDPUConnDetailsAllNetworks(pod.Annotations)
	if err != nil {
		return err
	}

	// Desired NADs: those present in the DPU connection details annotation
	// AND whose network still exists in networkMgr.
	desiredNADStates := make(map[string]*dpuConnectionState)
	for nadKey, dpuCD := range allDPUCDs {
		if c.getNetInfoForNADKey(nadKey) != nil {
			vfRepName, err := util.GetDPUOps().GetPortRepresentor(dpuCD.PfId, dpuCD.VfId)
			if err != nil {
				return fmt.Errorf("failed to get VF representor for pod %s/%s NAD %s (PfId=%s, VfId=%s): %v",
					pod.Namespace, pod.Name, nadKey, dpuCD.PfId, dpuCD.VfId, err)
			}
			desiredNADStates[nadKey] = &dpuConnectionState{
				vfRepName: vfRepName,
				sandboxId: dpuCD.SandboxId,
			}
		}
	}
	if len(desiredNADStates) == 0 {
		return c.handleDeleteDPUPodByKey(podKey)
	}

	var ps *podDPUState
	if v, ok := c.podNADToDPUCDMap.Load(pod.UID); ok {
		ps = v.(*podDPUState)
	} else {
		ps = &podDPUState{nadStates: make(map[string]*dpuConnectionState)}
		c.podNADToDPUCDMap.Store(pod.UID, ps)
	}
	ps.Lock()
	defer ps.Unlock()

	klog.V(5).Infof("Reconcile for Pod: %s (UID %s)", podKey, pod.UID)

	var errs []error

	// First clean up NADs that existed before but are either not desired or updated.
	var validPortsToDelete []string
	var nadsToDelete []string
	for nadKey, state := range ps.nadStates {
		desiredState, exists := desiredNADStates[nadKey]
		if !exists || dpuConnectionStateChanged(state, desiredState) {
			klog.Infof("Deleting stale VF representor %s for pod %s NAD %s", state.vfRepName, podKey, nadKey)
			valid, err := validateRepPort(state.vfRepName, state.sandboxId, nadKey)
			if err != nil {
				return fmt.Errorf("failed to validate representor %s for pod %s NAD %s: %v", state.vfRepName, podKey, nadKey, err)
			}
			if valid {
				validPortsToDelete = append(validPortsToDelete, state.vfRepName)
			}
			nadsToDelete = append(nadsToDelete, nadKey)
		}
	}
	if len(validPortsToDelete) > 0 {
		setRepPortInterfacesDown(validPortsToDelete)
		if err := libovsdbops.DeleteMultiplePortsWithInterfaces(c.ovsClient, "br-int", validPortsToDelete...); err != nil {
			return fmt.Errorf("failed to delete stale representor ports %v for pod %s: %w", validPortsToDelete, podKey, err)
		}
	}

	if len(nadsToDelete) > 0 {
		// Update connection status in one go
		statusMap := map[string]*util.DPUConnectionStatus{}
		for _, nadKey := range nadsToDelete {
			statusMap[nadKey] = nil
		}
		if err := util.UpdatePodDPUConnStatusWithRetry(c.watchFactory.PodCoreInformer().Lister(), c.kube, pod, statusMap); err != nil {
			if !util.IsAnnotationAlreadySetError(err) {
				return fmt.Errorf("failed to clear DPU connection status %v for pod %s: %v", statusMap, podKey, err)
			}
		}
		for _, nadKey := range nadsToDelete {
			delete(ps.nadStates, nadKey)
		}
	}

	// Then setup NADs that are new or updated.
	for nadKey, desiredState := range desiredNADStates {
		if _, ok := ps.nadStates[nadKey]; !ok {
			netInfo := c.getNetInfoForNADKey(nadKey)
			if netInfo == nil {
				klog.Warningf("Network not found for NAD %s, skipping pod %s", nadKey, podKey)
				continue
			}
			if err := c.addDPUPodForNAD(pod, desiredState, netInfo, nadKey, clientSet); err != nil {
				klog.Errorf("Error adding pod %s NAD %s: %v", podKey, nadKey, err)
				errs = append(errs, err)
				continue
			}
			ps.nadStates[nadKey] = desiredState
		}
	}

	c.podKeyToUID.Store(podKey, pod.UID)
	return utilerrors.Join(errs...)
}

// handleDeleteDPUPodByKey cleans up DPU state using the namespace/name key to find the UID.
// Only removes the podKeyToUID entry after all NAD state has been successfully cleaned up,
// so that retries can still locate the UID.
func (c *Controller) handleDeleteDPUPodByKey(podKey string) error {
	uidVal, ok := c.podKeyToUID.Load(podKey)
	if !ok {
		return nil
	}
	uid := uidVal.(k8stypes.UID)
	if err := c.handleDeleteDPUPodByUID(uid, podKey); err != nil {
		return err
	}
	c.podKeyToUID.Delete(podKey)
	return nil
}

// handleDeleteDPUPodByUID cleans up DPU state for a specific pod UID.
// Pod is already deleted, so no annotation updates are needed; port deletions
// are batched into a single OVSDB transaction.
func (c *Controller) handleDeleteDPUPodByUID(uid k8stypes.UID, podKey string) error {
	v, ok := c.podNADToDPUCDMap.Load(uid)
	if !ok {
		return nil
	}
	ps := v.(*podDPUState)
	ps.Lock()
	defer ps.Unlock()

	klog.V(5).Infof("Delete for Pod: %s (UID %s)", podKey, uid)
	var portsToDelete []string
	var nadsToDelete []string
	for nadKey, state := range ps.nadStates {
		if state == nil {
			continue
		}
		klog.Infof("Deleting VF representor %s for pod %s NAD %s", state.vfRepName, podKey, nadKey)
		valid, err := validateRepPort(state.vfRepName, state.sandboxId, nadKey)
		if err != nil {
			return fmt.Errorf("failed to validate representor %s for pod %s NAD %s: %v", state.vfRepName, podKey, nadKey, err)
		}
		if valid {
			portsToDelete = append(portsToDelete, state.vfRepName)
		}
		nadsToDelete = append(nadsToDelete, nadKey)
	}

	if len(portsToDelete) > 0 {
		setRepPortInterfacesDown(portsToDelete)
		if err := libovsdbops.DeleteMultiplePortsWithInterfaces(c.ovsClient, "br-int", portsToDelete...); err != nil {
			return fmt.Errorf("failed to delete representor ports %v for pod %s: %w", portsToDelete, podKey, err)
		}
		klog.Infof("Deleted %d representor ports from br-int for pod %s", len(portsToDelete), podKey)
	}

	for _, nadKey := range nadsToDelete {
		delete(ps.nadStates, nadKey)
	}
	c.podNADToDPUCDMap.Delete(uid)
	return nil
}

// addRepPort adds the representor of the VF to the ovs bridge
func (c *Controller) addRepPort(pod *corev1.Pod, state *dpuConnectionState, ifInfo *cni.PodInterfaceInfo, getter cni.PodInfoGetter) error {

	nadKey := ifInfo.NADKey
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)

	// set netdevName so OVS interface can be added with external_ids:vf-netdev-name, and is able to
	// be part of healthcheck.
	ifInfo.NetdevName = state.vfRepName
	deviceID, err := util.GetDPUOps().GetDeviceAddress(state.vfRepName)
	if err != nil {
		return fmt.Errorf("failed to get PCI address of VF rep %s for pod %s: %v", state.vfRepName, podDesc, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	klog.Infof("Adding VF representor %s for %s", state.vfRepName, podDesc)
	err = cni.ConfigureOVS(ctx, pod.Namespace, pod.Name, "", state.vfRepName, ifInfo, state.sandboxId,
		deviceID, false, getter)
	if err != nil {
		// Note(adrianc): we are lenient with cleanup in this method as pod is going to be retried anyway.
		_ = c.delRepPort(pod, state, nadKey)
		return err
	}
	klog.Infof("Port %s added to bridge br-int", state.vfRepName)

	// Update connection-status annotation
	// TODO(adrianc): we should update Status in case of error as well
	statusMap := map[string]*util.DPUConnectionStatus{nadKey: {Status: util.DPUConnectionStatusReady, Reason: ""}}
	err = util.UpdatePodDPUConnStatusWithRetry(c.watchFactory.PodCoreInformer().Lister(), c.kube, pod, statusMap)
	if err != nil {
		_ = c.delRepPort(pod, state, nadKey)
		return fmt.Errorf("failed to update connection status annotation for %s: %v", podDesc, err)
	}
	return nil
}

// delRepPort validates and deletes a single representor port from br-int.
// TODO(adrianc): handle: clearPodBandwidth(pr.SandboxID), pr.deletePodConntrack()
func (c *Controller) delRepPort(pod *corev1.Pod, state *dpuConnectionState, nadKey string) error {
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)
	klog.Infof("Deleting VF representor %s for %s", state.vfRepName, podDesc)
	valid, err := validateRepPort(state.vfRepName, state.sandboxId, nadKey)
	if err != nil {
		return err
	}
	if !valid {
		return nil
	}
	setRepPortInterfacesDown([]string{state.vfRepName})
	if err = libovsdbops.DeleteMultiplePortsWithInterfaces(c.ovsClient, "br-int", state.vfRepName); err != nil {
		return fmt.Errorf("failed to delete representor port %s from br-int for %s: %w", state.vfRepName, podDesc, err)
	}
	klog.Infof("Port %s deleted from bridge br-int", state.vfRepName)
	return nil
}

// validateRepPort checks that the OVS port matches the expected sandbox and NAD key.
// Returns (true, nil) if valid; (false, nil) if the port doesn't exist or belongs
// to a different pod/NAD (mismatch is permanent, so retrying would not help);
// (false, err) only on transient OVS lookup failures.
func validateRepPort(vfRepName, sandboxId, nadKey string) (bool, error) {
	ifExists, sandbox, expectedNADKey, err := util.GetOVSPortPodInfo(vfRepName)
	if err != nil {
		return false, err
	}
	if !ifExists {
		klog.Infof("VF representor %s is not an OVS interface, nothing to do", vfRepName)
		return false, nil
	}
	if sandbox != sandboxId {
		klog.Warningf("OVS port %s belongs to sandbox %s, not %s; skipping", vfRepName, sandbox, sandboxId)
		return false, nil
	}
	if expectedNADKey != nadKey {
		klog.Warningf("OVS port %s belongs to NAD %s, not %s; skipping", vfRepName, expectedNADKey, nadKey)
		return false, nil
	}
	return true, nil
}

// setRepPortInterfacesDown sets the link down for each representor interface.
// Failures are logged as warnings but do not block deletion.
func setRepPortInterfacesDown(interfaces []string) {
	for _, iface := range interfaces {
		link, err := util.GetNetLinkOps().LinkByName(iface)
		if err != nil {
			klog.Warningf("Failed to get link device for representor port %s: %v", iface, err)
			continue
		}
		if err = util.GetNetLinkOps().LinkSetDown(link); err != nil {
			klog.Warningf("Failed to set link down for representor port %s: %v", iface, err)
		}
	}
}

// podKeyFromIfaceID extracts the namespace/name pod key from an OVS iface-id.
// For the default network, iface-id is "namespace_podName".
// For UDN, iface-id is "<udnPrefix>namespace_podName" where udnPrefix is derived from the NAD key.
func podKeyFromIfaceID(ifaceID, nadKey string) string {
	namespacePod := ifaceID
	if nadKey != types.DefaultNetworkName {
		prefix := util.GetUserDefinedNetworkPrefix(nadKey)
		namespacePod = strings.TrimPrefix(ifaceID, prefix)
	}
	parts := strings.SplitN(namespacePod, "_", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}
