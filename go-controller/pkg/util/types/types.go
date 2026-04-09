package types

// BridgeVlanOptions defines the options for a VLAN on a Linux bridge interface.
type BridgeVlanOptions struct {
	PVID     bool
	Untagged bool
	Self     bool
	Master   bool
}

