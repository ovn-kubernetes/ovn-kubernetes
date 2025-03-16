package controller

import (
	"encoding/json"
	"fmt"
	"net"

	"github.com/Microsoft/hcsshim/hcn"
	ps "github.com/bhendo/go-powershell"

	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

// Datastore for NetworkInfo.
type NetworkInfo struct {
	ID           string
	Name         string
	Subnets      []SubnetInfo
	VSID         uint32
	AutomaticDNS bool
	IsPersistent bool
	VXLANPort    uint16
}

// Datastore for SubnetInfo.
type SubnetInfo struct {
	AddressPrefix  *net.IPNet
	GatewayAddress net.IP
	VSID           uint32
}

// GetHostComputeSubnetConfig converts SubnetInfo into an HCN format.
func (subnet *SubnetInfo) GetHostComputeSubnetConfig() (*hcn.Subnet, error) {
	ipAddr := subnet.AddressPrefix.String()
	gwAddr := ""
	destPrefix := ""
	if subnet.GatewayAddress != nil {
		gwAddr = subnet.GatewayAddress.String()
		destPrefix = "0.0.0.0/0"
	}

	vsidJSON, err := json.Marshal(&hcn.VsidPolicySetting{
		IsolationId: subnet.VSID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal VSID policy: %v", err)
	}

	subnetPolicyJson, err := json.Marshal(hcn.SubnetPolicy{
		Type:     "VSID",
		Settings: vsidJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal subnet policy: %v", err)
	}

	return &hcn.Subnet{
		IpAddressPrefix: ipAddr,
		Routes: []hcn.Route{{
			NextHop:           gwAddr,
			DestinationPrefix: destPrefix,
		}},
		Policies: []json.RawMessage{
			subnetPolicyJson,
		},
	}, nil
}

// GetHostComputeNetworkConfig converts NetworkInfo to HCN format.
func (info *NetworkInfo) GetHostComputeNetworkConfig() (*hcn.HostComputeNetwork, error) {
	subnets := []hcn.Subnet{}
	for _, subnet := range info.Subnets {
		subnetConfig, err := subnet.GetHostComputeSubnetConfig()
		if err != nil {
			return nil, err
		}

		subnets = append(subnets, *subnetConfig)
	}

	dnsJSON, err := json.Marshal(&hcn.AutomaticDNSNetworkPolicySetting{
		Enable: info.AutomaticDNS,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal automatic DNS policy: %v", err)
	}

	var ipams []hcn.Ipam
	if len(subnets) > 0 {
		ipams = append(ipams, hcn.Ipam{
			Type:    "Static",
			Subnets: subnets,
		})
	}

	var flags hcn.NetworkFlags
	if !info.IsPersistent {
		flags = hcn.EnableNonPersistent
	}

	policies := []hcn.NetworkPolicy{{
		Type:     hcn.AutomaticDNS,
		Settings: dnsJSON,
	}}

	// Only configure the VXLAN UDP port if the port is not
	// the default port.
	// Note: the calling code is responsible for making sure non-default
	// ports are only passed to this function if the underlying platform
	// supports it.
	if info.VXLANPort != config.DefaultVXLANPort {
		vxlanPortJSON, err := json.Marshal(&hcn.VxlanPortPolicySetting{
			Port: info.VXLANPort,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to marshal the VXLAN port policy: %v", err)
		}

		policies = append(policies, hcn.NetworkPolicy{
			Type:     hcn.VxlanPort,
			Settings: vxlanPortJSON,
		})
	}

	return &hcn.HostComputeNetwork{
		SchemaVersion: hcn.SchemaVersion{
			Major: 2,
			Minor: 0,
		},
		Name:     info.Name,
		Type:     hcn.NetworkType("Overlay"),
		Ipams:    ipams,
		Flags:    flags,
		Policies: policies,
	}, nil
}

func AddHostRoutePolicy(network *hcn.HostComputeNetwork) error {
	return network.AddPolicy(hcn.PolicyNetworkRequest{
		Policies: []hcn.NetworkPolicy{{
			Type:     hcn.HostRoute,
			Settings: []byte("{}"),
		}},
	})
}

// CreateNetworkPolicySetting builds a NetAdapterNameNetworkPolicySetting.
func CreateNetworkPolicySetting(networkAdapterName string) (*hcn.NetworkPolicy, error) {
	policyJSON, err := json.Marshal(hcn.NetAdapterNameNetworkPolicySetting{
		NetworkAdapterName: networkAdapterName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed ot marshal network adapter policy: %v", err)
	}

	return &hcn.NetworkPolicy{
		Type:     hcn.NetAdapterName,
		Settings: policyJSON,
	}, nil
}

// AddRemoteSubnetPolicy adds a remote subnet policy
func AddRemoteSubnetPolicy(network *hcn.HostComputeNetwork, settings *hcn.RemoteSubnetRoutePolicySetting) error {
	json, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("failed to marshall remote subnet route policy settings: %v", err)
	}

	network.AddPolicy(hcn.PolicyNetworkRequest{
		Policies: []hcn.NetworkPolicy{{
			Type:     hcn.RemoteSubnetRoute,
			Settings: json,
		}},
	})
	return nil
}

func removeOneRemoteSubnetPolicy(network *hcn.HostComputeNetwork, settings []byte) error {
	network.RemovePolicy(hcn.PolicyNetworkRequest{
		Policies: []hcn.NetworkPolicy{{
			Type:     hcn.RemoteSubnetRoute,
			Settings: settings,
		}},
	})
	return nil
}

// RemoveRemoteSubnetPolicy removes a remote subnet policy
func RemoveRemoteSubnetPolicy(network *hcn.HostComputeNetwork, destinationPrefix string) error {
	for _, policy := range network.Policies {
		if policy.Type != hcn.RemoteSubnetRoute {
			continue
		}

		existingPolicySettings := hcn.RemoteSubnetRoutePolicySetting{}
		if err := json.Unmarshal(policy.Settings, &existingPolicySettings); err != nil {
			return fmt.Errorf("failed to unmarshal remote subnet route policy settings: %v", err)
		}

		if existingPolicySettings.DestinationPrefix == destinationPrefix {
			if err := removeOneRemoteSubnetPolicy(network, policy.Settings); err != nil {
				return fmt.Errorf("failed to remove remote subnet policy %v: %v",
					existingPolicySettings.DestinationPrefix, err)
			}
		}
	}

	return nil
}

func ClearRemoteSubnetPolicies(network *hcn.HostComputeNetwork) error {
	for _, policy := range network.Policies {
		if policy.Type != hcn.RemoteSubnetRoute {
			continue
		}

		existingPolicySettings := hcn.RemoteSubnetRoutePolicySetting{}
		if err := json.Unmarshal(policy.Settings, &existingPolicySettings); err != nil {
			return fmt.Errorf("failed to unmarshal remote subnet route policy settings: %v", err)
		}

		if err := removeOneRemoteSubnetPolicy(network, policy.Settings); err != nil {
			// We don't return the error in this case, we take a best effort
			// approach to clear the remote subnets.
			klog.Errorf("Failed to remove remote subnet policy %v: %v",
				existingPolicySettings.DestinationPrefix, err)
		}
	}

	return nil
}

func GetGatewayAddress(subnet *hcn.Subnet) string {
	for _, route := range subnet.Routes {
		if route.DestinationPrefix == "0.0.0.0/0" || route.DestinationPrefix == "::/0" {
			return route.NextHop
		}
	}
	return ""
}

// EnsureExistingNetworkIsValid returns the existing network defined by the given network name is valid, if there is a network with the
// given name that is invalid the network is deleted
func EnsureExistingNetworkIsValid(networkName string, expectedAddressPrefix string, expectedGW string) *hcn.HostComputeNetwork {
	existingNetwork, err := hcn.GetNetworkByName(networkName)
	if err != nil || existingNetwork.Type != hcn.Overlay {
		return nil
	}

	for _, existingIpams := range existingNetwork.Ipams {
		for _, existingSubnet := range existingIpams.Subnets {
			gatewayAddress := GetGatewayAddress(&existingSubnet)
			if existingSubnet.IpAddressPrefix == expectedAddressPrefix && gatewayAddress == expectedGW {
				return existingNetwork
			}
		}
	}

	if existingNetwork != nil {
		// the named network already exists but the nodes subnet or GW has changed
		existingNetwork.Delete()
	}

	return nil
}

// duplicateIPv4Routes duplicates all the IPv4 network routes associated with the physical interface to the host vNIC,
// where parameters policyStore and destinationPrefix are optional and could be used as filtering criteria.
// Otherwise, all IPv4 routes will be forwarded from the physical interface to the host vNIC.
func DuplicateIPv4Routes(shell ps.Shell) error {
	script := `
	# Find physical adapters whose interfaces are bound to a vswitch (i.e. the MAC addresses match)
	$boundAdapters = (Get-NetAdapter -Physical | where { (Get-NetAdapter -Name "*vEthernet*").MacAddress -eq $_.MacAddress })

	# Forward all the matching routes associated with the physical interface to the associated vNIC
	foreach ($boundAdapter in $boundAdapters) {
		$associatedVNic = Get-NetAdapter -Name "*vEthernet*" | where { $_.MacAddress -eq $boundAdapter.MacAddress }
		foreach ($route in $routes) {
			 if ($route.InterfaceIndex -eq $boundAdapter.IfIndex) {
        		New-NetRoute -InterfaceIndex $associatedVNic.ifIndex -DestinationPrefix $route.DestinationPrefix -NextHop $route.NextHop -RouteMetric $route.RouteMetric -ErrorAction SilentlyContinue
			 }
		}
	}
	`
	if _, stderr, err := shell.Execute(script + "\r\n\r\n"); err != nil {
		return fmt.Errorf("failed to duplicate network routes to the associated vNIC, %v: %v", stderr, err)
	}

	return nil
}
