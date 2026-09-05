// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"sync"

	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinklisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/listers/uplink/v1alpha1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// UserDefinedNodeNetworkController structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints) for user-defined networks
type UserDefinedNodeNetworkController struct {
	BaseNodeNetworkController
	// pod events factory handler
	podHandler *factory.Handler
	// responsible for programing gateway elements for this network
	gateway *UserDefinedNetworkGateway
	// management port device manager
	mpdm *managementport.MgmtPortDeviceManager

	// Dependencies retained so the node-side gateway can be constructed lazily
	// if a secondary network transitions to advertised at runtime (see
	// Reconcile). At construction time the gateway is only built for primary
	// networks and secondary networks that are already advertised; a secondary
	// CUDN created before its RouteAdvertisements has no gateway yet.
	vrfManager              *vrfmanager.Controller
	ruleManager             *iprulemanager.Controller
	defaultNetworkGateway   Gateway
	uplinkGatewayController *UplinkGatewayController
}

// NewUserDefinedNodeNetworkController creates a new OVN controller for creating logical network
// infrastructure and policy for the given secondary network. It supports layer3, layer2 and
// localnet topology types.
func NewUserDefinedNodeNetworkController(
	cnnci *CommonNodeNetworkControllerInfo,
	netInfo util.NetInfo,
	networkManager networkmanager.Interface,
	vrfManager *vrfmanager.Controller,
	ruleManager *iprulemanager.Controller,
	mpdm *managementport.MgmtPortDeviceManager,
	defaultNetworkGateway Gateway,
	ovsClient client.Client,
	uplinkGatewayController *UplinkGatewayController,
) (*UserDefinedNodeNetworkController, error) {
	if netInfo.Uplink() != "" && config.Gateway.Mode != config.GatewayModeShared {
		return nil, fmt.Errorf("uplink %q for network %s is supported only in shared gateway mode",
			netInfo.Uplink(), netInfo.GetNetworkName())
	}

	snnc := &UserDefinedNodeNetworkController{
		BaseNodeNetworkController: BaseNodeNetworkController{
			CommonNodeNetworkControllerInfo: *cnnci,
			ReconcilableNetInfo:             util.NewReconcilableNetInfo(netInfo),
			stopChan:                        make(chan struct{}),
			wg:                              &sync.WaitGroup{},
			networkManager:                  networkManager,
			ovsClient:                       ovsClient,
		},
		mpdm:                    mpdm,
		vrfManager:              vrfManager,
		ruleManager:             ruleManager,
		defaultNetworkGateway:   defaultNetworkGateway,
		uplinkGatewayController: uplinkGatewayController,
	}
	if util.IsNetworkSegmentationSupportEnabled() &&
		(snnc.IsPrimaryNetwork() || isAdvertisedSecondaryUDNAtNode(snnc.GetNetInfo(), snnc.name)) {
		if err := snnc.buildGateway(); err != nil {
			return nil, err
		}
	}
	return snnc, nil
}

// buildGateway constructs the node-side UDN gateway for this network and stores
// it on nc.gateway. It is used both at construction time (for primary networks
// and secondary networks that are already advertised) and lazily from Reconcile
// when a secondary network transitions to advertised at runtime. It does not
// program the datapath; the caller must invoke nc.gateway.AddNetwork().
func (nc *UserDefinedNodeNetworkController) buildGateway() error {
	node, err := nc.watchFactory.GetNode(nc.name)
	if err != nil {
		return fmt.Errorf("error retrieving node %s while creating node network controller for network %s: %v",
			nc.name, nc.GetNetworkName(), err)
	}

	var uplinkStateLister uplinklisters.UplinkStateLister
	if util.IsUplinkEnabled() {
		uplinkStateLister = nc.watchFactory.UplinkStateInformer().Lister()
	}
	gateway, err := NewUserDefinedNetworkGateway(nc.GetNetInfo(), node,
		nc.watchFactory.NodeCoreInformer().Lister(), nc.Kube, nc.vrfManager, nc.ruleManager, nc.defaultNetworkGateway,
		nc.ovsClient, uplinkStateLister, nc.uplinkGatewayController)
	if err != nil {
		return fmt.Errorf("error creating UDN gateway for network %s: %v", nc.GetNetworkName(), err)
	}
	nc.gateway = gateway
	return nil
}

// isAdvertisedSecondaryUDNAtNode reports whether netInfo is a SECONDARY
// user-defined network that is BGP-advertised at the given node.
//
// The node-side UDN gateway (management port, per-network VRF/ip-rules, br-ex
// OpenFlow, advertised UDN isolation) is provisioned for primary UDNs and, when
// this predicate holds, for advertised secondary UDNs so they get a north-south
// datapath. A plain (non-advertised) secondary UDN stays east/west-only and
// unchanged. This depends on the OVN-side datapath (join subnet, management
// port, NAT) being present for the network.
func isAdvertisedSecondaryUDNAtNode(netInfo util.NetInfo, nodeName string) bool {
	return netInfo != nil &&
		netInfo.IsUserDefinedNetwork() &&
		!netInfo.IsPrimaryNetwork() &&
		util.IsPodNetworkAdvertisedAtNode(netInfo, nodeName)
}

// Start starts the default controller; handles all events and creates all needed logical entities
func (nc *UserDefinedNodeNetworkController) Start(_ context.Context) error {
	klog.Infof("Starting UDN node network controller for network %s", nc.GetNetworkName())

	// enable adding ovs ports for dpu pods in both primary and secondary user-defined networks
	if (config.OVNKubernetesFeature.EnableMultiNetwork || util.IsNetworkSegmentationSupportEnabled()) && config.IsModeDPU() {
		handler, err := nc.watchPodsDPU()
		if err != nil {
			return err
		}
		nc.podHandler = handler
	}
	if util.IsNetworkSegmentationSupportEnabled() &&
		(nc.IsPrimaryNetwork() || isAdvertisedSecondaryUDNAtNode(nc.GetNetInfo(), nc.name)) {
		if err := nc.gateway.AddNetwork(); err != nil {
			return fmt.Errorf("failed to add network to node gateway for network %s at node %s: %w",
				nc.GetNetworkName(), nc.name, err)
		}
	}
	return nil
}

// Stop gracefully stops the controller
func (nc *UserDefinedNodeNetworkController) Stop() {
	if nc.stopChan == nil {
		klog.Infof("UDN node network controller for network %s is already stopped", nc.GetNetworkName())
		return
	}
	klog.Infof("Stopping UDN node network controller for network %s", nc.GetNetworkName())
	close(nc.stopChan)
	nc.stopChan = nil
	nc.wg.Wait()

	if nc.podHandler != nil {
		nc.watchFactory.RemovePodHandler(nc.podHandler)
	}
}

// Cleanup cleans up node entities for the given user-defined network
func (nc *UserDefinedNodeNetworkController) Cleanup() error {
	var errors []error
	var err error

	if nc.gateway != nil {
		if err = nc.gateway.DelNetwork(); err != nil {
			errors = append(errors, fmt.Errorf("deleting network gateway for network %s failed: %v", nc.GetNetworkName(), err))
		}
	}
	if nc.mpdm != nil && util.IsNetworkSegmentationSupportEnabled() &&
		(nc.IsPrimaryNetwork() || isAdvertisedSecondaryUDNAtNode(nc.GetNetInfo(), nc.name)) {
		if err = nc.mpdm.ReleaseDeviceIDForNetwork(nc.GetNetworkName()); err != nil {
			errors = append(errors, fmt.Errorf("deleting device ID for network %s failed: %v", nc.GetNetworkName(), err))
		}
	}
	if len(errors) > 0 {
		return kerrors.NewAggregate(errors)
	}
	return nil
}

// HandleNetworkRefChange satisfies the NetworkController interface. UDN node controllers only
// manage local node state, so NAD reference changes for remote nodes are ignored.
func (nc *UserDefinedNodeNetworkController) HandleNetworkRefChange(_ string, _ bool) {}

func (nc *UserDefinedNodeNetworkController) shouldReconcileNetworkChange(old, new util.NetInfo) bool {
	switch {
	case util.IsPodNetworkAdvertisedAtNode(old, nc.name) != util.IsPodNetworkAdvertisedAtNode(new, nc.name):
		return true
	case util.IsPodNetworkAdvertisedAtNodeDefaultVRF(old, nc.name) != util.IsPodNetworkAdvertisedAtNodeDefaultVRF(new, nc.name):
		return true
	}
	return false
}

// Reconcile function reconciles three entities based on whether UDN network is advertised
// and the gateway mode:
// 1. IP rules
// 2. OpenFlows on br-ex bridge to forward traffic to correct ofports
func (nc *UserDefinedNodeNetworkController) Reconcile(netInfo util.NetInfo) error {
	reconcilePodNetwork := nc.shouldReconcileNetworkChange(nc.ReconcilableNetInfo, netInfo)
	if reconcilePodNetwork && nc.gateway != nil && nc.Uplink() != "" {
		if err := nc.gateway.uplinkGatewayController.PrepareNetwork(netInfo); err != nil {
			return fmt.Errorf("failed to prepare Uplink gateway reconciliation for network %s: %w",
				nc.GetNetworkName(), err)
		}
	}

	err := util.ReconcileNetInfo(nc.ReconcilableNetInfo, netInfo)
	if err != nil {
		klog.Errorf("Failed to reconcile network information for network %s: %v", nc.GetNetworkName(), err)
	}

	if reconcilePodNetwork {
		nowNeedsGateway := util.IsNetworkSegmentationSupportEnabled() &&
			(nc.IsPrimaryNetwork() || isAdvertisedSecondaryUDNAtNode(netInfo, nc.name))
		switch {
		case nc.gateway == nil && nowNeedsGateway:
			// The network became advertised at runtime (e.g. a secondary CUDN
			// created before its RouteAdvertisements). The gateway was not built
			// at construction time, so build it now and program the node-side
			// datapath (management port, VRF/ip-rules, breth0 OpenFlow). Without
			// this the NBDB topology exists but there is no north-south datapath.
			if err := nc.buildGateway(); err != nil {
				return fmt.Errorf("failed to build node gateway for newly advertised network %s: %w",
					nc.GetNetworkName(), err)
			}
			if err := nc.gateway.AddNetwork(); err != nil {
				nc.gateway = nil
				return fmt.Errorf("failed to add newly advertised network %s to node gateway at node %s: %w",
					nc.GetNetworkName(), nc.name, err)
			}
		case nc.gateway != nil && !nowNeedsGateway && !nc.IsPrimaryNetwork():
			// A secondary network stopped being advertised; tear down the
			// node-side datapath that buildGateway/AddNetwork installed.
			if err := nc.gateway.DelNetwork(); err != nil {
				return fmt.Errorf("failed to remove de-advertised network %s from node gateway: %w",
					nc.GetNetworkName(), err)
			}
			nc.gateway = nil
		case nc.gateway != nil:
			nc.gateway.Reconcile()
		}
	}

	return nil
}
