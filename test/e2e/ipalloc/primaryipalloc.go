package ipalloc

import (
	"context"
	"fmt"
	"net"
	"sync"

	ipallocator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// primaryIPAllocator attempts to allocate an IP in the same subnet as a nodes primary network
type primaryIPAllocator struct {
	mu         *sync.Mutex
	v4         *ipallocator.Range
	v6         *ipallocator.Range
	nodeClient v1.NodeInterface
}

var pia *primaryIPAllocator

// InitPrimaryIPAllocator must be called to init IP allocator(s). Callers must be synchronise.
func InitPrimaryIPAllocator(nodeClient v1.NodeInterface) error {
	var err error
	pia, err = newPrimaryIPAllocator(nodeClient)
	return err
}

func NewPrimaryIPv4() (net.IP, error) {
	return pia.AllocateNextV4()
}

func NewPrimaryIPv6() (net.IP, error) {
	return pia.AllocateNextV6()
}

// newPrimaryIPAllocator gets a Nodes primary interfaces network info, increments the 2 octet and checks if the IP is still
// within the subnet of all the K8 nodes.
func newPrimaryIPAllocator(nodeClient v1.NodeInterface) (*primaryIPAllocator, error) {
	ipa := &primaryIPAllocator{mu: &sync.Mutex{}, nodeClient: nodeClient}
	nodes, err := nodeClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return ipa, fmt.Errorf("failed to get a list of node(s): %v", err)
	}
	if len(nodes.Items) == 0 {
		return ipa, fmt.Errorf("expected at least one node but found zero")
	}

	for _, node := range nodes.Items {
		nodePrimaryIPs, err := util.ParseNodePrimaryIfAddr(&node)
		if err != nil {
			return ipa, fmt.Errorf("failed to parse node primary interface address from Node %s object: %v", node.Name, err)
		}
		if nodePrimaryIPs.V4.IP != nil {
			if ipa.v4 == nil {
				ipa.v4, err = ipallocator.NewCIDRRange(nodePrimaryIPs.V4.Net)
				if err != nil {
					return ipa, fmt.Errorf("failed to create new CIDR range for IPv4: %v", err)
				}
			}
			if err := ipa.v4.Allocate(nodePrimaryIPs.V4.IP); err != nil {
				return ipa, fmt.Errorf("failed to allocate IPv4 %s: %v", nodePrimaryIPs.V4.IP, err)
			}
		}
		if nodePrimaryIPs.V6.IP != nil {
			if ipa.v6 == nil {
				ipa.v6, err = ipallocator.NewCIDRRange(nodePrimaryIPs.V6.Net)
				if err != nil {
					return ipa, fmt.Errorf("failed to create new CIDR range for IPv4: %v", err)
				}
			}
			if err := ipa.v6.Allocate(nodePrimaryIPs.V6.IP); err != nil {
				return ipa, fmt.Errorf("failed to allocate IPv6 %s: %v", nodePrimaryIPs.V4.IP, err)
			}
		}

	}
	return ipa, nil
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
	pia.mu.Lock()
	defer pia.mu.Unlock()
	return pia.v4.AllocateNext()
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
	pia.mu.Lock()
	defer pia.mu.Unlock()
	return pia.v6.AllocateNext()
}
