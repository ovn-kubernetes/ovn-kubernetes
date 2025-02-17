package managementport

import (
	"net"
)

// Interface holds information about the management porr that connects the OVN
// network with the host network
type Interface interface {
	GetInterfaceName() string
	GetAddresses() []*net.IPNet
}

// Controller manages the ManagementPort. It has a reconciliation
// loop that needs to be started and can reconcile on request
type Controller interface {
	Interface
	Start(stopChan <-chan struct{}) error
	Reconcile()
}
