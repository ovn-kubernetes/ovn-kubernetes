package userdefinednetwork

import (
	"encoding/json"
)

// evpnVIDConfig is a minimal struct for extracting VID information from NAD config.
// This struct mirrors the VID-related fields from types.EVPNConfig and types.VRFConfig
// (see pkg/cni/types/types.go), extracting only what's needed for VID recovery
// to minimize JSON unmarshalling overhead.
type evpnVIDConfig struct {
	EVPN *struct {
		MACVRF *struct {
			VID int `json:"vid"`
		} `json:"macVRF"`
		IPVRF *struct {
			VID int `json:"vid"`
		} `json:"ipVRF"`
	} `json:"evpn"`
}

// parseEVPNVIDs extracts MAC-VRF and IP-VRF VIDs from a NAD config JSON string.
//
// Returns (0, 0, nil) for non-EVPN configs or configs without VIDs.
// Returns an error only if the JSON is malformed.
func parseEVPNVIDs(config string) (macVID, ipVID int, err error) {
	if config == "" {
		return 0, 0, nil
	}

	var cfg evpnVIDConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return 0, 0, err
	}

	if cfg.EVPN == nil {
		return 0, 0, nil
	}
	if cfg.EVPN.MACVRF != nil {
		macVID = cfg.EVPN.MACVRF.VID
	}
	if cfg.EVPN.IPVRF != nil {
		ipVID = cfg.EVPN.IPVRF.VID
	}
	return macVID, ipVID, nil
}
