package node

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// SecondaryNodeNetworkController structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints) for secondary network
type SecondaryNodeNetworkController struct {
	BaseNodeNetworkController
	// pod events factory handler
	podHandler *factory.Handler
	// responsible for programing gateway elements for this network
	gateway *UserDefinedNetworkGateway
}

// NewSecondaryNodeNetworkController creates a new OVN controller for creating logical network
// infrastructure and policy for the given secondary network. It supports layer3, layer2 and
// localnet topology types.
func NewSecondaryNodeNetworkController(
	cnnci *CommonNodeNetworkControllerInfo,
	netInfo util.NetInfo,
	vrfManager *vrfmanager.Controller,
	ruleManager *iprulemanager.Controller,
	defaultNetworkGateway Gateway,
) (*SecondaryNodeNetworkController, error) {

	snnc := &SecondaryNodeNetworkController{
		BaseNodeNetworkController: BaseNodeNetworkController{
			CommonNodeNetworkControllerInfo: *cnnci,
			ReconcilableNetInfo:             util.NewReconcilableNetInfo(netInfo),
			stopChan:                        make(chan struct{}),
			wg:                              &sync.WaitGroup{},
		},
	}
	if util.IsNetworkSegmentationSupportEnabled() && snnc.IsPrimaryNetwork() {
		node, err := snnc.watchFactory.GetNode(snnc.name)
		if err != nil {
			return nil, fmt.Errorf("error retrieving node %s while creating node network controller for network %s: %v",
				snnc.name, netInfo.GetNetworkName(), err)
		}

		snnc.gateway, err = NewUserDefinedNetworkGateway(snnc.GetNetInfo(), node,
			snnc.watchFactory.NodeCoreInformer().Lister(), snnc.Kube, vrfManager, ruleManager, defaultNetworkGateway)
		if err != nil {
			return nil, fmt.Errorf("error creating UDN gateway for network %s: %v", netInfo.GetNetworkName(), err)
		}
	}
	return snnc, nil
}

// Start starts the default controller; handles all events and creates all needed logical entities
func (nc *SecondaryNodeNetworkController) Start(_ context.Context) error {
	klog.Infof("Start secondary node network controller of network %s", nc.GetNetworkName())

	// enable adding ovs ports for dpu pods in both primary and secondary user defined networks
	if (config.OVNKubernetesFeature.EnableMultiNetwork || util.IsNetworkSegmentationSupportEnabled()) && config.OvnKubeNode.Mode == types.NodeModeDPU {
		handler, err := nc.watchPodsDPU()
		if err != nil {
			return err
		}
		nc.podHandler = handler
	}
	if util.IsNetworkSegmentationSupportEnabled() && nc.IsPrimaryNetwork() {
		if err := nc.gateway.AddNetwork(); err != nil {
			return fmt.Errorf("failed to add network to node gateway for network %s at node %s: %w",
				nc.GetNetworkName(), nc.name, err)
		}
	}
	return nil
}

// Stop gracefully stops the controller
func (nc *SecondaryNodeNetworkController) Stop() {
	klog.Infof("Stop secondary node network controller of network %s", nc.GetNetworkName())
	close(nc.stopChan)
	nc.wg.Wait()

	if nc.podHandler != nil {
		nc.watchFactory.RemovePodHandler(nc.podHandler)
	}
}

// Cleanup cleans up node entities for the given secondary network
func (nc *SecondaryNodeNetworkController) Cleanup() error {
	if nc.gateway != nil {
		return nc.gateway.DelNetwork()
	}
	return nil
}

func (nc *SecondaryNodeNetworkController) shouldReconcileNetworkChange(old, new util.NetInfo) bool {
	wasUDNNetworkAdvertisedAtNode := util.IsPodNetworkAdvertisedAtNode(old, nc.name)
	isUDNNetworkAdvertisedAtNode := util.IsPodNetworkAdvertisedAtNode(new, nc.name)
	return wasUDNNetworkAdvertisedAtNode != isUDNNetworkAdvertisedAtNode
}

// Reconcile function reconciles three entities based on whether UDN network is advertised
// and the gateway mode:
// 1. IP rules
// 2. OpenFlows on br-ex bridge to forward traffic to correct ofports
func (nc *SecondaryNodeNetworkController) Reconcile(netInfo util.NetInfo) error {
	reconcilePodNetwork := nc.shouldReconcileNetworkChange(nc.ReconcilableNetInfo, netInfo)

	err := util.ReconcileNetInfo(nc.ReconcilableNetInfo, netInfo)
	if err != nil {
		klog.Errorf("Failed to reconcile network information for network %s: %v", nc.GetNetworkName(), err)
	}

	if reconcilePodNetwork {
		if nc.gateway != nil {
			nc.gateway.Reconcile()
		}
	}

	return nil
}
