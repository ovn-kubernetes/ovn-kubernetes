package kind

import (
	"fmt"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"k8s.io/utils/net"
)

type containerEngineNetwork struct {
	name    string
	Configs []containerEngineNetworkConfig
}

type containerEngineNetworkConfig struct {
	Subnet  string `json:"Subnet"`
	Gateway string `json:"Gateway"`
}

func (n containerEngineNetwork) Name() string {
	return n.name
}

func (n containerEngineNetwork) IPv4IPv6Subnets() (string, string, error) {
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
		return "", "", fmt.Errorf("failed to find IPv4 and IPv6 addresses for network %s", n.Name)
	}
	return v4, v6, nil
}

func (n containerEngineNetwork) Equal(candidate api.Network) bool {
	if n.name != candidate.Name() {
		return false
	}
	return true
}

func (n containerEngineNetwork) String() string {
	return n.name
}
