package multihoming

import nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

type NetworkAttachmentConfigParams struct {
	Cidr                string
	ExcludeCIDRs        []string
	Namespace           string
	Name                string
	Topology            string
	NetworkName         string
	VlanID              int
	AllowPersistentIPs  bool
	Role                string
	Mtu                 int
	PhysicalNetworkName string
}

type NetworkAttachmentConfig struct {
	NetworkAttachmentConfigParams
}

type PodConfiguration struct {
	Attachments                  []nadapi.NetworkSelectionElement
	ContainerCmd                 []string
	Name                         string
	Namespace                    string
	NodeSelector                 map[string]string
	IsPrivileged                 bool
	Labels                       map[string]string
	RequiresExtraNamespace       bool
	HostNetwork                  bool
	NeedsIPRequestFromHostSubnet bool
}
