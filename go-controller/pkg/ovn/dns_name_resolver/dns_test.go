// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dnsnameresolver

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	mock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set/mocks"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	util_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
)

const DefaultNetworkControllerName = "default-network-controller"

func TestNewEgressDNS(t *testing.T) {
	testCh := make(chan struct{})
	dbSetup := libovsdbtest.TestSetup{}

	libovsdbOvnNBClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
	require.NoError(t, err)
	t.Cleanup(libovsdbCleanup.Cleanup)

	testOvnAddFtry := addressset.NewOvnAddressSetFactory(libovsdbOvnNBClient, config.IPv4Mode, config.IPv6Mode)
	mockDnsOps := new(util_mocks.DNSOps)
	util.SetDNSLibOpsMockInst(mockDnsOps)
	tests := []struct {
		desc             string
		errExp           bool
		dnsOpsMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:   "fails to read the /etc/resolv.conf file",
			errExp: true,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ClientConfigFromFile", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}, CallTimes: 1},
			},
		},
		{
			desc: "positive tests case",
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ClientConfigFromFile", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&dns.ClientConfig{}, nil}, CallTimes: 1},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			for _, item := range tc.dnsOpsMockHelper {
				call := mockDnsOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			_, err := NewEgressDNS(testOvnAddFtry, DefaultNetworkControllerName, testCh, 0)
			//t.Log(res, err)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			mockDnsOps.AssertExpectations(t)
		})
	}
}

func generateRR(dnsName, ip, nextQueryTime string) dns.RR {
	var rr dns.RR
	if utilnet.IsIPv6(net.ParseIP(ip)) {
		rr, _ = dns.NewRR(dnsName + ".        " + nextQueryTime + "     IN      AAAA       " + ip)
	} else {
		rr, _ = dns.NewRR(dnsName + ".        " + nextQueryTime + "     IN      A       " + ip)
	}
	return rr
}

// TestUpdateAddressSetRecreatesMissingAddressSet verifies replacement of a stale address set handle.
func TestUpdateAddressSetRecreatesMissingAddressSet(t *testing.T) {
	const dnsName = "www.test.com"
	addresses := []string{"192.0.2.10"}
	staleAddressSet := new(mocks.AddressSet)
	recreatedAddressSet := new(mocks.AddressSet)
	factory := new(mocks.AddressSetFactory)
	expectedDBIDs := GetEgressFirewallDNSAddrSetDbIDs(dnsName, DefaultNetworkControllerName)

	staleAddressSet.On("SetAddresses", addresses).
		Return(fmt.Errorf("address set was removed: %w", libovsdbclient.ErrNotFound)).Once()
	factory.On("EnsureAddressSet", mock.MatchedBy(func(dbIDs *libovsdbops.DbObjectIDs) bool {
		return dbIDs.String() == expectedDBIDs.String()
	})).Return(recreatedAddressSet, nil).Once()
	recreatedAddressSet.On("SetAddresses", addresses).Return(nil).Once()

	resolver := &EgressDNS{
		dnsEntries: map[string]*dnsEntry{
			dnsName: {
				dnsAddressSet: staleAddressSet,
			},
		},
		addressSetFactory: factory,
		controllerName:    DefaultNetworkControllerName,
	}

	require.NoError(t, resolver.updateAddressSet(dnsName, addresses),
		"failed to recreate address set for DNS name %s", dnsName)
	assert.Same(t, recreatedAddressSet, resolver.dnsEntries[dnsName].dnsAddressSet,
		"recreated address set was not stored for DNS name %s", dnsName)

	staleAddressSet.AssertExpectations(t)
	recreatedAddressSet.AssertExpectations(t)
	factory.AssertExpectations(t)
}

// TestUpdateAddressSetDoesNotRecreateOnUnexpectedError verifies that only a
// missing address set triggers recreation.
func TestUpdateAddressSetDoesNotRecreateOnUnexpectedError(t *testing.T) {
	const dnsName = "www.test.com"
	addresses := []string{"192.0.2.10"}
	staleAddressSet := new(mocks.AddressSet)
	factory := new(mocks.AddressSetFactory)

	staleAddressSet.On("SetAddresses", addresses).Return(fmt.Errorf("update failed")).Once()

	resolver := &EgressDNS{
		dnsEntries: map[string]*dnsEntry{
			dnsName: {
				dnsAddressSet: staleAddressSet,
			},
		},
		addressSetFactory: factory,
		controllerName:    DefaultNetworkControllerName,
	}

	require.Error(t, resolver.updateAddressSet(dnsName, addresses),
		"unexpected address-set update errors must be returned")
	factory.AssertNotCalled(t, "EnsureAddressSet", mock.Anything)
	staleAddressSet.AssertExpectations(t)
}

// TestACLMatchReferencesAddressSet verifies address-set references are matched
// as complete ACL tokens rather than arbitrary substrings.
func TestACLMatchReferencesAddressSet(t *testing.T) {
	tests := []struct {
		name          string
		match         string
		addressSet    string
		expectedMatch bool
	}{
		{
			name:          "exact reference",
			match:         "ip4.dst == $a123",
			addressSet:    "a123",
			expectedMatch: true,
		},
		{
			name:          "reference in set",
			match:         "ip4.dst == {$a123, $a456}",
			addressSet:    "a123",
			expectedMatch: true,
		},
		{
			name:          "hash prefix is not a reference",
			match:         "ip4.dst == $a1234",
			addressSet:    "a123",
			expectedMatch: false,
		},
		{
			name:          "hash embedded in another token is not a reference",
			match:         "ip4.dst == $xa123",
			addressSet:    "a123",
			expectedMatch: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expectedMatch, aclMatchReferencesAddressSet(tc.match, tc.addressSet),
				"ACL match %q must identify address set %q correctly", tc.match, tc.addressSet)
		})
	}
}

// TestUpdateEntryForNameRecreatesMissingAddressSet verifies the recovery path against a real
// libovsdb client for IPv4-only, IPv6-only, and dual-stack address sets.
func TestUpdateEntryForNameRecreatesMissingAddressSet(t *testing.T) {
	const dnsName = "www.test.com"
	tests := []struct {
		name              string
		ipv4Mode          bool
		ipv6Mode          bool
		ipv4Address       string
		ipv6Address       string
		deleteIPv6Address bool
	}{
		{
			name:        "IPv4-only address set",
			ipv4Mode:    true,
			ipv4Address: "192.0.2.10",
		},
		{
			name:              "IPv6-only address set",
			ipv6Mode:          true,
			ipv6Address:       "2001:db8::10",
			deleteIPv6Address: true,
		},
		{
			name:              "dual-stack address set with missing IPv6 row",
			ipv4Mode:          true,
			ipv6Mode:          true,
			ipv4Address:       "192.0.2.10",
			ipv6Address:       "2001:db8::10",
			deleteIPv6Address: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldDNSOps := util.GetDNSLibOps()
			require.NoError(t, config.PrepareTestConfig())
			config.IPv4Mode = tc.ipv4Mode
			config.IPv6Mode = tc.ipv6Mode
			dnsServer := "192.0.2.53"
			dnsServerAddress := "192.0.2.53:53"
			if tc.ipv6Mode && !tc.ipv4Mode {
				dnsServer = "2001:db8::53"
				dnsServerAddress = "[2001:db8::53]:53"
			}
			t.Cleanup(func() {
				util.SetDNSLibOpsMockInst(oldDNSOps)
				if err := config.PrepareTestConfig(); err != nil {
					t.Errorf("failed to restore test config: %v", err)
				}
			})

			mockDnsOps := new(util_mocks.DNSOps)
			util.SetDNSLibOpsMockInst(mockDnsOps)
			mockDnsOps.On("ClientConfigFromFile", mock.AnythingOfType("string")).
				Return(&dns.ClientConfig{Servers: []string{dnsServer}, Port: "53"}, nil).Once()
			if tc.ipv4Address != "" {
				mockDnsOps.On("Fqdn", dnsName).Return(dnsName + ".").Once()
				mockDnsOps.On("SetQuestion", mock.AnythingOfType("*dns.Msg"), dnsName+".", uint16(dns.TypeA)).
					Return(&dns.Msg{}).Once()
				mockDnsOps.On("Exchange", mock.AnythingOfType("*dns.Client"), mock.AnythingOfType("*dns.Msg"), dnsServerAddress).
					Return(&dns.Msg{Answer: []dns.RR{generateRR(dnsName, tc.ipv4Address, "30")}}, time.Second, nil).Once()
			}
			if tc.ipv6Address != "" {
				mockDnsOps.On("Fqdn", dnsName).Return(dnsName + ".").Once()
				mockDnsOps.On("SetQuestion", mock.AnythingOfType("*dns.Msg"), dnsName+".", uint16(dns.TypeAAAA)).
					Return(&dns.Msg{}).Once()
				mockDnsOps.On("Exchange", mock.AnythingOfType("*dns.Client"), mock.AnythingOfType("*dns.Msg"), dnsServerAddress).
					Return(&dns.Msg{Answer: []dns.RR{generateRR(dnsName, tc.ipv6Address, "30")}}, time.Second, nil).Once()
			}

			dnsInfo, err := util.NewDNS("/etc/resolv.conf")
			require.NoError(t, err, "failed to create DNS resolver for %s", tc.name)
			require.NoError(t, dnsInfo.Add(dnsName), "failed to resolve DNS name for %s", tc.name)
			mockDnsOps.AssertExpectations(t)

			nbClient, _, cleanup, err := libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{})
			require.NoError(t, err, "failed to create libovsdb test harness for %s", tc.name)
			t.Cleanup(cleanup.Cleanup)

			factory := addressset.NewOvnAddressSetFactory(nbClient, tc.ipv4Mode, tc.ipv6Mode)
			dbIDs := GetEgressFirewallDNSAddrSetDbIDs(dnsName, DefaultNetworkControllerName)
			initialAddresses := []string{}
			if tc.ipv4Address != "" {
				initialAddresses = append(initialAddresses, tc.ipv4Address)
			}
			if tc.ipv6Address != "" {
				initialAddresses = append(initialAddresses, tc.ipv6Address)
			}
			staleAddressSet, err := factory.NewAddressSet(dbIDs, initialAddresses)
			require.NoError(t, err, "failed to create initial address set for %s", tc.name)

			if tc.deleteIPv6Address {
				_, ipv6HashName := addressset.GetHashNamesForAS(dbIDs)
				require.NoError(t, libovsdbops.DeleteAddressSets(nbClient, &nbdb.AddressSet{Name: ipv6HashName}),
					"failed to delete IPv6 address set for %s", tc.name)
			} else {
				require.NoError(t, staleAddressSet.Destroy(), "failed to delete address set for %s", tc.name)
			}

			resolver := &EgressDNS{
				dns:               dnsInfo,
				dnsEntries:        map[string]*dnsEntry{dnsName: {dnsAddressSet: staleAddressSet}},
				addressSetFactory: factory,
				controllerName:    DefaultNetworkControllerName,
			}
			require.NoError(t, resolver.updateEntryForName(dnsName),
				"failed to update DNS entry for %s", tc.name)

			recreatedAddressSet, err := factory.GetAddressSet(dbIDs)
			require.NoError(t, err, "failed to retrieve recovered address set for %s", tc.name)
			addresses, ipv6Addresses := recreatedAddressSet.GetAddresses()
			expectedIPv4 := []string{}
			if tc.ipv4Address != "" {
				expectedIPv4 = append(expectedIPv4, tc.ipv4Address)
			}
			expectedIPv6 := []string{}
			if tc.ipv6Address != "" {
				expectedIPv6 = append(expectedIPv6, net.ParseIP(tc.ipv6Address).String())
			}
			assert.ElementsMatch(t, expectedIPv4, addresses,
				"recovered IPv4 addresses for %s", tc.name)
			assert.ElementsMatch(t, expectedIPv6, ipv6Addresses,
				"recovered IPv6 addresses for %s", tc.name)
		})
	}
}

// TestDeleteStaleAddrSetsKeepsReferencedMissingEntries verifies missing rows
// remain recoverable while unreferenced resolver entries are forgotten.
func TestDeleteStaleAddrSetsKeepsReferencedMissingEntries(t *testing.T) {
	staleDNSName := "stale.test.com"
	activeDNSName := "active.test.com"
	staleDBIDs := GetEgressFirewallDNSAddrSetDbIDs(staleDNSName, DefaultNetworkControllerName)
	activeDBIDs := GetEgressFirewallDNSAddrSetDbIDs(activeDNSName, DefaultNetworkControllerName)
	staleDBAddressSet, _ := addressset.GetTestDbAddrSets(staleDBIDs, nil)
	activeDBAddressSet, _ := addressset.GetTestDbAddrSets(activeDBIDs, nil)
	activeV4HashName := activeDBAddressSet.Name
	aclDBIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: "test",
			libovsdbops.RuleIndex:     "0",
		})
	acl := libovsdbutil.BuildACLWithDefaultTier(aclDBIDs, 1000, activeV4HashName, nbdb.ACLActionAllow,
		nil, libovsdbutil.LportIngress)
	acl.UUID = "uactive-acl"
	portGroup := &nbdb.PortGroup{
		UUID: "uactive-port-group",
		ACLs: []string{acl.UUID},
	}
	nbClient, _, cleanup, err := libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{staleDBAddressSet, activeDBAddressSet, acl, portGroup},
	})
	require.NoError(t, err)
	t.Cleanup(cleanup.Cleanup)

	factory := addressset.NewOvnAddressSetFactory(nbClient, true, false)
	staleAddressSet, err := factory.GetAddressSet(staleDBIDs)
	require.NoError(t, err)
	activeAddressSet, err := factory.GetAddressSet(activeDBIDs)
	require.NoError(t, err)
	require.NoError(t, activeAddressSet.Destroy())

	resolver := &EgressDNS{
		dns: &util.DNS{},
		dnsEntries: map[string]*dnsEntry{
			staleDNSName: {
				namespaces:    map[string]struct{}{"namespace": {}},
				dnsAddressSet: staleAddressSet,
			},
			activeDNSName: {
				namespaces:    map[string]struct{}{"namespace": {}},
				dnsAddressSet: activeAddressSet,
			},
		},
		addressSetFactory: factory,
		controllerName:    DefaultNetworkControllerName,
		deleted:           make(chan string, 1),
	}

	require.NoError(t, resolver.DeleteStaleAddrSets(nbClient))
	_, staleEntryExists := resolver.dnsEntries[staleDNSName]
	_, activeEntryExists := resolver.dnsEntries[activeDNSName]
	assert.False(t, staleEntryExists, "unreferenced DNS entry should be removed")
	assert.True(t, activeEntryExists, "referenced missing DNS entry must be retained for recovery")

	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressFirewallDNS, DefaultNetworkControllerName, nil)
	addressSets, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil))
	require.NoError(t, err)
	assert.Empty(t, addressSets, "unreferenced address set should be deleted")
}

func TestAdd(t *testing.T) {
	mockAddressSetFactoryOps := new(mocks.AddressSetFactory)
	mockAddressSetOps := new(mocks.AddressSet)
	mockDnsOps := new(util_mocks.DNSOps)
	util.SetDNSLibOpsMockInst(mockDnsOps)
	test1DNSName := "www.test.com"
	test1IPv4 := "2.2.2.2"
	test1IPv4Update := "3.3.3.3"
	test1IPv6 := "2001:0db8:85a3:0000:0000:8a2e:0370:7334"
	clusterSubnetStr := "10.128.0.0/14"
	_, clusterSubnet, _ := net.ParseCIDR(clusterSubnetStr)
	clusterSubnetIP := "10.128.0.1"
	tests := []struct {
		desc                       string
		errExp                     bool
		dnsName                    string
		configIPv4                 bool
		configIPv6                 bool
		testingUpdateOnQueryTime   bool
		syncTime                   time.Duration
		waitForSyncLoop            bool
		dnsOpsMockHelper           []ovntest.TestifyMockHelper
		addressSetFactoryOpsHelper []ovntest.TestifyMockHelper
		addressSetOpsHelper        []ovntest.TestifyMockHelper
	}{
		{
			desc:     "NewAddressSet returns error",
			errExp:   true,
			syncTime: 5 * time.Minute,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "ClientConfigFromFile",
					OnCallMethodArgType: []string{"string"},
					RetArgList:          []interface{}{&dns.ClientConfig{Servers: []string{"1.1.1.1"}, Port: "1234"}, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "NewAddressSet",
					OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"},
					RetArgList:          []interface{}{nil, fmt.Errorf("mock error")},
					CallTimes:           1,
				},
			},
		},
		{
			desc:       "EgressFirewall Add(dnsName) succeeds IPv4 only",
			errExp:     false,
			syncTime:   5 * time.Minute,
			dnsName:    test1DNSName,
			configIPv4: true,
			configIPv6: false,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "ClientConfigFromFile", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&dns.ClientConfig{
						Servers: []string{"1.1.1.1"},
						Port:    "1234"}, nil}, CallTimes: 1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NewAddressSet", OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{mockAddressSetOps, nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
		{
			desc:       "EgressFirewall Add(dnsName) ignores ips from clusterSubnet",
			errExp:     false,
			syncTime:   5 * time.Minute,
			dnsName:    test1DNSName,
			configIPv4: true,
			configIPv6: false,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "ClientConfigFromFile",
					OnCallMethodArgType: []string{"string"},
					RetArgList:          []interface{}{&dns.ClientConfig{Servers: []string{"1.1.1.1"}, Port: "1234"}, nil},
					CallTimes:           1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, clusterSubnetIP, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NewAddressSet", OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{mockAddressSetOps, nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
		{
			desc:       "EgressFirewall Add(dnsName) ignores ips from clusterSubnet leaving other ips",
			errExp:     false,
			syncTime:   5 * time.Minute,
			dnsName:    test1DNSName,
			configIPv4: true,
			configIPv6: false,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "ClientConfigFromFile",
					OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&dns.ClientConfig{Servers: []string{"1.1.1.1"}, Port: "1234"}, nil},
					CallTimes: 1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "300"), generateRR(test1DNSName, clusterSubnetIP, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NewAddressSet", OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"}, RetArgList: []interface{}{mockAddressSetOps, nil}, CallTimes: 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
		{
			desc:                     "EgressFirewall Add(dnsName) succeeds dual stack",
			errExp:                   false,
			syncTime:                 5 * time.Minute,
			dnsName:                  test1DNSName,
			testingUpdateOnQueryTime: false,
			configIPv4:               true,
			configIPv6:               true,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "ClientConfigFromFile",
					OnCallMethodArgType: []string{"string"},
					RetArgList:          []interface{}{&dns.ClientConfig{Servers: []string{"1.1.1.1"}, Port: "1234"}, nil},
					CallTimes:           1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv6, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NewAddressSet", OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{mockAddressSetOps, nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4, net.ParseIP(test1IPv6).String()}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
		{
			desc:                     "EgressFirewall DNS Run Runs update after the ttl returned from the DNS server expires",
			errExp:                   false,
			dnsName:                  test1DNSName,
			testingUpdateOnQueryTime: true,
			syncTime:                 5 * time.Minute,
			configIPv4:               true,
			configIPv6:               false,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{

				{OnCallMethodName: "ClientConfigFromFile",
					OnCallMethodArgType: []string{"string"},
					RetArgList:          []interface{}{&dns.ClientConfig{Servers: []string{"1.1.1.1"}, Port: "1234"}, nil},
					CallTimes:           1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},

				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				// return a very low ttl so that the update based on ttl timeout occurs
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "4")}}, 1 * time.Second, nil},
					CallTimes:           1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4Update, "300")}}, 1 * time.Second, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NewAddressSet", OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{mockAddressSetOps, nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4}},
					RetArgList:       []interface{}{nil},
				},
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4Update}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			testCh := make(chan struct{})
			config.IPv4Mode = tc.configIPv4
			config.IPv6Mode = tc.configIPv6
			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: clusterSubnet}}

			for _, item := range tc.dnsOpsMockHelper {
				call := mockDnsOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			for _, item := range tc.addressSetFactoryOpsHelper {
				call := mockAddressSetFactoryOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			for _, item := range tc.addressSetOpsHelper {
				call := mockAddressSetOps.On(item.OnCallMethodName)
				// use exact arguments for AddressSet call to match ips
				call.Arguments = item.OnCallMethodArgs
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			res, err := NewEgressDNS(mockAddressSetFactoryOps, DefaultNetworkControllerName, testCh, tc.syncTime)
			require.NoError(t, err)

			err = res.Run()
			require.NoError(t, err)

			_, err = res.Add("addNamespace", test1DNSName)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				for stay, timeout := true, time.After(10*time.Second); stay; {
					_, dnsResolves, _ := res.getDNSEntry(tc.dnsName)
					if dnsResolves != nil {
						break
					}
					select {
					case <-timeout:
						stay = false
						t.Errorf("timeout: it is taking too long for the goroutine to complete")
					default:
					}

				}
			}

			if tc.testingUpdateOnQueryTime {
				for stay, timeout := true, time.After(15*time.Second); stay; {
					_, dnsResolves, _ := res.getDNSEntry(tc.dnsName)
					if dnsResolves != nil {
						if len(dnsResolves) == 1 && dnsResolves[0].String() == test1IPv4Update {
							break
						}
					}
					select {
					case <-timeout:
						stay = false
						t.Errorf("timeout waiting for update based on ttl to fire or process")
					default:
					}

				}
			}

			close(testCh)
			mockDnsOps.AssertExpectations(t)
			mockAddressSetFactoryOps.AssertExpectations(t)
			mockAddressSetOps.AssertExpectations(t)

			mockDnsOps.ExpectedCalls = nil
			mockAddressSetFactoryOps.ExpectedCalls = nil
			mockAddressSetOps.ExpectedCalls = nil
		})
	}
}

func TestDelete(t *testing.T) {
	mockAddressSetFactoryOps := new(mocks.AddressSetFactory)
	mockAddressSetOps := new(mocks.AddressSet)
	mockDnsOps := new(util_mocks.DNSOps)
	util.SetDNSLibOpsMockInst(mockDnsOps)
	test1DNSName := "www.test.com"
	test1IPv4 := "2.2.2.2"
	test1IPv6 := "2001:0db8:85a3:0000:0000:8a2e:0370:7334"
	tests := []struct {
		desc                       string
		errExp                     bool
		dnsName                    string
		configIPv4                 bool
		configIPv6                 bool
		testingUpdateOnQueryTime   bool
		syncTime                   time.Duration
		waitForSyncLoop            bool
		dnsOpsMockHelper           []ovntest.TestifyMockHelper
		addressSetFactoryOpsHelper []ovntest.TestifyMockHelper
		addressSetOpsHelper        []ovntest.TestifyMockHelper
	}{
		{
			desc:                     "EgressFirewall Delete functions",
			errExp:                   false,
			syncTime:                 5 * time.Minute,
			dnsName:                  test1DNSName,
			testingUpdateOnQueryTime: false,
			configIPv4:               true,
			configIPv6:               true,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:    "ClientConfigFromFile",
					OnCallMethodArgType: []string{"string"},
					RetArgList:          []interface{}{&dns.ClientConfig{Servers: []string{"1.1.1.1"}, Port: "1234"}, nil},
					CallTimes:           1,
				},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{test1DNSName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv6, "300")}}, 500 * time.Second, nil},
					CallTimes:           1,
				},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "NewAddressSet", OnCallMethodArgType: []string{"*ops.DbObjectIDs", "[]string"}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{mockAddressSetOps, nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "SetAddresses", OnCallMethodArgType: []string{"[]string"}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
				{OnCallMethodName: "Destroy", OnCallMethodArgType: []string{}, OnCallMethodArgs: []interface{}{}, RetArgList: []interface{}{nil}, OnCallMethodsArgsStrTypeAppendCount: 0, CallTimes: 1},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			testCh := make(chan struct{})
			config.IPv4Mode = tc.configIPv4
			config.IPv6Mode = tc.configIPv6

			for _, item := range tc.dnsOpsMockHelper {
				call := mockDnsOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			for _, item := range tc.addressSetFactoryOpsHelper {
				call := mockAddressSetFactoryOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			for _, item := range tc.addressSetOpsHelper {
				call := mockAddressSetOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			res, err := NewEgressDNS(mockAddressSetFactoryOps, DefaultNetworkControllerName, testCh, tc.syncTime)
			require.NoError(t, err)

			err = res.Run()
			require.NoError(t, err)

			_, err = res.Add("addNamespace", test1DNSName)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				for stay, timeout := true, time.After(10*time.Second); stay; {
					_, dnsResolves, _ := res.getDNSEntry(tc.dnsName)
					if dnsResolves != nil {
						break
					}
					select {
					case <-timeout:
						stay = false
						t.Errorf("timeout: it is taking too long for the goroutine to complete")
					default:
					}

				}
			}
			_, dnsResolves, _ := res.getDNSEntry(tc.dnsName)
			err = res.Delete("addNamespace")
			require.NoError(t, err)
			for stay, timeout := true, time.After(10*time.Second); stay; {
				_, dnsResolves, _ = res.getDNSEntry(tc.dnsName)
				if dnsResolves == nil {
					break
				}
				select {
				case <-timeout:
					stay = false
					t.Errorf("timeout: dns is taking to long for the goroutine to update the dns object")
				default:
				}
			}

			assert.Nil(t, dnsResolves)

			close(testCh)
			mockDnsOps.AssertExpectations(t)
			mockAddressSetFactoryOps.AssertExpectations(t)
			mockAddressSetOps.AssertExpectations(t)

			mockDnsOps.ExpectedCalls = nil
			mockAddressSetFactoryOps.ExpectedCalls = nil
			mockAddressSetOps.ExpectedCalls = nil
		})
	}
}

func (e *EgressDNS) getDNSEntry(dnsName string) (map[string]struct{}, []net.IP, addressset.AddressSet) {
	e.lock.Lock()
	defer e.lock.Unlock()
	if dnsEntry, exists := e.dnsEntries[dnsName]; exists {
		return dnsEntry.namespaces, dnsEntry.dnsResolves, dnsEntry.dnsAddressSet
	}

	return nil, nil, nil
}
