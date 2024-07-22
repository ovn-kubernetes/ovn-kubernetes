package provider

import (
	"fmt"
	"k8s.io/utils/net"
)

type Network struct {
	Name    string
	Configs []NetworkConfig
}

type NetworkConfig struct {
	Subnet  string `json:"Subnet"`
	Gateway string `json:"Gateway"`
}

func (n Network) GetIPV4V6Subnets() (string, string) {
	if len(n.Configs) == 0 {
		panic("failed to get IPV4/V6 because network doesnt contain configuration")
	}
	var v4, v6 string
	for _, config := range n.Configs {
		if config.Subnet == "" {
			panic(fmt.Sprintf("failed to get IPV4/V6 because network %s contains a config with an empty subnet", n.Name))
		}
		ip, _, err := net.ParseCIDRSloppy(config.Subnet)
		if err != nil {
			panic(fmt.Sprintf("failed to parse network %s subnet %q: %v", n.Name, config.Subnet, err))
		}
		if net.IsIPv4(ip) {
			v4 = config.Subnet
		} else {
			v6 = config.Subnet
		}
	}
	if v4 == "" && v6 == "" {
		panic(fmt.Sprintf("failed to find IPv4 and IPv6 addresses for network %s", n.Name))
	}
	return v4, v6
}

func (n Network) Equal(candidate Network) bool {
	if n.Name != candidate.Name {
		return false
	}
	return true
}

func (n Network) String() string {
	return n.Name
}

type Networks struct {
	List []Network
}

func (n *Networks) Contains(name string) bool {
	_, found := n.Get(name)
	return found
}

func (n *Networks) Get(name string) (Network, bool) {
	for _, network := range n.List {
		if network.Name == name {
			return network, true
		}
	}
	return Network{}, false
}

func (n *Networks) InsertNoDupe(candidate Network) {
	var found bool
	for _, network := range n.List {
		if network.Equal(candidate) {
			found = true
			break
		}
	}
	if !found {
		n.List = append(n.List, candidate)
	}
}

type networkAttachment struct {
	network Network
	node    string
}

func (na networkAttachment) equal(candidate networkAttachment) bool {
	if na.node != candidate.node {
		return false
	}
	if !na.network.Equal(candidate.network) {
		return false
	}
	return true
}

type NetworkAttachments struct {
	List []networkAttachment
}

func (na *NetworkAttachments) insertNoDupe(candidate networkAttachment) {
	var found bool
	for _, existingNetworkAttachment := range na.List {
		if existingNetworkAttachment.equal(candidate) {
			found = true
			break
		}
	}
	if !found {
		na.List = append(na.List, candidate)
	}
}

type NetworkInterface struct {
	IPv4Gateway string
	IPv4        string
	IPv4Prefix  string
	IPv6Gateway string
	IPv6        string
	IPv6Prefix  string
	MAC         string
	InfName     string
}
