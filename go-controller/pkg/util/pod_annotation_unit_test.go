// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
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
		{
			desc:           "PodAnnotation instance when IPAMMode is set to dhcp",
			inpPodAnnot:    PodAnnotation{IPAMMode: "dhcp"},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"","ipam_mode":"dhcp"}}`},
		},
		{
			desc:           "ipam_mode is omitted when IPAMMode is not set",
			inpPodAnnot:    PodAnnotation{Role: types.NetworkRoleSecondary},
			expectedOutput: map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":null,"mac_address":"","role":"secondary"}}`},
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

func TestUnmarshalPodAnnotationIPAMMode(t *testing.T) {
	inpAnnotMap := map[string]string{"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["192.168.0.5/24"],"mac_address":"0a:58:fd:98:00:01","ip_address":"192.168.0.5/24","ipam_mode":"dhcp"}}`}
	res, e := UnmarshalPodAnnotation(inpAnnotMap, "default")
	require.NoError(t, e)
	require.NotNil(t, res)
	assert.Equal(t, "dhcp", res.IPAMMode)
}

// TestMarshalPodAnnotationDHCPOverwrite covers the override guard for
// entries whose IPs are learned from an external DHCP server (ipam_mode=dhcp):
// ovnkube-node fills the IPs in during CNI ADD after the entry was created
// without IPs, and may replace them on a repeat CNI ADD (sandbox recreation)
// when the DHCP server hands out a new lease. Non-DHCP entries must keep the
// strict no-override behavior.
func TestMarshalPodAnnotationDHCPOverwrite(t *testing.T) {
	const nadKey = "foo-ns/localnet-nad"
	mac := ovntest.MustParseMAC("0a:58:fd:98:00:01")

	newAnnotations := func(entry string) map[string]string {
		return map[string]string{"k8s.ovn.org/pod-networks": `{"` + nadKey + `":` + entry + `}`}
	}
	dhcpPodInfo := func(macStr, ip string) *PodAnnotation {
		podInfo := &PodAnnotation{
			MAC:      ovntest.MustParseMAC(macStr),
			IPAMMode: "dhcp",
			Role:     types.NetworkRoleSecondary,
		}
		if ip != "" {
			podInfo.IPs = []*net.IPNet{ovntest.MustParseIPNet(ip)}
		}
		return podInfo
	}

	t.Run("fills IPs into a DHCP entry created without IPs", func(t *testing.T) {
		annotations := newAnnotations(`{"mac_address":"` + mac.String() + `","role":"secondary","ipam_mode":"dhcp"}`)
		updated, err := MarshalPodAnnotation(annotations, dhcpPodInfo(mac.String(), "10.1.192.102/24"), nadKey)
		require.NoError(t, err)
		assert.Contains(t, updated["k8s.ovn.org/pod-networks"], "10.1.192.102/24")
	})

	t.Run("overwrites the IPs of a DHCP entry on a new lease", func(t *testing.T) {
		annotations := newAnnotations(`{"ip_addresses":["10.1.192.102/24"],"mac_address":"` + mac.String() + `","role":"secondary","ipam_mode":"dhcp"}`)
		updated, err := MarshalPodAnnotation(annotations, dhcpPodInfo(mac.String(), "10.1.192.150/24"), nadKey)
		require.NoError(t, err)
		assert.Contains(t, updated["k8s.ovn.org/pod-networks"], "10.1.192.150/24")
		assert.NotContains(t, updated["k8s.ovn.org/pod-networks"], "10.1.192.102/24")
	})

	t.Run("re-marshaling a DHCP entry with unchanged IPs is idempotent", func(t *testing.T) {
		annotations := newAnnotations(`{"ip_addresses":["10.1.192.102/24"],"mac_address":"` + mac.String() + `","role":"secondary","ipam_mode":"dhcp"}`)
		updated, err := MarshalPodAnnotation(annotations, dhcpPodInfo(mac.String(), "10.1.192.102/24"), nadKey)
		require.NoError(t, err)
		assert.Contains(t, updated["k8s.ovn.org/pod-networks"], "10.1.192.102/24")
	})

	t.Run("still refuses to change the IPs of a non-DHCP entry", func(t *testing.T) {
		annotations := newAnnotations(`{"ip_addresses":["10.1.192.102/24"],"mac_address":"` + mac.String() + `","role":"secondary"}`)
		podInfo := dhcpPodInfo(mac.String(), "10.1.192.150/24")
		podInfo.IPAMMode = ""
		_, err := MarshalPodAnnotation(annotations, podInfo, nadKey)
		require.ErrorIs(t, err, ErrOverridePodIPs)
	})
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
		networkInfo NetInfo
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
			networkInfo: &DefaultNetInfo{},
			outExp:      []net.IP{ovntest.MustParseIP("192.168.0.1")},
		},
		{
			desc:        "test when pod.status.PodIP is empty",
			inpPod:      &corev1.Pod{},
			networkInfo: &DefaultNetInfo{},
			errMatch:    ErrNoPodIPFound,
		},
		{
			desc: "test when pod.status.PodIP is non-empty",
			inpPod: &corev1.Pod{
				Status: corev1.PodStatus{
					PodIP: "192.168.1.15",
				},
			},
			networkInfo: &DefaultNetInfo{},
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
			networkInfo: &DefaultNetInfo{},
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
			networkInfo: &DefaultNetInfo{},
			errMatch:    ErrNoPodIPFound,
		},
		{
			desc: "test when pod annotation is non-nil for a secondary network",
			inpPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.v1.cni.cncf.io/networks": fmt.Sprintf(`[{"name": %q, "namespace": %q}]`, secondaryNetworkName, namespace),
						"k8s.ovn.org/pod-networks":    fmt.Sprintf(`{%q:{"ip_addresses":["%s/24"],"mac_address":"0a:58:fd:98:00:01"}}`, GetNADName(namespace, secondaryNetworkName), secondaryNetworkIPAddr),
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
			var resolver func(nadKey string) string
			if tc.networkInfo.IsUserDefinedNetwork() {
				expectedNADKey := GetNADName(namespace, secondaryNetworkName)
				resolver = func(nadKey string) string {
					if nadKey == expectedNADKey {
						return tc.networkInfo.GetNetworkName()
					}
					return ""
				}
			}
			res1, e := GetPodIPsOfNetwork(tc.inpPod, tc.networkInfo, resolver)
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
				res2, e := GetPodCIDRsWithFullMask(tc.inpPod, tc.networkInfo, resolver)
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
						Mask: GetIPFullMask(expectedIP),
					}
					assert.Equal(t, []*net.IPNet{&ipNet}, res2)
				}
			}
		})
	}
}

func newDummyNetInfo(namespace, networkName string) NetInfo {
	netInfo, _ := newLayer2NetConfInfo(&ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{Name: networkName},
	})
	mutableNetInfo := NewMutableNetInfo(netInfo)
	mutableNetInfo.AddNADs(GetNADName(namespace, networkName))
	return mutableNetInfo
}

func newPrimaryNetInfo(namespace, networkName string) NetInfo {
	netInfo, _ := newLayer3NetConfInfo(&ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{Name: networkName},
		Role:    types.NetworkRolePrimary,
	})
	mutableNetInfo := NewMutableNetInfo(netInfo)
	mutableNetInfo.AddNADs(GetNADName(namespace, networkName))
	return mutableNetInfo
}

func TestSecondaryNetworkPodIPs(t *testing.T) {
	nadResolver := func(nadKey string) string {
		switch nadKey {
		case "ns1/mynet":
			return "mynet"
		case "ns2/mynet":
			return "mynet"
		default:
			return ""
		}
	}

	tests := []struct {
		desc        string
		annotations map[string]string
		netInfo     NetInfo
		wantIPs     []net.IP
		wantErr     bool
	}{
		{
			desc: "primary network: matching NAD key returns IPs",
			annotations: map[string]string{
				types.OvnPodAnnotationName: `{"ns1/mynet":{"ip_addresses":["10.0.0.5/24"],"mac_address":"0a:58:0a:00:00:05"}}`,
			},
			netInfo: newPrimaryNetInfo("ns1", "mynet"),
			wantIPs: []net.IP{net.ParseIP("10.0.0.5")},
		},
		{
			desc: "primary network: unrelated NAD key returns no IPs",
			annotations: map[string]string{
				types.OvnPodAnnotationName: `{"ns1/othernet":{"ip_addresses":["10.0.0.5/24"],"mac_address":"0a:58:0a:00:00:05"}}`,
			},
			netInfo: newPrimaryNetInfo("ns1", "mynet"),
			wantIPs: []net.IP{},
		},
		{
			desc: "primary network: multiple matching NADs returns all IPs",
			annotations: map[string]string{
				types.OvnPodAnnotationName: `{"ns1/mynet":{"ip_addresses":["10.0.0.5/24"],"mac_address":"0a:58:0a:00:00:05"},"ns2/mynet":{"ip_addresses":["10.0.1.5/24"],"mac_address":"0a:58:0a:00:01:05"}}`,
			},
			netInfo: newPrimaryNetInfo("ns1", "mynet"),
			wantIPs: []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("10.0.1.5")},
		},
		{
			desc: "primary network: bad annotation returns error",
			annotations: map[string]string{
				types.OvnPodAnnotationName: `not-json`,
			},
			netInfo: newPrimaryNetInfo("ns1", "mynet"),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   "ns1",
					Annotations: tc.annotations,
				},
			}
			ips, err := SecondaryNetworkPodIPs(pod, tc.netInfo, nadResolver)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.wantIPs, ips)
		})
	}
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

// TestGetK8sPodNetworkSelection pins the indexed-NAD-key derivation to the
// GetIndexedNADKey convention: the n-th selection of the same NAD resolves to
// nadName[/n], so every attachment of a NAD attached multiple times finds its
// own selection element (and with it its own MacRequest).
func TestGetK8sPodNetworkSelection(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bar-pod",
			Namespace: "foo-ns",
			Annotations: map[string]string{
				nadapi.NetworkAttachmentAnnot: `[{"name":"dhcpnet","namespace":"default","mac":"02:00:00:00:00:01"},` +
					`{"name":"other","namespace":"default"},` +
					`{"name":"dhcpnet","namespace":"default","mac":"02:00:00:00:00:02"}]`,
			},
		},
	}

	nse, err := GetK8sPodNetworkSelection(pod, "default/dhcpnet")
	require.NoError(t, err)
	require.NotNil(t, nse)
	assert.Equal(t, "02:00:00:00:00:01", nse.MacRequest)

	nse, err = GetK8sPodNetworkSelection(pod, "default/dhcpnet/1")
	require.NoError(t, err)
	require.NotNil(t, nse)
	assert.Equal(t, "02:00:00:00:00:02", nse.MacRequest)

	nse, err = GetK8sPodNetworkSelection(pod, "default/absent")
	require.NoError(t, err)
	assert.Nil(t, nse)
}
