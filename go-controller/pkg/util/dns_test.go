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
		retry            bool
		ipv4Mode         bool
		ipv6Mode         bool
		dnsOpsMockHelper []ovntest.TestifyMockHelper
		expectedTTL      time.Duration
	}{
		{
			desc:     "call to Exchange fails IPv4 only",
			errExp:   true,
			retry:    true,
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
			expectedTTL: defaultMinTTL,
		},
		{
			desc:     "Exchange returns correctly but Rcode != RcodeSuccess IPv4 only",
			errExp:   true,
			retry:    true,
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
			expectedTTL: defaultMinTTL,
		},
		{
			desc:     "Exchange returns correctly but with TTL 0 IPv4 only",
			errExp:   false,
			retry:    false,
			ipv4Mode: true,
			ipv6Mode: false,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"www.test.com"}, CallTimes: 1},
				{OnCallMethodName: "Exchange", OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"}, RetArgList: []interface{}{&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}, Answer: []dns.RR{&dns.A{A: net.ParseIP("1.2.3.4")}}}, 0 * time.Second, nil}, CallTimes: 1},
			},
			expectedTTL: defaultMinTTL,
		},
		{
			desc:     "Exchange returns correctly but no Answer IPv4 only",
			errExp:   true,
			retry:    true,
			ipv4Mode: true,
			ipv6Mode: false,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"www.test.com"}, CallTimes: 1},
				{OnCallMethodName: "Exchange", OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"}, RetArgList: []interface{}{&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}, Answer: []dns.RR{}}, 0 * time.Second, nil}, CallTimes: 1},
			},
			expectedTTL: defaultMinTTL,
		},
		{
			desc:     "Exchange returns correctly but with non-zero TTL IPv4 only",
			errExp:   false,
			retry:    false,
			ipv4Mode: true,
			ipv6Mode: false,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1},
				{OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"www.test.com"}, CallTimes: 1},
				{OnCallMethodName: "Exchange", OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"}, RetArgList: []interface{}{&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}, Answer: []dns.RR{&dns.A{Hdr: dns.RR_Header{Ttl: 100}, A: net.ParseIP("1.2.3.4")}}}, 0 * time.Second, nil}, CallTimes: 1},
			},
			expectedTTL: 100 * time.Second,
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
			res, ttl, retry, err := testDNS.getIPsAndMinTTL("www.test.com")
			t.Log(res, ttl, retry, err)
			if tc.errExp {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.retry, retry, "the exponentialBackoff variable should match the return from dns.getIPsAndMinTTL()")
			assert.Equal(t, tc.expectedTTL, ttl, "the ttl variable should match the return from dns.getIPsAndMinTTL()")
			mockDNSOps.AssertExpectations(t)
		})
	}
}

func TestUpdate(t *testing.T) {
	config.IPv4Mode = true
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
	config.IPv4Mode = true
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

func TestIPsEqual(t *testing.T) {
	tests := []struct {
		desc     string
		oldips   []net.IP
		newips   []net.IP
		expEqual bool
	}{
		{
			desc:     "oldips and newips are the same",
			oldips:   []net.IP{net.ParseIP("1.2.3.4")},
			newips:   []net.IP{net.ParseIP("1.2.3.4")},
			expEqual: true,
		},
		{
			desc:     "oldips and newips are different",
			oldips:   []net.IP{net.ParseIP("1.2.3.4")},
			newips:   []net.IP{net.ParseIP("1.2.3.5")},
			expEqual: false,
		},
		{
			desc:     "oldips and newips are different length",
			oldips:   []net.IP{net.ParseIP("1.2.3.4")},
			newips:   []net.IP{net.ParseIP("1.2.3.4"), net.ParseIP("1.2.3.5")},
			expEqual: false,
		},
		{
			desc:     "oldips is nil and newips is not nil",
			oldips:   nil,
			newips:   []net.IP{net.ParseIP("1.2.3.4"), net.ParseIP("1.2.3.5")},
			expEqual: false,
		},
		{
			desc:     "oldips is empty and newips is not empty",
			oldips:   []net.IP{},
			newips:   []net.IP{net.ParseIP("1.2.3.4"), net.ParseIP("1.2.3.5")},
			expEqual: false,
		},
		{
			desc:     "oldips is not nil and newips is nil",
			oldips:   []net.IP{net.ParseIP("1.2.3.4"), net.ParseIP("1.2.3.5")},
			newips:   nil,
			expEqual: false,
		},
		{
			desc:     "oldips is not empty and newips is empty",
			oldips:   []net.IP{net.ParseIP("1.2.3.4"), net.ParseIP("1.2.3.5")},
			newips:   []net.IP{},
			expEqual: false,
		},
		{
			desc:     "oldips and newips are both nil",
			oldips:   nil,
			newips:   nil,
			expEqual: true,
		},
		{
			desc:     "oldips and newips are both empty",
			oldips:   []net.IP{},
			newips:   []net.IP{},
			expEqual: true,
		},
		{
			desc:     "oldips is nil and newips is empty",
			oldips:   nil,
			newips:   []net.IP{},
			expEqual: true,
		},
		{
			desc:     "oldips is empty and newips is nil",
			oldips:   []net.IP{},
			newips:   nil,
			expEqual: true,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res := ipsEqual(tc.oldips, tc.newips)
			assert.Equal(t, tc.expEqual, res)
		})
	}
}

func TestUpdateOne(t *testing.T) {
	config.IPv4Mode = true
	dnsName := "www.testing.com"
	newIP := net.ParseIP("1.2.3.4")
	fqdnOpsMockHelper := ovntest.TestifyMockHelper{
		OnCallMethodName: "Fqdn", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{dnsName}, CallTimes: 1,
	}
	setQuestionOpsMockHelper := ovntest.TestifyMockHelper{
		OnCallMethodName: "SetQuestion", OnCallMethodArgType: []string{"*dns.Msg", "string", "uint16"}, RetArgList: []interface{}{&dns.Msg{}}, CallTimes: 1,
	}
	exchangeSuccessNoAnswerOpsMockHelper := ovntest.TestifyMockHelper{
		OnCallMethodName: "Exchange", OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"}, RetArgList: []interface{}{&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}, Answer: []dns.RR{}}, 0 * time.Second, nil}, CallTimes: 1,
	}
	exchangeSuccessZeroTTLOpsMockHelper := ovntest.TestifyMockHelper{
		OnCallMethodName: "Exchange", OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"}, RetArgList: []interface{}{&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}, Answer: []dns.RR{&dns.A{A: newIP}}}, 0 * time.Second, nil}, CallTimes: 1,
	}
	exchangeSuccessNonZeroTTLOpsMockHelper := ovntest.TestifyMockHelper{
		OnCallMethodName: "Exchange", OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"}, RetArgList: []interface{}{&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess}, Answer: []dns.RR{&dns.A{Hdr: dns.RR_Header{Ttl: 100}, A: newIP}}}, 0 * time.Second, nil}, CallTimes: 1,
	}
	exchangeFailureOpsMockHelper := ovntest.TestifyMockHelper{
		OnCallMethodName: "Exchange", OnCallMethodArgType: []string{"*dns.Client", "*dns.Msg", "string"}, RetArgList: []interface{}{&dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure}}, 0 * time.Second, nil}, CallTimes: 1,
	}
	tests := []struct {
		desc                  string
		numCalls              int
		exchangeOpsMockHelper ovntest.TestifyMockHelper
		expTTL                time.Duration
	}{
		{
			desc:                  "when Exchange function returns with Rcode != RcodeSuccess, defaultMinTTL is used",
			numCalls:              1,
			exchangeOpsMockHelper: exchangeFailureOpsMockHelper,
			expTTL:                defaultMinTTL,
		},
		{
			desc:                  "when Exchange function returns successfully but without Answer, defaultMinTTL is used",
			numCalls:              1,
			exchangeOpsMockHelper: exchangeSuccessNoAnswerOpsMockHelper,
			expTTL:                defaultMinTTL,
		},
		{
			desc:                  "when TTL returned is 0 by Exchange function, defaultMinTTL is used",
			numCalls:              1,
			exchangeOpsMockHelper: exchangeSuccessZeroTTLOpsMockHelper,
			expTTL:                defaultMinTTL,
		},
		{
			desc:                  "when TTL returned is 0 by Exchange function 2 times, defaultMinTTL is used",
			numCalls:              2,
			exchangeOpsMockHelper: exchangeSuccessZeroTTLOpsMockHelper,
			expTTL:                defaultMinTTL,
		},
		{
			desc:                  "when TTL returned is 0 by Exchange function 11 times, defaultMinTTL is used",
			numCalls:              11,
			exchangeOpsMockHelper: exchangeSuccessZeroTTLOpsMockHelper,
			expTTL:                defaultMinTTL,
		},
		{
			desc:                  "when Exchange function returns with Rcode != RcodeSuccess twice, defaultMinTTL is used",
			numCalls:              2,
			exchangeOpsMockHelper: exchangeFailureOpsMockHelper,
			expTTL:                defaultMinTTL,
		},
		{
			desc:                  "when Exchange function returns with Rcode != RcodeSuccess 10 times, defaultMinTTL is used",
			numCalls:              10,
			exchangeOpsMockHelper: exchangeFailureOpsMockHelper,
			expTTL:                defaultMinTTL,
		},
		{
			desc:                  "when Exchange function returns with Rcode != RcodeSuccess 11 times, defaultMinTTL is doubled",
			numCalls:              11,
			exchangeOpsMockHelper: exchangeFailureOpsMockHelper,
			expTTL:                2 * defaultMinTTL,
		},
		{
			desc:                  "when Exchange function returns with Rcode != RcodeSuccess 14 times, 16 (2^4) times defaultMinTTL is used",
			numCalls:              14,
			exchangeOpsMockHelper: exchangeFailureOpsMockHelper,
			expTTL:                16 * defaultMinTTL,
		},
		{
			desc:                  "when Exchange function returns with Rcode != RcodeSuccess 15 times, defaultMaxTTL is used",
			numCalls:              15,
			exchangeOpsMockHelper: exchangeFailureOpsMockHelper,
			expTTL:                defaultMaxTTL,
		},
		{
			desc:                  "when TTL returned is non-zero by Exchange function, it is used",
			numCalls:              1,
			exchangeOpsMockHelper: exchangeSuccessNonZeroTTLOpsMockHelper,
			expTTL:                100 * time.Second,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			mockDNSOps := new(util_mocks.DNSOps)
			SetDNSLibOpsMockInst(mockDNSOps)
			dnsOpsMockHelper := []ovntest.TestifyMockHelper{fqdnOpsMockHelper, setQuestionOpsMockHelper, tc.exchangeOpsMockHelper}
			for index := 0; index < tc.numCalls; index++ {
				for _, item := range dnsOpsMockHelper {
					call := mockDNSOps.On(item.OnCallMethodName)
					for _, arg := range item.OnCallMethodArgType {
						call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
					}
					for _, ret := range item.RetArgList {
						call.ReturnArguments = append(call.ReturnArguments, ret)
					}
					call.Once()
				}
			}
			dns := DNS{
				dnsMap:      make(map[string]dnsValue),
				nameservers: []string{"1.1.1.1"},
			}
			dns.dnsMap[dnsName] = dnsValue{}
			for i := 0; i < tc.numCalls; i++ {
				_, _ = dns.updateOne(dnsName)
			}
			assert.Equal(t, tc.expTTL, dns.dnsMap[dnsName].ttl)
			mockDNSOps.AssertExpectations(t)
		})
	}
}
