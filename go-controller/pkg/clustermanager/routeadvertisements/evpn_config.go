package routeadvertisements

import (
	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"

	"k8s.io/apimachinery/pkg/util/sets"
)

// applyEVPNConfig modifies routers grouped by VRF to add EVPN configuration
// using the typed frr-k8s API. The same EVPN config is applied to all routers
// within a VRF to guarantee a successful merge by frr-k8s.
//
// For default VRF routers (key ""):
//   - Activates EVPN address family on neighbors
//   - Sets AdvertiseVNIs to advertise all VNIs
//   - Adds L2VNIs for MAC-VRF configs with route targets
//
// For VRF routers:
//   - Sets L3VNI with VNI, route targets and advertise unicast prefixes
func applyEVPNConfig(routersByVRF map[string][]*frrtypes.Router, selected *selectedNetworks) {
	if len(selected.macVRFConfigs) == 0 && len(selected.ipVRFConfigs) == 0 {
		return
	}

	// Handle default VRF router EVPN config
	applyDefaultVRFEVPNConfig(routersByVRF, selected.macVRFConfigs)

	// Handle VRF routers EVPN config (L3VNI)
	for _, cfg := range selected.ipVRFConfigs {
		if len(routersByVRF[cfg.VRFName]) == 0 {
			continue
		}
		applyVRFEVPNConfig(routersByVRF, cfg)
	}
}

// applyDefaultVRFEVPNConfig adds EVPN configuration to all default VRF routers.
func applyDefaultVRFEVPNConfig(routersByVRF map[string][]*frrtypes.Router, macVRFs []*vrfConfig) {
	var evpnConfig *frrtypes.EVPNConfig
	buildEVPNConfig := func() *frrtypes.EVPNConfig {
		if evpnConfig != nil {
			return evpnConfig
		}
		advertiseAll := frrtypes.VNIAdvertisementAll
		evpnConfig = &frrtypes.EVPNConfig{
			AdvertiseVNIs: &advertiseAll,
		}
		for _, cfg := range macVRFs {
			if cfg.RouteTarget == "" {
				continue
			}
			evpnConfig.L2VNIs = append(evpnConfig.L2VNIs, frrtypes.L2VNI{
				VNI: frrtypes.VNI{
					VNI:       uint32(cfg.VNI),
					ImportRTs: []frrtypes.ImportRouteTarget{frrtypes.ImportRouteTarget(cfg.RouteTarget)},
					ExportRTs: []frrtypes.ExportRouteTarget{frrtypes.ExportRouteTarget(cfg.RouteTarget)},
				},
			})
		}
		return evpnConfig
	}

	for _, router := range routersByVRF[""] {
		if len(router.Neighbors) == 0 {
			continue
		}
		router.EVPN = buildEVPNConfig()

		// Activate EVPN address family on existing neighbors
		for i := range router.Neighbors {
			n := &router.Neighbors[i]
			f := sets.New(n.AddressFamilies...)
			f.Insert(frrtypes.AddressFamilyEVPN)
			n.AddressFamilies = sets.List(f)
		}
	}
}

// applyVRFEVPNConfig adds L3VNI EVPN configuration to all routers for a VRF.
func applyVRFEVPNConfig(routersByVRF map[string][]*frrtypes.Router, cfg *ipVRFConfig) {
	l3vni := &frrtypes.L3VNI{
		VNI: frrtypes.VNI{
			VNI: uint32(cfg.VNI),
		},
		AdvertisePrefixes: []frrtypes.AdvertisePrefixType{frrtypes.AdvertisePrefixUnicast},
	}
	if cfg.RouteTarget != "" {
		l3vni.ImportRTs = []frrtypes.ImportRouteTarget{frrtypes.ImportRouteTarget(cfg.RouteTarget)}
		l3vni.ExportRTs = []frrtypes.ExportRouteTarget{frrtypes.ExportRouteTarget(cfg.RouteTarget)}
	}

	for _, router := range routersByVRF[cfg.VRFName] {
		if router.EVPN == nil {
			router.EVPN = &frrtypes.EVPNConfig{}
		}
		router.EVPN.L3VNI = l3vni
		// Normalize empty neighbor slices to nil so VRF routers
		// that were matched without neighbors don't carry empty slices.
		if len(router.Neighbors) == 0 {
			router.Neighbors = nil
		}
	}
}
