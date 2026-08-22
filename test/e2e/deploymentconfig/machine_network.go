// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deploymentconfig

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// GetMachineNetworkSubnets retrieves the machine network subnets from node
// annotations (k8s.ovn.org/node-primary-ifaddr). It returns the unique IPv4
// and IPv6 CIDR networks found across all nodes. When config is nil the call
// is a no-op and returns nil sets.
func GetMachineNetworkSubnets(config *rest.Config) (ipv4, ipv6 sets.Set[string], err error) {
	if config == nil {
		return nil, nil, nil
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	ipv4 = sets.New[string]()
	ipv6 = sets.New[string]()
	for i := range nodes.Items {
		ifAddr, err := util.GetNodeIfAddrAnnotation(&nodes.Items[i])
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get interface address annotation for node %q: %w", nodes.Items[i].Name, err)
		}
		for _, addr := range []string{ifAddr.IPv4, ifAddr.IPv6} {
			if addr == "" {
				continue
			}
			_, cidr, err := net.ParseCIDR(addr)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to parse CIDR %q for node %q: %w", addr, nodes.Items[i].Name, err)
			}
			if cidr == nil {
				return nil, nil, fmt.Errorf("parsed nil CIDR from %q for node %q", addr, nodes.Items[i].Name)
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
