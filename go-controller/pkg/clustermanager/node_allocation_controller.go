// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	ipgenerator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// nodeAllocationController allocates node IDs and transit switch port addresses.
type nodeAllocationController struct {
	kube         kube.Interface
	watchFactory *factory.WatchFactory

	nodeController controller.Controller

	// ID allocator for the nodes
	nodeIDAllocator id.Allocator

	// Transit switch IP generators allocate node transit switch port addresses.
	transitSwitchIPv4Generator *ipgenerator.IPGenerator
	transitSwitchIPv6Generator *ipgenerator.IPGenerator
}

func newNodeAllocationController(ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory) (*nodeAllocationController, error) {
	// Since we don't assign 0 to any node, create IDAllocator with one extra element in maxIds.
	nodeIDAllocator := id.NewIDAllocator("NodeIDs", types.MaxNodeIDs+1)
	// Reserve the id 0. We don't want to assign this id to any of the nodes.
	if err := nodeIDAllocator.ReserveID("zero", 0); err != nil {
		return nil, fmt.Errorf("idAllocator failed to reserve id 0")
	}
	if err := nodeIDAllocator.ReserveID("one", 1); err != nil {
		return nil, fmt.Errorf("idAllocator failed to reserve id 1")
	}

	kube := &kube.Kube{
		KClient: ovnClient.KubeClient,
	}

	var transitSwitchIPv4Generator, transitSwitchIPv6Generator *ipgenerator.IPGenerator
	var err error
	if config.IPv4Mode {
		transitSwitchIPv4Generator, err = ipgenerator.NewIPGenerator(config.ClusterManager.V4TransitSubnet)
		if err != nil {
			return nil, fmt.Errorf("error creating IP Generator for v4 transit subnet %s: %w", config.ClusterManager.V4TransitSubnet, err)
		}
	}

	if config.IPv6Mode {
		transitSwitchIPv6Generator, err = ipgenerator.NewIPGenerator(config.ClusterManager.V6TransitSubnet)
		if err != nil {
			return nil, fmt.Errorf("error creating IP Generator for v6 transit subnet %s: %w", config.ClusterManager.V6TransitSubnet, err)
		}
	}

	nac := &nodeAllocationController{
		kube:                       kube,
		watchFactory:               wf,
		nodeIDAllocator:            nodeIDAllocator,
		transitSwitchIPv4Generator: transitSwitchIPv4Generator,
		transitSwitchIPv6Generator: transitSwitchIPv6Generator,
	}

	nac.nodeController = controller.NewController("node-allocation-controller", &controller.ControllerConfig[corev1.Node]{
		Informer:       wf.NodeCoreInformer().Informer(),
		ObjNeedsUpdate: nodeAllocationNeedsUpdate,
		Reconcile:      nac.reconcileNode,
		// A node can't do without its ID and transit switch port addresses.
		MaxAttempts: controller.InfiniteAttempts,
		Threadiness: 1,
	})
	return nac, nil
}

// Start starts the node allocation controller to watch the kubernetes nodes
func (nac *nodeAllocationController) Start(_ context.Context) error {
	// the IDs the nodes already have are reserved before any other is allocated
	err := controller.StartWithInitialSync(func() error {
		nodes, err := nac.watchFactory.GetNodes()
		if err != nil {
			return fmt.Errorf("failed to list nodes: %w", err)
		}
		return nac.syncNodeIDs(nodes)
	}, nac.nodeController)
	if err != nil {
		return fmt.Errorf("unable to watch nodes: %w", err)
	}
	return nil
}

func (nac *nodeAllocationController) Stop() {
	controller.Stop(nac.nodeController)
}

// nodeAllocationNeedsUpdate tells if what this controller sets on a node, or
// whether it has to set it, changed.
func nodeAllocationNeedsUpdate(oldNode, newNode *corev1.Node) bool {
	return oldNode == nil ||
		util.NodeIDAnnotationChanged(oldNode, newNode) ||
		util.NodeTransitSwitchPortAddrAnnotationChanged(oldNode, newNode) ||
		// a node switched between hybrid overlay node and ovn node
		util.NoHostSubnet(oldNode) != util.NoHostSubnet(newNode)
}

func (nac *nodeAllocationController) reconcileNode(ctx context.Context, nodeName string) error {
	node, err := nac.watchFactory.GetNode(nodeName)
	if apierrors.IsNotFound(err) {
		nac.nodeIDAllocator.ReleaseID(nodeName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	return nac.allocateNode(ctx, node)
}

// allocateNode makes sure the node has an ID and the transit switch port
// addresses derived from it, and that its annotations say so.
func (nac *nodeAllocationController) allocateNode(ctx context.Context, node *corev1.Node) error {
	if config.HybridOverlay.Enabled && util.NoHostSubnet(node) {
		// skip hybrid overlay nodes
		return nil
	}
	allocatedNodeID, err := nac.nodeIDAllocator.AllocateID(node.Name)
	if err != nil {
		return fmt.Errorf("failed to allocate an id to the node %s : err - %w", node.Name, err)
	}
	klog.V(5).Infof("Allocated id %d to the node %s", allocatedNodeID, node.Name)
	nodeAnnotations := util.UpdateNodeIDAnnotation(nil, allocatedNodeID)

	// Allocate the IP address(es) for the node Gateway router port connecting
	// to the Join switch
	var v4Addr, v6Addr *net.IPNet

	if config.IPv4Mode {
		v4Addr, err = nac.transitSwitchIPv4Generator.GenerateIP(allocatedNodeID)
		if err != nil {
			return fmt.Errorf("failed to generate transit switch port IPv4 address for node %s : err - %w", node.Name, err)
		}
	}

	if config.IPv6Mode {
		v6Addr, err = nac.transitSwitchIPv6Generator.GenerateIP(allocatedNodeID)
		if err != nil {
			return fmt.Errorf("failed to generate transit switch port IPv6 address for node %s : err - %w", node.Name, err)
		}
	}

	nodeAnnotations, err = util.CreateNodeTransitSwitchPortAddrAnnotation(nodeAnnotations, v4Addr, v6Addr)
	if err != nil {
		return fmt.Errorf("failed to marshal node %q transit switch port IP annotation, err : %w",
			node.Name, err)
	}

	upToDate := true
	for annotation, value := range nodeAnnotations {
		if node.Annotations[annotation] != value {
			upToDate = false
			break
		}
	}
	if upToDate {
		return nil
	}

	return nac.kube.SetAnnotationsOnNode(ctx, node.Name, nodeAnnotations)
}

func (nac *nodeAllocationController) syncNodeIDs(nodes []*corev1.Node) error {
	duplicateIdNodes := []string{}

	for _, node := range nodes {
		nodeID, _ := util.GetNodeID(node)
		if nodeID != util.InvalidNodeID {
			klog.Infof("Node %s has the id %d set", node.Name, nodeID)
			if err := nac.nodeIDAllocator.ReserveID(node.Name, nodeID); err != nil {
				// The id set on this node is duplicate.
				klog.Infof("Node %s has a duplicate id %d set", node.Name, nodeID)
				duplicateIdNodes = append(duplicateIdNodes, node.Name)
			}
		}
	}

	for i := range duplicateIdNodes {
		newNodeID, err := nac.nodeIDAllocator.AllocateID(duplicateIdNodes[i])
		if err != nil {
			return fmt.Errorf("failed to allocate id for node %s : err - %w", duplicateIdNodes[i], err)
		} else {
			klog.Infof("Allocated new id %d for node %q", newNodeID, duplicateIdNodes[i])
		}
	}

	return nil
}
