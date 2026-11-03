// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corelisters "k8s.io/client-go/listers/core/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	egressGatewayHealthInterval = 5 * time.Second
	// Node names contain no slash, so this key is reserved for periodic health checks.
	egressGatewayHealthCheckKey = "egress-gateway/health-check"
)

type egressGatewayNode struct {
	uid           string
	managementIPs []net.IP
	healthy       bool
	client        healthcheck.EgressGatewayHealthClient
}

// egressGatewayClusterController owns gateway health independently of EgressIP.
// A single worker serializes all access to its health clients and node cache.
type egressGatewayClusterController struct {
	kube                             kube.Interface
	nodes                            corelisters.NodeLister
	gatewayNodes                     map[string]*egressGatewayNode
	controller                       controller.Controller
	egressGatewayTotalTimeout        int
	egressGatewayNodeHealthCheckPort int
}

func newEgressGatewayClusterController(ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory) *egressGatewayClusterController {
	informer := wf.NodeCoreInformer()
	c := &egressGatewayClusterController{
		kube:                             &kube.Kube{KClient: ovnClient.KubeClient},
		nodes:                            informer.Lister(),
		gatewayNodes:                     map[string]*egressGatewayNode{},
		egressGatewayTotalTimeout:        config.OVNKubernetesFeature.EgressGatewayReachabilityTotalTimeout,
		egressGatewayNodeHealthCheckPort: config.OVNKubernetesFeature.EgressGatewayNodeHealthCheckPort,
	}
	c.controller = controller.NewController("egress-gateway-health", &controller.ControllerConfig[corev1.Node]{
		Informer:       informer.Informer(),
		Lister:         informer.Lister().List,
		ObjNeedsUpdate: c.needsUpdate,
		Reconcile:      c.reconcile,
		Threadiness:    1,
		MaxAttempts:    controller.InfiniteAttempts,
	})
	return c
}

func (c *egressGatewayClusterController) needsUpdate(old, node *corev1.Node) bool {
	if old == nil {
		// Reconcile existing nodes at startup to initialize gateway membership.
		return true
	}
	_, wasSelected := old.Labels[util.EgressGatewayLabel]
	_, selected := node.Labels[util.EgressGatewayLabel]
	if wasSelected != selected {
		return true
	}
	if !selected {
		return false
	}
	return old.UID != node.UID
}

func (c *egressGatewayClusterController) Start() error {
	if err := controller.Start(c.controller); err != nil {
		return err
	}
	c.controller.Reconcile(egressGatewayHealthCheckKey)
	return nil
}

func (c *egressGatewayClusterController) Stop() {
	controller.Stop(c.controller)
	for name, gatewayNode := range c.gatewayNodes {
		gatewayNode.client.Disconnect()
		delete(c.gatewayNodes, name)
	}
}

func (c *egressGatewayClusterController) reconcile(ctx context.Context, name string) error {
	if name == egressGatewayHealthCheckKey {
		// 1. Health-check key: check all tracked gateways.
		return c.checkGatewayHealth(ctx)
	}
	node, err := c.nodes.Get(name)
	gatewayNode, exists := c.gatewayNodes[name]
	if apierrors.IsNotFound(err) {
		if !exists {
			// 3. Deleted node, untracked: noop
			return nil
		}
		// 2. Deleted node, tracked: disconnect its client and remove it.
		c.deleteGatewayNode(name, gatewayNode)
		return c.checkGatewayHealth(ctx)
	}
	if err != nil {
		// 8. Node lookup fails with another error: return the error.
		return err
	}

	_, labelOK := node.Labels[util.EgressGatewayLabel]
	switch {
	case !labelOK && exists:
		// 4. Gateway label removed, tracked: disconnect its client and remove it.
		c.deleteGatewayNode(name, gatewayNode)
	case !labelOK:
		// 5. New node or startup discovery without the gateway label: noop
		return nil
	case !exists:
		// 6. Gateway label present, untracked: add it.
		// Covers new nodes, startup discovery, label addition, and retries.
		err = c.addGatewayNode(node)
	case gatewayNode.uid != string(node.UID):
		// 7. Gateway label present, tracked, new UID: replace the recreated Node's entry.
		c.deleteGatewayNode(name, gatewayNode)
		err = c.addGatewayNode(node)
	default:
		// Gateway label present, tracked, same UID: keep the existing entry.
	}
	return errors.Join(err, c.checkGatewayHealth(ctx))
}

func (c *egressGatewayClusterController) checkGatewayHealth(ctx context.Context) error {
	defer c.controller.ReconcileAfter(egressGatewayHealthCheckKey, egressGatewayHealthInterval)

	var errs []error
	// TODO: Probe gateways in parallel so slow or unreachable gateways do not delay checks of others.
	for name, gatewayNode := range c.gatewayNodes {
		ok := false
		if len(gatewayNode.managementIPs) > 0 {
			dialCtx, dialCancel := context.WithTimeout(ctx, time.Duration(c.egressGatewayTotalTimeout)*time.Second)
			if gatewayNode.client.IsConnected() {
				ok = gatewayNode.client.Probe(dialCtx)
			} else {
				ok = gatewayNode.client.Connect(dialCtx, gatewayNode.managementIPs, c.egressGatewayNodeHealthCheckPort)
			}
			dialCancel()
		}
		if ok {
			gatewayNode.healthy = true
			if err := c.kube.SetAnnotationsOnNode(ctx, name, map[string]interface{}{util.EgressGatewayReady: "true"}); err != nil {
				errs = append(errs, fmt.Errorf("failed to add EgressGateway readiness annotation to node %s: %w", name, err))
				continue
			}
			// TODO: Record an EgressGatewayReady Node event.
			continue
		}
		gatewayNode.healthy = false
		if err := c.kube.SetAnnotationsOnNode(ctx, name, map[string]interface{}{util.EgressGatewayReady: nil}); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove EgressGateway readiness annotation from node %s: %w", name, err))
			continue
		}
		// TODO: Record an EgressGatewayNotReady Node event.
	}
	return errors.Join(errs...)
}

func (c *egressGatewayClusterController) deleteGatewayNode(name string, gatewayNode *egressGatewayNode) {
	gatewayNode.client.Disconnect()
	delete(c.gatewayNodes, name)
}

func (c *egressGatewayClusterController) addGatewayNode(node *corev1.Node) error {
	subnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
	if err != nil {
		return err
	}
	managementIPs := make([]net.IP, 0, len(subnets))
	for _, subnet := range subnets {
		managementIPs = append(managementIPs, util.GetNodeManagementIfAddr(subnet).IP)
	}
	if len(managementIPs) == 0 {
		return fmt.Errorf("no management IPs for EgressGateway node %s", node.Name)
	}
	c.gatewayNodes[node.Name] = &egressGatewayNode{
		uid: string(node.UID), managementIPs: managementIPs, client: healthcheck.NewEgressGatewayHealthClient(node.Name),
	}
	return nil
}
