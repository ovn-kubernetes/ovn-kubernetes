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
		// "ovnkube_controller_ipsec_enabled 1" this is the legacy metric
		/* ipsec tunnel state metrics with remote ip, below is metrics retrieved from ovn pod on node 172.18.0.4
				ovnkube_controller_ipsec_tunnel_state{remote_ip="172.18.0.2"} 1
		        ovnkube_controller_ipsec_tunnel_state{remote_ip="172.18.0.3"} 1
		*/
		// Kill the pluto process to bring down the ipsec tunnel
		// Verify metric retrieved from local node, all the ipsec tunnels down, metrics set to 0
		// Verify the metric retrieved from remote node, only metrics for the the remote IP is the node IP that pluto process was killed set to 0
		// Wait pluto process back
		// Verify metrics reflected that, set back to 1
		ginkgo.It("should be able to get IPSec metrics which reflect the down and up state of the IPSec tunnel.", func() {
			targetMetrics := "ovnkube_controller_ipsec_enabled 1"
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
			framework.ExpectNoError(err)
			nodesIPinfo := collectNodesAddresses(nodes, v1.NodeInternalIP)

			ovnKubernetesNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
			for _, nodeInfo := range nodesIPinfo {
				metricsOutput := getIPSecMetricsFromNode(f, nodeInfo.name)

				ginkgo.By(fmt.Sprintf("Verify IPSec metrics was enabled for ovn pod on node %s. ", nodeInfo.name))
				gomega.Expect(metricsOutput).Should(gomega.ContainSubstring(targetMetrics))

				ginkgo.By(fmt.Sprintf("Verify IPSec tunnel metrics was able to retrieved from ovn pod on node and was set 1 for node %s.", nodeInfo.name))
				for _, remoteNode := range nodesIPinfo {
					if remoteNode.name == nodeInfo.name {
						continue
					}
					ipsecTunnelMetric := fmt.Sprintf("ovnkube_controller_ipsec_tunnel_state{remote_ip=\"%s\"} 1", remoteNode.nodeIP)
					gomega.Expect(metricsOutput).Should(gomega.ContainSubstring(ipsecTunnelMetric))
				}

			}

			node1 := nodesIPinfo[0]
			node2 := nodesIPinfo[1]

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
			framework.ExpectNoError(err, "Retrieving metrics failed")
			time.Sleep(10 * time.Second)

			ginkgo.By(fmt.Sprintf("Verify ipsec metrics reflecting the down tunnel from local node %s", node1.name))
			metricsOutput := getIPSecMetricsFromNode(f, node1.name)
			// Verify that tunnel state metrics show 0 for all remote nodes from
			for _, remoteNode := range nodesIPinfo {
				if remoteNode.name == node1.name {
					continue
				}
				ipsecTunnelMetric := fmt.Sprintf("ovnkube_controller_ipsec_tunnel_state{remote_ip=\"%s\"} 0", remoteNode.nodeIP)
				gomega.Expect(metricsOutput).Should(gomega.ContainSubstring(ipsecTunnelMetric))
			}

			ginkgo.By(fmt.Sprintf("Verify ipsec metrics reflecting the tunnel state retrieved from node %s", node2.name))
			metricsOutput = getIPSecMetricsFromNode(f, node2.name)
			for _, remoteNode := range nodesIPinfo {
				if remoteNode.name == node2.name {
					continue
				}
				if remoteNode.name == node1.name {
					ginkgo.By(fmt.Sprintf("Verify ipsec metrics reflecting the down tunnel from remote node %s", remoteNode.name))
					ipsecTunnelMetric := fmt.Sprintf("ovnkube_controller_ipsec_tunnel_state{remote_ip=\"%s\"} 0", remoteNode.nodeIP)
					gomega.Expect(metricsOutput).Should(gomega.ContainSubstring(ipsecTunnelMetric))

				} else {
					ginkgo.By(fmt.Sprintf("Verify ipsec metrics reflecting the up tunnel from remote node %s", remoteNode.name))
					ipsecTunnelMetric := fmt.Sprintf("ovnkube_controller_ipsec_tunnel_state{remote_ip=\"%s\"} 1", remoteNode.nodeIP)
					gomega.Expect(metricsOutput).Should(gomega.ContainSubstring(ipsecTunnelMetric))
				}
			}

			// pluto process will be back automatically around a couple of minutes later.
			ginkgo.By(fmt.Sprintf("Wait pluto process back on node %s", node1.name))
			gomega.Eventually(func() bool {
				output, _ := e2ekubectl.RunKubectl(ovnKubernetesNamespace, "exec", ipsecPod.Items[0].Name, "--container", ipsecContainer, "--",
					"bash", "-c", "sudo ps -ef |grep pluto")
				return strings.Contains(output, " /usr/libexec/ipsec/pluto --leak-detective --config /etc/ipsec.conf")
			}, 180*time.Second, 10*time.Second).Should(gomega.BeTrue())

			ginkgo.By("Verify ipsec metrics reflecting the up tunnel for all nodes")
			//Wait for the metrics reflecting the tunnel up for all the nodes
			gomega.Eventually(func() bool {
				for _, nodeInfo := range nodesIPinfo {
					metricsOutput := getIPSecMetricsFromNode(f, nodeInfo.name)

					ginkgo.By(fmt.Sprintf("Verify IPSec tunnel metrics was able to retrieved from ovn pod on node %s.", nodeInfo.name))
					for _, remoteNode := range nodesIPinfo {
						if remoteNode.name == nodeInfo.name {
							continue
						}
						ipsecTunnelMetric := fmt.Sprintf("ovnkube_controller_ipsec_tunnel_state{remote_ip=\"%s\"} 1", remoteNode.nodeIP)
						if strings.Contains(metricsOutput, ipsecTunnelMetric) {
							return false
						}
					}
				}
				return true
			}, 180*time.Second, 10*time.Second).Should(gomega.BeTrue())
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
