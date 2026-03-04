package model

import (
	"testing"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

var mapping = map[string]string{
	egressFirewallOwnerType:             libovsdbops.EgressFirewallOwnerType,
	adminNetworkPolicyOwnerType:         libovsdbops.AdminNetworkPolicyOwnerType,
	baselineAdminNetworkPolicyOwnerType: libovsdbops.BaselineAdminNetworkPolicyOwnerType,
	networkPolicyOwnerType:              libovsdbops.NetworkPolicyOwnerType,
	multicastNamespaceOwnerType:         libovsdbops.MulticastNamespaceOwnerType,
	multicastClusterOwnerType:           libovsdbops.MulticastClusterOwnerType,
	netpolNodeOwnerType:                 libovsdbops.NetpolNodeOwnerType,
	netpolNamespaceOwnerType:            libovsdbops.NetpolNamespaceOwnerType,
	udnIsolationOwnerType:               libovsdbops.UDNIsolationOwnerType,
	aclActionAllow:                      nbdb.ACLActionAllow,
	aclActionAllowRelated:               nbdb.ACLActionAllowRelated,
	aclActionAllowStateless:             nbdb.ACLActionAllowStateless,
	aclActionDrop:                       nbdb.ACLActionDrop,
	aclActionPass:                       nbdb.ACLActionPass,
	aclActionReject:                     nbdb.ACLActionReject,
}

// Protects from potential future renaming in ovn/ovs constants, since all constants are duplicated here
func TestConstantsMatch(t *testing.T) {
	for k, v := range mapping {
		if k != v {
			t.Fatalf("Constant %s does not match %s", k, v)
		}
	}
}
