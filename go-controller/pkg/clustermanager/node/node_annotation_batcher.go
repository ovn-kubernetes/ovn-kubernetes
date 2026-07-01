// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"fmt"
	"net"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// NodeAnnotationBatcher batches node annotation updates from multiple networks
// and applies them in a single API call. When multiple networks enqueue updates
// for the same node before the worker processes it, the workqueue coalesces the
// key and all pending updates are applied together, reducing API server load.
type NodeAnnotationBatcher struct {
	kube kube.Interface

	pendingUpdates map[string]*pendingNodeUpdate
	mutex          sync.Mutex

	reconciler controller.Reconciler

	cache *nodecontroller.NodeAnnotationCache
}

// pendingNodeUpdate accumulates annotation updates for a single node
// from potentially multiple networks before batching them together
type pendingNodeUpdate struct {
	hostSubnets map[string][]*net.IPNet
	networkIDs  map[string]int
	tunnelIDs   map[string]int
}

func NewNodeAnnotationBatcher(kube kube.Interface) *NodeAnnotationBatcher {
	b := &NodeAnnotationBatcher{
		kube:           kube,
		pendingUpdates: make(map[string]*pendingNodeUpdate),
		cache:          nodecontroller.NewNodeAnnotationCache(),
	}

	b.reconciler = controller.NewReconciler("node-annotation-batcher", &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   b.reconcile,
		Threadiness: 15,
		MaxAttempts: controller.InfiniteAttempts,
	})

	return b
}

func (b *NodeAnnotationBatcher) Reconciler() controller.Reconciler {
	return b.reconciler
}

// EnqueueUpdate queues an annotation update for a node from a specific network.
// Multiple updates for the same node are merged and applied together.
func (b *NodeAnnotationBatcher) EnqueueUpdate(nodeName, networkName string, hostSubnets []*net.IPNet, networkID, tunnelID int) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	pending, ok := b.pendingUpdates[nodeName]
	if !ok {
		pending = &pendingNodeUpdate{
			hostSubnets: make(map[string][]*net.IPNet),
			networkIDs:  make(map[string]int),
			tunnelIDs:   make(map[string]int),
		}
		b.pendingUpdates[nodeName] = pending
	}

	pending.hostSubnets[networkName] = hostSubnets
	if networkID != types.NoNetworkID {
		pending.networkIDs[networkName] = networkID
	}
	if tunnelID != types.NoTunnelID {
		pending.tunnelIDs[networkName] = tunnelID
	}

	klog.V(5).Infof("Enqueued annotation update for node %s network %s (subnets=%v, netID=%v, tunnelID=%v)",
		nodeName, networkName, len(hostSubnets), networkID, tunnelID)

	b.reconciler.Reconcile(nodeName)
}

func (b *NodeAnnotationBatcher) reconcile(nodeName string) error {
	b.mutex.Lock()
	pending, ok := b.pendingUpdates[nodeName]
	if ok {
		delete(b.pendingUpdates, nodeName)
	}
	b.mutex.Unlock()

	if !ok || pending == nil {
		return nil
	}

	klog.V(4).Infof("Processing batched annotation update for node %s (%d networks)",
		nodeName, len(pending.hostSubnets)+len(pending.networkIDs)+len(pending.tunnelIDs))

	return b.updateNodeAnnotations(nodeName, pending)
}

func pendingNodeUpdateToString(pending *pendingNodeUpdate) string {
	allNetworks := make(map[string]bool)
	for name := range pending.hostSubnets {
		allNetworks[name] = true
	}
	for name := range pending.networkIDs {
		allNetworks[name] = true
	}
	for name := range pending.tunnelIDs {
		allNetworks[name] = true
	}
	var perNetwork []string
	for name := range allNetworks {
		var parts []string
		if subnets, ok := pending.hostSubnets[name]; ok {
			parts = append(parts, fmt.Sprintf("subnets=%v", subnets))
		}
		if netID, ok := pending.networkIDs[name]; ok {
			parts = append(parts, fmt.Sprintf("netID=%d", netID))
		}
		if tunID, ok := pending.tunnelIDs[name]; ok {
			parts = append(parts, fmt.Sprintf("tunnelID=%d", tunID))
		}
		perNetwork = append(perNetwork, fmt.Sprintf("%s{%s}", name, strings.Join(parts, ", ")))
	}
	return strings.Join(perNetwork, "; ")
}

func (b *NodeAnnotationBatcher) updateNodeAnnotations(nodeName string, pending *pendingNodeUpdate) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cnode, err := b.kube.GetNode(nodeName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.Warningf("Node %s not found, dropping annotation update", nodeName)
				return nil
			}
			return fmt.Errorf("failed to get node %s: %w", nodeName, err)
		}

		existingSubnets, err := b.cache.ParseNodeHostSubnetsAnnotationCached(cnode)
		if err != nil && !util.IsAnnotationNotSetError(err) {
			return fmt.Errorf("failed to parse existing node-subnets for node %s: %w", nodeName, err)
		} else if existingSubnets == nil {
			existingSubnets = make(map[string][]*net.IPNet)
		}

		existingNetworkIDs, err := b.cache.ParseNetworkIDsMapCached(cnode)
		if err != nil && !util.IsAnnotationNotSetError(err) {
			return fmt.Errorf("failed to parse existing network-ids for node %s: %w", nodeName, err)
		} else if existingNetworkIDs == nil {
			existingNetworkIDs = make(map[string]int)
		}

		existingTunnelIDs, err := b.cache.ParseUDNLayer2NodeGRLRPTunnelIDsMapCached(cnode)
		if err != nil && !util.IsAnnotationNotSetError(err) {
			return fmt.Errorf("failed to parse existing tunnel-ids for node %s: %w", nodeName, err)
		} else if existingTunnelIDs == nil {
			existingTunnelIDs = make(map[string]int)
		}

		subnetsUpdated := false
		networkIDsUpdated := false
		tunnelIDsUpdated := false

		for networkName, subnets := range pending.hostSubnets {
			existingSubnets[networkName] = subnets
			subnetsUpdated = true
		}
		if subnetsUpdated {
			cnode.Annotations, err = util.UpdateNodeHostSubnetAnnotationMap(cnode.Annotations, existingSubnets)
			if err != nil {
				return fmt.Errorf("failed to update node-subnets annotation for node %s: %w", nodeName, err)
			}
		}

		for networkName, networkID := range pending.networkIDs {
			if networkID != types.NoNetworkID {
				existingNetworkIDs[networkName] = networkID
				networkIDsUpdated = true
			}
		}
		if networkIDsUpdated {
			cnode.Annotations, err = util.UpdateNetworkIDAnnotationMap(cnode.Annotations, existingNetworkIDs)
			if err != nil {
				return fmt.Errorf("failed to update network-ids annotation for node %s: %w", nodeName, err)
			}
		}

		for networkName, tunnelID := range pending.tunnelIDs {
			if tunnelID != types.NoTunnelID {
				existingTunnelIDs[networkName] = tunnelID
				tunnelIDsUpdated = true
			}
		}
		if tunnelIDsUpdated {
			cnode.Annotations, err = util.UpdateUDNLayer2NodeGRLRPTunnelIDsMap(cnode.Annotations, existingTunnelIDs)
			if err != nil {
				return fmt.Errorf("failed to update tunnel-ids annotation for node %s: %w", nodeName, err)
			}
		}

		klog.V(4).Infof("Updating node %s with batched annotations: [%s]", nodeName, pendingNodeUpdateToString(pending))

		// It is possible to update the node annotations using status subresource
		// because changes to metadata via status subresource are not restricted for nodes.
		err = b.kube.UpdateNodeStatus(cnode)
		if apierrors.IsNotFound(err) {
			klog.Warningf("Node %s not found during status update, dropping annotation update", nodeName)
			return nil
		}
		return err
	})
}
