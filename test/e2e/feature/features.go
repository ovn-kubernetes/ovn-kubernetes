package feature

import "github.com/ovn-org/ovn-kubernetes/test/e2e/label"

var (
	Service               = label.NewFeature("Service").GinkgoLabel()
	NetworkPolicy         = label.NewFeature("NetworkPolicy").GinkgoLabel()
	AdminNetworkPolicy    = label.NewFeature("AdminNetworkPolicy").GinkgoLabel()
	BaselineNetworkPolicy = label.NewFeature("BaselineNetworkPolicy").GinkgoLabel()
	NetworkSegmentation   = label.NewFeature("NetworkSegmentation").GinkgoLabel()
	EgressIP              = label.NewFeature("EgressIP").GinkgoLabel()
	EgressService         = label.NewFeature("EgressService").GinkgoLabel()
	EgressFirewall        = label.NewFeature("EgressFirewall").GinkgoLabel()
	EgressQos             = label.NewFeature("EgressQos").GinkgoLabel()
	ExternalGateway       = label.NewFeature("ExternalGateway").GinkgoLabel()
	DisablePacketMTUCheck = label.NewFeature("DisablePacketMTUCheck").GinkgoLabel()
	VirtualMachineSupport = label.NewFeature("VirtualMachineSupport").GinkgoLabel()
	Interconnect          = label.NewFeature("Interconnect").GinkgoLabel()
	Multicast             = label.NewFeature("Multicast").GinkgoLabel()
	MultiHoming           = label.NewFeature("MultiHoming").GinkgoLabel()
	NodeIPMACMigration    = label.NewFeature("NodeIPMACMigration").GinkgoLabel()
	OVSCPUPin             = label.NewFeature("OVSCPUPin").GinkgoLabel()
	Unidle                = label.NewFeature("Unidle").GinkgoLabel()
)
