// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"net"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	egresssvc "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/egressservice"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// initClusterEgressPoliciesOps is the ops-building version of InitClusterEgressPolicies
// It batches all cluster-wide egress policy creation
func initClusterEgressPoliciesOps(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory, ni util.NetInfo,
	clusterSubnets []*net.IPNet, controllerName, routerName string, clusterNodeIPsAddrSetDbIDs *libovsdbops.DbObjectIDs, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {

	if len(clusterSubnets) == 0 {
		return ops, nil
	}

	var v4ClusterSubnet, v6ClusterSubnet []*net.IPNet
	for _, subnet := range clusterSubnets {
		if utilnet.IsIPv6CIDR(subnet) {
			v6ClusterSubnet = append(v6ClusterSubnet, subnet)
		} else {
			v4ClusterSubnet = append(v4ClusterSubnet, subnet)
		}
	}

	var v4JoinSubnet, v6JoinSubnet *net.IPNet
	var err error
	if len(v4ClusterSubnet) > 0 {
		if config.Gateway.V4JoinSubnet == "" {
			return ops, fmt.Errorf("network %s: cannot process IPv4 addresses because no IPv4 join subnet is available", ni.GetNetworkName())
		}
		_, v4JoinSubnet, err = net.ParseCIDR(config.Gateway.V4JoinSubnet)
		if err != nil {
			return ops, fmt.Errorf("network %s: failed to parse IPv4 join subnet: %v", ni.GetNetworkName(), err)
		}
	}
	if len(v6ClusterSubnet) > 0 {
		if config.Gateway.V6JoinSubnet == "" {
			return ops, fmt.Errorf("network %s: cannot process IPv6 addresses because no IPv6 join subnet is available", ni.GetNetworkName())
		}
		_, v6JoinSubnet, err = net.ParseCIDR(config.Gateway.V6JoinSubnet)
		if err != nil {
			return ops, fmt.Errorf("network %s: failed to parse IPv6 join subnet: %v", ni.GetNetworkName(), err)
		}
	}

	// Create pod-to-pod policies (batched)
	ops, err = createDefaultNoReroutePodPoliciesOps(nbClient, ni.GetNetworkName(), controllerName, routerName, v4ClusterSubnet, v6ClusterSubnet, ops)
	if err != nil {
		return ops, fmt.Errorf("failed to create no reroute policies for pods on network %s: %v", ni.GetNetworkName(), err)
	}

	// Create pod-to-join policies (batched)
	ops, err = createDefaultNoRerouteServicePoliciesOps(nbClient, ni.GetNetworkName(), controllerName, routerName, v4ClusterSubnet, v6ClusterSubnet,
		v4JoinSubnet, v6JoinSubnet, ops)
	if err != nil {
		return ops, fmt.Errorf("failed to create no reroute policies for services on network %s: %v", ni.GetNetworkName(), err)
	}

	// Create reply traffic policy (batched)
	ops, err = createDefaultNoRerouteReplyTrafficPolicyOps(nbClient, ni, controllerName, routerName, ops)
	if err != nil {
		return ops, fmt.Errorf("failed to create no reroute reply traffic policy for network %s: %v", ni.GetNetworkName(), err)
	}

	// ensure the address-set for storing nodeIPs exists
	// The address set with controller name 'default' is shared with all networks
	if clusterNodeIPsAddrSetDbIDs == nil {
		return ops, fmt.Errorf("cluster node IP address set DB IDs are required")
	}
	if _, err = addressSetFactory.EnsureAddressSet(clusterNodeIPsAddrSetDbIDs); err != nil {
		return ops, fmt.Errorf("cannot ensure that addressSet %s exists %v", NodeIPAddrSetName, err)
	}

	// ensure the address-set for storing egressIP pods exists
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, ni.GetNetworkName(), controllerName)
	_, err = addressSetFactory.EnsureAddressSet(dbIDs)
	if err != nil {
		return ops, fmt.Errorf("cannot ensure that addressSet for egressIP pods %s exists for network %s: %v", EgressIPServedPodsAddrSetName, ni.GetNetworkName(), err)
	}

	// ensure the address-set for storing egressservice pod backends exists
	dbIDs = egresssvc.GetEgressServiceAddrSetDbIDs(controllerName)
	_, err = addressSetFactory.EnsureAddressSet(dbIDs)
	if err != nil {
		return ops, fmt.Errorf("cannot ensure that addressSet for egressService pods %s exists %v", egresssvc.EgressServiceServedPodsAddrSetName, err)
	}

	if !ni.IsDefault() && isEgressIPForUDNSupported() {
		v4, v6 := len(v4ClusterSubnet) > 0, len(v6ClusterSubnet) > 0
		ops, err = ensureDefaultNoRerouteUDNEnabledSvcPoliciesOps(nbClient, addressSetFactory, ni, controllerName, routerName, v4, v6, ops)
		if err != nil {
			return ops, fmt.Errorf("failed to ensure no reroute for UDN enabled services for network %s: %v", ni.GetNetworkName(), err)
		}
	}

	return ops, nil
}

// createDefaultNoReroutePodPoliciesOps is the ops-building version of createDefaultNoReroutePodPolicies
// It batches pod-to-pod policy creation (O(S²) policies)
func createDefaultNoReroutePodPoliciesOps(nbClient libovsdbclient.Client, network, controller, routerName string,
	v4ClusterSubnet, v6ClusterSubnet []*net.IPNet, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {

	// Create policies for all v4 subnet pairs
	for _, v4Subnet1 := range v4ClusterSubnet {
		for _, v4Subnet2 := range v4ClusterSubnet {
			match := fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Subnet1.String(), v4Subnet2.String())
			dbIDs := getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV4, network, controller)
			lrp := nbdb.LogicalRouterPolicy{
				Priority:    types.DefaultNoRereoutePriority,
				Action:      nbdb.LogicalRouterPolicyActionAllow,
				Match:       match,
				ExternalIDs: dbIDs.GetExternalIDs(),
			}
			// Use predicate with match filter for pod-to-pod policies (handles multiple cluster subnets)
			p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Match == match
			})
			var err error
			ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient, ops, routerName, &lrp, p)
			if err != nil {
				return ops, fmt.Errorf("failed to create pod-to-pod policy ops for %s->%s: %v", v4Subnet1, v4Subnet2, err)
			}
		}
	}

	// Create policies for all v6 subnet pairs
	for _, v6Subnet1 := range v6ClusterSubnet {
		for _, v6Subnet2 := range v6ClusterSubnet {
			match := fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6Subnet1.String(), v6Subnet2.String())
			dbIDs := getEgressIPLRPNoReRoutePodToPodDbIDs(IPFamilyValueV6, network, controller)
			lrp := nbdb.LogicalRouterPolicy{
				Priority:    types.DefaultNoRereoutePriority,
				Action:      nbdb.LogicalRouterPolicyActionAllow,
				Match:       match,
				ExternalIDs: dbIDs.GetExternalIDs(),
			}
			// Use predicate with match filter for pod-to-pod policies (handles multiple cluster subnets)
			p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Match == match
			})
			var err error
			ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient, ops, routerName, &lrp, p)
			if err != nil {
				return ops, fmt.Errorf("failed to create pod-to-pod policy ops for %s->%s: %v", v6Subnet1, v6Subnet2, err)
			}
		}
	}

	return ops, nil
}

// createDefaultNoRerouteServicePoliciesOps is the ops-building version of createDefaultNoRerouteServicePolicies
// It batches pod-to-join policy creation
func createDefaultNoRerouteServicePoliciesOps(nbClient libovsdbclient.Client, network, controller, routerName string,
	v4ClusterSubnet, v6ClusterSubnet []*net.IPNet, v4JoinSubnet, v6JoinSubnet *net.IPNet, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {

	// Create v4 pod-to-join policies
	if v4JoinSubnet != nil {
		for _, v4Subnet := range v4ClusterSubnet {
			match := fmt.Sprintf("ip4.src == %s && ip4.dst == %s", v4Subnet.String(), v4JoinSubnet.String())
			dbIDs := getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV4, network, controller)
			lrp := nbdb.LogicalRouterPolicy{
				Priority:    types.DefaultNoRereoutePriority,
				Action:      nbdb.LogicalRouterPolicyActionAllow,
				Match:       match,
				ExternalIDs: dbIDs.GetExternalIDs(),
			}
			// Use predicate with match filter for pod-to-join policies (handles multiple cluster subnets)
			p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Match == match
			})
			var err error
			ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient, ops, routerName, &lrp, p)
			if err != nil {
				return ops, fmt.Errorf("failed to create pod-to-join policy ops for %s: %v", v4Subnet, err)
			}
		}
	}

	// Create v6 pod-to-join policies
	if v6JoinSubnet != nil {
		for _, v6Subnet := range v6ClusterSubnet {
			match := fmt.Sprintf("ip6.src == %s && ip6.dst == %s", v6Subnet.String(), v6JoinSubnet.String())
			dbIDs := getEgressIPLRPNoReRoutePodToJoinDbIDs(IPFamilyValueV6, network, controller)
			lrp := nbdb.LogicalRouterPolicy{
				Priority:    types.DefaultNoRereoutePriority,
				Action:      nbdb.LogicalRouterPolicyActionAllow,
				Match:       match,
				ExternalIDs: dbIDs.GetExternalIDs(),
			}
			// Use predicate with match filter for pod-to-join policies (handles multiple cluster subnets)
			p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Match == match
			})
			var err error
			ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient, ops, routerName, &lrp, p)
			if err != nil {
				return ops, fmt.Errorf("failed to create pod-to-join policy ops for %s: %v", v6Subnet, err)
			}
		}
	}

	return ops, nil
}

// createDefaultNoRerouteReplyTrafficPolicyOps is the ops-building version of createDefaultNoRerouteReplyTrafficPolicy
func createDefaultNoRerouteReplyTrafficPolicyOps(nbClient libovsdbclient.Client, ni util.NetInfo, controller, routerName string, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {
	network := ni.GetNetworkName()
	dbIDs := getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, ReplyTrafficNoReroute, IPFamilyValue, network, controller)
	lrp := nbdb.LogicalRouterPolicy{
		Priority:    types.DefaultNoRereoutePriority,
		Action:      nbdb.LogicalRouterPolicyActionAllow,
		Match:       fmt.Sprintf("pkt.mark == %d", types.EgressIPReplyTrafficConnectionMark),
		ExternalIDs: dbIDs.GetExternalIDs(),
	}
	var err error
	libovsdbutil.SetDefaultEgressNoReroutePolicy(&lrp, ni, types.DefaultEgressChain)
	ops, err = libovsdbutil.EnsureDefaultEgressChainOps(nbClient, ops, routerName, ni, types.DefaultEgressChain)
	if err != nil {
		return nil, err
	}
	// Use predicate generator based on dbIDs to match the original implementation
	p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
	return libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient, ops, routerName, &lrp, p, &lrp.Action, &lrp.JumpChain, &lrp.Options)
}

// ensureDefaultNoRerouteUDNEnabledSvcPoliciesOps is the ops-building version of ensureDefaultNoRerouteUDNEnabledSvcPolicies
func ensureDefaultNoRerouteUDNEnabledSvcPoliciesOps(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory,
	ni util.NetInfo, controller, routerName string, v4, v6 bool, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {

	var err error
	var as addressset.AddressSet
	// fetch the egressIP pods address-set
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, ni.GetNetworkName(), controller)
	if as, err = addressSetFactory.EnsureAddressSet(dbIDs); err != nil {
		return ops, fmt.Errorf("cannot ensure that addressSet %s exists %v", EgressIPServedPodsAddrSetName, err)
	}
	ipv4EgressIPServedPodsAS, ipv6EgressIPServedPodsAS := as.GetASHashNames()

	// fetch the egressService pods address-set
	dbIDs = egresssvc.GetEgressServiceAddrSetDbIDs(controller)
	if as, err = addressSetFactory.EnsureAddressSet(dbIDs); err != nil {
		return ops, fmt.Errorf("cannot ensure that addressSet %s exists %v", egresssvc.EgressServiceServedPodsAddrSetName, err)
	}
	ipv4EgressServiceServedPodsAS, ipv6EgressServiceServedPodsAS := as.GetASHashNames()

	dbIDs = udnenabledsvc.GetAddressSetDBIDs()
	var ipv4UDNEnabledSvcAS, ipv6UDNEnabledSvcAS string
	// address set maybe not created immediately
	err = wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 5*time.Second, true, func(_ context.Context) (done bool, err error) {
		as, err := addressSetFactory.GetAddressSet(dbIDs)
		if err != nil {
			klog.V(5).Infof("Failed to get UDN enabled service address set, retrying: %v", err)
			return false, nil
		}
		ipv4UDNEnabledSvcAS, ipv6UDNEnabledSvcAS = as.GetASHashNames()
		if ipv4UDNEnabledSvcAS == "" && ipv6UDNEnabledSvcAS == "" {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return ops, fmt.Errorf("failed to retrieve UDN enabled service address set from NB DB: %v", err)
	}
	// if address set hash name is empty, the address set has yet to be created
	if (v4 && ipv4UDNEnabledSvcAS == "") || (v6 && ipv6UDNEnabledSvcAS == "") {
		return ops, types.NewSuppressedError(fmt.Errorf("failed to retrieve UDN enabled service address set"))
	}

	var matchV4, matchV6 string
	// construct the policy match
	if v4 {
		if ipv4EgressIPServedPodsAS == "" || ipv4EgressServiceServedPodsAS == "" || ipv4UDNEnabledSvcAS == "" {
			return ops, fmt.Errorf("address set hash name(s) not found")
		}
		matchV4 = fmt.Sprintf(`(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s`,
			ipv4EgressIPServedPodsAS, ipv4EgressServiceServedPodsAS, ipv4UDNEnabledSvcAS)
	}
	if v6 {
		if ipv6EgressIPServedPodsAS == "" || ipv6EgressServiceServedPodsAS == "" || ipv6UDNEnabledSvcAS == "" {
			return ops, fmt.Errorf("address set hash name(s) not found")
		}
		matchV6 = fmt.Sprintf(`(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s`,
			ipv6EgressIPServedPodsAS, ipv6EgressServiceServedPodsAS, ipv6UDNEnabledSvcAS)
	}

	// Create policy for v4
	if matchV4 != "" {
		dbIDs := getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, NoReRouteUDNPodToCDNSvc, IPFamilyValueV4, ni.GetNetworkName(), controller)
		lrp := nbdb.LogicalRouterPolicy{
			Priority:    types.DefaultNoRereoutePriority,
			Action:      nbdb.LogicalRouterPolicyActionAllow,
			Match:       matchV4,
			ExternalIDs: dbIDs.GetExternalIDs(),
		}
		p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient, ops, routerName, &lrp, p)
		if err != nil {
			return ops, err
		}
	}

	// Create policy for v6
	if matchV6 != "" {
		dbIDs := getEgressIPLRPNoReRouteDbIDs(types.DefaultNoRereoutePriority, NoReRouteUDNPodToCDNSvc, IPFamilyValueV6, ni.GetNetworkName(), controller)
		lrp := nbdb.LogicalRouterPolicy{
			Priority:    types.DefaultNoRereoutePriority,
			Action:      nbdb.LogicalRouterPolicyActionAllow,
			Match:       matchV6,
			ExternalIDs: dbIDs.GetExternalIDs(),
		}
		p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient, ops, routerName, &lrp, p)
		if err != nil {
			return ops, err
		}
	}

	return ops, nil
}

// ensureDefaultNoRerouteNodePoliciesOps builds ovsdb operations for node policies
// without committing. It follows the accumulator pattern: accepts an existing
// ops slice, appends new operations, and returns the extended slice for batching.
func ensureDefaultNoRerouteNodePoliciesOps(nbClient libovsdbclient.Client, addressSetFactory addressset.AddressSetFactory, ni util.NetInfo, router, controller string, v4, v6 bool, clusterNodesAddressSets addressset.AddressSet, ops []ovsdb.Operation) ([]ovsdb.Operation, error) {
	network := ni.GetNetworkName()
	ipv4ClusterNodeIPAS, ipv6ClusterNodeIPAS := clusterNodesAddressSets.GetASHashNames()

	var as addressset.AddressSet
	var err error
	// fetch the egressIP pods address-set
	dbIDs := getEgressIPAddrSetDbIDs(EgressIPServedPodsAddrSetName, network, controller)
	if as, err = addressSetFactory.EnsureAddressSet(dbIDs); err != nil {
		return ops, fmt.Errorf("cannot ensure that addressSet %s exists %v", EgressIPServedPodsAddrSetName, err)
	}
	ipv4EgressIPServedPodsAS, ipv6EgressIPServedPodsAS := as.GetASHashNames()

	// fetch the egressService pods address-set
	dbIDs = egresssvc.GetEgressServiceAddrSetDbIDs(controller)
	if as, err = addressSetFactory.EnsureAddressSet(dbIDs); err != nil {
		return ops, fmt.Errorf("cannot ensure that addressSet %s exists %v", egresssvc.EgressServiceServedPodsAddrSetName, err)
	}
	ipv4EgressServiceServedPodsAS, ipv6EgressServiceServedPodsAS := as.GetASHashNames()

	var matchV4, matchV6 string
	// construct the policy match
	if v4 {
		if ipv4EgressIPServedPodsAS == "" || ipv4EgressServiceServedPodsAS == "" || ipv4ClusterNodeIPAS == "" {
			return ops, types.NewSuppressedError(fmt.Errorf("address set hash name(s) not found %q %q %q", ipv4EgressIPServedPodsAS, ipv4EgressServiceServedPodsAS, ipv4ClusterNodeIPAS))
		}
		matchV4 = fmt.Sprintf(`(ip4.src == $%s || ip4.src == $%s) && ip4.dst == $%s`,
			ipv4EgressIPServedPodsAS, ipv4EgressServiceServedPodsAS, ipv4ClusterNodeIPAS)
	}
	if v6 {
		if ipv6EgressIPServedPodsAS == "" || ipv6EgressServiceServedPodsAS == "" || ipv6ClusterNodeIPAS == "" {
			return ops, types.NewSuppressedError(fmt.Errorf("address set hash name(s) not found"))
		}
		matchV6 = fmt.Sprintf(`(ip6.src == $%s || ip6.src == $%s) && ip6.dst == $%s`,
			ipv6EgressIPServedPodsAS, ipv6EgressServiceServedPodsAS, ipv6ClusterNodeIPAS)
	}

	if matchV4 != "" || matchV6 != "" {
		ops, err = libovsdbutil.EnsureDefaultEgressChainOps(nbClient, ops, router, ni, types.DefaultEgressNodeChain)
		if err != nil {
			return nil, err
		}
	}

	// Create global no-reroute policy for node traffic (batched)
	if matchV4 != "" {
		dbIDs := getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV4, network, controller)
		lrp := nbdb.LogicalRouterPolicy{
			Priority:    types.DefaultNoRereoutePriority,
			Action:      nbdb.LogicalRouterPolicyActionAllow,
			Match:       matchV4,
			Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
			ExternalIDs: dbIDs.GetExternalIDs(),
		}
		libovsdbutil.SetDefaultEgressNoReroutePolicy(&lrp, ni, types.DefaultEgressNodeChain)
		p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient, ops, router, &lrp, p, &lrp.Match, &lrp.Action, &lrp.JumpChain, &lrp.Options)
		if err != nil {
			return ops, err
		}
	}

	if matchV6 != "" {
		dbIDs := getEgressIPLRPNoReRoutePodToNodeDbIDs(IPFamilyValueV6, network, controller)
		lrp := nbdb.LogicalRouterPolicy{
			Priority:    types.DefaultNoRereoutePriority,
			Action:      nbdb.LogicalRouterPolicyActionAllow,
			Match:       matchV6,
			Options:     map[string]string{"pkt_mark": types.EgressIPNodeConnectionMark},
			ExternalIDs: dbIDs.GetExternalIDs(),
		}
		libovsdbutil.SetDefaultEgressNoReroutePolicy(&lrp, ni, types.DefaultEgressNodeChain)
		p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
		ops, err = libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicateOps(nbClient, ops, router, &lrp, p, &lrp.Match, &lrp.Action, &lrp.JumpChain, &lrp.Options)
		if err != nil {
			return ops, err
		}
	}

	return ops, nil
}
