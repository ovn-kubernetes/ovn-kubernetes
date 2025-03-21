package feature

import "github.com/ovn-org/ovn-kubernetes/test/e2e/label"

var (
	Service               = label.NewFeature("Service")
	NetworkPolicy         = label.NewFeature("NetworkPolicy")
	AdminNetworkPolicy    = label.NewFeature("AdminNetworkPolicy")
	BaselineNetworkPolicy = label.NewFeature("BaselineNetworkPolicy")
	NetworkSegmentation   = label.NewFeature("NetworkSegmentation")
	EgressIP              = label.NewFeature("EgressIP")
	EgressService         = label.NewFeature("EgressService")
	EgressFirewall        = label.NewFeature("EgressFirewall")
	EgressQos             = label.NewFeature("EgressQos")
	ExternalGateway       = label.NewFeature("ExternalGateway")
	DisablePacketMTUCheck = label.NewFeature("DisablePacketMTUCheck")
	VirtualMachineSupport = label.NewFeature("VirtualMachineSupport")
	Interconnect          = label.NewFeature("Interconnect")
	Multicast             = label.NewFeature("Multicast")
	MultiHoming           = label.NewFeature("MultiHoming")
	NodeIPMACMigration    = label.NewFeature("NodeIPMACMigration")
	OVSCPUPin             = label.NewFeature("OVSCPUPin")
	Unidle                = label.NewFeature("Unidle")
)
