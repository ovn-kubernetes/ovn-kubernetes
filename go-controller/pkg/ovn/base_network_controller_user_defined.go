package ovn

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/persistentips"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

func (bsnc *BaseUserDefinedNetworkController) getPortInfoForUserDefinedNetwork(pod *corev1.Pod) map[string]*lpInfo {
	if util.PodWantsHostNetwork(pod) {
		return nil
	}
	portInfoMap, _ := bsnc.logicalPortCache.getAll(pod)
	return portInfoMap
}

// GetInternalCacheEntryForUserDefinedNetwork returns the internal cache entry for this object, given an object and its type.
// This is now used only for pods, which will get their the logical port cache entry.
func (bsnc *BaseUserDefinedNetworkController) GetInternalCacheEntryForUserDefinedNetwork(objType reflect.Type, obj interface{}) interface{} {
	switch objType {
	case factory.PodType:
		pod := obj.(*corev1.Pod)
		return bsnc.getPortInfoForUserDefinedNetwork(pod)
	default:
		return nil
	}
}

// AddUserDefinedNetworkResourceCommon adds the specified object to the cluster according to its type and returns the error,
// if any, yielded during object creation. This function is called for User Defined Networks only.
func (bsnc *BaseUserDefinedNetworkController) AddUserDefinedNetworkResourceCommon(objType reflect.Type, obj interface{}) error {
	switch objType {
	case factory.PodType:
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("could not cast %T object to *knet.Pod", obj)
		}
		return bsnc.ensurePodForUserDefinedNetwork(pod, true)

	case factory.NamespaceType:
		ns, ok := obj.(*corev1.Namespace)
		if !ok {
			return fmt.Errorf("could not cast %T object to *kapi.Namespace", obj)
		}
		return bsnc.AddNamespaceForUserDefinedNetwork(ns)

	case factory.MultiNetworkPolicyType:
		mp, ok := obj.(*mnpapi.MultiNetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *multinetworkpolicyapi.MultiNetworkPolicy", obj)
		}

		if !bsnc.shouldApplyMultiPolicy(mp) {
			return nil
		}

		np, err := bsnc.convertMultiNetPolicyToNetPolicy(mp)
		if err != nil {
			return err
		}
		if err := bsnc.addNetworkPolicy(np); err != nil {
			klog.Infof("MultiNetworkPolicy add failed for %s/%s, will try again later: %v",
				mp.Namespace, mp.Name, err)
			return err
		}
	case factory.IPAMClaimsType:
		return nil

	default:
		return bsnc.AddResourceCommon(objType, obj)
	}
	return nil
}

// UpdateUserDefinedNetworkResourceCommon updates the specified object in the cluster to its version in newObj
// according to its type and returns the error, if any, yielded during the object update. This function is
// called for User Defined Networks only.
// Given an old and a new object; The inRetryCache boolean argument is to indicate if the given resource
// is in the retryCache or not.
func (bsnc *BaseUserDefinedNetworkController) UpdateUserDefinedNetworkResourceCommon(objType reflect.Type, oldObj, newObj interface{}, inRetryCache bool) error {
	switch objType {
	case factory.PodType:
		oldPod := oldObj.(*corev1.Pod)
		newPod := newObj.(*corev1.Pod)

		return bsnc.ensurePodForUserDefinedNetwork(newPod, shouldAddPort(oldPod, newPod, inRetryCache))

	case factory.NamespaceType:
		oldNs, newNs := oldObj.(*corev1.Namespace), newObj.(*corev1.Namespace)
		return bsnc.updateNamespaceForUserDefinedNetwork(oldNs, newNs)

	case factory.MultiNetworkPolicyType:
		oldMp, ok := oldObj.(*mnpapi.MultiNetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *multinetworkpolicyapi.MultiNetworkPolicy", oldObj)
		}
		newMp, ok := newObj.(*mnpapi.MultiNetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *multinetworkpolicyapi.MultiNetworkPolicy", newObj)
		}

		oldShouldApply := bsnc.shouldApplyMultiPolicy(oldMp)
		newShouldApply := bsnc.shouldApplyMultiPolicy(newMp)
		if oldShouldApply {
			// this multi-netpol no longer applies to this network controller, delete it
			np, err := bsnc.convertMultiNetPolicyToNetPolicy(oldMp)
			if err != nil {
				return err
			}
			if err := bsnc.deleteNetworkPolicy(np); err != nil {
				klog.Infof("MultiNetworkPolicy delete failed for %s/%s, will try again later: %v",
					oldMp.Namespace, oldMp.Name, err)
				return err
			}
		}
		if newShouldApply {
			// now this multi-netpol applies to this network controller
			np, err := bsnc.convertMultiNetPolicyToNetPolicy(newMp)
			if err != nil {
				return err
			}
			if err := bsnc.addNetworkPolicy(np); err != nil {
				klog.Infof("MultiNetworkPolicy add failed for %s/%s, will try again later: %v",
					newMp.Namespace, newMp.Name, err)
				return err
			}
		}
	case factory.IPAMClaimsType:
		return nil

	default:
		return fmt.Errorf("object type %s not supported", objType)
	}
	return nil
}

// DeleteUserDefinedNetworkResourceCommon deletes the object from the cluster according to the delete logic of its resource type.
// Given an object and optionally a cachedObj; cachedObj is the internal cache entry for this object,
// used for now for pods.
// This function is called for User Defined Networks only.
func (bsnc *BaseUserDefinedNetworkController) DeleteUserDefinedNetworkResourceCommon(objType reflect.Type, obj, cachedObj interface{}) error {
	switch objType {
	case factory.PodType:
		var portInfoMap map[string]*lpInfo
		pod := obj.(*corev1.Pod)

		if cachedObj != nil {
			portInfoMap = cachedObj.(map[string]*lpInfo)
		}
		return bsnc.removePodForUserDefinedNetwork(pod, portInfoMap)

	case factory.NamespaceType:
		ns := obj.(*corev1.Namespace)
		return bsnc.deleteNamespaceForUserDefinedNetwork(ns)

	case factory.MultiNetworkPolicyType:
		mp, ok := obj.(*mnpapi.MultiNetworkPolicy)
		if !ok {
			return fmt.Errorf("could not cast %T object to *multinetworkpolicyapi.MultiNetworkPolicy", obj)
		}
		np, err := bsnc.convertMultiNetPolicyToNetPolicy(mp)
		if err != nil {
			return err
		}
		// delete this policy regardless it applies to this network controller, in case of missing update event
		if err := bsnc.deleteNetworkPolicy(np); err != nil {
			klog.Infof("MultiNetworkPolicy delete failed for %s/%s, will try again later: %v",
				mp.Namespace, mp.Name, err)
			return err
		}

	case factory.IPAMClaimsType:
		ipamClaim, ok := obj.(*ipamclaimsapi.IPAMClaim)
		if !ok {
			return fmt.Errorf("could not cast obj of type %T to *ipamclaimsapi.IPAMClaim", obj)
		}

		switchName, err := bsnc.getExpectedSwitchName(dummyPod())
		if err != nil {
			return err
		}
		ipAllocator := bsnc.lsManager.ForSwitch(switchName)
		err = bsnc.ipamClaimsReconciler.Reconcile(ipamClaim, nil, ipAllocator)
		if err != nil && !errors.Is(err, persistentips.ErrIgnoredIPAMClaim) {
			return fmt.Errorf("error deleting IPAMClaim: %w", err)
		} else if errors.Is(err, persistentips.ErrIgnoredIPAMClaim) {
			return nil // let's avoid the log below, since nothing was released.
		}
		klog.Infof("Released IPs %q for network %q", ipamClaim.Status.IPs, ipamClaim.Spec.Network)

	default:
		return bsnc.DeleteResourceCommon(objType, obj)
	}
	return nil
}

// ensurePodForUserDefinedNetwork tries to set up the User Defined Network for a pod. It returns nil on success and error
// on failure; failure indicates the pod set up should be retried later.
func (bsnc *BaseUserDefinedNetworkController) ensurePodForUserDefinedNetwork(pod *corev1.Pod, addPort bool) error {
	// Try unscheduled pods later
	if !util.PodScheduled(pod) {
		return nil
	}

	if util.PodWantsHostNetwork(pod) {
		return nil
	}

	var kubevirtLiveMigrationStatus *kubevirt.LiveMigrationStatus
	var err error

	if kubevirt.IsPodAllowedForMigration(pod, bsnc.GetNetInfo()) {
		kubevirtLiveMigrationStatus, err = kubevirt.DiscoverLiveMigrationStatus(bsnc.watchFactory, pod)
		if err != nil {
			return fmt.Errorf("failed to discover Live-migration status: %w", err)
		}
	}
	updatePort := kubevirtLiveMigrationStatus != nil && pod.Name == kubevirtLiveMigrationStatus.TargetPod.Name

	if !addPort && !updatePort {
		return nil
	}

	// If a node does not have an assigned hostsubnet don't wait for the logical switch to appear
	var switchName string
	switchName, err = bsnc.getExpectedSwitchName(pod)
	if err != nil {
		return err
	}

	var activeNetwork util.NetInfo
	if bsnc.IsPrimaryNetwork() {
		activeNetwork, err = bsnc.networkManager.GetActiveNetworkForNamespace(pod.Namespace)
		if err != nil {
			return fmt.Errorf("failed looking for the active network at namespace '%s': %w", pod.Namespace, err)
		}
	}

	on, networkMap, err := util.GetPodNADToNetworkMappingWithActiveNetwork(pod, bsnc.GetNetInfo(), activeNetwork)
	if err != nil {
		bsnc.recordPodErrorEvent(pod, err)
		// configuration error, no need to retry, do not return error
		klog.Errorf("Error getting network-attachment for pod %s/%s network %s: %v",
			pod.Namespace, pod.Name, bsnc.GetNetworkName(), err)
		return nil
	}

	if !on {
		// the pod is not attached to this specific network
		klog.V(5).Infof("Pod %s/%s is not attached on this network controller %s",
			pod.Namespace, pod.Name, bsnc.GetNetworkName())
		return nil
	}

	if bsnc.isNonHostSubnetSwitch(switchName) {
		klog.V(5).Infof(
			"Pod %s/%s requires IPAM but does not have an assigned IP address", pod.Namespace, pod.Name)
		return nil
	}

	var errs []error
	for nadKey, network := range networkMap {
		if err = bsnc.addLogicalPortToNetworkForNAD(pod, nadKey, switchName, network, kubevirtLiveMigrationStatus); err != nil {
			errs = append(errs, fmt.Errorf("failed to add logical port of Pod %s/%s for NAD key %s: %w", pod.Namespace, pod.Name, nadKey, err))
		}
	}
	if len(errs) != 0 {
		return utilerrors.Join(errs...)
	}
	return nil
}

func (bsnc *BaseUserDefinedNetworkController) addLogicalPortToNetworkForNAD(pod *corev1.Pod, nadKey, switchName string,
	network *nadapi.NetworkSelectionElement, kubevirtLiveMigrationStatus *kubevirt.LiveMigrationStatus,
) error {
	var libovsdbExecuteTime time.Duration

	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort for NAD key %s took %v, libovsdb time %v",
			pod.Namespace, pod.Name, nadKey, time.Since(start), libovsdbExecuteTime)
	}()

	var err error
	var podAnnotation *util.PodAnnotation
	var ops []ovsdb.Operation
	var lsp *nbdb.LogicalSwitchPort
	var newlyCreated bool

	var lspEnabled *bool
	// actions on the pods' LSP are only triggerred from the target pod
	shouldHandleLiveMigration := kubevirtLiveMigrationStatus != nil && pod.Name == kubevirtLiveMigrationStatus.TargetPod.Name
	if shouldHandleLiveMigration {
		// LSP should be altered inside addLogicalPortToNetwork() before ops are generated because one cannot append
		// multiple ops regarding the same object in the same transact, so passing enabled parameter.
		lspEnabled = ptr.To(kubevirtLiveMigrationStatus.IsTargetDomainReady())
	}

	// we need to create a logical port for all local pods
	// we also need to create a remote logical port for remote pods on layer2
	// topologies with interconnect
	isLocalPod := bsnc.isPodScheduledinLocalZone(pod)
	requiresLogicalPort := isLocalPod || bsnc.isLayer2Interconnect()

	if requiresLogicalPort {
		ops, lsp, podAnnotation, newlyCreated, err = bsnc.addLogicalPortToNetwork(pod, nadKey, network, lspEnabled)
		if err != nil {
			return err
		}
	} else if bsnc.TopologyType() == types.LocalnetTopology {
		// On localnet networks, we might be processing the pod as a result of a
		// node changing zone local -> remote so cleanup the logical port in
		// case it exists and is no longer needed.
		// This should be an idempotent operation.
		// Not needed for layer3 networks as in that case the whole node switch
		// is removed
		// No need to release IPs as those are allocated from cluster manager
		logicalPort := bsnc.GetLogicalPortName(pod, nadKey)
		expectedSwitchName, err := bsnc.getExpectedSwitchName(pod)
		if err != nil {
			return err
		}
		ops, err = bsnc.delLSPOps(logicalPort, expectedSwitchName, "")
		if err != nil {
			return err
		}
		bsnc.logicalPortCache.remove(pod, nadKey)
	}

	if shouldHandleLiveMigration &&
		kubevirtLiveMigrationStatus.IsTargetDomainReady() &&
		// At localnet there is no source pod remote LSP so it should be skipped
		(bsnc.TopologyType() != types.LocalnetTopology || bsnc.isPodScheduledinLocalZone(kubevirtLiveMigrationStatus.SourcePod)) {
		ops, err = bsnc.disableLiveMigrationSourceLSPOps(kubevirtLiveMigrationStatus, nadKey, ops)
		if err != nil {
			return fmt.Errorf("failed to create LSP ops for source pod during Live-migration status: %w", err)
		}
	}

	if podAnnotation == nil {
		podAnnotation, err = util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
		if err != nil {
			return err
		}
	}

	if bsnc.doesNetworkRequireIPAM() &&
		(util.IsMultiNetworkPoliciesSupportEnabled() || (util.IsNetworkSegmentationSupportEnabled() && bsnc.IsPrimaryNetwork())) {
		// Ensure the namespace/nsInfo exists
		portUUID := ""
		if lsp != nil {
			portUUID = lsp.UUID
		}
		addOps, err := bsnc.addPodToNamespaceForUserDefinedNetwork(pod.Namespace, podAnnotation.IPs, portUUID)
		if err != nil {
			return err
		}
		ops = append(ops, addOps...)
	}

	recordOps, txOkCallBack, _, err := bsnc.AddConfigDurationRecord("pod", pod.Namespace, pod.Name)
	if err != nil {
		klog.Errorf("Config duration recorder: %v", err)
	}
	ops = append(ops, recordOps...)

	transactStart := time.Now()
	_, err = libovsdbops.TransactAndCheckAndSetUUIDs(bsnc.nbClient, lsp, ops)
	libovsdbExecuteTime = time.Since(transactStart)
	if err != nil {
		return fmt.Errorf("error transacting operations %+v: %v", ops, err)
	}
	txOkCallBack()

	// For Layer 3 interconnect with multi-VTEP, ensure transit switch port exists for this pod's encap IP
	if isLocalPod && bsnc.isLayer3Interconnect() {
		klog.V(5).Infof("Ensuring transit switch port for local pod %s/%s in network %s", pod.Namespace, pod.Name, bsnc.GetNetworkName())
		if err = bsnc.zoneICHandler.EnsureLocalNodeTransitSwitchPortForPod(pod, podAnnotation); err != nil {
			return fmt.Errorf("failed to ensure transit switch port for local pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	// For Layer 3 interconnect with multi-VTEP, ensure remote transit switch port exists for this pod's encap IP
	if !isLocalPod && bsnc.isLayer3Interconnect() {
		klog.V(5).Infof("Ensuring transit switch port for remote pod %s/%s in network %s", pod.Namespace, pod.Name, bsnc.GetNetworkName())
		if err = bsnc.zoneICHandler.EnsureRemoteNodeTransitSwitchPortForPod(pod, podAnnotation); err != nil {
			return fmt.Errorf("failed to ensure remote transit switch port for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	// set remote layer 2 LSP's Port Binding's encap field according to encap ip in the pod annotation
	if !isLocalPod && bsnc.isLayer2Interconnect() && podAnnotation.EncapIP != "" {
		nodeObj, err := bsnc.watchFactory.GetNode(pod.Spec.NodeName)
		if err != nil {
			return fmt.Errorf("failed to fetch node %s: %w", pod.Spec.NodeName, err)
		}
		chassisID, err := util.ParseNodeChassisIDAnnotation(nodeObj)
		if err != nil || chassisID == "" {
			return fmt.Errorf("failed to parse chassis ID for node %s: %w", pod.Spec.NodeName, err)
		}
		if err = libovsdbops.UpdatePortBindingSetEncap(bsnc.sbClient, lsp.Name, chassisID, podAnnotation.EncapIP); err != nil {
			return fmt.Errorf("failed to update port binding for %s: %w", lsp.Name, err)
		}
	}

	if lsp != nil {
		_ = bsnc.logicalPortCache.add(pod, switchName, nadKey, lsp.UUID, podAnnotation.MAC, podAnnotation.IPs)
		if bsnc.requireDHCP(pod) {
			if err := bsnc.ensureDHCP(pod, podAnnotation, lsp); err != nil {
				return err
			}
		}
	}

	if isLocalPod {
		bsnc.podRecorder.AddLSP(pod.UID, bsnc.GetNetInfo())
		if newlyCreated {
			metrics.RecordPodCreated(pod, bsnc.GetNetInfo())
		}
	}

	return nil
}

// removePodForUserDefinedNetwork tried to tear down a pod. It returns nil on success and error on failure;
// failure indicates the pod tear down should be retried later.
func (bsnc *BaseUserDefinedNetworkController) removePodForUserDefinedNetwork(pod *corev1.Pod, portInfoMap map[string]*lpInfo) error {
	if util.PodWantsHostNetwork(pod) || !util.PodScheduled(pod) {
		return nil
	}

	podDesc := pod.Namespace + "/" + pod.Name

	// there is only a logical port for local pods or remote pods of layer2
	// networks on interconnect, so only delete in these cases
	isLocalPod := bsnc.isPodScheduledinLocalZone(pod)
	hasLogicalPort := isLocalPod || bsnc.isLayer2Interconnect()

	// for a specific NAD belongs to this network, Pod's logical port might already be created half-way
	// without its lpInfo cache being created; need to deleted resources created for that NAD as well.
	// So, first get all nadKeys from pod annotation, but handle NADs belong to this network only.
	podNetworks, err := util.UnmarshalPodAnnotationAllNetworks(pod.Annotations)
	if err != nil {
		return err
	}

	if portInfoMap == nil {
		portInfoMap = map[string]*lpInfo{}
	}

	var alreadyProcessed bool
	for nadKey, podAnnotation := range podNetworks {
		if !bsnc.HasNADKey(nadKey) {
			continue
		}

		networkAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
		if err != nil {
			return err
		}
		// pod has a network managed by this controller
		klog.Infof("Deleting pod: %s for network %s, NAD key: %s", podDesc, bsnc.GetNetworkName(), nadKey)
		if !isLocalPod && bsnc.isLayer3Interconnect() {
			err = bsnc.zoneICHandler.DeleteRemotePod(pod, networkAnnotation)
			if err != nil {
				klog.Errorf("Failed to delete remote pod %s/%s for network %s: %v", pod.Namespace, pod.Name, bsnc.GetNetworkName(), err)
				// ignore error and continue with the delete flow
			}
		}

		// handle remote pod clean up but only do this one time
		if !hasLogicalPort && !alreadyProcessed {
			if bsnc.doesNetworkRequireIPAM() &&
				// address set is for network policy only. So either multi network policy is enabled or network
				// segmentation, and it is a primary UDN (regular netpol)
				(util.IsMultiNetworkPoliciesSupportEnabled() || (util.IsNetworkSegmentationSupportEnabled() && bsnc.IsPrimaryNetwork())) {
				return bsnc.removeRemoteZonePodFromNamespaceAddressSet(pod)
			}

			// except for localnet networks, continue the delete flow in case a node just
			// became remote where we might still need to cleanup. On L3 networks
			// the node switch is removed so there is no need to do this.
			if bsnc.TopologyType() != types.LocalnetTopology {
				return nil
			}
			alreadyProcessed = true
		}

		if kubevirt.IsPodAllowedForMigration(pod, bsnc.GetNetInfo()) {
			if err = bsnc.enableSourceLSPFailedLiveMigration(pod, nadKey, podAnnotation.MAC, podAnnotation.IPs); err != nil {
				return err
			}
		}
		bsnc.logicalPortCache.remove(pod, nadKey)
		pInfo, err := bsnc.deletePodLogicalPort(pod, portInfoMap[nadKey], nadKey)
		if err != nil {
			return err
		}

		// do not release IP address if this controller does not handle IP allocation
		if !bsnc.allocatesPodAnnotation() {
			continue
		}

		// do not release IP address unless we have validated no other pod is using it
		if pInfo == nil || len(pInfo.ips) == 0 {
			bsnc.forgetPodReleasedBeforeStartup(string(pod.UID), nadKey)
			continue
		}

		// if we allow for persistent IPs, then we need to check if this pod has an IPAM Claim
		if bsnc.allowPersistentIPs() {
			hasIPAMClaim, err := bsnc.hasIPAMClaim(pod, nadKey)
			if err != nil {
				return fmt.Errorf("unable to determine if pod %s has IPAM Claim: %w", podDesc, err)
			}
			// if there is an IPAM claim, don't release the pod IPs
			if hasIPAMClaim {
				continue
			}
		}

		// Releasing IPs needs to happen last so that we can deterministically know that if delete failed that
		// the IP of the pod needs to be released. Otherwise we could have a completed pod failed to be removed
		// and we dont know if the IP was released or not, and subsequently could accidentally release the IP
		// while it is now on another pod
		klog.Infof("Attempting to release IPs for pod: %s/%s, ips: %s network %s", pod.Namespace, pod.Name,
			util.JoinIPNetIPs(pInfo.ips, " "), bsnc.GetNetworkName())
		if err = bsnc.releasePodIPs(pInfo); err != nil {
			return err
		}

		bsnc.forgetPodReleasedBeforeStartup(string(pod.UID), nadKey)

	}
	return nil
}

// hasIPAMClaim determines whether a pod's IPAM is being handled by IPAMClaim CR.
// pod passed should already be validated as having a network connection to nadKey
func (bsnc *BaseUserDefinedNetworkController) hasIPAMClaim(pod *corev1.Pod, nadKey string) (bool, error) {
	if !bsnc.AllowsPersistentIPs() {
		return false, nil
	}

	var ipamClaimName string
	var wasPersistentIPRequested bool
	if bsnc.IsPrimaryNetwork() {
		// 'k8s.ovn.org/primary-udn-ipamclaim' annotation has been deprecated. Maintain backward compatibility by
		// using it as a fallback; when defaultNSE.IPAMClaimReference is set, it takes precedence.
		if desiredClaimName, isIPAMClaimRequested := pod.Annotations[util.DeprecatedOvnUDNIPAMClaimName]; isIPAMClaimRequested && desiredClaimName != "" {
			wasPersistentIPRequested = true
			ipamClaimName = desiredClaimName
		}
		defaultNSE, err := util.GetK8sPodDefaultNetworkSelection(pod)
		if err != nil {
			return false, err
		}
		if defaultNSE != nil && defaultNSE.IPAMClaimReference != "" {
			wasPersistentIPRequested = true
			ipamClaimName = defaultNSE.IPAMClaimReference
		}
	} else {
		// secondary network the IPAM claim reference is on the network selection element
		on, networkMap, err := util.GetPodNADToNetworkMapping(pod, bsnc)
		if err != nil {
			return false, fmt.Errorf("failed to get network mapping for pod %s/%s on network %s: %v",
				pod.Namespace, pod.Name, bsnc.GetNetworkName(), err)
		}
		if !on {
			klog.Warningf("Pod %s/%s is not scheduled on network %s", pod.Namespace, pod.Name, bsnc.GetNetworkName())
			return false, nil
		}
		for key, network := range networkMap {
			if key == nadKey {
				if len(network.IPAMClaimReference) > 0 {
					ipamClaimName = network.IPAMClaimReference
					wasPersistentIPRequested = true
				}
				break
			}
		}
	}

	if !wasPersistentIPRequested || len(ipamClaimName) == 0 {
		return false, nil
	}

	ipamClaim, err := bsnc.ipamClaimsReconciler.FindIPAMClaim(ipamClaimName, pod.Namespace)
	if apierrors.IsNotFound(err) {
		klog.Errorf("IPAMClaim %q for namespace: %q not found...will release IPs: %v",
			ipamClaimName, pod.Namespace, err)
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("failed to get IPAMClaim %s/%s: %w", pod.Namespace, ipamClaimName, err)
	}

	hasIPAMClaim := ipamClaim != nil && len(ipamClaim.Status.IPs) > 0
	return hasIPAMClaim, nil
}

func (bsnc *BaseUserDefinedNetworkController) syncPodsForUserDefinedNetwork(pods []interface{}) error {
	annotatedLocalPods := map[*corev1.Pod]map[string]*util.PodAnnotation{}
	// get the list of logical switch ports (equivalent to pods). Reserve all existing Pod IPs to
	// avoid subsequent new Pods getting the same duplicate Pod IP.
	expectedLogicalPorts := make(map[string]bool)
	for _, podInterface := range pods {
		pod, ok := podInterface.(*corev1.Pod)
		if !ok {
			return fmt.Errorf("spurious object in syncPods: %v", podInterface)
		}

		var activeNetwork util.NetInfo
		var err error
		if bsnc.IsPrimaryNetwork() {
			activeNetwork, err = bsnc.networkManager.GetActiveNetworkForNamespace(pod.Namespace)
			if err != nil {
				if apierrors.IsNotFound(err) {
					// namespace is gone after we listed this pod, that means the pod no longer exists
					// we don't need to preserve it's previously allocated IP address or logical switch port
					klog.Infof("%s network controller pod sync: pod %s/%s namespace has been deleted, ignoring pod",
						bsnc.GetNetworkName(), pod.Namespace, pod.Name)
					continue
				}
				return fmt.Errorf("failed looking for the active network at namespace '%s': %w", pod.Namespace, err)
			}
		}

		on, networkMap, err := util.GetPodNADToNetworkMappingWithActiveNetwork(pod, bsnc.GetNetInfo(), activeNetwork)
		if err != nil || !on {
			if err != nil {
				bsnc.recordPodErrorEvent(pod, err)
				klog.Errorf("Failed to determine if pod %s/%s needs to be plumb interface on network %s: %v",
					pod.Namespace, pod.Name, bsnc.GetNetworkName(), err)
			}
			continue
		}

		isLocalPod := bsnc.isPodScheduledinLocalZone(pod)
		hasRemotePort := !isLocalPod || bsnc.isLayer2Interconnect()

		for nadKey := range networkMap {
			annotations, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
			if err != nil {
				if !util.IsAnnotationNotSetError(err) {
					klog.Errorf("Failed to get pod annotation of pod %s/%s for NAD key %s", pod.Namespace, pod.Name, nadKey)
				}
				continue
			}

			if bsnc.allocatesPodAnnotation() && isLocalPod {
				// only keep track of IPs/ports that have been allocated by this
				// controller
				expectedLogicalPortName, err := bsnc.allocatePodIPs(pod, annotations, nadKey)
				if err != nil {
					return err
				}
				if expectedLogicalPortName != "" {
					expectedLogicalPorts[expectedLogicalPortName] = true
				}

				if annotatedLocalPods[pod] == nil {
					annotatedLocalPods[pod] = map[string]*util.PodAnnotation{}
				}
				annotatedLocalPods[pod][nadKey] = annotations
			} else if hasRemotePort {
				// keep also track of remote ports created for layer2 on
				// interconnect
				expectedLogicalPorts[bsnc.GetLogicalPortName(pod, nadKey)] = true
			}
		}
	}

	// keep track of which pods might have already been released
	bsnc.trackPodsReleasedBeforeStartup(annotatedLocalPods)

	return bsnc.deleteStaleLogicalSwitchPorts(expectedLogicalPorts)
}

// addPodToNamespaceForUserDefinedNetwork returns the ops needed to add pod's IP to the namespace's address set.
func (bsnc *BaseUserDefinedNetworkController) addPodToNamespaceForUserDefinedNetwork(ns string, ips []*net.IPNet, portUUID string) ([]ovsdb.Operation, error) {
	var err error
	nsInfo, nsUnlock, err := bsnc.ensureNamespaceLockedForUserDefinedNetwork(ns, true, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure namespace locked: %v", err)
	}

	defer nsUnlock()

	return bsnc.addLocalPodToNamespaceLocked(nsInfo, ips, portUUID)
}

// AddNamespaceForUserDefinedNetwork creates corresponding addressset in ovn db for User Defined Network
func (bsnc *BaseUserDefinedNetworkController) AddNamespaceForUserDefinedNetwork(ns *corev1.Namespace) error {
	klog.Infof("[%s] adding namespace for network %s", ns.Name, bsnc.GetNetworkName())
	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s] adding namespace took %v for network %s", ns.Name, time.Since(start), bsnc.GetNetworkName())
	}()

	_, nsUnlock, err := bsnc.ensureNamespaceLockedForUserDefinedNetwork(ns.Name, false, ns)
	if err != nil {
		return fmt.Errorf("failed to ensure namespace locked: %v", err)
	}
	nsUnlock()
	// Enqueue the UDN namespace into network policy controller if it needs to be
	// processed by network policy peer namespace handlers.
	if bsnc.IsPrimaryNetwork() {
		err = bsnc.requeuePeerNamespace(ns)
		if err != nil {
			return fmt.Errorf("failed to requeue peer namespace %s: %v", ns.Name, err)
		}
	}
	return nil
}

// ensureNamespaceLockedForUserDefinedNetwork locks namespacesMutex, gets/creates an entry for ns, configures OVN nsInfo,
// and returns it with its mutex locked.
// ns is the name of the namespace, while namespace is the optional k8s namespace object
func (bsnc *BaseUserDefinedNetworkController) ensureNamespaceLockedForUserDefinedNetwork(ns string, readOnly bool, namespace *corev1.Namespace) (*namespaceInfo, func(), error) {
	return bsnc.ensureNamespaceLockedCommon(ns, readOnly, namespace, bsnc.getAllNamespacePodAddresses, bsnc.configureNamespaceCommon)
}

func (bsnc *BaseUserDefinedNetworkController) updateNamespaceForUserDefinedNetwork(old, newer *corev1.Namespace) error {
	var errors []error
	klog.Infof("[%s] updating namespace for network %s", old.Name, bsnc.GetNetworkName())

	nsInfo, nsUnlock := bsnc.getNamespaceLocked(old.Name, false)
	if nsInfo == nil {
		klog.Warningf("Update event for unknown namespace %q", old.Name)
		return nil
	}
	defer nsUnlock()

	aclAnnotation := newer.Annotations[util.AclLoggingAnnotation]
	oldACLAnnotation := old.Annotations[util.AclLoggingAnnotation]
	// support for ACL logging update, if new annotation is empty, make sure we propagate new setting
	if aclAnnotation != oldACLAnnotation {
		if err := bsnc.updateNamespaceAclLogging(old.Name, aclAnnotation, nsInfo); err != nil {
			errors = append(errors, err)
		}
	}

	if err := bsnc.multicastUpdateNamespace(newer, nsInfo); err != nil {
		errors = append(errors, err)
	}
	return utilerrors.Join(errors...)
}

func (bsnc *BaseUserDefinedNetworkController) deleteNamespaceForUserDefinedNetwork(ns *corev1.Namespace) error {
	klog.Infof("[%s] deleting namespace for network %s", ns.Name, bsnc.GetNetworkName())

	nsInfo, err := bsnc.deleteNamespaceLocked(ns.Name)
	if err != nil {
		return err
	}
	if nsInfo == nil {
		return nil
	}
	defer nsInfo.Unlock()

	if err := bsnc.multicastDeleteNamespace(ns, nsInfo); err != nil {
		return fmt.Errorf("failed to delete multicast namespace error %v", err)
	}
	return nil
}

// WatchNetworkPolicy starts the watching of networkpolicy resource and calls
// back the appropriate handler logic
func (bsnc *BaseUserDefinedNetworkController) WatchNetworkPolicy() error {
	if bsnc.netPolicyHandler != nil {
		return nil
	}
	handler, err := bsnc.retryNetworkPolicies.WatchResource()
	if err != nil {
		return err
	}
	bsnc.netPolicyHandler = handler
	return nil
}

// WatchMultiNetworkPolicy starts the watching of multinetworkpolicy resource and calls
// back the appropriate handler logic
func (bsnc *BaseUserDefinedNetworkController) WatchMultiNetworkPolicy() error {
	if bsnc.multiNetPolicyHandler != nil {
		return nil
	}
	handler, err := bsnc.retryMultiNetworkPolicies.WatchResource()
	if err != nil {
		return err
	}
	bsnc.multiNetPolicyHandler = handler
	return nil
}

// cleanupPolicyLogicalEntities cleans up all the port groups and address sets that belong to the given controller
func cleanupPolicyLogicalEntities(nbClient libovsdbclient.Client, ops []ovsdb.Operation, controllerName string) ([]ovsdb.Operation, error) {
	var err error
	portGroupPredicate := func(item *nbdb.PortGroup) bool {
		return item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == controllerName
	}
	ops, err = libovsdbops.DeletePortGroupsWithPredicateOps(nbClient, ops, portGroupPredicate)
	if err != nil {
		return ops, fmt.Errorf("failed to get ops to delete port groups owned by controller %s", controllerName)
	}

	asPredicate := func(item *nbdb.AddressSet) bool {
		return item.ExternalIDs[libovsdbops.OwnerControllerKey.String()] == controllerName
	}
	ops, err = libovsdbops.DeleteAddressSetsWithPredicateOps(nbClient, ops, asPredicate)
	if err != nil {
		return ops, fmt.Errorf("failed to get ops to delete address sets owned by controller %s", controllerName)
	}
	return ops, nil
}

// WatchIPAMClaims starts the watching of IPAMClaim resources and calls
// back the appropriate handler logic
func (bsnc *BaseUserDefinedNetworkController) WatchIPAMClaims() error {
	if bsnc.ipamClaimsHandler != nil {
		return nil
	}
	handler, err := bsnc.retryIPAMClaims.WatchResource()
	if err != nil {
		bsnc.ipamClaimsHandler = handler
	}
	return err
}

func (oc *BaseUserDefinedNetworkController) allowPersistentIPs() bool {
	return config.OVNKubernetesFeature.EnablePersistentIPs &&
		util.DoesNetworkRequireIPAM(oc.GetNetInfo()) &&
		util.AllowsPersistentIPs(oc.GetNetInfo())
}

// buildUDNEgressSNAT is used to build the conditional SNAT required on L3 and L2 UDNs to
// steer traffic correctly via mp0 when leaving OVN to the host
func (bsnc *BaseUserDefinedNetworkController) buildUDNEgressSNAT(localPodSubnets []*net.IPNet, outputPort string, isUDNAdvertised bool) ([]*nbdb.NAT, error) {
	if len(localPodSubnets) == 0 {
		return nil, nil // nothing to do
	}
	var snats []*nbdb.NAT
	var masqIP *udn.MasqueradeIPs
	var err error
	networkID := bsnc.GetNetworkID()
	// calculate MAC
	dstMac := util.IPAddrToHWAddr(bsnc.GetNodeManagementIP(localPodSubnets[0]).IP)
	dstMacMatch := getMasqueradeManagementIPSNATMatch(dstMac.String())

	extIDs := map[string]string{
		types.NetworkExternalID:  bsnc.GetNetworkName(),
		types.TopologyExternalID: bsnc.TopologyType(),
	}

	var nodeIPsAS, svcIPsAS addressset.AddressSet
	if isUDNAdvertised {
		// For advertised networks, we need to SNAT any traffic leaving the
		// pods from these networks towards the node IPs in the cluster. In
		// order to do such a conditional SNAT, we need an address set that
		// contains the node IPs in the cluster. Given that egressIP feature
		// already has an address set containing these nodeIPs owned by the
		// default network controller, let's re-use it.
		nodeIPsASIDs := getEgressIPAddrSetDbIDs(NodeIPAddrSetName, types.DefaultNetworkName, DefaultNetworkControllerName)
		nodeIPsAS, err = bsnc.addressSetFactory.GetAddressSet(nodeIPsASIDs)
		if err != nil {
			return nil, fmt.Errorf("failed to get address set with IDs %v: %w", nodeIPsASIDs, err)
		}

		// We also need to SNAT any traffic leaving the pods from these
		// networks towards the default network service cluster IPs
		// accessible from UDNs: we want the reply traffic to hit the
		// masquerade IP rule rather than the UDN subnet ip rule to allow
		// for overlaps in VRF-Lite configurations
		svcIPsASIDs := udnenabledsvc.GetAddressSetDBIDs()
		svcIPsAS, err = bsnc.addressSetFactory.GetAddressSet(svcIPsASIDs)
		if err != nil {
			return nil, fmt.Errorf("failed to get address set with IDs %v: %w", svcIPsASIDs, err)
		}
	}

	for _, localPodSubnet := range localPodSubnets {
		snatMatch := dstMacMatch
		ipFamily := utilnet.IPv4
		masqIP, err = udn.AllocateV4MasqueradeIPs(networkID)
		if utilnet.IsIPv6CIDR(localPodSubnet) {
			masqIP, err = udn.AllocateV6MasqueradeIPs(networkID)
			ipFamily = utilnet.IPv6
		}
		if err != nil {
			return nil, err
		}
		if masqIP == nil {
			return nil, fmt.Errorf("masquerade IP cannot be empty network %s (%d): %v", bsnc.GetNetworkName(), networkID, err)
		}

		if isUDNAdvertised {
			additionalSNATMatch := getClusterNodesDestinationBasedSNATMatch(ipFamily, nodeIPsAS, svcIPsAS)
			if additionalSNATMatch != "" {
				snatMatch = fmt.Sprintf("%s && %s", snatMatch, additionalSNATMatch)
			}
		}

		snat := libovsdbops.BuildSNATWithMatch(
			&masqIP.ManagementPort.IP,
			localPodSubnet,
			outputPort,
			extIDs,
			snatMatch,
		)
		snats = append(snats, snat)
	}

	return snats, nil
}

func getMasqueradeManagementIPSNATMatch(dstMac string) string {
	return fmt.Sprintf("eth.dst == %s", dstMac)
}

// getClusterNodesDestinationBasedSNATMatch creates destination-based SNAT match
// for the specified IP family. Returns an empty string if there is no address
// set for the provided IP family.
func getClusterNodesDestinationBasedSNATMatch(ipFamily utilnet.IPFamily, addressSets ...addressset.AddressSet) string {
	asMatches := make([]string, 0, len(addressSets))
	for _, as := range addressSets {
		asIPv4, asIPv6 := as.GetASHashNames()
		switch {
		case ipFamily == utilnet.IPv4 && asIPv4 != "":
			asMatches = append(asMatches, fmt.Sprintf("ip4.dst == $%s", asIPv4))
		case ipFamily == utilnet.IPv6 && asIPv6 != "":
			asMatches = append(asMatches, fmt.Sprintf("ip6.dst == $%s", asIPv6))
		}
	}
	switch len(asMatches) {
	case 0:
		return ""
	case 1:
		return asMatches[0]
	default:
		return fmt.Sprintf("(%s)", strings.Join(asMatches, " || "))
	}
}

func (bsnc *BaseUserDefinedNetworkController) requireDHCP(pod *corev1.Pod) bool {
	// Configure DHCP only for kubevirt VMs layer2 primary udn with subnets
	return kubevirt.IsPodOwnedByVirtualMachine(pod) &&
		util.IsNetworkSegmentationSupportEnabled() &&
		bsnc.IsPrimaryNetwork() &&
		bsnc.TopologyType() == types.Layer2Topology
}

func (bsnc *BaseUserDefinedNetworkController) setPodLogicalSwitchPortAddressesAndEnabledField(
	pod *corev1.Pod, nadKey string, mac string, ips []string, enabled bool, ops []ovsdb.Operation,
) ([]ovsdb.Operation, *nbdb.LogicalSwitchPort, error) {
	lsp := &nbdb.LogicalSwitchPort{Name: bsnc.GetLogicalPortName(pod, nadKey)}
	lsp.Enabled = ptr.To(enabled)
	customFields := []libovsdbops.ModelUpdateField{
		libovsdbops.LogicalSwitchPortEnabled,
		libovsdbops.LogicalSwitchPortAddresses,
	}
	if !enabled {
		lsp.Addresses = nil
	} else {
		if len(mac) == 0 || len(ips) == 0 {
			return nil, nil, fmt.Errorf("failed to configure addresses for lsp, missing mac and ips for pod %s", pod.Name)
		}

		// Remove length
		for i, ip := range ips {
			ips[i] = strings.Split(ip, "/")[0]
		}

		lsp.Addresses = []string{
			strings.Join(append([]string{mac}, ips...), " "),
		}
	}
	switchName, err := bsnc.getExpectedSwitchName(pod)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch switch name for pod %s: %w", pod.Name, err)
	}
	ops, err = libovsdbops.UpdateLogicalSwitchPortsOnSwitchWithCustomFieldsOps(bsnc.nbClient, ops, &nbdb.LogicalSwitch{Name: switchName}, customFields, lsp)
	if err != nil {
		return nil, nil, fmt.Errorf("failed updating logical switch port %+v on switch %s: %w", *lsp, switchName, err)
	}
	return ops, lsp, nil
}

func (bsnc *BaseUserDefinedNetworkController) disableLiveMigrationSourceLSPOps(
	kubevirtLiveMigrationStatus *kubevirt.LiveMigrationStatus,
	nadKey string, ops []ovsdb.Operation,
) ([]ovsdb.Operation, error) {
	// closing the sourcePod lsp to ensure traffic goes to the now ready targetPod.
	ops, _, err := bsnc.setPodLogicalSwitchPortAddressesAndEnabledField(kubevirtLiveMigrationStatus.SourcePod, nadKey, "", nil, false, ops)
	return ops, err
}

func (bsnc *BaseUserDefinedNetworkController) enableSourceLSPFailedLiveMigration(pod *corev1.Pod, nadKey string, mac string, ips []string) error {
	kubevirtLiveMigrationStatus, err := kubevirt.DiscoverLiveMigrationStatus(bsnc.watchFactory, pod)
	if err != nil {
		return fmt.Errorf("failed to discover Live-migration status after pod termination: %w", err)
	}
	if kubevirtLiveMigrationStatus == nil ||
		pod.Name != kubevirtLiveMigrationStatus.TargetPod.Name ||
		kubevirtLiveMigrationStatus.State != kubevirt.LiveMigrationFailed {
		return nil
	}
	// make sure sourcePod lsp is enabled if migration failed after DomainReady was set.
	ops, sourcePodLsp, err := bsnc.setPodLogicalSwitchPortAddressesAndEnabledField(kubevirtLiveMigrationStatus.SourcePod, nadKey, mac, ips, true, nil)
	if err != nil {
		return fmt.Errorf("failed to set source Pod lsp to enabled after migration failed: %w", err)
	}
	_, err = libovsdbops.TransactAndCheckAndSetUUIDs(bsnc.nbClient, sourcePodLsp, ops)
	if err != nil {
		return fmt.Errorf("failed transacting operations %+v: %w", ops, err)
	}

	return nil
}

func shouldAddPort(oldPod, newPod *corev1.Pod, inRetryCache bool) bool {

	// ovn-k8s-cni-overlay adds encap_ip to the pod annotation after ovnkube-controller
	// initially creates it. Here need to check if the annotation is changed, if so,
	// update the Port Binding accordingly to reflect the new encap_ip.

	return inRetryCache || util.PodScheduled(oldPod) != util.PodScheduled(newPod) || util.PodAnnotationChanged(oldPod, newPod)
}
