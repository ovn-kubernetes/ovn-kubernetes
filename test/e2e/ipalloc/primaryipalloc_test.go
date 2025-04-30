package ipalloc

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestUtilSuite(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "node ip alloc suite")
}

// mustParseCIDRIncIP parses the IP and CIDR. It adds the IP to the returned IPNet.
func mustParseCIDRIncIP(cidr string) *net.IPNet {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(fmt.Sprintf("failed to parse CIDR %q: %v", cidr, err))
	}
	ipNet.IP = ip
	return ipNet
}

type network struct {
	ip   string
	mask string
}

type node struct {
	v4 network
	v6 network
}

func TestIPAlloc(t *testing.T) {
	g := gomega.NewWithT(t)

	tests := []struct {
		desc                     string
		existingPrimaryNodeIPs   []node
		expectedFromAllocateNext []string
	}{
		{
			desc:                   "IPv4",
			existingPrimaryNodeIPs: []node{{v4: network{ip: "192.168.1.1", mask: "16"}}, {v4: network{ip: "192.168.1.2", mask: "16"}}},
		},
		{
			desc:                   "IPv6",
			existingPrimaryNodeIPs: []node{{v6: network{ip: "fc00:f853:ccd:e793::5", mask: "64"}}, {v6: network{ip: "fc00:f853:ccd:e793::6", mask: "64"}}},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			cs := fake.NewSimpleClientset(getNodesWithIPs(tc.existingPrimaryNodeIPs))
			pipa, err := newPrimaryIPAllocator(cs.CoreV1().Nodes())
			if err != nil {
				t.Errorf(err.Error())
				return
			}
			existingIPv4IPs := []string{}
			existingIPv6IPs := []string{}
			allocatedIPv4IPs := []string{}
			allocatedIPv6IPs := []string{}
			for _, existingPrimaryNodeIP := range tc.existingPrimaryNodeIPs {
				if existingPrimaryNodeIP.v4.ip != "" {
					existingIPv4IPs = append(existingIPv4IPs, existingPrimaryNodeIP.v4.ip)
					nextIPv4, err := pipa.AllocateNextV4()
					g.Expect(err).ToNot(gomega.HaveOccurred(), "should success allocating next IPv4 address")
					g.Expect(nextIPv4).ToNot(gomega.BeNil(), "should allocate next IPv4 address")
					allocatedIPv4IPs = append(allocatedIPv4IPs, nextIPv4.String())
				}

				if existingPrimaryNodeIP.v6.ip != "" {
					existingIPv6IPs = append(existingIPv6IPs, existingPrimaryNodeIP.v6.ip)
					nextIPv6, err := pipa.AllocateNextV6()
					g.Expect(err).ToNot(gomega.HaveOccurred(), "should success allocating next IPv6 address")
					g.Expect(nextIPv6).ToNot(gomega.BeNil(), "should allocate next IPv6 address")
					allocatedIPv6IPs = append(allocatedIPv6IPs, nextIPv6.String())
				}
			}
			if len(existingIPv4IPs) > 0 {
				g.Expect(allocatedIPv4IPs).NotTo(gomega.ContainElements(existingIPv4IPs))
			}
			if len(existingIPv6IPs) > 0 {
				g.Expect(allocatedIPv6IPs).NotTo(gomega.ContainElements(existingIPv6IPs))
			}
		})
	}

}

func getNodesWithIPs(nodesSpec []node) runtime.Object {
	nodeObjs := make([]corev1.Node, 0, len(nodesSpec))
	getIPMaskFn := func(ip, mask string) string {
		if ip == "" || mask == "" {
			return ""
		}
		return fmt.Sprintf("%s/%s", ip, mask)
	}

	getArrayForHostCIDRs := func(n node) string {
		cidrs := []string{}
		if cidr := getIPMaskFn(n.v4.ip, n.v4.mask); cidr != "" {
			cidrs = append(cidrs, fmt.Sprintf("\"%s\"", cidr))
		}
		if cidr := getIPMaskFn(n.v6.ip, n.v6.mask); cidr != "" {
			cidrs = append(cidrs, fmt.Sprintf("\"%s\"", cidr))
		}
		return fmt.Sprintf("[%s]", strings.Join(cidrs, ","))
	}

	for i, node := range nodesSpec {
		nodePrimaryIfAddrValue := fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}",
			getIPMaskFn(node.v4.ip, node.v4.mask), getIPMaskFn(node.v6.ip, node.v6.mask))
		node1Annotations := map[string]string{
			"k8s.ovn.org/node-primary-ifaddr": nodePrimaryIfAddrValue,
			util.OVNNodeHostCIDRs:             getArrayForHostCIDRs(node),
		}
		nodeObjs = append(nodeObjs, getNodeObj(fmt.Sprintf("node%d", i), node1Annotations, map[string]string{}))
	}
	nl := &corev1.NodeList{Items: nodeObjs}
	return nl
}

func getNodeObj(nodeName string, annotations, labels map[string]string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: annotations,
			Labels:      labels,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
}
