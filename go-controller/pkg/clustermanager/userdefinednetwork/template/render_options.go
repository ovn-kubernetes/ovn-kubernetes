package template

// RenderOption is a functional option for configuring NAD rendering.
type RenderOption func(*RenderOptions)

// RenderOptions contains optional configuration for NAD rendering.
type RenderOptions struct {
	EVPNVIDs *EVPNVIDs
}

// EVPNVIDs contains pre-allocated VLAN IDs for EVPN MAC-VRF and IP-VRF.
type EVPNVIDs struct {
	// MACVRFVID is the VLAN ID for the MAC-VRF (Layer 2 EVPN).
	// A value of 0 means no VID is allocated for MAC-VRF.
	MACVRFVID int
	// IPVRFVID is the VLAN ID for the IP-VRF (Layer 3 EVPN).
	// A value of 0 means no VID is allocated for IP-VRF.
	IPVRFVID int
}

// WithEVPNVIDs returns a RenderOption that sets the EVPN VIDs for rendering.
func WithEVPNVIDs(macVRFVID, ipVRFVID int) RenderOption {
	return func(opts *RenderOptions) {
		opts.EVPNVIDs = &EVPNVIDs{
			MACVRFVID: macVRFVID,
			IPVRFVID:  ipVRFVID,
		}
	}
}

// applyOptions applies the given functional options and returns the resulting RenderOptions.
// Nil options in the slice are safely skipped to prevent panics.
func applyOptions(opts []RenderOption) *RenderOptions {
	options := &RenderOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(options)
		}
	}
	return options
}
