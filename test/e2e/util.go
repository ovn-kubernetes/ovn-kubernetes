package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	testutils "k8s.io/kubernetes/test/utils"
	utilnet "k8s.io/utils/net"
)

const ovnNamespace = "ovn-kubernetes"

// newAgnhostPod returns a pod that uses the agnhost image. The image's binary supports various subcommands
// that behave the same, no matter the underlying OS.
func newAgnhostPod(name string, command ...string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    name,
					Image:   agnhostImage,
					Command: command,
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
}

// IsIPv6Cluster returns true if the kubernetes default service is IPv6
func IsIPv6Cluster(c clientset.Interface) bool {
	// Get the ClusterIP of the kubernetes service created in the default namespace
	svc, err := c.CoreV1().Services(metav1.NamespaceDefault).Get(context.Background(), "kubernetes", metav1.GetOptions{})
	if err != nil {
		framework.Failf("Failed to get kubernetes service ClusterIP: %v", err)
	}
	if utilnet.IsIPv6String(svc.Spec.ClusterIP) {
		return true
	}
	return false
}

// PodAnnotation describes the assigned network details for a single pod network. (The
// actual annotation may include the equivalent of multiple PodAnnotations.)
type PodAnnotation struct {
	// IPs are the pod's assigned IP addresses/prefixes
	IPs []*net.IPNet
	// MAC is the pod's assigned MAC address
	MAC net.HardwareAddr
	// Gateways are the pod's gateway IP addresses; note that there may be
	// fewer Gateways than IPs.
	Gateways []net.IP
	// Routes are additional routes to add to the pod's network namespace
	Routes []PodRoute
}

// PodRoute describes any routes to be added to the pod's network namespace
type PodRoute struct {
	// Dest is the route destination
	Dest *net.IPNet
	// NextHop is the IP address of the next hop for traffic destined for Dest
	NextHop net.IP
}

// Internal struct used to marshal PodAnnotation to the pod annotation
type podAnnotation struct {
	IPs      []string   `json:"ip_addresses"`
	MAC      string     `json:"mac_address"`
	Gateways []string   `json:"gateway_ips,omitempty"`
	Routes   []podRoute `json:"routes,omitempty"`

	IP      string `json:"ip_address,omitempty"`
	Gateway string `json:"gateway_ip,omitempty"`
}

// Internal struct used to marshal PodRoute to the pod annotation
type podRoute struct {
	Dest    string `json:"dest"`
	NextHop string `json:"nextHop"`
}

type annotationNotSetError struct {
	msg string
}

func (anse annotationNotSetError) Error() string {
	return anse.msg
}

// newAnnotationNotSetError returns an error for an annotation that is not set
func newAnnotationNotSetError(format string, args ...interface{}) error {
	return annotationNotSetError{msg: fmt.Sprintf(format, args...)}
}

// UnmarshalPodAnnotation returns the default network info from pod.Annotations
func unmarshalPodAnnotation(annotations map[string]string) (*PodAnnotation, error) {
	ovnAnnotation, ok := annotations[podNetworkAnnotation]
	if !ok {
		return nil, newAnnotationNotSetError("could not find OVN pod annotation in %v", annotations)
	}

	podNetworks := make(map[string]podAnnotation)
	if err := json.Unmarshal([]byte(ovnAnnotation), &podNetworks); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ovn pod annotation %q: %v",
			ovnAnnotation, err)
	}
	tempA := podNetworks["default"]
	a := &tempA

	podAnnotation := &PodAnnotation{}
	var err error

	podAnnotation.MAC, err = net.ParseMAC(a.MAC)
	if err != nil {
		return nil, fmt.Errorf("failed to parse pod MAC %q: %v", a.MAC, err)
	}

	if len(a.IPs) == 0 {
		if a.IP == "" {
			return nil, fmt.Errorf("bad annotation data (neither ip_address nor ip_addresses is set)")
		}
		a.IPs = append(a.IPs, a.IP)
	} else if a.IP != "" && a.IP != a.IPs[0] {
		return nil, fmt.Errorf("bad annotation data (ip_address and ip_addresses conflict)")
	}
	for _, ipstr := range a.IPs {
		ip, ipnet, err := net.ParseCIDR(ipstr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pod IP %q: %v", ipstr, err)
		}
		ipnet.IP = ip
		podAnnotation.IPs = append(podAnnotation.IPs, ipnet)
	}

	if len(a.Gateways) == 0 {
		if a.Gateway != "" {
			a.Gateways = append(a.Gateways, a.Gateway)
		}
	} else if a.Gateway != "" && a.Gateway != a.Gateways[0] {
		return nil, fmt.Errorf("bad annotation data (gateway_ip and gateway_ips conflict)")
	}
	for _, gwstr := range a.Gateways {
		gw := net.ParseIP(gwstr)
		if gw == nil {
			return nil, fmt.Errorf("failed to parse pod gateway %q", gwstr)
		}
		podAnnotation.Gateways = append(podAnnotation.Gateways, gw)
	}

	for _, r := range a.Routes {
		route := PodRoute{}
		_, route.Dest, err = net.ParseCIDR(r.Dest)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pod route dest %q: %v", r.Dest, err)
		}
		if route.Dest.IP.IsUnspecified() {
			return nil, fmt.Errorf("bad podNetwork data: default route %v should be specified as gateway", route)
		}
		if r.NextHop != "" {
			route.NextHop = net.ParseIP(r.NextHop)
			if route.NextHop == nil {
				return nil, fmt.Errorf("failed to parse pod route next hop %q", r.NextHop)
			} else if utilnet.IsIPv6(route.NextHop) != utilnet.IsIPv6CIDR(route.Dest) {
				return nil, fmt.Errorf("pod route %s has next hop %s of different family", r.Dest, r.NextHop)
			}
		}
		podAnnotation.Routes = append(podAnnotation.Routes, route)
	}

	return podAnnotation, nil
}

func nodePortServiceSpecFrom(svcName string, httpPort, updPort, clusterHTTPPort, clusterUDPPort int, selector map[string]string, local v1.ServiceExternalTrafficPolicyType) *v1.Service {
	preferDual := v1.IPFamilyPolicyPreferDualStack

	res := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: svcName,
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeNodePort,
			Ports: []v1.ServicePort{
				{Port: int32(clusterHTTPPort), Name: "http", Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt(httpPort)},
				{Port: int32(clusterUDPPort), Name: "udp", Protocol: v1.ProtocolUDP, TargetPort: intstr.FromInt(updPort)},
			},
			Selector:              selector,
			IPFamilyPolicy:        &preferDual,
			ExternalTrafficPolicy: local,
		},
	}

	return res
}

func externalIPServiceSpecFrom(svcName string, httpPort, updPort, clusterHTTPPort, clusterUDPPort int, selector map[string]string, externalIps []string) *v1.Service {
	preferDual := v1.IPFamilyPolicyPreferDualStack

	res := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: svcName,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{Port: int32(clusterHTTPPort), Name: "http", Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt(httpPort)},
				{Port: int32(clusterUDPPort), Name: "udp", Protocol: v1.ProtocolUDP, TargetPort: intstr.FromInt(updPort)},
			},
			Selector:       selector,
			IPFamilyPolicy: &preferDual,
			ExternalIPs:    externalIps,
		},
	}

	return res
}

// leverages a container running the netexec command to send a "hostname" request to a target running
// netexec on the given target host / protocol / port
// returns either the name of backend pod or "Timeout" if the curl request timed out
func pokeEndpointHostname(clientContainer, protocol, targetHost string, targetPort int32) string {
	ipPort := net.JoinHostPort("localhost", "80")
	cmd := []string{"docker", "exec", clientContainer}

	// we leverage the dial command from netexec, that is already supporting multiple protocols
	curlCommand := strings.Split(fmt.Sprintf("curl -g -q -s http://%s/dial?request=hostname&protocol=%s&host=%s&port=%d&tries=1",
		ipPort,
		protocol,
		targetHost,
		targetPort), " ")

	cmd = append(cmd, curlCommand...)
	res, err := runCommand(cmd...)
	framework.ExpectNoError(err, "failed to run command on external container")
	hostName, err := parseNetexecResponse(res)
	if err != nil {
		fmt.Printf("FAILED Command was %s", curlCommand)
		fmt.Printf("FAILED Response was %v", res)
	}
	framework.ExpectNoError(err)

	return hostName
}

// leverages a container running the netexec command to send a "clientip" request to a target running
// netexec on the given target host / protocol / port
// returns either the src ip of the packet or "Timeout" if the curl request timed out
func pokeEndpointClientIP(clientContainer, protocol, targetHost string, targetPort int32) string {
	ipPort := net.JoinHostPort("localhost", "80")
	cmd := []string{"docker", "exec", clientContainer}

	// we leverage the dial command from netexec, that is already supporting multiple protocols
	curlCommand := strings.Split(fmt.Sprintf("curl -g -q -s http://%s/dial?request=clientip&protocol=%s&host=%s&port=%d&tries=1",
		ipPort,
		protocol,
		targetHost,
		targetPort), " ")

	cmd = append(cmd, curlCommand...)
	res, err := runCommand(cmd...)
	framework.ExpectNoError(err, "failed to run command on external container")
	clientIP, err := parseNetexecResponse(res)
	framework.ExpectNoError(err)
	ip, _, err := net.SplitHostPort(clientIP)
	if err != nil {
		fmt.Printf("FAILED Command was %s", curlCommand)
		fmt.Printf("FAILED Response was %v", res)
	}
	framework.ExpectNoError(err, "failed to parse client ip:port")

	return ip
}

// leverages a container running the netexec command to send a request to a target running
// netexec on the given target host / protocol / port
// returns either the name of backend pod or "Timeout" if the curl request timed out
func curlInContainer(clientContainer, protocol, targetHost string, targetPort int32, endPoint string) string {
	cmd := []string{"docker", "exec", clientContainer}
	if utilnet.IsIPv6String(targetHost) {
		targetHost = fmt.Sprintf("[%s]", targetHost)
	}

	// we leverage the dial command from netexec, that is already supporting multiple protocols
	curlCommand := strings.Split(fmt.Sprintf("curl -g -q -s http://%s:%d/%s",
		targetHost,
		targetPort,
		endPoint), " ")

	cmd = append(cmd, curlCommand...)
	res, err := runCommand(cmd...)
	framework.ExpectNoError(err, "failed to run command on external container")

	return res
}

func parseNetexecResponse(response string) (string, error) {
	res := struct {
		Responses []string `json:"responses"`
		Errors    []string `json:"errors"`
	}{}
	if err := json.Unmarshal([]byte(response), &res); err != nil {
		return "", fmt.Errorf("failed to unmarshal curl response %s", response)
	}
	if len(res.Errors) > 0 {
		if strings.Contains(strings.ToLower(res.Errors[0]), "timeout") {
			return "Timeout", nil
		}
		return "", fmt.Errorf("curl response %s contains errors", response)
	}
	if len(res.Responses) == 0 {
		return "", fmt.Errorf("curl response %s has no values", response)
	}
	return res.Responses[0], nil
}

func nodePortsFromService(service *v1.Service) (int32, int32) {
	var resTCP, resUDP int32
	for _, p := range service.Spec.Ports {
		if p.Protocol == v1.ProtocolTCP {
			resTCP = p.NodePort
		}
		if p.Protocol == v1.ProtocolUDP {
			resUDP = p.NodePort
		}
	}
	return resTCP, resUDP
}

// addressIsIP tells wether the given address is an
// address or a hostname
func addressIsIP(address v1.NodeAddress) bool {
	addr := net.ParseIP(address.Address)
	if addr == nil {
		return false
	}
	return true
}

// Returns pod's ipv4 and ipv6 addresses IN ORDER
func getPodAddresses(pod *v1.Pod) (string, string) {
	var ipv4Res, ipv6Res string
	for _, a := range pod.Status.PodIPs {
		if utilnet.IsIPv4String(a.IP) {
			ipv4Res = a.IP
		}
		if utilnet.IsIPv6String(a.IP) {
			ipv6Res = a.IP
		}
	}
	return ipv4Res, ipv6Res
}

// Returns nodes's ipv4 and ipv6 addresses IN ORDER
func getNodeAddresses(node *v1.Node) (string, string) {
	var ipv4Res, ipv6Res string
	for _, a := range node.Status.Addresses {
		if utilnet.IsIPv4String(a.Address) {
			ipv4Res = a.Address
		}
		if utilnet.IsIPv6String(a.Address) {
			ipv6Res = a.Address
		}
	}
	return ipv4Res, ipv6Res
}

// Returns the container's ipv4 and ipv6 addresses IN ORDER
// related to the given network.
func getContainerAddressesForNetwork(container, network string) (string, string) {
	ipv4Format := fmt.Sprintf("{{.NetworkSettings.Networks.%s.IPAddress}}", network)
	ipv6Format := fmt.Sprintf("{{.NetworkSettings.Networks.%s.GlobalIPv6Address}}", network)

	ipv4, err := runCommand("docker", "inspect", "-f", ipv4Format, container)
	if err != nil {
		framework.Failf("failed to inspect external test container for its IPv4: %v", err)
	}
	ipv6, err := runCommand("docker", "inspect", "-f", ipv6Format, container)
	if err != nil {
		framework.Failf("failed to inspect external test container for its IPv4: %v", err)
	}
	return strings.TrimSuffix(ipv4, "\n"), strings.TrimSuffix(ipv6, "\n")
}

// deletePodSyncNS deletes a pod and wait for its deletion.
// accept the namespace as a parameter.
func deletePodSyncNS(clientSet kubernetes.Interface, namespace, podName string) {
	err := clientSet.CoreV1().Pods(namespace).Delete(context.Background(), podName, metav1.DeleteOptions{})
	framework.ExpectNoError(err, "Failed to delete pod %s in the default namespace", podName)

	gomega.Eventually(func() bool {
		_, err := clientSet.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 3*time.Minute, 5*time.Second).Should(gomega.BeTrue(), "Pod was not being deleted")
}

// waitClusterHealthy ensures we have a given number of ovn-k worker and master nodes,
// as well as all nodes are healthy
func waitClusterHealthy(f *framework.Framework, numMasters int) error {
	return wait.PollImmediate(2*time.Second, 120*time.Second, func() (bool, error) {
		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to list nodes: %w", err)
		}

		numNodes := len(nodes.Items)
		if numNodes == 0 {
			return false, fmt.Errorf("list returned no Node objects, something is wrong")
		}

		// Check that every node is schedulable
		afterNodes, err := e2enode.GetReadySchedulableNodes(f.ClientSet)
		if err != nil {
			return false, fmt.Errorf("failed to look for healthy nodes: %w", err)
		}
		if len(afterNodes.Items) != numNodes {
			framework.Logf("Not enough schedulable nodes, have %d want %d", len(afterNodes.Items), numNodes)
			return false, nil
		}

		podClient := f.ClientSet.CoreV1().Pods("ovn-kubernetes")
		// Ensure all nodes are running and healthy
		podList, err := podClient.List(context.Background(), metav1.ListOptions{
			LabelSelector: "app=ovnkube-node",
		})
		if err != nil {
			return false, fmt.Errorf("failed to list ovn-kube node pods: %w", err)
		}
		if len(podList.Items) != numNodes {
			framework.Logf("Not enough running ovnkube-node pods, want %d, have %d", numNodes, len(podList.Items))
			return false, nil
		}

		for _, pod := range podList.Items {
			if ready, err := testutils.PodRunningReady(&pod); !ready {
				framework.Logf("%v", err)
				return false, nil
			}
		}

		podList, err = podClient.List(context.Background(), metav1.ListOptions{
			LabelSelector: "name=ovnkube-master",
		})
		if err != nil {
			return false, fmt.Errorf("failed to list ovn-kube node pods: %w", err)
		}
		if len(podList.Items) != numMasters {
			framework.Logf("Not enough running ovnkube-master pods, want %d, have %d", numMasters, len(podList.Items))
			return false, nil
		}

		for _, pod := range podList.Items {
			if ready, err := testutils.PodRunningReady(&pod); !ready {
				framework.Logf("%v", err)
				return false, nil
			}
		}

		return true, nil
	})
}

// waitForDaemonSetUpdate waits for the daemon set in a given namespace to be
// successfully rolled out following an update.
//
// If allowedNotReadyNodes is -1, this method returns immediately without waiting.
func waitForDaemonSetUpdate(c clientset.Interface, ns string, dsName string, allowedNotReadyNodes int32, timeout time.Duration) error {
	if allowedNotReadyNodes == -1 {
		return nil
	}

	start := time.Now()
	framework.Logf("Waiting up to %v for daemonset %s in namespace %s to update",
		timeout, dsName, ns)

	return wait.Poll(framework.Poll, timeout, func() (bool, error) {
		ds, err := c.AppsV1().DaemonSets(ns).Get(context.TODO(), dsName, metav1.GetOptions{})
		if err != nil {
			framework.Logf("Error getting daemonset: %s in namespace: %s: %v", dsName, ns, err)
			return false, err
		}

		if ds.Generation <= ds.Status.ObservedGeneration {
			if ds.Status.UpdatedNumberScheduled < ds.Status.DesiredNumberScheduled {
				framework.Logf("Waiting for daemon set %q rollout to finish: %d out of %d new pods have been updated (%d seconds elapsed)", ds.Name,
					ds.Status.UpdatedNumberScheduled, ds.Status.DesiredNumberScheduled, int(time.Since(start).Seconds()))
				return false, nil
			}
			if ds.Status.NumberAvailable < ds.Status.DesiredNumberScheduled {
				framework.Logf("Waiting for daemon set %q rollout to finish: %d of %d updated pods are available (%d seconds elapsed)", ds.Name,
					ds.Status.NumberAvailable, ds.Status.DesiredNumberScheduled, int(time.Since(start).Seconds()))
				return false, nil
			}
			framework.Logf("daemon set %q successfully rolled out", ds.Name)
			return true, nil
		}

		framework.Logf("Waiting for daemon set: %s spec update to be observed...", dsName)
		return false, nil
	})
}

func pokePod(fr *framework.Framework, srcPodName string, dstPodIP string) error {
	stdout, stderr, err := fr.ExecShellInPodWithFullOutput(
		srcPodName,
		fmt.Sprintf("curl --output /dev/stdout -m 1 -I %s:8000 | head -n1", dstPodIP))
	if err == nil && stdout == "HTTP/1.1 200 OK" {
		return nil
	}
	return fmt.Errorf("http request failed; stdout: %s, err: %v", stdout+stderr, err)
}

func assertDenyLogs(targetNodeName string, namespace string, policyName string, expectedAclSeverity string) (bool, error) {
	framework.Logf("collecting the ovn-controller logs for node: %s", targetNodeName)
	targetNodeLog, err := runCommand([]string{"docker", "exec", targetNodeName, "grep", "acl_log", ovnControllerLogPath}...)
	if err != nil {
		return false, fmt.Errorf("error accessing logs in node %s: %v", targetNodeName, err)
	}

	composedPolicyName := fmt.Sprintf("%s_%s", namespace, policyName)
	framework.Logf("Ensuring the *deny* audit log contains: '%s\", verdict=drop' AND 'severity=%s'", composedPolicyName, expectedAclSeverity)
	for _, logLine := range strings.Split(targetNodeLog, "\n") {
		if strings.Contains(logLine, fmt.Sprintf("%s\", verdict=drop", composedPolicyName)) &&
			strings.Contains(logLine, fmt.Sprintf("severity=%s", expectedAclSeverity)) {
			return true, nil
		}
	}
	return false, nil
}
