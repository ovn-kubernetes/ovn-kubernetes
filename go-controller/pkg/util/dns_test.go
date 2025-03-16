package util

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	mock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	util_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
)

func TestNewDNS(t *testing.T) {
	mockDNSOps := new(util_mocks.DNSOps)
	SetDNSLibOpsMockInst(mockDNSOps)
	tests := []struct {
		desc             string
		errExp           bool
		dnsOpsMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:   "fails to read config file ",
			errExp: true,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ClientConfigFromFile", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{nil, fmt.Errorf("mock error")}, CallTimes: 1},
			},
		},
		{
			desc:   "positive test case",
			errExp: false,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "ClientConfigFromFile", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{&dns.ClientConfig{}, nil}, CallTimes: 1},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			for _, item := range tc.dnsOpsMockHelper {
				call := mockDNSOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			res, err := NewDNS("DNS temp Name")
			t.Log(res, err)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			mockDNSOps.AssertExpectations(t)

		})
	}
}

func TestGetIPsAndMinTTL(t *testing.T) {
	mockDNSOps := new(util_mocks.DNSOps)
	SetDNSLibOpsMockInst(mockDNSOps)
	tests := []struct {
		desc             string
		errExp           bool
		ipv4Mode         bool
		ipv6Mode         bool
		dnsOpsMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:     "call to Exchange fails IPv4 only",
			errExp:   true,
			ipv4Mode: true,
			ipv6Mode: false,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"www.test.com"}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{nil, 0 * time.Second, fmt.Errorf("mock error")},
					CallTimes:           1,
				},
			},
		},
		{
			desc:     "Exchange returns correctly but Rcode != RcodeSuccess IPv4 only",
			errExp:   true,
			ipv4Mode: true,
			ipv6Mode: false,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"www.test.com"}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: 2}}, 0 * time.Second, nil},
					CallTimes:           1,
				},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			for _, item := range tc.dnsOpsMockHelper {
				call := mockDNSOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			//setup need to have a DNS object and
			testDNS := DNS{
				dnsMap:      make(map[string]dnsValue),
				nameservers: []string{"127.0.0.1"},
				port:        "1",
			}
			config.IPv4Mode = tc.ipv4Mode
			config.IPv6Mode = tc.ipv6Mode
			res, _, err := testDNS.getIPsAndMinTTL("www.test.com")
			t.Log(res, err)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			mockDNSOps.AssertExpectations(t)
		})
	}
}

func TestUpdate(t *testing.T) {
	mockDNSOps := new(util_mocks.DNSOps)
	SetDNSLibOpsMockInst(mockDNSOps)

	dnsName := "www.testing.com"
	newIP := net.ParseIP("1.2.3.4")

	tests := []struct {
		desc             string
		dnsMap           map[string]dnsValue
		dnsOpsMockHelper []ovntest.TestifyMockHelper
		errExp           bool
		changeExp        bool
	}{
		{
			desc:   "value not in DNS map",
			dnsMap: nil,
			errExp: true,
		},
		{
			desc: "error returned from getIPsAndMinTTL",
			dnsMap: map[string]dnsValue{dnsName: {
				ips: []net.IP{net.ParseIP("1.2.3.4")},
			},
			},
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{dnsName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{nil, 0 * time.Second, fmt.Errorf("mock error")},
					CallTimes:           1,
				},
			},
			errExp: true,
		},
		{
			desc: "Update Succeeds but the old and new IP sets are the same ",
			dnsMap: map[string]dnsValue{dnsName: {
				ips: []net.IP{newIP},
			},
			},
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{dnsName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}, Answer: []dns.RR{&dns.A{A: newIP}}}, 0 * time.Second, nil},
					CallTimes:           1,
				},
			},
			errExp:    false,
			changeExp: false,
		},
		{
			desc: "Update Succeeds and the IP address has changed ",
			dnsMap: map[string]dnsValue{dnsName: {
				ips: []net.IP{net.ParseIP("1.1.1.1")},
			},
			},
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{dnsName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}, Answer: []dns.RR{&dns.A{A: newIP}}}, 0 * time.Second, nil},
					CallTimes:           1,
				},
			},
			errExp:    false,
			changeExp: true,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			for _, item := range tc.dnsOpsMockHelper {
				call := mockDNSOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			dns := DNS{
				dnsMap:      tc.dnsMap,
				nameservers: []string{"1.1.1.1"},
				port:        "1234",
			}
			returned, err := dns.Update(dnsName)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.changeExp, returned, "the change expected varaible should match the return from dns.Update()")

				assert.Len(t, dns.dnsMap[dnsName].ips, 1)
				assert.Equal(t, dns.dnsMap[dnsName].ips[0], newIP)

			}

		})
	}

}

func TestAdd(t *testing.T) {
	dnsName := "www.testing.com"
	mockDNSOps := new(util_mocks.DNSOps)
	SetDNSLibOpsMockInst(mockDNSOps)
	addedIP := net.ParseIP("2.3.4.5")

	tests := []struct {
		desc             string
		errExp           bool
		dnsOpsMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:   "Add fails",
			errExp: true,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{dnsName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}, Answer: []dns.RR{}}, 0 * time.Second, nil},
					CallTimes:           1,
				},
			},
		},
		{
			desc:   "Add succeeds ",
			errExp: false,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{dnsName}, CallTimes: 1},
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{
					OnCallMethodName:    "Exchange",
					OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"},
					RetArgList:          []interface{}{&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}, Answer: []dns.RR{&dns.A{A: addedIP}}}, 0 * time.Second, nil},
					CallTimes:           1,
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			for _, item := range tc.dnsOpsMockHelper {
				call := mockDNSOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			dns := DNS{
				dnsMap:      make(map[string]dnsValue),
				nameservers: []string{"1.1.1.1"},
			}
			err := dns.Add(dnsName)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Len(t, dns.dnsMap[dnsName].ips, 1)
				assert.Equal(t, dns.dnsMap[dnsName].ips[0], addedIP)
			}
		})
	}

}
