// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	iputils "github.com/containernetworking/plugins/pkg/ip"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eendpointslice "k8s.io/kubernetes/test/e2e/framework/endpointslice"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	utilnet "k8s.io/utils/net"
)

// The IPv4 source-selection sysctl is node-wide, so these tests must run serially.
var _ = ginkgo.Describe("Service host-generated PMTUD", feature.Service, feature.NetworkSegmentation, ginkgo.Serial, func() {
	f := wrappedTestFramework("service-pmtud")
	const (
		backendPort   = 8443
		bottleneckMTU = 1280
		clientMSS     = 1360
		responseBytes = 64 * 1024
	)

	for _, family := range []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol} {
		ginkgo.DescribeTable(fmt.Sprintf("delivers related ICMP errors to the TCP backend [%s]", family), func(remoteBackend bool) {
			if !isNetworkSegmentationEnabled() {
				ginkgo.Skip("requires network segmentation")
			}
			if family == v1.IPv4Protocol && !isIPv4Supported(f.ClientSet) || family == v1.IPv6Protocol && !isIPv6Supported(f.ClientSet) {
				ginkgo.Skip(fmt.Sprintf("requires %s", family))
			}
			nodeCtx, cancelNode := context.WithTimeout(context.Background(), framework.SingleCallTimeout)
			defer cancelNode()
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(nodeCtx, f.ClientSet, 2)
			framework.ExpectNoError(err)
			if remoteBackend && len(nodes.Items) < 2 {
				ginkgo.Skip("cross-node backend requires at least two ready, schedulable nodes")
			}
			ingressNode := nodes.Items[0].Name
			backendNode := ingressNode
			if remoteBackend {
				backendNode = nodes.Items[1].Name
			}
			provider := infraprovider.Get()
			providerCtx := provider.NewTestContext()
			// An unbridged secondary interface forces NodePort ingress through Linux
			// in both gateway modes; the primary uplink can bypass it in shared mode.
			ginkgo.By("attaching an isolated secondary network to the ingress node")
			subnet := "198.18.231.0/24"
			if family == v1.IPv6Protocol {
				subnet = "fd00:1319:3655::/64"
			}
			network, err := providerCtx.CreateNetwork(f.Namespace.Name+"-client", subnet)
			framework.ExpectNoError(err)
			ingressInterface, err := providerCtx.AttachNetwork(network, ingressNode)
			framework.ExpectNoError(err)
			// Permit forwarding only on this temporary interface, including when
			// the cluster disables forwarding by default. Detaching it cleans up.
			if family == v1.IPv4Protocol {
				_, err = provider.ExecK8NodeCommand(ingressNode, []string{"sysctl", "-w",
					fmt.Sprintf("net.ipv4.conf.%s.forwarding=1", ingressInterface.InfName)})
			} else {
				_, err = provider.ExecK8NodeCommand(ingressNode, []string{"sh", "-ec", `
 sysctl -w "net.ipv6.conf.$1.forwarding=1"
 if [ -f "/proc/sys/net/ipv6/conf/$1/force_forwarding" ]; then
     sysctl -w "net.ipv6.conf.$1.force_forwarding=1"
 fi`, "sh", ingressInterface.InfName})
			}
			framework.ExpectNoError(err, "enable %s forwarding on interface %s", family, ingressInterface.InfName)

			familyFlag, prefixLength := "-4", "/32"
			ingressIP := ingressInterface.IPv4
			if family == v1.IPv6Protocol {
				familyFlag, prefixLength = "-6", "/128"
				ingressIP = ingressInterface.IPv6
			}
			gomega.Expect(ingressIP).NotTo(gomega.BeEmpty())
			masqueradeSubnets, err := util.ParseNodeMasqueradeSubnet(&nodes.Items[0])
			framework.ExpectNoError(err, "read masquerade subnets on ingress node %s", ingressNode)
			masqueradeSubnet := getFirstCIDROfFamily(utilnet.IPFamilyOfString(ingressIP), masqueradeSubnets)
			gomega.Expect(masqueradeSubnet).NotTo(gomega.BeNil(), "ingress node %s must advertise a %s masquerade subnet", ingressNode, family)
			// The host masquerade address is allocated two addresses after the subnet base.
			hostMasqueradeIP := iputils.NextIP(iputils.NextIP(masqueradeSubnet.IP)).String()
			if family == v1.IPv4Protocol {
				ginkgo.By("using the default ICMP source selection on the ingress node")
				const sourceSysctl = "net.ipv4.icmp_errors_use_inbound_ifaddr"
				original, err := provider.ExecK8NodeCommand(ingressNode, []string{"sysctl", "-n", sourceSysctl})
				framework.ExpectNoError(err)
				ginkgo.DeferCleanup(func() {
					_, err := provider.ExecK8NodeCommand(ingressNode, []string{"sysctl", "-w", sourceSysctl + "=" + strings.TrimSpace(original)})
					framework.ExpectNoError(err, "restore ICMP source selection")
				})
				_, err = provider.ExecK8NodeCommand(ingressNode, []string{"sysctl", "-w", sourceSysctl + "=0"})
				framework.ExpectNoError(err)
			}
			ginkgo.By(fmt.Sprintf("creating a fresh backend and %s NodePort Service", family))
			labels := map[string]string{"app": "pmtud-backend"}
			backend := e2epod.NewAgnhostPod(f.Namespace.Name, "backend", nil, nil, nil,
				"netexec", fmt.Sprintf("--http-port=%d", backendPort), "--udp-port=-1")
			backend.Labels = labels
			backend.Spec.NodeName = backendNode
			backend.Spec.Containers[0].ReadinessProbe = &v1.Probe{
				ProbeHandler: v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{Path: "/hostname", Port: intstr.FromInt32(backendPort)}},
			}
			podCtx, cancelPod := context.WithTimeout(context.Background(), framework.PodStartTimeout)
			defer cancelPod()
			backend = e2epod.NewPodClient(f).CreateSync(podCtx, backend)
			backendMTU, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", backend.Name, "--", "cat", "/sys/class/net/eth0/mtu")
			framework.ExpectNoError(err)
			mtu, err := strconv.Atoi(strings.TrimSpace(backendMTU))
			framework.ExpectNoError(err)
			gomega.Expect(mtu).To(gomega.BeNumerically(">", bottleneckMTU), "backend must send packets larger than the bottleneck")
			probing, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", backend.Name, "--", "sysctl", "-n", "net.ipv4.tcp_mtu_probing")
			framework.ExpectNoError(err)
			gomega.Expect(strings.TrimSpace(probing)).To(gomega.Equal("0"), "black-hole probing must not mask missing ICMP errors")

			serviceCtx, cancelService := context.WithTimeout(context.Background(), framework.SingleCallTimeout)
			defer cancelService()
			service, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Create(serviceCtx, &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "pmtud"},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeNodePort, IPFamilies: []v1.IPFamily{family},
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyCluster,
					Selector:              labels,
					Ports:                 []v1.ServicePort{{Port: 443, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt32(backendPort)}},
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			framework.ExpectNoError(e2eendpointslice.WaitForEndpointCount(serviceCtx, f.ClientSet, f.Namespace.Name, service.Name, 1))

			// Host-routed NodePort ingress DNATs to the ClusterIP before entering
			// the host-to-Service conntrack zone in either gateway mode.
			ginkgo.By("creating an external client with an MSS larger than the bottleneck")
			client, err := providerCtx.CreateExternalContainer(infraapi.ExternalContainer{
				Name: f.Namespace.Name + "-client", Image: images.AgnHost(), Network: network, CmdArgs: []string{"pause"},
			})
			framework.ExpectNoError(err)
			clientIP := client.GetIPv4()
			if family == v1.IPv6Protocol {
				clientIP = client.GetIPv6()
			}
			gomega.Expect(clientIP).NotTo(gomega.BeEmpty())
			_, err = provider.ExecExternalContainerCommand(client, []string{"ip", familyFlag, "route", "add", ingressIP + prefixLength,
				"dev", provider.ExternalContainerPrimaryInterfaceName(), "advmss", strconv.Itoa(clientMSS)})
			framework.ExpectNoError(err)

			ginkgo.By("constraining only the host route to this client, without changing the interface MTU")
			_, err = provider.ExecK8NodeCommand(ingressNode, []string{"ip", familyFlag, "route", "add", clientIP + prefixLength, "dev", ingressInterface.InfName, "mtu", strconv.Itoa(bottleneckMTU)})
			framework.ExpectNoError(err, "failed to add %s host route to client %s on interface %s with MTU %d", family, clientIP, ingressInterface.InfName, bottleneckMTU)
			ginkgo.DeferCleanup(func() {
				_, err := provider.ExecK8NodeCommand(ingressNode, []string{"ip", familyFlag, "route", "del", clientIP + prefixLength, "dev", ingressInterface.InfName})
				framework.ExpectNoError(err, "remove the test client's route")
			})
			// This source is covered by the already-masqueraded UDN flow even
			// though the error describes a default-network Service connection.
			route, err := provider.ExecK8NodeCommand(ingressNode, []string{"ip", familyFlag, "-j", "route", "get", service.Spec.ClusterIP})
			framework.ExpectNoError(err)
			var routes []struct {
				PrefSrc string `json:"prefsrc"`
			}
			framework.ExpectNoError(json.Unmarshal([]byte(route), &routes))
			gomega.Expect(routes).To(gomega.HaveLen(1))
			gomega.Expect(routes[0].PrefSrc).To(gomega.Equal(hostMasqueradeIP))

			baseURL := "http://" + net.JoinHostPort(ingressIP, strconv.Itoa(int(service.Spec.Ports[0].NodePort)))
			curl := func(path string) (string, error) {
				return provider.ExecExternalContainerCommand(client, []string{"curl", "--noproxy", "*", "--fail", "--silent", "--show-error",
					"--retry", "0", "--connect-timeout", "5", "--max-time", "20", baseURL + path})
			}
			ginkgo.By("checking small responses while the Service is programmed")
			gomega.Eventually(func() (string, error) { return curl("/hostname") }, time.Minute, time.Second).
				Should(gomega.Equal(backend.Name))

			pmtudErrors := func() uint64 {
				if family == v1.IPv6Protocol {
					out, err := provider.ExecK8NodeCommand(ingressNode, []string{"cat", "/proc/net/snmp6"})
					framework.ExpectNoError(err)
					for _, line := range strings.Split(out, "\n") {
						fields := strings.Fields(line)
						if len(fields) == 2 && fields[0] == "Icmp6OutPktTooBigs" {
							n, err := strconv.ParseUint(fields[1], 10, 64)
							framework.ExpectNoError(err)
							return n
						}
					}
					ginkgo.Fail("Icmp6OutPktTooBigs counter not found")
					return 0
				}
				out, err := provider.ExecK8NodeCommand(ingressNode, []string{"cat", "/proc/net/snmp"})
				framework.ExpectNoError(err)
				lines := strings.Split(out, "\n")
				for i, line := range lines {
					fields := strings.Fields(line)
					if len(fields) == 0 || fields[0] != "Ip:" {
						continue
					}
					for j, field := range fields {
						if field == "FragFails" {
							values := strings.Fields(lines[i+1])
							n, err := strconv.ParseUint(values[j], 10, 64)
							framework.ExpectNoError(err)
							return n
						}
					}
				}
				ginkgo.Fail("Ip FragFails counter not found")
				return 0
			}
			before := pmtudErrors()
			ginkgo.By("receiving a large response through PMTUD, without retrying the transfer")
			// Keep the request small: only the response should need PMTUD.
			body, err := curl("/shell?cmd=" + url.QueryEscape(fmt.Sprintf("printf '%%0%dd' 0", responseBytes)))
			framework.ExpectNoError(err, "related ICMP errors must reach the backend")
			var response struct {
				Output string `json:"output"`
			}
			framework.ExpectNoError(json.Unmarshal([]byte(body), &response))
			gomega.Expect(response.Output).To(gomega.Equal(strings.Repeat("0", responseBytes)))
			gomega.Expect(pmtudErrors()).To(gomega.BeNumerically(">", before), "the transfer must exercise host-generated PMTUD errors")
		},
			ginkgo.Entry("with a backend on the ingress node", false),
			ginkgo.Entry("with a backend on another node", true),
		)
	}
})
