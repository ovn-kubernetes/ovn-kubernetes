package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

func toTransportZoneAnnotation(zones []string) string {
	quotedZones := []string{}
	for _, zone := range zones {
		quotedZones = append(quotedZones, fmt.Sprintf("\"%s\"", zone))
	}
	return fmt.Sprintf("[%s]", strings.Join(quotedZones, ","))
}

func setTransportZone(node *v1.Node, zones []string, cs clientset.Interface) {
	annotations := make(map[string]any)
	if len(zones) > 0 {
		annotations[ovnNodeTransportZones] = toTransportZoneAnnotation(zones)
	} else {
		annotations[ovnNodeTransportZones] = nil
	}

	var err error
	var patchData []byte
	patch := struct {
		Metadata map[string]any `json:"metadata"`
	}{
		Metadata: map[string]any{
			"annotations": annotations,
		},
	}

	patchData, err = json.Marshal(&patch)
	framework.ExpectNoError(err)

	_, err = cs.CoreV1().Nodes().Patch(context.TODO(), node.Name, types.MergePatchType, patchData, metav1.PatchOptions{})
	framework.ExpectNoError(err)

	gomega.Eventually(func() error {
		return checkTransportZonesInSB(node, zones)
	}, 2*time.Minute, 6*time.Second).Should(gomega.Succeed())
}

func checkTransportZonesInSB(node *v1.Node, zones []string) error {
	// Wait for new configuration to apply in sbdb
	var err error
	var dbPods string
	dbContainerName := "sb-ovsdb"

	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()

	if isInterconnectEnabled() {
		dbPods, err = e2ekubectl.RunKubectl(ovnNamespace, "get", "pods", "-l", "name=ovnkube-node", "--field-selector", fmt.Sprintf("spec.nodeName=%s", node.Name), "-o=jsonpath='{.items..metadata.name}'")
	} else {
		dbPods, err = e2ekubectl.RunKubectl(ovnNamespace, "get", "pods", "-l", "name=ovnkube-db", "-o=jsonpath='{.items..metadata.name}'")
	}
	if err != nil {
		return fmt.Errorf("can't get list of node pods, err: %v", err)
	}
	if len(dbPods) == 0 {
		return fmt.Errorf("list of node pods is empty")
	}
	dbPod := strings.Split(dbPods, " ")[0]
	dbPod = strings.TrimPrefix(dbPod, "'")
	dbPod = strings.TrimSuffix(dbPod, "'")
	if len(dbPod) == 0 {
		return fmt.Errorf("node pod name is empty")
	}

	transportZones, err := e2ekubectl.RunKubectl(ovnNamespace, "exec", dbPod, "-c", dbContainerName, "--", "ovn-sbctl", "--no-leader-only", "--columns=transport_zones", "--bare", "find", "chassis", fmt.Sprintf("hostname=%s", node.Name))
	if err != nil {
		return fmt.Errorf("failed to get transport zones for chassis %s, err: %v", node.Name, err)
	}
	transportZones = strings.TrimSuffix(transportZones, "\n")
	expectedZones := strings.Join(zones, " ")
	if transportZones != expectedZones {
		return fmt.Errorf("zones are different: expected \"%s\", got \"%s\"", expectedZones, transportZones)
	}
	return nil
}

var _ = ginkgo.Describe("Multi node transport zones", func() {

	const (
		serverPodName = "server-pod"
		clientPodName = "client-pod"
	)
	fr := wrappedTestFramework("multi-node-transport-zones")

	var (
		cs clientset.Interface

		serverPodNode *v1.Node
		clientPodNode *v1.Node
	)

	ginkgo.BeforeEach(func() {
		cs = fr.ClientSet

		nodes, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 3 {
			e2eskipper.Skipf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		serverPodNode, err = cs.CoreV1().Nodes().Get(context.TODO(), nodes.Items[0].Name, metav1.GetOptions{})
		if err != nil {
			e2eskipper.Skipf(
				"Test requires node with the name %s", serverPodNode.Name,
			)
		}
		clientPodNode, err = cs.CoreV1().Nodes().Get(context.TODO(), nodes.Items[1].Name, metav1.GetOptions{})
		if err != nil {
			e2eskipper.Skipf(
				"Test requires node with the name %s", clientPodNode.Name,
			)
		}
	})

	ginkgo.AfterEach(func() {
		// Never leave transport zones set on exit
		setTransportZone(clientPodNode, []string{}, cs)
		setTransportZone(serverPodNode, []string{}, cs)
	})

	ginkgo.It("Pod interconnectivity", func() {
		// Create a server pod on zone - zone-1
		cmd := httpServerContainerCmd(8000)
		serverPod := e2epod.NewAgnhostPod(fr.Namespace.Name, serverPodName, nil, nil, nil, cmd...)
		serverPod.Spec.NodeName = serverPodNode.Name
		e2epod.NewPodClient(fr).CreateSync(context.TODO(), serverPod)

		// Create a client pod on zone - zone-2
		cmd = []string{}
		clientPod := e2epod.NewAgnhostPod(fr.Namespace.Name, clientPodName, nil, nil, nil, cmd...)
		clientPod.Spec.NodeName = clientPodNode.Name
		e2epod.NewPodClient(fr).CreateSync(context.TODO(), clientPod)

		ginkgo.By("Checking that the client-pod can connect to the server-pod when they are not part of any transport zones")
		err := checkPodsInterconnectivity(clientPod, serverPod, fr.Namespace.Name, cs, false)
		framework.ExpectNoError(err, "failed to check pods interconnectivity")

		// Change the zone of client-pod node to that of server-pod node
		ginkgo.By("Setting different transport zones for two nodes")
		setTransportZone(clientPodNode, []string{"tz1"}, cs)
		setTransportZone(serverPodNode, []string{"tz2"}, cs)

		ginkgo.By("Checking that the client-pod cannot connect to the server pod when they are in different transport zones")
		err = checkPodsInterconnectivity(clientPod, serverPod, fr.Namespace.Name, cs, true)
		framework.ExpectNoError(err, "failed to check pods interconnectivity")

		// Move client pod node to the same transport zone as server's
		ginkgo.By("Moving client pod node to server pod node transport zone")
		setTransportZone(clientPodNode, []string{"tz2"}, cs)

		ginkgo.By("Checking that the client-pod can connect to the server-pod when they are in the same transport zone")
		err = checkPodsInterconnectivity(clientPod, serverPod, fr.Namespace.Name, cs, false)
		framework.ExpectNoError(err, "failed to check pods interconnectivity")

		// Unset client pod node transport zone
		ginkgo.By("Moving client pod node out of all transport zones")
		setTransportZone(clientPodNode, []string{}, cs)

		ginkgo.By("Checking that the client-pod cannot connect to the server-pod when server is part of a transport zone")
		err = checkPodsInterconnectivity(clientPod, serverPod, fr.Namespace.Name, cs, true)
		framework.ExpectNoError(err, "failed to check pods interconnectivity")

		// Set multiple transport zones for the client
		ginkgo.By("Moving client pod node to both transport zones")
		setTransportZone(clientPodNode, []string{"tz1", "tz2"}, cs)

		ginkgo.By("Checking that the client-pod can connect to the server-pod when one of its transport zones intersect with server node")
		err = checkPodsInterconnectivity(clientPod, serverPod, fr.Namespace.Name, cs, false)
		framework.ExpectNoError(err, "failed to check pods interconnectivity")
	})
})
