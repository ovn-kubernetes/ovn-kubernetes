// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package allocators

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubernetes/test/e2e/framework"
)

var (
	udnOnce      sync.Once
	udnV4, udnV6 subnetSpec
)

func initSubnetSpecs() {
	udnOnce.Do(func() {
		v4Exclusions, v6Exclusions := infrastructureNetworkExclusions()
		udnV4 = newSubnetSpec(udnSubnets, v4Exclusions)
		udnV6 = newSubnetSpec(udnSubnets6, v6Exclusions)
	})
}

func infrastructureNetworkExclusions() (ipv4, ipv6 []string) {
	v4, v6, err := getMachineNetworkSubnets()
	if err != nil {
		framework.Logf("Warning: failed to get machine network subnets for exclusion: %v", err)
	}
	// infraprovider.Get() panics when the provider is not set, which happens
	// during test listing where no cluster is available.
	if infraprovider.IsSet() {
		infraV4, infraV6 := infraprovider.Get().InfrastructureNetworkExclusions()
		v4 = v4.Union(infraV4)
		v6 = v6.Union(infraV6)
	}
	return v4.UnsortedList(), v6.UnsortedList()
}

// GetFirstUDNSubnets always allocates the first UDN IPv4 and IPv6 subnet
// within the dedicated UDN subnet broader range. Used when overlaps across UDNs
// are not a concern but still prevents overlaps with other subnets.
func GetFirstUDNSubnets() (ipv4, ipv6 string) {
	subnets4, subnets6 := GetNthFirstUDNSubnets(1)
	return subnets4[0], subnets6[0]
}

// GetNthFirstUDNSubnets returns the first n UDN IPv4 and IPv6 subnets within
// the dedicated UDN subnet broader range. Used when overlaps across UDNs are
// not a concern but still prevents overlaps with other subnets.
func GetNthFirstUDNSubnets(n int) (ipv4, ipv6 []string) {
	if n < 1 {
		panic("GetNthFirstUDNSubnets: n must be >= 1")
	}
	initSubnetSpecs()
	if n > udnV4.usable() || n > udnV6.usable() {
		panic("GetNthFirstUDNSubnets: not enough free subnets available")
	}

	ipv4 = make([]string, 0, n)
	ipv6 = make([]string, 0, n)
	for i := 1; i < n+1; i++ {
		udnV4Idx := udnV4.nthFree(i)
		udnV6Idx := udnV6.nthFree(i)
		ipv4 = append(ipv4, udnV4.cidr(udnV4Idx))
		ipv6 = append(ipv6, udnV6.cidr(udnV6Idx))
	}
	return ipv4, ipv6
}

// getMachineNetworkSubnets retrieves the machine network subnets from node
// annotations. It first attempts to use the cloud egress IP config annotation
// (cloud.network.openshift.io/egress-ipconfig) which provides the actual
// network CIDR (e.g. 10.0.128.0/24). If that annotation is not present (e.g.
// on non-cloud platforms), it falls back to the node primary interface address
// annotation (k8s.ovn.org/node-primary-ifaddr) which may only provide a host
// address (e.g. 10.0.128.4/32). It returns the unique IPv4 and IPv6 CIDR
// networks found across all nodes. When KUBECONFIG is not set, the call is a
// no-op and returns empty sets.
func getMachineNetworkSubnets() (sets.Set[string], sets.Set[string], error) {
	ipv4 := sets.New[string]()
	ipv6 := sets.New[string]()
	kubeConfig := os.Getenv("KUBECONFIG")
	if kubeConfig == "" {
		return ipv4, ipv6, nil
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		return ipv4, ipv6, err
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return ipv4, ipv6, fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ipv4, ipv6, fmt.Errorf("failed to list nodes: %w", err)
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		v4CIDR, v6CIDR, err := machineNetworkCIDRsFromNode(node)
		if err != nil {
			return ipv4, ipv6, err
		}
		if v4CIDR != "" {
			ipv4.Insert(v4CIDR)
		}
		if v6CIDR != "" {
			ipv6.Insert(v6CIDR)
		}
	}
	return ipv4, ipv6, nil
}

// machineNetworkCIDRsFromNode extracts machine network CIDRs from a node.
// It prefers the cloud egress IP config annotation which has the real subnet
// CIDR, falling back to the node primary interface address annotation.
func machineNetworkCIDRsFromNode(node *corev1.Node) (v4, v6 string, err error) {
	egressIPConfig, err := util.ParseCloudEgressIPConfig(node)
	if err == nil {
		if egressIPConfig.V4.Net != nil {
			v4 = egressIPConfig.V4.Net.String()
		}
		if egressIPConfig.V6.Net != nil {
			v6 = egressIPConfig.V6.Net.String()
		}
		return v4, v6, nil
	}
	if !util.IsAnnotationNotSetError(err) {
		return "", "", fmt.Errorf("failed to parse cloud egress IP config for node %q: %w", node.Name, err)
	}

	// Fall back to node-primary-ifaddr annotation.
	ifAddr, err := util.GetNodeIfAddrAnnotation(node)
	if err != nil {
		return "", "", fmt.Errorf("failed to get interface address annotation for node %q: %w", node.Name, err)
	}
	for _, addr := range []string{ifAddr.IPv4, ifAddr.IPv6} {
		if addr == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(addr)
		if err != nil {
			return "", "", fmt.Errorf("failed to parse CIDR %q for node %q: %w", addr, node.Name, err)
		}
		if cidr == nil {
			return "", "", fmt.Errorf("parsed nil CIDR from %q for node %q", addr, node.Name)
		}
		if cidr.IP.To4() != nil {
			v4 = cidr.String()
		} else {
			v6 = cidr.String()
		}
	}
	return v4, v6, nil
}
