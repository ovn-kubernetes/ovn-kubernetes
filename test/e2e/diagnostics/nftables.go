package diagnostics

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
)

func (d *Diagnostics) NFTablesDumpingDaemonSet() {
	if !d.nftables {
		return
	}
	By("Creating nftables dumping daemonsets")
	daemonSets := []appsv1.DaemonSet{}
	daemonSetName := fmt.Sprintf("dump-nftables")
	cmd := composePeriodicCmd("nft list ruleset", 10)
	daemonSets = append(daemonSets, d.composeDiagnosticsDaemonSet(daemonSetName, cmd, "nftables"))
	Expect(d.runDaemonSets(daemonSets)).To(Succeed())
}
