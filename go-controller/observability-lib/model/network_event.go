package model

import (
	"fmt"
)

const (
	// Constants are duplicated to minimize dependencies
	// When adding constants here, add them in network_event_test.go too

	// libovsdb constants: see also github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops
	egressFirewallOwnerType             = "EgressFirewall"
	adminNetworkPolicyOwnerType         = "AdminNetworkPolicy"
	baselineAdminNetworkPolicyOwnerType = "BaselineAdminNetworkPolicy"
	networkPolicyOwnerType              = "NetworkPolicy"
	multicastNamespaceOwnerType         = "MulticastNS"
	multicastClusterOwnerType           = "MulticastCluster"
	netpolNodeOwnerType                 = "NetpolNode"
	netpolNamespaceOwnerType            = "NetpolNamespace"
	udnIsolationOwnerType               = "UDNIsolation"

	// nbdb constants: see also github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb
	aclActionAllow          = "allow"
	aclActionAllowRelated   = "allow-related"
	aclActionAllowStateless = "allow-stateless"
	aclActionDrop           = "drop"
	aclActionReject         = "reject"
	aclActionPass           = "pass"
)

type NetworkEvent interface {
	String() string
}

type ACLEvent struct {
	NetworkEvent
	Action    string
	Actor     string
	Name      string
	Namespace string
	Direction string
}

func (e *ACLEvent) String() string {
	var action string
	switch e.Action {
	case aclActionAllow, aclActionAllowRelated, aclActionAllowStateless:
		action = "Allowed"
	case aclActionDrop:
		action = "Dropped"
	case aclActionPass:
		action = "Delegated to network policy"
	default:
		action = "Action " + e.Action
	}
	var msg string
	switch e.Actor {
	case adminNetworkPolicyOwnerType:
		msg = fmt.Sprintf("admin network policy %s, direction %s", e.Name, e.Direction)
	case baselineAdminNetworkPolicyOwnerType:
		msg = fmt.Sprintf("baseline admin network policy %s, direction %s", e.Name, e.Direction)
	case multicastNamespaceOwnerType:
		msg = fmt.Sprintf("multicast in namespace %s, direction %s", e.Namespace, e.Direction)
	case multicastClusterOwnerType:
		msg = fmt.Sprintf("cluster multicast policy, direction %s", e.Direction)
	case netpolNodeOwnerType:
		msg = fmt.Sprintf("default allow from local node policy, direction %s", e.Direction)
	case networkPolicyOwnerType:
		if e.Namespace != "" {
			msg = fmt.Sprintf("network policy %s in namespace %s, direction %s", e.Name, e.Namespace, e.Direction)
		} else {
			msg = fmt.Sprintf("network policy %s, direction %s", e.Name, e.Direction)
		}
	case netpolNamespaceOwnerType:
		msg = fmt.Sprintf("network policies isolation in namespace %s, direction %s", e.Namespace, e.Direction)
	case egressFirewallOwnerType:
		msg = fmt.Sprintf("egress firewall in namespace %s", e.Namespace)
	case udnIsolationOwnerType:
		msg = fmt.Sprintf("UDN isolation of type %s", e.Name)
	}
	return fmt.Sprintf("%s by %s", action, msg)
}
