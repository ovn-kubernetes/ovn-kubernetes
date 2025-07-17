package podannotation

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func TestMarshalPodAnnotation(t *testing.T) {
	tests := []struct {
		desc           string
		inpPodAnnot    PodAnnotation
		errAssert      bool  // used when an error string CANNOT be matched or sub-matched
		errMatch       error //used when an error string CAN be matched or sub-matched
		expectedOutput map[string]string
	}{
		{
			desc:           "PodAnnotation instance with no fields set",
			inpPodAnnot:    PodAnnotation{},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":""}}`},
		},
		{
			desc:           "PodAnnotation instance when role is set to primary",
			inpPodAnnot:    PodAnnotation{Role: types.NetworkRolePrimary},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"","role":"primary"}}`},
		},
		{
			desc: "single IP assigned to pod with MAC, Gateway, Routes NOT SPECIFIED",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/24")},
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"","ip_address":"192.168.0.5/24"}}`},
		},
		{
			desc: "multiple IPs assigned to pod with MAC, Gateway, Routes NOT SPECIFIED",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{
					ovntest.MustParseIPNet("192.168.0.5/24"),
					ovntest.MustParseIPNet("fd01::1234/64"),
				},
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24","fd01::1234/64"],"mac_address":""}}`},
		},
		{
			desc: "test code path when podInfo.Gateways count is equal to ONE",
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/24")},
				Gateways: []net.IP{
					net.ParseIP("192.168.0.1"),
				},
				Role: types.NetworkRoleSecondary,
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"","gateway_ips":["192.168.0.1"],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1","role":"secondary"}}`},
		},
		{
			desc:     "verify error thrown when number of gateways greater than one for a single-stack network",
			errMatch: fmt.Errorf("bad podNetwork data: single-stack network can only have a single gateway"),
			inpPodAnnot: PodAnnotation{
				IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.5/24")},
				Gateways: []net.IP{
					net.ParseIP("192.168.1.0"),
					net.ParseIP("fd01::1"),
				},
			},
		},
		{
			desc:      "verify error thrown when destination IP not specified as part of Route",
			errAssert: true,
			inpPodAnnot: PodAnnotation{
				Routes: []PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("0.0.0.0/0"),
						NextHop: net.ParseIP("192.168.1.1"),
					},
				},
			},
		},
		{
			desc: "test code path when destination IP is specified as part of Route",
			inpPodAnnot: PodAnnotation{
				Routes: []PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("192.168.1.0/24"),
						NextHop: net.ParseIP("192.168.1.1"),
					},
				},
				Role: types.NetworkRoleInfrastructure,
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"","routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1.1"}],"role":"infrastructure-locked"}}`},
		},
		{
			desc: "next hop not set for route",
			inpPodAnnot: PodAnnotation{
				Routes: []PodRoute{
					{
						Dest: ovntest.MustParseIPNet("192.168.1.0/24"),
					},
				},
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"","routes":[{"dest":"192.168.1.0/24","nextHop":""}]}}`},
		},
		{
			desc: "ipv6 LLA gateway ip is set",
			inpPodAnnot: PodAnnotation{
				GatewayIPv6LLA: ovntest.MustParseIP("fe80::"),
			},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"","ipv6_lla_gateway_ip":"fe80::"}}`},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			var e error
			res := map[string]string{}
			res, e = MarshalPodAnnotation(res, &tc.inpPodAnnot, types.DefaultNetworkName)
			t.Log(res, e)
			if tc.errAssert {
				assert.Error(t, e)
			} else if tc.errMatch != nil {
				assert.Contains(t, e.Error(), tc.errMatch.Error())
			} else {
				assert.True(t, reflect.DeepEqual(res, tc.expectedOutput))
			}
		})
	}
}

func TestUnmarshalPodAnnotation(t *testing.T) {
	tests := []struct {
		desc        string
		inpAnnotMap map[string]string
		errAssert   bool
		errMatch    error
		nadName     string
	}{
		{
			desc:        "verify `OVN pod annotation not found` error thrown",
			inpAnnotMap: nil,
			errMatch:    fmt.Errorf("could not find OVN pod annotation in"),
		},
		{
			desc:        "verify json unmarshal error",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"}}`}, //removed a quote to force json unmarshal error
			errMatch:    fmt.Errorf("failed to unmarshal ovn pod annotation"),
			nadName:     "default",
		},
		{
			desc:        "verify MAC error parse error",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":""}}`},
			errMatch:    fmt.Errorf("failed to parse pod MAC"),
			nadName:     "default",
		},
		{
			desc:        "test path when ip_addresses is empty and ip_address is set",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"0a:58:fd:98:00:01", "ip_address":"192.168.0.11/24"}}`},
			nadName:     "default",
		},
		{
			desc:        "verify error thrown when ip_address and ip_addresses are conflicted",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.11/24"}}`},
			errMatch:    fmt.Errorf("bad annotation data (ip_address and ip_addresses conflict)"),
			nadName:     "default",
		},
		{
			desc:        "verify error thrown when failed to parse pod IP",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0./24"],"mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0./24"}}`},
			errMatch:    fmt.Errorf("failed to parse pod IP"),
			nadName:     "default",
		},
		{
			desc:        "verify error thrown when gateway_ip and gateway_ips are conflicted",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"gateway_ips":["192.168.0.1"], "gateway_ip":"192.168.1.1","mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.5/24"}}`},
			errMatch:    fmt.Errorf("bad annotation data (gateway_ip and gateway_ips conflict)"),
			nadName:     "default",
		},
		{
			desc:        "test path when gateway_ips list is empty but gateway_ip is present",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"gateway_ips":[], "gateway_ip":"192.168.0.1","mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.5/24"}}`},
			nadName:     "default",
		},
		{
			desc:        "verify error thrown when failed to parse pod gateway",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"gateway_ips":["192.168.0."], "gateway_ip":"192.168.0.","mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.5/24"}}`},
			errMatch:    fmt.Errorf("failed to parse pod gateway"),
			nadName:     "default",
		},
		{
			desc:        "verify error thrown when failed to parse pod route destination",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1./24"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			errMatch:    fmt.Errorf("failed to parse pod route dest"),
			nadName:     "default",
		},
		{
			desc:        "verify error thrown when default Route not specified as gateway",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"0.0.0.0/0"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			errAssert:   true,
			nadName:     "default",
		},
		{
			desc:        "verify error thrown when failed to parse pod route next hop",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1."}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			errMatch:    fmt.Errorf("failed to parse pod route next hop"),
			nadName:     "default",
		},
		{
			desc:        "verify error thrown where pod route has next hop of different family",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"fd01::1234/64","nextHop":"192.168.1.1"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			errAssert:   true,
			nadName:     "default",
		},
		{
			desc:        "verify successful unmarshal of pod annotation",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1.1"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1"}}`},
			nadName:     "default",
		},
		{
			desc:        "verify successful unmarshal of pod annotation when role field is set",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["192.168.0.1"],"routes":[{"dest":"192.168.1.0/24","nextHop":"192.168.1.1"}],"ip_address":"192.168.0.5/24","gateway_ip":"192.168.0.1","role":"primary"}}`},
			nadName:     "default",
		},
		{
			desc:        "verify successful unmarshal of pod annotation when *only* the MAC address is present",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"mac_address":"0a:58:fd:98:00:01"}}`},
			nadName:     "default",
		},
		{
			desc:        "verify successful unmarshal of pod annotation with ipv6 lla gateway ip",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"test_ns/l2":{"mac_address":"0a:58:fd:98:00:01","ipv6_lla_gateway_ip":"fe80::858:fdff:fe98:1"}}`},
			nadName:     "test_ns/l2",
		},
		{
			desc:        "verify error thrown when failed to unmarshal of pod annotation with non ipv6 lla gateway ip",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"test_ns/l2":{"mac_address":"0a:58:fd:98:00:01","ipv6_lla_gateway_ip":"2001:0db8::1"}}`},
			errMatch:    fmt.Errorf(`failed to parse pod ipv6 lla gateway, or non ipv6 lla "2001:0db8::1"`),
			nadName:     "test_ns/l2",
		},
		{
			desc:        "verify error thrown when failed to unmarshal of pod annotation with ipv4 instead ipv6 lla gateway ip",
			inpAnnotMap: map[string]string{"k8s.ovn.org/pod-networks": `{"test_ns/l2":{"mac_address":"0a:58:fd:98:00:01","ipv6_lla_gateway_ip":"192.168.0.5"}}`},
			errMatch:    fmt.Errorf(`failed to parse pod ipv6 lla gateway, or non ipv6 lla "192.168.0.5"`),
			nadName:     "test_ns/l2",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, e := UnmarshalPodAnnotation(tc.inpAnnotMap, tc.nadName)
			t.Log(res, e)
			if tc.errAssert {
				assert.Error(t, e)
			} else if tc.errMatch != nil {
				assert.Contains(t, e.Error(), tc.errMatch.Error())
			} else {
				t.Log(res)
				assert.NotNil(t, res)
			}
		})
	}
}

func TestGetPodIPsOfNetwork(t *testing.T) {
	const (
		secondaryNetworkIPAddr = "200.200.200.200"
		namespace              = "ns1"
		secondaryNetworkName   = "bluetenant"
	)
	tests := []struct {
		desc        string
		inpPod      *corev1.Pod
		networkInfo util.NetInfo
		errAssert   bool
		errMatch    error
		outExp      []net.IP
	}{
		// TODO: The function body may need to check that pod input is non-nil to avoid panic ?
		/*{
			desc:	"test when pod input is nil",
			inpPod: nil,
			errExp: true,
		},*/
		{
			desc: "test when pod annotation is non-nil for the default cluster network",
			inpPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.1/24"],"mac_address":"0a:58:fd:98:00:01"}}`},
				},
			},
			networkInfo: &util.DefaultNetInfo{},
			outExp:      []net.IP{ovntest.MustParseIP("192.168.0.1")},
		},
		{
			desc:        "test when pod.status.PodIP is empty",
			inpPod:      &corev1.Pod{},
			networkInfo: &util.DefaultNetInfo{},
			errMatch:    ErrNoPodIPFound,
		},
		{
			desc: "test when pod.status.PodIP is non-empty",
			inpPod: &corev1.Pod{
				Status: corev1.PodStatus{
					PodIP: "192.168.1.15",
				},
			},
			networkInfo: &util.DefaultNetInfo{},
			outExp:      []net.IP{ovntest.MustParseIP("192.168.1.15")},
		},
		{
			desc: "test when pod.status.PodIPs is non-empty",
			inpPod: &corev1.Pod{
				Status: corev1.PodStatus{
					PodIPs: []corev1.PodIP{
						{IP: "192.168.1.15"},
					},
				},
			},
			networkInfo: &util.DefaultNetInfo{},
			outExp:      []net.IP{ovntest.MustParseIP("192.168.1.15")},
		},
		{
			desc: "test path when an entry in pod.status.PodIPs is malformed",
			inpPod: &corev1.Pod{
				Status: corev1.PodStatus{
					PodIPs: []corev1.PodIP{
						{IP: "192.168.1."},
					},
				},
			},
			networkInfo: &util.DefaultNetInfo{},
			errMatch:    ErrNoPodIPFound,
		},
		{
			desc: "test when pod annotation is non-nil for a secondary network",
			inpPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.v1.cni.cncf.io/networks": fmt.Sprintf(`[{"name": %q, "namespace": %q}]`, secondaryNetworkName, namespace),
						"k8s.ovn.org/pod-networks":    fmt.Sprintf(`{%q:{"ip_addresses":["%s/24"],"mac_address":"0a:58:fd:98:00:01"}}`, util.GetNADName(namespace, secondaryNetworkName), secondaryNetworkIPAddr),
					},
				},
			},
			networkInfo: newDummyNetInfo(namespace, secondaryNetworkName),
			outExp:      []net.IP{ovntest.MustParseIP(secondaryNetworkIPAddr)},
		},
		{
			desc: "test when pod annotation is non-nil for a secondary network",
			inpPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.v1.cni.cncf.io/networks": fmt.Sprintf(`[{"name": %q, "namespace": %q}]`, secondaryNetworkName, namespace),
						"k8s.ovn.org/pod-networks":    "{}",
					},
				},
			},
			networkInfo: newDummyNetInfo(namespace, secondaryNetworkName),
			outExp:      []net.IP{},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res1, e := GetPodIPsOfNetwork(tc.inpPod, tc.networkInfo)
			t.Log(res1, e)
			if tc.errAssert {
				require.Error(t, e)
			} else if tc.errMatch != nil {
				if errors.Is(tc.errMatch, ErrNoPodIPFound) {
					assert.ErrorIs(t, e, ErrNoPodIPFound)
				} else {
					assert.Contains(t, e.Error(), tc.errMatch.Error())
				}
			} else {
				assert.Equal(t, tc.outExp, res1)
			}
			if len(tc.outExp) > 0 {
				res2, e := GetPodCIDRsWithFullMask(tc.inpPod, tc.networkInfo)
				t.Log(res2, e)
				if tc.errAssert {
					assert.Error(t, e)
				} else if tc.errMatch != nil {
					if errors.Is(tc.errMatch, ErrNoPodIPFound) {
						assert.ErrorIs(t, e, ErrNoPodIPFound)
					} else {
						assert.Contains(t, e.Error(), tc.errMatch.Error())
					}
				} else {
					expectedIP := tc.outExp[0]
					ipNet := net.IPNet{
						IP:   expectedIP,
						Mask: util.GetIPFullMask(expectedIP),
					}
					assert.Equal(t, []*net.IPNet{&ipNet}, res2)
				}
			}
		})
	}
}

func newDummyNetInfo(namespace, networkName string) util.NetInfo {
	netInfo, _ := util.NewNetInfo(&ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: networkName},
		Topology: types.Layer2Topology,
	})
	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.AddNADs(util.GetNADName(namespace, networkName))
	return mutableNetInfo
}

func TestUnmarshalUDNOpenPortsAnnotation(t *testing.T) {
	intRef := func(i int) *int {
		return &i
	}

	tests := []struct {
		desc      string
		input     string
		errSubstr string
		result    []*OpenPort
	}{
		{
			desc:      "protocol without port",
			input:     `- protocol: tcp`,
			errSubstr: "port is required",
		},
		{
			desc:      "port without protocol",
			input:     `- port: 80`,
			errSubstr: "invalid protocol",
		},
		{
			desc:      "invalid protocol",
			input:     `- protocol: foo`,
			errSubstr: "invalid protocol",
		},
		{
			desc: "icmp with port",
			input: `- protocol: icmp
  port: 80`,
			errSubstr: "invalid port 80 for icmp protocol, should be empty",
		},
		{
			desc:  "valid icmp",
			input: `- protocol: icmp`,
			result: []*OpenPort{
				{
					Protocol: "icmp",
				},
			},
		},
		{
			desc: "invalid port",
			input: `- protocol: tcp
  port: 100000`,
			errSubstr: "invalid port",
		},
		{
			desc: "valid tcp",
			input: `- protocol: tcp
  port: 80`,
			result: []*OpenPort{
				{
					Protocol: "tcp",
					Port:     intRef(80),
				},
			},
		},
		{
			desc: "valid multiple protocols",
			input: `- protocol: tcp
  port: 1
- protocol: udp
  port: 2
- protocol: sctp
  port: 3
- protocol: icmp`,
			result: []*OpenPort{
				{
					Protocol: "tcp",
					Port:     intRef(1),
				},
				{
					Protocol: "udp",
					Port:     intRef(2),
				},
				{
					Protocol: "sctp",
					Port:     intRef(3),
				},
				{
					Protocol: "icmp",
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			res, err := UnmarshalUDNOpenPortsAnnotation(map[string]string{
				UDNOpenPortsAnnotationName: tc.input,
			})
			if tc.errSubstr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.result, res)
			}
		})
	}
}

func TestGetPodNADToNetworkMapping(t *testing.T) {
	const (
		attachmentName = "attachment1"
		namespaceName  = "ns1"
		networkName    = "l3-network"
	)

	type testConfig struct {
		desc                          string
		inputNamespace                string
		inputNetConf                  *ovncnitypes.NetConf
		inputPodAnnotations           map[string]string
		expectedError                 error
		expectedIsAttachmentRequested bool
	}

	tests := []testConfig{
		{
			desc:                "Looking for a network *not* present in the pod's attachment requests",
			inputNamespace:      namespaceName,
			inputPodAnnotations: map[string]string{nadv1.NetworkAttachmentAnnot: "[]"},
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer3Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
			},
			expectedIsAttachmentRequested: false,
		},
		{
			desc: "Looking for a network present in the pod's attachment requests",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer3Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: util.GetNADName(namespaceName, attachmentName),
			},
			expectedIsAttachmentRequested: true,
		},
		{
			desc:           "Multiple attachments to the same network in the same pod are not supported",
			inputNamespace: namespaceName,
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer3Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: fmt.Sprintf("%[1]s,%[1]s", util.GetNADName(namespaceName, attachmentName)),
			},
			expectedError: fmt.Errorf("unexpected error: more than one of the same NAD ns1/attachment1 specified for pod ns1/test-pod"),
		},
		{
			desc:           "Attaching to a secondary network to a user defined primary network is not supported",
			inputNamespace: namespaceName,
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l3-network"},
				Topology: types.Layer3Topology,
				Role:     types.NetworkRolePrimary,
				NADName:  util.GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: util.GetNADName(namespaceName, attachmentName),
			},
			expectedError: fmt.Errorf("unexpected primary network \"l3-network\" specified with a NetworkSelectionElement &{Name:attachment1 Namespace:ns1 IPRequest:[] MacRequest: InfinibandGUIDRequest: InterfaceRequest: PortMappingsRequest:[] BandwidthRequest:<nil> CNIArgs:<nil> GatewayRequest:[] IPAMClaimReference:}"),
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := util.NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			if test.inputNetConf.NADName != "" {
				mutableNetInfo := util.NewMutableNetInfo(netInfo)
				mutableNetInfo.AddNADs(test.inputNetConf.NADName)
				netInfo = mutableNetInfo
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   test.inputNamespace,
					Annotations: test.inputPodAnnotations,
				},
			}

			isAttachmentRequested, _, err := GetPodNADToNetworkMapping(pod, netInfo)

			if err != nil {
				g.Expect(err).To(gomega.MatchError(test.expectedError))
			}
			g.Expect(isAttachmentRequested).To(gomega.Equal(test.expectedIsAttachmentRequested))
		})
	}
}

func TestGetPodNADToNetworkMappingWithActiveNetwork(t *testing.T) {
	const (
		attachmentName = "attachment1"
		namespaceName  = "ns1"
		networkName    = "l3-network"
	)

	type testConfig struct {
		desc                             string
		inputNamespace                   string
		inputNetConf                     *ovncnitypes.NetConf
		inputPrimaryUDNConfig            *ovncnitypes.NetConf
		inputPodAnnotations              map[string]string
		expectedError                    error
		expectedIsAttachmentRequested    bool
		expectedNetworkSelectionElements map[string]*nadv1.NetworkSelectionElement
		enablePreconfiguredUDNAddresses  bool
	}

	tests := []testConfig{
		{
			desc: "there isn't a primary UDN",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer3Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: util.GetNADName(namespaceName, attachmentName),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "the netinfo is different from the active network",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer3Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "another-network"},
				Topology: types.Layer3Topology,
				NADName:  util.GetNADName(namespaceName, "another-network"),
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: util.GetNADName(namespaceName, "another-network"),
			},
			expectedIsAttachmentRequested: false,
		},
		{
			desc: "the network configuration for a primary layer2 UDN features allow persistent IPs but the pod does not request it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           types.Layer2Topology,
				NADName:            util.GetNADName(namespaceName, attachmentName),
				Role:               types.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           types.Layer2Topology,
				NADName:            util.GetNADName(namespaceName, attachmentName),
				Role:               types.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: util.GetNADName(namespaceName, "another-network"),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer2 UDN features allow persistent IPs, and the pod requests it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           types.Layer2Topology,
				NADName:            util.GetNADName(namespaceName, attachmentName),
				Role:               types.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           types.Layer2Topology,
				NADName:            util.GetNADName(namespaceName, attachmentName),
				Role:               types.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: util.GetNADName(namespaceName, "another-network"),
				OvnUDNIPAMClaimName:          "the-one-to-the-left-of-the-pony",
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:               "attachment1",
					Namespace:          "ns1",
					IPAMClaimReference: "the-one-to-the-left-of-the-pony",
				},
			},
		},
		{
			desc: "the network configuration for a secondary layer2 UDN features allow persistent IPs and the pod requests it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           types.Layer2Topology,
				NADName:            util.GetNADName(namespaceName, attachmentName),
				Role:               types.NetworkRoleSecondary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           types.Layer2Topology,
				NADName:            util.GetNADName(namespaceName, attachmentName),
				Role:               types.NetworkRoleSecondary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: util.GetNADName(namespaceName, "another-network"),
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer3 UDN features allow persistent IPs and the pod requests it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           types.Layer3Topology,
				NADName:            util.GetNADName(namespaceName, attachmentName),
				Role:               types.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:            cnitypes.NetConf{Name: networkName},
				Topology:           types.Layer3Topology,
				NADName:            util.GetNADName(namespaceName, attachmentName),
				Role:               types.NetworkRolePrimary,
				AllowPersistentIPs: true,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: util.GetNADName(namespaceName, "another-network"),
				OvnUDNIPAMClaimName:          "the-one-to-the-left-of-the-pony",
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:      "attachment1",
					Namespace: "ns1",
				},
			},
		},
		{
			desc: "the network configuration for a primary layer2 UDN receive pod requesting IP, MAC and IPAMClaimRef on default network annotation for it",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer2Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
				Role:     types.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer2Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
				Role:     types.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: util.GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:         `[{"namespace": "ovn-kubernetes", "name": "default", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff", "ipam-claim-reference": "my-ipam-claim"}]`,
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:               "attachment1",
					Namespace:          "ns1",
					IPRequest:          []string{"192.168.0.3/24", "fda6::3/48"},
					MacRequest:         "aa:bb:cc:dd:ee:ff",
					IPAMClaimReference: "my-ipam-claim",
				},
			},
			enablePreconfiguredUDNAddresses: true,
		},
		{
			desc: "the network configuration for a primary layer2 UDN receive pod requesting IP and MAC on default network annotation for it, but with unexpected namespace",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer2Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
				Role:     types.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer2Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
				Role:     types.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				DefNetworkAnnotation: `[{"namespace": "other-namespace", "name": "default", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff"}]`,
			},
			enablePreconfiguredUDNAddresses: true,
			expectedError:                   fmt.Errorf(`unexpected default NSE namespace "other-namespace", expected "ovn-kubernetes"`),
		},
		{
			desc: "the network configuration for a primary layer2 UDN receive pod requesting IP and MAC on default network annotation for it, but with unexpected name",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer2Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
				Role:     types.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer2Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
				Role:     types.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				DefNetworkAnnotation: `[{"namespace": "ovn-kubernetes", "name": "unexpected-name", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff"}]`,
			},
			enablePreconfiguredUDNAddresses: true,
			expectedError:                   fmt.Errorf(`unexpected default NSE name "unexpected-name", expected "default"`),
		},

		{
			desc: "default-network ips and mac is is ignored for Layer3 topology",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer3Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
				Role:     types.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer3Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
				Role:     types.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: util.GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:         `[{"namespace": "ovn-kubernetes", "name": "default", "ips": ["192.168.0.3/24", "fda6::3/48"], "mac": "aa:bb:cc:dd:ee:ff"}]`,
			},
			expectedIsAttachmentRequested: true,
			expectedNetworkSelectionElements: map[string]*nadv1.NetworkSelectionElement{
				"ns1/attachment1": {
					Name:       "attachment1",
					Namespace:  "ns1",
					IPRequest:  nil,
					MacRequest: "",
				},
			},
			enablePreconfiguredUDNAddresses: true,
		},
		{
			desc: "default-network with bad format",
			inputNetConf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer2Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
				Role:     types.NetworkRolePrimary,
			},
			inputPrimaryUDNConfig: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: networkName},
				Topology: types.Layer2Topology,
				NADName:  util.GetNADName(namespaceName, attachmentName),
				Role:     types.NetworkRolePrimary,
			},
			inputPodAnnotations: map[string]string{
				nadv1.NetworkAttachmentAnnot: util.GetNADName(namespaceName, "another-network"),
				DefNetworkAnnotation:         `[{"foo}`,
			},
			enablePreconfiguredUDNAddresses: true,
			expectedError:                   fmt.Errorf(`failed getting default-network annotation for pod "/test-pod": %w`, fmt.Errorf(`GetK8sPodDefaultNetwork: failed to parse CRD object: parsePodNetworkAnnotation: failed to parse pod Network Attachment Selection Annotation JSON format: unexpected end of JSON input`)),
		},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			g := gomega.NewWithT(t)

			t.Cleanup(func() {
				_ = config.PrepareTestConfig()
			})

			// Set custom network config based on test requirements
			config.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses = test.enablePreconfiguredUDNAddresses
			if test.enablePreconfiguredUDNAddresses {
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			}

			netInfo, err := util.NewNetInfo(test.inputNetConf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			if test.inputNetConf.NADName != "" {
				mutableNetInfo := util.NewMutableNetInfo(netInfo)
				mutableNetInfo.AddNADs(test.inputNetConf.NADName)
				netInfo = mutableNetInfo
			}

			var primaryUDNNetInfo util.NetInfo
			if test.inputPrimaryUDNConfig != nil {
				primaryUDNNetInfo, err = util.NewNetInfo(test.inputPrimaryUDNConfig)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				if test.inputPrimaryUDNConfig.NADName != "" {
					mutableNetInfo := util.NewMutableNetInfo(primaryUDNNetInfo)
					mutableNetInfo.AddNADs(test.inputPrimaryUDNConfig.NADName)
					primaryUDNNetInfo = mutableNetInfo
				}
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   test.inputNamespace,
					Annotations: test.inputPodAnnotations,
				},
			}

			isAttachmentRequested, networkSelectionElements, err := GetPodNADToNetworkMappingWithActiveNetwork(
				pod,
				netInfo,
				primaryUDNNetInfo,
			)

			if test.expectedError != nil {
				g.Expect(err).To(gomega.HaveOccurred(), "unexpected success operation, epecting error")
				g.Expect(err).To(gomega.MatchError(test.expectedError))
			} else {
				g.Expect(err).ToNot(gomega.HaveOccurred())
				g.Expect(isAttachmentRequested).To(gomega.Equal(test.expectedIsAttachmentRequested))
				g.Expect(networkSelectionElements).To(gomega.Equal(test.expectedNetworkSelectionElements))
			}
		})
	}
}
