package routeadvertisements

import (
	"reflect"
	"testing"

	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
)

func TestApplyEVPNConfig(t *testing.T) {
	advertiseAll := frrtypes.VNIAdvertisementAll

	tests := []struct {
		name     string
		routers  []frrtypes.Router
		selected *selectedNetworks
		want     []frrtypes.Router
	}{
		{
			name: "MAC-VRF without route target",
			routers: []frrtypes.Router{
				{ASN: 65000, Neighbors: []frrtypes.Neighbor{{Address: "192.168.1.1", AddressFamilies: []frrtypes.AddressFamily{frrtypes.AddressFamilyUnicast}}}},
			},
			selected: &selectedNetworks{
				macVRFConfigs: []*vrfConfig{
					{VNI: 1000},
				},
			},
			want: []frrtypes.Router{
				{
					ASN: 65000,
					Neighbors: []frrtypes.Neighbor{
						{
							Address:         "192.168.1.1",
							AddressFamilies: []frrtypes.AddressFamily{frrtypes.AddressFamilyEVPN, frrtypes.AddressFamilyUnicast},
						},
					},
					EVPN: &frrtypes.EVPNConfig{
						AdvertiseVNIs: &advertiseAll,
					},
				},
			},
		},
		{
			name: "MAC-VRF with route target",
			routers: []frrtypes.Router{
				{ASN: 65000, Neighbors: []frrtypes.Neighbor{{Address: "192.168.1.1", AddressFamilies: []frrtypes.AddressFamily{frrtypes.AddressFamilyUnicast}}}},
			},
			selected: &selectedNetworks{
				macVRFConfigs: []*vrfConfig{
					{VNI: 1000, RouteTarget: "65000:1000"},
				},
			},
			want: []frrtypes.Router{
				{
					ASN: 65000,
					Neighbors: []frrtypes.Neighbor{
						{
							Address:         "192.168.1.1",
							AddressFamilies: []frrtypes.AddressFamily{frrtypes.AddressFamilyEVPN, frrtypes.AddressFamilyUnicast},
						},
					},
					EVPN: &frrtypes.EVPNConfig{
						AdvertiseVNIs: &advertiseAll,
						L2VNIs: []frrtypes.L2VNI{
							{VNI: frrtypes.VNI{
								VNI:       1000,
								ImportRTs: []frrtypes.ImportRouteTarget{"65000:1000"},
								ExportRTs: []frrtypes.ExportRouteTarget{"65000:1000"},
							}},
						},
					},
				},
			},
		},
		{
			name: "IP-VRF IPv6",
			routers: []frrtypes.Router{
				{ASN: 65000, Neighbors: []frrtypes.Neighbor{{Address: "192.168.1.1", AddressFamilies: []frrtypes.AddressFamily{frrtypes.AddressFamilyUnicast}}}},
				{ASN: 65000, VRF: "blue", Prefixes: []string{"fd00::/64"}},
			},
			selected: &selectedNetworks{
				ipVRFConfigs: []*ipVRFConfig{
					{
						vrfConfig: vrfConfig{VNI: 2000, RouteTarget: "65000:2000"},
						VRFName:   "blue",
						HasIPv6:   true,
					},
				},
			},
			want: []frrtypes.Router{
				{
					ASN: 65000,
					Neighbors: []frrtypes.Neighbor{
						{
							Address:         "192.168.1.1",
							AddressFamilies: []frrtypes.AddressFamily{frrtypes.AddressFamilyEVPN, frrtypes.AddressFamilyUnicast},
						},
					},
					EVPN: &frrtypes.EVPNConfig{
						AdvertiseVNIs: &advertiseAll,
					},
				},
				{
					ASN:      65000,
					VRF:      "blue",
					Prefixes: []string{"fd00::/64"},
					EVPN: &frrtypes.EVPNConfig{
						L3VNI: &frrtypes.L3VNI{
							VNI: frrtypes.VNI{
								VNI:       2000,
								ImportRTs: []frrtypes.ImportRouteTarget{"65000:2000"},
								ExportRTs: []frrtypes.ExportRouteTarget{"65000:2000"},
							},
							AdvertisePrefixes: []frrtypes.AdvertisePrefixType{frrtypes.AdvertisePrefixUnicast},
						},
					},
				},
			},
		},
		{
			name: "MAC-VRF and IP-VRF combined",
			routers: []frrtypes.Router{
				{ASN: 65000, Neighbors: []frrtypes.Neighbor{
					{Address: "192.168.1.1", AddressFamilies: []frrtypes.AddressFamily{frrtypes.AddressFamilyUnicast}},
					{Address: "192.168.1.2", AddressFamilies: []frrtypes.AddressFamily{frrtypes.AddressFamilyUnicast}},
				}},
				{ASN: 65000, VRF: "blue", Prefixes: []string{"10.0.0.0/24"}},
			},
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
			want: []frrtypes.Router{
				{
					ASN: 65000,
					Neighbors: []frrtypes.Neighbor{
						{
							Address:         "192.168.1.1",
							AddressFamilies: []frrtypes.AddressFamily{frrtypes.AddressFamilyEVPN, frrtypes.AddressFamilyUnicast},
						},
						{
							Address:         "192.168.1.2",
							AddressFamilies: []frrtypes.AddressFamily{frrtypes.AddressFamilyEVPN, frrtypes.AddressFamilyUnicast},
						},
					},
					EVPN: &frrtypes.EVPNConfig{
						AdvertiseVNIs: &advertiseAll,
						L2VNIs: []frrtypes.L2VNI{
							{VNI: frrtypes.VNI{
								VNI:       1000,
								ImportRTs: []frrtypes.ImportRouteTarget{"65000:1000"},
								ExportRTs: []frrtypes.ExportRouteTarget{"65000:1000"},
							}},
						},
					},
				},
				{
					ASN:      65000,
					VRF:      "blue",
					Prefixes: []string{"10.0.0.0/24"},
					EVPN: &frrtypes.EVPNConfig{
						L3VNI: &frrtypes.L3VNI{
							VNI: frrtypes.VNI{
								VNI:       2000,
								ImportRTs: []frrtypes.ImportRouteTarget{"65000:2000"},
								ExportRTs: []frrtypes.ExportRouteTarget{"65000:2000"},
							},
							AdvertisePrefixes: []frrtypes.AdvertisePrefixType{frrtypes.AdvertisePrefixUnicast},
						},
					},
				},
			},
		},
		{
			name: "EVPN applied to all routers in same VRF",
			routers: []frrtypes.Router{
				{ASN: 65000, Neighbors: []frrtypes.Neighbor{{Address: "192.168.1.1", AddressFamilies: []frrtypes.AddressFamily{frrtypes.AddressFamilyUnicast}}}, Prefixes: []string{"10.0.0.0/24"}},
				{ASN: 65000, Imports: []frrtypes.Import{{VRF: "blue"}}},
			},
			selected: &selectedNetworks{
				macVRFConfigs: []*vrfConfig{{VNI: 1000}},
			},
			want: []frrtypes.Router{
				{
					ASN: 65000,
					Neighbors: []frrtypes.Neighbor{
						{
							Address:         "192.168.1.1",
							AddressFamilies: []frrtypes.AddressFamily{frrtypes.AddressFamilyEVPN, frrtypes.AddressFamilyUnicast},
						},
					},
					Prefixes: []string{"10.0.0.0/24"},
					EVPN:     &frrtypes.EVPNConfig{AdvertiseVNIs: &advertiseAll},
				},
				{
					ASN:     65000,
					Imports: []frrtypes.Import{{VRF: "blue"}},
				},
			},
		},
		{
			name:     "no EVPN configs returns routers unchanged",
			routers:  []frrtypes.Router{{ASN: 65000}},
			selected: &selectedNetworks{},
			want:     []frrtypes.Router{{ASN: 65000}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routersByVRF := map[string][]*frrtypes.Router{}
			for i := range tt.routers {
				routersByVRF[tt.routers[i].VRF] = append(routersByVRF[tt.routers[i].VRF], &tt.routers[i])
			}
			applyEVPNConfig(routersByVRF, tt.selected)
			if !reflect.DeepEqual(tt.routers, tt.want) {
				t.Errorf("applyEVPNConfig() mismatch\nGot:  %+v\nWant: %+v", tt.routers, tt.want)
			}
		})
	}
}
