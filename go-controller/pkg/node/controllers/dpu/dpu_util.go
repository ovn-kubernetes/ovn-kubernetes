// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dpu

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// dpuConnectionState is the DPU connection state of one NAD of a pod. The VF
// representor name is stored already resolved, so that the state can be rebuilt
// from OVS external_ids on restart.
type dpuConnectionState struct {
	vfRepName string
	sandboxId string
	// revalidate is set on a port rebuilt from OVS that the pod does not
	// already report ready, so it exists but may not be fully configured.
	revalidate bool
}

// podDPUState holds the per-NAD state of a single pod, along with the UID of the
// pod incarnation it was created for. Access is serialized by the podStates key
// lock.
type podDPUState struct {
	uid       k8stypes.UID
	nadStates map[string]*dpuConnectionState
}

func (c *Controller) addDPUPodForNAD(ctx context.Context, pod *corev1.Pod, state *dpuConnectionState,
	netInfo util.NetInfo, nadKey string, getter cni.PodInfoGetter, isRevalidation bool) error {
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)
	klog.Infof("Adding %s on DPU", podDesc)
	podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations, nil,
		string(pod.UID), "", nadKey, netInfo.GetNetworkName(), netInfo.MTU())
	if err != nil {
		return fmt.Errorf("failed to get pod interface information of %s: %w", podDesc, err)
	}
	err = c.addRepPort(ctx, pod, state, podInterfaceInfo, getter, isRevalidation)
	if err != nil {
		return fmt.Errorf("failed to add rep port for %s: %w", podDesc, err)
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

// getNetInfoForNADKey returns the NetInfo for a NAD key, or nil when the NAD it
// names no longer exists. An indexed key ("ns/nad/1", from a pod referencing the
// same NAD more than once) is stripped to the base key networkMgr tracks, and
// the default network is looked up with GetNetwork since it has no NAD resource.
// A key that cannot be parsed is a malformed annotation rather than a deleted
// NAD, so it is reported as an error.
func (c *Controller) getNetInfoForNADKey(nadKey string) (util.NetInfo, error) {
	if nadKey == types.DefaultNetworkName {
		return c.networkMgr.GetNetwork(nadKey), nil
	}
	nadName, _, err := util.GetNadFromIndexedNADKey(nadKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse NAD key %s: %w", nadKey, err)
	}
	return c.networkMgr.GetNetInfoForNADKey(nadName), nil
}

// handleAddOrUpdateDPUPod reconciles DPU state for a pod.
// The podStates lock for podKey must be held by the caller.
func (c *Controller) handleAddOrUpdateDPUPod(ctx context.Context, podKey string, pod *corev1.Pod, clientSet cni.PodInfoGetter) error {
	allDPUCDs, err := util.UnmarshalPodDPUConnDetailsAllNetworks(pod.Annotations)
	if err != nil {
		return err
	}

	// Index the pod by the NADs it asks for, before the lookups below, so that
	// a NAD becoming known between the two still requeues the pod. The default
	// network has no NAD to wait for.
	nadNames := sets.New[string]()
	for nadKey := range allDPUCDs {
		if nadKey == types.DefaultNetworkName {
			continue
		}
		if nadName, _, err := util.GetNadFromIndexedNADKey(nadKey); err == nil {
			nadNames.Insert(nadName)
		}
	}
	c.nadPods.set(podKey, nadNames)

	// Desired NADs: those in the connection details annotation whose network
	// still exists. Each one keeps the NetInfo the add below configures from.
	type desiredNAD struct {
		state   *dpuConnectionState
		netInfo util.NetInfo
	}
	desiredNADs := make(map[string]desiredNAD)
	for nadKey, dpuCD := range allDPUCDs {
		netInfo, err := c.getNetInfoForNADKey(nadKey)
		if err != nil {
			return err
		}
		if netInfo == nil {
			klog.V(5).Infof("Network of NAD %s not found, skipping it for pod %s", nadKey, podKey)
			continue
		}
		vfRepName, err := util.GetDPUOps().GetPortRepresentor(dpuCD.PfId, dpuCD.VfId)
		if err != nil {
			return fmt.Errorf("failed to get VF representor for pod %s/%s NAD %s (PfId=%s, VfId=%s): %w",
				pod.Namespace, pod.Name, nadKey, dpuCD.PfId, dpuCD.VfId, err)
		}
		desiredNADs[nadKey] = desiredNAD{
			state: &dpuConnectionState{
				vfRepName: vfRepName,
				sandboxId: dpuCD.SandboxId,
			},
			netInfo: netInfo,
		}
	}
	if len(desiredNADs) == 0 {
		return c.handleDeleteDPUPod(ctx, podKey, pod)
	}

	ps, _ := c.podStates.LoadOrStore(podKey, &podDPUState{
		uid:       pod.UID,
		nadStates: make(map[string]*dpuConnectionState),
	})

	klog.V(5).Infof("Reconcile for Pod: %s (UID %s)", podKey, pod.UID)

	var errs []error

	// First clean up NADs that existed before but are either not desired or updated.
	var validPortsToDelete []repPortDeletion
	var nadsToUntrack []string
	revalidating := map[string]*dpuConnectionState{}
	nadsToKeep := make(map[string]*dpuConnectionState, len(ps.nadStates))
	for nadKey, state := range ps.nadStates {
		desired, exists := desiredNADs[nadKey]
		if !exists || dpuConnectionStateChanged(state, desired.state) {
			klog.Infof("Deleting stale VF representor %s for pod %s NAD %s", state.vfRepName, podKey, nadKey)
			validPortsToDelete = append(validPortsToDelete, repPortDeletion{state.vfRepName, state.sandboxId, nadKey})
			nadsToUntrack = append(nadsToUntrack, nadKey)
			continue
		}
		if state.revalidate {
			// ConfigureOVS adds the port before it sets the representor up, so
			// a port recovered from OVS may have been left half configured.
			// Untracking it runs ConfigureOVS again, which is idempotent, while
			// keeping the status the pod already carries. The port predates this
			// reconcile and may well be carrying traffic, so it is left in place
			// if that run fails instead of being deleted and built again.
			klog.Infof("Revalidating VF representor %s for pod %s NAD %s", state.vfRepName, podKey, nadKey)
			nadsToKeep[nadKey] = state
			nadsToUntrack = append(nadsToUntrack, nadKey)
			revalidating[nadKey] = state
			continue
		}
		if _, err := libovsdbops.GetOVSInterface(c.ovsClient, state.vfRepName); err != nil {
			if !errors.Is(err, libovsdbclient.ErrNotFound) {
				return fmt.Errorf("failed to look up representor %s for pod %s NAD %s: %w",
					state.vfRepName, podKey, nadKey, err)
			}
			// Untracking it is what makes the loop below add it back.
			klog.Infof("VF representor %s for pod %s NAD %s is gone from OVS, re-adding",
				state.vfRepName, podKey, nadKey)
			nadsToUntrack = append(nadsToUntrack, nadKey)
			continue
		}
		nadsToKeep[nadKey] = state
	}

	// Make the status match the NADs that stay configured, before the deletions
	// below: the host side waits on this status to plumb a new sandbox, and must
	// not be told a representor is ready once it has been deleted. NADs added
	// below get their status from addRepPort instead.
	if err := c.reconcileDPUConnStatus(ctx, pod, podKey, nadsToKeep); err != nil {
		if len(validPortsToDelete) > 0 {
			// Nothing is untracked yet, so the next retry finds these ports.
			return err
		}
		errs = append(errs, err)
	}

	if len(validPortsToDelete) > 0 {
		if err := c.deleteRepPorts(ctx, validPortsToDelete); err != nil {
			return fmt.Errorf("failed to delete stale representor ports %v for pod %s: %w", validPortsToDelete, podKey, err)
		}
	}

	// Only untrack once the ports are gone, so that a failed deletion is picked
	// up by the next retry.
	for _, nadKey := range nadsToUntrack {
		delete(ps.nadStates, nadKey)
	}

	// Then setup NADs that are new or updated.
	for nadKey, desired := range desiredNADs {
		if _, ok := ps.nadStates[nadKey]; ok {
			continue
		}
		recovered, isRevalidation := revalidating[nadKey]
		if err := c.addDPUPodForNAD(ctx, pod, desired.state, desired.netInfo, nadKey, clientSet,
			isRevalidation); err != nil {
			klog.Errorf("Error adding pod %s NAD %s: %v", podKey, nadKey, err)
			if isRevalidation {
				// Keep the recovered state tracked, since the port was left in
				// place: the retry revalidates it again, and a teardown in
				// between still finds the port to delete.
				ps.nadStates[nadKey] = recovered
			}
			errs = append(errs, err)
			continue
		}
		ps.nadStates[nadKey] = desired.state
	}

	return utilerrors.Join(errs...)
}

// reconcileDPUConnStatus makes the pod's connection status annotation match the
// NADs tracked for it: every tracked NAD whose port is confirmed is reported
// ready, every other entry is removed.
func (c *Controller) reconcileDPUConnStatus(ctx context.Context, pod *corev1.Pod, podKey string, tracked map[string]*dpuConnectionState) error {
	currentStatus, err := util.UnmarshalPodDPUConnStatusAllNetworks(pod.Annotations)
	if err != nil {
		return err
	}

	statusMap := map[string]*util.DPUConnectionStatus{}
	for nadKey, state := range tracked {
		if state.revalidate {
			// Nothing confirms this port yet, so leave the status as it is
			// rather than claim a representor that may still be down.
			// addRepPort reports it ready once the configuration completes.
			continue
		}
		if status, ok := currentStatus[nadKey]; !ok || status.Status != util.DPUConnectionStatusReady {
			statusMap[nadKey] = &util.DPUConnectionStatus{Status: util.DPUConnectionStatusReady}
		}
	}
	for nadKey := range currentStatus {
		if _, ok := tracked[nadKey]; !ok {
			statusMap[nadKey] = nil
		}
	}
	if len(statusMap) == 0 {
		return nil
	}

	klog.V(5).Infof("Updating DPU connection status of pod %s for %d NADs", podKey, len(statusMap))
	err = util.UpdatePodDPUConnStatusWithRetry(ctx, c.podLister, c.kube, pod, statusMap)
	if err != nil && !util.IsAnnotationAlreadySetError(err) {
		return fmt.Errorf("failed to update DPU connection status of pod %s: %w", podKey, err)
	}
	return nil
}

// handleDeleteDPUPod cleans up all DPU state tracked for podKey, batching the
// port deletions into a single OVSDB transaction. pod must be passed when it
// still exists, so that the connection status it carries can be cleared; a UID
// mismatch means the state belongs to an earlier pod of the same name.
// The state entry is dropped last, so that a retry still finds the ports.
// The podStates lock for podKey must be held by the caller.
func (c *Controller) handleDeleteDPUPod(ctx context.Context, podKey string, pod *corev1.Pod) error {
	ps, ok := c.podStates.Load(podKey)
	if !ok {
		// Nothing is tracked, but the pod can still carry status that this
		// controller never got to clean up.
		if pod != nil {
			return c.reconcileDPUConnStatus(ctx, pod, podKey, nil)
		}
		return nil
	}

	klog.V(5).Infof("Delete for Pod: %s (UID %s)", podKey, ps.uid)
	var portsToDelete []repPortDeletion
	for nadKey, state := range ps.nadStates {
		klog.Infof("Deleting VF representor %s for pod %s NAD %s", state.vfRepName, podKey, nadKey)
		portsToDelete = append(portsToDelete, repPortDeletion{state.vfRepName, state.sandboxId, nadKey})
	}

	// Clear the status before the ports go away, so that the host side is never
	// told a representor is ready after it has been deleted.
	if pod != nil && pod.UID == ps.uid {
		if err := c.reconcileDPUConnStatus(ctx, pod, podKey, nil); err != nil {
			return err
		}
	}

	if len(portsToDelete) > 0 {
		if err := c.deleteRepPorts(ctx, portsToDelete); err != nil {
			return fmt.Errorf("failed to delete representor ports %v for pod %s: %w", portsToDelete, podKey, err)
		}
	}

	c.podStates.Delete(podKey)
	return nil
}

// addRepPort adds the representor of the VF to the ovs bridge. isRevalidation
// says the port was recovered after a restart rather than created here, and is
// kept rather than deleted again if the configuration fails.
func (c *Controller) addRepPort(ctx context.Context, pod *corev1.Pod, state *dpuConnectionState, ifInfo *cni.PodInterfaceInfo,
	getter cni.PodInfoGetter, isRevalidation bool) error {

	nadKey := ifInfo.NADKey
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)

	// set netdevName so OVS interface can be added with external_ids:vf-netdev-name, and is able to
	// be part of healthcheck.
	ifInfo.NetdevName = state.vfRepName
	deviceID, err := util.GetDPUOps().GetDeviceAddress(state.vfRepName)
	if err != nil {
		return fmt.Errorf("failed to get PCI address of VF rep %s for pod %s: %w", state.vfRepName, podDesc, err)
	}

	ovsCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	klog.Infof("Adding VF representor %s for %s", state.vfRepName, podDesc)
	err = cni.ConfigureOVS(ovsCtx, c.ovsClient, pod.Namespace, pod.Name, "", state.vfRepName, ifInfo, state.sandboxId,
		deviceID, false, getter)
	if err != nil {
		if !isRevalidation {
			// Note(adrianc): we are lenient with cleanup in this method as pod is going to be retried anyway.
			_ = c.delRepPort(pod, state, nadKey)
		}
		return err
	}
	klog.Infof("Port %s added to bridge br-int", state.vfRepName)

	// Update connection-status annotation
	// TODO(adrianc): we should update Status in case of error as well
	statusMap := map[string]*util.DPUConnectionStatus{nadKey: {Status: util.DPUConnectionStatusReady, Reason: ""}}
	err = util.UpdatePodDPUConnStatusWithRetry(ctx, c.podLister, c.kube, pod, statusMap)
	if err != nil && !util.IsAnnotationAlreadySetError(err) {
		if !isRevalidation {
			_ = c.delRepPort(pod, state, nadKey)
		}
		return fmt.Errorf("failed to update connection status annotation for %s: %w", podDesc, err)
	}
	return nil
}

// delRepPort validates and deletes a single representor port from br-int.
// TODO(adrianc): handle: clearPodBandwidth(pr.SandboxID), pr.deletePodConntrack()
func (c *Controller) delRepPort(pod *corev1.Pod, state *dpuConnectionState, nadKey string) error {
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)
	klog.Infof("Deleting VF representor %s for %s", state.vfRepName, podDesc)
	if err := c.deleteRepPorts(context.Background(), []repPortDeletion{{state.vfRepName, state.sandboxId, nadKey}}); err != nil {
		return fmt.Errorf("failed to delete representor port %s from br-int for %s: %w", state.vfRepName, podDesc, err)
	}
	return nil
}

// repPortDeletion retains ownership from the pod state or bootstrap snapshot,
// so a device reused by another sandbox is left alone after taking its lock.
type repPortDeletion struct {
	name, sandboxID, nadKey string
}

func (c *Controller) deleteRepPorts(ctx context.Context, ports []repPortDeletion) error {
	ctx, cancel := context.WithTimeout(ctx, types.OVSDBTimeout)
	defer cancel()
	names := make([]string, 0, len(ports))
	for _, port := range ports {
		names = append(names, port.name)
	}
	release, err := libovsdbops.LockOVSInterfaces(ctx, c.ovsClient, names...)
	if err != nil {
		return fmt.Errorf("failed to lock representor ports %v for deletion: %w", names, err)
	}
	defer release()

	var ops []ovsdb.Operation
	var ownedNames []string
	var errs []error
	for _, expected := range ports {
		iface, err := libovsdbops.GetOVSInterfaceFromDB(ctx, c.ovsClient, expected.name)
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		nadKey := iface.ExternalIDs[types.NADExternalID]
		if nadKey == "" {
			nadKey = types.DefaultNetworkName
		}
		if iface.ExternalIDs["sandbox"] != expected.sandboxID || nadKey != expected.nadKey {
			klog.Infof("Leaving representor %s in place: its sandbox or NAD has changed", expected.name)
			continue
		}
		port, err := libovsdbops.GetOVSPort(c.ovsClient, expected.name)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to get representor port %s for deletion: %w", expected.name, err))
			continue
		}
		bridge, err := libovsdbops.GetBridge(c.ovsClient, "br-int")
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to get br-int for deletion of representor %s: %w", expected.name, err))
			continue
		}
		if len(port.Interfaces) != 1 || port.Interfaces[0] != iface.UUID || !slices.Contains(bridge.Ports, port.UUID) {
			errs = append(errs, fmt.Errorf("representor %s is not a single-interface port on br-int", expected.name))
			continue
		}
		// Also guard against OVS changes by writers outside our lock.
		zero := 0
		guard, err := c.ovsClient.Where(&vswitchd.Interface{UUID: iface.UUID}).Wait(ovsdb.WaitConditionEqual, &zero, iface, &iface.ExternalIDs)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to guard ownership of representor %s for deletion: %w", expected.name, err))
			continue
		}
		// Keep each port's operations separate until construction succeeds.
		portOps, err := libovsdbops.DeletePortWithInterfacesOps(c.ovsClient, guard, port, "br-int")
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to build deletion of representor %s from br-int: %w", expected.name, err))
			continue
		}
		ops = append(ops, portOps...)
		ownedNames = append(ownedNames, expected.name)
	}
	setRepPortInterfacesDown(ownedNames)
	_, err = libovsdbops.TransactAndCheckWithContext(ctx, c.ovsClient, ops)
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to delete representor ports %v from br-int: %w", ownedNames, err))
	} else if len(ownedNames) > 0 {
		klog.Infof("Deleted representor ports %v from br-int", ownedNames)
	}
	return errors.Join(errs...)
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
