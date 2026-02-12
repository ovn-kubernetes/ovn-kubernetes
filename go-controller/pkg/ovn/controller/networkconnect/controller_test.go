package networkconnect

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// =============================================================================
// Test Helper Types and Functions
// =============================================================================

const (
	// Annotation keys used in tests (matching unexported constants in util package)
	ovnNodeSubnetsAnnotation          = "k8s.ovn.org/node-subnets"
	ovnNetworkConnectSubnetAnnotation = "k8s.ovn.org/network-connect-subnet"
)

// IP mode test configurations
var ipModes = []struct {
	name string
	v4   bool
	v6   bool
}{
	{"IPv4", true, false},
	{"IPv6", false, true},
	{"DualStack", true, true},
}

// testNetwork represents a network configuration for testing
type testNetwork struct {
	name         string
	id           int
	topologyType string
	subnets      []string // Pod subnets for the network (optional, defaults based on IP mode)
}

// DefaultSubnets returns subnets based on IP mode and topology type
// For Layer3: format is "cidr/hostSubnetLength" e.g., "10.128.0.0/14/23"
// For Layer2: format is just "cidr" e.g., "10.200.0.0/16"
// When explicit subnets are provided, filters them based on IP mode
func (n testNetwork) DefaultSubnets() []string {
	if len(n.subnets) > 0 {
		// Filter explicit subnets by IP mode
		var filtered []string
		for _, subnet := range n.subnets {
			// Check if subnet is IPv6 (contains ":")
			isIPv6 := strings.Contains(subnet, ":")
			if isIPv6 && config.IPv6Mode {
				filtered = append(filtered, subnet)
			} else if !isIPv6 && config.IPv4Mode {
				filtered = append(filtered, subnet)
			}
		}
		return filtered
	}
	var subnets []string
	if n.topologyType == ovntypes.Layer3Topology {
		if config.IPv4Mode {
			subnets = append(subnets, "10.128.0.0/14/23")
		}
		if config.IPv6Mode {
			subnets = append(subnets, "fd00:10:128::/48/64")
		}
	} else { // Layer2
		if config.IPv4Mode {
			subnets = append(subnets, "10.200.0.0/16")
		}
		if config.IPv6Mode {
			subnets = append(subnets, "fd00:10:200::/48")
		}
	}
	return subnets
}

// RouterName returns the OVN router name for this network
func (n testNetwork) RouterName() string {
	prefix := util.GetUserDefinedNetworkPrefix(n.name)
	if n.topologyType == ovntypes.Layer2Topology {
		return prefix + ovntypes.TransitRouter
	}
	return prefix + ovntypes.OVNClusterRouter
}

// testNode represents a node configuration for testing
type testNode struct {
	name        string
	id          int
	zone        string
	nodeSubnets map[string]subnetPair // networkName -> subnet pair (v4, v6)
}

// setupTestConfig initializes the test config with the given IP mode
func setupTestConfig(v4Enabled, v6Enabled bool) {
	Expect(config.PrepareTestConfig()).NotTo(HaveOccurred())
	config.IPv4Mode = v4Enabled
	config.IPv6Mode = v6Enabled
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableNetworkConnect = true
}

// createTestNode creates a test Node object
// nodeSubnets is a map of network names to subnet pairs (v4, v6)
// If id is 0, node ID annotation is not set (to test nodes without ID allocation)
// If zone is empty, zone annotation is not set
func createTestNode(n testNode) *corev1.Node {
	annotations := map[string]string{}
	// Only set zone annotation if zone is not empty
	if n.zone != "" {
		annotations[util.OvnNodeZoneName] = n.zone
	}
	// Only set node ID annotation if id > 0 (0 means no node ID)
	if n.id > 0 {
		annotations[util.OvnNodeID] = strconv.Itoa(n.id)
	}
	// Add node subnet annotations based on IP mode
	if len(n.nodeSubnets) > 0 {
		annotations[ovnNodeSubnetsAnnotation] = buildNodeSubnetAnnotation(n.nodeSubnets)
	}
	annotations[util.OvnNodeChassisID] = chassisIDForNode(n.name)

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        n.name,
			Annotations: annotations,
		},
	}
}

// defaultConnectSubnets returns the default connect subnets based on the current IP mode
func defaultConnectSubnets() []networkconnectv1.ConnectSubnet {
	var subnets []networkconnectv1.ConnectSubnet
	if config.IPv4Mode {
		subnets = append(subnets, networkconnectv1.ConnectSubnet{CIDR: "192.168.0.0/16", NetworkPrefix: 24})
	}
	if config.IPv6Mode {
		subnets = append(subnets, networkconnectv1.ConnectSubnet{CIDR: "fd00:192:168::/48", NetworkPrefix: 64})
	}
	return subnets
}

// subnetPair holds v4 and v6 subnet strings for a network owner
type subnetPair struct{ v4, v6 string }

// buildConnectSubnetAnnotation builds the CNC subnet annotation JSON based on current IP mode
// Pass a map of owner keys (e.g., "layer3_1") to subnet pairs
// The function uses config.IPv4Mode/IPv6Mode to decide what to include
func buildConnectSubnetAnnotation(owners map[string]subnetPair) string {
	if len(owners) == 0 {
		return ""
	}

	result := "{"
	first := true
	for owner, subnets := range owners {
		includeV4 := config.IPv4Mode && subnets.v4 != ""
		includeV6 := config.IPv6Mode && subnets.v6 != ""

		if !includeV4 && !includeV6 {
			continue
		}

		if !first {
			result += ","
		}
		first = false

		if includeV4 && includeV6 {
			result += fmt.Sprintf(`"%s":{"ipv4":"%s","ipv6":"%s"}`, owner, subnets.v4, subnets.v6)
		} else if includeV4 {
			result += fmt.Sprintf(`"%s":{"ipv4":"%s"}`, owner, subnets.v4)
		} else {
			result += fmt.Sprintf(`"%s":{"ipv6":"%s"}`, owner, subnets.v6)
		}
	}
	result += "}"
	return result
}

// buildNodeSubnetAnnotation builds the node subnet annotation JSON based on current IP mode
// Pass a map of network names to subnet pairs
// The function uses config.IPv4Mode/IPv6Mode to decide what to include
func buildNodeSubnetAnnotation(networks map[string]subnetPair) string {
	if len(networks) == 0 {
		return ""
	}

	result := "{"
	first := true
	for netName, subnets := range networks {
		includeV4 := config.IPv4Mode && subnets.v4 != ""
		includeV6 := config.IPv6Mode && subnets.v6 != ""

		if !includeV4 && !includeV6 {
			continue
		}

		if !first {
			result += ","
		}
		first = false

		if includeV4 && includeV6 {
			result += fmt.Sprintf(`"%s":["%s","%s"]`, netName, subnets.v4, subnets.v6)
		} else if includeV4 {
			result += fmt.Sprintf(`"%s":"%s"`, netName, subnets.v4)
		} else {
			result += fmt.Sprintf(`"%s":"%s"`, netName, subnets.v6)
		}
	}
	result += "}"
	return result
}

// createTestCNC creates a test CNC object with proper annotations
func createTestCNC(name string, tunnelID int, connectSubnets []networkconnectv1.ConnectSubnet, subnetAnnotation string) *networkconnectv1.ClusterNetworkConnect {
	cnc := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: make(map[string]string),
		},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{
			ConnectSubnets: connectSubnets,
			Connectivity: []networkconnectv1.ConnectivityType{
				networkconnectv1.PodNetwork,
			},
		},
	}
	if tunnelID > 0 {
		cnc.Annotations[util.OvnConnectRouterTunnelKeyAnnotation] = strconv.Itoa(tunnelID)
	}
	if subnetAnnotation != "" {
		cnc.Annotations[ovnNetworkConnectSubnetAnnotation] = subnetAnnotation
	}
	return cnc
}

// createInitialDBWithRouters creates initial DB data with network routers
func createInitialDBWithRouters(networks []testNetwork) []libovsdbtest.TestData {
	var data []libovsdbtest.TestData
	for _, net := range networks {
		routerName := net.RouterName()
		data = append(data, &nbdb.LogicalRouter{
			UUID: routerName + "-uuid",
			Name: routerName,
		})
	}
	return data
}

// createNetInfo creates a real NetInfo for testing
func createNetInfo(net testNetwork) (util.NetInfo, error) {
	// Use DefaultSubnets which handles IP mode
	subnets := net.DefaultSubnets()
	subnetsStr := strings.Join(subnets, ",")

	netConf := &ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: net.name},
		Topology: net.topologyType,
		Role:     ovntypes.NetworkRoleSecondary,
		Subnets:  subnetsStr,
	}
	netInfo, err := util.NewNetInfo(netConf)
	if err != nil {
		return nil, err
	}
	// Set the network ID
	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNetworkID(net.id)
	return mutableNetInfo, nil
}

// verifyConnectRouter checks that a connect router exists with the expected properties
// Returns error if router doesn't exist or has wrong properties (for use in Eventually)
func verifyConnectRouter(nbClient libovsdbclient.Client, cncName string, tunnelID int) error {
	routerName := getConnectRouterName(cncName)
	router, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: routerName})
	if err != nil {
		return err
	}
	if router.Options == nil || router.Options["requested-tnl-key"] != strconv.Itoa(tunnelID) {
		return fmt.Errorf("connect router %s has wrong tunnel key, expected %d", routerName, tunnelID)
	}
	if router.ExternalIDs == nil || router.ExternalIDs[libovsdbops.ObjectNameKey.String()] != cncName {
		return fmt.Errorf("connect router %s has wrong external ID, expected %s", routerName, cncName)
	}
	return nil
}

// verifyRouterPorts checks that expected ports exist on a router
// Returns error for use in Eventually
func verifyRouterPortsCount(nbClient libovsdbclient.Client, routerName string, expectedPortCount int) error {
	router, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: routerName})
	if err != nil {
		return err
	}
	if len(router.Ports) != expectedPortCount {
		return fmt.Errorf("expected %d ports on %s, got %d", expectedPortCount, routerName, len(router.Ports))
	}
	return nil
}

// verifyRouterPort verifies a port exists on any router with expected properties
// Returns error for use in Eventually
func verifyRouterPort(nbClient libovsdbclient.Client, portName, expectedCNCName string, expectedNetworks []string) error {
	port, err := libovsdbops.GetLogicalRouterPort(nbClient, &nbdb.LogicalRouterPort{Name: portName})
	if err != nil {
		return fmt.Errorf("port %s not found: %v", portName, err)
	}

	// Verify external IDs
	if port.ExternalIDs == nil || port.ExternalIDs[libovsdbops.ObjectNameKey.String()] != expectedCNCName {
		return fmt.Errorf("port %s has wrong CNC name in external IDs, expected %s, got %v", portName, expectedCNCName, port.ExternalIDs)
	}

	// Verify networks (IPs) if provided
	if len(expectedNetworks) > 0 {
		if len(port.Networks) != len(expectedNetworks) {
			return fmt.Errorf("port %s networks count mismatch, expected %d, got %d", portName, len(expectedNetworks), len(port.Networks))
		}
		for _, expected := range expectedNetworks {
			found := false
			for _, actual := range port.Networks {
				if actual == expected {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("port %s missing expected network %s, got %v", portName, expected, port.Networks)
			}
		}
	}

	// Verify MAC is set
	if port.MAC == "" {
		return fmt.Errorf("port %s has no MAC address", portName)
	}

	return nil
}

// verifyRouterPortIsRemote verifies a port exists and has requested-chassis option set (remote node port)
// Also verifies requested-tnl-key if expectedTunnelKey > 0
// Returns error for use in Eventually
func verifyRouterPortIsRemote(nbClient libovsdbclient.Client, portName string, expectedTunnelKey int) error {
	port, err := libovsdbops.GetLogicalRouterPort(nbClient, &nbdb.LogicalRouterPort{Name: portName})
	if err != nil {
		return fmt.Errorf("port %s not found: %v", portName, err)
	}

	// Remote ports have requested-chassis option set
	if port.Options == nil || port.Options["requested-chassis"] == "" {
		return fmt.Errorf("port %s is not a remote port (missing requested-chassis option), options: %v", portName, port.Options)
	}

	// Verify tunnel key if expected
	if expectedTunnelKey > 0 {
		if port.Options["requested-tnl-key"] != strconv.Itoa(expectedTunnelKey) {
			return fmt.Errorf("port %s has wrong tunnel key, expected %d, got %s", portName, expectedTunnelKey, port.Options["requested-tnl-key"])
		}
	}

	return nil
}

// verifyRouterPortIsLocal verifies a port exists and does NOT have requested-chassis option set (local node port)
// Also verifies requested-tnl-key if expectedTunnelKey > 0
// Also verifies Peer field is set to expectedPeerPortName if not empty
// Returns error for use in Eventually
func verifyRouterPortIsLocal(nbClient libovsdbclient.Client, portName string, expectedTunnelKey int, expectedPeerPortName string) error {
	port, err := libovsdbops.GetLogicalRouterPort(nbClient, &nbdb.LogicalRouterPort{Name: portName})
	if err != nil {
		return fmt.Errorf("port %s not found: %v", portName, err)
	}

	// Local ports should NOT have requested-chassis option set
	if port.Options != nil && port.Options["requested-chassis"] != "" {
		return fmt.Errorf("port %s is not a local port (has requested-chassis option), options: %v", portName, port.Options)
	}

	// Verify tunnel key if expected
	if expectedTunnelKey > 0 {
		if port.Options == nil || port.Options["requested-tnl-key"] != strconv.Itoa(expectedTunnelKey) {
			return fmt.Errorf("port %s has wrong tunnel key, expected %d, options: %v", portName, expectedTunnelKey, port.Options)
		}
	}

	// Verify Peer field for local ports
	if expectedPeerPortName != "" {
		if port.Peer == nil || *port.Peer != expectedPeerPortName {
			var actualPeer string
			if port.Peer != nil {
				actualPeer = *port.Peer
			}
			return fmt.Errorf("port %s has wrong peer, expected %s, got %s", portName, expectedPeerPortName, actualPeer)
		}
	}

	return nil
}

// getExpectedPortNetworks returns expected networks for connect and network router ports
// Pass allocated subnets (/24 for v4, /64 for v6) and node ID - function calculates P2P subnets
func getExpectedPortNetworks(v4AllocatedSubnet, v6AllocatedSubnet *net.IPNet, nodeID int) (connectRouterNetworks, networkRouterNetworks []string) {
	var allocatedSubnets []*net.IPNet
	if config.IPv4Mode && v4AllocatedSubnet != nil {
		allocatedSubnets = append(allocatedSubnets, v4AllocatedSubnet)
	}
	if config.IPv6Mode && v6AllocatedSubnet != nil {
		allocatedSubnets = append(allocatedSubnets, v6AllocatedSubnet)
	}

	// Calculate P2P subnets for this node ID (same logic as controller)
	portPairInfo, err := GetP2PAddresses(allocatedSubnets, nodeID)
	if err != nil {
		return nil, nil
	}

	for _, ip := range portPairInfo.connectPortIPs {
		connectRouterNetworks = append(connectRouterNetworks, ip.String())
	}
	for _, ip := range portPairInfo.networkPortIPs {
		networkRouterNetworks = append(networkRouterNetworks, ip.String())
	}
	return connectRouterNetworks, networkRouterNetworks
}

// getExpectedPortTunnelKey calculates the expected tunnel key for a port using the same
// logic as production code via getNetworkIndexAndMaxNodes.
// For Layer3: tunnelKey = networkIndex * maxNodes + nodeID + 1
// For Layer2: tunnelKey = networkIndex * maxNodes + subIndex + 1 (subIndex from getLayer2SubIndex)
//
// Parameters:
// - allocatedSubnetV4/V6: the allocated subnet for this network from the annotation (e.g., "192.168.0.0/24")
// - topologyType: ovntypes.Layer3Topology or ovntypes.Layer2Topology
// - nodeID: node ID for Layer3, ignored for Layer2
func getExpectedPortTunnelKey(allocatedSubnetV4, allocatedSubnetV6 string, topologyType string, nodeID int) int {
	// Parse allocated subnets based on current IP mode
	var subnets []*net.IPNet
	if config.IPv4Mode && allocatedSubnetV4 != "" {
		subnets = append(subnets, ovntest.MustParseIPNet(allocatedSubnetV4))
	}
	if config.IPv6Mode && allocatedSubnetV6 != "" {
		subnets = append(subnets, ovntest.MustParseIPNet(allocatedSubnetV6))
	}

	tunnelKey, err := GetTunnelKey(defaultConnectSubnets(), subnets, topologyType, nodeID)
	if err != nil {
		panic(err) // test helper should not fail with default connect subnets
	}
	return tunnelKey
}

// extractNexthops extracts nexthop IPs (without mask) from network strings
// Uses ovntest.MustParseIPNet to parse CIDR strings like "192.168.0.3/31"
func extractNexthops(networks []string) (nexthopV4, nexthopV6 string) {
	for _, net := range networks {
		ipNet := ovntest.MustParseIPNet(net)
		if ipNet.IP.To4() != nil {
			nexthopV4 = ipNet.IP.String()
		} else {
			nexthopV6 = ipNet.IP.String()
		}
	}
	return
}

// verifyRouterPolicy verifies routing policies exist on a router with expected properties
// Returns error for use in Eventually
func verifyRouterPolicy(nbClient libovsdbclient.Client, routerName, cncName string, srcNetworkID, dstNetworkID, expectedCount int) error {
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(nbClient, routerName,
		func(policy *nbdb.LogicalRouterPolicy) bool {
			return policy.ExternalIDs != nil &&
				policy.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
				policy.ExternalIDs[libovsdbops.SourceNetworkIDKey.String()] == strconv.Itoa(srcNetworkID) &&
				policy.ExternalIDs[libovsdbops.DestinationNetworkIDKey.String()] == strconv.Itoa(dstNetworkID)
		})
	if err != nil {
		return fmt.Errorf("failed to get policies from %s: %v", routerName, err)
	}

	if len(policies) != expectedCount {
		return fmt.Errorf("expected %d policies on %s for CNC %s srcNetworkID %d dstNetworkID %d, got %d",
			expectedCount, routerName, cncName, srcNetworkID, dstNetworkID, len(policies))
	}
	return nil
}

// verifyRouterPolicyCount verifies the count of policies on a router for a given CNC
// Returns error for use in Eventually
func verifyRouterPolicyCount(nbClient libovsdbclient.Client, routerName, cncName string,
	srcNetworkID, dstNetworkID int, expectedCount int) error {
	policies, err := libovsdbops.FindALogicalRouterPoliciesWithPredicate(nbClient, routerName,
		func(policy *nbdb.LogicalRouterPolicy) bool {
			return policy.ExternalIDs != nil &&
				policy.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
				policy.ExternalIDs[libovsdbops.SourceNetworkIDKey.String()] == strconv.Itoa(srcNetworkID) &&
				policy.ExternalIDs[libovsdbops.DestinationNetworkIDKey.String()] == strconv.Itoa(dstNetworkID)
		})
	if err != nil {
		return fmt.Errorf("failed to get policies from %s: %v", routerName, err)
	}

	if len(policies) != expectedCount {
		return fmt.Errorf("expected %d policies on %s for CNC %s, got %d",
			expectedCount, routerName, cncName, len(policies))
	}
	return nil
}

// verifyRouterStaticRoutes verifies static routes exist on a router with expected properties
// Returns error for use in Eventually
// networkID is the network's ID from the NAD annotation
// nodeID is the node's ID (0 for Layer2 networks)
// Pass v4/v6 prefix and nexthop pairs - function checks based on config.IPv4Mode/IPv6Mode
func verifyRouterStaticRoutes(nbClient libovsdbclient.Client, routerName, cncName string, networkID int,
	nodeID int, prefixV4, prefixV6, nexthopV4, nexthopV6 string) error {
	routes, err := libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, &nbdb.LogicalRouter{Name: routerName},
		func(route *nbdb.LogicalRouterStaticRoute) bool {
			return route.ExternalIDs != nil &&
				route.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
				route.ExternalIDs[libovsdbops.NetworkIDKey.String()] == strconv.Itoa(networkID) &&
				route.ExternalIDs[libovsdbops.NodeIDKey.String()] == strconv.Itoa(nodeID)
		})
	if err != nil {
		return fmt.Errorf("failed to get static routes from %s: %v", routerName, err)
	}

	// Calculate expected count based on IP mode
	expectedCount := 0
	if config.IPv4Mode {
		expectedCount++
	}
	if config.IPv6Mode {
		expectedCount++
	}

	if len(routes) != expectedCount {
		return fmt.Errorf("expected %d static routes on %s for CNC %s, networkID %d, node %d, got %d",
			expectedCount, routerName, cncName, networkID, nodeID, len(routes))
	}

	// Verify v4 route if IPv4Mode is enabled
	if config.IPv4Mode {
		found := false
		for _, route := range routes {
			if route.IPPrefix == prefixV4 {
				found = true
				if route.Nexthop != nexthopV4 {
					return fmt.Errorf("v4 route %s on %s has wrong Nexthop, expected %s, got %s",
						prefixV4, routerName, nexthopV4, route.Nexthop)
				}
				if route.ExternalIDs[libovsdbops.IPFamilyKey.String()] != "v4" {
					return fmt.Errorf("v4 route %s on %s has wrong IPFamilyKey, expected v4, got %s",
						prefixV4, routerName, route.ExternalIDs[libovsdbops.IPFamilyKey.String()])
				}
				break
			}
		}
		if !found {
			return fmt.Errorf("v4 route with IPPrefix %s not found on %s", prefixV4, routerName)
		}
	}

	// Verify v6 route if IPv6Mode is enabled
	if config.IPv6Mode {
		found := false
		for _, route := range routes {
			if route.IPPrefix == prefixV6 {
				found = true
				if route.Nexthop != nexthopV6 {
					return fmt.Errorf("v6 route %s on %s has wrong Nexthop, expected %s, got %s",
						prefixV6, routerName, nexthopV6, route.Nexthop)
				}
				if route.ExternalIDs[libovsdbops.IPFamilyKey.String()] != "v6" {
					return fmt.Errorf("v6 route %s on %s has wrong IPFamilyKey, expected v6, got %s",
						prefixV6, routerName, route.ExternalIDs[libovsdbops.IPFamilyKey.String()])
				}
				break
			}
		}
		if !found {
			return fmt.Errorf("v6 route with IPPrefix %s not found on %s", prefixV6, routerName)
		}
	}

	return nil
}

// verifyRouterStaticRoutesCount verifies the count of static routes on a router for a given CNC
// Returns error for use in Eventually
func verifyRouterStaticRoutesCount(nbClient libovsdbclient.Client, routerName, cncName string,
	networkID, nodeID int, expectedCount int) error {
	routes, err := libovsdbops.GetRouterLogicalRouterStaticRoutesWithPredicate(nbClient, &nbdb.LogicalRouter{Name: routerName},
		func(route *nbdb.LogicalRouterStaticRoute) bool {
			return route.ExternalIDs != nil &&
				route.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
				route.ExternalIDs[libovsdbops.NetworkIDKey.String()] == strconv.Itoa(networkID) &&
				route.ExternalIDs[libovsdbops.NodeIDKey.String()] == strconv.Itoa(nodeID)
		})
	if err != nil {
		return fmt.Errorf("failed to get static routes from %s: %v", routerName, err)
	}

	if len(routes) != expectedCount {
		return fmt.Errorf("expected %d static routes on %s for CNC %s, got %d",
			expectedCount, routerName, cncName, len(routes))
	}
	return nil
}

// =============================================================================
// Integration Tests for Network Connect Controller
// =============================================================================

var _ = Describe("OVNKube Network Connect Controller Integration Tests", func() {
	for _, ipMode := range ipModes {

		Context("["+ipMode.name+"]", func() {
			var (
				nbClient      libovsdbclient.Client
				testCtx       *libovsdbtest.Context
				fakeClientset *util.OVNKubeControllerClientset
				wf            *factory.WatchFactory
				fakeNM        *networkmanager.FakeNetworkManager
				controller    *Controller
				zoneName      string
			)

			// start initializes and starts the controller with the given initial state
			start := func(initialDB []libovsdbtest.TestData, nodes []testNode, networks map[string]testNetwork) {
				var err error

				// Always add COPP to initialDB (required for router creation)
				// Appending to nil is safe in Go - it creates a new slice
				initialDB = append(initialDB, &nbdb.Copp{
					UUID: "copp-uuid",
					Name: ovntypes.DefaultCOPPName,
				})

				// Create libovsdb test harness
				nbClient, testCtx, err = libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
					NBData: initialDB,
				}, nil)
				Expect(err).NotTo(HaveOccurred())

				// Create fake clientset with nodes
				var nodeObjs []runtime.Object
				for _, n := range nodes {
					nodeObjs = append(nodeObjs, createTestNode(n))
				}
				fakeClientset = util.GetOVNClientset(nodeObjs...).GetOVNKubeControllerClientset()

				// Create watch factory
				wf, err = factory.NewOVNKubeControllerWatchFactory(fakeClientset)
				Expect(err).NotTo(HaveOccurred())

				err = wf.Start()
				Expect(err).NotTo(HaveOccurred())

				// Create FakeNetworkManager with networks
				fakeNM = &networkmanager.FakeNetworkManager{
					PrimaryNetworks: make(map[string]util.NetInfo),
				}
				for name, net := range networks {
					netInfo, err := createNetInfo(net)
					Expect(err).NotTo(HaveOccurred())
					fakeNM.PrimaryNetworks[name] = netInfo
				}

				// Create and start controller
				controller = NewController(zoneName, nbClient, wf, fakeNM.Interface())

				err = controller.Start()
				Expect(err).NotTo(HaveOccurred())
			}

			cleanup := func() {
				if controller != nil {
					controller.Stop()
				}
				if testCtx != nil {
					testCtx.Cleanup()
				}
				if wf != nil {
					wf.Shutdown()
				}
			}

			BeforeEach(func() {
				setupTestConfig(ipMode.v4, ipMode.v6)
				zoneName = "node1" // Default zone name
			})

			AfterEach(func() {
				cleanup()
			})

			// =============================================================================
			// Context: CNC Lifecycle Tests
			// =============================================================================
			Context("CNC Lifecycle", func() {

				It("should create connect router when CNC is created", func() {
					// Setup - no networks needed for just connect router creation
					start(nil, nil, nil)

					// Create CNC with tunnel ID - no subnet annotation needed
					cnc := createTestCNC("test-cnc", 100, defaultConnectSubnets(), "")

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Wait for connect router to be created and verify properties
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "test-cnc", 100)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should not create connect router without tunnel ID annotation", func() {
					// Setup - no networks needed
					start(nil, nil, nil)

					// Create CNC without tunnel ID (tunnelID=0 means no annotation)
					cnc := createTestCNC("no-tunnel-cnc", 0, defaultConnectSubnets(), "")

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Wait a bit and verify no router was created
					Consistently(func() error {
						routerName := getConnectRouterName("no-tunnel-cnc")
						_, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: routerName})
						return err
					}).WithTimeout(2 * time.Second).Should(HaveOccurred())
				})

				It("should create connect router when tunnel ID annotation is added later", func() {
					// Setup - no networks needed for just connect router creation
					start(nil, nil, nil)

					// Create CNC without tunnel ID
					cnc := createTestCNC("delayed-tunnel-cnc", 0, defaultConnectSubnets(), "")

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify no router created initially
					Consistently(func() error {
						routerName := getConnectRouterName("delayed-tunnel-cnc")
						_, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: routerName})
						return err
					}).WithTimeout(1 * time.Second).Should(HaveOccurred())

					// Now update CNC to add tunnel ID annotation
					cnc.Annotations[util.OvnConnectRouterTunnelKeyAnnotation] = "150"
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), cnc, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify router is now created
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "delayed-tunnel-cnc", 150)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should not update connect router tunnel ID once set", func() {
					// Setup - no networks needed for just connect router update
					start(nil, nil, nil)

					// Create CNC with initial tunnel ID
					cnc := createTestCNC("update-tunnel-cnc", 100, defaultConnectSubnets(), "")

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify router is created with initial tunnel ID
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "update-tunnel-cnc", 100)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Update CNC to change tunnel ID annotation
					cnc.Annotations[util.OvnConnectRouterTunnelKeyAnnotation] = "250"
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), cnc, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify router still has the original tunnel ID (tunnel ID is immutable once set)
					Consistently(func() error {
						return verifyConnectRouter(nbClient, "update-tunnel-cnc", 100)
					}).WithTimeout(2 * time.Second).Should(Succeed())
				})

				It("should delete connect router when CNC is deleted", func() {
					// Setup - no networks needed for just connect router lifecycle
					start(nil, nil, nil)

					// Create CNC
					cnc := createTestCNC("delete-me-cnc", 300, defaultConnectSubnets(), "")

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Wait for connect router to be created
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "delete-me-cnc", 300)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Delete CNC
					err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Delete(
						context.Background(), "delete-me-cnc", metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Wait for connect router to be deleted
					Eventually(func() error {
						routerName := getConnectRouterName("delete-me-cnc")
						_, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: routerName})
						if err != nil {
							return nil // Router deleted, which is what we want
						}
						return errRouterStillExists
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should create ports and routes when CNC has only one allocated network", func() {
					// Setup with Layer3 network
					networks := []testNetwork{
						{name: "red-network", id: 1, topologyType: ovntypes.Layer3Topology},
					}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"red-network": {"10.128.1.0/24", "fd00:10:128:1::/64"},
						}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"red-network": networks[0]})

					// Create CNC with allocated network
					subnetAnnotation := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
					})
					cnc := createTestCNC("ports-cnc", 200, defaultConnectSubnets(), subnetAnnotation)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Get expected port IPs based on allocated connect subnet and node ID
					// Pass the allocated subnet (/24, /64) and node ID - function calculates P2P subnet
					connectNets, networkNets := getExpectedPortNetworks(
						ovntest.MustParseIPNet("192.168.0.0/24"),
						ovntest.MustParseIPNet("fd00:192:168::/64"),
						1, // node1 has ID 1
					)

					// Verify connect router
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "ports-cnc", 200)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port count on connect router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("ports-cnc"), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port count on network router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[0].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify ports are local (node is in same zone as controller)
					connectPortName := getConnectRouterToNetworkRouterPortName("ports-cnc", networks[0].name, "node1")
					networkPortName := getNetworkRouterToConnectRouterPortName(networks[0].name, "node1", "ports-cnc")
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, connectPortName, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 1), networkPortName)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, networkPortName, 0, connectPortName)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port on connect router (crtor-{cnc}-{network}-{node})
					// Connect router port uses first IP of P2P subnet
					Eventually(func() error {
						return verifyRouterPort(nbClient, getConnectRouterToNetworkRouterPortName(
							"ports-cnc", networks[0].name, "node1"), "ports-cnc", connectNets)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port on network router (rtocr-{network}-{node}-{cnc})
					// Network router port uses second IP of P2P subnet
					Eventually(func() error {
						return verifyRouterPort(nbClient, getNetworkRouterToConnectRouterPortName(
							networks[0].name, "node1", "ports-cnc"), "ports-cnc", networkNets)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no routing policies exist on network router since
					// there is only 1 network - so nothing to connect to
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[0].RouterName(),
							"ports-cnc", networks[0].id, networks[0].id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify static routes on connect router
					// IPPrefix = node's subnet, Nexthop = network router port IP
					nexthopV4, nexthopV6 := extractNexthops(networkNets)
					Eventually(func() error {
						return verifyRouterStaticRoutes(nbClient, getConnectRouterName("ports-cnc"),
							"ports-cnc", 1, 1,
							"10.128.1.0/24", "fd00:10:128:1::/64", nexthopV4, nexthopV6)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should not create ports, policies and routes when CNC has no subnet annotation", func() {
					// Setup with Layer3 network
					networks := []testNetwork{
						{name: "red-network", id: 1, topologyType: ovntypes.Layer3Topology},
					}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"red-network": {"10.128.1.0/24", "fd00:10:128:1::/64"},
						}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"red-network": networks[0]})

					cnc := createTestCNC("ports-cnc", 200, defaultConnectSubnets(), "")

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify connect router is created
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "ports-cnc", 200)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no ports on connect router (no subnet annotation = no allocated networks)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("ports-cnc"), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no ports on network router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[0].RouterName(), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[0].RouterName(),
							"ports-cnc", networks[0].id, networks[0].id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no static routes on connect router
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("ports-cnc"),
							"ports-cnc", 1, 1, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should create ports, policies and routes when CNC has more than one allocated network", func() {
					// Setup with two Layer3 networks
					networks := []testNetwork{
						{name: "red-network", id: 1, topologyType: ovntypes.Layer3Topology},
						{name: "blue-network", id: 2, topologyType: ovntypes.Layer3Topology},
					}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"red-network":  {"10.128.1.0/24", "fd00:10:128:1::/64"},
							"blue-network": {"10.129.1.0/24", "fd00:10:129:1::/64"},
						}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"red-network": networks[0], "blue-network": networks[1]})

					// Allocated connect subnets for each network
					type networkTestData struct {
						connectSubnetV4 string
						connectSubnetV6 string
						nodeSubnetV4    string
						nodeSubnetV6    string
						networkOwner    string
					}
					networkData := []networkTestData{
						{"192.168.0.0/24", "fd00:192:168::/64", "10.128.1.0/24", "fd00:10:128:1::/64", "layer3_1"},
						{"192.168.1.0/24", "fd00:192:168:1::/64", "10.129.1.0/24", "fd00:10:129:1::/64", "layer3_2"},
					}

					// Create CNC with both networks allocated
					subnetAnnotation := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
						"layer3_2": {"192.168.1.0/24", "fd00:192:168:1::/64"},
					})
					cnc := createTestCNC("ports-cnc", 200, defaultConnectSubnets(), subnetAnnotation)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify connect router is created
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "ports-cnc", 200)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port count on connect router (1 port per network = 2 total)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("ports-cnc"), len(networks))
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify each network's ports, policies, and routes
					for i, net := range networks {
						data := networkData[i]

						// Verify port count on this network's router (1 port for the node)
						Eventually(func() error {
							return verifyRouterPortsCount(nbClient, net.RouterName(), 1)
						}).WithTimeout(5 * time.Second).Should(Succeed())

						// Verify ports are local (node is in same zone as controller)
						connectPortName := getConnectRouterToNetworkRouterPortName("ports-cnc", net.name, "node1")
						networkPortName := getNetworkRouterToConnectRouterPortName(net.name, "node1", "ports-cnc")
						Eventually(func() error {
							return verifyRouterPortIsLocal(nbClient, connectPortName, getExpectedPortTunnelKey(data.connectSubnetV4, data.connectSubnetV6, ovntypes.Layer3Topology, 1), networkPortName)
						}).WithTimeout(5 * time.Second).Should(Succeed())

						Eventually(func() error {
							return verifyRouterPortIsLocal(nbClient, networkPortName, 0, connectPortName)
						}).WithTimeout(5 * time.Second).Should(Succeed())

						// Get expected port IPs based on allocated connect subnet and node ID
						connectNets, networkNets := getExpectedPortNetworks(
							ovntest.MustParseIPNet(data.connectSubnetV4),
							ovntest.MustParseIPNet(data.connectSubnetV6),
							1, // node1 has ID 1
						)

						// Verify port on connect router (crtor-{cnc}-{network}-{node})
						Eventually(func() error {
							return verifyRouterPort(nbClient, getConnectRouterToNetworkRouterPortName(
								"ports-cnc", net.name, "node1"), "ports-cnc", connectNets)
						}).WithTimeout(5 * time.Second).Should(Succeed())

						// Verify port on network router (rtocr-{network}-{node}-{cnc})
						Eventually(func() error {
							return verifyRouterPort(nbClient, getNetworkRouterToConnectRouterPortName(
								net.name, "node1", "ports-cnc"), "ports-cnc", networkNets)
						}).WithTimeout(5 * time.Second).Should(Succeed())

						// Verify routing policies on this network's router
						// Policies point to the OTHER network (1 per IP family)
						otherNetworkID := networks[1-i].id // 0->1, 1->0
						expectedPolicyCount := 0
						if config.IPv4Mode {
							expectedPolicyCount++
						}
						if config.IPv6Mode {
							expectedPolicyCount++
						}
						Eventually(func() error {
							return verifyRouterPolicy(nbClient, net.RouterName(),
								"ports-cnc", net.id, otherNetworkID, expectedPolicyCount)
						}).WithTimeout(5 * time.Second).Should(Succeed())

						// Verify static routes on connect router for this network
						// IPPrefix = node's subnet, Nexthop = network router port IP
						nexthopV4, nexthopV6 := extractNexthops(networkNets)
						Eventually(func() error {
							return verifyRouterStaticRoutes(nbClient, getConnectRouterName("ports-cnc"),
								"ports-cnc", net.id, 1,
								data.nodeSubnetV4, data.nodeSubnetV6, nexthopV4, nexthopV6)
						}).WithTimeout(5 * time.Second).Should(Succeed())
					}
				})

				It("should delete all ports, policies and routes when CNC with two networks is deleted", func() {
					// Setup with two Layer3 networks
					networks := []testNetwork{
						{name: "red-network", id: 1, topologyType: ovntypes.Layer3Topology},
						{name: "blue-network", id: 2, topologyType: ovntypes.Layer3Topology},
					}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"red-network":  {"10.128.1.0/24", "fd00:10:128:1::/64"},
							"blue-network": {"10.129.1.0/24", "fd00:10:129:1::/64"},
						}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"red-network": networks[0], "blue-network": networks[1]})

					// Create CNC with both networks allocated
					subnetAnnotation := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
						"layer3_2": {"192.168.1.0/24", "fd00:192:168:1::/64"},
					})
					cnc := createTestCNC("delete-cnc", 300, defaultConnectSubnets(), subnetAnnotation)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Wait for connect router, ports, and routes to be created
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "delete-cnc", 300)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("delete-cnc"), len(networks))
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Calculate expected counts per IP family
					expectedPerIPFamily := 0
					if config.IPv4Mode {
						expectedPerIPFamily++
					}
					if config.IPv6Mode {
						expectedPerIPFamily++
					}

					// Verify policies exist for each network before deletion
					// Each network's router has policies pointing to the OTHER network
					for i, net := range networks {
						otherNetworkID := networks[1-i].id
						Eventually(func() error {
							return verifyRouterPolicyCount(nbClient, net.RouterName(),
								"delete-cnc", net.id, otherNetworkID, expectedPerIPFamily)
						}).WithTimeout(5 * time.Second).Should(Succeed())
					}

					// Verify static routes exist for each network before deletion
					for _, net := range networks {
						Eventually(func() error {
							return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("delete-cnc"),
								"delete-cnc", net.id, 1, expectedPerIPFamily)
						}).WithTimeout(5 * time.Second).Should(Succeed())
					}

					// Delete the CNC
					err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Delete(
						context.Background(), "delete-cnc", metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify connect router is deleted
					Eventually(func() error {
						routerName := getConnectRouterName("delete-cnc")
						_, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: routerName})
						if err != nil {
							return nil // Router deleted, which is what we want
						}
						return errRouterStillExists
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify all ports are deleted from each network router
					for _, net := range networks {
						Eventually(func() error {
							return verifyRouterPortsCount(nbClient, net.RouterName(), 0)
						}).WithTimeout(5 * time.Second).Should(Succeed())
					}

					// Verify all policies are deleted from each network router
					for i, net := range networks {
						otherNetworkID := networks[1-i].id
						Eventually(func() error {
							return verifyRouterPolicyCount(nbClient, net.RouterName(),
								"delete-cnc", net.id, otherNetworkID, 0)
						}).WithTimeout(5 * time.Second).Should(Succeed())
					}

					// Note: Static routes are on the connect router which is already deleted
				})
			})

			// =============================================================================
			// Context: Network Changes
			// =============================================================================
			Context("Network Changes", func() {

				It("should add ports, policies and routes when new network is connected via annotation update", func() {
					// Setup with two networks (provide both v4 and v6 subnets)
					networks := []testNetwork{
						{name: "net1", id: 1, topologyType: ovntypes.Layer3Topology,
							subnets: []string{"10.128.0.0/14/23", "fd00:10:128::/48/64"}},
						{name: "net2", id: 2, topologyType: ovntypes.Layer3Topology,
							subnets: []string{"10.132.0.0/14/23", "fd00:10:132::/48/64"}},
					}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"net1": {"10.128.1.0/24", "fd00:10:128:1::/64"}, "net2": {"10.129.1.0/24", "fd00:10:129:1::/64"}}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"net1": networks[0], "net2": networks[1]})

					// Create CNC with only first network
					subnetAnnotation := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
					})
					cnc := createTestCNC("update-cnc", 400, defaultConnectSubnets(), subnetAnnotation)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify connect router exists
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "update-cnc", 400)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port on connect router for first network
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("update-cnc"), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port on first network's router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[0].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify static routes for first network (no policies since only 1 network)
					expectedRouteCount := 0
					if config.IPv4Mode {
						expectedRouteCount++
					}
					if config.IPv6Mode {
						expectedRouteCount++
					}
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("update-cnc"),
							"update-cnc", 1, 1, expectedRouteCount)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no policies since only 1 network (nothing to connect to)
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[0].RouterName(),
							"update-cnc", networks[0].id, networks[1].id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Update CNC to add second network
					updatedCNC, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.Background(), "update-cnc", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					updatedCNC.Annotations[ovnNetworkConnectSubnetAnnotation] = buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
						"layer3_2": {"192.168.1.0/24", "fd00:192:168:1::/64"},
					})
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), updatedCNC, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify port count on connect router (2 ports now)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("update-cnc"), 2)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port on second network's router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[1].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify static routes for second network
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("update-cnc"),
							"update-cnc", 2, 1, expectedRouteCount)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Now with 2 networks, policies should exist on both network routers
					// Each network's router has policies pointing to the other network
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[0].RouterName(),
							"update-cnc", networks[0].id, networks[1].id, expectedRouteCount)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[1].RouterName(),
							"update-cnc", networks[1].id, networks[0].id, expectedRouteCount)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should remove ports, policies and routes when network is disconnected via annotation update", func() {
					// Setup with two networks
					networks := []testNetwork{
						{name: "remove-net1", id: 1, topologyType: ovntypes.Layer3Topology,
							subnets: []string{"10.128.0.0/14/23", "fd00:10:128::/48/64"}},
						{name: "remove-net2", id: 2, topologyType: ovntypes.Layer3Topology,
							subnets: []string{"10.132.0.0/14/23", "fd00:10:132::/48/64"}},
					}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"remove-net1": {"10.128.1.0/24", "fd00:10:128:1::/64"}, "remove-net2": {"10.129.1.0/24", "fd00:10:129:1::/64"}}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"remove-net1": networks[0], "remove-net2": networks[1]})

					// Calculate expected count per IP family
					expectedPerIPFamily := 0
					if config.IPv4Mode {
						expectedPerIPFamily++
					}
					if config.IPv6Mode {
						expectedPerIPFamily++
					}

					// Create CNC with both networks (2 networks)
					subnetAnnotation := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
						"layer3_2": {"192.168.1.0/24", "fd00:192:168:1::/64"},
					})
					cnc := createTestCNC("remove-net-cnc", 500, defaultConnectSubnets(), subnetAnnotation)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// ========== VERIFY 2 NETWORKS ==========
					// Verify connect router
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "remove-net-cnc", 500)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify ports on connect router (2)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("remove-net-cnc"), 2)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify each network has ports, policies, routes
					for i, net := range networks {
						otherNetworkID := networks[1-i].id

						// Port on network router
						Eventually(func() error {
							return verifyRouterPortsCount(nbClient, net.RouterName(), 1)
						}).WithTimeout(5 * time.Second).Should(Succeed())

						// Policies pointing to other network
						Eventually(func() error {
							return verifyRouterPolicyCount(nbClient, net.RouterName(),
								"remove-net-cnc", net.id, otherNetworkID, expectedPerIPFamily)
						}).WithTimeout(5 * time.Second).Should(Succeed())

						// Static routes
						Eventually(func() error {
							return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("remove-net-cnc"),
								"remove-net-cnc", net.id, 1, expectedPerIPFamily)
						}).WithTimeout(5 * time.Second).Should(Succeed())
					}

					// ========== UPDATE TO 1 NETWORK ==========
					updatedCNC, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.Background(), "remove-net-cnc", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					updatedCNC.Annotations[ovnNetworkConnectSubnetAnnotation] = buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
					})
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), updatedCNC, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify ports on connect router (1 now)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("remove-net-cnc"), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify first network still has port
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[0].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify second network has no ports
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[1].RouterName(), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no policies on first network (only 1 network now, nothing to connect to)
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[0].RouterName(),
							"remove-net-cnc", networks[0].id, networks[1].id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify first network still has routes
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("remove-net-cnc"),
							"remove-net-cnc", 1, 1, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify second network has no routes
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("remove-net-cnc"),
							"remove-net-cnc", 2, 1, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// ========== UPDATE TO 0 NETWORKS ==========
					updatedCNC, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.Background(), "remove-net-cnc", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					updatedCNC.Annotations[ovnNetworkConnectSubnetAnnotation] = ""
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), updatedCNC, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify no ports on connect router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("remove-net-cnc"), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no ports on either network router
					for _, net := range networks {
						Eventually(func() error {
							return verifyRouterPortsCount(nbClient, net.RouterName(), 0)
						}).WithTimeout(5 * time.Second).Should(Succeed())
					}

					// Verify no routes on connect router
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("remove-net-cnc"),
							"remove-net-cnc", 1, 1, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should handle Layer2 networks with 0 -> 1 -> 2 -> 1 -> 0 network transitions", func() {
					// Setup with two Layer2 networks
					networks := []testNetwork{
						{name: "l2-net1", id: 1, topologyType: ovntypes.Layer2Topology, subnets: []string{"10.200.0.0/16", "fd00:10:200::/48"}},
						{name: "l2-net2", id: 2, topologyType: ovntypes.Layer2Topology, subnets: []string{"10.201.0.0/16", "fd00:10:201::/48"}},
					}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1"},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"l2-net1": networks[0], "l2-net2": networks[1]})

					// Calculate expected count per IP family
					expectedPerIPFamily := 0
					if config.IPv4Mode {
						expectedPerIPFamily++
					}
					if config.IPv6Mode {
						expectedPerIPFamily++
					}

					// ========== START WITH 0 NETWORKS ==========
					// Create CNC with no networks (empty annotation)
					cnc := createTestCNC("l2-cnc", 850, defaultConnectSubnets(), "")

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify connect router exists but has no ports
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "l2-cnc", 850)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("l2-cnc"), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no ports on network routers
					for _, net := range networks {
						Eventually(func() error {
							return verifyRouterPortsCount(nbClient, net.RouterName(), 0)
						}).WithTimeout(5 * time.Second).Should(Succeed())
					}

					// Verify no routes
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("l2-cnc"),
							"l2-cnc", 1, 0, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("l2-cnc"),
							"l2-cnc", 2, 0, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no policies on network routers
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[0].RouterName(),
							"l2-cnc", networks[0].id, networks[1].id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[1].RouterName(),
							"l2-cnc", networks[1].id, networks[0].id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// ========== ADD NETWORK 1 (0 -> 1) ==========
					updatedCNC, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.Background(), "l2-cnc", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					updatedCNC.Annotations[ovnNetworkConnectSubnetAnnotation] = buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer2_1": {"192.168.0.0/31", "fd00:192:168::/127"},
					})
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), updatedCNC, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify 1 port on connect router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("l2-cnc"), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify 1 port on network1 router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[0].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port is local (node1 is in same zone as controller)
					// Layer2 uses subIndex=0 for tunnel key, and no per-node ports (empty nodeName)
					l2ConnectPort1 := getConnectRouterToNetworkRouterPortName("l2-cnc", "l2-net1", "")
					l2NetworkPort1 := getNetworkRouterToConnectRouterPortName("l2-net1", "", "l2-cnc")
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, l2ConnectPort1, getExpectedPortTunnelKey("192.168.0.0/31", "fd00:192:168::/127", ovntypes.Layer2Topology, 0), l2NetworkPort1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, l2NetworkPort1, 0, l2ConnectPort1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify routes for network1 (Layer2 uses nodeID=0)
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("l2-cnc"),
							"l2-cnc", 1, 0, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// No policies with only 1 network
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[0].RouterName(),
							"l2-cnc", networks[0].id, networks[1].id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// ========== ADD NETWORK 2 (1 -> 2) ==========
					updatedCNC, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.Background(), "l2-cnc", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					updatedCNC.Annotations[ovnNetworkConnectSubnetAnnotation] = buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer2_1": {"192.168.0.0/31", "fd00:192:168::/127"},
						"layer2_2": {"192.168.1.0/31", "fd00:192:168:1::/127"},
					})
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), updatedCNC, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify 2 ports on connect router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("l2-cnc"), 2)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify each network
					l2Subnets := []subnetPair{
						{"192.168.0.0/31", "fd00:192:168::/127"},
						{"192.168.1.0/31", "fd00:192:168:1::/127"},
					}
					for i, net := range networks {
						otherNetworkID := networks[1-i].id

						// Port on network router
						Eventually(func() error {
							return verifyRouterPortsCount(nbClient, net.RouterName(), 1)
						}).WithTimeout(5 * time.Second).Should(Succeed())

						// Verify port is local (node1 in same zone as controller)
						// Layer2 uses empty nodeName (no per-node ports)
						connectPort := getConnectRouterToNetworkRouterPortName("l2-cnc", net.name, "")
						networkPort := getNetworkRouterToConnectRouterPortName(net.name, "", "l2-cnc")
						Eventually(func() error {
							return verifyRouterPortIsLocal(nbClient, connectPort, getExpectedPortTunnelKey(l2Subnets[i].v4, l2Subnets[i].v6, ovntypes.Layer2Topology, 0), networkPort)
						}).WithTimeout(5 * time.Second).Should(Succeed())

						Eventually(func() error {
							return verifyRouterPortIsLocal(nbClient, networkPort, 0, connectPort)
						}).WithTimeout(5 * time.Second).Should(Succeed())

						// Policies pointing to other network
						Eventually(func() error {
							return verifyRouterPolicyCount(nbClient, net.RouterName(),
								"l2-cnc", net.id, otherNetworkID, expectedPerIPFamily)
						}).WithTimeout(5 * time.Second).Should(Succeed())

						// Static routes (Layer2 uses nodeID=0)
						Eventually(func() error {
							return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("l2-cnc"),
								"l2-cnc", net.id, 0, expectedPerIPFamily)
						}).WithTimeout(5 * time.Second).Should(Succeed())
					}

					// ========== REMOVE NETWORK 1 (2 -> 1) ==========
					updatedCNC, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.Background(), "l2-cnc", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					updatedCNC.Annotations[ovnNetworkConnectSubnetAnnotation] = buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer2_2": {"192.168.1.0/31", "fd00:192:168:1::/127"},
					})
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), updatedCNC, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify 1 port on connect router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("l2-cnc"), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify network1 has no ports
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[0].RouterName(), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify network2 still has port
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[1].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify network2 port is local (Layer2: empty nodeName)
					l2ConnectPort2 := getConnectRouterToNetworkRouterPortName("l2-cnc", "l2-net2", "")
					l2NetworkPort2 := getNetworkRouterToConnectRouterPortName("l2-net2", "", "l2-cnc")
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, l2ConnectPort2, getExpectedPortTunnelKey("192.168.1.0/31", "fd00:192:168:1::/127", ovntypes.Layer2Topology, 0), l2NetworkPort2)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, l2NetworkPort2, 0, l2ConnectPort2)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no policies on network2 (only 1 network now)
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[1].RouterName(),
							"l2-cnc", networks[1].id, networks[0].id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify network1 has no routes
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("l2-cnc"),
							"l2-cnc", 1, 0, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify network2 still has routes
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("l2-cnc"),
							"l2-cnc", 2, 0, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// ========== REMOVE NETWORK 2 (1 -> 0) ==========
					updatedCNC, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.Background(), "l2-cnc", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					updatedCNC.Annotations[ovnNetworkConnectSubnetAnnotation] = ""
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), updatedCNC, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify no ports on connect router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("l2-cnc"), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no ports on either network router
					for _, net := range networks {
						Eventually(func() error {
							return verifyRouterPortsCount(nbClient, net.RouterName(), 0)
						}).WithTimeout(5 * time.Second).Should(Succeed())
					}

					// Verify no policies on network routers (both networks pointing to each other should be gone)
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[0].RouterName(),
							"l2-cnc", networks[0].id, networks[1].id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[1].RouterName(),
							"l2-cnc", networks[1].id, networks[0].id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no routes on connect router
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("l2-cnc"),
							"l2-cnc", 2, 0, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should handle mixed Layer2 and Layer3 networks with 0 -> 1 -> 2 -> 1 -> 0 transitions", func() {
					// Setup with one Layer2 and one Layer3 network (provide both v4 and v6 subnets)
					l2Net := testNetwork{name: "mixed-l2", id: 1, topologyType: ovntypes.Layer2Topology,
						subnets: []string{"10.200.0.0/16", "fd00:10:200::/48"}}
					l3Net := testNetwork{name: "mixed-l3", id: 2, topologyType: ovntypes.Layer3Topology,
						subnets: []string{"10.128.0.0/14/23", "fd00:10:128::/48/64"}}
					networks := []testNetwork{l2Net, l3Net}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"mixed-l3": {"10.128.1.0/24", "fd00:10:128:1::/64"}}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"mixed-l2": l2Net, "mixed-l3": l3Net})

					// Calculate expected count per IP family
					expectedPerIPFamily := 0
					if config.IPv4Mode {
						expectedPerIPFamily++
					}
					if config.IPv6Mode {
						expectedPerIPFamily++
					}

					// ========== START WITH 0 NETWORKS ==========
					cnc := createTestCNC("mixed-cnc", 900, defaultConnectSubnets(), "")

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify connect router exists but has no ports
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "mixed-cnc", 900)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("mixed-cnc"), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no ports on network routers
					for _, net := range networks {
						Eventually(func() error {
							return verifyRouterPortsCount(nbClient, net.RouterName(), 0)
						}).WithTimeout(5 * time.Second).Should(Succeed())
					}

					// Verify no routes
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("mixed-cnc"),
							"mixed-cnc", 1, 0, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("mixed-cnc"),
							"mixed-cnc", 2, 1, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// ========== ADD LAYER2 NETWORK (0 -> 1) ==========
					updatedCNC, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.Background(), "mixed-cnc", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					updatedCNC.Annotations[ovnNetworkConnectSubnetAnnotation] = buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer2_1": {"192.168.0.0/31", "fd00:192:168::/127"},
					})
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), updatedCNC, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify 1 port on connect router (Layer2 has 1 port)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("mixed-cnc"), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer2 network router has port
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, l2Net.RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer2 port is local (node1 in same zone as controller)
					// Layer2: empty nodeName (no per-node ports)
					mixedL2ConnectPort := getConnectRouterToNetworkRouterPortName("mixed-cnc", l2Net.name, "")
					mixedL2NetworkPort := getNetworkRouterToConnectRouterPortName(l2Net.name, "", "mixed-cnc")
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, mixedL2ConnectPort, getExpectedPortTunnelKey("192.168.0.0/31", "fd00:192:168::/127", ovntypes.Layer2Topology, 0), mixedL2NetworkPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, mixedL2NetworkPort, 0, mixedL2ConnectPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer3 network router has no port yet
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, l3Net.RouterName(), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify routes for Layer2 (nodeID=0)
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("mixed-cnc"),
							"mixed-cnc", 1, 0, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// No policies with only 1 network
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, l2Net.RouterName(),
							"mixed-cnc", l2Net.id, l3Net.id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// ========== ADD LAYER3 NETWORK (1 -> 2) ==========
					updatedCNC, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.Background(), "mixed-cnc", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					updatedCNC.Annotations[ovnNetworkConnectSubnetAnnotation] = buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer2_1": {"192.168.0.0/31", "fd00:192:168::/127"},
						"layer3_2": {"192.168.1.0/24", "fd00:192:168:1::/64"},
					})
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), updatedCNC, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify 2 ports on connect router (1 for Layer2, 1 for Layer3 node1)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("mixed-cnc"), 2)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify both network routers have ports
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, l2Net.RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, l3Net.RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer2 port is local
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, mixedL2ConnectPort, getExpectedPortTunnelKey("192.168.0.0/31", "fd00:192:168::/127", ovntypes.Layer2Topology, 0), mixedL2NetworkPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer3 port is local (node1 id=1)
					mixedL3ConnectPort := getConnectRouterToNetworkRouterPortName("mixed-cnc", l3Net.name, "node1")
					mixedL3NetworkPort := getNetworkRouterToConnectRouterPortName(l3Net.name, "node1", "mixed-cnc")
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, mixedL3ConnectPort, getExpectedPortTunnelKey("192.168.1.0/24", "fd00:192:168:1::/64", ovntypes.Layer3Topology, 1), mixedL3NetworkPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, mixedL3NetworkPort, 0, mixedL3ConnectPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify routes for both networks
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("mixed-cnc"),
							"mixed-cnc", 1, 0, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("mixed-cnc"),
							"mixed-cnc", 2, 1, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify policies on both routers pointing to each other
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, l2Net.RouterName(),
							"mixed-cnc", l2Net.id, l3Net.id, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, l3Net.RouterName(),
							"mixed-cnc", l3Net.id, l2Net.id, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// ========== REMOVE LAYER2 NETWORK (2 -> 1) ==========
					updatedCNC, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.Background(), "mixed-cnc", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					updatedCNC.Annotations[ovnNetworkConnectSubnetAnnotation] = buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_2": {"192.168.1.0/24", "fd00:192:168:1::/64"},
					})
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), updatedCNC, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify 1 port on connect router (only Layer3 now)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("mixed-cnc"), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer2 has no ports
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, l2Net.RouterName(), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer3 still has port
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, l3Net.RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer3 port is local (node1 id=1)
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, mixedL3ConnectPort, getExpectedPortTunnelKey("192.168.1.0/24", "fd00:192:168:1::/64", ovntypes.Layer3Topology, 1), mixedL3NetworkPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, mixedL3NetworkPort, 0, mixedL3ConnectPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer2 routes removed
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("mixed-cnc"),
							"mixed-cnc", 1, 0, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer3 routes still exist
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("mixed-cnc"),
							"mixed-cnc", 2, 1, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no policies on Layer3 (only 1 network now)
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, l3Net.RouterName(),
							"mixed-cnc", l3Net.id, l2Net.id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// ========== REMOVE LAYER3 NETWORK (1 -> 0) ==========
					updatedCNC, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(
						context.Background(), "mixed-cnc", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					updatedCNC.Annotations[ovnNetworkConnectSubnetAnnotation] = ""
					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Update(
						context.Background(), updatedCNC, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify no ports on connect router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("mixed-cnc"), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify no ports on either network router
					for _, net := range networks {
						Eventually(func() error {
							return verifyRouterPortsCount(nbClient, net.RouterName(), 0)
						}).WithTimeout(5 * time.Second).Should(Succeed())
					}

					// Verify no routes
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("mixed-cnc"),
							"mixed-cnc", 2, 1, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})
			})

			// =============================================================================
			// Context: Node Events
			// =============================================================================
			Context("Node Events", func() {

				It("should add ports and routes when a new node is added", func() {
					// Setup with one node initially
					networks := []testNetwork{
						{name: "node-add-net", id: 1, topologyType: ovntypes.Layer3Topology,
							subnets: []string{"10.128.0.0/14/23", "fd00:10:128::/48/64"}},
					}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"node-add-net": {"10.128.1.0/24", "fd00:10:128:1::/64"}}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"node-add-net": networks[0]})

					// Create CNC
					subnetAnnotation := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
					})
					cnc := createTestCNC("node-add-cnc", 1600, defaultConnectSubnets(), subnetAnnotation)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify initial state: 1 port on connect router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("node-add-cnc"), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify initial port is local (node1 is in same zone as controller)
					connectPortName := getConnectRouterToNetworkRouterPortName("node-add-cnc", "node-add-net", "node1")
					networkPortName := getNetworkRouterToConnectRouterPortName("node-add-net", "node1", "node-add-cnc")
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, connectPortName, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 1), networkPortName)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port details for node1
					connectNets1, networkNets1 := getExpectedPortNetworks(
						ovntest.MustParseIPNet("192.168.0.0/24"),
						ovntest.MustParseIPNet("fd00:192:168::/64"),
						1, // node1 has ID 1
					)
					Eventually(func() error {
						return verifyRouterPort(nbClient, connectPortName, "node-add-cnc", connectNets1)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPort(nbClient, networkPortName, "node-add-cnc", networkNets1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify initial routes for node1
					// IPPrefix = node's subnet, Nexthop = network router port IP
					nexthopV4_1, nexthopV6_1 := extractNexthops(networkNets1)
					Eventually(func() error {
						return verifyRouterStaticRoutes(nbClient, getConnectRouterName("node-add-cnc"),
							"node-add-cnc", 1, 1,
							"10.128.1.0/24", "fd00:10:128:1::/64", nexthopV4_1, nexthopV6_1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Add a new node
					newNode := createTestNode(testNode{
						name: "node2", id: 2, zone: "node2",
						nodeSubnets: map[string]subnetPair{"node-add-net": {"10.128.2.0/24", "fd00:10:128:2::/64"}},
					})
					_, err = fakeClientset.KubeClient.CoreV1().Nodes().Create(
						context.Background(), newNode, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify new port added (2 total now)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("node-add-cnc"), 2)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify node2's port is remote (node2 is in different zone "node2")
					connectPortName2 := getConnectRouterToNetworkRouterPortName("node-add-cnc", "node-add-net", "node2")
					Eventually(func() error {
						return verifyRouterPortIsRemote(nbClient, connectPortName2, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 2))
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port details for node2 (remote - only connect router port)
					connectNets2, networkNets2 := getExpectedPortNetworks(
						ovntest.MustParseIPNet("192.168.0.0/24"),
						ovntest.MustParseIPNet("fd00:192:168::/64"),
						2, // node2 has ID 2
					)
					Eventually(func() error {
						return verifyRouterPort(nbClient, connectPortName2, "node-add-cnc", connectNets2)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify routes for both nodes
					// node1 routes verified above, now verify node2 routes
					nexthopV4_2, nexthopV6_2 := extractNexthops(networkNets2)
					Eventually(func() error {
						return verifyRouterStaticRoutes(nbClient, getConnectRouterName("node-add-cnc"),
							"node-add-cnc", 1, 2,
							"10.128.2.0/24", "fd00:10:128:2::/64", nexthopV4_2, nexthopV6_2)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should remove ports and routes when a node is deleted", func() {
					// Setup with two nodes initially
					networks := []testNetwork{
						{name: "node-del-net", id: 1, topologyType: ovntypes.Layer3Topology,
							subnets: []string{"10.128.0.0/14/23", "fd00:10:128::/48/64"}},
					}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"node-del-net": {"10.128.1.0/24", "fd00:10:128:1::/64"}}},
						{name: "node2", id: 2, zone: "node2", nodeSubnets: map[string]subnetPair{
							"node-del-net": {"10.128.2.0/24", "fd00:10:128:2::/64"}}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"node-del-net": networks[0]})

					// Create CNC
					subnetAnnotation := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
					})
					cnc := createTestCNC("node-del-cnc", 1700, defaultConnectSubnets(), subnetAnnotation)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify initial state: 2 ports on connect router
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("node-del-cnc"), 2)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port details for node1 (local)
					connectPortName1 := getConnectRouterToNetworkRouterPortName("node-del-cnc", "node-del-net", "node1")
					networkPortName1 := getNetworkRouterToConnectRouterPortName("node-del-net", "node1", "node-del-cnc")
					connectNets1, networkNets1 := getExpectedPortNetworks(
						ovntest.MustParseIPNet("192.168.0.0/24"),
						ovntest.MustParseIPNet("fd00:192:168::/64"),
						1, // node1 has ID 1
					)
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, connectPortName1, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 1), networkPortName1)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPort(nbClient, connectPortName1, "node-del-cnc", connectNets1)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPort(nbClient, networkPortName1, "node-del-cnc", networkNets1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port details for node2 (remote)
					connectPortName2 := getConnectRouterToNetworkRouterPortName("node-del-cnc", "node-del-net", "node2")
					connectNets2, networkNets2 := getExpectedPortNetworks(
						ovntest.MustParseIPNet("192.168.0.0/24"),
						ovntest.MustParseIPNet("fd00:192:168::/64"),
						2, // node2 has ID 2
					)
					Eventually(func() error {
						return verifyRouterPortIsRemote(nbClient, connectPortName2, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 2))
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPort(nbClient, connectPortName2, "node-del-cnc", connectNets2)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify routes for both nodes with full details
					nexthopV4_1, nexthopV6_1 := extractNexthops(networkNets1)
					Eventually(func() error {
						return verifyRouterStaticRoutes(nbClient, getConnectRouterName("node-del-cnc"),
							"node-del-cnc", 1, 1,
							"10.128.1.0/24", "fd00:10:128:1::/64", nexthopV4_1, nexthopV6_1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					nexthopV4_2, nexthopV6_2 := extractNexthops(networkNets2)
					Eventually(func() error {
						return verifyRouterStaticRoutes(nbClient, getConnectRouterName("node-del-cnc"),
							"node-del-cnc", 1, 2,
							"10.128.2.0/24", "fd00:10:128:2::/64", nexthopV4_2, nexthopV6_2)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Delete node2
					err = fakeClientset.KubeClient.CoreV1().Nodes().Delete(
						context.Background(), "node2", metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify port removed (1 remaining)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("node-del-cnc"), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify node1 port still exists with correct details
					Eventually(func() error {
						return verifyRouterPort(nbClient, connectPortName1, "node-del-cnc", connectNets1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify routes for node2 are removed
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("node-del-cnc"),
							"node-del-cnc", 1, 2, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify routes for node1 still exist with full details
					Eventually(func() error {
						return verifyRouterStaticRoutes(nbClient, getConnectRouterName("node-del-cnc"),
							"node-del-cnc", 1, 1,
							"10.128.1.0/24", "fd00:10:128:1::/64", nexthopV4_1, nexthopV6_1)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should not create ports or routes until node ID is allocated", func() {
					// Setup with network but node WITHOUT node ID annotation
					networks := []testNetwork{
						{name: "nodeid-wait-net", id: 1, topologyType: ovntypes.Layer3Topology,
							subnets: []string{"10.128.0.0/14/23", "fd00:10:128::/48/64"}},
					}
					// Create node without node ID (id: 0 means no annotation)
					nodes := []testNode{
						{name: "node1", id: 0, zone: "node1", nodeSubnets: map[string]subnetPair{
							"nodeid-wait-net": {"10.128.1.0/24", "fd00:10:128:1::/64"}}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"nodeid-wait-net": networks[0]})

					// Create CNC
					subnetAnnotation := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
					})
					cnc := createTestCNC("nodeid-wait-cnc", 1750, defaultConnectSubnets(), subnetAnnotation)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify connect router is created but has no ports (node has no ID)
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "nodeid-wait-cnc", 1750)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Consistently(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("nodeid-wait-cnc"), 0)
					}).WithTimeout(2 * time.Second).Should(Succeed())

					// Verify network router also has no connect ports
					Consistently(func() error {
						return verifyRouterPortsCount(nbClient, networks[0].RouterName(), 0)
					}).WithTimeout(2 * time.Second).Should(Succeed())

					// Verify no static routes either
					Consistently(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("nodeid-wait-cnc"),
							"nodeid-wait-cnc", networks[0].id, 0, 0)
					}).WithTimeout(2 * time.Second).Should(Succeed())

					// Now add node ID annotation
					node, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(
						context.Background(), "node1", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					node.Annotations[util.OvnNodeID] = "1"
					_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(
						context.Background(), node, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Now ports and routes should be created
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("nodeid-wait-cnc"), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify network router also has the connect port
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[0].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify ports are local (node is in same zone as controller)
					connectPortName := getConnectRouterToNetworkRouterPortName("nodeid-wait-cnc", "nodeid-wait-net", "node1")
					networkPortName := getNetworkRouterToConnectRouterPortName("nodeid-wait-net", "node1", "nodeid-wait-cnc")
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, connectPortName, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 1), networkPortName)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, networkPortName, 0, connectPortName)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port details
					connectNets, networkNets := getExpectedPortNetworks(
						ovntest.MustParseIPNet("192.168.0.0/24"),
						ovntest.MustParseIPNet("fd00:192:168::/64"),
						1, // node1 has ID 1
					)
					Eventually(func() error {
						return verifyRouterPort(nbClient, connectPortName, "nodeid-wait-cnc", connectNets)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPort(nbClient, networkPortName, "nodeid-wait-cnc", networkNets)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify static routes with full details
					nexthopV4, nexthopV6 := extractNexthops(networkNets)
					Eventually(func() error {
						return verifyRouterStaticRoutes(nbClient, getConnectRouterName("nodeid-wait-cnc"),
							"nodeid-wait-cnc", 1, 1,
							"10.128.1.0/24", "fd00:10:128:1::/64", nexthopV4, nexthopV6)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should reconcile when node zone annotation is updated", func() {
					// Setup with node in local zone
					// The controller considers a node "local" when util.GetNodeZone(node) == c.zone
					// So we set controller's zoneName to match the node's zone annotation
					zoneName = "node1"
					networks := []testNetwork{
						{name: "zone-update-net", id: 1, topologyType: ovntypes.Layer3Topology,
							subnets: []string{"10.128.0.0/14/23", "fd00:10:128::/48/64"}},
					}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"zone-update-net": {"10.128.1.0/24", "fd00:10:128:1::/64"}}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"zone-update-net": networks[0]})

					// Create CNC
					subnetAnnotation := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
					})
					cnc := createTestCNC("zone-update-cnc", 1800, defaultConnectSubnets(), subnetAnnotation)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify initial port (local zone node)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("zone-update-cnc"), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify ports are local with peer set
					connectPortName := getConnectRouterToNetworkRouterPortName("zone-update-cnc", "zone-update-net", "node1")
					networkPortName := getNetworkRouterToConnectRouterPortName("zone-update-net", "node1", "zone-update-cnc")
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, connectPortName, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 1), networkPortName)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, networkPortName, 0, connectPortName)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port details
					connectNets, networkNets := getExpectedPortNetworks(
						ovntest.MustParseIPNet("192.168.0.0/24"),
						ovntest.MustParseIPNet("fd00:192:168::/64"),
						1, // node1 has ID 1
					)
					Eventually(func() error {
						return verifyRouterPort(nbClient, connectPortName, "zone-update-cnc", connectNets)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPort(nbClient, networkPortName, "zone-update-cnc", networkNets)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify initial static routes with full details
					nexthopV4, nexthopV6 := extractNexthops(networkNets)
					Eventually(func() error {
						return verifyRouterStaticRoutes(nbClient, getConnectRouterName("zone-update-cnc"),
							"zone-update-cnc", 1, 1,
							"10.128.1.0/24", "fd00:10:128:1::/64", nexthopV4, nexthopV6)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Update node's zone annotation to a different zone
					node, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(
						context.Background(), "node1", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					node.Annotations[util.OvnNodeZoneName] = "zone2"
					_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(
						context.Background(), node, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Controller should reconcile - port and routes should still exist (now treated as remote zone)
					Consistently(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("zone-update-cnc"), 1)
					}).WithTimeout(2 * time.Second).Should(Succeed())

					// After zone update, connect router port should be remote (no peer, has requested-chassis)
					Eventually(func() error {
						return verifyRouterPortIsRemote(nbClient, connectPortName, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 1))
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Network router port is deleted for remote nodes - only connect router side port exists
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[0].RouterName(), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify routes still exist after zone change
					Consistently(func() error {
						return verifyRouterStaticRoutes(nbClient, getConnectRouterName("zone-update-cnc"),
							"zone-update-cnc", 1, 1,
							"10.128.1.0/24", "fd00:10:128:1::/64", nexthopV4, nexthopV6)
					}).WithTimeout(2 * time.Second).Should(Succeed())
				})

				It("should handle multiple zones", func() {
					// Controller in zone "local-node"
					zoneName = "local-node"
					networks := []testNetwork{
						{name: "zone-net", id: 1, topologyType: ovntypes.Layer3Topology,
							subnets: []string{"10.128.0.0/14/23", "fd00:10:128::/48/64"}},
					}
					nodes := []testNode{
						// local-node has zone annotation "local-node" which matches c.zone
						{name: "local-node", id: 1, zone: "local-node", nodeSubnets: map[string]subnetPair{
							"zone-net": {"10.128.1.0/24", "fd00:10:128:1::/64"}}},
						// remote-node is in different zone
						{name: "remote-node-1", id: 2, zone: "remote-zone", nodeSubnets: map[string]subnetPair{
							"zone-net": {"10.128.2.0/24", "fd00:10:128:2::/64"}}},
						{name: "remote-node-2", id: 3, zone: "remote-zone", nodeSubnets: map[string]subnetPair{
							"zone-net": {"10.128.3.0/24", "fd00:10:128:3::/64"}}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"zone-net": networks[0]})

					expectedPerIPFamily := 0
					if config.IPv4Mode {
						expectedPerIPFamily++
					}
					if config.IPv6Mode {
						expectedPerIPFamily++
					}

					// Create CNC with IP mode-aware subnets
					subnetAnnotation := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
					})
					cnc := createTestCNC("zone-cnc", 1500, defaultConnectSubnets(), subnetAnnotation)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify 3 ports on connect router (one for each node: 1 local + 2 remote)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("zone-cnc"), 3)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify local-node port is local (with peer to network router)
					localConnectPort := getConnectRouterToNetworkRouterPortName("zone-cnc", "zone-net", "local-node")
					localNetworkPort := getNetworkRouterToConnectRouterPortName("zone-net", "local-node", "zone-cnc")
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, localConnectPort, 0, localNetworkPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port details for local-node
					localConnectNets, localNetworkNets := getExpectedPortNetworks(
						ovntest.MustParseIPNet("192.168.0.0/24"),
						ovntest.MustParseIPNet("fd00:192:168::/64"),
						1, // local-node has ID 1
					)
					Eventually(func() error {
						return verifyRouterPort(nbClient, localConnectPort, "zone-cnc", localConnectNets)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPort(nbClient, localNetworkPort, "zone-cnc", localNetworkNets)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify network router has port for local node only (remote nodes don't get network router ports)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[0].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, localNetworkPort, 0, localConnectPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify remote-node-1 port is remote (no peer, has requested-chassis)
					remoteConnectPort1 := getConnectRouterToNetworkRouterPortName("zone-cnc", "zone-net", "remote-node-1")
					Eventually(func() error {
						return verifyRouterPortIsRemote(nbClient, remoteConnectPort1, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 2))
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port details for remote-node-1
					remoteConnectNets1, _ := getExpectedPortNetworks(
						ovntest.MustParseIPNet("192.168.0.0/24"),
						ovntest.MustParseIPNet("fd00:192:168::/64"),
						2, // remote-node-1 has ID 2
					)
					Eventually(func() error {
						return verifyRouterPort(nbClient, remoteConnectPort1, "zone-cnc", remoteConnectNets1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify remote-node-2 port is remote (no peer, has requested-chassis)
					remoteConnectPort2 := getConnectRouterToNetworkRouterPortName("zone-cnc", "zone-net", "remote-node-2")
					Eventually(func() error {
						return verifyRouterPortIsRemote(nbClient, remoteConnectPort2, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 3))
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify port details for remote-node-2
					remoteConnectNets2, _ := getExpectedPortNetworks(
						ovntest.MustParseIPNet("192.168.0.0/24"),
						ovntest.MustParseIPNet("fd00:192:168::/64"),
						3, // remote-node-2 has ID 3
					)
					Eventually(func() error {
						return verifyRouterPort(nbClient, remoteConnectPort2, "zone-cnc", remoteConnectNets2)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify static routes on connect router for all 3 nodes
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("zone-cnc"),
							"zone-cnc", 1, 1, expectedPerIPFamily) // local-node routes
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("zone-cnc"),
							"zone-cnc", 1, 2, expectedPerIPFamily) // remote-node-1 routes
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("zone-cnc"),
							"zone-cnc", networks[0].id, 3, expectedPerIPFamily) // remote-node-2 routes
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should update routes when node subnet annotation is updated", func() {
					// Setup
					networks := []testNetwork{
						{name: "subnet-update-net", id: 1, topologyType: ovntypes.Layer3Topology,
							subnets: []string{"10.128.0.0/14/23", "fd00:10:128::/48/64"}},
					}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"subnet-update-net": {"10.128.1.0/24", "fd00:10:128:1::/64"}}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"subnet-update-net": networks[0]})

					// Create CNC
					subnetAnnotation := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
					})
					cnc := createTestCNC("subnet-update-cnc", 1900, defaultConnectSubnets(), subnetAnnotation)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify initial port
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("subnet-update-cnc"), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify initial port details
					connectPortName := getConnectRouterToNetworkRouterPortName("subnet-update-cnc", "subnet-update-net", "node1")
					networkPortName := getNetworkRouterToConnectRouterPortName("subnet-update-net", "node1", "subnet-update-cnc")
					connectNets, networkNets := getExpectedPortNetworks(
						ovntest.MustParseIPNet("192.168.0.0/24"),
						ovntest.MustParseIPNet("fd00:192:168::/64"),
						1, // node1 has ID 1
					)
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, connectPortName, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 1), networkPortName)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPort(nbClient, connectPortName, "subnet-update-cnc", connectNets)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPort(nbClient, networkPortName, "subnet-update-cnc", networkNets)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify initial routes with full details
					nexthopV4, nexthopV6 := extractNexthops(networkNets)
					Eventually(func() error {
						return verifyRouterStaticRoutes(nbClient, getConnectRouterName("subnet-update-cnc"),
							"subnet-update-cnc", 1, 1,
							"10.128.1.0/24", "fd00:10:128:1::/64", nexthopV4, nexthopV6)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Update node subnet annotation
					node, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(
						context.Background(), "node1", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					node.Annotations[ovnNodeSubnetsAnnotation] = buildNodeSubnetAnnotation(map[string]subnetPair{
						"subnet-update-net": {"10.128.10.0/24", "fd00:10:128:10::/64"},
					})
					_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(
						context.Background(), node, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Controller should react and update routes
					// Port count stays the same
					Consistently(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("subnet-update-cnc"), 1)
					}).WithTimeout(2 * time.Second).Should(Succeed())

					// Port details should remain unchanged
					Eventually(func() error {
						return verifyRouterPort(nbClient, connectPortName, "subnet-update-cnc", connectNets)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Routes should still exist with updated destination prefix
					Eventually(func() error {
						return verifyRouterStaticRoutes(nbClient, getConnectRouterName("subnet-update-cnc"),
							"subnet-update-cnc", 1, 1,
							"10.128.10.0/24", "fd00:10:128:10::/64", nexthopV4, nexthopV6)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})

				It("should create ports for all nodes when multiple networks are connected", func() {
					// Setup with 1 Layer3 and 1 Layer2 network and 2 nodes (1 local, 1 remote)
					zoneName = "node1"
					l3Net := testNetwork{name: "net1", id: 1, topologyType: ovntypes.Layer3Topology, subnets: []string{"10.128.0.0/14/23", "fd00:10:128::/48/64"}}
					l2Net := testNetwork{name: "net2", id: 2, topologyType: ovntypes.Layer2Topology, subnets: []string{"10.132.0.0/16", "fd00:10:132::/48"}}
					networks := []testNetwork{l3Net, l2Net}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"net1": {"10.128.1.0/24", "fd00:10:128:1::/64"},
						}},
						{name: "node2", id: 2, zone: "node2", nodeSubnets: map[string]subnetPair{
							"net1": {"10.128.2.0/24", "fd00:10:128:2::/64"},
						}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{"net1": l3Net, "net2": l2Net})

					// Create CNC with Layer3 and Layer2 networks
					// Network index is based on subnet position within connect subnet range (192.168.0.0/16):
					// - 192.168.0.0/24 → index 0 (Layer3)
					// - 192.168.1.0/31 → index 1 (Layer2)
					subnetAnnotation := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
						"layer2_2": {"192.168.1.0/31", "fd00:192:168:1::/127"},
					})
					cnc := createTestCNC("multi-cnc", 1000, defaultConnectSubnets(), subnetAnnotation)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Verify 3 ports on connect router (Layer3: 2 nodes = 2 ports, Layer2: 1 port)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("multi-cnc"), 3)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer3 network router has 1 port (local node only)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, l3Net.RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer2 network router has 1 port
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, l2Net.RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer3 local node port (node1, id=1) - should be local with peer
					l3ConnectPort := getConnectRouterToNetworkRouterPortName("multi-cnc", l3Net.name, "node1")
					l3NetworkPort := getNetworkRouterToConnectRouterPortName(l3Net.name, "node1", "multi-cnc")
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, l3ConnectPort, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 1), l3NetworkPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, l3NetworkPort, 0, l3ConnectPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer3 remote node port (node2, id=2) - should be remote with requested-chassis
					l3RemoteConnectPort := getConnectRouterToNetworkRouterPortName("multi-cnc", l3Net.name, "node2")
					Eventually(func() error {
						return verifyRouterPortIsRemote(nbClient, l3RemoteConnectPort, getExpectedPortTunnelKey("192.168.0.0/24", "fd00:192:168::/64", ovntypes.Layer3Topology, 2))
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify Layer2 port (single port, no node name)
					l2ConnectPort := getConnectRouterToNetworkRouterPortName("multi-cnc", l2Net.name, "")
					l2NetworkPort := getNetworkRouterToConnectRouterPortName(l2Net.name, "", "multi-cnc")
					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, l2ConnectPort, getExpectedPortTunnelKey("192.168.1.0/31", "fd00:192:168:1::/127", ovntypes.Layer2Topology, 0), l2NetworkPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyRouterPortIsLocal(nbClient, l2NetworkPort, 0, l2ConnectPort)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify static routes
					expectedPerIPFamily := 0
					if config.IPv4Mode {
						expectedPerIPFamily++
					}
					if config.IPv6Mode {
						expectedPerIPFamily++
					}

					// Layer3 routes: one per node
					for _, node := range nodes {
						Eventually(func() error {
							return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("multi-cnc"),
								"multi-cnc", l3Net.id, node.id, expectedPerIPFamily)
						}).WithTimeout(5*time.Second).Should(Succeed(), "routes for %s node %s (id=%d)", l3Net.name, node.name, node.id)
					}

					// Layer2 routes: single route (nodeID=0)
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("multi-cnc"),
							"multi-cnc", 2, 0, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify policies on Layer3 network router pointing to Layer2 network
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, l3Net.RouterName(),
							"multi-cnc", l3Net.id, l2Net.id, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify policies on Layer2 network router pointing to Layer3 network
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, l2Net.RouterName(),
							"multi-cnc", l2Net.id, l3Net.id, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})
			})

			// =============================================================================
			// Context: Multiple CNCs
			// =============================================================================
			Context("Multiple CNCs", func() {

				It("should handle multiple CNCs independently", func() {
					// Setup: 4 networks (2 per CNC), 2 nodes
					// CNC1 connects net1 and net2, CNC2 connects net3 and net4
					zoneName = "node1"
					networks := []testNetwork{
						{name: "net1", id: 1, topologyType: ovntypes.Layer3Topology, subnets: []string{"10.128.0.0/14/23", "fd00:10:128::/48/64"}},
						{name: "net2", id: 2, topologyType: ovntypes.Layer3Topology, subnets: []string{"10.132.0.0/14/23", "fd00:10:132::/48/64"}},
						{name: "net3", id: 3, topologyType: ovntypes.Layer3Topology, subnets: []string{"10.136.0.0/14/23", "fd00:10:136::/48/64"}},
						{name: "net4", id: 4, topologyType: ovntypes.Layer3Topology, subnets: []string{"10.140.0.0/14/23", "fd00:10:140::/48/64"}},
					}
					nodes := []testNode{
						{name: "node1", id: 1, zone: "node1", nodeSubnets: map[string]subnetPair{
							"net1": {"10.128.1.0/24", "fd00:10:128:1::/64"},
							"net2": {"10.132.1.0/24", "fd00:10:132:1::/64"},
							"net3": {"10.136.1.0/24", "fd00:10:136:1::/64"},
							"net4": {"10.140.1.0/24", "fd00:10:140:1::/64"},
						}},
						{name: "node2", id: 2, zone: "node2", nodeSubnets: map[string]subnetPair{
							"net1": {"10.128.2.0/24", "fd00:10:128:2::/64"},
							"net2": {"10.132.2.0/24", "fd00:10:132:2::/64"},
							"net3": {"10.136.2.0/24", "fd00:10:136:2::/64"},
							"net4": {"10.140.2.0/24", "fd00:10:140:2::/64"},
						}},
					}
					initialDB := createInitialDBWithRouters(networks)
					start(initialDB, nodes, map[string]testNetwork{
						"net1": networks[0], "net2": networks[1],
						"net3": networks[2], "net4": networks[3],
					})

					expectedPerIPFamily := 0
					if config.IPv4Mode {
						expectedPerIPFamily++
					}
					if config.IPv6Mode {
						expectedPerIPFamily++
					}

					// Create first CNC connecting net1 and net2
					subnetAnnotation1 := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_1": {"192.168.0.0/24", "fd00:192:168::/64"},
						"layer3_2": {"192.168.1.0/24", "fd00:192:168:1::/64"},
					})
					cnc1 := createTestCNC("multi-cnc1", 600, defaultConnectSubnets(), subnetAnnotation1)

					_, err := fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc1, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Create second CNC connecting net3 and net4
					subnetAnnotation2 := buildConnectSubnetAnnotation(map[string]subnetPair{
						"layer3_3": {"192.168.2.0/24", "fd00:192:168:2::/64"},
						"layer3_4": {"192.168.3.0/24", "fd00:192:168:3::/64"},
					})
					cnc2 := createTestCNC("multi-cnc2", 700, defaultConnectSubnets(), subnetAnnotation2)

					_, err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Create(
						context.Background(), cnc2, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Wait for both connect routers
					Eventually(func() error {
						return verifyConnectRouter(nbClient, "multi-cnc1", 600)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					Eventually(func() error {
						return verifyConnectRouter(nbClient, "multi-cnc2", 700)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify ports on CNC1's connect router
					// 2 networks x 2 nodes = 4 ports, but node2 is remote so only 2 local + 2 remote = 4
					// Local node (node1) ports for net1 and net2 + remote node (node2) ports for net1 and net2
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("multi-cnc1"), 4)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify ports on CNC2's connect router (same: 4 ports)
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("multi-cnc2"), 4)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify ports on network routers (only local node1 gets ports)
					// net1 and net2 connected to CNC1
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[0].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[1].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// net3 and net4 connected to CNC2
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[2].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[3].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify routes on CNC1's connect router
					// Routes for net1: node1 (local) and node2 (remote) subnets
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("multi-cnc1"),
							"multi-cnc1", 1, 1, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("multi-cnc1"),
							"multi-cnc1", 1, 2, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					// Routes for net2
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("multi-cnc1"),
							"multi-cnc1", 2, 1, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("multi-cnc1"),
							"multi-cnc1", 2, 2, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify routes on CNC2's connect router
					// Routes for net3
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("multi-cnc2"),
							"multi-cnc2", 3, 1, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("multi-cnc2"),
							"multi-cnc2", 3, 2, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					// Routes for net4
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("multi-cnc2"),
							"multi-cnc2", 4, 1, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterStaticRoutesCount(nbClient, getConnectRouterName("multi-cnc2"),
							"multi-cnc2", 4, 2, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify routing policies on network routers
					// net1's router has policy for traffic to net2 (srcOwner=layer3_1, dstNetworkID=2)
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[0].RouterName(), "multi-cnc1", networks[0].id, networks[1].id, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					// net2's router has policy for traffic to net1 (srcOwner=layer3_2, dstNetworkID=1)
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[1].RouterName(), "multi-cnc1", networks[1].id, networks[0].id, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					// net3's router has policy for traffic to net4 (srcOwner=layer3_3, dstNetworkID=4)
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[2].RouterName(), "multi-cnc2", networks[2].id, networks[3].id, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					// net4's router has policy for traffic to net3 (srcOwner=layer3_4, dstNetworkID=3)
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[3].RouterName(), "multi-cnc2", networks[3].id, networks[2].id, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Delete first CNC
					err = fakeClientset.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Delete(
						context.Background(), "multi-cnc1", metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Wait for first router to be deleted
					Eventually(func() error {
						routerName := getConnectRouterName("multi-cnc1")
						_, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: routerName})
						if err != nil {
							return nil // Deleted
						}
						return errRouterStillExists
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify CNC1's ports are removed from network routers
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[0].RouterName(), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[1].RouterName(), 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Verify CNC1's policies are removed from network routers
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[0].RouterName(), "multi-cnc1", networks[0].id, networks[1].id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[1].RouterName(), "multi-cnc1", networks[1].id, networks[0].id, 0)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// Second CNC should still be fully functional
					Expect(verifyConnectRouter(nbClient, "multi-cnc2", 700)).To(Succeed())

					// CNC2's ports should still exist
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, getConnectRouterName("multi-cnc2"), 4)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[2].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPortsCount(nbClient, networks[3].RouterName(), 1)
					}).WithTimeout(5 * time.Second).Should(Succeed())

					// CNC2's policies should still exist
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[2].RouterName(), "multi-cnc2", networks[2].id, networks[3].id, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())
					Eventually(func() error {
						return verifyRouterPolicyCount(nbClient, networks[3].RouterName(), "multi-cnc2", networks[3].id, networks[2].id, expectedPerIPFamily)
					}).WithTimeout(5 * time.Second).Should(Succeed())
				})
			})

		}) // end Context for ipMode
	}
})

// Sentinel error for router existence check
var errRouterStillExists = &routerStillExistsError{}

type routerStillExistsError struct{}

func (e *routerStillExistsError) Error() string {
	return "router still exists"
}
