package helpers

const (
	Charset                   = "abcdefghijklmnopqrstuvwxyz"
	ExternalContainerImage    = "quay.io/trozet/ovnkbfdtest:0.3"
	ovnControllerLogPath      = "/var/log/openvswitch/ovn-controller.log"
	AgnhostImage              = "registry.k8s.io/e2e-test-images/agnhost:2.26"
	AgnhostImageNew           = "registry.k8s.io/e2e-test-images/agnhost:2.53"
	Iperf3Image               = "quay.io/sronanrh/iperf"
	DefaultPodInterface       = "eth0"
	RequiredUDNNamespaceLabel = "k8s.ovn.org/primary-user-defined-network"
	OvnPodAnnotationName      = "k8s.ovn.org/pod-networks"
)
