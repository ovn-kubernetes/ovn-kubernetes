// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"time"

	"github.com/containernetworking/plugins/pkg/ip"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const egressGatewaySNATChain = "egress-gateway-snat"

func (nc *DefaultNodeNetworkController) startEgressGateway() error {
	informer := nc.watchFactory.NodeCoreInformer()
	nc.egressGatewayController = controller.NewController("egress-gateway-snat", &controller.ControllerConfig[corev1.Node]{
		Informer: informer.Informer(), Lister: informer.Lister().List,
		ObjNeedsUpdate: func(old, node *corev1.Node) bool {
			if node.Name != nc.name {
				return false
			}
			if old == nil {
				return true
			}
			_, wasSelected := old.Labels[util.EgressGatewayLabel]
			_, selected := node.Labels[util.EgressGatewayLabel]
			return wasSelected != selected
		},
		Reconcile: nc.reconcileEgressGatewaySNAT, Threadiness: 1, MaxAttempts: controller.InfiniteAttempts,
	})
	if err := controller.StartWithInitialSync(func() error {
		return nc.reconcileEgressGatewaySNAT(context.Background(), nc.name)
	}, nc.egressGatewayController); err != nil {
		return err
	}

	healthCheckPort := config.OVNKubernetesFeature.EgressGatewayNodeHealthCheckPort

	// Copy the EgressIP management-port health endpoint setup, with a separate
	// server, port and configuration. It runs even when EgressIP is disabled.
	ifName := nc.mgmtPortController.GetInterfaceName()
	addresses := nc.mgmtPortController.GetAddresses()
	if len(addresses) == 0 {
		return fmt.Errorf("no management addresses for EgressGateway health on %s", ifName)
	}
	if err := ip.SettleAddresses(ifName, 10*time.Second); err != nil {
		return fmt.Errorf("failed to settle EgressGateway health addresses on %s: %w", ifName, err)
	}
	server, err := healthcheck.NewEgressGatewayHealthServer(addresses[0].IP, healthCheckPort)
	if err != nil {
		return err
	}
	nc.wg.Add(1)
	go func(stopCh <-chan struct{}) {
		defer nc.wg.Done()
		server.Run(stopCh)
	}(nc.stopChan)
	return nil
}

func (nc *DefaultNodeNetworkController) reconcileEgressGatewaySNAT(ctx context.Context, name string) error {
	if name != nc.name {
		return nil
	}
	node, err := nc.watchFactory.NodeCoreInformer().Lister().Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	selected := false
	if node != nil {
		_, selected = node.Labels[util.EgressGatewayLabel]
	}
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()
	if config.Gateway.Mode != config.GatewayModeLocal || !selected {
		// Add first so cleanup also works before this chain has ever existed.
		tx.Add(&knftables.Chain{Name: egressGatewaySNATChain})
		tx.Flush(&knftables.Chain{Name: egressGatewaySNATChain})
		tx.Delete(&knftables.Chain{Name: egressGatewaySNATChain})
	} else {
		tx.Add(&knftables.Chain{
			Name: egressGatewaySNATChain, Type: knftables.PtrTo(knftables.NATType),
			Hook: knftables.PtrTo(knftables.PostroutingHook),
			// EgressIP/EgressService run at 100; ordinary local SNAT runs at 101.
			Priority: knftables.PtrTo(knftables.BaseChainPriority("srcnat + 2")),
		})
		tx.Flush(&knftables.Chain{Name: egressGatewaySNATChain})
		for _, subnet := range config.Default.ClusterSubnets {
			rules, err := getLocalGatewayPodSubnetMasqueradeNFTRule(subnet.CIDR, nc.Gateway.GetDefaultPodNetworkAdvertised())
			if err != nil {
				return err
			}
			for _, rule := range rules {
				rule.Chain = egressGatewaySNATChain
				tx.Add(rule)
			}
		}
	}
	if err := nft.Run(ctx, tx); err != nil {
		return fmt.Errorf("failed to reconcile EgressGateway SNAT: %w", err)
	}
	return nil
}
