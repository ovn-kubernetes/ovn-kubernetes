package ovs

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// Get OpenvSwitch entry from the cache
func GetOpenvSwitch(ovsClient libovsdbclient.Client) (*vswitchd.OpenvSwitch, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	openvSwitchList := []*vswitchd.OpenvSwitch{}
	err := ovsClient.List(ctx, &openvSwitchList)
	if err != nil {
		return nil, err
	}
	if len(openvSwitchList) == 0 {
		return nil, fmt.Errorf("no openvSwitch entry found")
	}

	return openvSwitchList[0], err
}
