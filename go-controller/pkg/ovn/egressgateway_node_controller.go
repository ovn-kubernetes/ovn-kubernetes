// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/generator/udn"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type egressGatewayNodeController struct {
	oc         *DefaultNetworkController
	controller controller.Controller
}

type egressGatewayFamily struct {
	name         string
	cluster      sets.Set[string]
	local        sets.Set[string]
	remote       []*net.IPNet
	excluded     sets.Set[string]
	nextHops     sets.Set[string]
	localNextHop string
}

func newEgressGatewayNodeController(oc *DefaultNetworkController) *egressGatewayNodeController {
	c := &egressGatewayNodeController{oc: oc}
	informer := oc.watchFactory.NodeCoreInformer()
	c.controller = controller.NewController("egress-gateway-routing", &controller.ControllerConfig[corev1.Node]{
		Informer: informer.Informer(), Lister: informer.Lister().List,
		ObjNeedsUpdate: func(old, node *corev1.Node) bool {
			return old == nil || !reflect.DeepEqual(old.Labels, node.Labels) ||
				!reflect.DeepEqual(old.Annotations, node.Annotations) ||
				!reflect.DeepEqual(old.Status.Addresses, node.Status.Addresses) ||
				!reflect.DeepEqual(old.Status.Conditions, node.Status.Conditions) ||
				!reflect.DeepEqual(old.DeletionTimestamp, node.DeletionTimestamp)
		},
		Reconcile: c.reconcile, Threadiness: 1, MaxAttempts: controller.InfiniteAttempts,
	})
	return c
}

func (c *egressGatewayNodeController) Start() error {
	return controller.Start(c.controller)
}

func (c *egressGatewayNodeController) Stop() {
	controller.Stop(c.controller)
}

func egressGatewaySet(values sets.Set[string]) string {
	items := values.UnsortedList()
	slices.Sort(items)
	return "{ " + strings.Join(items, ", ") + " }"
}

func (c *egressGatewayNodeController) reconcile(_ context.Context, _ string) error {
	nodes, err := c.oc.watchFactory.NodeCoreInformer().Lister().List(labels.Everything())
	if err != nil {
		return err
	}
	families, topologyErr := c.topology(nodes)
	var policies []*nbdb.LogicalRouterPolicy
	var qoses []*nbdb.QoS
	for _, family := range families {
		if family.nextHops.Len() == 0 || family.local.Len() == 0 {
			continue
		}
		external := fmt.Sprintf("%s.dst != %s", family.name, egressGatewaySet(family.excluded))
		policies = append(policies, c.policy("local", family.name,
			fmt.Sprintf("%s.src == %s && %s", family.name, egressGatewaySet(family.local), external), family.nextHops))
		if family.localNextHop != "" {
			// Traffic arriving from another node exits locally instead of entering
			// ECMP again, which would permit loops between selected gateways.
			match := fmt.Sprintf("%s.src == %s && %s.src != %s && %s", family.name,
				egressGatewaySet(family.cluster), family.name, egressGatewaySet(family.local), external)
			policies = append(policies, c.policy("transit", family.name, match, sets.New(family.localNextHop)))
		}
		// Copy EgressIP's reply marking approach so service replies retain the
		// route through the node that owns the request's DNAT state.
		qoses = append(qoses, &nbdb.QoS{
			Priority: types.EgressGatewayReplyQoSPriority, Direction: nbdb.QoSDirectionFromLport,
			Match:       fmt.Sprintf("%s.src == %s && ct.trk && ct.rpl", family.name, egressGatewaySet(family.local)),
			Action:      map[string]int{"mark": types.EgressGatewayReplyTrafficMark},
			ExternalIDs: c.ids(libovsdbops.QoSEgressGateway, "reply", family.name).GetExternalIDs(),
		})
	}
	if len(qoses) != 0 {
		policies = append(policies, &nbdb.LogicalRouterPolicy{
			Priority: types.DefaultNoRereoutePriority, Action: nbdb.LogicalRouterPolicyActionAllow,
			Match:       fmt.Sprintf("pkt.mark == %d", types.EgressGatewayReplyTrafficMark),
			ExternalIDs: c.ids(libovsdbops.LogicalRouterPolicyEgressGateway, "reply", "ip").GetExternalIDs(),
		})
	}
	nats, err := c.gatewaySNATs(families)
	if err != nil {
		return errors.Join(topologyErr, err)
	}
	// Withdraw unusable next hops even if another node has incomplete topology.
	return errors.Join(topologyErr, c.apply(policies, qoses, nats))
}

func (c *egressGatewayNodeController) topology(nodes []*corev1.Node) ([]*egressGatewayFamily, error) {
	var families []*egressGatewayFamily
	var errs []error
	for _, ipv6 := range []bool{false, true} {
		family := &egressGatewayFamily{
			name: "ip4", cluster: sets.New[string](), local: sets.New[string](),
			excluded: sets.New[string](), nextHops: sets.New[string](),
		}
		if ipv6 {
			family.name = "ip6"
			family.excluded.Insert(config.Gateway.V6JoinSubnet, config.ClusterManager.V6TransitSubnet, "fe80::/10")
		} else {
			family.excluded.Insert(config.Gateway.V4JoinSubnet, config.ClusterManager.V4TransitSubnet, "169.254.0.0/16")
		}
		for _, subnet := range c.oc.Subnets() {
			if utilnet.IsIPv6CIDR(subnet.CIDR) == ipv6 {
				family.cluster.Insert(subnet.CIDR.String())
				family.excluded.Insert(subnet.CIDR.String())
			}
		}
		if family.cluster.Len() == 0 {
			continue
		}
		for _, subnet := range config.Kubernetes.ServiceCIDRs {
			if utilnet.IsIPv6CIDR(subnet) == ipv6 {
				family.excluded.Insert(subnet.String())
			}
		}
		for _, node := range nodes {
			for _, address := range node.Status.Addresses {
				if ip := net.ParseIP(address.Address); ip != nil && utilnet.IsIPv6(ip) == ipv6 {
					family.excluded.Insert(ip.String())
				}
			}
			if _, exists := node.Annotations[util.OVNNodeHostCIDRs]; exists {
				cidrs, err := util.ParseNodeHostCIDRs(node)
				if err != nil {
					errs = append(errs, err)
				}
				for cidr := range cidrs {
					ip, _, parseErr := net.ParseCIDR(cidr)
					if parseErr != nil {
						errs = append(errs, parseErr)
					} else if utilnet.IsIPv6(ip) == ipv6 {
						// Other hosts on the node's subnet still use the selected gateways.
						family.excluded.Insert(ip.String())
					}
				}
			}
			if util.NoHostSubnet(node) || node.DeletionTimestamp != nil {
				continue
			}
			_, selected := node.Labels[util.EgressGatewayLabel]
			selected = selected && node.Annotations[util.EgressGatewayReady] == "true"
			subnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
			if err != nil {
				if selected || node.Name == c.oc.nodeName {
					errs = append(errs, err)
				}
				continue
			}
			for _, subnet := range subnets {
				if utilnet.IsIPv6CIDR(subnet) != ipv6 {
					continue
				}
				if node.Name == c.oc.nodeName {
					family.local.Insert(subnet.String())
				} else {
					family.remote = append(family.remote, subnet)
				}
				if !selected {
					continue
				}
				nextHop, err := c.nextHop(node, subnet)
				if err != nil {
					errs = append(errs, fmt.Errorf("EgressGateway next hop for %s: %w", node.Name, err))
					continue
				}
				family.nextHops.Insert(nextHop)
				if node.Name == c.oc.nodeName {
					family.localNextHop = nextHop
				}
			}
		}
		families = append(families, family)
	}
	return families, errors.Join(errs...)
}

func (c *egressGatewayNodeController) nextHop(node *corev1.Node, subnet *net.IPNet) (string, error) {
	ipv6 := utilnet.IsIPv6CIDR(subnet)
	if node.Name != c.oc.nodeName {
		addresses, err := util.ParseNodeTransitSwitchPortAddrs(node)
		if err != nil {
			return "", err
		}
		address, err := util.MatchFirstIPNetFamily(ipv6, addresses)
		if err != nil {
			return "", err
		}
		return address.IP.String(), nil
	}
	joinIPs, err := udn.GetGWRouterIPs(node, &util.DefaultNetInfo{})
	if err != nil {
		return "", err
	}
	// OVN performs route lookup before policy routing. Remote pod sources need
	// this catch-all route even when the EgressIP controller is disabled.
	// The route is shared gateway infrastructure also used by EgressIP.
	if err := libovsdbutil.CreateDefaultRouteToExternal(c.oc.nbClient, c.oc.GetNetworkScopedClusterRouterName(),
		c.oc.GetNetworkScopedGWRouterName(node.Name), c.oc.Subnets(), joinIPs); err != nil {
		return "", err
	}
	if config.Gateway.Mode == config.GatewayModeShared {
		port := types.GWRouterToJoinSwitchPrefix + c.oc.GetNetworkScopedGWRouterName(node.Name)
		addresses, err := libovsdbutil.GetLRPAddrs(c.oc.nbClient, port)
		if err != nil {
			return "", err
		}
		address, err := util.MatchFirstIPNetFamily(ipv6, addresses)
		if err != nil {
			return "", err
		}
		return address.IP.String(), nil
	}
	if _, err := libovsdbops.GetLogicalSwitchPort(c.oc.nbClient, &nbdb.LogicalSwitchPort{Name: util.GetK8sMgmtIntfName(node.Name)}); err != nil {
		return "", err
	}
	return util.GetNodeManagementIfAddr(subnet).IP.String(), nil
}

func (c *egressGatewayNodeController) ids(kind *libovsdbops.ObjectIDsType, name, family string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(kind, c.oc.controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: name, libovsdbops.IPFamilyKey: family, libovsdbops.NetworkKey: types.DefaultNetworkName,
	})
}

func (c *egressGatewayNodeController) policy(name, family, match string, hops sets.Set[string]) *nbdb.LogicalRouterPolicy {
	nextHops := hops.UnsortedList()
	slices.Sort(nextHops)
	return &nbdb.LogicalRouterPolicy{
		Priority: types.EgressGatewayReroutePriority, Action: nbdb.LogicalRouterPolicyActionReroute,
		Match: match, Nexthops: nextHops,
		ExternalIDs: c.ids(libovsdbops.LogicalRouterPolicyEgressGateway, name, family).GetExternalIDs(),
	}
}

func (c *egressGatewayNodeController) gatewaySNATs(families []*egressGatewayFamily) ([]*nbdb.NAT, error) {
	if config.Gateway.Mode != config.GatewayModeShared || !config.Gateway.DisableSNATMultipleGWs {
		return nil, nil
	}
	var nats []*nbdb.NAT
	for _, family := range families {
		if family.localNextHop == "" || len(family.remote) == 0 {
			continue
		}
		externalIPs, err := getExternalIPsGR(c.oc.watchFactory, c.oc.nodeName)
		if err != nil {
			return nil, err
		}
		externalIP, err := util.MatchFirstIPNetFamily(family.name == "ip6", externalIPs)
		if err != nil {
			return nil, fmt.Errorf("EgressGateway external IP for %s: %w", c.oc.nodeName, err)
		}
		// Local pods already have per-pod SNAT. Remote node subnets keep these
		// rules below EgressIP's host prefixes and separate from the cluster-wide
		// SNAT rules that the normal gateway controller removes in this mode.
		for _, subnet := range family.remote {
			nats = append(nats, libovsdbops.BuildSNAT(&externalIP.IP, subnet, "",
				c.ids(libovsdbops.NATEgressGateway, subnet.String(), family.name).GetExternalIDs()))
		}
	}
	return nats, nil
}

func (c *egressGatewayNodeController) apply(policies []*nbdb.LogicalRouterPolicy, qoses []*nbdb.QoS, nats []*nbdb.NAT) error {
	policyIDs := libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressGateway, c.oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{libovsdbops.NetworkKey: types.DefaultNetworkName})
	owned := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](policyIDs, nil)
	wanted := sets.New[string]()
	var ops []ovsdb.Operation
	var err error
	for _, policy := range policies {
		id := policy.ExternalIDs[libovsdbops.PrimaryIDKey.String()]
		wanted.Insert(id)
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(c.oc.nbClient, ops,
			c.oc.GetNetworkScopedClusterRouterName(), policy, func(item *nbdb.LogicalRouterPolicy) bool {
				return owned(item) && item.ExternalIDs[libovsdbops.PrimaryIDKey.String()] == id
			}, &policy.Priority, &policy.Match, &policy.Action, &policy.Nexthops, &policy.ExternalIDs)
		if err != nil {
			return err
		}
	}
	ops, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(c.oc.nbClient, ops,
		c.oc.GetNetworkScopedClusterRouterName(), func(item *nbdb.LogicalRouterPolicy) bool {
			return owned(item) && !wanted.Has(item.ExternalIDs[libovsdbops.PrimaryIDKey.String()])
		})
	if err != nil {
		return err
	}
	ops, err = c.syncReplyQoS(ops, qoses)
	if err != nil {
		return err
	}
	ops, err = c.syncGatewaySNATs(ops, nats)
	if err != nil {
		return err
	}
	_, err = libovsdbops.TransactAndCheck(c.oc.nbClient, ops)
	return err
}

func (c *egressGatewayNodeController) syncGatewaySNATs(ops []ovsdb.Operation, desired []*nbdb.NAT) ([]ovsdb.Operation, error) {
	ids := libovsdbops.NewDbObjectIDs(libovsdbops.NATEgressGateway, c.oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{libovsdbops.NetworkKey: types.DefaultNetworkName})
	owned := libovsdbops.GetPredicate[*nbdb.NAT](ids, nil)
	existing, err := libovsdbops.FindNATsWithPredicate(c.oc.nbClient, owned)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*nbdb.NAT, len(existing))
	for _, nat := range existing {
		byID[nat.ExternalIDs[libovsdbops.PrimaryIDKey.String()]] = nat
	}
	wanted := sets.New[string]()
	for _, nat := range desired {
		id := nat.ExternalIDs[libovsdbops.PrimaryIDKey.String()]
		wanted.Insert(id)
		if current, exists := byID[id]; exists {
			// Keep the row identity when the gateway's external address changes.
			nat.UUID = current.UUID
		}
	}
	ops, err = libovsdbops.DeleteNATsWithPredicateOps(c.oc.nbClient, ops, func(nat *nbdb.NAT) bool {
		return owned(nat) && !wanted.Has(nat.ExternalIDs[libovsdbops.PrimaryIDKey.String()])
	})
	if err != nil || len(desired) == 0 {
		return ops, err
	}
	return libovsdbops.CreateOrUpdateNATsOps(c.oc.nbClient, ops,
		&nbdb.LogicalRouter{Name: c.oc.GetNetworkScopedGWRouterName(c.oc.nodeName)}, desired...)
}

func (c *egressGatewayNodeController) syncReplyQoS(ops []ovsdb.Operation, desired []*nbdb.QoS) ([]ovsdb.Operation, error) {
	ids := libovsdbops.NewDbObjectIDs(libovsdbops.QoSEgressGateway, c.oc.controllerName,
		map[libovsdbops.ExternalIDKey]string{libovsdbops.NetworkKey: types.DefaultNetworkName})
	existing, err := libovsdbops.FindQoSesWithPredicate(c.oc.nbClient, libovsdbops.GetPredicate[*nbdb.QoS](ids, nil))
	if err != nil {
		return nil, err
	}
	wanted := sets.New[string]()
	for _, qos := range desired {
		wanted.Insert(qos.ExternalIDs[libovsdbops.PrimaryIDKey.String()])
	}
	var stale []*nbdb.QoS
	for _, qos := range existing {
		if !wanted.Has(qos.ExternalIDs[libovsdbops.PrimaryIDKey.String()]) {
			stale = append(stale, qos)
		}
	}
	if len(stale) > 0 {
		ops, err = libovsdbops.RemoveQoSesFromLogicalSwitchOps(c.oc.nbClient, ops, c.oc.nodeName, stale...)
		if err != nil {
			return nil, err
		}
		ops, err = libovsdbops.DeleteQoSesOps(c.oc.nbClient, ops, stale...)
		if err != nil {
			return nil, err
		}
	}
	if len(desired) > 0 {
		ops, err = libovsdbops.CreateOrUpdateQoSesOps(c.oc.nbClient, ops, desired...)
		if err != nil {
			return nil, err
		}
		ops, err = libovsdbops.AddQoSesToLogicalSwitchOps(c.oc.nbClient, ops, c.oc.nodeName, desired...)
	}
	return ops, err
}
