package userdefinednetwork

import (
	"encoding/json"
	"strings"

	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// evpnVIDConfig is a minimal struct for extracting VID information from NAD config.
// This struct intentionally mirrors only the fields needed for VID recovery,
// minimizing JSON unmarshalling overhead.
type evpnVIDConfig struct {
	EVPNConfig *struct {
		MACVRF *struct {
			VID int `json:"vid"`
		} `json:"macVRF"`
		IPVRF *struct {
			VID int `json:"vid"`
		} `json:"ipVRF"`
	} `json:"evpnConfig"`
}

// parseEVPNVIDs extracts MAC-VRF and IP-VRF VIDs from a NAD config JSON string.
//
// Returns (0, 0, nil) for non-EVPN configs or configs without VIDs.
// Returns an error only if the JSON is malformed.
func parseEVPNVIDs(config string) (macVID, ipVID int, err error) {
	if config == "" {
		return 0, 0, nil
	}

	// Fast path: skip configs that definitely don't have EVPN transport.
	// We check for the literal `"evpn"` string, which handles normal JSON encoding.
	// However, JSON allows unicode escapes (e.g., `\u0065` for 'e'), so if the config
	// contains any `\u` sequences, we skip the fast path and do a full parse.
	// False positives are harmless (JSON parse returns correct result).
	hasUnicodeEscapes := strings.Contains(config, `\u`)
	if !hasUnicodeEscapes && !strings.Contains(config, `"`+ovntypes.TransportEVPN+`"`) {
		return 0, 0, nil
	}

	// Slow path: parse EVPN config to extract VIDs
	var cfg evpnVIDConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return 0, 0, err
	}

	if cfg.EVPNConfig == nil {
		return 0, 0, nil
	}
	if cfg.EVPNConfig.MACVRF != nil {
		macVID = cfg.EVPNConfig.MACVRF.VID
	}
	if cfg.EVPNConfig.IPVRF != nil {
		ipVID = cfg.EVPNConfig.IPVRF.VID
	}
	return macVID, ipVID, nil
}
