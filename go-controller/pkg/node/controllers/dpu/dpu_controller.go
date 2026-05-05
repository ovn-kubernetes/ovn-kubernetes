// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dpu

import (
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops/ovs"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// Controller is a single controller that manages DPU representor port lifecycle
// for pods across all networks (default + user-defined). It replaces the
// per-network watchPodsDPU pattern.
type Controller struct {
	watchFactory factory.NodeWatchFactory
	kube         kube.Interface
	networkMgr   networkmanager.Interface
	ovsClient    libovsdbclient.Client

	// nodeName is the (host) node this DPU serves. In DPU mode ovnkube runs as
	// combined ovnkube-controller + node, so the pod informer is cluster-wide
	// (no spec.nodeName field selector). We must filter pods to this node
	// ourselves, otherwise multiple DPUs would fight over the same representor.
	nodeName string

	podController controller.Controller
	podLister     corelisters.PodLister
	clientSet     cni.PodInfoGetter

	nadReconciler   controller.Reconciler
	nadReconcilerID uint64

	// podNADToDPUCDMap tracks the per-NAD DPU connection state for all pods
	// across all networks. Key is pod.UID; value is *podDPUState.
	// New entries are only Store'd from the pod sync path (initial sync or
	// pod reconciler); the NAD reconciler only reads and deletes entries.
	podNADToDPUCDMap sync.Map

	// podKeyToUID maps pod key (namespace/name) to the pod UID tracked in
	// podNADToDPUCDMap.
	podKeyToUID sync.Map
}

func NewController(
	wf factory.NodeWatchFactory,
	kube kube.Interface,
	kclient kubernetes.Interface,
	networkMgr networkmanager.Interface,
	ovsClient libovsdbclient.Client,
	nodeName string,
) *Controller {
	podLister := corelisters.NewPodLister(wf.LocalPodInformer().GetIndexer())

	c := &Controller{
		watchFactory: wf,
		kube:         kube,
		networkMgr:   networkMgr,
		ovsClient:    ovsClient,
		podLister:    podLister,
		clientSet:    cni.NewClientSet(kclient, podLister),
		nodeName:     nodeName,
	}

	c.podController = controller.NewController("dpu-pod-controller",
		&controller.ControllerConfig[corev1.Pod]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      c.reconcileDPUPod,
			ObjNeedsUpdate: c.dpuPodNeedsUpdate,
			Threadiness:    1,
			Informer:       wf.LocalPodInformer(),
			Lister:         podLister.List,
		})

	c.nadReconciler = controller.NewReconciler("dpu-nad-reconciler",
		&controller.ReconcilerConfig{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:   c.reconcileNAD,
			Threadiness: 1,
			MaxAttempts: controller.InfiniteAttempts,
		})

	return c
}

func (c *Controller) Start() error {
	klog.Info("Starting DPU pod controller")

	// RegisterNADReconciler handles NAD deletion and cleans up representors for the deleted NAD
	c.nadReconcilerID = c.networkMgr.RegisterNADReconciler(c.nadReconciler)
	return controller.StartWithInitialSync(c.bootstrapDPUPodMapFromOVS, c.podController, c.nadReconciler)
}

func (c *Controller) Stop() {
	klog.Info("Stopping DPU pod controller")
	if c.nadReconcilerID != 0 {
		c.networkMgr.DeRegisterNADReconciler(c.nadReconcilerID)
	}
	controller.Stop(c.podController)
	controller.Stop(c.nadReconciler)
}

func (c *Controller) dpuPodNeedsUpdate(oldPod, newPod *corev1.Pod) bool {
	if oldPod == nil || newPod == nil {
		return true
	}
	// Reconcile on connection-details changes, and also if the pod's node
	// assignment changes, so reconcileDPUPod can clean up any state/representor
	// created for a pod that is no longer scheduled on this node.
	return oldPod.Annotations[util.DPUConnectionDetailsAnnot] != newPod.Annotations[util.DPUConnectionDetailsAnnot] ||
		oldPod.Spec.NodeName != newPod.Spec.NodeName
}

// reconcileDPUPod reconciles the DPU state for a single pod.
// In DPU mode ovnkube runs as combined ovnkube-controller + node, so the pod
// informer is cluster-wide and returns pods for all nodes. A pod not scheduled
// on the node this DPU serves (c.nodeName) is treated as a deletion so any
// representor/state we may have created is cleaned up; the DPU on the pod's
// actual node is responsible for its representor.
// The key is namespace/name from the controller workqueue. The podNADToDPUCDMap
// is keyed by pod.UID to handle the case where a pod is recreated with the same
// name but a different UID (e.g. StatefulSet after host reboot).
func (c *Controller) reconcileDPUPod(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Failed to split meta namespace cache key %s: %v", key, err)
		return nil
	}

	pod, err := c.watchFactory.GetPod(namespace, name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// If the pod is not (or no longer) scheduled on the node this DPU serves,
	// treat it as a deletion so any representor/state gets cleaned up. This also
	// covers the case where a pod's node assignment changes away from this node.
	if pod != nil && pod.Spec.NodeName != c.nodeName {
		pod = nil
	}

	if err != nil || pod == nil {
		return c.handleDeleteDPUPodByKey(key)
	}

	if util.PodWantsHostNetwork(pod) {
		return c.handleDeleteDPUPodByKey(key)
	}

	// If the UID changed (pod recreated with same name), clean up the old UID's state first
	if oldUIDVal, ok := c.podKeyToUID.Load(key); ok {
		oldUID := oldUIDVal.(k8stypes.UID)
		if oldUID != pod.UID {
			if err := c.handleDeleteDPUPodByUID(oldUID, key); err != nil {
				return err
			}
		}
	}

	return c.handleAddOrUpdateDPUPod(key, pod, c.clientSet)
}

// reconcileNAD is called by the NAD reconciler when a NAD is added/updated/deleted.
// For NAD deletion, it cleans up OVS representor ports matching the deleted NAD
// and removes local state. No action for NAD creation/update.
func (c *Controller) reconcileNAD(nadName string) error {
	if c.networkMgr.GetNetInfoForNADKey(nadName) != nil {
		return nil
	}

	klog.Infof("NAD %s deleted, cleaning up representor ports", nadName)

	// Find all OVS interfaces whose NAD external ID matches the deleted NAD.
	// The external ID may be indexed (e.g. "ns/nad/1"), so strip the index
	// before comparing.
	p := func(item *vswitchd.Interface) bool {
		nadExtID := item.ExternalIDs[types.NADExternalID]
		nad, _, _ := util.GetNadFromIndexedNADKey(nadExtID)
		return nad == nadName
	}
	ovsIfaces, err := ovsops.FindInterfacesWithPredicate(c.ovsClient, p)
	if err != nil {
		return fmt.Errorf("failed to find interfaces for NAD %s: %w", nadName, err)
	}

	if len(ovsIfaces) > 0 {
		var portNames []string
		for _, iface := range ovsIfaces {
			portNames = append(portNames, iface.Name)
		}
		setRepPortInterfacesDown(portNames)
		if err := libovsdbops.DeleteMultiplePortsWithInterfaces(c.ovsClient, "br-int", portNames...); err != nil {
			return fmt.Errorf("failed to delete representor ports %v for NAD %s: %w", portNames, nadName, err)
		}
		klog.Infof("Deleted representor ports %v from br-int for NAD %s", portNames, nadName)
	}

	// Clean up local state only after OVS ports are successfully removed.
	// On OVS failure we return early and keep the entries so that pod
	// deletion can still find and clean up the representor ports.
	c.podNADToDPUCDMap.Range(func(_, val interface{}) bool {
		ps := val.(*podDPUState)
		ps.Lock()
		defer ps.Unlock()
		for nadKey := range ps.nadStates {
			nad, _, _ := util.GetNadFromIndexedNADKey(nadKey)
			if nad == nadName {
				delete(ps.nadStates, nadKey)
			}
		}
		return true
	})

	return nil
}

// bootstrapDPUPodMapFromOVS pre-populates podNADToDPUCDMap and podKeyToUID from
// existing OVS representor ports, and removes orphaned representor ports whose pods
// no longer exist.
func (c *Controller) bootstrapDPUPodMapFromOVS() error {
	var errs []error
	p := func(item *vswitchd.Interface) bool {
		return item.ExternalIDs["sandbox"] != "" && item.ExternalIDs["vf-netdev-name"] != ""
	}

	ovsIfaces, err := ovsops.FindInterfacesWithPredicate(c.ovsClient, p)
	if err != nil {
		return fmt.Errorf("failed to find representor interfaces during DPU bootstrap: %w", err)
	}
	if len(ovsIfaces) == 0 {
		return nil
	}

	existingUIDs := make(map[k8stypes.UID]bool)
	pods, err := c.watchFactory.GetAllPods()
	if err != nil {
		return fmt.Errorf("failed to list pods for DPU bootstrap: %v", err)
	}
	for _, pod := range pods {
		if pod.Spec.NodeName != c.nodeName {
			continue
		}
		existingUIDs[pod.UID] = true
	}

	var orphanedPorts []string
	for _, ovsIface := range ovsIfaces {
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
			orphanedPorts = append(orphanedPorts, repName)
			continue
		}

		state := &dpuConnectionState{
			vfRepName: repName,
			sandboxId: sandbox,
		}

		// No ps.Lock() needed: this runs as the initial sync before controller
		// workers are started (see StartWithInitialSync), so no concurrent access.
		var ps *podDPUState
		if v, ok := c.podNADToDPUCDMap.Load(uid); ok {
			ps = v.(*podDPUState)
		} else {
			ps = &podDPUState{nadStates: make(map[string]*dpuConnectionState)}
			c.podNADToDPUCDMap.Store(uid, ps)
		}
		ps.nadStates[nadKey] = state

		if podKey != "" {
			c.podKeyToUID.Store(podKey, uid)
		}
	}

	if len(orphanedPorts) > 0 {
		setRepPortInterfacesDown(orphanedPorts)
		if err := libovsdbops.DeleteMultiplePortsWithInterfaces(c.ovsClient, "br-int", orphanedPorts...); err != nil {
			errs = append(errs, fmt.Errorf("DPU bootstrap: failed to delete orphaned representor ports %v: %w", orphanedPorts, err))
		} else {
			klog.Infof("DPU bootstrap: deleted orphaned representor ports %v from br-int", orphanedPorts)
		}
	}

	klog.Infof("DPU bootstrap: populated state from OVS representor ports")
	return utilerrors.Join(errs...)
}
