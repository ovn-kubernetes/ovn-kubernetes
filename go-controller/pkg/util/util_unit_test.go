package util

import (
	"bytes"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	mock_k8s_io_utils_exec "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
)

func TestGetLegacyK8sMgmtIntfName(t *testing.T) {
	tests := []struct {
		desc        string
		inpNodeName string
		expRetStr   string
	}{
		{
			desc:        "node name less than 11 characters",
			inpNodeName: "lesseleven",
			expRetStr:   "k8s-lesseleven",
		},
		{
			desc:        "node name more than 11 characters",
			inpNodeName: "morethaneleven",
			expRetStr:   "k8s-morethanele",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ret := GetLegacyK8sMgmtIntfName(tc.inpNodeName)
			if tc.expRetStr != ret {
				t.Fail()
			}
		})
	}
}

func TestGetNodeChassisID(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	runCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc                    string
		errExpected             bool
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "ovs-vsctl command returns error",
			errExpected:             true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("test error")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "ovs-vsctl command returns empty chassisID along with error",
			errExpected:             true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), fmt.Errorf("test error")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "ovs-vsctl command returns empty chassisID with NO error",
			errExpected:             true,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "ovs-vsctl command returns valid chassisID",
			errExpected:             false,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("4e98c281-f12b-4601-ab5a-a3d759fcb493")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			ret, e := GetNodeChassisID()
			if tc.errExpected {
				require.Error(t, e)
			} else {
				assert.NotEmpty(t, ret)
			}
			mockExecRunner.AssertExpectations(t)
			mockCmd.AssertExpectations(t)
		})
	}
}

func TestFilterIPsSlice(t *testing.T) {

	var tests = []struct {
		s, cidrs []string
		keep     bool
		want     []string
	}{
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"1.0.0.0/24"},
			keep:  true,
			want:  []string{"1.0.0.1"},
		},
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"1.0.0.0/24"},
			keep:  false,
			want:  []string{"2.0.0.1", "2001::1", "2002::1"},
		},
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"2001::/64"},
			keep:  true,
			want:  []string{"2001::1"},
		},
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"2001::/64"},
			keep:  false,
			want:  []string{"1.0.0.1", "2.0.0.1", "2002::1"},
		},
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"1.0.0.0/24", "2001::/64", "3.0.0.0/24"},
			keep:  false,
			want:  []string{"2.0.0.1", "2002::1"},
		},
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"1.0.0.0/24", "2001::/64", "3.0.0.0/24"},
			keep:  true,
			want:  []string{"1.0.0.1", "2001::1"},
		},
		{
			s:     []string{"1.0.0.1", "2.0.0.1", "2001::1", "2002::1"},
			cidrs: []string{"1.0.0.0/24", "0.0.0.0/0"},
			keep:  true,
			want:  []string{"1.0.0.1", "2.0.0.1"},
		},
	}

	for i, tc := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			cidrs := []net.IPNet{}
			for _, cidr := range tc.cidrs {
				_, n, err := net.ParseCIDR(cidr)
				if err != nil {
					t.Fatal(err)
				}
				cidrs = append(cidrs, *n)
			}

			actual := FilterIPsSlice(tc.s, cidrs, tc.keep)
			assert.Equal(t, tc.want, actual)
		})
	}
}

func TestGenerateId(t *testing.T) {
	id := GenerateId(10)
	assert.Len(t, id, 10)
	matchesPattern, _ := regexp.MatchString("([a-zA-Z0-9-]*)", id)
	assert.True(t, matchesPattern)
}

func TestGetNetworkScopedK8sMgmtHostIntfName(t *testing.T) {
	intfName := GetNetworkScopedK8sMgmtHostIntfName(1245678)
	assert.Equal(t, "ovn-k8s-mp12456", intfName)
}

func TestServiceFromEndpointSlice(t *testing.T) {
	config.IPv4Mode = true
	type args struct {
		eps     *discovery.EndpointSlice
		netInfo NetInfo
	}
	netInfo, _ := NewNetInfo(
		&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "primary-network"},
			Topology: types.Layer3Topology,
			Subnets:  "10.1.130.0/16/24",
			Role:     types.NetworkRolePrimary,
		})
	defaultNetInfo, _ := NewNetInfo(
		&ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{Name: types.DefaultNetworkName},
		})
	var tests = []struct {
		name    string
		args    args
		want    *k8stypes.NamespacedName
		wantErr assert.ErrorAssertionFunc
	}{
		{
			name: "Primary network with matching label",
			args: args{
				eps: &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-namespace",
						Name:      "test-eps",
						Annotations: map[string]string{
							types.UserDefinedNetworkEndpointSliceAnnotation: "primary-network",
						},
						Labels: map[string]string{
							types.LabelUserDefinedServiceName: "test-service",
						},
					},
				},
				netInfo: netInfo,
			},
			want: &k8stypes.NamespacedName{
				Namespace: "test-namespace",
				Name:      "test-service",
			},
			wantErr: assert.NoError,
		},
		{
			name: "Wrong primary network with matching label",
			args: args{
				eps: &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-namespace",
						Name:      "test-eps",
						Annotations: map[string]string{
							types.UserDefinedNetworkEndpointSliceAnnotation: "wrong-network",
						},
						Labels: map[string]string{
							types.LabelUserDefinedServiceName: "test-service",
						},
					},
				},
				netInfo: netInfo,
			},
			want:    nil,
			wantErr: assert.Error,
		},
		{
			name: "Primary network with no service label set",
			args: args{
				eps: &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-namespace",
						Name:      "test-eps",
						Annotations: map[string]string{
							types.UserDefinedNetworkEndpointSliceAnnotation: "primary-network",
						},
					},
				},
				netInfo: netInfo,
			},
			want:    nil,
			wantErr: assert.NoError,
		},
		{
			name: "default network with a service label set",
			args: args{
				eps: &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-namespace",
						Name:      "test-eps",
						Labels: map[string]string{
							discovery.LabelServiceName: "test-service",
						},
					},
				},
				netInfo: defaultNetInfo,
			},
			want:    &k8stypes.NamespacedName{Namespace: "test-namespace", Name: "test-service"},
			wantErr: assert.NoError,
		},
		{
			name: "default network with no service label set",
			args: args{
				eps: &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "test-namespace",
						Name:      "test-eps",
					},
				},
				netInfo: defaultNetInfo,
			},
			want:    nil,
			wantErr: assert.NoError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ServiceFromEndpointSlice(tt.args.eps, tt.args.netInfo.GetNetworkName())
			if !tt.wantErr(t, err, fmt.Sprintf("ServiceFromEndpointSlice(%v, %v)", tt.args.eps, tt.args.netInfo)) {
				return
			}
			assert.Equalf(t, tt.want, got, "ServiceFromEndpointSlice(%v, %v)", tt.args.eps, tt.args.netInfo)
		})
	}
}

func TestLBEndpointsListGetV4Destinations(t *testing.T) {
	tests := []struct {
		name     string
		input    LBEndpointsList
		expected []IPPort
	}{
		{
			name:     "empty list",
			input:    LBEndpointsList{},
			expected: []IPPort{},
		},
		{
			name: "single endpoint with multiple IPs",
			input: LBEndpointsList{
				{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11"}},
			},
			expected: []IPPort{
				{IP: "192.168.1.10", Port: 8080},
				{IP: "192.168.1.11", Port: 8080},
			},
		},
		{
			name: "multiple endpoints",
			input: LBEndpointsList{
				{Port: 8080, V4IPs: []string{"192.168.1.10"}},
				{Port: 9090, V4IPs: []string{"192.168.1.20", "192.168.1.21"}},
			},
			expected: []IPPort{
				{IP: "192.168.1.10", Port: 8080},
				{IP: "192.168.1.20", Port: 9090},
				{IP: "192.168.1.21", Port: 9090},
			},
		},
		{
			name: "endpoint with no V4 IPs",
			input: LBEndpointsList{
				{Port: 8080, V6IPs: []string{"2001:db8::1"}},
			},
			expected: []IPPort{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.input.GetV4Destinations()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLBEndpointsListGetV6Destinations(t *testing.T) {
	tests := []struct {
		name     string
		input    LBEndpointsList
		expected []IPPort
	}{
		{
			name:     "empty list",
			input:    LBEndpointsList{},
			expected: []IPPort{},
		},
		{
			name: "single endpoint with multiple IPs",
			input: LBEndpointsList{
				{Port: 8080, V6IPs: []string{"2001:db8::1", "2001:db8::2"}},
			},
			expected: []IPPort{
				{IP: "2001:db8::1", Port: 8080},
				{IP: "2001:db8::2", Port: 8080},
			},
		},
		{
			name: "multiple endpoints",
			input: LBEndpointsList{
				{Port: 8080, V6IPs: []string{"2001:db8::1"}},
				{Port: 9090, V6IPs: []string{"2001:db8::10", "2001:db8::11"}},
			},
			expected: []IPPort{
				{IP: "2001:db8::1", Port: 8080},
				{IP: "2001:db8::10", Port: 9090},
				{IP: "2001:db8::11", Port: 9090},
			},
		},
		{
			name: "endpoint with no V6 IPs",
			input: LBEndpointsList{
				{Port: 8080, V4IPs: []string{"192.168.1.10"}},
			},
			expected: []IPPort{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.input.GetV6Destinations()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLBEndpointsGetV4Destinations(t *testing.T) {
	tests := []struct {
		name     string
		input    LBEndpoints
		expected []IPPort
	}{
		{
			name:     "no V4 IPs",
			input:    LBEndpoints{Port: 8080},
			expected: []IPPort{},
		},
		{
			name:  "single V4 IP",
			input: LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}},
			expected: []IPPort{
				{IP: "192.168.1.10", Port: 8080},
			},
		},
		{
			name:  "multiple V4 IPs",
			input: LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11", "192.168.1.12"}},
			expected: []IPPort{
				{IP: "192.168.1.10", Port: 8080},
				{IP: "192.168.1.11", Port: 8080},
				{IP: "192.168.1.12", Port: 8080},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.input.GetV4Destinations()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLBEndpointsGetV6Destinations(t *testing.T) {
	tests := []struct {
		name     string
		input    LBEndpoints
		expected []IPPort
	}{
		{
			name:     "no V6 IPs",
			input:    LBEndpoints{Port: 8080},
			expected: []IPPort{},
		},
		{
			name:  "single V6 IP",
			input: LBEndpoints{Port: 8080, V6IPs: []string{"2001:db8::1"}},
			expected: []IPPort{
				{IP: "2001:db8::1", Port: 8080},
			},
		},
		{
			name:  "multiple V6 IPs",
			input: LBEndpoints{Port: 8080, V6IPs: []string{"2001:db8::1", "2001:db8::2", "2001:db8::3"}},
			expected: []IPPort{
				{IP: "2001:db8::1", Port: 8080},
				{IP: "2001:db8::2", Port: 8080},
				{IP: "2001:db8::3", Port: 8080},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.input.GetV6Destinations()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLBEndpointsEmpty(t *testing.T) {
	tests := []struct {
		name     string
		input    LBEndpoints
		expected bool
	}{
		{
			name:     "empty endpoint",
			input:    LBEndpoints{Port: 8080},
			expected: true,
		},
		{
			name:     "only V4 IPs",
			input:    LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}},
			expected: false,
		},
		{
			name:     "only V6 IPs",
			input:    LBEndpoints{Port: 8080, V6IPs: []string{"2001:db8::1"}},
			expected: false,
		},
		{
			name:     "both V4 and V6 IPs",
			input:    LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}, V6IPs: []string{"2001:db8::1"}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.input.Empty()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestPortToLBEndpointsListEquals(t *testing.T) {
	tests := []struct {
		name     string
		a        PortToLBEndpointsList
		b        PortToLBEndpointsList
		expected bool
	}{
		{
			name:     "both empty",
			a:        PortToLBEndpointsList{},
			b:        PortToLBEndpointsList{},
			expected: true,
		},
		{
			name: "equal with single port",
			a: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			},
			b: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			},
			expected: true,
		},
		{
			name: "equal with single port, 2 IPs in different order",
			a: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11"}}},
			},
			b: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.11", "192.168.1.10"}}},
			},
			expected: true,
		},
		{
			name: "equal with multiple IPs in different order (V4 and V6)",
			a: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11", "192.168.1.12"}, V6IPs: []string{"2001:db8::1", "2001:db8::2", "2001:db8::3"}}},
			},
			b: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.12", "192.168.1.10", "192.168.1.11"}, V6IPs: []string{"2001:db8::3", "2001:db8::1", "2001:db8::2"}}},
			},
			expected: true,
		},
		{
			name: "equal with multiple LBEndpoints in different element order",
			a: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}, {Port: 9090, V4IPs: []string{"192.168.1.20"}}},
			},
			b: PortToLBEndpointsList{
				"TCP/http": {{Port: 9090, V4IPs: []string{"192.168.1.20"}}, {Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			},
			expected: true,
		},
		{
			name: "equal with multiple LBEndpoints in different element order and different IP order",
			a: PortToLBEndpointsList{
				"TCP/http": {
					{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11"}},
					{Port: 9090, V4IPs: []string{"192.168.1.20", "192.168.1.21"}},
					{Port: 7070, V4IPs: []string{"192.168.1.30", "192.168.1.31"}},
				},
			},
			b: PortToLBEndpointsList{
				"TCP/http": {
					{Port: 9090, V4IPs: []string{"192.168.1.21", "192.168.1.20"}},
					{Port: 7070, V4IPs: []string{"192.168.1.31", "192.168.1.30"}},
					{Port: 8080, V4IPs: []string{"192.168.1.11", "192.168.1.10"}},
				},
			},
			expected: true,
		},
		{
			name: "equal with multiple ports, different element order in each",
			a: PortToLBEndpointsList{
				"TCP/http":  {{Port: 8080, V4IPs: []string{"192.168.1.10"}}, {Port: 9090, V4IPs: []string{"192.168.1.20"}}},
				"TCP/https": {{Port: 8443, V4IPs: []string{"192.168.1.30"}}, {Port: 9443, V4IPs: []string{"192.168.1.40"}}},
			},
			b: PortToLBEndpointsList{
				"TCP/http":  {{Port: 9090, V4IPs: []string{"192.168.1.20"}}, {Port: 8080, V4IPs: []string{"192.168.1.10"}}},
				"TCP/https": {{Port: 9443, V4IPs: []string{"192.168.1.40"}}, {Port: 8443, V4IPs: []string{"192.168.1.30"}}},
			},
			expected: true,
		},
		{
			name: "different IPs",
			a: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			},
			b: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.11"}}},
			},
			expected: false,
		},
		{
			name: "different number of IPs",
			a: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11"}}},
			},
			b: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			},
			expected: false,
		},
		{
			name: "different ports",
			a: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			},
			b: PortToLBEndpointsList{
				"TCP/https": {{Port: 8443, V4IPs: []string{"192.168.1.10"}}},
			},
			expected: false,
		},
		{
			name: "different number of LBEndpoints elements",
			a: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}, {Port: 9090, V4IPs: []string{"192.168.1.20"}}},
			},
			b: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			},
			expected: false,
		},
		{
			name: "one empty",
			a: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			},
			b:        PortToLBEndpointsList{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.a.Equals(tt.b)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestPortToLBEndpointsListEmpty(t *testing.T) {
	tests := []struct {
		name     string
		input    PortToLBEndpointsList
		expected bool
	}{
		{
			name:     "empty map",
			input:    PortToLBEndpointsList{},
			expected: true,
		},
		{
			name: "non-empty map",
			input: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.input.Empty()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestPortToLBEndpointsListGetLBEndpoints(t *testing.T) {
	tests := []struct {
		name      string
		input     PortToLBEndpointsList
		key       string
		expected  LBEndpointsList
		expectErr bool
	}{
		{
			name:      "empty map",
			input:     PortToLBEndpointsList{},
			key:       "TCP/http",
			expected:  LBEndpointsList{},
			expectErr: true,
		},
		{
			name: "key exists",
			input: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			},
			key:       "TCP/http",
			expected:  LBEndpointsList{{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			expectErr: false,
		},
		{
			name: "key does not exist",
			input: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			},
			key:       "TCP/https",
			expected:  LBEndpointsList{},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.input.GetLBEndpoints(tt.key)
			if tt.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestPortToNodeToLBEndpointsListGetNode(t *testing.T) {
	tests := []struct {
		name     string
		input    PortToNodeToLBEndpointsList
		node     string
		expected PortToLBEndpointsList
	}{
		{
			name:     "empty map",
			input:    PortToNodeToLBEndpointsList{},
			node:     "node1",
			expected: PortToLBEndpointsList{},
		},
		{
			name: "node exists with single port",
			input: PortToNodeToLBEndpointsList{
				"TCP/http": {
					"node1": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
					"node2": {{Port: 8080, V4IPs: []string{"192.168.1.11"}}},
				},
			},
			node: "node1",
			expected: PortToLBEndpointsList{
				"TCP/http": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			},
		},
		{
			name: "node exists with multiple ports",
			input: PortToNodeToLBEndpointsList{
				"TCP/http": {
					"node1": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
				},
				"TCP/https": {
					"node1": {{Port: 8443, V4IPs: []string{"192.168.1.10"}}},
				},
			},
			node: "node1",
			expected: PortToLBEndpointsList{
				"TCP/http":  {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
				"TCP/https": {{Port: 8443, V4IPs: []string{"192.168.1.10"}}},
			},
		},
		{
			name: "node does not exist",
			input: PortToNodeToLBEndpointsList{
				"TCP/http": {
					"node1": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
				},
			},
			node:     "node2",
			expected: PortToLBEndpointsList{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.input.GetNode(tt.node)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestPortToNodeToLBEndpointsListGet(t *testing.T) {
	tests := []struct {
		name     string
		input    PortToNodeToLBEndpointsList
		port     string
		expected NodeToLBEndpointsList
	}{
		{
			name:     "empty map",
			input:    PortToNodeToLBEndpointsList{},
			port:     "TCP/http",
			expected: NodeToLBEndpointsList{},
		},
		{
			name: "port exists",
			input: PortToNodeToLBEndpointsList{
				"TCP/http": {
					"node1": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
					"node2": {{Port: 8080, V4IPs: []string{"192.168.1.11"}}},
				},
			},
			port: "TCP/http",
			expected: NodeToLBEndpointsList{
				"node1": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
				"node2": {{Port: 8080, V4IPs: []string{"192.168.1.11"}}},
			},
		},
		{
			name: "port does not exist",
			input: PortToNodeToLBEndpointsList{
				"TCP/http": {
					"node1": {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
				},
			},
			port:     "TCP/https",
			expected: NodeToLBEndpointsList{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.input.Get(tt.port)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLBEndpointsListInsert(t *testing.T) {
	tests := []struct {
		name     string
		input    LBEndpointsList
		toInsert LBEndpoints
		expected LBEndpointsList
	}{
		{
			name:     "insert into empty list",
			input:    LBEndpointsList{},
			toInsert: LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}},
			expected: LBEndpointsList{{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
		},
		{
			name:     "insert at beginning",
			input:    LBEndpointsList{{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			toInsert: LBEndpoints{Port: 7070, V4IPs: []string{"192.168.1.20"}},
			expected: LBEndpointsList{
				{Port: 7070, V4IPs: []string{"192.168.1.20"}},
				{Port: 8080, V4IPs: []string{"192.168.1.10"}},
			},
		},
		{
			name:     "insert at end",
			input:    LBEndpointsList{{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
			toInsert: LBEndpoints{Port: 9090, V4IPs: []string{"192.168.1.20"}},
			expected: LBEndpointsList{
				{Port: 8080, V4IPs: []string{"192.168.1.10"}},
				{Port: 9090, V4IPs: []string{"192.168.1.20"}},
			},
		},
		{
			name: "insert in middle",
			input: LBEndpointsList{
				{Port: 7070, V4IPs: []string{"192.168.1.10"}},
				{Port: 9090, V4IPs: []string{"192.168.1.30"}},
			},
			toInsert: LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.20"}},
			expected: LBEndpointsList{
				{Port: 7070, V4IPs: []string{"192.168.1.10"}},
				{Port: 8080, V4IPs: []string{"192.168.1.20"}},
				{Port: 9090, V4IPs: []string{"192.168.1.30"}},
			},
		},
		{
			name: "insert with same port",
			input: LBEndpointsList{
				{Port: 8080, V4IPs: []string{"192.168.1.10"}},
			},
			toInsert: LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.20"}},
			expected: LBEndpointsList{
				{Port: 8080, V4IPs: []string{"192.168.1.20"}},
				{Port: 8080, V4IPs: []string{"192.168.1.10"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.input.Insert(tt.toInsert)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLBEndpointsEquals(t *testing.T) {
	tests := []struct {
		name     string
		a        LBEndpoints
		b        LBEndpoints
		expected bool
	}{
		{
			name:     "both empty",
			a:        LBEndpoints{Port: 8080},
			b:        LBEndpoints{Port: 8080},
			expected: true,
		},
		{
			name:     "different ports",
			a:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}},
			b:        LBEndpoints{Port: 9090, V4IPs: []string{"192.168.1.10"}},
			expected: false,
		},
		{
			name:     "equal V4 IPs",
			a:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}},
			b:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}},
			expected: true,
		},
		{
			name:     "equal V4 IPs in different order",
			a:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11"}},
			b:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.11", "192.168.1.10"}},
			expected: true,
		},
		{
			name:     "different V4 IPs",
			a:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}},
			b:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.11"}},
			expected: false,
		},
		{
			name:     "different number of V4 IPs",
			a:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11"}},
			b:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}},
			expected: false,
		},
		{
			name:     "equal V6 IPs",
			a:        LBEndpoints{Port: 8080, V6IPs: []string{"2001:db8::1"}},
			b:        LBEndpoints{Port: 8080, V6IPs: []string{"2001:db8::1"}},
			expected: true,
		},
		{
			name:     "equal V6 IPs in different order",
			a:        LBEndpoints{Port: 8080, V6IPs: []string{"2001:db8::1", "2001:db8::2"}},
			b:        LBEndpoints{Port: 8080, V6IPs: []string{"2001:db8::2", "2001:db8::1"}},
			expected: true,
		},
		{
			name:     "different V6 IPs",
			a:        LBEndpoints{Port: 8080, V6IPs: []string{"2001:db8::1"}},
			b:        LBEndpoints{Port: 8080, V6IPs: []string{"2001:db8::2"}},
			expected: false,
		},
		{
			name:     "different number of V6 IPs",
			a:        LBEndpoints{Port: 8080, V6IPs: []string{"2001:db8::1", "2001:db8::2"}},
			b:        LBEndpoints{Port: 8080, V6IPs: []string{"2001:db8::1"}},
			expected: false,
		},
		{
			name:     "equal with both V4 and V6 IPs",
			a:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}, V6IPs: []string{"2001:db8::1"}},
			b:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}, V6IPs: []string{"2001:db8::1"}},
			expected: true,
		},
		{
			name:     "equal with both V4 and V6 IPs in different order",
			a:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11"}, V6IPs: []string{"2001:db8::1", "2001:db8::2"}},
			b:        LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.11", "192.168.1.10"}, V6IPs: []string{"2001:db8::2", "2001:db8::1"}},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.a.Equals(tt.b)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetEndpointsForService(t *testing.T) {
	node1 := "node1"
	node2 := "node2"
	node3 := "node3"

	tests := []struct {
		name                 string
		endpointSlices       []*discovery.EndpointSlice
		service              *corev1.Service
		nodes                sets.Set[string]
		needsGlobalEndpoints bool
		needsLocalEndpoints  bool
		expectedGlobal       PortToLBEndpointsList
		expectedLocal        PortToNodeToLBEndpointsList
		expectError          bool
	}{
		{
			name: "basic service with single endpoint slice",
			endpointSlices: []*discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc-abc",
						Namespace: "default",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.10"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node1,
						},
						{
							Addresses: []string{"192.168.1.11"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node2,
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To("http"),
							Port:     ptr.To(int32(8080)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
					},
				},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "http",
							Port:     80,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
			nodes:                sets.New(node1, node2),
			needsGlobalEndpoints: true,
			needsLocalEndpoints:  true,
			expectedGlobal: PortToLBEndpointsList{
				"TCP/http": {
					{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11"}},
				},
			},
			expectedLocal: PortToNodeToLBEndpointsList{
				"TCP/http": {
					node1: {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
					node2: {{Port: 8080, V4IPs: []string{"192.168.1.11"}}},
				},
			},
			expectError: false,
		},
		{
			name: "basic service with single endpoint slice and invalid endpoint slice key",
			endpointSlices: []*discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc-abc",
						Namespace: "default",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.10"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node1,
						},
						{
							Addresses: []string{"192.168.1.11"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node2,
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To("http"),
							Port:     ptr.To(int32(8080)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
						{
							Name:     ptr.To("https"),
							Port:     ptr.To(int32(8081)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
					},
				},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "http",
							Port:     80,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
			nodes:                sets.New(node1, node2),
			needsGlobalEndpoints: true,
			needsLocalEndpoints:  true,
			expectedGlobal: PortToLBEndpointsList{
				"TCP/http": {
					{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11"}},
				},
			},
			expectedLocal: PortToNodeToLBEndpointsList{
				"TCP/http": {
					node1: {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
					node2: {{Port: 8080, V4IPs: []string{"192.168.1.11"}}},
				},
			},
			expectError: false,
		},
		{
			name: "basic service with single endpoint slice and invalid endpoint slice key",
			endpointSlices: []*discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc-abc",
						Namespace: "default",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.10"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node1,
						},
						{
							Addresses: []string{"192.168.1.11"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node2,
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To("http"),
							Port:     ptr.To(int32(8080)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
						{
							Name:     ptr.To("https"),
							Port:     ptr.To(int32(8081)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
					},
				},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "http",
							Port:     80,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
			nodes:                sets.New(node1, node2),
			needsGlobalEndpoints: true,
			needsLocalEndpoints:  true,
			expectedGlobal: PortToLBEndpointsList{
				"TCP/http": {
					{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11"}},
				},
			},
			expectedLocal: PortToNodeToLBEndpointsList{
				"TCP/http": {
					node1: {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
					node2: {{Port: 8080, V4IPs: []string{"192.168.1.11"}}},
				},
			},
			expectError: false,
		},
		{
			name: "dual stack service",
			endpointSlices: []*discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc-v4",
						Namespace: "default",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.10"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node1,
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To("http"),
							Port:     ptr.To(int32(8080)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc-v6",
						Namespace: "default",
					},
					AddressType: discovery.AddressTypeIPv6,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"2001:db8::1"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node1,
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To("http"),
							Port:     ptr.To(int32(8080)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
					},
				},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "http",
							Port:     80,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
			nodes:                sets.New(node1),
			needsGlobalEndpoints: true,
			needsLocalEndpoints:  true,
			expectedGlobal: PortToLBEndpointsList{
				"TCP/http": {
					{Port: 8080, V4IPs: []string{"192.168.1.10"}, V6IPs: []string{"2001:db8::1"}},
				},
			},
			expectedLocal: PortToNodeToLBEndpointsList{
				"TCP/http": {
					node1: {{Port: 8080, V4IPs: []string{"192.168.1.10"}, V6IPs: []string{"2001:db8::1"}}},
				},
			},
			expectError: false,
		},
		{
			name: "filter endpoints not on requested nodes",
			endpointSlices: []*discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc-abc",
						Namespace: "default",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.10"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node1,
						},
						{
							Addresses: []string{"192.168.1.11"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node2,
						},
						{
							Addresses: []string{"192.168.1.12"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node3,
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To("http"),
							Port:     ptr.To(int32(8080)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
					},
				},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "http",
							Port:     80,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
			nodes:                sets.New(node1, node2),
			needsGlobalEndpoints: true,
			needsLocalEndpoints:  true,
			expectedGlobal: PortToLBEndpointsList{
				"TCP/http": {
					{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11", "192.168.1.12"}},
				},
			},
			expectedLocal: PortToNodeToLBEndpointsList{
				"TCP/http": {
					node1: {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
					node2: {{Port: 8080, V4IPs: []string{"192.168.1.11"}}},
				},
			},
			expectError: false,
		},
		{
			name: "nil service allows all endpoints (deletion scenario)",
			endpointSlices: []*discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc-abc",
						Namespace: "default",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.10"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node1,
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To("http"),
							Port:     ptr.To(int32(8080)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
					},
				},
			},
			service:              nil,
			nodes:                sets.New(node1),
			needsGlobalEndpoints: true,
			needsLocalEndpoints:  true,
			expectedGlobal: PortToLBEndpointsList{
				"TCP/http": {
					{Port: 8080, V4IPs: []string{"192.168.1.10"}},
				},
			},
			expectedLocal: PortToNodeToLBEndpointsList{
				"TCP/http": {
					node1: {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
				},
			},
			expectError: false,
		},
		{
			name: "only global endpoints needed",
			endpointSlices: []*discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc-abc",
						Namespace: "default",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.10"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node1,
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To("http"),
							Port:     ptr.To(int32(8080)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
					},
				},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "http",
							Port:     80,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
			nodes:                sets.New(node1),
			needsGlobalEndpoints: true,
			needsLocalEndpoints:  false,
			expectedGlobal: PortToLBEndpointsList{
				"TCP/http": {
					{Port: 8080, V4IPs: []string{"192.168.1.10"}},
				},
			},
			expectedLocal: PortToNodeToLBEndpointsList{},
			expectError:   false,
		},
		{
			name: "only local endpoints needed",
			endpointSlices: []*discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc-abc",
						Namespace: "default",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.10"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node1,
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To("http"),
							Port:     ptr.To(int32(8080)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
					},
				},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "http",
							Port:     80,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
			nodes:                sets.New(node1),
			needsGlobalEndpoints: false,
			needsLocalEndpoints:  true,
			expectedGlobal:       PortToLBEndpointsList{},
			expectedLocal: PortToNodeToLBEndpointsList{
				"TCP/http": {
					node1: {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
				},
			},
			expectError: false,
		},
		{
			name: "multiple service portnames mapping into the same endpoint slice different names on same service",
			endpointSlices: []*discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc-abc",
						Namespace: "default",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.10"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node1,
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To("http"),
							Port:     ptr.To(int32(8080)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
						{
							Name:     ptr.To("https"),
							Port:     ptr.To(int32(8443)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
					},
				},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "http",
							Port:     80,
							Protocol: corev1.ProtocolTCP,
						},
						{
							Name:     "https",
							Port:     443,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
			nodes:                sets.New(node1),
			needsGlobalEndpoints: true,
			needsLocalEndpoints:  true,
			expectedGlobal: PortToLBEndpointsList{
				"TCP/http": {
					{Port: 8080, V4IPs: []string{"192.168.1.10"}},
				},
				"TCP/https": {
					{Port: 8443, V4IPs: []string{"192.168.1.10"}},
				},
			},
			expectedLocal: PortToNodeToLBEndpointsList{
				"TCP/http": {
					node1: {{Port: 8080, V4IPs: []string{"192.168.1.10"}}},
				},
				"TCP/https": {
					node1: {{Port: 8443, V4IPs: []string{"192.168.1.10"}}},
				},
			},
			expectError: false,
		},
		// The following scenario will happen when named ports are used together with and pods with a different name to
		// port mapping.
		{
			name: "single service port with multiple endpoint ports with same name",
			endpointSlices: []*discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc-abc",
						Namespace: "default",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.10"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node1,
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To("http"),
							Port:     ptr.To(int32(8080)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc-abc2",
						Namespace: "default",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"192.168.1.11"},
							Conditions: discovery.EndpointConditions{
								Ready: ptr.To(true),
							},
							NodeName: &node1,
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To("http"),
							Port:     ptr.To(int32(8081)),
							Protocol: ptr.To(corev1.ProtocolTCP),
						},
					},
				},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "http",
							Port:     80,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
			nodes:                sets.New(node1),
			needsGlobalEndpoints: true,
			needsLocalEndpoints:  true,
			expectedGlobal: PortToLBEndpointsList{
				"TCP/http": {
					{Port: 8080, V4IPs: []string{"192.168.1.10"}},
					{Port: 8081, V4IPs: []string{"192.168.1.11"}},
				},
			},
			expectedLocal: PortToNodeToLBEndpointsList{
				"TCP/http": {
					node1: {
						{Port: 8080, V4IPs: []string{"192.168.1.10"}},
						{Port: 8081, V4IPs: []string{"192.168.1.11"}},
					},
				},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			global, local, err := GetEndpointsForService(
				tt.endpointSlices,
				tt.service,
				tt.nodes,
				tt.needsGlobalEndpoints,
				tt.needsLocalEndpoints,
			)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			if tt.needsGlobalEndpoints {
				assert.True(t, tt.expectedGlobal.Equals(global), "global endpoints mismatch")
			}

			if tt.needsLocalEndpoints {
				assert.Equal(t, tt.expectedLocal, local)
			}
		})
	}
}

func TestGroupEndpointsByNode(t *testing.T) {
	node1 := "node1"
	node2 := "node2"

	tests := []struct {
		name      string
		endpoints []discovery.Endpoint
		expected  map[string][]discovery.Endpoint
	}{
		{
			name:      "empty endpoints",
			endpoints: []discovery.Endpoint{},
			expected:  map[string][]discovery.Endpoint{},
		},
		{
			name: "single endpoint with node",
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					NodeName:  &node1,
				},
			},
			expected: map[string][]discovery.Endpoint{
				node1: {
					{
						Addresses: []string{"192.168.1.10"},
						NodeName:  &node1,
					},
				},
			},
		},
		{
			name: "multiple endpoints on different nodes",
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					NodeName:  &node1,
				},
				{
					Addresses: []string{"192.168.1.11"},
					NodeName:  &node2,
				},
			},
			expected: map[string][]discovery.Endpoint{
				node1: {
					{
						Addresses: []string{"192.168.1.10"},
						NodeName:  &node1,
					},
				},
				node2: {
					{
						Addresses: []string{"192.168.1.11"},
						NodeName:  &node2,
					},
				},
			},
		},
		{
			name: "multiple endpoints on same node",
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					NodeName:  &node1,
				},
				{
					Addresses: []string{"192.168.1.11"},
					NodeName:  &node1,
				},
			},
			expected: map[string][]discovery.Endpoint{
				node1: {
					{
						Addresses: []string{"192.168.1.10"},
						NodeName:  &node1,
					},
					{
						Addresses: []string{"192.168.1.11"},
						NodeName:  &node1,
					},
				},
			},
		},
		{
			name: "endpoint without node name is skipped",
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					NodeName:  &node1,
				},
				{
					Addresses: []string{"192.168.1.11"},
					NodeName:  nil,
				},
			},
			expected: map[string][]discovery.Endpoint{
				node1: {
					{
						Addresses: []string{"192.168.1.10"},
						NodeName:  &node1,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := groupEndpointsByNode(tt.endpoints)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildNodeLBEndpointsList(t *testing.T) {
	node1 := "node1"
	node2 := "node2"
	node3 := "node3"

	tests := []struct {
		name          string
		service       *corev1.Service
		portNumberMap map[int32][]discovery.Endpoint
		nodes         sets.Set[string]
		expected      NodeToLBEndpointsList
		expectError   bool
	}{
		{
			name: "single port with endpoints on multiple nodes",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			portNumberMap: map[int32][]discovery.Endpoint{
				8080: {
					{
						Addresses: []string{"192.168.1.10"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
						NodeName: &node1,
					},
					{
						Addresses: []string{"192.168.1.11"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
						NodeName: &node2,
					},
				},
			},
			nodes: sets.New(node1, node2),
			expected: NodeToLBEndpointsList{
				node1: {
					{Port: 8080, V4IPs: []string{"192.168.1.10"}},
				},
				node2: {
					{Port: 8080, V4IPs: []string{"192.168.1.11"}},
				},
			},
			expectError: false,
		},
		{
			name: "multiple ports with endpoints",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			portNumberMap: map[int32][]discovery.Endpoint{
				8080: {
					{
						Addresses: []string{"192.168.1.10"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
						NodeName: &node1,
					},
				},
				9090: {
					{
						Addresses: []string{"192.168.1.10"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
						NodeName: &node1,
					},
				},
			},
			nodes: sets.New(node1),
			expected: NodeToLBEndpointsList{
				node1: {
					{Port: 8080, V4IPs: []string{"192.168.1.10"}},
					{Port: 9090, V4IPs: []string{"192.168.1.10"}},
				},
			},
			expectError: false,
		},
		{
			name: "filter out endpoints not in node set",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			portNumberMap: map[int32][]discovery.Endpoint{
				8080: {
					{
						Addresses: []string{"192.168.1.10"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
						NodeName: &node1,
					},
					{
						Addresses: []string{"192.168.1.11"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
						NodeName: &node3,
					},
				},
			},
			nodes: sets.New(node1, node2),
			expected: NodeToLBEndpointsList{
				node1: {
					{Port: 8080, V4IPs: []string{"192.168.1.10"}},
				},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := buildNodeLBEndpointsList(tt.service, tt.portNumberMap, tt.nodes)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestBuildNodeLBEndpoints(t *testing.T) {
	node1 := "node1"
	node2 := "node2"

	tests := []struct {
		name        string
		service     *corev1.Service
		portNumber  int32
		endpoints   []discovery.Endpoint
		nodes       sets.Set[string]
		expected    NodeToLBEndpoints
		expectError bool
	}{
		{
			name: "endpoints on different nodes",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			portNumber: 8080,
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
					NodeName: &node1,
				},
				{
					Addresses: []string{"192.168.1.11"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
					NodeName: &node2,
				},
			},
			nodes: sets.New(node1, node2),
			expected: NodeToLBEndpoints{
				node1: {Port: 8080, V4IPs: []string{"192.168.1.10"}},
				node2: {Port: 8080, V4IPs: []string{"192.168.1.11"}},
			},
			expectError: false,
		},
		{
			name: "filter nodes not in set",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			portNumber: 8080,
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
					NodeName: &node1,
				},
				{
					Addresses: []string{"192.168.1.11"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
					NodeName: &node2,
				},
			},
			nodes: sets.New(node1),
			expected: NodeToLBEndpoints{
				node1: {Port: 8080, V4IPs: []string{"192.168.1.10"}},
			},
			expectError: false,
		},
		{
			name: "no valid nodes results in error",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			portNumber: 8080,
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
					NodeName: &node1,
				},
			},
			nodes:       sets.New(node2),
			expected:    NodeToLBEndpoints{},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := buildNodeLBEndpoints(tt.service, tt.portNumber, tt.endpoints, tt.nodes)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestBuildLBEndpointsList(t *testing.T) {
	tests := []struct {
		name          string
		service       *corev1.Service
		portNumberMap map[int32][]discovery.Endpoint
		expected      LBEndpointsList
		expectError   bool
	}{
		{
			name: "single port with multiple endpoints",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			portNumberMap: map[int32][]discovery.Endpoint{
				8080: {
					{
						Addresses: []string{"192.168.1.10"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
					{
						Addresses: []string{"192.168.1.11"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
				},
			},
			expected: LBEndpointsList{
				{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11"}},
			},
			expectError: false,
		},
		{
			name: "multiple ports with endpoints",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			portNumberMap: map[int32][]discovery.Endpoint{
				8080: {
					{
						Addresses: []string{"192.168.1.10"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
				},
				9090: {
					{
						Addresses: []string{"192.168.1.11"},
						Conditions: discovery.EndpointConditions{
							Ready: ptr.To(true),
						},
					},
				},
			},
			expected: LBEndpointsList{
				{Port: 8080, V4IPs: []string{"192.168.1.10"}},
				{Port: 9090, V4IPs: []string{"192.168.1.11"}},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := buildLBEndpointsList(tt.service, tt.portNumberMap)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.True(t, tt.expected.Equals(result), "expected %+v, got %+v", tt.expected, result)
			}
		})
	}
}

func TestBuildLBEndpoints(t *testing.T) {
	tests := []struct {
		name        string
		service     *corev1.Service
		port        int32
		endpoints   []discovery.Endpoint
		expected    LBEndpoints
		expectError bool
	}{
		{
			name: "IPv4 endpoints",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			port: 8080,
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
				},
				{
					Addresses: []string{"192.168.1.11"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
				},
			},
			expected: LBEndpoints{
				Port:  8080,
				V4IPs: []string{"192.168.1.10", "192.168.1.11"},
			},
			expectError: false,
		},
		{
			name: "IPv6 endpoints",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			port: 8080,
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"2001:db8::1"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
				},
				{
					Addresses: []string{"2001:db8::2"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
				},
			},
			expected: LBEndpoints{
				Port:  8080,
				V6IPs: []string{"2001:db8::1", "2001:db8::2"},
			},
			expectError: false,
		},
		{
			name: "dual stack endpoints",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			port: 8080,
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
				},
				{
					Addresses: []string{"2001:db8::1"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
				},
			},
			expected: LBEndpoints{
				Port:  8080,
				V4IPs: []string{"192.168.1.10"},
				V6IPs: []string{"2001:db8::1"},
			},
			expectError: false,
		},
		{
			name: "invalid port number - too low",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			port: 0,
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
				},
			},
			expected:    LBEndpoints{},
			expectError: true,
		},
		{
			name: "invalid port number - too high",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			port: 65536,
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
				},
			},
			expected:    LBEndpoints{},
			expectError: true,
		},
		{
			name:    "invalid port number - too high - with nil service",
			service: nil,
			port:    65536,
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
				},
			},
			expected:    LBEndpoints{},
			expectError: true,
		},
		{
			name:    "nil service with valid endpoints",
			service: nil,
			port:    8080,
			endpoints: []discovery.Endpoint{
				{
					Addresses: []string{"192.168.1.10"},
					Conditions: discovery.EndpointConditions{
						Ready: ptr.To(true),
					},
				},
			},
			expected: LBEndpoints{
				Port:  8080,
				V4IPs: []string{"192.168.1.10"},
			},
			expectError: false,
		},
		{
			name: "no valid IP addresses in endpoints",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
				},
			},
			port:        10000,
			endpoints:   []discovery.Endpoint{},
			expected:    LBEndpoints{},
			expectError: true,
		},
		{
			name:        "no valid IP addresses in endpoints with nil service",
			service:     nil,
			port:        10000,
			endpoints:   []discovery.Endpoint{},
			expected:    LBEndpoints{},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := buildLBEndpoints(tt.service, tt.port, tt.endpoints)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.True(t, tt.expected.Equals(result), "expected %+v, got %+v", tt.expected, result)
			}
		})
	}
}

func TestTargetEndpointsAddEndpoint(t *testing.T) {
	tests := []struct {
		name       string
		initial    targetEndpoints
		portName   string
		protocol   corev1.Protocol
		portNumber int32
		endpoint   discovery.Endpoint
		verify     func(t *testing.T, te targetEndpoints)
	}{
		{
			name:       "add to empty target endpoints",
			initial:    targetEndpoints{},
			portName:   "http",
			protocol:   corev1.ProtocolTCP,
			portNumber: 8080,
			endpoint: discovery.Endpoint{
				Addresses: []string{"192.168.1.10"},
			},
			verify: func(t *testing.T, te targetEndpoints) {
				t.Helper()
				assert.Contains(t, te, "http")
				assert.Contains(t, te["http"], corev1.ProtocolTCP)
				assert.Contains(t, te["http"][corev1.ProtocolTCP], int32(8080))
				assert.Len(t, te["http"][corev1.ProtocolTCP][8080], 1)
				assert.Equal(t, "192.168.1.10", te["http"][corev1.ProtocolTCP][8080][0].Addresses[0])
			},
		},
		{
			name:       "add multiple endpoints to same port",
			initial:    targetEndpoints{},
			portName:   "http",
			protocol:   corev1.ProtocolTCP,
			portNumber: 8080,
			endpoint: discovery.Endpoint{
				Addresses: []string{"192.168.1.10"},
			},
			verify: func(t *testing.T, te targetEndpoints) {
				t.Helper()
				te.addEndpoint("http", corev1.ProtocolTCP, 8080, discovery.Endpoint{
					Addresses: []string{"192.168.1.11"},
				})
				assert.Len(t, te["http"][corev1.ProtocolTCP][8080], 2)
			},
		},
		{
			name:       "add endpoint with different protocol",
			initial:    targetEndpoints{},
			portName:   "dns",
			protocol:   corev1.ProtocolUDP,
			portNumber: 53,
			endpoint: discovery.Endpoint{
				Addresses: []string{"192.168.1.10"},
			},
			verify: func(t *testing.T, te targetEndpoints) {
				t.Helper()
				assert.Contains(t, te["dns"], corev1.ProtocolUDP)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te := tt.initial
			te.addEndpoint(tt.portName, tt.protocol, tt.portNumber, tt.endpoint)
			tt.verify(t, te)
		})
	}
}
