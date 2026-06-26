// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = ginkgo.Describe("IPsec", feature.IPsec, func() {

	f := wrappedTestFramework("ipsec")
	var nodes *v1.NodeList

	ginkgo.BeforeEach(func() {
		var err error
		if !isIPsecEnabled() {
			ginkgo.Skip("Test requires IPsec enabled cluster. but IPsec is not enabled in this cluster.")
		}
		nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			e2eskipper.Skipf(
				"Test requires >= 2 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

	})
	ginkgo.Context("Metrics", func() {
		// Validate IPsec metrics can be retrieved in IPsec enabled cluster
		// Verify two kinds of metrics were collected
		// "ovnkube_controller_ipsec_enabled 1" is the IPsec legacy metric
		// "ovnkube_controller_ipsec_tunnel_ike_child_sa_state 1" is the new one added,
		// 1 reflect tunnel established, 0 reflecting tunnel not established.
		// Note: IPsec connection state is held in the kernel (ip xfrm state and policy), not in the
		// pluto daemon itself. Killing pluto doesn't immediately affect existing tunnels as the kernel
		// maintains them. Other nodes only detect issues during rekey or new connection attempts
		// (infrequent operations). This test verifies that killing the pluto daemon and restarting
		//  ovn-ipsec pod does not disrupt tunnel state, and the metric remains set to 1 throughout.
		ginkgo.It("IKE Child SA State which reflects the down and up state of the IPsec tunnel", func() {
			ipsecMetricName := "ovnkube_controller_ipsec_enabled"
			ipsecTunnelMetricName := "ovnkube_controller_ipsec_tunnel_ike_child_sa_state"
			node1 := nodes.Items[0]

			ovnKubernetesNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
			ovnPods, err := f.ClientSet.CoreV1().Pods(ovnKubernetesNamespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: "name=ovnkube-node",
			})
			if err != nil {
				framework.Failf("failed to list OVN pods: %v", err)
			}

			for _, ovnPod := range ovnPods.Items {
				ginkgo.By(fmt.Sprintf("Verify IPsec enabled metric from ovn pod %s", ovnPod.Name))
				ipsecMetricValue := getMetricValue(f, ovnPod.Name, ovnKubernetesNamespace, ipsecMetricName)
				gomega.Expect(ipsecMetricValue).Should(gomega.Equal("1"))

				ginkgo.By(fmt.Sprintf("Verify IPsec tunnel metrics reflecting up from ovn pod %s", ovnPod.Name))
				ipsecTunnelMetricValue := getMetricValue(f, ovnPod.Name, ovnKubernetesNamespace, ipsecTunnelMetricName)
				gomega.Expect(ipsecTunnelMetricValue).Should(gomega.Equal("1"))
			}

			ipsecContainer := "ovn-ipsec"
			ginkgo.By(fmt.Sprintf("Kill pluto process in IPsec pod which was deployed on node %s", node1.Name))
			ipsecPod, err := f.ClientSet.CoreV1().Pods(ovnKubernetesNamespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: "app=ovn-ipsec",
				FieldSelector: "spec.nodeName=" + node1.Name,
			})
			if err != nil {
				framework.Failf("could not get ipsec pods: %v", err)
			}
			if len(ipsecPod.Items) == 0 {
				framework.Failf("no ovn-ipsec pods found on node %s", node1.Name)
			}

			_, err = e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", ipsecPod.Items[0].Name, "--container", ipsecContainer, "--",
				"bash", "-c", "sudo pkill pluto")
			framework.ExpectNoError(err, "killing pluto process failed")
			err = f.ClientSet.CoreV1().Pods(ovnKubernetesNamespace).Delete(context.TODO(), ipsecPod.Items[0].Name, metav1.DeleteOptions{})
			if err != nil {
				framework.Failf("failed to delete pod %s: %v", ipsecPod.Items[0].Name, err)
			}
			ginkgo.By(fmt.Sprintf("Wait the recreated ipsec pod to be ready on node %s", node1.Name))
			gomega.Eventually(func() bool {
				ginkgo.By(fmt.Sprintf("Get the new ipsec pod on node %s", node1.Name))
				ipsecPod, err := f.ClientSet.CoreV1().Pods(ovnKubernetesNamespace).List(context.TODO(), metav1.ListOptions{
					LabelSelector: "app=ovn-ipsec",
					FieldSelector: "spec.nodeName=" + node1.Name,
				})
				if err != nil {
					if apierrors.IsNotFound(err) {
						return false // Pod not yet recreated, keep polling
					}
					framework.Logf("Unexpected error getting ipsec pod: %v", err)
					return false
				}
				if len(ipsecPod.Items) == 0 {
					return false
				}

				framework.Logf("Wait pluto process back in ipsec pod %s", ipsecPod.Items[0].Name)
				output, _ := e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", ipsecPod.Items[0].Name, "--container", ipsecContainer, "--",
					"bash", "-c", "sudo ps -ef | grep pluto | grep -v ovs-monitor-ipsec")
				return strings.Contains(output, " /usr/libexec/ipsec/pluto --leak-detective --config /etc/ipsec.conf")
			}, 90*time.Second, 2*time.Second).Should(gomega.BeTrue())

			ginkgo.By("Verify IPsec metrics reflecting the up tunnel from all ovn pods")
			//Wait a few seconds for tunnels setting up.
			gomega.Eventually(func() bool {
				for _, ovnPod := range ovnPods.Items {
					ipsecTunnelMetricValue := getMetricValue(f, ovnPod.Name, ovnKubernetesNamespace, ipsecTunnelMetricName)
					if ipsecTunnelMetricValue != "1" {
						return false
					}
				}
				return true
			}, 20*time.Second, 5*time.Second).Should(gomega.BeTrue())
		})

	})

})

// Get metric value by prom2json tool which is for name:value pair,not for labels matching.
func getMetricValue(f *framework.Framework, podName, podNamespace, metricName string) string {
	ginkgo.GinkgoHelper()

	pod, err := f.ClientSet.CoreV1().Pods(podNamespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		framework.Failf("failed to get pod %s in namespace %s: %v", podName, podNamespace, err)
	}

	podIP := pod.Status.PodIP
	if podIP == "" {
		framework.Failf("no pod IP available for pod %s", podName)
	}

	metricURL := fmt.Sprintf("http://%s/metrics", net.JoinHostPort(podIP, "9410"))

	// Run prom2json directly from the host where tests are running
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "prom2json", metricURL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			framework.Failf("prom2json timed out for %s", metricURL)
		}
		framework.Failf("prom2json failed: %v, output: %s, pod: %s/%s", err, string(output), podNamespace, podName)
	}

	// Parse prom2json output to extract the specific metric value
	type promMetric struct {
		Name    string `json:"name"`
		Metrics []struct {
			Value string `json:"value"`
		} `json:"metrics"`
	}

	var metrics []promMetric
	if err := json.Unmarshal(output, &metrics); err != nil {
		framework.Failf("failed to parse prom2json output: %v", err)
	}

	// Find the metric by name and extract its value
	for _, m := range metrics {
		if m.Name == metricName {
			if len(m.Metrics) == 0 {
				framework.Failf("no metric value found for %s from pod %s/%s", metricName, podNamespace, podName)
			}
			metric := m.Metrics[0].Value
			framework.Logf("The value of %s is %s from pod %s/%s", metricName, metric, podNamespace, podName)
			return metric
		}
	}

	framework.Failf("metric %s not found in prom2json output from pod %s/%s", metricName, podNamespace, podName)
	return ""
}
