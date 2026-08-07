// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

// This is a list of options used for OVN operations.
// Started with adding only some of them, feel free to continue extending this list.
// Eventually we expect to have no string options in the code.
const (
	// RequestedTnlKey can be used by LogicalSwitch, LogicalSwitchPort, LogicalRouter and LogicalRouterPort
	// for distributed switches/routers
	RequestedTnlKey = "requested-tnl-key"
	// RequestedChassis can be used by LogicalSwitchPort and LogicalRouterPort.
	// It specifies the chassis (by name or hostname) that is allowed to bind this port.
	RequestedChassis = "requested-chassis"
	// RouterPort can be used by LogicalSwitchPort to specify a connection to a logical router.
	RouterPort = "router-port"
	// GatewayMTU can be used by LogicalRouterPort to specify the MTU for the gateway port.
	// If set, logical flows will be added to router pipeline to check packet length.
	GatewayMTU = "gateway_mtu"
	// ForceFdbLookup can be used by LogicalSwitchPort. If set to true, configured
	// MAC addresses are not installed in the L2 lookup table but are learnt and
	// stored in the FDB table instead. Only takes effect when the port is of the
	// default type (empty "type" column) and its "addresses" column contains
	// "unknown".
	ForceFdbLookup = "force_fdb_lookup"
)
