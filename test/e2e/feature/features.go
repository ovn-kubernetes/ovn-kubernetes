package feature

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/label"
)

var (
	Service               = New("Service")
	NetworkPolicy         = New("NetworkPolicy")
	AdminNetworkPolicy    = New("AdminNetworkPolicy")
	BaselineNetworkPolicy = New("BaselineNetworkPolicy")
	NetworkSegmentation   = New("NetworkSegmentation")
	EgressIP              = New("EgressIP")
	EgressService         = New("EgressService")
	EgressFirewall        = New("EgressFirewall")
	EgressQos             = New("EgressQos")
	EVPN                  = New("EVPN")
	ExternalGateway       = New("ExternalGateway")
	DisablePacketMTUCheck = New("DisablePacketMTUCheck")
	VirtualMachineSupport = New("VirtualMachineSupport")
	Interconnect          = New("Interconnect")
	Multicast             = New("Multicast")
	MultiHoming           = New("MultiHoming")
	NodeIPMACMigration    = New("NodeIPMACMigration")
	NoOverlay             = New("NoOverlay")
	OVSCPUPin             = New("OVSCPUPin")
	RouteAdvertisements   = New("RouteAdvertisements")
	Unidle                = New("Unidle")
	NetworkQos            = New("NetworkQos")
	NetworkConnect        = New("NetworkConnect")
)

func New(name string) ginkgo.Labels {
	return label.New("Feature", name).GinkgoLabel()
}
