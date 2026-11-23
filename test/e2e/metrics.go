// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
)

// These smoke tests guard what the metrics unit tests structurally cannot: that
// the OVN/OVS metric endpoints actually serve their expected series against real
// daemons (catching parser-vs-real-output drift) and that the metrics servers
// are wired up in a real deployment. They deliberately assert only presence of a
// representative subset, not exact values, to stay stable across environments.
var _ = ginkgo.Describe("e2e OVN/OVS metrics", feature.Metrics, func() {
	const (
		ovnMetricsPort = 9476 // --ovn-metrics-bind-address, see dist/images/ovnkube.sh
		ovsMetricsPort = 9310 // ovs-metrics exporter

		controlPlanePodName       = "ovnkube-control-plane"
		controlPlaneLeaseName     = "ovn-kubernetes-master"
		clusterManagerMetricsPort = 9411 // --cluster-manager-metrics-bind-address, see dist/images/ovnkube.sh
	)

	f := wrappedTestFramework("metrics")

	var (
		ovnNamespace string
		nodePod      corev1.Pod
	)

	ginkgo.BeforeEach(func() {
		ovnNamespace = deploymentconfig.Get().OVNKubernetesNamespace()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
		})
		framework.ExpectNoError(err, "listing ovnkube-node pods")
		gomega.Expect(pods.Items).NotTo(gomega.BeEmpty(), "no ovnkube-node pods found")
		nodePod = pods.Items[0]
		gomega.Expect(nodePod.Status.PodIP).NotTo(gomega.BeEmpty(), "ovnkube-node pod has no IP")
	})

	scrapeNoErrorPodIPPort := func(nodePodName, container, ip string, port int) (string, error) {
		url := "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/metrics"
		stdout, stderr, err := ExecCommandInContainerWithFullOutput(
			f, ovnNamespace, nodePodName, container,
			"curl", "-sS", "--max-time", "10", url,
		)
		if err != nil {
			return "", fmt.Errorf("curl %s failed: %s", url, stderr)
		}
		if stdout == "" {
			return "", fmt.Errorf("empty metrics response from %s", url)
		}
		return stdout, nil
	}

	scrapeNoError := func(container string, port int) (string, error) {
		return scrapeNoErrorPodIPPort(nodePod.Name, container, nodePod.Status.PodIP, port)
	}

	// scrape is a convenience wrapper that asserts success, for non-retrying contexts.
	scrape := func(container string, port int) string {
		body, err := scrapeNoError(container, port)
		framework.ExpectNoError(err)
		return body
	}

	asserValuePresent := func(body string, name string, value float64) error {
		for line := range strings.SplitSeq(body, "\n") {
			if strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"{") {
				// Match the metric name at the start of a sample line, with or
				// without a label set, e.g. `name 1` or `name{db_name="..."} 1`.
				if strings.HasPrefix(line, name+"{") {
					end := strings.LastIndexByte(line, '}')
					if end < 0 {
						return fmt.Errorf("unterminated label set for %q", name)
					}
					line = line[end+1:]
				}
				parts := strings.Split(line, " ")
				if len(parts) != 2 {
					return fmt.Errorf("expected 2 parts for metric %q, got %d", line, len(parts))
				}
				parsedValue, err := strconv.ParseFloat(parts[1], 64)
				if err != nil {
					return fmt.Errorf("failed to parse value %q for metric %q: %w", parts[1], name, err)
				}
				if parsedValue != value {
					return fmt.Errorf("expected value %v for metric %q, got %q", value, name, parts[1])
				}
				return nil
			}
		}
		return fmt.Errorf("expected metric %q missing from endpoint", name)
	}

	assertPresent := func(body string, names ...string) {
		for _, name := range names {
			// Match the metric name at the start of a sample line, with or
			// without a label set, e.g. `name 1` or `name{db_name="..."} 1`.
			present := false
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"{") {
					present = true
					break
				}
			}
			gomega.Expect(present).To(gomega.BeTrue(), "expected metric %q missing from endpoint", name)
		}
	}

	ginkgo.It("delivers OVN controller and northd metrics, including the loop-collected ones", func() {
		body := scrape("ovnkube-controller", ovnMetricsPort)
		assertPresent(body,
			// northd status/connection metrics (background-loop gauges)
			"ovn_northd_status",
			"ovn_northd_nb_connection_status",
			"ovn_northd_sb_connection_status",
			// integration-bridge metrics (background-loop gauges)
			"ovn_controller_integration_bridge_openflow_total",
			"ovn_controller_integration_bridge_patch_ports",
			"ovn_controller_integration_bridge_geneve_ports",
			// a representative always-registered controller metric
			"ovn_controller_southbound_database_connected",
			// OVN DB gauges refreshed by the loop
			"ovn_db_db_size_bytes",
		)
	})

	ginkgo.It("delivers OVS metrics from the standalone exporter", func() {
		body := scrape("ovs-metrics-exporter", ovsMetricsPort)
		assertPresent(body,
			"ovs_build_info",
			"ovs_vswitchd_dp_flows_total",
		)
	})

	ginkgo.It("keeps the endpoint responsive to repeated scrapes", func() {
		// The scrape handler only serializes the in-memory registry (extraction
		// runs on its own loop), so repeated back-to-back scrapes must all return
		// promptly and non-empty regardless of daemon load.
		for i := 0; i < 5; i++ {
			gomega.Eventually(func(g gomega.Gomega) {
				body, err := scrapeNoError("ovnkube-controller", ovnMetricsPort)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(body).To(gomega.ContainSubstring("ovn_northd_status"))
			}).Within(20 * time.Second).ProbeEvery(time.Second).Should(gomega.Succeed())
		}
	})

	ginkgo.It("sets the leader metric when the cluster manager is the leader", func() {
		gomega.Eventually(func(g gomega.Gomega) {
			ginkgo.By("finding the leader ovnkube-control-plane node")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			lease, err := f.ClientSet.CoordinationV1().Leases(ovnNamespace).Get(ctx, controlPlaneLeaseName, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(lease.Spec.HolderIdentity).NotTo(gomega.BeNil(), "lease has no holder identity")
			g.Expect(*lease.Spec.HolderIdentity).NotTo(gomega.BeEmpty(), "lease holder identity is empty")
			leaderNode := *lease.Spec.HolderIdentity

			ginkgo.By("finding the leader ovnkube-control-plane pod")
			pods, err := f.ClientSet.CoreV1().Pods(ovnNamespace).List(ctx, metav1.ListOptions{
				FieldSelector: "spec.nodeName=" + leaderNode,
				LabelSelector: "name=" + controlPlanePodName,
			})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(pods.Items).NotTo(gomega.BeEmpty(), "no ovnkube-control-plane pods found on leader node")
			g.Expect(len(pods.Items)).To(gomega.Equal(1), "multiple ovnkube-control-plane pods found on leader node")
			leaderPod := pods.Items[0]
			g.Expect(leaderPod.Status.PodIP).NotTo(gomega.BeEmpty(), "ovnkube-control-plane pod has no IP")

			ginkgo.By("scraping the metrics endpoint")
			body, err := scrapeNoErrorPodIPPort(nodePod.Name, "ovnkube-controller", leaderPod.Status.PodIP, clusterManagerMetricsPort)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(asserValuePresent(body, "ovnkube_clustermanager_leader", 1.0)).NotTo(gomega.HaveOccurred())
		}).Within(1 * time.Minute).ProbeEvery(1 * time.Second).Should(gomega.Succeed())
	})
})
