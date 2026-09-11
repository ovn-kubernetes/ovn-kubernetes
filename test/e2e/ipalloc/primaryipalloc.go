// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ipalloc

import (
	"context"
	"errors"
	"fmt"
	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"net"
	"os"
	"strings"
	"sync"
)

// primaryIPAllocator attempts to allocate an IP in the same subnet as a nodes primary network
type primaryIPAllocator struct {
	mu         *sync.Mutex
	v4         *ipAllocator
	v6         *ipAllocator
	nodeClient v1.NodeInterface
}

// PrimaryIPPoolEnvVar names a CIDR, or a comma separated pair for dual stack, to allocate from
// where no range can be derived from the Node subnets.
const PrimaryIPPoolEnvVar = "OVN_TEST_PRIMARY_IP_POOL"

// errNoRange marks the Node subnets yielding nothing to allocate from, which is a fact about how
// the cluster is addressed. Every other error here is a fault in it and has to fail the run.
var errNoRange = errors.New("no range")

func IsNoRangeError(err error) bool {
	return errors.Is(err, errNoRange)
}

// pia holds no range until initialised, so a run that never initialises it, as the DPU uplink
// lane does not, skips those specs instead of dereferencing nothing.
var pia = &primaryIPAllocator{mu: &sync.Mutex{}}

// InitPrimaryIPAllocator must be called to init IP allocator(s). Callers must be synchronise.
func InitPrimaryIPAllocator(nodeClient v1.NodeInterface) error {
	var err error
	pia, err = newPrimaryIPAllocator(nodeClient, os.Getenv(PrimaryIPPoolEnvVar))
	return err
}

func NewPrimaryIPv4() (net.IP, error) {
	skipWithoutRange(pia.v4, "IPv4")
	return pia.AllocateNextV4()
}

func NewPrimaryIPv6() (net.IP, error) {
	skipWithoutRange(pia.v6, "IPv6")
	return pia.AllocateNextV6()
}

func skipWithoutRange(allocator *ipAllocator, family string) {
	if allocator == nil {
		ginkgo.Skip(fmt.Sprintf("this run has no %s range to allocate from; give %s one to run this spec",
			family, PrimaryIPPoolEnvVar), 2)
	}
}

// newPrimaryIPAllocator gets the Nodes primary interface network info and picks a starting IP that stays
// within the subnet of all the K8 nodes.
func newPrimaryIPAllocator(nodeClient v1.NodeInterface, pool string) (*primaryIPAllocator, error) {
	ipa := &primaryIPAllocator{mu: &sync.Mutex{}, nodeClient: nodeClient}
	if pool != "" {
		return ipa, ipa.setPool(pool)
	}
	nodes, err := nodeClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return ipa, fmt.Errorf("failed to get a list of node(s): %v", err)
	}
	if len(nodes.Items) == 0 {
		return ipa, fmt.Errorf("expected at least one node but found zero")
	}
	nodePrimaryIPs, err := util.ParseNodePrimaryIfAddr(&nodes.Items[0])
	if err != nil {
		return ipa, fmt.Errorf("failed to parse node primary interface address from Node object: %v", err)
	}
	var rangeErrs []error
	if nodePrimaryIPs.V4.IP != nil {
		if ipa.v4, err = deriveRange(nodes.Items, &nodePrimaryIPs.V4, false); err != nil {
			if !IsNoRangeError(err) {
				return ipa, err
			}
			rangeErrs = append(rangeErrs, err)
		}
	}
	if nodePrimaryIPs.V6.IP != nil {
		if ipa.v6, err = deriveRange(nodes.Items, &nodePrimaryIPs.V6, true); err != nil {
			if !IsNoRangeError(err) {
				return ipa, err
			}
			rangeErrs = append(rangeErrs, err)
		}
	}
	if ipa.v4 == nil && ipa.v6 == nil {
		return ipa, errors.Join(rangeErrs...)
	}
	return ipa, nil
}

// deriveRange picks a range out of the Node subnets, and proves the choice by taking an address.
// Bumping the second last octet clears the Node addresses where the subnet is wide enough for
// the bump; where it is not, a /24, allocation starts at a Node IP and relies on allocateIP
// stepping over the addresses the Nodes hold.
func deriveRange(nodes []corev1.Node, primary *util.ParsedIFAddr, isIPv6 bool) (*ipAllocator, error) {
	ipNets, err := getNodePrimaryProviderIPs(nodes, isIPv6)
	if err != nil {
		return nil, err
	}
	start := append(net.IP(nil), primary.IP...)
	bumped := append(net.IP(nil), primary.IP...)
	bumped[len(bumped)-2]++
	if isIPWithinAllSubnets(ipNets, bumped) {
		start = bumped
	}
	allocator := newIPAllocator(&net.IPNet{IP: start, Mask: primary.Net.Mask})
	nextIP, err := allocator.AllocateNextIP()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoRange, err)
	}
	if !isIPWithinAllSubnets(ipNets, nextIP) {
		return nil, fmt.Errorf("%w: IP %s is not within all Node subnets", errNoRange, nextIP)
	}
	return allocator, nil
}

// setPool allocates from the given CIDRs, which it deliberately does not check against the Node
// subnets: a pool exists precisely because it lies outside them.
func (pia *primaryIPAllocator) setPool(pool string) error {
	for _, entry := range strings.Split(pool, ",") {
		ip, ipNet, err := net.ParseCIDR(strings.TrimSpace(entry))
		if err != nil {
			return fmt.Errorf("failed to parse %s entry %q: %v", PrimaryIPPoolEnvVar, entry, err)
		}
		// allocateIP skips last-octet 0 and 1, and IPv4 broadcast, so narrower than this hands out none.
		if ones, bits := ipNet.Mask.Size(); bits-ones < 2 {
			return fmt.Errorf("%s entry %q is too small to allocate from", PrimaryIPPoolEnvVar, entry)
		}
		allocator := newIPAllocator(&net.IPNet{IP: ip, Mask: ipNet.Mask})
		if ip.To4() != nil {
			if pia.v4 != nil {
				return fmt.Errorf("%s names IPv4 twice", PrimaryIPPoolEnvVar)
			}
			pia.v4 = allocator
		} else {
			if pia.v6 != nil {
				return fmt.Errorf("%s names IPv6 twice", PrimaryIPPoolEnvVar)
			}
			pia.v6 = allocator
		}
	}
	return nil
}

func getNodePrimaryProviderIPs(nodes []corev1.Node, isIPv6 bool) ([]*net.IPNet, error) {
	ipNets := make([]*net.IPNet, 0, len(nodes))
	for _, node := range nodes {
		nodePrimaryIPs, err := util.ParseNodePrimaryIfAddr(&node)
		if err != nil {
			return nil, fmt.Errorf("failed to parse node primary interface address from Node %s object: %v", node.Name, err)
		}
		var mask net.IPMask
		var ip net.IP

		if isIPv6 {
			ip = nodePrimaryIPs.V6.IP
			mask = nodePrimaryIPs.V6.Net.Mask
		} else {
			ip = nodePrimaryIPs.V4.IP
			mask = nodePrimaryIPs.V4.Net.Mask
		}
		if len(ip) == 0 || len(mask) == 0 {
			return nil, fmt.Errorf("failed to find Node %s primary Node IP and/or mask", node.Name)
		}
		ipNets = append(ipNets, &net.IPNet{IP: ip, Mask: mask})
	}
	return ipNets, nil
}

func isIPWithinAllSubnets(ipNets []*net.IPNet, ip net.IP) bool {
	if len(ipNets) == 0 {
		return false
	}
	for _, ipNet := range ipNets {
		if !ipNet.Contains(ip) {
			return false
		}
	}
	return true
}

func (pia *primaryIPAllocator) IncrementAndGetNextV4(times int) (net.IP, error) {
	var err error
	for i := 0; i < times; i++ {
		if _, err = pia.AllocateNextV4(); err != nil {
			return nil, err
		}
	}
	return pia.AllocateNextV4()
}

func (pia *primaryIPAllocator) AllocateNextV4() (net.IP, error) {
	if pia.v4 == nil {
		return nil, fmt.Errorf("IPv4 is not enable ")
	}
	if pia.v4.net == nil {
		return nil, fmt.Errorf("IPv4 is not enabled but Allocation request was called")
	}
	pia.mu.Lock()
	defer pia.mu.Unlock()
	return allocateIP(pia.nodeClient, pia.v4.AllocateNextIP)
}

func (pia *primaryIPAllocator) IncrementAndGetNextV6(times int) (net.IP, error) {
	var err error
	for i := 0; i < times; i++ {
		if _, err = pia.AllocateNextV6(); err != nil {
			return nil, err
		}
	}
	return pia.AllocateNextV6()
}

func (pia primaryIPAllocator) AllocateNextV6() (net.IP, error) {
	if pia.v6 == nil {
		return nil, fmt.Errorf("IPv6 is not enabled but Allocation request was called")
	}
	if pia.v6.net == nil {
		return nil, fmt.Errorf("ipv6 network is not set")
	}
	pia.mu.Lock()
	defer pia.mu.Unlock()
	return allocateIP(pia.nodeClient, pia.v6.AllocateNextIP)
}

type allocNextFn func() (net.IP, error)

func allocateIP(nodeClient v1.NodeInterface, allocateFn allocNextFn) (net.IP, error) {
	nodeList, err := nodeClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %v", err)
	}
	for {
		nextIP, err := allocateFn()
		if err != nil {
			return nil, fmt.Errorf("failed to allocated next IP address: %v", err)
		}
		firstOctet := nextIP[len(nextIP)-1]
		// skip 0 and 1
		if firstOctet == 0 || firstOctet == 1 {
			continue
		}
		isConflict, err := isConflictWithExistingHostIPs(nodeList.Items, nextIP)
		if err != nil {
			return nil, fmt.Errorf("failed to determine if IP conflicts with existing IPs: %v", err)
		}
		if !isConflict {
			return nextIP, nil
		}
	}
}

func isConflictWithExistingHostIPs(nodes []corev1.Node, ip net.IP) (bool, error) {
	ipStr := ip.String()
	for _, node := range nodes {
		nodeIPsSet, err := util.ParseNodeHostCIDRsDropNetMask(&node)
		if err != nil {
			return false, fmt.Errorf("failed to parse node %s primary annotation info: %v", node.Name, err)
		}
		if nodeIPsSet.Has(ipStr) {
			return true, nil
		}
	}
	return false, nil
}
