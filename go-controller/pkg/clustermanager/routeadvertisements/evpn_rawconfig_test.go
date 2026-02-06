package routeadvertisements

import (
	"testing"
)

func TestGenerateEVPNRawConfig(t *testing.T) {
	tests := []struct {
		name      string
		selected  *selectedNetworks
		asn       uint32
		neighbors []string
		want      string
	}{
		{
			name: "MAC-VRF without route target",
			selected: &selectedNetworks{
				macVRFConfigs: []*vrfConfig{
					{VNI: 1000},
				},
			},
			asn:       65000,
			neighbors: []string{"192.168.1.1"},
			want: `router bgp 65000
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  advertise-all-vni
 exit-address-family
exit
!
`,
		},
		{
			name: "MAC-VRF with route target",
			selected: &selectedNetworks{
				macVRFConfigs: []*vrfConfig{
					{VNI: 1000, RouteTarget: "65000:1000"},
				},
			},
			asn:       65000,
			neighbors: []string{"192.168.1.1"},
			want: `router bgp 65000
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  advertise-all-vni
  vni 1000
   route-target import 65000:1000
   route-target export 65000:1000
  exit-vni
 exit-address-family
exit
!
`,
		},
		{
			name: "IP-VRF IPv6",
			selected: &selectedNetworks{
				ipVRFConfigs: []*ipVRFConfig{
					{
						vrfConfig: vrfConfig{VNI: 2000, RouteTarget: "65000:2000"},
						VRFName:   "blue",
						HasIPv6:   true,
					},
				},
			},
			asn:       65000,
			neighbors: []string{"192.168.1.1"},
			want: `router bgp 65000
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv6 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
		},
		{
			name: "IP-VRF dual stack",
			selected: &selectedNetworks{
				ipVRFConfigs: []*ipVRFConfig{
					{
						vrfConfig: vrfConfig{VNI: 2000, RouteTarget: "65000:2000"},
						VRFName:   "blue",
						HasIPv4:   true,
						HasIPv6:   true,
					},
				},
			},
			asn:       65000,
			neighbors: []string{"192.168.1.1"},
			want: `router bgp 65000
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  advertise ipv6 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
		},
		{
			name: "MAC-VRF and IP-VRF combined",
			selected: &selectedNetworks{
				macVRFConfigs: []*vrfConfig{
					{VNI: 1000, RouteTarget: "65000:1000"},
				},
				ipVRFConfigs: []*ipVRFConfig{
					{
						vrfConfig: vrfConfig{VNI: 2000, RouteTarget: "65000:2000"},
						VRFName:   "blue",
						HasIPv4:   true,
					},
				},
			},
			asn:       65000,
			neighbors: []string{"192.168.1.1", "192.168.1.2"},
			want: `router bgp 65000
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.2 activate
  advertise-all-vni
  vni 1000
   route-target import 65000:1000
   route-target export 65000:1000
  exit-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vrfASNs := map[string]uint32{}
			for _, cfg := range tt.selected.ipVRFConfigs {
				if cfg.VRFName != "" {
					vrfASNs[cfg.VRFName] = tt.asn
				}
			}
			got := generateEVPNRawConfig(tt.selected, tt.asn, tt.neighbors, vrfASNs)
			if got != tt.want {
				t.Errorf("generateEVPNRawConfig() mismatch\nGot:\n%s\nWant:\n%s", got, tt.want)
			}
		})
	}
}
