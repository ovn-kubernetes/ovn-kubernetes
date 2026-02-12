package networkconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"

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
	mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
)

func chassisIDForNode(nodeName string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(nodeName)).String()
}

type testNetworkManager struct {
	networkmanager.FakeNetworkManager
	nodeHas map[string]bool
}

func (t *testNetworkManager) NodeHasNetwork(_ string, networkName string) bool {
	return t.nodeHas[networkName]
}

func TestGetConnectRouterName(t *testing.T) {
	tests := []struct {
		name     string
		cncName  string
		expected string
	}{
		{
			name:     "simple cnc name",
			cncName:  "my-cnc",
			expected: "connect_router_my-cnc",
		},
		{
			name:     "cnc name with dashes",
			cncName:  "my-complex-cnc-name",
			expected: "connect_router_my-complex-cnc-name",
		},
		{
			name:     "short cnc name",
			cncName:  "a",
			expected: "connect_router_a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getConnectRouterName(tt.cncName)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestEnsureConnectRouter(t *testing.T) {
	tests := []struct {
		name            string
		cncName         string
		tunnelID        int
		initialDB       []libovsdbtest.TestData
		expectedRouters []*nbdb.LogicalRouter
	}{
		{
			name:     "create new connect router",
			cncName:  "test-cnc",
			tunnelID: 100,
			initialDB: []libovsdbtest.TestData{
				&nbdb.Copp{
					UUID: "copp-uuid",
					Name: ovntypes.DefaultCOPPName,
				},
			},
			expectedRouters: []*nbdb.LogicalRouter{
				{
					Name: "connect_router_test-cnc",
					Options: map[string]string{
						"requested-tnl-key": "100",
					},
				},
			},
		},
		{
			name:     "update existing connect router",
			cncName:  "existing-cnc",
			tunnelID: 200,
			initialDB: []libovsdbtest.TestData{
				&nbdb.Copp{
					UUID: "copp-uuid",
					Name: ovntypes.DefaultCOPPName,
				},
				&nbdb.LogicalRouter{
					UUID: "existing-router-uuid",
					Name: "connect_router_existing-cnc",
					ExternalIDs: map[string]string{
						libovsdbops.OwnerControllerKey.String(): controllerName,
						libovsdbops.OwnerTypeKey.String():       string(libovsdbops.ClusterNetworkConnectOwnerType),
						libovsdbops.ObjectNameKey.String():      "existing-cnc",
					},
					Options: map[string]string{
						"requested-tnl-key": "100", // old tunnel ID
					},
				},
			},
			expectedRouters: []*nbdb.LogicalRouter{
				{
					Name: "connect_router_existing-cnc",
					Options: map[string]string{
						"requested-tnl-key": "200", // updated tunnel ID
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: tt.initialDB,
			}, nil)
			require.NoError(t, err)
			defer cleanup.Cleanup()

			c := &Controller{
				nbClient: nbClient,
			}

			cnc := &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{
					Name: tt.cncName,
				},
			}

			err = c.ensureConnectRouter(cnc, tt.tunnelID)
			require.NoError(t, err)

			// Get the COPP UUID for verification
			copp, err := libovsdbops.GetCOPP(nbClient, &nbdb.Copp{Name: ovntypes.DefaultCOPPName})
			require.NoError(t, err, "expected default COPP to exist")

			// Verify the router was created/updated correctly
			for _, expectedRouter := range tt.expectedRouters {
				router, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: expectedRouter.Name})
				require.NoError(t, err)
				assert.Equal(t, expectedRouter.Name, router.Name)
				assert.Equal(t, expectedRouter.Options, router.Options)
				// Check that required external IDs are present (there may be additional IDs like k8s.ovn.org/id)
				assert.Equal(t, controllerName, router.ExternalIDs[libovsdbops.OwnerControllerKey.String()])
				assert.Equal(t, string(libovsdbops.ClusterNetworkConnectOwnerType), router.ExternalIDs[libovsdbops.OwnerTypeKey.String()])
				assert.Equal(t, tt.cncName, router.ExternalIDs[libovsdbops.ObjectNameKey.String()])
				// Verify COPP is set on the router
				require.NotNil(t, router.Copp, "expected COPP to be set on router")
				assert.Equal(t, copp.UUID, *router.Copp, "COPP UUID mismatch")
			}
		})
	}
}

func TestDeleteConnectRouter(t *testing.T) {
	tests := []struct {
		name              string
		cncName           string
		initialDB         []libovsdbtest.TestData
		expectRouterGone  bool
		expectedRemaining []string // router names that should still exist
	}{
		{
			name:    "delete existing connect router",
			cncName: "test-cnc",
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-uuid",
					Name: "connect_router_test-cnc",
				},
			},
			expectRouterGone:  true,
			expectedRemaining: []string{},
		},
		{
			name:              "delete non-existent router (should not error)",
			cncName:           "non-existent",
			initialDB:         []libovsdbtest.TestData{},
			expectRouterGone:  true,
			expectedRemaining: []string{},
		},
		{
			name:    "delete one router, keep others",
			cncName: "delete-me",
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-1-uuid",
					Name: "connect_router_delete-me",
				},
				&nbdb.LogicalRouter{
					UUID: "router-2-uuid",
					Name: "connect_router_keep-me",
				},
			},
			expectRouterGone:  true,
			expectedRemaining: []string{"connect_router_keep-me"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: tt.initialDB,
			}, nil)
			require.NoError(t, err)
			defer cleanup.Cleanup()

			c := &Controller{
				nbClient: nbClient,
			}

			err = c.deleteConnectRouter(tt.cncName)
			require.NoError(t, err)

			// Verify the router was deleted
			deletedRouterName := getConnectRouterName(tt.cncName)
			_, err = libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: deletedRouterName})
			if tt.expectRouterGone {
				require.Error(t, err, "expected router %s to be deleted", deletedRouterName)
			}

			// Verify remaining routers
			for _, routerName := range tt.expectedRemaining {
				_, err = libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: routerName})
				assert.NoError(t, err, "expected router %s to still exist", routerName)
			}
		})
	}
}

func TestGetConnectRouterToNetworkRouterPortName(t *testing.T) {
	tests := []struct {
		name        string
		cncName     string
		networkName string
		nodeName    string
		expected    string
	}{
		{
			name:        "Layer2 network (no node)",
			cncName:     "my-cnc",
			networkName: "blue-network",
			nodeName:    "",
			expected:    ovntypes.ConnectRouterToRouterPrefix + "my-cnc_blue-network",
		},
		{
			name:        "Layer3 network with node",
			cncName:     "my-cnc",
			networkName: "red-network",
			nodeName:    "node-1",
			expected:    ovntypes.ConnectRouterToRouterPrefix + "my-cnc_red-network_node-1",
		},
		{
			name:        "Layer3 network with different node",
			cncName:     "test-cnc",
			networkName: "network-a",
			nodeName:    "worker-node-2",
			expected:    ovntypes.ConnectRouterToRouterPrefix + "test-cnc_network-a_worker-node-2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getConnectRouterToNetworkRouterPortName(tt.cncName, tt.networkName, tt.nodeName)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetNetworkRouterToConnectRouterPortName(t *testing.T) {
	tests := []struct {
		name        string
		networkName string
		nodeName    string
		cncName     string
		expected    string
	}{
		{
			name:        "Layer2 network (no node)",
			networkName: "blue-network",
			nodeName:    "",
			cncName:     "my-cnc",
			expected:    ovntypes.RouterToConnectRouterPrefix + "blue-network_my-cnc",
		},
		{
			name:        "Layer3 network with node",
			networkName: "red-network",
			nodeName:    "node-1",
			cncName:     "my-cnc",
			expected:    ovntypes.RouterToConnectRouterPrefix + "red-network_node-1_my-cnc",
		},
		{
			name:        "Layer3 network with different node",
			networkName: "network-a",
			nodeName:    "worker-node-2",
			cncName:     "test-cnc",
			expected:    ovntypes.RouterToConnectRouterPrefix + "network-a_worker-node-2_test-cnc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getNetworkRouterToConnectRouterPortName(tt.networkName, tt.nodeName, tt.cncName)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCreateRouterPortOps(t *testing.T) {
	tests := []struct {
		name              string
		routerName        string
		portName          string
		ipNets            []*net.IPNet
		peerPortName      string
		cncName           string
		networkID         int
		nodeID            int
		tunnelKey         int
		remoteChassisName string
		initialDB         []libovsdbtest.TestData
		expectError       bool
	}{
		{
			name:       "create local port with peer",
			routerName: "connect_router_test",
			portName:   "crtor-test_network_node1",
			ipNets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/31"),
			},
			peerPortName:      "rtocr-network_node1_test",
			cncName:           "test-cnc",
			networkID:         1,
			nodeID:            1,
			tunnelKey:         100,
			remoteChassisName: "",
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-uuid",
					Name: "connect_router_test",
				},
			},
			expectError: false,
		},
		{
			name:       "create remote port with requested-chassis",
			routerName: "connect_router_test",
			portName:   "crtor-test_network_node2",
			ipNets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.2/31"),
			},
			peerPortName:      "",
			cncName:           "test-cnc",
			networkID:         1,
			nodeID:            2,
			tunnelKey:         101,
			remoteChassisName: chassisIDForNode("node2"),
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-uuid",
					Name: "connect_router_test",
				},
			},
			expectError: false,
		},
		{
			name:       "create dual-stack port",
			routerName: "connect_router_test",
			portName:   "crtor-test_network_node1",
			ipNets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/31"),
				ovntest.MustParseIPNet("fd00::0/127"),
			},
			peerPortName:      "rtocr-network_node1_test",
			cncName:           "test-cnc",
			networkID:         1,
			nodeID:            1,
			tunnelKey:         100,
			remoteChassisName: "",
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-uuid",
					Name: "connect_router_test",
				},
			},
			expectError: false,
		},
		{
			name:       "create port on network router (rtocr)",
			routerName: "test-network_ovn_cluster_router",
			portName:   "rtocr-test_network_node1_test-cnc",
			ipNets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.1/31"),
			},
			peerPortName:      "crtor-test_cnc_test_network_node1",
			cncName:           "test-cnc",
			networkID:         1,
			nodeID:            1,
			tunnelKey:         0, // network router ports don't have tunnel keys
			remoteChassisName: "",
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "network-router-uuid",
					Name: "test-network_ovn_cluster_router",
				},
			},
			expectError: false,
		},
		{
			name:              "error when no IPNets provided",
			routerName:        "connect_router_test",
			portName:          "test-port",
			ipNets:            []*net.IPNet{},
			peerPortName:      "",
			cncName:           "test-cnc",
			networkID:         1,
			nodeID:            1,
			tunnelKey:         0,
			remoteChassisName: "",
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-uuid",
					Name: "connect_router_test",
				},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: tt.initialDB,
			}, nil)
			require.NoError(t, err)
			defer cleanup.Cleanup()

			c := &Controller{
				nbClient: nbClient,
			}

			ops, err := c.createRouterPortOps(nil, tt.routerName, tt.portName, tt.ipNets, tt.peerPortName, tt.cncName, tt.networkID, tt.nodeID, tt.tunnelKey, tt.remoteChassisName)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotEmpty(t, ops)

			// Execute ops
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			require.NoError(t, err)

			// Verify the port was created with correct fields
			router, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: tt.routerName})
			require.NoError(t, err)
			require.NotEmpty(t, router.Ports, "expected router to have ports")

			// Get the port and verify its fields
			port, err := libovsdbops.GetLogicalRouterPort(nbClient, &nbdb.LogicalRouterPort{Name: tt.portName})
			require.NoError(t, err)

			// Verify port name
			assert.Equal(t, tt.portName, port.Name)

			// Verify ExternalIDs
			assert.Equal(t, tt.cncName, port.ExternalIDs[libovsdbops.ObjectNameKey.String()], "ObjectNameKey mismatch")
			assert.Equal(t, strconv.Itoa(tt.networkID), port.ExternalIDs[libovsdbops.NetworkIDKey.String()], "NetworkIDKey mismatch")
			assert.Equal(t, strconv.Itoa(tt.nodeID), port.ExternalIDs[libovsdbops.NodeIDKey.String()], "NodeIDKey mismatch")
			assert.Equal(t, tt.routerName, port.ExternalIDs[libovsdbops.RouterNameKey.String()], "RouterNameKey mismatch")

			// Verify Networks (IP addresses)
			expectedNetworks := make([]string, len(tt.ipNets))
			for i, ipNet := range tt.ipNets {
				expectedNetworks[i] = ipNet.String()
			}
			assert.ElementsMatch(t, expectedNetworks, port.Networks, "Networks mismatch")

			// Verify peer port name
			if tt.peerPortName != "" {
				require.NotNil(t, port.Peer)
				assert.Equal(t, tt.peerPortName, *port.Peer, "Peer port name mismatch")
			} else {
				assert.Nil(t, port.Peer, "Expected no peer port")
			}

			// Verify Options (tunnel key, requested-chassis)
			if tt.tunnelKey != 0 {
				assert.Equal(t, strconv.Itoa(tt.tunnelKey), port.Options[libovsdbops.RequestedTnlKey], "Tunnel key mismatch")
			}
			if tt.remoteChassisName != "" {
				assert.Equal(t, tt.remoteChassisName, port.Options[libovsdbops.RequestedChassis], "Requested chassis mismatch")
			}
		})
	}
}

func TestEnsureConnectPortsOps(t *testing.T) {
	tests := []struct {
		name                 string
		cncName              string
		zone                 string // controller's zone (local node name)
		connectSubnets       []networkconnectv1.ConnectSubnet
		networkName          string
		networkID            int
		topologyType         string
		subnets              []*net.IPNet
		nodes                []*corev1.Node
		initialDB            []libovsdbtest.TestData
		expectError          bool
		expectedConnectPorts []string // port names on connect router
		expectedNetworkPorts []string // port names on network router
	}{
		{
			name:    "Layer3 with local node - creates both ports with peer",
			cncName: "test-cnc",
			zone:    "node1", // local node
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
			},
			networkName:  "test-network",
			networkID:    1,
			topologyType: ovntypes.Layer3Topology,
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
			},
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
						Annotations: map[string]string{
							"k8s.ovn.org/node-id": "1",
							util.OvnNodeChassisID: chassisIDForNode("node1"),
							util.OvnNodeZoneName:  "node1", // local zone
						},
					},
				},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "connect-router-uuid", Name: "connect_router_test-cnc"},
				&nbdb.LogicalRouter{UUID: "network-router-uuid", Name: "cluster-router_test-network"},
			},
			expectError:          false,
			expectedConnectPorts: []string{ovntypes.ConnectRouterToRouterPrefix + "test-cnc_test-network_node1"},
			expectedNetworkPorts: []string{ovntypes.RouterToConnectRouterPrefix + "test-network_node1_test-cnc"},
		},
		{
			name:    "Layer3 with remote node - creates only connect router port with requested-chassis",
			cncName: "test-cnc",
			zone:    "node1", // local node is node1, but we're creating for node2
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
			},
			networkName:  "test-network",
			networkID:    1,
			topologyType: ovntypes.Layer3Topology,
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
			},
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node2", // remote node
						Annotations: map[string]string{
							"k8s.ovn.org/node-id": "2",
							util.OvnNodeChassisID: chassisIDForNode("node2"),
							util.OvnNodeZoneName:  "node2", // different zone
						},
					},
				},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "connect-router-uuid", Name: "connect_router_test-cnc"},
				&nbdb.LogicalRouter{UUID: "network-router-uuid", Name: "cluster-router_test-network"},
			},
			expectError:          false,
			expectedConnectPorts: []string{ovntypes.ConnectRouterToRouterPrefix + "test-cnc_test-network_node2"},
			expectedNetworkPorts: []string{}, // no network router port for remote nodes
		},
		{
			name:    "Layer3 with 2 nodes (1 local + 1 remote) - creates correct ports for each",
			cncName: "test-cnc",
			zone:    "node1", // node1 is local, node2 is remote
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
			},
			networkName:  "test-network",
			networkID:    1,
			topologyType: ovntypes.Layer3Topology,
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
			},
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1", // local node
						Annotations: map[string]string{
							"k8s.ovn.org/node-id": "1",
							util.OvnNodeChassisID: chassisIDForNode("node1"),
							util.OvnNodeZoneName:  "node1", // local zone
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node2", // remote node
						Annotations: map[string]string{
							"k8s.ovn.org/node-id": "2",
							util.OvnNodeChassisID: chassisIDForNode("node2"),
							util.OvnNodeZoneName:  "node2", // different zone
						},
					},
				},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "connect-router-uuid", Name: "connect_router_test-cnc"},
				&nbdb.LogicalRouter{UUID: "network-router-uuid", Name: "cluster-router_test-network"},
			},
			expectError: false,
			expectedConnectPorts: []string{
				ovntypes.ConnectRouterToRouterPrefix + "test-cnc_test-network_node1",
				ovntypes.ConnectRouterToRouterPrefix + "test-cnc_test-network_node2",
			},
			expectedNetworkPorts: []string{
				ovntypes.RouterToConnectRouterPrefix + "test-network_node1_test-cnc", // only local node
			},
		},
		{
			name:    "Layer2 - creates port pair on connect and transit router",
			cncName: "test-cnc",
			zone:    "node1",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
			},
			networkName:  "test-l2-network",
			networkID:    2,
			topologyType: ovntypes.Layer2Topology,
			subnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/31"), // /31 for L2
			},
			nodes: []*corev1.Node{}, // Layer2 doesn't need nodes
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "connect-router-uuid", Name: "connect_router_test-cnc"},
				&nbdb.LogicalRouter{UUID: "transit-router-uuid", Name: "transit_test-l2-network"},
			},
			expectError:          false,
			expectedConnectPorts: []string{ovntypes.ConnectRouterToRouterPrefix + "test-cnc_test-l2-network"},
			expectedNetworkPorts: []string{ovntypes.RouterToConnectRouterPrefix + "test-l2-network_test-cnc"},
		},
		{
			name:    "error when no subnets provided",
			cncName: "test-cnc",
			zone:    "node1",
			connectSubnets: []networkconnectv1.ConnectSubnet{
				{CIDR: "192.168.0.0/16", NetworkPrefix: 24},
			},
			networkName:  "test-network",
			networkID:    1,
			topologyType: ovntypes.Layer3Topology,
			subnets:      []*net.IPNet{}, // empty
			nodes:        []*corev1.Node{},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "connect-router-uuid", Name: "connect_router_test-cnc"},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: tt.initialDB,
			}, nil)
			require.NoError(t, err)
			defer cleanup.Cleanup()

			c := &Controller{
				nbClient: nbClient,
				zone:     tt.zone,
			}

			cnc := &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: tt.cncName},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{
					ConnectSubnets: tt.connectSubnets,
				},
			}

			// Create a mock NetInfo
			netInfo := &mocks.NetInfo{}
			netInfo.On("GetNetworkName").Return(tt.networkName)
			netInfo.On("GetNetworkID").Return(tt.networkID)
			netInfo.On("TopologyType").Return(tt.topologyType)
			if tt.topologyType == ovntypes.Layer2Topology {
				netInfo.On("GetNetworkScopedClusterRouterName").Return("transit_" + tt.networkName)
			} else {
				netInfo.On("GetNetworkScopedClusterRouterName").Return("cluster-router_" + tt.networkName)
			}

			ops, err := c.ensureConnectPortsOps(nil, cnc, netInfo, tt.subnets, tt.nodes, true)

			if tt.expectError {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)

			// Execute ops
			if len(ops) > 0 {
				_, err = libovsdbops.TransactAndCheck(nbClient, ops)
				require.NoError(t, err)
			}

			// Verify connect router ports
			connectRouterName := getConnectRouterName(tt.cncName)
			connectRouter, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: connectRouterName})
			require.NoError(t, err)

			for _, expectedPortName := range tt.expectedConnectPorts {
				port, err := libovsdbops.GetLogicalRouterPort(nbClient, &nbdb.LogicalRouterPort{Name: expectedPortName})
				require.NoError(t, err, "expected connect router port %s to exist", expectedPortName)
				assert.Equal(t, tt.cncName, port.ExternalIDs[libovsdbops.ObjectNameKey.String()])
			}
			assert.Len(t, connectRouter.Ports, len(tt.expectedConnectPorts), "connect router port count mismatch")

			// Verify network router ports
			for _, expectedPortName := range tt.expectedNetworkPorts {
				port, err := libovsdbops.GetLogicalRouterPort(nbClient, &nbdb.LogicalRouterPort{Name: expectedPortName})
				require.NoError(t, err, "expected network router port %s to exist", expectedPortName)
				assert.Equal(t, tt.cncName, port.ExternalIDs[libovsdbops.ObjectNameKey.String()])
			}
		})
	}
}

func TestCleanupNetworkConnections(t *testing.T) {
	tests := []struct {
		name                     string
		cncName                  string
		initialDB                []libovsdbtest.TestData
		expectError              bool
		expectedConnectPortCount int    // connect router ports should remain (not deleted)
		expectedNetworkPortCount int    // network router ports should be deleted
		networkRouterName        string // name of network router to check (if any)
	}{
		{
			name:    "cleanup single network connection",
			cncName: "test-cnc",
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID:  "connect-router-uuid",
					Name:  "connect_router_test-cnc",
					Ports: []string{"port-1-uuid"},
				},
				&nbdb.LogicalRouterPort{
					UUID: "port-1-uuid",
					Name: "crtor-test-cnc-net1-node1",
					ExternalIDs: map[string]string{
						libovsdbops.ObjectNameKey.String(): "test-cnc",
						libovsdbops.NetworkIDKey.String():  "1",
						libovsdbops.RouterNameKey.String(): "connect_router_test-cnc",
					},
				},
				&nbdb.LogicalRouter{
					UUID:  "network-router-uuid",
					Name:  "network-router-1",
					Ports: []string{"port-2-uuid"},
				},
				&nbdb.LogicalRouterPort{
					UUID: "port-2-uuid",
					Name: "rtocr-net1-node1-test-cnc",
					ExternalIDs: map[string]string{
						libovsdbops.ObjectNameKey.String(): "test-cnc",
						libovsdbops.NetworkIDKey.String():  "1",
						libovsdbops.RouterNameKey.String(): "network-router-1",
					},
				},
			},
			expectError:              false,
			expectedConnectPortCount: 1, // connect router ports are NOT deleted
			expectedNetworkPortCount: 0, // network router ports ARE deleted
			networkRouterName:        "network-router-1",
		},
		{
			name:    "cleanup remote port (no peer on network router)",
			cncName: "test-cnc",
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID:  "connect-router-uuid",
					Name:  "connect_router_test-cnc",
					Ports: []string{"port-1-uuid", "port-2-uuid"},
				},
				&nbdb.LogicalRouterPort{
					UUID: "port-1-uuid",
					Name: "crtor-test-cnc-net1-node1",
					ExternalIDs: map[string]string{
						libovsdbops.ObjectNameKey.String(): "test-cnc",
						libovsdbops.NetworkIDKey.String():  "1",
						libovsdbops.NodeIDKey.String():     "1",
						libovsdbops.RouterNameKey.String(): "connect_router_test-cnc",
					},
					// Local port has peer set
					Peer: func() *string { s := "rtocr-net1-node1-test-cnc"; return &s }(),
				},
				&nbdb.LogicalRouterPort{
					UUID: "port-2-uuid",
					Name: "crtor-test-cnc-net1-node2",
					ExternalIDs: map[string]string{
						libovsdbops.ObjectNameKey.String(): "test-cnc",
						libovsdbops.NetworkIDKey.String():  "1",
						libovsdbops.NodeIDKey.String():     "2",
						libovsdbops.RouterNameKey.String(): "connect_router_test-cnc",
					},
					Options: map[string]string{
						libovsdbops.RequestedChassis: chassisIDForNode("node2"),
					},
					// Remote port has no peer
				},
			},
			expectError:              false,
			expectedConnectPortCount: 2, // both local and remote connect router ports are NOT deleted
			expectedNetworkPortCount: 0, // no network router ports in this test
		},
		{
			name:    "cleanup with no connected networks",
			cncName: "empty-cnc",
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "connect-router-uuid",
					Name: "connect_router_empty-cnc",
				},
			},
			expectError:              false,
			expectedConnectPortCount: 0, // no ports to begin with
			expectedNetworkPortCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: tt.initialDB,
			}, nil)
			require.NoError(t, err)
			defer cleanup.Cleanup()

			c := &Controller{
				nbClient: nbClient,
			}

			err = c.cleanupNetworkConnections(tt.cncName)
			if tt.expectError {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)

			// Verify connect router ports are NOT deleted (they remain because the router will be deleted separately)
			connectRouterName := getConnectRouterName(tt.cncName)
			connectRouter, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: connectRouterName})
			if err == nil {
				assert.Len(t, connectRouter.Ports, tt.expectedConnectPortCount, "connect router ports should remain")
			}

			// Verify network router ports ARE deleted
			if tt.networkRouterName != "" {
				networkRouter, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: tt.networkRouterName})
				if err == nil {
					assert.Len(t, networkRouter.Ports, tt.expectedNetworkPortCount, "network router ports should be deleted")
				}
			}
		})
	}
}

func TestSyncNetworkConnectionsInactiveNetwork(t *testing.T) {
	g := gomega.NewWithT(t)

	err := config.PrepareTestConfig()
	g.Expect(err).ToNot(gomega.HaveOccurred())

	fakeClientset := util.GetOVNClientset().GetOVNKubeControllerClientset()

	nodeSubnets := map[string]string{
		"netA": "10.0.0.0/24",
		"netB": "10.1.0.0/24",
	}
	subnetsBytes, err := json.Marshal(nodeSubnets)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Seed a local node with per-network subnets and required annotations.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{
				util.OvnNodeZoneName:       "zone1",
				util.OvnNodeID:             "1",
				util.OvnNodeChassisID:      chassisIDForNode("node1"),
				"k8s.ovn.org/node-subnets": string(subnetsBytes),
			},
		},
	}
	_, err = fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// Start node informer so getNodeSubnet can resolve node annotations.
	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClientset)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer syncCancel()
	synced := cache.WaitForCacheSync(syncCtx.Done(), wf.NodeCoreInformer().Informer().HasSynced)
	g.Expect(synced).To(gomega.BeTrue(), "informer caches should sync")

	cncName := "cnc1"
	// Mock netinfos for two connected networks.
	networkA := &mocks.NetInfo{}
	networkA.On("GetNetworkName").Return("netA")
	networkA.On("GetNetworkID").Return(1)
	networkA.On("TopologyType").Return(ovntypes.Layer3Topology)
	networkA.On("GetNetworkScopedClusterRouterName").Return("cluster-router_netA")
	networkA.On("GetNetworkScopedRouterToSwitchPortName", "node1").Return("rtos-netA-node1")
	networkA.On("Subnets").Return([]config.CIDRNetworkEntry{
		{CIDR: ovntest.MustParseIPNet("10.0.0.0/16")},
	})

	networkB := &mocks.NetInfo{}
	networkB.On("GetNetworkName").Return("netB")
	networkB.On("GetNetworkID").Return(2)
	networkB.On("TopologyType").Return(ovntypes.Layer3Topology)
	networkB.On("GetNetworkScopedClusterRouterName").Return("cluster-router_netB")
	networkB.On("GetNetworkScopedRouterToSwitchPortName", "node1").Return("rtos-netB-node1")
	networkB.On("Subnets").Return([]config.CIDRNetworkEntry{
		{CIDR: ovntest.MustParseIPNet("10.1.0.0/16")},
	})

	// netA is active locally, netB is inactive initially.
	networks := map[string]util.NetInfo{
		"netA": networkA,
		"netB": networkB,
	}
	nm := &testNetworkManager{
		FakeNetworkManager: networkmanager.FakeNetworkManager{
			PrimaryNetworks: networks,
		},
		nodeHas: map[string]bool{
			"netA": true,
			"netB": false,
		},
	}

	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
		NBData: []libovsdbtest.TestData{
			&nbdb.LogicalRouter{Name: getConnectRouterName(cncName)},
			&nbdb.LogicalRouter{Name: "cluster-router_netA"},
			&nbdb.LogicalRouter{Name: "cluster-router_netB"},
		},
	}, nil)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer cleanup.Cleanup()

	// Controller with connect router and both network routers.
	c := &Controller{
		nbClient:       nbClient,
		zone:           "zone1",
		nodeLister:     wf.NodeCoreInformer().Lister(),
		networkManager: nm,
		cncCache: map[string]*networkConnectState{
			cncName: {
				name:              cncName,
				connectedNetworks: sets.New[string](),
			},
		},
	}

	cnc := &networkconnectv1.ClusterNetworkConnect{
		ObjectMeta: metav1.ObjectMeta{Name: cncName},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{
			ConnectSubnets: []networkconnectv1.ConnectSubnet{
				{
					CIDR:          "192.168.0.0/16",
					NetworkPrefix: 24,
				},
			},
		},
	}
	allocatedSubnets := map[string][]*net.IPNet{
		util.ComputeNetworkOwner(ovntypes.Layer3Topology, 1): {ovntest.MustParseIPNet("192.168.0.0/24")},
		util.ComputeNetworkOwner(ovntypes.Layer3Topology, 2): {ovntest.MustParseIPNet("192.168.1.0/24")},
	}

	// First sync: netB inactive, so only remote/static programming is expected.
	err = c.syncNetworkConnections(cnc, allocatedSubnets)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	policies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(nbClient, func(item *nbdb.LogicalRouterPolicy) bool {
		return item.ExternalIDs[libovsdbops.SourceNetworkIDKey.String()] == "1" &&
			item.ExternalIDs[libovsdbops.DestinationNetworkIDKey.String()] == "2"
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(policies).ToNot(gomega.BeEmpty())

	// Static routes for netB should be programmed on the connect router.
	routes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(nbClient, func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == cncName &&
			item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == "2"
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(routes).ToNot(gomega.BeEmpty())
	portPairInfo, err := GetP2PAddresses(allocatedSubnets[util.ComputeNetworkOwner(ovntypes.Layer3Topology, 2)], 1)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	expectedNexthops := util.IPNetsToIPs(portPairInfo.networkPortIPs)
	g.Expect(expectedNexthops).ToNot(gomega.BeEmpty())

	foundRoute := false
	for _, route := range routes {
		if route.IPPrefix == "10.1.0.0/24" {
			g.Expect(route.Nexthop).To(gomega.Equal(expectedNexthops[0].String()))
			foundRoute = true
			break
		}
	}
	g.Expect(foundRoute).To(gomega.BeTrue())

	// No local router port should be created for inactive netB.
	ports, err := libovsdbops.FindLogicalRouterPortWithPredicate(nbClient, func(item *nbdb.LogicalRouterPort) bool {
		return item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == "2" &&
			item.ExternalIDs[libovsdbops.RouterNameKey.String()] == "cluster-router_netB"
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(ports).To(gomega.BeEmpty())

	// Activate netB and re-sync: local router ports and policies should now be created.
	nm.nodeHas["netB"] = true
	err = c.syncNetworkConnections(cnc, allocatedSubnets)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	ports, err = libovsdbops.FindLogicalRouterPortWithPredicate(nbClient, func(item *nbdb.LogicalRouterPort) bool {
		return item.ExternalIDs[libovsdbops.NetworkIDKey.String()] == "2" &&
			item.ExternalIDs[libovsdbops.RouterNameKey.String()] == "cluster-router_netB"
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(ports).ToNot(gomega.BeEmpty())

	policies, err = libovsdbops.FindLogicalRouterPoliciesWithPredicate(nbClient, func(item *nbdb.LogicalRouterPolicy) bool {
		return item.ExternalIDs[libovsdbops.SourceNetworkIDKey.String()] == "2" &&
			item.ExternalIDs[libovsdbops.DestinationNetworkIDKey.String()] == "1"
	})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(policies).ToNot(gomega.BeEmpty())
}

func TestCreateRoutingPoliciesOps(t *testing.T) {
	type expectedPolicy struct {
		match    string
		nexthop  string
		ipFamily string // "v4" or "v6"
	}

	tests := []struct {
		name             string
		dstNetworkID     int
		routerName       string
		inportName       string
		nexthops         []net.IP
		cncName          string
		srcNetworkID     int
		dstSubnets       []string // CIDR strings
		initialDB        []libovsdbtest.TestData
		expectError      bool
		expectedPolicies []expectedPolicy
	}{
		{
			name:         "create single IPv4 routing policy",
			dstNetworkID: 2,
			routerName:   "network-router-1",
			inportName:   "switch-to-router-port",
			nexthops: []net.IP{
				net.ParseIP("192.168.0.0"),
			},
			cncName:      "test-cnc",
			srcNetworkID: 1,
			dstSubnets:   []string{"10.200.0.0/24"},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-uuid",
					Name: "network-router-1",
				},
			},
			expectError: false,
			expectedPolicies: []expectedPolicy{
				{
					match:    `inport == "switch-to-router-port" && ip4.dst == 10.200.0.0/24`,
					nexthop:  "192.168.0.0",
					ipFamily: "v4",
				},
			},
		},
		{
			name:         "create dual-stack routing policies",
			dstNetworkID: 2,
			routerName:   "network-router-1",
			inportName:   "switch-to-router-port",
			nexthops: []net.IP{
				net.ParseIP("192.168.0.0"),
				net.ParseIP("fd00::"),
			},
			cncName:      "test-cnc",
			srcNetworkID: 1,
			dstSubnets:   []string{"10.200.0.0/24", "fd00:10:200::/64"},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-uuid",
					Name: "network-router-1",
				},
			},
			expectError: false,
			expectedPolicies: []expectedPolicy{
				{
					match:    `inport == "switch-to-router-port" && ip4.dst == 10.200.0.0/24`,
					nexthop:  "192.168.0.0",
					ipFamily: "v4",
				},
				{
					match:    `inport == "switch-to-router-port" && ip6.dst == fd00:10:200::/64`,
					nexthop:  "fd00::",
					ipFamily: "v6",
				},
			},
		},
		{
			name:         "skip when no matching nexthop for IP family",
			dstNetworkID: 2,
			routerName:   "network-router-1",
			inportName:   "switch-to-router-port",
			nexthops: []net.IP{
				net.ParseIP("fd00::"), // IPv6 only
			},
			cncName:      "test-cnc",
			srcNetworkID: 1,
			dstSubnets:   []string{"10.200.0.0/24"}, // IPv4 destination
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-uuid",
					Name: "network-router-1",
				},
			},
			expectError:      false,
			expectedPolicies: nil, // no policies created due to family mismatch
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: tt.initialDB,
			}, nil)
			require.NoError(t, err)
			defer cleanup.Cleanup()

			c := &Controller{
				nbClient: nbClient,
			}

			// Convert string subnets to config.CIDRNetworkEntry
			var dstSubnets []config.CIDRNetworkEntry
			for _, s := range tt.dstSubnets {
				dstSubnets = append(dstSubnets, config.CIDRNetworkEntry{CIDR: ovntest.MustParseIPNet(s)})
			}

			ops, err := c.createRoutingPoliciesOps(nil, tt.dstNetworkID, tt.routerName, tt.inportName, dstSubnets,
				tt.srcNetworkID, tt.nexthops, tt.cncName)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)

			if len(tt.expectedPolicies) == 0 {
				assert.Empty(t, ops)
				return
			}

			require.NotEmpty(t, ops)

			// Execute ops
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			require.NoError(t, err)

			// Verify the policies were created
			router, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: tt.routerName})
			require.NoError(t, err)
			assert.Len(t, router.Policies, len(tt.expectedPolicies))

			// Fetch all policies attached to the router and verify their fields
			policies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(nbClient, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == tt.cncName
			})
			require.NoError(t, err)
			assert.Len(t, policies, len(tt.expectedPolicies))

			for _, expected := range tt.expectedPolicies {
				// Find the policy with matching IP family
				var found *nbdb.LogicalRouterPolicy
				for _, p := range policies {
					if p.ExternalIDs[libovsdbops.IPFamilyKey.String()] == expected.ipFamily {
						found = p
						break
					}
				}
				require.NotNilf(t, found, "expected policy with IP family %s not found", expected.ipFamily)

				// Verify policy fields
				assert.Equal(t, ovntypes.NetworkConnectPolicyPriority, found.Priority, "Priority mismatch for IP family %s", expected.ipFamily)
				assert.Equal(t, expected.match, found.Match, "Match mismatch for IP family %s", expected.ipFamily)
				assert.Equal(t, nbdb.LogicalRouterPolicyActionReroute, found.Action, "Action mismatch for IP family %s", expected.ipFamily)
				assert.Equal(t, []string{expected.nexthop}, found.Nexthops, "Nexthops mismatch for IP family %s", expected.ipFamily)

				// Verify ExternalIDs
				assert.Equal(t, strconv.Itoa(tt.dstNetworkID), found.ExternalIDs[libovsdbops.DestinationNetworkIDKey.String()], "DestinationNetworkIDKey mismatch")
				assert.Equal(t, strconv.Itoa(tt.srcNetworkID), found.ExternalIDs[libovsdbops.SourceNetworkIDKey.String()], "SourceNetworkIDKey mismatch")
				assert.Equal(t, tt.cncName, found.ExternalIDs[libovsdbops.ObjectNameKey.String()], "ObjectNameKey mismatch")
				assert.Equal(t, expected.ipFamily, found.ExternalIDs[libovsdbops.IPFamilyKey.String()], "IPFamilyKey mismatch")
			}
		})
	}
}

func TestEnsureRoutingPoliciesOps(t *testing.T) {
	tests := []struct {
		name             string
		cncName          string
		zone             string
		srcNetworkName   string
		srcNetworkID     int
		srcTopologyType  string
		srcSubnets       []string // CIDR strings for source network
		allocatedSubnets map[string][]*net.IPNet
		dstNetworks      []struct { // destination networks
			name         string
			id           int
			topologyType string
			subnets      []string
		}
		initialDB        []libovsdbtest.TestData
		expectError      bool
		expectedPolicies int
	}{
		{
			name:            "Layer3 source with single destination network",
			cncName:         "test-cnc",
			zone:            "node1",
			srcNetworkName:  "src-network",
			srcNetworkID:    1,
			srcTopologyType: ovntypes.Layer3Topology,
			srcSubnets:      []string{"10.128.0.0/24"},
			allocatedSubnets: map[string][]*net.IPNet{
				"layer3_1": {ovntest.MustParseIPNet("192.168.0.0/24")}, // source network's connect subnet (large enough for P2P)
				"layer3_2": {ovntest.MustParseIPNet("192.168.1.0/24")}, // destination network's connect subnet
			},
			dstNetworks: []struct {
				name         string
				id           int
				topologyType string
				subnets      []string
			}{
				{name: "dst-network", id: 2, topologyType: ovntypes.Layer3Topology, subnets: []string{"10.129.0.0/24"}},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "network-router-uuid", Name: "cluster-router_src-network"},
			},
			expectError:      false,
			expectedPolicies: 1, // one policy for dst-network's subnet
		},
		{
			name:            "Layer3 source with multiple destination networks",
			cncName:         "test-cnc",
			zone:            "node1",
			srcNetworkName:  "src-network",
			srcNetworkID:    1,
			srcTopologyType: ovntypes.Layer3Topology,
			srcSubnets:      []string{"10.128.0.0/24"},
			allocatedSubnets: map[string][]*net.IPNet{
				"layer3_1": {ovntest.MustParseIPNet("192.168.0.0/24")},
				"layer3_2": {ovntest.MustParseIPNet("192.168.1.0/24")},
				"layer3_3": {ovntest.MustParseIPNet("192.168.2.0/24")},
			},
			dstNetworks: []struct {
				name         string
				id           int
				topologyType string
				subnets      []string
			}{
				{name: "dst-network-1", id: 2, topologyType: ovntypes.Layer3Topology, subnets: []string{"10.129.0.0/24"}},
				{name: "dst-network-2", id: 3, topologyType: ovntypes.Layer3Topology, subnets: []string{"10.130.0.0/24"}},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "network-router-uuid", Name: "cluster-router_src-network"},
			},
			expectError:      false,
			expectedPolicies: 2, // one policy for each dst network
		},
		{
			name:            "Layer2 source with destination network",
			cncName:         "test-cnc",
			zone:            "node1",
			srcNetworkName:  "src-l2-network",
			srcNetworkID:    1,
			srcTopologyType: ovntypes.Layer2Topology,
			srcSubnets:      []string{"10.128.0.0/24"},
			allocatedSubnets: map[string][]*net.IPNet{
				"layer2_1": {ovntest.MustParseIPNet("192.168.0.0/31")}, // Layer2 doesn't use P2P per node
				"layer3_2": {ovntest.MustParseIPNet("192.168.1.0/24")},
			},
			dstNetworks: []struct {
				name         string
				id           int
				topologyType string
				subnets      []string
			}{
				{name: "dst-network", id: 2, topologyType: ovntypes.Layer3Topology, subnets: []string{"10.129.0.0/24"}},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "network-router-uuid", Name: "transit_src-l2-network"},
			},
			expectError:      false,
			expectedPolicies: 1,
		},
		{
			name:            "Layer2 only - both source and destination are Layer2",
			cncName:         "test-cnc",
			zone:            "node1",
			srcNetworkName:  "src-l2-network",
			srcNetworkID:    1,
			srcTopologyType: ovntypes.Layer2Topology,
			srcSubnets:      []string{"10.128.0.0/24"},
			allocatedSubnets: map[string][]*net.IPNet{
				"layer2_1": {ovntest.MustParseIPNet("192.168.0.0/31")},
				"layer2_2": {ovntest.MustParseIPNet("192.168.0.2/31")},
			},
			dstNetworks: []struct {
				name         string
				id           int
				topologyType string
				subnets      []string
			}{
				{name: "dst-l2-network", id: 2, topologyType: ovntypes.Layer2Topology, subnets: []string{"10.129.0.0/24"}},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "network-router-uuid", Name: "transit_src-l2-network"},
			},
			expectError:      false,
			expectedPolicies: 1,
		},
		{
			name:            "Mixed Layer2 and Layer3 networks",
			cncName:         "test-cnc",
			zone:            "node1",
			srcNetworkName:  "src-l3-network",
			srcNetworkID:    1,
			srcTopologyType: ovntypes.Layer3Topology,
			srcSubnets:      []string{"10.128.0.0/24"},
			allocatedSubnets: map[string][]*net.IPNet{
				"layer3_1": {ovntest.MustParseIPNet("192.168.0.0/24")},
				"layer2_2": {ovntest.MustParseIPNet("192.168.1.0/31")},
				"layer3_3": {ovntest.MustParseIPNet("192.168.2.0/24")},
			},
			dstNetworks: []struct {
				name         string
				id           int
				topologyType string
				subnets      []string
			}{
				{name: "dst-l2-network", id: 2, topologyType: ovntypes.Layer2Topology, subnets: []string{"10.129.0.0/24"}},
				{name: "dst-l3-network", id: 3, topologyType: ovntypes.Layer3Topology, subnets: []string{"10.130.0.0/24"}},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "network-router-uuid", Name: "cluster-router_src-l3-network"},
			},
			expectError:      false,
			expectedPolicies: 2, // one for each dst network
		},
		{
			name:            "IPv6 single-stack allocated subnets",
			cncName:         "test-cnc",
			zone:            "node1",
			srcNetworkName:  "src-network",
			srcNetworkID:    1,
			srcTopologyType: ovntypes.Layer3Topology,
			srcSubnets:      []string{"fd00:10:128::/64"},
			allocatedSubnets: map[string][]*net.IPNet{
				"layer3_1": {ovntest.MustParseIPNet("fd00:192:168::/120")}, // IPv6 connect subnet
				"layer3_2": {ovntest.MustParseIPNet("fd00:192:169::/120")},
			},
			dstNetworks: []struct {
				name         string
				id           int
				topologyType string
				subnets      []string
			}{
				{name: "dst-network", id: 2, topologyType: ovntypes.Layer3Topology, subnets: []string{"fd00:10:129::/64"}},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "network-router-uuid", Name: "cluster-router_src-network"},
			},
			expectError:      false,
			expectedPolicies: 1,
		},
		{
			name:            "Dual-stack allocated subnets",
			cncName:         "test-cnc",
			zone:            "node1",
			srcNetworkName:  "src-network",
			srcNetworkID:    1,
			srcTopologyType: ovntypes.Layer3Topology,
			srcSubnets:      []string{"10.128.0.0/24", "fd00:10:128::/64"},
			allocatedSubnets: map[string][]*net.IPNet{
				"layer3_1": {ovntest.MustParseIPNet("192.168.0.0/24"), ovntest.MustParseIPNet("fd00:192:168::/120")},
				"layer3_2": {ovntest.MustParseIPNet("192.168.1.0/24"), ovntest.MustParseIPNet("fd00:192:169::/120")},
			},
			dstNetworks: []struct {
				name         string
				id           int
				topologyType string
				subnets      []string
			}{
				{name: "dst-network", id: 2, topologyType: ovntypes.Layer3Topology, subnets: []string{"10.129.0.0/24", "fd00:10:129::/64"}},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "network-router-uuid", Name: "cluster-router_src-network"},
			},
			expectError:      false,
			expectedPolicies: 2, // one for IPv4, one for IPv6
		},
		{
			name:            "Dual-stack with mixed Layer2 and Layer3",
			cncName:         "test-cnc",
			zone:            "node1",
			srcNetworkName:  "src-l3-network",
			srcNetworkID:    1,
			srcTopologyType: ovntypes.Layer3Topology,
			srcSubnets:      []string{"10.128.0.0/24", "fd00:10:128::/64"},
			allocatedSubnets: map[string][]*net.IPNet{
				"layer3_1": {ovntest.MustParseIPNet("192.168.0.0/24"), ovntest.MustParseIPNet("fd00:192:168::/120")},
				"layer2_2": {ovntest.MustParseIPNet("192.168.1.0/31"), ovntest.MustParseIPNet("fd00:192:169::/127")},
				"layer3_3": {ovntest.MustParseIPNet("192.168.2.0/24"), ovntest.MustParseIPNet("fd00:192:170::/120")},
			},
			dstNetworks: []struct {
				name         string
				id           int
				topologyType string
				subnets      []string
			}{
				{name: "dst-l2-network", id: 2, topologyType: ovntypes.Layer2Topology, subnets: []string{"10.129.0.0/24", "fd00:10:129::/64"}},
				{name: "dst-l3-network", id: 3, topologyType: ovntypes.Layer3Topology, subnets: []string{"10.130.0.0/24", "fd00:10:130::/64"}},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "network-router-uuid", Name: "cluster-router_src-l3-network"},
			},
			expectError:      false,
			expectedPolicies: 4, // 2 dst networks x 2 IP families
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake clientset with multiple nodes
			nodes := []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
						Annotations: map[string]string{
							"k8s.ovn.org/node-id": "1",
							util.OvnNodeChassisID: chassisIDForNode("node1"),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node2",
						Annotations: map[string]string{
							"k8s.ovn.org/node-id": "2",
							util.OvnNodeChassisID: chassisIDForNode("node2"),
						},
					},
				},
			}
			fakeClientset := util.GetOVNClientset().GetOVNKubeControllerClientset()
			for _, node := range nodes {
				_, err := fakeClientset.KubeClient.CoreV1().Nodes().Create(
					context.Background(), node, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			defer func() {
				for _, node := range nodes {
					err := fakeClientset.KubeClient.CoreV1().Nodes().Delete(context.Background(), node.Name, metav1.DeleteOptions{})
					require.NoError(t, err)
				}
			}()

			// Create watch factory
			wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClientset)
			require.NoError(t, err)
			err = wf.Start()
			require.NoError(t, err)
			defer wf.Shutdown()

			// Wait for cache sync
			syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer syncCancel()
			synced := cache.WaitForCacheSync(syncCtx.Done(), wf.NodeCoreInformer().Informer().HasSynced)
			require.True(t, synced, "informer caches should sync")

			// Create NB test harness
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: tt.initialDB,
			}, nil)
			require.NoError(t, err)
			defer cleanup.Cleanup()

			// Create mock source network
			srcNetwork := &mocks.NetInfo{}
			srcNetwork.On("GetNetworkName").Return(tt.srcNetworkName)
			srcNetwork.On("GetNetworkID").Return(tt.srcNetworkID)
			srcNetwork.On("TopologyType").Return(tt.srcTopologyType)
			if tt.srcTopologyType == ovntypes.Layer2Topology {
				srcNetwork.On("GetNetworkScopedClusterRouterName").Return("transit_" + tt.srcNetworkName)
				srcNetwork.On("GetNetworkScopedRouterToSwitchPortName", "").Return("trtos-" + tt.srcNetworkName)
			} else {
				srcNetwork.On("GetNetworkScopedClusterRouterName").Return("cluster-router_" + tt.srcNetworkName)
				srcNetwork.On("GetNetworkScopedRouterToSwitchPortName", tt.zone).Return("rtos-" + tt.srcNetworkName + "-" + tt.zone)
			}
			var srcSubnets []config.CIDRNetworkEntry
			for _, s := range tt.srcSubnets {
				srcSubnets = append(srcSubnets, config.CIDRNetworkEntry{CIDR: ovntest.MustParseIPNet(s)})
			}
			srcNetwork.On("Subnets").Return(srcSubnets)

			// Create mock destination networks and FakeNetworkManager
			fakeNM := &networkmanager.FakeNetworkManager{
				PrimaryNetworks: make(map[string]util.NetInfo),
			}
			for _, dst := range tt.dstNetworks {
				dstNet := &mocks.NetInfo{}
				dstNet.On("GetNetworkName").Return(dst.name)
				dstNet.On("GetNetworkID").Return(dst.id)
				dstNet.On("TopologyType").Return(dst.topologyType)
				var dstSubnets []config.CIDRNetworkEntry
				for _, s := range dst.subnets {
					dstSubnets = append(dstSubnets, config.CIDRNetworkEntry{CIDR: ovntest.MustParseIPNet(s)})
				}
				dstNet.On("Subnets").Return(dstSubnets)
				// Add to FakeNetworkManager (key is namespace but we use name for simplicity)
				fakeNM.PrimaryNetworks[dst.name] = dstNet
			}

			// Create controller
			c := &Controller{
				nbClient:       nbClient,
				zone:           tt.zone,
				nodeLister:     wf.NodeCoreInformer().Lister(),
				networkManager: fakeNM,
			}

			cnc := &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: tt.cncName},
			}

			ops, err := c.ensureRoutingPoliciesOps(nil, cnc.Name, srcNetwork, tt.allocatedSubnets, nodes[0])

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)

			if tt.expectedPolicies == 0 {
				assert.Empty(t, ops)
				return
			}

			require.NotEmpty(t, ops)

			// Execute ops
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			require.NoError(t, err)

			// Verify policies were created
			policies, err := libovsdbops.FindLogicalRouterPoliciesWithPredicate(nbClient, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == tt.cncName
			})
			require.NoError(t, err)
			assert.Len(t, policies, tt.expectedPolicies)

			// Verify each policy has expected fields
			for _, policy := range policies {
				assert.Equal(t, ovntypes.NetworkConnectPolicyPriority, policy.Priority)
				assert.Equal(t, nbdb.LogicalRouterPolicyActionReroute, policy.Action)
				assert.NotEmpty(t, policy.Match)
				assert.NotEmpty(t, policy.Nexthops)
				assert.Equal(t, tt.cncName, policy.ExternalIDs[libovsdbops.ObjectNameKey.String()])
			}
		})
	}
}

func TestCreateStaticRoutesOps(t *testing.T) {
	type expectedRoute struct {
		ipPrefix string
		nexthop  string
		ipFamily string // "v4" or "v6"
	}

	tests := []struct {
		name           string
		networkID      int
		routerName     string
		dstSubnets     []*net.IPNet
		nexthops       []net.IP
		cncName        string
		nodeID         int
		initialDB      []libovsdbtest.TestData
		expectError    bool
		expectedRoutes []expectedRoute
	}{
		{
			name:       "create IPv4 static route",
			networkID:  1,
			routerName: "connect-router-test",
			dstSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("10.128.0.0/24"),
			},
			nexthops: []net.IP{
				net.ParseIP("192.168.0.1"),
			},
			cncName: "test-cnc",
			nodeID:  1,
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-uuid",
					Name: "connect-router-test",
				},
			},
			expectError: false,
			expectedRoutes: []expectedRoute{
				{ipPrefix: "10.128.0.0/24", nexthop: "192.168.0.1", ipFamily: "v4"},
			},
		},
		{
			name:       "create dual-stack static routes",
			networkID:  1,
			routerName: "connect-router-test",
			dstSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("10.128.0.0/24"),
				ovntest.MustParseIPNet("fd00:10:128::/64"),
			},
			nexthops: []net.IP{
				net.ParseIP("192.168.0.1"),
				net.ParseIP("fd00::1"),
			},
			cncName: "test-cnc",
			nodeID:  1,
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-uuid",
					Name: "connect-router-test",
				},
			},
			expectError: false,
			expectedRoutes: []expectedRoute{
				{ipPrefix: "10.128.0.0/24", nexthop: "192.168.0.1", ipFamily: "v4"},
				{ipPrefix: "fd00:10:128::/64", nexthop: "fd00::1", ipFamily: "v6"},
			},
		},
		{
			name:       "skip when no matching nexthop for IP family",
			networkID:  1,
			routerName: "connect-router-test",
			dstSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("10.128.0.0/24"),
			},
			nexthops: []net.IP{
				net.ParseIP("fd00::1"), // IPv6 nexthop for IPv4 destination
			},
			cncName: "test-cnc",
			nodeID:  1,
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-uuid",
					Name: "connect-router-test",
				},
			},
			expectError:    false,
			expectedRoutes: nil, // no routes created due to family mismatch
		},
		{
			name:       "Layer2 route with nodeID 0",
			networkID:  1,
			routerName: "connect-router-test",
			dstSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("10.200.0.0/24"),
			},
			nexthops: []net.IP{
				net.ParseIP("192.168.100.1"),
			},
			cncName: "test-cnc",
			nodeID:  0, // Layer2 networks use nodeID 0
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					UUID: "router-uuid",
					Name: "connect-router-test",
				},
			},
			expectError: false,
			expectedRoutes: []expectedRoute{
				{ipPrefix: "10.200.0.0/24", nexthop: "192.168.100.1", ipFamily: "v4"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: tt.initialDB,
			}, nil)
			require.NoError(t, err)
			defer cleanup.Cleanup()

			c := &Controller{
				nbClient: nbClient,
			}

			ops, err := c.createStaticRoutesOps(nil, tt.networkID, tt.routerName, tt.dstSubnets, tt.nexthops, tt.cncName, tt.nodeID)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)

			if len(tt.expectedRoutes) == 0 {
				assert.Empty(t, ops)
				return
			}

			require.NotEmpty(t, ops)

			// Execute ops
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			require.NoError(t, err)

			// Verify the routes were created
			router, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: tt.routerName})
			require.NoError(t, err)
			assert.Len(t, router.StaticRoutes, len(tt.expectedRoutes))

			routes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(nbClient, func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == tt.cncName
			})
			require.NoError(t, err)
			assert.Len(t, routes, len(tt.expectedRoutes))

			for _, expected := range tt.expectedRoutes {
				// Find the route with matching IP family
				var found *nbdb.LogicalRouterStaticRoute
				for _, r := range routes {
					if r.ExternalIDs[libovsdbops.IPFamilyKey.String()] == expected.ipFamily {
						found = r
						break
					}
				}
				require.NotNilf(t, found, "expected route with IP family %s not found", expected.ipFamily)

				// Verify route fields
				assert.Equal(t, expected.ipPrefix, found.IPPrefix, "IPPrefix mismatch for IP family %s", expected.ipFamily)
				assert.Equal(t, expected.nexthop, found.Nexthop, "Nexthop mismatch for IP family %s", expected.ipFamily)

				// Verify ExternalIDs
				assert.Equal(t, tt.cncName, found.ExternalIDs[libovsdbops.ObjectNameKey.String()], "ObjectNameKey mismatch")
				assert.Equal(t, strconv.Itoa(tt.networkID), found.ExternalIDs[libovsdbops.NetworkIDKey.String()], "NetworkIDKey mismatch")
				assert.Equal(t, strconv.Itoa(tt.nodeID), found.ExternalIDs[libovsdbops.NodeIDKey.String()], "NodeIDKey mismatch")
				assert.Equal(t, expected.ipFamily, found.ExternalIDs[libovsdbops.IPFamilyKey.String()], "IPFamilyKey mismatch")
			}
		})
	}
}

func TestEnsureStaticRoutesOps(t *testing.T) {
	tests := []struct {
		name           string
		cncName        string
		zone           string
		networkName    string
		networkID      int
		topologyType   string
		podSubnets     []string // network's pod subnets
		connectSubnets []*net.IPNet
		nodes          []struct {
			name        string
			id          string
			nodeSubnets map[string]string // networkName -> subnet annotation
		}
		initialDB      []libovsdbtest.TestData
		expectError    bool
		expectedRoutes int
	}{
		{
			name:         "Layer3 with single node",
			cncName:      "test-cnc",
			zone:         "node1",
			networkName:  "test-network",
			networkID:    1,
			topologyType: ovntypes.Layer3Topology,
			podSubnets:   []string{"10.128.0.0/16"},
			connectSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
			},
			nodes: []struct {
				name        string
				id          string
				nodeSubnets map[string]string
			}{
				{
					name: "node1",
					id:   "1",
					nodeSubnets: map[string]string{
						"test-network": "10.128.1.0/24",
					},
				},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "connect-router-uuid", Name: "connect_router_test-cnc"},
			},
			expectError:    false,
			expectedRoutes: 1,
		},
		{
			name:         "Layer3 with multiple nodes",
			cncName:      "test-cnc",
			zone:         "node1",
			networkName:  "test-network",
			networkID:    1,
			topologyType: ovntypes.Layer3Topology,
			podSubnets:   []string{"10.128.0.0/16"},
			connectSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
			},
			nodes: []struct {
				name        string
				id          string
				nodeSubnets map[string]string
			}{
				{
					name: "node1",
					id:   "1",
					nodeSubnets: map[string]string{
						"test-network": "10.128.1.0/24",
					},
				},
				{
					name: "node2",
					id:   "2",
					nodeSubnets: map[string]string{
						"test-network": "10.128.2.0/24",
					},
				},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "connect-router-uuid", Name: "connect_router_test-cnc"},
			},
			expectError:    false,
			expectedRoutes: 2, // one route per node
		},
		{
			name:         "Layer2 creates single route",
			cncName:      "test-cnc",
			zone:         "node1",
			networkName:  "test-l2-network",
			networkID:    1,
			topologyType: ovntypes.Layer2Topology,
			podSubnets:   []string{"10.200.0.0/24"},
			connectSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/31"),
			},
			nodes: []struct {
				name        string
				id          string
				nodeSubnets map[string]string
			}{
				{name: "node1", id: "1", nodeSubnets: nil},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "connect-router-uuid", Name: "connect_router_test-cnc"},
			},
			expectError:    false,
			expectedRoutes: 1, // single route for L2
		},
		{
			name:         "Dual-stack Layer3 with single node",
			cncName:      "test-cnc",
			zone:         "node1",
			networkName:  "test-network",
			networkID:    1,
			topologyType: ovntypes.Layer3Topology,
			podSubnets:   []string{"10.128.0.0/16", "fd00:10:128::/48"},
			connectSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/24"),
				ovntest.MustParseIPNet("fd00:192:168::/120"),
			},
			nodes: []struct {
				name        string
				id          string
				nodeSubnets map[string]string
			}{
				{
					name: "node1",
					id:   "1",
					nodeSubnets: map[string]string{
						// Dual-stack format: JSON array
						"test-network": `["10.128.1.0/24", "fd00:10:128:1::/64"]`,
					},
				},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "connect-router-uuid", Name: "connect_router_test-cnc"},
			},
			expectError:    false,
			expectedRoutes: 2, // one IPv4 + one IPv6 route
		},
		{
			name:         "Dual-stack Layer2",
			cncName:      "test-cnc",
			zone:         "node1",
			networkName:  "test-l2-network",
			networkID:    1,
			topologyType: ovntypes.Layer2Topology,
			podSubnets:   []string{"10.200.0.0/24", "fd00:10:200::/64"},
			connectSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("192.168.0.0/31"),
				ovntest.MustParseIPNet("fd00:192:168::/127"),
			},
			nodes: []struct {
				name        string
				id          string
				nodeSubnets map[string]string
			}{
				{name: "node1", id: "1", nodeSubnets: nil},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "connect-router-uuid", Name: "connect_router_test-cnc"},
			},
			expectError:    false,
			expectedRoutes: 2, // one IPv4 + one IPv6 route for L2
		},
		{
			name:         "IPv6 single-stack Layer3",
			cncName:      "test-cnc",
			zone:         "node1",
			networkName:  "test-network",
			networkID:    1,
			topologyType: ovntypes.Layer3Topology,
			podSubnets:   []string{"fd00:10:128::/48"},
			connectSubnets: []*net.IPNet{
				ovntest.MustParseIPNet("fd00:192:168::/120"),
			},
			nodes: []struct {
				name        string
				id          string
				nodeSubnets map[string]string
			}{
				{
					name: "node1",
					id:   "1",
					nodeSubnets: map[string]string{
						"test-network": "fd00:10:128:1::/64",
					},
				},
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{UUID: "connect-router-uuid", Name: "connect_router_test-cnc"},
			},
			expectError:    false,
			expectedRoutes: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake clientset with nodes
			fakeClientset := util.GetOVNClientset().GetOVNKubeControllerClientset()
			var createdNodes []*corev1.Node
			for _, n := range tt.nodes {
				annotations := map[string]string{
					"k8s.ovn.org/node-id": n.id,
				}
				// Add node subnet annotations
				for netName, subnet := range n.nodeSubnets {
					// If subnet is already a JSON array (starts with [), use it directly
					// Otherwise, wrap it as a single subnet string
					if len(subnet) > 0 && subnet[0] == '[' {
						annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf(`{"%s":%s}`, netName, subnet)
					} else {
						annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf(`{"%s":"%s"}`, netName, subnet)
					}
				}
				node := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:        n.name,
						Annotations: annotations,
					},
				}
				_, err := fakeClientset.KubeClient.CoreV1().Nodes().Create(
					context.Background(), node, metav1.CreateOptions{})
				require.NoError(t, err)
				createdNodes = append(createdNodes, node)
			}
			defer func() {
				for _, node := range createdNodes {
					err := fakeClientset.KubeClient.CoreV1().Nodes().Delete(context.Background(), node.Name, metav1.DeleteOptions{})
					require.NoError(t, err)
				}
			}()

			// Create watch factory
			wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClientset)
			require.NoError(t, err)
			err = wf.Start()
			require.NoError(t, err)
			defer wf.Shutdown()

			// Wait for cache sync
			syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer syncCancel()
			synced := cache.WaitForCacheSync(syncCtx.Done(), wf.NodeCoreInformer().Informer().HasSynced)
			require.True(t, synced, "informer caches should sync")

			// Create NB test harness
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: tt.initialDB,
			}, nil)
			require.NoError(t, err)
			defer cleanup.Cleanup()

			// Create mock network
			netInfo := &mocks.NetInfo{}
			netInfo.On("GetNetworkName").Return(tt.networkName)
			netInfo.On("GetNetworkID").Return(tt.networkID)
			netInfo.On("TopologyType").Return(tt.topologyType)
			var podSubnets []config.CIDRNetworkEntry
			for _, s := range tt.podSubnets {
				podSubnets = append(podSubnets, config.CIDRNetworkEntry{CIDR: ovntest.MustParseIPNet(s)})
			}
			netInfo.On("Subnets").Return(podSubnets)

			// Create controller
			c := &Controller{
				nbClient:   nbClient,
				zone:       tt.zone,
				nodeLister: wf.NodeCoreInformer().Lister(),
			}

			cnc := &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: tt.cncName},
			}

			// Get nodes for the function call
			var nodes []*corev1.Node
			for _, n := range tt.nodes {
				node, err := wf.NodeCoreInformer().Lister().Get(n.name)
				require.NoError(t, err)
				nodes = append(nodes, node)
			}

			ops, err := c.ensureStaticRoutesOps(nil, cnc, netInfo, tt.connectSubnets, nodes)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)

			if tt.expectedRoutes == 0 {
				assert.Empty(t, ops)
				return
			}

			require.NotEmpty(t, ops)

			// Execute ops
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			require.NoError(t, err)

			// Verify routes were created
			router, err := libovsdbops.GetLogicalRouter(nbClient, &nbdb.LogicalRouter{Name: getConnectRouterName(tt.cncName)})
			require.NoError(t, err)
			assert.Len(t, router.StaticRoutes, tt.expectedRoutes)

			routes, err := libovsdbops.FindLogicalRouterStaticRoutesWithPredicate(nbClient, func(item *nbdb.LogicalRouterStaticRoute) bool {
				return item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == tt.cncName
			})
			require.NoError(t, err)
			assert.Len(t, routes, tt.expectedRoutes)

			expectedNodeIDs := map[string]struct{}{}
			if tt.topologyType == ovntypes.Layer2Topology {
				expectedNodeIDs["0"] = struct{}{}
			} else {
				for _, node := range tt.nodes {
					expectedNodeIDs[node.id] = struct{}{}
				}
			}

			for _, route := range routes {
				_, ok := expectedNodeIDs[route.ExternalIDs[libovsdbops.NodeIDKey.String()]]
				assert.True(t, ok, "unexpected NodeID %s", route.ExternalIDs[libovsdbops.NodeIDKey.String()])
				assert.Equal(t, tt.cncName, route.ExternalIDs[libovsdbops.ObjectNameKey.String()], "ObjectNameKey mismatch")
				assert.Equal(t, strconv.Itoa(tt.networkID), route.ExternalIDs[libovsdbops.NetworkIDKey.String()], "NetworkIDKey mismatch")
				expectedIPFamily := "v4"
				if strings.Contains(route.IPPrefix, ":") {
					expectedIPFamily = "v6"
				}
				assert.Equal(t, expectedIPFamily, route.ExternalIDs[libovsdbops.IPFamilyKey.String()], "IPFamilyKey mismatch")
			}
		})
	}
}
