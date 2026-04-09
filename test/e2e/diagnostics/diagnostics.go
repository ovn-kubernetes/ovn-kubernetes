package diagnostics

import (
	"k8s.io/kubernetes/test/e2e/framework"
)

type Diagnostics struct {
	fr                                               *framework.Framework
	conntrack, iptables, nftables, ovsflows, tcpdump bool
}

func New(fr *framework.Framework) *Diagnostics {
	return &Diagnostics{
		fr:        fr,
		conntrack: conntrack,
		iptables:  iptables,
		nftables:  nftables,
		ovsflows:  ovsflows,
		tcpdump:   tcpdump,
	}
}
