package diagnostics

import (
	appsv1 "k8s.io/api/apps/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func (d *Diagnostics) IPTablesDumpingDaemonSet() {
	if !d.iptables {
		return
	}
	By("Creating iptables dumping daemonsets")
	daemonSets := []appsv1.DaemonSet{}
	daemonSetName := "dump-iptables"
	cmd := composePeriodicCmd("iptables -L -n", 10)
	daemonSets = append(daemonSets, d.composeDiagnosticsDaemonSet(daemonSetName, cmd, "iptables"))
	Expect(d.runDaemonSets(daemonSets)).To(Succeed())
}
