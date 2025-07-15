package types

const (
	// CtMarkOVN is the conntrack mark value for OVN traffic
	CtMarkOVN = "0x1"
	// OvsLocalPort is the name of the OVS bridge local port
	OvsLocalPort = "LOCAL"
	// DefaultOpenFlowCookie identifies default open flow rules added to the host OVS bridge.
	// The hex number 0xdeff105, aka defflos, is meant to sound like default flows.
	DefaultOpenFlowCookie = "0xdeff105"
	// OutputPortDrop is used to signify that there is no output port for an openflow action and the
	// rendered action should result in a drop
	OutputPortDrop = "output-port-drop"
	// OvnKubeNodeSNATMark is used to mark packets that need to be SNAT-ed to nodeIP for
	// traffic originating from egressIP and egressService controlled pods towards other nodes in the cluster.
	OvnKubeNodeSNATMark = "0x3f0"
	// PmtudOpenFlowCookie identifies the flows used to drop ICMP type (3) destination unreachable,
	// fragmentation-needed (4)
	PmtudOpenFlowCookie = "0x0304"
	// DropGARPCookie identifies the flows used to drop GARPs
	DropGARPCookie = "0x0305"
	// CtMarkHost is the conntrack mark value for host traffic
	CtMarkHost = "0x2"
)
