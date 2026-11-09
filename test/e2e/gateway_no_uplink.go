// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"

	"k8s.io/kubernetes/test/e2e/framework"
	e2eendpointslice "k8s.io/kubernetes/test/e2e/framework/endpointslice"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

var _ = Describe("[sig-network] local gateway without an uplink", func() {
	const (
		serverName = "no-uplink-server"
		clientName = "no-uplink-client"
		httpPort   = uint16(8080)
	)

	f := wrappedTestFramework("no-uplink")

	It("keeps internal pod, service, and host connectivity available", func() {
		if os.Getenv("OVN_DUMMY_GATEWAY_BRIDGE") != "true" {
			Skip("requires the dummy gateway bridge topology")
		}
		Expect(isLocalGWModeEnabled()).To(BeTrue(), "the no-uplink topology requires local gateway mode")

		nodes, err := e2enode.GetReadyNodesIncludingTainted(context.Background(), f.ClientSet)
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
		server = e2epod.NewPodClient(f).CreateSync(context.Background(), server)

		client := newAgnhostPod(f.Namespace.Name, clientName, "/agnhost", "pause")
		client.Spec.NodeName = clientNodeName
		client = e2epod.NewPodClient(f).CreateSync(context.Background(), client)

		By("verifying pod-to-pod connectivity")
		response := pokeEndpointViaPod(f, f.Namespace.Name, client.Name, server.Status.PodIP, httpPort, "hostname")
		Expect(response).To(Equal(server.Name))

		By("verifying pod-to-service connectivity")
		serviceIP, err := createServiceForPodsWithLabel(
			f, f.Namespace.Name, httpPort, httpPort, "ClusterIP", serverLabels)
		framework.ExpectNoError(err, "failed to create the no-uplink service")
		err = e2eendpointslice.WaitForEndpointCount(
			context.Background(), f.ClientSet, f.Namespace.Name, "service-for-pods", 1)
		framework.ExpectNoError(err, "the no-uplink service did not get an endpoint")
		response = pokeEndpointViaPod(f, f.Namespace.Name, client.Name, serviceIP, httpPort, "hostname")
		Expect(response).To(Equal(server.Name))

		By("verifying host-to-pod connectivity")
		response, err = infraprovider.Get().ExecK8NodeCommand(clientNodeName, []string{
			"curl", "--fail", "--silent", "--show-error", "--max-time", "5",
			fmt.Sprintf("http://%s/hostname", net.JoinHostPort(server.Status.PodIP, fmt.Sprintf("%d", httpPort))),
		})
		framework.ExpectNoError(err, "the node could not reach the pod without a gateway uplink")
		Expect(strings.TrimSpace(response)).To(Equal(server.Name))
	})
})
