package node

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops/ovs"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// dpuConnectionState tracks the per-NAD DPU connection state for a pod stored
// in podNADToDPUCDMap. It uses the resolved VF representor name directly instead
// of PfId/VfId, allowing state to be bootstrapped from OVS external_ids on restart.
type dpuConnectionState struct {
	vfRepName string
	sandboxId string
}

// getNewPodDPUState builds a dpuConnectionState from the current pod annotations for a given NAD.
// Returns (nil, nil, nil) if the pod is not ready for DPU plumbing on this NAD.
// Returns a non-nil error on transient failures (e.g. sysfs lookup) so the caller can retry
// without tearing down existing state.
func (bnnc *BaseNodeNetworkController) getNewPodDPUState(pod *corev1.Pod, nadKey string) (*dpuConnectionState, *util.DPUConnectionDetails, error) {
	dpuCD := bnnc.podReadyToAddDPU(pod, nadKey)
	if dpuCD == nil {
		return nil, nil, nil
	}
	vfRepName, err := util.GetSriovnetOps().GetVfRepresentorDPU(dpuCD.PfId, dpuCD.VfId)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get VF representor for pod %s/%s NAD %s (PfId=%s, VfId=%s): %v",
			pod.Namespace, pod.Name, nadKey, dpuCD.PfId, dpuCD.VfId, err)
	}
	return &dpuConnectionState{
		vfRepName: vfRepName,
		sandboxId: dpuCD.SandboxId,
	}, dpuCD, nil
}

// dpuPodNeedsUpdate returns true if the pod's DPU-relevant annotations changed
// for this specific network. Both DPUConnectionDetailsAnnot and OvnPodAnnotationName
// are JSON maps keyed by NAD annotation key containing all networks' data in a single
// annotation value, so we parse and compare only the entries for this network's NADs.
func (bnnc *BaseNodeNetworkController) dpuPodNeedsUpdate(oldPod, newPod *corev1.Pod) bool {
	if oldPod == nil || newPod == nil {
		return true
	}
	if oldPod.Annotations[util.DPUConnectionDetailsAnnot] == newPod.Annotations[util.DPUConnectionDetailsAnnot] {
		return false
	}

	// A pod's NAD selection is fixed at creation time and never changes for the
	// same UID, so we can use either old or new pod to determine the NAD keys.
	desiredNADs, err := bnnc.getDesiredNADsForDPUPod(newPod)
	if err != nil {
		return true
	}
	if desiredNADs == nil {
		return false
	}

	for nadKey := range desiredNADs {
		oldDPUCD, _ := util.UnmarshalPodDPUConnDetails(oldPod.Annotations, nadKey)
		newDPUCD, _ := util.UnmarshalPodDPUConnDetails(newPod.Annotations, nadKey)
		if !reflect.DeepEqual(oldDPUCD, newDPUCD) {
			return true
		}
	}
	return false
}

// watchPodsDPU sets up a level-driven controller for pod DPU annotations
func (bnnc *BaseNodeNetworkController) watchPodsDPU() error {
	podLister := corev1listers.NewPodLister(bnnc.watchFactory.LocalPodInformer().GetIndexer())
	clientSet := cni.NewClientSet(bnnc.client, podLister)

	controllerConfig := &controller.ControllerConfig[corev1.Pod]{
		RateLimiter:    workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
		Informer:       bnnc.watchFactory.LocalPodInformer(),
		Lister:         podLister.List,
		ObjNeedsUpdate: bnnc.dpuPodNeedsUpdate,
		Reconcile: func(key string) error {
			return bnnc.reconcileDPUPod(key, clientSet)
		},
		Threadiness: 1,
	}

	bnnc.dpuPodController = controller.NewController[corev1.Pod](
		fmt.Sprintf("dpu-pod-%s", bnnc.GetNetworkName()),
		controllerConfig,
	)

	return controller.StartWithInitialSync(bnnc.bootstrapDPUPodMapFromOVS, bnnc.dpuPodController)
}

func (bnnc *BaseNodeNetworkController) stopPodsDPU() {
	if bnnc.dpuPodController != nil {
		controller.Stop(bnnc.dpuPodController)
		bnnc.dpuPodController = nil
	}
}

// reconcileDPUPod reconciles the DPU state for a single pod.
// The key is namespace/name from the controller workqueue. The podNADToDPUCDMap
// is keyed by pod.UID to handle the case where a pod is recreated with the same
// name but a different UID (e.g. StatefulSet after host reboot).
func (bnnc *BaseNodeNetworkController) reconcileDPUPod(key string, clientSet cni.PodInfoGetter) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Failed to split meta namespace cache key %s: %v", key, err)
		return nil
	}

	pod, err := bnnc.watchFactory.GetPod(namespace, name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if err != nil || pod == nil {
		return bnnc.handleDeleteDPUPodByKey(key)
	}

	if util.PodWantsHostNetwork(pod) {
		return bnnc.handleDeleteDPUPodByKey(key)
	}

	// If the UID changed (pod recreated with same name), clean up the old UID's state first
	if oldUIDVal, ok := bnnc.podKeyToUID.Load(key); ok {
		oldUID := oldUIDVal.(k8stypes.UID)
		if oldUID != pod.UID {
			if err := bnnc.handleDeleteDPUPodByUID(oldUID, key); err != nil {
				return err
			}
		}
	}

	return bnnc.handleAddOrUpdateDPUPod(key, pod, clientSet)
}

// Check if the Pod is ready so that we can add its associated DPU to br-int.
// If true, return its dpuConnDetails, otherwise return nil
func (bnnc *BaseNodeNetworkController) podReadyToAddDPU(pod *corev1.Pod, nadKey string) *util.DPUConnectionDetails {
	if bnnc.name != pod.Spec.NodeName {
		klog.V(5).Infof("Pod %s/%s is not scheduled on this node %s", pod.Namespace, pod.Name, bnnc.name)
		return nil
	}

	dpuCD, err := util.UnmarshalPodDPUConnDetails(pod.Annotations, nadKey)
	if err != nil {
		if !util.IsAnnotationNotSetError(err) {
			klog.Errorf("Failed to get DPU annotation for pod %s/%s NAD %s: %v",
				pod.Namespace, pod.Name, nadKey, err)
		} else {
			klog.V(5).Infof("DPU connection details annotation still not found for %s/%s for NAD %s",
				pod.Namespace, pod.Name, nadKey)
		}
		return nil
	}

	return dpuCD
}

func (bnnc *BaseNodeNetworkController) addDPUPodForNAD(pod *corev1.Pod, dpuCD *util.DPUConnectionDetails,
	netName, nadKey string, getter cni.PodInfoGetter) error {
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)
	klog.Infof("Adding %s on DPU", podDesc)
	podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations, nil,
		string(pod.UID), "", nadKey, netName, bnnc.MTU())
	if err != nil {
		return fmt.Errorf("failed to get pod interface information of %s: %v. retrying", podDesc, err)
	}
	err = bnnc.addRepPort(pod, dpuCD, podInterfaceInfo, getter)
	if err != nil {
		return fmt.Errorf("failed to add rep port for %s, %v. retrying", podDesc, err)
	}
	return nil
}

func (bnnc *BaseNodeNetworkController) delDPUPodForNAD(pod *corev1.Pod, state *dpuConnectionState, nadKey string, podDeleted bool) error {
	var errs []error
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)
	klog.Infof("Deleting %s from DPU", podDesc)

	// no need to unset connection status annotation if pod is deleted anyway
	if !podDeleted {
		err := bnnc.updatePodDPUConnStatusWithRetry(pod, nil, nadKey)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to remove the old DPU connection status annotation for %s: %v", podDesc, err))
		}
	}

	err := bnnc.delRepPort(pod, state.sandboxId, state.vfRepName, nadKey)
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to delete VF representor for %s: %v", podDesc, err))
	}
	return utilerrors.Join(errs...)
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

// handleAddOrUpdateDPUPod reconciles DPU state for a pod that exists
func (bnnc *BaseNodeNetworkController) handleAddOrUpdateDPUPod(podKey string, pod *corev1.Pod, clientSet cni.PodInfoGetter) error {
	netName := bnnc.GetNetworkName()

	desiredNADs, err := bnnc.getDesiredNADsForDPUPod(pod)
	if err != nil {
		return err
	}
	if desiredNADs == nil {
		return bnnc.handleDeleteDPUPodByKey(podKey)
	}

	var existingNADMap map[string]*dpuConnectionState
	if v, ok := bnnc.podNADToDPUCDMap.Load(pod.UID); ok {
		existingNADMap = v.(map[string]*dpuConnectionState)
	}

	klog.V(5).Infof("Reconcile for Pod: %s (UID %s) for network %s", podKey, pod.UID, netName)

	var errs []error
	newNADMap := make(map[string]*dpuConnectionState)
	for nadKey := range desiredNADs {
		var oldState *dpuConnectionState
		if existingNADMap != nil {
			oldState = existingNADMap[nadKey]
		}
		newState, dpuCD, err := bnnc.getNewPodDPUState(pod, nadKey)
		if err != nil {
			klog.Errorf("Error resolving DPU state for pod %s NAD %s: %v", podKey, nadKey, err)
			errs = append(errs, err)
			newNADMap[nadKey] = oldState
			continue
		}

		if !dpuConnectionStateChanged(oldState, newState) {
			newNADMap[nadKey] = oldState
			continue
		}

		if oldState != nil {
			klog.Infof("Deleting the old VF for pod %s NAD %s since either kubelet issued cmdDEL or assigned a new VF or "+
				"the sandbox id itself changed. Old state (%+v), New state (%+v)",
				podKey, nadKey, oldState, newState)
			if err := bnnc.delDPUPodForNAD(pod, oldState, nadKey, false); err != nil {
				klog.Errorf("Error deleting old state for pod %s NAD %s: %v", podKey, nadKey, err)
				errs = append(errs, err)
				newNADMap[nadKey] = oldState
				continue
			}
		}

		if newState != nil {
			if err := bnnc.addDPUPodForNAD(pod, dpuCD, netName, nadKey, clientSet); err != nil {
				klog.Errorf("Error adding pod %s for network %s: %v", podKey, bnnc.GetNetworkName(), err)
				errs = append(errs, err)
				newNADMap[nadKey] = nil
				continue
			}
			newNADMap[nadKey] = newState
		} else {
			newNADMap[nadKey] = nil
		}
	}

	// Clean up NADs that existed before but are no longer desired
	for nadKey, state := range existingNADMap {
		if _, exists := desiredNADs[nadKey]; !exists && state != nil {
			if err := bnnc.delDPUPodForNAD(pod, state, nadKey, false); err != nil {
				klog.Errorf("Error deleting stale NAD %s for pod %s: %v", nadKey, podKey, err)
				errs = append(errs, err)
				newNADMap[nadKey] = state
			}
		}
	}

	bnnc.podNADToDPUCDMap.Store(pod.UID, newNADMap)
	bnnc.podKeyToUID.Store(podKey, pod.UID)
	return utilerrors.Join(errs...)
}

// handleDeleteDPUPodByKey cleans up DPU state using the namespace/name key to find the UID.
// Only removes the podKeyToUID entry after all NAD state has been successfully cleaned up,
// so that retries can still locate the UID.
func (bnnc *BaseNodeNetworkController) handleDeleteDPUPodByKey(podKey string) error {
	uidVal, ok := bnnc.podKeyToUID.Load(podKey)
	if !ok {
		return nil
	}
	uid := uidVal.(k8stypes.UID)
	if err := bnnc.handleDeleteDPUPodByUID(uid, podKey); err != nil {
		return err
	}
	bnnc.podKeyToUID.Delete(podKey)
	return nil
}

// handleDeleteDPUPodByUID cleans up DPU state for a specific pod UID
func (bnnc *BaseNodeNetworkController) handleDeleteDPUPodByUID(uid k8stypes.UID, podKey string) error {
	v, ok := bnnc.podNADToDPUCDMap.Load(uid)
	if !ok {
		return nil
	}
	nadStateMap := v.(map[string]*dpuConnectionState)

	namespace, name, _ := cache.SplitMetaNamespaceKey(podKey)
	podRef := &corev1.Pod{}
	podRef.Namespace = namespace
	podRef.Name = name

	klog.V(5).Infof("Delete for Pod: %s (UID %s) for network %s", podKey, uid, bnnc.GetNetworkName())
	var errs []error
	for nadKey, state := range nadStateMap {
		if state != nil {
			if err := bnnc.delDPUPodForNAD(podRef, state, nadKey, true); err != nil {
				errs = append(errs, err)
				klog.Errorf("Error deleting pod %s NAD %s for network %s: %v", podKey, nadKey, bnnc.GetNetworkName(), err)
			} else {
				delete(nadStateMap, nadKey)
			}
		} else {
			delete(nadStateMap, nadKey)
		}
	}
	if len(errs) > 0 {
		bnnc.podNADToDPUCDMap.Store(uid, nadStateMap)
		return utilerrors.Join(errs...)
	}
	bnnc.podNADToDPUCDMap.Delete(uid)
	return nil
}

// getDesiredNADsForDPUPod determines which NADs this pod should have on this network.
// Returns (nil, nil) if the pod doesn't belong to this network.
// Returns a non-nil error on transient failures so the caller can retry.
func (bnnc *BaseNodeNetworkController) getDesiredNADsForDPUPod(pod *corev1.Pod) (map[string]struct{}, error) {
	netName := bnnc.GetNetworkName()
	var activeNetwork util.NetInfo
	var err error

	result := make(map[string]struct{})

	if bnnc.IsUserDefinedNetwork() {
		if bnnc.IsPrimaryNetwork() {
			activeNetwork, err = bnnc.networkManager.GetActiveNetworkForNamespace(pod.Namespace)
			if err != nil {
				return nil, fmt.Errorf("failed looking for the active network for namespace %s: %v", pod.Namespace, err)
			}
			if activeNetwork == nil {
				return nil, fmt.Errorf("unable to find an active network for namespace %s", pod.Namespace)
			}
			if activeNetwork.GetNetworkName() != netName {
				return nil, nil
			}
		}

		on, networkMap, err := util.GetPodNADToNetworkMappingWithActiveNetwork(
			pod,
			bnnc.GetNetInfo(),
			activeNetwork,
			bnnc.networkManager.GetNetworkNameForNADKey,
			bnnc.networkManager.GetPrimaryNADForNamespace,
		)
		if err != nil {
			return nil, fmt.Errorf("error getting network-attachment for pod %s/%s network %s: %v",
				pod.Namespace, pod.Name, netName, err)
		}
		if !on {
			klog.V(5).Infof("Skipping Pod %s/%s as it is not attached to network: %s",
				pod.Namespace, pod.Name, netName)
			return nil, nil
		}

		for nadKey := range networkMap {
			result[nadKey] = struct{}{}
		}
	} else {
		result[types.DefaultNetworkName] = struct{}{}
	}
	return result, nil
}

// updatePodDPUConnStatusWithRetry update the pod annotion with the givin connection details
func (bnnc *BaseNodeNetworkController) updatePodDPUConnStatusWithRetry(origPod *corev1.Pod,
	dpuConnStatus *util.DPUConnectionStatus, nadKey string) error {
	podDesc := fmt.Sprintf("pod %s/%s", origPod.Namespace, origPod.Name)
	klog.Infof("Updating pod %s with connection status (%+v) for NAD %s", podDesc, dpuConnStatus, nadKey)
	err := util.UpdatePodDPUConnStatusWithRetry(
		bnnc.watchFactory.PodCoreInformer().Lister(),
		bnnc.Kube,
		origPod,
		dpuConnStatus,
		nadKey,
	)
	if util.IsAnnotationAlreadySetError(err) {
		return nil
	}

	return err
}

// addRepPort adds the representor of the VF to the ovs bridge
func (bnnc *BaseNodeNetworkController) addRepPort(pod *corev1.Pod, dpuCD *util.DPUConnectionDetails, ifInfo *cni.PodInterfaceInfo, getter cni.PodInfoGetter) error {

	nadKey := ifInfo.NADKey
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)
	vfRepName, err := util.GetSriovnetOps().GetVfRepresentorDPU(dpuCD.PfId, dpuCD.VfId)
	if err != nil {
		klog.Infof("Failed to get VF representor for %s dpuConnDetail %+v: %v", podDesc, dpuCD, err)
		return err
	}

	// set netdevName so OVS interface can be added with external_ids:vf-netdev-name, and is able to
	// be part of healthcheck.
	ifInfo.NetdevName = vfRepName
	vfPciAddress, err := util.GetSriovnetOps().GetPCIFromDeviceName(vfRepName)
	if err != nil {
		klog.Infof("Failed to get PCI address of VF rep %s: %v", vfRepName, err)
		return err
	}

	klog.Infof("Adding VF representor %s for %s", vfRepName, podDesc)
	err = cni.ConfigureOVS(context.TODO(), pod.Namespace, pod.Name, "", vfRepName, ifInfo, dpuCD.SandboxId, vfPciAddress, getter)
	if err != nil {
		// Note(adrianc): we are lenient with cleanup in this method as pod is going to be retried anyway.
		_ = bnnc.delRepPort(pod, dpuCD.SandboxId, vfRepName, nadKey)
		return err
	}
	klog.Infof("Port %s added to bridge br-int", vfRepName)

	link, err := util.GetNetLinkOps().LinkByName(vfRepName)
	if err != nil {
		_ = bnnc.delRepPort(pod, dpuCD.SandboxId, vfRepName, nadKey)
		return fmt.Errorf("failed to get link device for interface %s", vfRepName)
	}

	if err = util.GetNetLinkOps().LinkSetMTU(link, ifInfo.MTU); err != nil {
		_ = bnnc.delRepPort(pod, dpuCD.SandboxId, vfRepName, nadKey)
		return fmt.Errorf("failed to setup representor port. failed to set MTU for interface %s", vfRepName)
	}

	if err = util.GetNetLinkOps().LinkSetUp(link); err != nil {
		_ = bnnc.delRepPort(pod, dpuCD.SandboxId, vfRepName, nadKey)
		return fmt.Errorf("failed to setup representor port. failed to set link up for interface %s", vfRepName)
	}

	// Update connection-status annotation
	// TODO(adrianc): we should update Status in case of error as well
	connStatus := util.DPUConnectionStatus{Status: util.DPUConnectionStatusReady, Reason: ""}
	err = bnnc.updatePodDPUConnStatusWithRetry(pod, &connStatus, nadKey)
	if err != nil {
		_ = util.GetNetLinkOps().LinkSetDown(link)
		_ = bnnc.delRepPort(pod, dpuCD.SandboxId, vfRepName, nadKey)
		return fmt.Errorf("failed to setup representor port. failed to set pod annotations. %v", err)
	}
	return nil
}

// delRepPort delete the representor of the VF from the ovs bridge
func (bnnc *BaseNodeNetworkController) delRepPort(pod *corev1.Pod, sandboxId, vfRepName, nadKey string) error {
	//TODO(adrianc): handle: clearPodBandwidth(pr.SandboxID), pr.deletePodConntrack()
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadKey)
	klog.Infof("Delete VF representor %s for %s", vfRepName, podDesc)
	ifExists, sandbox, expectedNADKey, err := util.GetOVSPortPodInfo(vfRepName)
	if err != nil {
		return err
	}
	if !ifExists {
		klog.Infof("VF representor %s for %s is not an OVS interface, nothing to do", vfRepName, podDesc)
		return nil
	}
	if sandbox != sandboxId {
		return fmt.Errorf("OVS port %s was added for sandbox (%s), expecting (%s)", vfRepName, sandbox, sandboxId)
	}
	if expectedNADKey != nadKey {
		return fmt.Errorf("OVS port %s was added for NAD key (%s), expecting (%s)", vfRepName, expectedNADKey, nadKey)
	}
	// Set link down for representor port
	link, err := util.GetNetLinkOps().LinkByName(vfRepName)
	if err != nil {
		klog.Warningf("Failed to get link device for representor port %s. %v", vfRepName, err)
	} else {
		if err = util.GetNetLinkOps().LinkSetDown(link); err != nil {
			klog.Warningf("Failed to set link down for representor port %s. %v", vfRepName, err)
		}
	}

	// remove from br-int
	return wait.PollUntilContextTimeout(context.Background(), 500*time.Millisecond, 60*time.Second, true, func(_ context.Context) (bool, error) {
		_, _, err := util.RunOVSVsctl("--if-exists", "del-port", "br-int", vfRepName)
		if err != nil {
			return false, nil
		}
		klog.Infof("Port %s deleted from bridge br-int", vfRepName)
		return true, nil
	})
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

// bootstrapDPUPodMapFromOVS pre-populates podNADToDPUCDMap and podKeyToUID from
// existing OVS representor ports, and removes orphaned representor ports whose pods
// no longer exist. This ensures that after an ovnkube-node-dpu restart, the reconcile
// loop can detect changes in DPU connection details, and stale ports left behind by
// deleted pods are cleaned up.
func (bnnc *BaseNodeNetworkController) bootstrapDPUPodMapFromOVS() error {
	// find all representor interfaces
	p := func(item *vswitchd.Interface) bool {
		if item.ExternalIDs["sandbox"] != "" && item.ExternalIDs["vf-netdev-name"] != "" {
			return true
		}
		return false
	}

	ovsIfaces, err := ovsops.FindInterfacesWithPredicate(bnnc.ovsClient, p)
	if err != nil {
		return fmt.Errorf("failed to find representor interfaces when %s network controller bootstrap: %w", bnnc.GetNetworkName(), err)
	}
	if len(ovsIfaces) == 0 {
		return nil
	}

	// Build a set of existing pod UIDs from the informer cache so we can
	// identify orphaned OVS representor ports whose pods have been deleted.
	existingUIDs := make(map[k8stypes.UID]bool)
	pods, err := bnnc.watchFactory.GetAllPods()
	if err != nil {
		return fmt.Errorf("failed to list pods for DPU bootstrap: %v", err)
	}
	for _, pod := range pods {
		existingUIDs[pod.UID] = true
	}

	netName := bnnc.GetNetworkName()

	for _, ovsIface := range ovsIfaces {
		ovsNetName := ovsIface.ExternalIDs[types.NetworkExternalID]
		if ovsNetName == "" {
			if !bnnc.IsDefault() {
				continue
			}
		} else if ovsNetName != netName {
			continue
		}

		podUID := ovsIface.ExternalIDs["iface-id-ver"]
		sandbox := ovsIface.ExternalIDs["sandbox"]
		repName := ovsIface.ExternalIDs["vf-netdev-name"]
		ifaceID := ovsIface.ExternalIDs["iface-id"]
		nadKey := ovsIface.ExternalIDs[types.NADExternalID]
		if nadKey == "" {
			nadKey = types.DefaultNetworkName
		}

		if podUID == "" || sandbox == "" || repName == "" || ifaceID == "" {
			continue
		}

		uid := k8stypes.UID(podUID)
		podKey := podKeyFromIfaceID(ifaceID, nadKey)

		if !existingUIDs[uid] {
			klog.Infof("DPU bootstrap: removing orphaned representor %s (pod UID %s, NAD %s): pod no longer exists", repName, podUID, nadKey)
			podRef := &corev1.Pod{}
			if podKey != "" {
				namespace, name, _ := cache.SplitMetaNamespaceKey(podKey)
				podRef.Namespace = namespace
				podRef.Name = name
			}
			if err := bnnc.delRepPort(podRef, sandbox, repName, nadKey); err != nil {
				klog.Errorf("DPU bootstrap: failed to remove orphaned representor %s: %v", repName, err)
			}
			continue
		}

		state := &dpuConnectionState{
			vfRepName: repName,
			sandboxId: sandbox,
		}

		var nadMap map[string]*dpuConnectionState
		if v, ok := bnnc.podNADToDPUCDMap.Load(uid); ok {
			nadMap = v.(map[string]*dpuConnectionState)
		} else {
			nadMap = make(map[string]*dpuConnectionState)
		}
		nadMap[nadKey] = state
		bnnc.podNADToDPUCDMap.Store(uid, nadMap)

		if podKey != "" {
			bnnc.podKeyToUID.Store(podKey, uid)
		}
	}

	klog.Infof("DPU bootstrap for network %s: populated state from OVS representor ports", netName)
	return nil
}

// cleanupAllOVSRepresentors removes all OVS representor ports belongs to this network.
// It is called when the network is deleted and cleaned up.
// checkForStaleOVSRepresentorInterfaces only removes ports whose pod no longer
// exists, but does not handle the case where the pod is still running and the
// network itself is deleted.
func (bnnc *BaseNodeNetworkController) cleanupAllOVSRepresentors() error {
	// find all representor interfaces of the network
	p := func(item *vswitchd.Interface) bool {
		if item.ExternalIDs["sandbox"] == "" || item.ExternalIDs["vf-netdev-name"] == "" {
			return false
		}
		// this function is only called for non-default network
		return item.ExternalIDs[types.NetworkExternalID] == bnnc.GetNetworkName()
	}

	ovsIfaces, err := ovsops.FindInterfacesWithPredicate(bnnc.ovsClient, p)
	if err != nil {
		return fmt.Errorf("failed to find representor interfaces when cleaning up network %s: %w", bnnc.GetNetworkName(), err)
	}
	if len(ovsIfaces) == 0 {
		return nil
	}

	interfaces := make([]string, 0, len(ovsIfaces))
	for _, ovsIface := range ovsIfaces {
		interfaces = append(interfaces, ovsIface.Name)
		link, err := util.GetNetLinkOps().LinkByName(ovsIface.Name)
		if err != nil {
			klog.Warningf("Failed to get link device for representor port %s. %v", ovsIface.Name, err)
		} else if err = util.GetNetLinkOps().LinkSetDown(link); err != nil {
			klog.Warningf("Failed to set link down for representor port %s. %v", ovsIface.Name, err)
		}
	}

	err = ovsops.DeletePortsFromBridge(bnnc.ovsClient, "br-int", interfaces...)
	if err != nil {
		return fmt.Errorf("failed to delete representor interfaces %s of network %s: %w", interfaces, bnnc.GetNetworkName(), err)
	}
	return nil
}
