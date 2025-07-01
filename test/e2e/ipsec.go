package e2e

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

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
			targetMetrics := "ovnkube_controller_ipsec_enabled 1"
			ipsecTunnelMetrics := []string{"ovnkube_controller_ipsec_tunnel_state 1", "ovnkube_controller_ipsec_tunnel_state 0"}
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			framework.ExpectNoError(err)
			nodesIPinfo := collectNodesAddresses(nodes, v1.NodeInternalIP)
			node1 := nodesIPinfo[0]

			ovnKubernetesNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
			for _, nodeInfo := range nodesIPinfo {
				metricsOutput := getIPSecMetricsFromNode(f, nodeInfo.name)

				ginkgo.By(fmt.Sprintf("Verify IPSec metrics was enabled for ovn pod on node %s. ", nodeInfo.name))
				gomega.Expect(metricsOutput).Should(gomega.ContainSubstring(targetMetrics))
				ginkgo.By(fmt.Sprintf("Verify ipsec tunnel metrics reflecting up from node %s. ", nodeInfo.name))
				gomega.Expect(metricsOutput).Should(gomega.ContainSubstring(ipsecTunnelMetrics[0]))
			}

			ipsecContainer := "ovn-ipsec"
			ginkgo.By(fmt.Sprintf("Kill pluto process in ipsec pod which was deployed on node %s.", node1.name))
			ipsecPod, err := f.ClientSet.CoreV1().Pods(ovnKubernetesNamespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: "app=ovn-ipsec",
				FieldSelector: "spec.nodeName=" + node1.name,
			})
			if err != nil {
				framework.Failf("could not get ipsec pods: %v", err)
			}

			_, err = e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", ipsecPod.Items[0].Name, "--container", ipsecContainer, "--",
				"bash", "-c", "sudo pkill pluto")
			framework.ExpectNoError(err, "killing pluto process failed")
			time.Sleep(10 * time.Second)

			for _, nodeInfo := range nodesIPinfo {
				metricsOutput := getIPSecMetricsFromNode(f, nodeInfo.name)

				ginkgo.By(fmt.Sprintf("Verify ipsec tunnel metrics reflecting down from node %s. ", nodeInfo.name))
				gomega.Expect(metricsOutput).Should(gomega.ContainSubstring(ipsecTunnelMetrics[1]))
			}

			// pluto process will be back automatically.
			ginkgo.By(fmt.Sprintf("Wait pluto process back on node %s", node1.name))
			gomega.Eventually(func() bool {
				output, _ := e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", ipsecPod.Items[0].Name, "--container", ipsecContainer, "--",
					"bash", "-c", "sudo ps -ef |grep pluto")
				return strings.Contains(output, " /usr/libexec/ipsec/pluto --leak-detective --config /etc/ipsec.conf")
			}, 120*time.Second, 10*time.Second).Should(gomega.BeTrue())

			ginkgo.By("Verify ipsec metrics reflecting the up tunnel for all nodes")
			//Wait for the metrics reflecting the tunnel up for all the nodes
			gomega.Eventually(func() bool {
				for _, nodeInfo := range nodesIPinfo {
					metricsOutput := getIPSecMetricsFromNode(f, nodeInfo.name)

					ginkgo.By(fmt.Sprintf("Verify ipsec tunnel metrics reflecting up again from node %s.", nodeInfo.name))
					if !strings.Contains(metricsOutput, ipsecTunnelMetrics[0]) {
						return false
					}
				}
				return true
			}, 240*time.Second, 30*time.Second).Should(gomega.BeTrue())
		})

	})

})

// getIPSecMetricsFromNode retrieves IPSec metrics from the specified node
func getIPSecMetricsFromNode(f *framework.Framework, nodeName string) string {
	ovnKubernetesNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	containerName := "ovnkube-node"
	if isInterconnectEnabled() {
		containerName = "ovnkube-controller"
	}

	ovnKubeNodePods, err := f.ClientSet.CoreV1().Pods(ovnKubernetesNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "name=ovnkube-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		framework.Failf("could not get ovnkube-node pods: %v", err)
	}
	if len(ovnKubeNodePods.Items) == 0 {
		framework.Failf("no ovnkube-node pods found on node %s", nodeName)
	}
	ovnPodName := ovnKubeNodePods.Items[0].Name

	framework.Logf("Check ipsec metrics on ovnkube pod %s", ovnPodName)
	url := fmt.Sprintf("http://%s/metrics", net.JoinHostPort(nodeName, "9410"))
	metricsOutput, err := e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", ovnPodName, "--container", containerName, "--",
		"bash", "-c", fmt.Sprintf("curl --connect-timeout 2 %s | grep ipsec", url))
	framework.ExpectNoError(err, "Retrieving metrics failed")

	return metricsOutput
}

func collectNodesAddresses(nodes *v1.NodeList, addressType v1.NodeAddressType) []nodeInfo {
	nodeInfos := []nodeInfo{}
	for i := range nodes.Items {
		addresses := e2enode.GetAddresses(&nodes.Items[i], addressType)
		nodeIP := ""
		if len(addresses) > 0 {
			nodeIP = addresses[0]
		} else {
			framework.Logf("Warning: node %s has no address of type %s", nodes.Items[i].Name, addressType)
		}
		nodeInfos = append(nodeInfos, nodeInfo{
			name:   nodes.Items[i].Name,
			nodeIP: nodeIP,
		})
	}
	return nodeInfos
}
