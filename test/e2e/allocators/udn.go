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
		v4Exclusions, v6Exclusions := machineNetworkExclusions()
		udnV4 = newSubnetSpec(udnSubnets, v4Exclusions)
		udnV6 = newSubnetSpec(udnSubnets6, v6Exclusions)
	})
}

func machineNetworkExclusions() (ipv4, ipv6 []string) {
	v4, v6, err := getMachineNetworkSubnets()
	if err != nil {
		framework.Logf("Warning: failed to get machine network subnets for exclusion: %v", err)
		return nil, nil
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
// annotations (k8s.ovn.org/node-primary-ifaddr). It returns the unique IPv4
// and IPv6 CIDR networks found across all nodes. When KUBECONFIG is not set,
// the call is a no-op and returns empty sets.
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
		ifAddr, err := util.GetNodeIfAddrAnnotation(&nodes.Items[i])
		if err != nil {
			return ipv4, ipv6, fmt.Errorf("failed to get interface address annotation for node %q: %w", nodes.Items[i].Name, err)
		}
		for _, addr := range []string{ifAddr.IPv4, ifAddr.IPv6} {
			if addr == "" {
				continue
			}
			_, cidr, err := net.ParseCIDR(addr)
			if err != nil {
				return ipv4, ipv6, fmt.Errorf("failed to parse CIDR %q for node %q: %w", addr, nodes.Items[i].Name, err)
			}
			if cidr == nil {
				return ipv4, ipv6, fmt.Errorf("parsed nil CIDR from %q for node %q", addr, nodes.Items[i].Name)
			}
			if cidr.IP.To4() != nil {
				ipv4.Insert(cidr.String())
			} else {
				ipv6.Insert(cidr.String())
			}
		}
	}
	return ipv4, ipv6, nil
}
