package ovn

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func nodesToInterfaces(nodes []*corev1.Node) []interface{} {
	objs := make([]interface{}, 0, len(nodes))
	for _, node := range nodes {
		objs = append(objs, node)
	}
	return objs
}

func nodeSubnetChangedForUDN(oldNode, newNode *corev1.Node, netName string, cache util.NodeAnnotationCache, oldState, newState *util.NodeAnnotationState) bool {
	if !util.NodeSubnetAnnotationChanged(oldNode, newNode) {
		return false
	}
	if oldState != nil && newState != nil {
		return util.NodeSubnetAnnotationChangedForNetworkWithState(oldState, newState, netName)
	}
	return util.NodeSubnetAnnotationChangedForNetworkWithCache(oldNode, newNode, netName, cache)
}

// ReconcileNode reconciles a node for a layer3 UDN controller.
func (oc *Layer3UserDefinedNetworkController) ReconcileNode(oldNode, newNode *corev1.Node, oldState, newState *util.NodeAnnotationState) error {
	if newNode == nil {
		return fmt.Errorf("nil node received for network %s", oc.GetNetworkName())
	}

	if oc.isLocalZoneNode(newNode) {
		var nodeParams *nodeSyncs
		if oldNode == nil {
			_, nodeSync := oc.addNodeFailed.Load(newNode.Name)
			_, clusterRtrSync := oc.nodeClusterRouterPortFailed.Load(newNode.Name)
			_, syncMgmtPort := oc.mgmtPortFailed.Load(newNode.Name)
			_, syncGw := oc.gatewaysFailed.Load(newNode.Name)
			_, syncZoneIC := oc.syncZoneICFailed.Load(newNode.Name)
			_, syncReRoute := oc.syncEIPNodeRerouteFailed.Load(newNode.Name)
			if nodeSync || clusterRtrSync || syncMgmtPort || syncGw || syncZoneIC || syncReRoute {
				nodeParams = &nodeSyncs{
					syncNode:              nodeSync,
					syncClusterRouterPort: clusterRtrSync,
					syncMgmtPort:          syncMgmtPort,
					syncZoneIC:            syncZoneIC,
					syncGw:                syncGw,
					syncReroute:           syncReRoute,
				}
			} else {
				nodeParams = &nodeSyncs{
					syncNode:              true,
					syncClusterRouterPort: true,
					syncMgmtPort:          true,
					syncZoneIC:            config.OVNKubernetesFeature.EnableInterconnect,
					syncGw:                true,
					syncReroute:           true,
				}
			}
		} else if oc.isLocalZoneNode(oldNode) {
			zoneClusterChanged := oc.nodeZoneClusterChanged(oldNode, newNode)
			nodeSubnetChange := nodeSubnetChangedForUDN(oldNode, newNode, oc.GetNetworkName(), oc.nodeAnnotationCache, oldState, newState)

			_, nodeSync := oc.addNodeFailed.Load(newNode.Name)
			_, failed := oc.nodeClusterRouterPortFailed.Load(newNode.Name)
			clusterRtrSync := failed || nodeChassisChanged(oldNode, newNode) || nodeSubnetChange
			_, failed = oc.mgmtPortFailed.Load(newNode.Name)
			syncMgmtPort := failed || nodeSubnetChange
			_, syncZoneIC := oc.syncZoneICFailed.Load(newNode.Name)
			syncZoneIC = syncZoneIC || zoneClusterChanged
			_, failed = oc.gatewaysFailed.Load(newNode.Name)
			syncGw := failed ||
				gatewayChanged(oldNode, newNode) ||
				nodeSubnetChange ||
				hostCIDRsChanged(oldNode, newNode) ||
				nodeGatewayMTUSupportChanged(oldNode, newNode)
			_, failed = oc.syncEIPNodeRerouteFailed.Load(newNode.Name)
			syncReroute := failed || util.NodeHostCIDRsAnnotationChanged(oldNode, newNode)
			nodeParams = &nodeSyncs{
				syncNode:              nodeSync,
				syncClusterRouterPort: clusterRtrSync,
				syncMgmtPort:          syncMgmtPort,
				syncZoneIC:            syncZoneIC,
				syncGw:                syncGw,
				syncReroute:           syncReroute,
			}
		} else {
			klog.Infof("Node %s moved from the remote zone %s to local zone %s.",
				newNode.Name, util.GetNodeZone(oldNode), util.GetNodeZone(newNode))
			nodeParams = &nodeSyncs{
				syncNode:              true,
				syncClusterRouterPort: true,
				syncMgmtPort:          true,
				syncZoneIC:            config.OVNKubernetesFeature.EnableInterconnect,
				syncGw:                true,
				syncReroute:           true,
			}
		}
		return oc.addUpdateLocalNodeEvent(newNode, nodeParams)
	}

	if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		if !oc.networkManager.NodeHasNetwork(newNode.Name, oc.GetNetworkName()) {
			klog.V(5).Infof("Ignoring processing remote node: %s as it has no active NAD for network: %s",
				newNode.Name, oc.GetNetworkName())
			oc.syncZoneICFailed.Store(newNode.Name, true)
			return nil
		}
	}

	if oldNode == nil {
		return oc.addUpdateRemoteNodeEvent(newNode, config.OVNKubernetesFeature.EnableInterconnect)
	}

	zoneClusterChanged := oc.nodeZoneClusterChanged(oldNode, newNode)
	nodeSubnetChange := nodeSubnetChangedForUDN(oldNode, newNode, oc.GetNetworkName(), oc.nodeAnnotationCache, oldState, newState)
	_, syncZoneIC := oc.syncZoneICFailed.Load(newNode.Name)
	syncZoneIC = syncZoneIC || oc.isLocalZoneNode(oldNode) || nodeSubnetChange || zoneClusterChanged
	if syncZoneIC {
		klog.Infof("Node %s in remote zone %s needs interconnect zone sync up. Zone cluster changed: %v",
			newNode.Name, util.GetNodeZone(newNode), zoneClusterChanged)
	}
	return oc.addUpdateRemoteNodeEvent(newNode, syncZoneIC)
}

// DeleteNode deletes node resources for a layer3 UDN controller.
func (oc *Layer3UserDefinedNetworkController) DeleteNode(node *corev1.Node, _ *util.NodeAnnotationState) error {
	return oc.deleteNodeEvent(node)
}

// SyncNodes runs the node sync for a layer3 UDN controller.
func (oc *Layer3UserDefinedNetworkController) SyncNodes(nodes []*corev1.Node) error {
	return oc.syncNodes(nodesToInterfaces(nodes))
}

// ReconcileNode reconciles a node for a layer2 UDN controller.
func (oc *Layer2UserDefinedNetworkController) ReconcileNode(oldNode, newNode *corev1.Node, oldState, newState *util.NodeAnnotationState) error {
	if newNode == nil {
		return fmt.Errorf("nil node received for network %s", oc.GetNetworkName())
	}

	if oc.isLocalZoneNode(newNode) {
		var nodeParams *nodeSyncs
		if oldNode == nil {
			_, syncMgmtPort := oc.mgmtPortFailed.Load(newNode.Name)
			_, syncGw := oc.gatewaysFailed.Load(newNode.Name)
			_, syncReroute := oc.syncEIPNodeRerouteFailed.Load(newNode.Name)
			_, syncClusterRouterPort := oc.nodeClusterRouterPortFailed.Load(newNode.Name)
			if syncMgmtPort || syncGw || syncReroute || syncClusterRouterPort {
				nodeParams = &nodeSyncs{
					syncMgmtPort:          syncMgmtPort,
					syncGw:                syncGw,
					syncReroute:           syncReroute,
					syncClusterRouterPort: syncClusterRouterPort,
				}
			} else {
				nodeParams = &nodeSyncs{
					syncMgmtPort:          true,
					syncGw:                true,
					syncReroute:           true,
					syncClusterRouterPort: true,
				}
			}
		} else if oc.isLocalZoneNode(oldNode) {
			nodeSubnetChange := nodeSubnetChangedForUDN(oldNode, newNode, oc.GetNetworkName(), oc.nodeAnnotationCache, oldState, newState)
			_, mgmtUpdateFailed := oc.mgmtPortFailed.Load(newNode.Name)
			shouldSyncMgmtPort := mgmtUpdateFailed || nodeSubnetChange
			_, gwUpdateFailed := oc.gatewaysFailed.Load(newNode.Name)
			shouldSyncGW := gwUpdateFailed ||
				gatewayChanged(oldNode, newNode) ||
				hostCIDRsChanged(oldNode, newNode) ||
				nodeGatewayMTUSupportChanged(oldNode, newNode)
			_, syncRerouteFailed := oc.syncEIPNodeRerouteFailed.Load(newNode.Name)
			shouldSyncReroute := syncRerouteFailed || util.NodeHostCIDRsAnnotationChanged(oldNode, newNode)
			_, clusterRouterPortFailed := oc.nodeClusterRouterPortFailed.Load(newNode.Name)
			nodeParams = &nodeSyncs{
				syncMgmtPort:          shouldSyncMgmtPort,
				syncGw:                shouldSyncGW,
				syncReroute:           shouldSyncReroute,
				syncClusterRouterPort: clusterRouterPortFailed,
			}
		} else {
			klog.Infof("Node %s moved from the remote zone %s to local zone %s.",
				newNode.Name, util.GetNodeZone(oldNode), util.GetNodeZone(newNode))
			nodeParams = &nodeSyncs{
				syncMgmtPort:          true,
				syncGw:                true,
				syncReroute:           true,
				syncClusterRouterPort: true,
			}
		}
		return oc.addUpdateLocalNodeEvent(newNode, nodeParams, newState)
	}

	if config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		if !oc.networkManager.NodeHasNetwork(newNode.Name, oc.GetNetworkName()) {
			klog.V(5).Infof("Ignoring processing remote node: %s as it has no active NAD for network: %s",
				newNode.Name, oc.GetNetworkName())
			// store sync IC failed for the node, so if on node update if the NAD is no longer filtered, we actually
			// process it
			oc.syncZoneICFailed.Store(newNode.Name, true)
			return nil
		}
	}

	if oldNode == nil {
		return oc.addUpdateRemoteNodeEvent(newNode, config.OVNKubernetesFeature.EnableInterconnect, newState)
	}

	_, syncZoneIC := oc.syncZoneICFailed.Load(newNode.Name)
	_, oldNodeNoRouter := oc.remoteNodesNoRouter.Load(oldNode.Name)
	if oldNodeNoRouter && util.UDNLayer2NodeUsesTransitRouter(newNode) {
		syncZoneIC = true
	}
	return oc.addUpdateRemoteNodeEvent(newNode, syncZoneIC, newState)
}

// DeleteNode deletes node resources for a layer2 UDN controller.
func (oc *Layer2UserDefinedNetworkController) DeleteNode(node *corev1.Node, _ *util.NodeAnnotationState) error {
	return oc.deleteNodeEvent(node)
}

// SyncNodes runs the node sync for a layer2 UDN controller.
func (oc *Layer2UserDefinedNetworkController) SyncNodes(nodes []*corev1.Node) error {
	return oc.syncNodes(nodesToInterfaces(nodes))
}

// ReconcileNode reconciles a node for a localnet UDN controller.
func (oc *LocalnetUserDefinedNetworkController) ReconcileNode(_ *corev1.Node, newNode *corev1.Node, _ *util.NodeAnnotationState, _ *util.NodeAnnotationState) error {
	if newNode == nil {
		return fmt.Errorf("nil node received for network %s", oc.GetNetworkName())
	}
	return oc.addUpdateNodeEvent(newNode)
}

// DeleteNode deletes node resources for a localnet UDN controller.
func (oc *LocalnetUserDefinedNetworkController) DeleteNode(node *corev1.Node, _ *util.NodeAnnotationState) error {
	return oc.deleteNodeEvent(node)
}

// SyncNodes runs the node sync for a localnet UDN controller.
func (oc *LocalnetUserDefinedNetworkController) SyncNodes(nodes []*corev1.Node) error {
	return oc.syncNodes(nodesToInterfaces(nodes))
}
