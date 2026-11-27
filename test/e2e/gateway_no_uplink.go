// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eendpointslice "k8s.io/kubernetes/test/e2e/framework/endpointslice"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

var _ = Describe("[sig-network] local gateway without an uplink", func() {
	const (
		serverName  = "no-uplink-server"
		clientName  = "no-uplink-client"
		serviceName = "no-uplink-service"
		httpPort    = uint16(8080)
	)

	f := wrappedTestFramework("no-uplink")

	It("keeps internal pod, service, and host connectivity available", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		if os.Getenv("OVN_DUMMY_GATEWAY_BRIDGE") != "true" {
			Skip("requires the dummy gateway bridge topology")
		}
		Expect(isLocalGWModeEnabled()).To(BeTrue(), "the no-uplink topology requires local gateway mode")

		nodes, err := e2enode.GetReadyNodesIncludingTainted(ctx, f.ClientSet)
		framework.ExpectNoError(err, "failed to list ready nodes")
		Expect(nodes.Items).NotTo(BeEmpty(), "the no-uplink topology requires a ready node")

		externalBridge := deploymentconfig.Get().ExternalBridgeName()
		for _, node := range nodes.Items {
			By(fmt.Sprintf("verifying %s has no physical uplink on %s", externalBridge, node.Name))
			physicalPorts, err := infraprovider.Get().ExecK8NodeCommand(node.Name, []string{
				"bash", "-ec", `
bridge="$1"
ovs-vsctl --timeout=15 br-exists "$bridge"
for port in $(ovs-vsctl --timeout=15 list-ports "$bridge"); do
    type=$(ovs-vsctl --timeout=15 --if-exists get Interface "$port" type | tr -d '"')
    if [ -z "$type" ] || [ "$type" = "system" ]; then
        echo "$port"
    fi
done
`, "--", externalBridge,
			})
			framework.ExpectNoError(err, "failed to inspect %s on node %s", externalBridge, node.Name)
			Expect(strings.TrimSpace(physicalPorts)).To(BeEmpty(),
				"node %s unexpectedly has a physical uplink attached to %s", node.Name, externalBridge)
		}

		serverNodeName := nodes.Items[0].Name
		clientNodeName := serverNodeName
		if len(nodes.Items) > 1 {
			clientNodeName = nodes.Items[1].Name
		}
		serverLabels := map[string]string{"app": serverName}
		server := newAgnhostPod(f.Namespace.Name, serverName, getAgnHostHTTPPortBindFullCMD(httpPort)...)
		server.Labels = serverLabels
		server.Spec.NodeName = serverNodeName
		server = e2epod.NewPodClient(f).CreateSync(ctx, server)

		client := newAgnhostPod(f.Namespace.Name, clientName, "/agnhost", "pause")
		client.Spec.NodeName = clientNodeName
		client = e2epod.NewPodClient(f).CreateSync(ctx, client)

		By("verifying pod-to-pod connectivity")
		Expect(server.Status.PodIPs).NotTo(BeEmpty(), "server pod %s has no IP addresses", server.Name)
		for _, podIP := range server.Status.PodIPs {
			response := pokeNoUplinkEndpointViaPod(
				ctx, f, f.Namespace.Name, client.Name, podIP.IP, httpPort, "hostname")
			Expect(response).To(Equal(server.Name), "pod-to-pod connectivity to %s failed", podIP.IP)
		}

		By("verifying pod-to-service connectivity")
		preferDualStack := corev1.IPFamilyPolicyPreferDualStack
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: f.Namespace.Name},
			Spec: corev1.ServiceSpec{
				Selector:       serverLabels,
				Type:           corev1.ServiceTypeClusterIP,
				IPFamilyPolicy: &preferDualStack,
				Ports: []corev1.ServicePort{{
					Protocol:   corev1.ProtocolTCP,
					Port:       int32(httpPort),
					TargetPort: intstr.FromInt(int(httpPort)),
				}},
			},
		}
		service, err = f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(ctx, service, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create the no-uplink service")
		err = e2eendpointslice.WaitForEndpointPods(
			ctx, f.ClientSet, f.Namespace.Name, service.Name, server.Name)
		framework.ExpectNoError(err, "the no-uplink service did not get an endpoint")
		Expect(service.Spec.ClusterIPs).NotTo(BeEmpty(), "service %s has no cluster IPs", service.Name)
		for _, serviceIP := range service.Spec.ClusterIPs {
			response := pokeNoUplinkEndpointViaPod(
				ctx, f, f.Namespace.Name, client.Name, serviceIP, httpPort, "hostname")
			Expect(response).To(Equal(server.Name), "pod-to-service connectivity via %s failed", serviceIP)
		}

		By("verifying host-to-pod connectivity")
		for _, podIP := range server.Status.PodIPs {
			response, err := infraprovider.Get().ExecK8NodeCommand(clientNodeName, []string{
				"curl", "--fail", "--silent", "--show-error", "--max-time", "5",
				fmt.Sprintf("http://%s/hostname", net.JoinHostPort(podIP.IP, fmt.Sprintf("%d", httpPort))),
			})
			framework.ExpectNoError(err, "node %s could not reach pod IP %s without a gateway uplink", clientNodeName, podIP.IP)
			Expect(strings.TrimSpace(response)).To(Equal(server.Name),
				"host-to-pod connectivity from node %s to %s failed", clientNodeName, podIP.IP)
		}
	})
})

func pokeNoUplinkEndpointViaPod(
	ctx context.Context,
	f *framework.Framework,
	namespace, podName, targetHost string,
	targetPort uint16,
	request string,
) string {
	ipPort := net.JoinHostPort(targetHost, fmt.Sprintf("%d", targetPort))
	curlCommand := fmt.Sprintf("curl -g -q -s --max-time 10 http://%s/%s", ipPort, request)

	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stdOut, stdErr, err := e2epod.ExecShellInPodWithFullOutput(probeCtx, f, podName, curlCommand)
	framework.ExpectNoError(err, "failed to run command within pod")
	if stdErr != "" {
		framework.Failf("failed to run command within pod %s/%s, stdout: %q, stderr: %q", namespace, podName, stdOut, stdErr)
	}
	return stdOut
}
