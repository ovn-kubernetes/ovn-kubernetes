package networkconnect

import (
	"fmt"
	"strconv"

	"k8s.io/klog/v2"

	networkconnectv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// getConnectRouterName returns the connect router name for a CNC.
func getConnectRouterName(cncName string) string {
	return ovntypes.ConnectRouterPrefix + cncName
}

// ensureConnectRouter creates or updates the connect router for a CNC.
func (c *Controller) ensureConnectRouter(cnc *networkconnectv1.ClusterNetworkConnect, tunnelID int) error {
	routerName := getConnectRouterName(cnc.Name)
	// The default COPP is used for all routers in all networks.
	// Since the default COPP is created in SetupMaster() which is
	// called before the network connect controller is initialized (run() method),
	// we can safely fetch and use the default COPP here.
	copp, err := libovsdbops.GetCOPP(c.nbClient, &nbdb.Copp{Name: ovntypes.DefaultCOPPName})
	if err != nil {
		return fmt.Errorf("unable to create router control plane protection: %w", err)
	}
	router := &nbdb.LogicalRouter{
		Name: routerName,
		ExternalIDs: map[string]string{
			libovsdbops.ObjectNameKey.String():      cnc.Name,
			libovsdbops.OwnerControllerKey.String(): controllerName,
			libovsdbops.OwnerTypeKey.String():       libovsdbops.ClusterNetworkConnectOwnerType,
		},
		Options: map[string]string{
			// Set the tunnel key for the connect router
			"requested-tnl-key": strconv.Itoa(tunnelID),
		},
		Copp: &copp.UUID,
	}

	// Create or update the router
	err = libovsdbops.CreateOrUpdateLogicalRouter(c.nbClient, router, &router.ExternalIDs, &router.Options, &router.Copp)
	if err != nil {
		return fmt.Errorf("failed to create/update connect router %s for CNC %s: %v", routerName, cnc.Name, err)
	}

	klog.V(4).Infof("Ensured connect router %s with tunnel ID %d", routerName, tunnelID)
	return nil
}

// deleteConnectRouter deletes the connect router for a CNC.
func (c *Controller) deleteConnectRouter(cncName string) error {
	routerName := getConnectRouterName(cncName)

	router := &nbdb.LogicalRouter{Name: routerName}
	err := libovsdbops.DeleteLogicalRouter(c.nbClient, router)
	if err != nil {
		return fmt.Errorf("failed to delete connect router %s: %v", routerName, err)
	}

	klog.V(4).Infof("Deleted connect router %s", routerName)
	return nil
}
