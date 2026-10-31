// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"fmt"
	"reflect"

	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
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
	syncFunc func(context.Context, []interface{}) error
}

func (h *egressIPClusterControllerEventHandler) FilterOutResource(_ interface{}) bool {
	return false
}

// egressIPClusterControllerEventHandler functions

// AddResource adds the specified object to the cluster according to its type and
// returns the error, if any, yielded during object creation.
func (h *egressIPClusterControllerEventHandler) AddResource(ctx context.Context, obj interface{}, _ bool) error {
	switch h.objType {
	case factory.EgressNodeType:
		node := obj.(*corev1.Node)
		// EgressIP is not supported on hybrid overlay nodes
		if util.NoHostSubnet(node) {
			return nil
		}

		// Initialize the allocator on every add,
		// ovnkube-node/cloud-network-config-controller will make sure to
		// annotate the node with the egressIPConfig, but that might have
		// happened after we processed the ADD for that object, hence keep
		// retrying for all UPDATEs.
		// Track all nodes in the allocator cache to avoid churn on label changes;
		// selector filtering happens in getSortedEgressData at assignment time.
		if err := h.eIPC.initEgressIPAllocator(node); err != nil {
			klog.Warningf("Egress node initialization error: %v", err)
		}
		isReady := h.eIPC.isEgressNodeReady(node)
		if isReady {
			h.eIPC.setNodeEgressReady(node.Name, true)
		}

		isReachable := h.eIPC.isEgressNodeReachable(node)
		if isReachable && isReady {
			h.eIPC.setNodeEgressReachable(node.Name, true)
			// Only wake up the EIP reconciler if this node can satisfy at least
			// one EgressIP's selector; calling addEgressNode for every healthy
			// node would cause needless reconciliation churn for nodes no EIP
			// will ever use. isHostCIDRsReady was set by initEgressIPAllocator.
			if h.eIPC.isNodeHostCIDRsReady(node.Name) {
				selectors := h.eIPC.getEgressIPNodeSelectors()
				nodeLabels := labels.Set(node.GetLabels())
				if selectors.Matches(nodeLabels) {
					if err := h.eIPC.addEgressNode(ctx, node.Name); err != nil {
						return err
					}
				}
			}
		}
	case factory.EgressIPType:
		eIP := obj.(*egressipv1.EgressIP)
		if err := h.eIPC.reconcileEgressIP(ctx, nil, eIP); err != nil {
			return err
		}
		// A freshly created EgressIP may select nodes that are assignable and
		// ready but whose reachability has not been confirmed yet (only selected
		// nodes are probed). Nudge the reachability loop so those nodes are probed
		// and activated now instead of after the next periodic tick.
		h.eIPC.nudgeReachabilityCheck()
		return nil
	case factory.CloudPrivateIPConfigType:
		cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return h.eIPC.reconcileCloudPrivateIPConfig(ctx, nil, cloudPrivateIPConfig)
	default:
		return fmt.Errorf("no add function for object type %s", h.objType)
	}
	return nil
}

// UpdateResource updates the specified object in the cluster to its version in newObj according
// to its type and returns the error, if any, yielded during the object update.
func (h *egressIPClusterControllerEventHandler) UpdateResource(ctx context.Context, oldObj, newObj interface{}, _ bool) error {
	switch h.objType {
	case factory.EgressIPType:
		oldEIP := oldObj.(*egressipv1.EgressIP)
		newEIP := newObj.(*egressipv1.EgressIP)
		if err := h.eIPC.reconcileEgressIP(ctx, oldEIP, newEIP); err != nil {
			return err
		}
		// If the node selector changed, nodes that now match may be assignable
		// and ready but not yet probed; nudge the reachability loop to pick them
		// up without waiting for the next periodic tick.
		if !reflect.DeepEqual(oldEIP.Spec.EgressNodeSelector, newEIP.Spec.EgressNodeSelector) {
			klog.V(4).Infof("EgressIP %s node selector changed, triggering a reachability check for nodes that started matching", newEIP.Name)
			h.eIPC.nudgeReachabilityCheck()
		}
		return nil
	case factory.EgressNodeType:
		oldNode := oldObj.(*corev1.Node)
		newNode := newObj.(*corev1.Node)

		// EgressIP is not supported on hybrid overlay nodes
		if util.NoHostSubnet(newNode) {
			return nil
		}

		// Early exit if nothing egress-relevant changed (filters out heartbeats).
		oldNodeLabels := labels.Set(oldNode.GetLabels())
		newNodeLabels := labels.Set(newNode.GetLabels())
		labelsChanged := !reflect.DeepEqual(oldNodeLabels, newNodeLabels)
		isHostCIDRsAltered := util.NodeHostCIDRsAnnotationChanged(oldNode, newNode)
		isCloudEgressIPConfigAltered := util.CloudEgressIPConfigAnnotationChanged(oldNode, newNode)
		readinessChanged := h.eIPC.isEgressNodeReady(oldNode) != h.eIPC.isEgressNodeReady(newNode)
		if !labelsChanged && !readinessChanged && !isHostCIDRsAltered && !isCloudEgressIPConfigAltered {
			return nil
		}
		klog.V(5).Infof("Node %s egress-relevant update: labels=%t readiness=%t hostCIDRs=%t cloudConfig=%t",
			newNode.Name, labelsChanged, readinessChanged, isHostCIDRsAltered, isCloudEgressIPConfigAltered)

		// Update allocator cache (labels, mgmtIPs, egressIPConfig).
		if err := h.eIPC.initEgressIPAllocator(newNode); err != nil {
			klog.Warningf("Egress node initialization error: %v", err)
		}

		// Update isReady flag from node conditions.
		// isHostCIDRsReady is already updated by initEgressIPAllocator above.
		isNewReady := h.eIPC.isEgressNodeReady(newNode)
		h.eIPC.setNodeEgressReady(newNode.Name, isNewReady)

		// Check selector match to decide whether assignment reconciliation is needed.
		selectors := h.eIPC.getEgressIPNodeSelectors()
		oldMatchesSelector := selectors.Matches(oldNodeLabels)
		newMatchesSelector := selectors.Matches(newNodeLabels)

		// Neither old nor new matches any selector — health flags are fresh, no
		// assignment work needed.
		if !oldMatchesSelector && !newMatchesSelector {
			return nil
		}

		// Node stopped matching all selectors — remove from assignment.
		if oldMatchesSelector && !newMatchesSelector {
			klog.Infof("Node: %s no longer matches any EgressIP selector, deleting it from egress assignment", newNode.Name)
			h.eIPC.clearNodeEgressAllocations(newNode.Name)
			return h.eIPC.deleteEgressNode(ctx, newNode.Name)
		}

		// Node matches at least one selector. Trigger reconciliation via
		// deleteEgressNode + addEgressNode. This handles all cases uniformly:
		// - node started matching (was unselected, now selected)
		// - label change flipped which selectors match
		// - readiness/host-cidrs/cloud-config changed
		// The reconcile path (validateEgressIPStatus) re-checks all conditions
		// and only reassigns as needed.
		isNewReachable := h.eIPC.isEgressNodeReachable(newNode)
		if oldMatchesSelector {
			// Was already matching — delete first to evict stale assignments.
			if err := h.eIPC.deleteEgressNode(ctx, newNode.Name); err != nil {
				return err
			}
		}
		if isNewReady && isNewReachable {
			h.eIPC.setNodeEgressReachable(newNode.Name, isNewReachable)
			if err := h.eIPC.addEgressNode(ctx, newNode.Name); err != nil {
				return err
			}
		} else {
			klog.V(5).Infof("Node: %s matches an EgressIP selector but is not ready/reachable, skipping assignment", newNode.Name)
		}

		if isHostCIDRsAltered {
			// We only need to consider EIPs that are assigned to networks that aren't managed by OVN.
			if err := h.eIPC.reconcileSecondaryHostNetworkEIPs(ctx, newNode); err != nil {
				return fmt.Errorf("failed to reconsider egress IPs that are secondary host networks: %v", err)
			}
		}
		return nil
	case factory.CloudPrivateIPConfigType:
		oldCloudPrivateIPConfig := oldObj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		newCloudPrivateIPConfig := newObj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return h.eIPC.reconcileCloudPrivateIPConfig(ctx, oldCloudPrivateIPConfig, newCloudPrivateIPConfig)
	default:
		return fmt.Errorf("no update function for object type %s", h.objType)
	}
}

// DeleteResource deletes the object from the cluster according to the delete logic of its resource type.
// cachedObj is the internal cache entry for this object, used for now for pods and network policies.
func (h *egressIPClusterControllerEventHandler) DeleteResource(ctx context.Context, obj, _ interface{}) error {
	switch h.objType {
	case factory.EgressIPType:
		eIP := obj.(*egressipv1.EgressIP)
		return h.eIPC.reconcileEgressIP(ctx, eIP, nil)
	case factory.EgressNodeType:
		node := obj.(*corev1.Node)
		// EgressIP is not supported on hybrid overlay nodes
		if util.NoHostSubnet(node) {
			return nil
		}
		h.eIPC.deleteNodeForEgress(node)
		selectors := h.eIPC.getEgressIPNodeSelectors()
		nodeLabels := labels.Set(node.GetLabels())
		if selectors.Matches(nodeLabels) {
			if err := h.eIPC.deleteEgressNode(ctx, node.Name); err != nil {
				return err
			}
		}
		return nil
	case factory.CloudPrivateIPConfigType:
		cloudPrivateIPConfig := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		return h.eIPC.reconcileCloudPrivateIPConfig(ctx, cloudPrivateIPConfig, nil)
	default:
		return fmt.Errorf("no delete function for object type %s", h.objType)
	}
}

func (h *egressIPClusterControllerEventHandler) SyncFunc(ctx context.Context, objs []interface{}) error {
	var syncFunc func(context.Context, []interface{}) error

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
	return syncFunc(ctx, objs)
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
