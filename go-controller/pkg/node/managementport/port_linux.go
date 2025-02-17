//go:build linux
// +build linux

package managementport

import (
	"net"

	v1 "k8s.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// NewManagementPortController creates a new ManagementPorts
func NewManagementPortController(
	node *v1.Node,
	hostSubnets []*net.IPNet,
	netdevDevName string,
	repDevName string,
	routeManager *routemanager.Controller,
	netInfo util.NetInfo,
) (Controller, error) {
	return nil, nil
}
