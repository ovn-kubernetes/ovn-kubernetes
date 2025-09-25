package e2e

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = ginkgo.Describe("IPsec", func() {

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
	ginkgo.Context("Test IPsec metrics ", func() {
		// Validate IPsec metrics can be retrieved in IPsec enabled cluster
		// Verify two kinds of metrics were collected
		// "ovnkube_controller_ipsec_enabled 1" is the IPsec legacy metric
		// "ovnkube_controller_ipsec_tunnel_ike_child_sa_state 1" is the new one added which reflects IKE Child SA establishment state
		// When all tunnels has IKE Child SAs established, then metric is set to 1, otherwise it will be reset to 0
		// Kill the pluto process to bring down the IPsec tunnel
		// Verify all the IPsec IKE Child SA establishment state down, metrics set to 0
		// Wait pluto process back
		// Verify metrics reflected that, set back to 1
		ginkgo.It("Get IPsec metrics which reflect IPsec tunnel IKE Child SA establishment state", func() {
			ipsecMetricName := "ovnkube_controller_" + types.MetricIPsecEnabled
			ipsecMetricIKEChildSAState := "ovnkube_controller_" + types.MetricIPsecTunnelIKEChildSAState
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

				ginkgo.By(fmt.Sprintf("Verify IPsec IKE Child SAs established state metrics reflecting up from ovn pod %s", ovnPod.Name))
				ipsecTunnelMetricValue := getMetricValue(f, ovnPod.Name, ovnKubernetesNamespace, ipsecMetricIKEChildSAState)
				gomega.Expect(ipsecTunnelMetricValue).Should(gomega.Equal("1"))
			}

			ipsecContainer := "ovn-ipsec"
			ginkgo.By(fmt.Sprintf("Kill pluto process in IPsec pod which was deployed on node %s", node1.Name))
			ipsecPod, err := f.ClientSet.CoreV1().Pods(ovnKubernetesNamespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: "app=ovn-ipsec",
				FieldSelector: "spec.nodeName=" + node1.Name,
			})
			if err != nil && apierrors.IsNotFound(err) {
				framework.Failf("could not get ipsec pods: %v", err)
			}
			if len(ipsecPod.Items) == 0 {
				framework.Failf("no ovn-ipsec pods found on node %s", node1.Name)
			}

			_, err = e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", ipsecPod.Items[0].Name, "--container", ipsecContainer, "--",
				"bash", "-c", "sudo pkill pluto")
			framework.ExpectNoError(err, "killing pluto process failed")

			ginkgo.By("Verify IPsec IKE Child SAs established metrics reflecting down from ovn pods.")
			gomega.Eventually(func() bool {
				for _, ovnPod := range ovnPods.Items {
					ipsecTunnelMetricValue := getMetricValue(f, ovnPod.Name, ovnKubernetesNamespace, ipsecMetricIKEChildSAState)
					if ipsecTunnelMetricValue != "0" {
						return false
					}
				}
				return true
			}, 10*time.Second, 5*time.Second).Should(gomega.BeTrue())

			// Recreate the ipsec pod to bring back pluto
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
				if err != nil && apierrors.IsNotFound(err) {
					return false
				}
				if len(ipsecPod.Items) == 0 {
					return false
				}

				framework.Logf("Wait pluto process back in ipsec pod %s", ipsecPod.Items[0].Name)
				output, _ := e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", ipsecPod.Items[0].Name, "--container", ipsecContainer, "--",
					"bash", "-c", "sudo ps -ef | grep pluto | grep -v ovs-monitor-ipsec")
				return strings.Contains(output, " /usr/libexec/ipsec/pluto --leak-detective --config /etc/ipsec.conf")
			}, 20*time.Second, 5*time.Second).Should(gomega.BeTrue())

			ginkgo.By("Verify IPsec IKE Child SAs established metrics reflecting up from ovn pods again")
			//Wait a few seconds for tunnels setting up.
			gomega.Eventually(func() bool {
				for _, ovnPod := range ovnPods.Items {
					ipsecTunnelMetricValue := getMetricValue(f, ovnPod.Name, ovnKubernetesNamespace, ipsecMetricIKEChildSAState)
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
	metricURLCmd := fmt.Sprintf("prom2json %s | jq '.[] | select(.name == \"%s\") | .metrics[].value' ", metricURL, metricName)
	cmd := exec.Command("bash", "-c", metricURLCmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		framework.Failf("prom2json failed: %v", err)
	}

	// Use prom2json and jq to extract the specific metric value from the output
	values := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(values) == 0 || values[0] == "" {
		framework.Failf("no metric value found for %s", metricName)
	}

	metric := strings.Trim(values[0], `"`)
	framework.Logf("The value of %s is %s", metricName, metric)

	return metric
}
