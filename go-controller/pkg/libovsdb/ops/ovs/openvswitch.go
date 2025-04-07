package ovs

import (
	"context"
	"fmt"
	"sync"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchd"
)

var (
	mutex             sync.Mutex
	ovsClientInstance libovsdbclient.Client
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

// GetOVSClient returns a singleton instance of the OVS client
func GetOVSClient(ctx context.Context) (libovsdbclient.Client, error) {
	mutex.Lock()
	defer mutex.Unlock()

	if ovsClientInstance != nil {
		return ovsClientInstance, nil
	}

	ovsClient, err := libovsdb.NewOVSClient(ctx.Done())
	if err != nil {
		return nil, err
	}

	ovsClientInstance = ovsClient
	return ovsClientInstance, nil
}
