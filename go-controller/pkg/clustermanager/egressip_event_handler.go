// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"fmt"
	"reflect"

	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"

	corev1 "k8s.io/api/core/v1"
	cache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	objretry "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// egressIPClusterControllerEventHandler object handles the events
// from retry framework for the egressIPClusterController.
type egressIPClusterControllerEventHandler struct {
	objretry.DefaultEventHandler
	objType  reflect.Type
	eIPC     *egressIPClusterController
	syncFunc func([]interface{}) error
}

func (h *egressIPClusterControllerEventHandler) FilterOutResource(_ interface{}) bool {
	return false
}

// egressIPClusterControllerEventHandler functions

// AddResource adds the specified object to the cluster according to its type and
// returns the error, if any, yielded during object creation.
func (h *egressIPClusterControllerEventHandler) AddResource(obj interface{}, _ bool) error {
	switch h.objType {
	case factory.EgressNodeType:
		node := obj.(*corev1.Node)
		// EgressIP is not supported on hybrid overlay nodes
		if util.NoHostSubnet(node) {
			return nil
		}

		// Initialize the allocator on every update,
		// ovnkube-node/cloud-network-config-controller will make sure to
		// annotate the node with the egressIPConfig, but that might have
		// happened after we processed the ADD for that object, hence keep
		// retrying for all UPDATEs.
		if err := h.eIPC.initEgressIPAllocator(node); err != nil {
			klog.Warningf("Egress node initialization error: %v", err)
		}
		nodeEgressLabel := util.GetNodeEgressLabel()
		nodeLabels := node.GetLabels()
		_, hasEgressLabel := nodeLabels[nodeEgressLabel]
		if hasEgressLabel {
			// A missing or unparseable host-cidrs annotation leaves the
			// conflict check with no addresses for this node, so it cannot be
			// considered for assignment.
			_, err := util.ParseNodeHostCIDRsDropNetMask(node)
			if err != nil {
				klog.Warningf("Node %s has the egress-assignable label but no usable host-cidrs "+
					"annotation, marking it not assignable: %v", node.Name, err)
			}
			h.eIPC.setNodeEgressAssignable(node.Name, err == nil)
		}
		isReady := h.eIPC.isEgressNodeReady(node)
		if isReady {
			h.eIPC.setNodeEgressReady(node.Name, true)
		}
		isReachable := h.eIPC.isEgressNodeReachable(node)
		if hasEgressLabel && isReachable && isReady {
			h.eIPC.setNodeEgressReachable(node.Name, true)
			if err := h.eIPC.addEgressNode(node.Name); err != nil {
				return err
			}
		}
	case factory.EgressIPType:
		eIP := obj.(*egressipv1.EgressIP)
		return h.eIPC.reconcileEgressIP(nil, eIP)
	case factory.CloudPrivateIPConfigType:
		cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return h.eIPC.reconcileCloudPrivateIPConfig(nil, cloudPrivateIPConfig)
	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}
	return nil
}

// UpdateResource updates the specified object in the cluster to its version in newObj according
// to its type and returns the error, if any, yielded during the object update.
// The inRetryCache boolean argument is to indicate if the given resource is in the retryCache or not.
func (h *egressIPClusterControllerEventHandler) UpdateResource(oldObj, newObj interface{}, _ bool) error {
	switch h.objType {
	case factory.EgressIPType:
		oldEIP := oldObj.(*egressipv1.EgressIP)
		newEIP := newObj.(*egressipv1.EgressIP)
		return h.eIPC.reconcileEgressIP(oldEIP, newEIP)
	case factory.EgressNodeType:
		oldNode := oldObj.(*corev1.Node)
		newNode := newObj.(*corev1.Node)

		// EgressIP is not supported on hybrid overlay nodes
		if util.NoHostSubnet(newNode) {
			return nil
		}

		// Initialize the allocator on every update,
		// ovnkube-node/cloud-network-config-controller will make sure to
		// annotate the node with the egressIPConfig, but that might have
		// happened after we processed the ADD for that object, hence keep
		// retrying for all UPDATEs.
		if err := h.eIPC.initEgressIPAllocator(newNode); err != nil {
			klog.Warningf("Egress node initialization error: %v", err)
		}
		nodeEgressLabel := util.GetNodeEgressLabel()
		oldLabels := oldNode.GetLabels()
		newLabels := newNode.GetLabels()
		_, oldHadEgressLabel := oldLabels[nodeEgressLabel]
		_, newHasEgressLabel := newLabels[nodeEgressLabel]
		// If the node is not labeled for egress assignment, just return
		// directly, we don't really need to set the ready / reachable
		// status on this node if the user doesn't care about using it.
		if !oldHadEgressLabel && !newHasEgressLabel {
			return nil
		}
		// A node is only assignable when it carries the egress-assignable label
		// and its host-cidrs annotation is present and parseable. Without the
		// addresses the conflict check has no data for this node, so any parse
		// error makes it not assignable.
		oldNodeIsAssignable := oldHadEgressLabel
		if oldNodeIsAssignable {
			_, err := util.ParseNodeHostCIDRsDropNetMask(oldNode)
			oldNodeIsAssignable = err == nil
		}

		nodeIsAssignable := newHasEgressLabel
		if nodeIsAssignable {
			_, err := util.ParseNodeHostCIDRsDropNetMask(newNode)
			if err != nil {
				klog.Warningf("Node %s has the egress-assignable label but no usable host-cidrs "+
					"annotation, marking it not assignable: %v", newNode.Name, err)
			}
			nodeIsAssignable = err == nil
		}
		h.eIPC.setNodeEgressAssignable(newNode.Name, nodeIsAssignable)

		// setNodeEgressAssignable clears the node's allocation cache when it
		// turns a node unassignable, on the understanding that the caller then
		// clears that node's assignments too. Release them here to keep the
		// cache and status.items in agreement. A node that is genuinely gone is
		// released anyway by the readiness and reachability handling below, so
		// this only fires for a labelled, reachable node whose annotation stopped
		// parsing.
		if oldNodeIsAssignable && !nodeIsAssignable && newHasEgressLabel {
			klog.Infof("Node: %s is no longer assignable (host-cidrs annotation missing or invalid), "+
				"deleting it from egress assignment", newNode.Name)
			if err := h.eIPC.deleteEgressNode(newNode.Name); err != nil {
				return fmt.Errorf("failed to delete egress assignments for node %s: %w", newNode.Name, err)
			}
			return nil
		}

		if oldHadEgressLabel && !newHasEgressLabel {
			klog.Infof("Node: %s has been un-labeled, deleting it from egress assignment", newNode.Name)
			return h.eIPC.deleteEgressNode(oldNode.Name)
		}
		isOldReady := h.eIPC.isEgressNodeReady(oldNode)
		isNewReady := h.eIPC.isEgressNodeReady(newNode)
		isNewReachable := h.eIPC.isEgressNodeReachable(newNode)
		isHostCIDRsAltered := util.NodeHostCIDRsAnnotationChanged(oldNode, newNode)
		isCloudEgressIPConfigAltered := util.CloudEgressIPConfigAnnotationChanged(oldNode, newNode)
		h.eIPC.setNodeEgressReady(newNode.Name, isNewReady)
		if !oldHadEgressLabel && newHasEgressLabel {
			klog.Infof("Node: %s has been labeled, adding it for egress assignment", newNode.Name)
			if isNewReady && isNewReachable {
				h.eIPC.setNodeEgressReachable(newNode.Name, isNewReachable)
				if err := h.eIPC.addEgressNode(newNode.Name); err != nil {
					return err
				}
			} else {
				klog.Warningf("Node: %s has been labeled, but node is not ready"+
					" and reachable, cannot use it for egress assignment", newNode.Name)
			}
			return nil
		}
		if isOldReady == isNewReady && !isHostCIDRsAltered && !isCloudEgressIPConfigAltered {
			return nil
		}
		if !isNewReady {
			klog.Warningf("Node: %s is not ready, deleting it from egress assignment", newNode.Name)
			if err := h.eIPC.deleteEgressNode(newNode.Name); err != nil {
				return err
			}
		} else if isNewReady && isNewReachable {
			// Build a log message that captures all reasons we are re-evaluating,
			// so operators can correlate annotation changes with re-assignments even
			// when multiple conditions change simultaneously.
			switch {
			case isCloudEgressIPConfigAltered && isHostCIDRsAltered:
				klog.Infof("Node: %s cloud egress IP config annotation and host CIDRs changed, re-evaluating egress IP assignments", newNode.Name)
			case isCloudEgressIPConfigAltered:
				klog.Infof("Node: %s cloud egress IP config annotation changed, re-evaluating egress IP assignments", newNode.Name)
			default:
				klog.Infof("Node: %s is ready and reachable, adding it for egress assignment", newNode.Name)
			}
			h.eIPC.setNodeEgressReachable(newNode.Name, isNewReachable)
			if err := h.eIPC.addEgressNode(newNode.Name); err != nil {
				return err
			}
		}
		if isHostCIDRsAltered {
			// we only need to consider EIPs that are assigned to networks that aren't managed by OVN
			if err := h.eIPC.reconcileSecondaryHostNetworkEIPs(newNode); err != nil {
				return fmt.Errorf("failed to reconsider egress IPs that are secondary host networks: %v", err)
			}
		}
		return nil
	case factory.CloudPrivateIPConfigType:
		oldCloudPrivateIPConfig := oldObj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		newCloudPrivateIPConfig := newObj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return h.eIPC.reconcileCloudPrivateIPConfig(oldCloudPrivateIPConfig, newCloudPrivateIPConfig)
	default:
		return fmt.Errorf("no update function for object type %s", h.objType)
	}
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// cachedObj is the internal cache entry for this object, used for now for pods and network policies.
func (h *egressIPClusterControllerEventHandler) DeleteResource(obj, _ interface{}) error {
	switch h.objType {
	case factory.EgressIPType:
		eIP := obj.(*egressipv1.EgressIP)
		return h.eIPC.reconcileEgressIP(eIP, nil)
	case factory.EgressNodeType:
		node := obj.(*corev1.Node)
		// EgressIP is not supported on hybrid overlay nodes
		if util.NoHostSubnet(node) {
			return nil
		}
		h.eIPC.deleteNodeForEgress(node)
		nodeEgressLabel := util.GetNodeEgressLabel()
		nodeLabels := node.GetLabels()
		_, hasEgressLabel := nodeLabels[nodeEgressLabel]
		if hasEgressLabel {
			if err := h.eIPC.deleteEgressNode(node.Name); err != nil {
				return err
			}
		}
		return nil
	case factory.CloudPrivateIPConfigType:
		cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return h.eIPC.reconcileCloudPrivateIPConfig(cloudPrivateIPConfig, nil)
	default:
		return fmt.Errorf("no delete function for object type %s", h.objType)
	}
}

func (h *egressIPClusterControllerEventHandler) SyncFunc(objs []interface{}) error {
	var syncFunc func([]interface{}) error

	if h.syncFunc != nil {
		// syncFunc was provided explicitly
		syncFunc = h.syncFunc
	} else {
		switch h.objType {
		case factory.EgressIPType:
			syncFunc = h.eIPC.syncEgressIPMarkAllocator
		case factory.EgressNodeType:
			syncFunc = h.eIPC.initEgressNodeReachability
		case factory.CloudPrivateIPConfigType:
			syncFunc = h.eIPC.syncCloudPrivateIPConfigs

		default:
			return fmt.Errorf("no sync function for object type %s", h.objType)
		}
	}
	if syncFunc == nil {
		return nil
	}
	return syncFunc(objs)
}

// getResourceFromInformerCache returns the latest state of the object from the informers cache
// given an object key and its type
func (h *egressIPClusterControllerEventHandler) GetResourceFromInformerCache(key string) (interface{}, error) {
	var obj interface{}
	var name string
	var err error

	_, name, err = cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to split key %s: %v", key, err)
	}

	switch h.objType {
	case factory.EgressNodeType:
		obj, err = h.eIPC.watchFactory.GetNode(name)
	case factory.CloudPrivateIPConfigType:
		obj, err = h.eIPC.watchFactory.GetCloudPrivateIPConfig(name)
	case factory.EgressIPType:
		obj, err = h.eIPC.watchFactory.GetEgressIP(name)

	default:
		err = fmt.Errorf("object type %s not supported, cannot retrieve it from informers cache",
			h.objType)
	}
	return obj, err
}
