package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

// PromMetric represents a single line metric
// Example:
// ovnkube_controller_ipsec_enabled 1
// ovnkube_node_workqueue_adds_total{name="ovn-lb-controller"} 16
type PromMetric struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
}

var _ = ginkgo.Describe("IPSec", func() {

	f := wrappedTestFramework("ipsec")

	ginkgo.BeforeEach(func() {
		if !isIPSecEnabled() {
			ginkgo.Skip("Test requires IPSec enabled cluster. but IPSec is not enabled in this cluster.")
		}
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			e2eskipper.Skipf(
				"Test requires >= 2 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

	})
	ginkgo.Context("Test IPSec metrics ", func() {
		// Validate IPSec metrics can be retrieved in IPSec enabled cluster
		// Verify two kinds of metrics were collected
		// "ovnkube_controller_ipsec_enabled 1" is the IPSec legacy metric
		// 	"ovnkube_controller_ipsec_tunnel_state 1" is the new one added, 1 reflect tunnel up, 0 reflecting tunnel down.
		// Kill the pluto process to bring down the ipsec tunnel
		// Verify all the ipsec tunnels down, metrics set to 0
		// Wait pluto process back
		// Verify metrics reflected that, set back to 1
		ginkgo.It("should be able to get IPSec metrics which reflect the down and up state of the IPSec tunnel.", func() {
			ipsecMetricName := "ovnkube_controller_ipsec_enabled"
			ipsecTunnelMetricName := "ovnkube_controller_ipsec_tunnel_state"
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			node1 := nodes.Items[0]
			framework.ExpectNoError(err)

			ovnKubernetesNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
			ovnContainerName := "ovnkube-controller"
			ovnPods, err := f.ClientSet.CoreV1().Pods(ovnKubernetesNamespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: "name=ovnkube-node",
			})
			if err != nil {
				framework.Failf("failed to list OVN pods: %v", err)
			}

			for _, ovnPod := range ovnPods.Items {
				ginkgo.By(fmt.Sprintf("Verify ipsec enabled metric from ovn pod %s. ", ovnPod.Name))
				metricsOutput := getIPSecRawMetricsFromPod(f, ovnPod.Name, ovnKubernetesNamespace, ovnContainerName)
				ipsecMetricValue, err := GetMetricValue(metricsOutput, ipsecMetricName)
				gomega.Expect(err).Should(gomega.BeNil())
				gomega.Expect(int(ipsecMetricValue)).Should(gomega.Equal(1))

				ginkgo.By(fmt.Sprintf("Verify ipsec tunnel metrics reflecting up from ovn pod %s ", ovnPod.Name))
				ipsecTunnelMetricValue, err := GetMetricValue(metricsOutput, ipsecTunnelMetricName)
				gomega.Expect(err).Should(gomega.BeNil())
				gomega.Expect(int(ipsecTunnelMetricValue)).Should(gomega.Equal(1))
			}

			ipsecContainer := "ovn-ipsec"
			ginkgo.By(fmt.Sprintf("Kill pluto process in ipsec pod which was deployed on node %s.", node1.Name))
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
			time.Sleep(10 * time.Second)

			for _, ovnPod := range ovnPods.Items {
				metricsOutput := getIPSecRawMetricsFromPod(f, ovnPod.Name, ovnKubernetesNamespace, ovnContainerName)

				ginkgo.By(fmt.Sprintf("Verify ipsec tunnel metrics reflecting down from ovn pod %s ", ovnPod.Name))
				ipsecTunnelMetricValue, err := GetMetricValue(metricsOutput, ipsecTunnelMetricName)
				gomega.Expect(err).Should(gomega.BeNil())
				gomega.Expect(int(ipsecTunnelMetricValue)).Should(gomega.Equal(0))
			}

			// pluto process will be back automatically.
			ginkgo.By(fmt.Sprintf("Wait pluto process back on node %s", node1.Name))
			gomega.Eventually(func() bool {
				output, _ := e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", ipsecPod.Items[0].Name, "--container", ipsecContainer, "--",
					"bash", "-c", "sudo ps -ef |grep pluto")
				return strings.Contains(output, " /usr/libexec/ipsec/pluto --leak-detective --config /etc/ipsec.conf")
			}, 120*time.Second, 10*time.Second).Should(gomega.BeTrue())

			ginkgo.By("Verify ipsec metrics reflecting the up tunnel from all ovn pods")
			//Wait for the metrics reflecting the tunnel up for all the ovn pods
			gomega.Eventually(func() bool {
				for _, ovnPod := range ovnPods.Items {
					metricsOutput := getIPSecRawMetricsFromPod(f, ovnPod.Name, ovnKubernetesNamespace, ovnContainerName)
					ipsecTunnelMetricValue, err := GetMetricValue(metricsOutput, ipsecTunnelMetricName)
					gomega.Expect(err).Should(gomega.BeNil())
					if int(ipsecTunnelMetricValue) != 1 {
						return false
					}
				}
				return true
			}, 240*time.Second, 30*time.Second).Should(gomega.BeTrue())
		})

	})

})

// getIPSecRawMetricsFromPod retrieves IPSec metrics from the specified pod
func getIPSecRawMetricsFromPod(f *framework.Framework, podName string, nameSpace string, containerName string) string {
	ginkgo.GinkgoHelper()
	pod, err := f.ClientSet.CoreV1().Pods(nameSpace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		framework.Failf("failed to get pod %s in namespace %s: %v", podName, nameSpace, err)
	}

	podIP := pod.Status.PodIP
	if podIP == "" {
		framework.Failf("no pod IP available for pod %s", podName)
	}

	url := fmt.Sprintf("http://%s/metrics", net.JoinHostPort(podIP, "9410"))
	metricsOutput, err := e2ekubectl.RunKubectl(nameSpace, "exec", podName, "--container", containerName, "--",
		"bash", "-c", fmt.Sprintf("curl --connect-timeout 2 %s | grep ipsec", url))
	framework.ExpectNoError(err, "Retrieving metrics failed")

	return metricsOutput

}

// PromToJSON converts Prometheus metrics format to JSON
func PromToJSON(metricsOutput string) (string, error) {
	ginkgo.GinkgoHelper()
	var metrics []PromMetric
	lines := strings.Split(metricsOutput, "\n")

	metricRegex := regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)(?:\{([^}]*)\})?\s+([0-9.+-eE]+)$`)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Skip line starting with #
		if strings.HasPrefix(line, "#") {
			continue
		}

		// Parse metric lines
		matches := metricRegex.FindStringSubmatch(line)
		if len(matches) > 0 {
			name := matches[1]
			labelsStr := matches[2]
			valueStr := matches[3]
			value, err := strconv.ParseFloat(valueStr, 64)
			if err != nil {
				continue
			}

			metric := PromMetric{
				Name:  name,
				Value: value,
			}

			// Parse labels if present
			if labelsStr != "" {
				labels := make(map[string]string)
				labelPairs := strings.Split(labelsStr, ",")
				for _, pair := range labelPairs {
					kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
					if len(kv) == 2 {
						key := strings.TrimSpace(kv[0])
						value := strings.Trim(strings.TrimSpace(kv[1]), `"`)
						labels[key] = value
					}
				}
				if len(labels) > 0 {
					metric.Labels = labels
				}
			}

			metrics = append(metrics, metric)
		}
	}

	metricsJSONData, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal metrics to JSON: %v", err)
	}

	return string(metricsJSONData), nil
}

// GetMetricValueFromJSON extracts a specific metric value from JSON output by name
func GetMetricValue(metricRawOutput string, metricName string) (float64, error) {
	ginkgo.GinkgoHelper()
	var metrics []PromMetric
	var metricValue float64

	jsonMetricsOutput, err := PromToJSON(metricRawOutput)
	if err != nil {
		return 0, fmt.Errorf("failed to convert metrics to JSON: %v", err)
	}

	err = json.Unmarshal([]byte(jsonMetricsOutput), &metrics)
	if err != nil {
		return 0, fmt.Errorf("failed to unmarshal JSON: %v", err)
	}

	for _, metric := range metrics {
		if metric.Name == metricName {
			metricValue = metric.Value
			framework.Logf("Metric name %s : %v", metricName, metricValue)
			return metricValue, nil
		}
	}

	return 0, fmt.Errorf("metric %s not found", metricName)
}
