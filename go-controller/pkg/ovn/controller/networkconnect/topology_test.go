package networkconnect

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

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
			name:      "create new connect router",
			cncName:   "test-cnc",
			tunnelID:  100,
			initialDB: []libovsdbtest.TestData{},
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
