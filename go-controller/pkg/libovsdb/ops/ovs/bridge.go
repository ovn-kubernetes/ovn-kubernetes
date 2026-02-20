package ovs

import (
	"context"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// ListBridges looks up all ovs bridges from the cache
func ListBridges(ovsClient libovsdbclient.Client) ([]*vswitchd.Bridge, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	searchedBridges := []*vswitchd.Bridge{}
	err := ovsClient.List(ctx, &searchedBridges)
	return searchedBridges, err
}
