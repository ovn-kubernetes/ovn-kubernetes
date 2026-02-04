package types

import "time"

const (
	// Default network name
	DefaultNetworkName    = "default"
	K8sPrefix             = "k8s-"
	HybridOverlayPrefix   = "int-"
	HybridOverlayGRSubfix = "-gr"

	// K8sMgmtIntfNamePrefix name to be used as an OVS internal port on the node as prefix for networs
	K8sMgmtIntfNamePrefix = "ovn-k8s-mp"

	// UDNVRFDeviceSuffix vrf device suffix associated with every user defined primary network.
	UDNVRFDeviceSuffix = "-udn-vrf"
	// UDNVRFDevicePrefix vrf device prefix associated with every user
	UDNVRFDevicePrefix = "mp"

	// K8sMgmtIntfName name to be used as an OVS internal port on the node
	K8sMgmtIntfName = K8sMgmtIntfNamePrefix + "0"

	// PhysicalNetworkName is the name that maps to an OVS bridge that provides
	// access to physical/external network
	PhysicalNetworkName     = "physnet"
	PhysicalNetworkExGwName = "exgwphysnet"

	// LoopbackInterfaceIndex is the link index corresponding to loopback interface
	LoopbackInterfaceIndex = 1

	// LocalNetworkName is the name that maps to an OVS bridge that provides
	// access to local service
	LocalNetworkName = "locnet"

	// OVS Bridge Datapath types
	DatapathUserspace = "netdev"

	// types.OVNClusterRouter is the name of the distributed router
	OVNClusterRouter = "ovn_cluster_router"
	OVNJoinSwitch    = "join"

	JoinSwitchPrefix           = "join_"
	ExternalSwitchPrefix       = "ext_"
	GWRouterPrefix             = "GR_"
	RouterToSwitchPrefix       = "rtos-"
	HybridSubnetPrefix         = "hybrid-subnet-"
	SwitchToRouterPrefix       = "stor-"
	JoinSwitchToGWRouterPrefix = "jtor-"
	GWRouterToJoinSwitchPrefix = "rtoj-"
	EXTSwitchToGWRouterPrefix  = "etor-"
	GWRouterToExtSwitchPrefix  = "rtoe-"
	EgressGWSwitchPrefix       = "exgw-"
	PatchPortPrefix            = "patch-"
	PatchPortSuffix            = "-to-br-int"

	// types.OVNLayer2Switch is the name of layer2 topology switch
	OVNLayer2Switch = "ovn_layer2_switch"
	// types.OVNLocalnetSwitch is the name of localnet topology switch
	OVNLocalnetSwitch = "ovn_localnet_switch"
	// types.OVNLocalnetPort is the name of localnet topology localnet port
	OVNLocalnetPort = "ovn_localnet_port"

	TransitSwitch               = "transit_switch"
	TransitSwitchToRouterPrefix = "tstor-"
	RouterToTransitSwitchPrefix = "rtots-"
	TransitRouter               = "transit_router"
	TransitRouterToRouterPrefix = "trtor-"
	RouterToTransitRouterPrefix = "rtotr-"
	TransitRouterToSwitchPrefix = "trtos-"
	SwitchToTransitRouterPrefix = "stotr-"

	// Connect router prefix (for ClusterNetworkConnect feature)
	ConnectRouterPrefix = "connect_router_"
	// Connect router port prefixes (for ClusterNetworkConnect)
	ConnectRouterToRouterPrefix = "crtor-"
	RouterToConnectRouterPrefix = "rtocr-"

	// DefaultACLTier Priorities

	// Default routed multicast allow acl rule priority
	DefaultRoutedMcastAllowPriority = 1013
	// Default multicast allow acl rule priority
	DefaultMcastAllowPriority = 1012
	// Default multicast deny acl rule priority
	DefaultMcastDenyPriority = 1011
	// Default allow acl rule priority
	DefaultAllowPriority = 1001
	// Default deny acl rule priority
	DefaultDenyPriority = 1000
	// Pass priority for isolated advertised networks
	AdvertisedNetworkPassPriority = 1100
	// Deny priority for isolated advertised networks
	AdvertisedNetworkDenyPriority = 1050

	// PrimaryACLTier Priorities

	PrimaryUDNAllowPriority = 1001
	// Default deny acl rule priority
	PrimaryUDNDenyPriority = 1000

	// ACL Tiers
	// Tier 0 is called Primary as it is evaluated before any other feature-related Tiers.
	// Currently used for User Defined Network Feature.
	PrimaryACLTier = 0
	// Default Tier for all ACLs
	DefaultACLTier = 2
	// Default Tier for all ACLs belonging to Admin Network Policy
	DefaultANPACLTier = 1
	// Default Tier for all ACLs belonging to Baseline Admin Network Policy
	DefaultBANPACLTier = 3

	// priority of logical router policies on the OVNClusterRouter
	EgressFirewallStartPriority           = 10000
	MinimumReservedEgressFirewallPriority = 2000
	MGMTPortPolicyPriority                = "1005"
	NodeSubnetPolicyPriority              = "1004"
	InterNodePolicyPriority               = "1003"
	UDNHostCIDRPolicyPriority             = "99"
	HybridOverlaySubnetPriority           = 1002
	HybridOverlayReroutePriority          = 501
	DefaultNoRereoutePriority             = 102
	EgressSVCReroutePriority              = 101
	EgressIPReroutePriority               = 100
	EgressIPRerouteQoSRulePriority        = 103
	NetworkConnectPolicyPriority          = 9001
	// priority of logical router policies on a nodes gateway router
	EgressIPSNATMarkPriority           = 95
	EgressLiveMigrationReroutePriority = 10

	// EndpointSliceMirrorControllerName mirror EndpointSlice controller name (used as a value for the "endpointslice.kubernetes.io/managed-by" label)
	EndpointSliceMirrorControllerName = "endpointslice-mirror-controller.k8s.ovn.org"
	// EndpointSliceDefaultControllerName default kubernetes EndpointSlice controller name (used as a value for the "endpointslice.kubernetes.io/managed-by" label)
	EndpointSliceDefaultControllerName = "endpointslice-controller.k8s.io"
	// SourceEndpointSliceAnnotation key used in mirrored EndpointSlice
	// that has the value of the default EndpointSlice name
	SourceEndpointSliceAnnotation = "k8s.ovn.org/source-endpointslice"
	// LabelSourceEndpointSliceVersion label key used in mirrored EndpointSlice
	// that has the value of the last known default EndpointSlice ResourceVersion
	LabelSourceEndpointSliceVersion = "k8s.ovn.org/source-endpointslice-version"
	// UserDefinedNetworkEndpointSliceAnnotation key used in mirrored EndpointSlices that contains the current primary user defined network name
	UserDefinedNetworkEndpointSliceAnnotation = "k8s.ovn.org/endpointslice-network"
	// LabelUserDefinedServiceName label key used in mirrored EndpointSlices that contains the service name matching the EndpointSlice
	LabelUserDefinedServiceName = "k8s.ovn.org/service-name"

	// Packet marking
	EgressIPNodeConnectionMark         = "1008"
	EgressIPReplyTrafficConnectionMark = 42

	// primary user defined network's default join subnet value
	// users can configure custom values using NADs
	UserDefinedPrimaryNetworkJoinSubnetV4 = "100.65.0.0/16"
	UserDefinedPrimaryNetworkJoinSubnetV6 = "fd99::/64"

	// OpenFlow and Networking constants
	RouteAdvertisementICMPType    = 134
	NeighborSolicitationICMPType  = 135
	NeighborAdvertisementICMPType = 136

	// Meter constants
	OvnACLLoggingMeter   = "acl-logging"
	OvnRateLimitingMeter = "rate-limiter"
	PacketsPerSecond     = "pktps"
	MeterAction          = "drop"

	// Default COPP object name
	DefaultCOPPName = "ovnkube-default"

	// OVN-K8S annotation & taint constants
	OvnK8sPrefix = "k8s.ovn.org"

	// OvnNetworkNameAnnotation is the name of the network annotated on the NAD
	// by cluster manager nad controller
	OvnNetworkNameAnnotation = OvnK8sPrefix + "/network-name"
	// OvnNetworkIDAnnotation is a unique network identifier annotated on the
	// NAD by cluster manager nad controller
	OvnNetworkIDAnnotation = OvnK8sPrefix + "/network-id"
	// OvnNetworkTunnelKeysAnnotation is used to assign tunnel keys for the distributed switches and routers
	// Assigned to the NADs for now
	OvnNetworkTunnelKeysAnnotation = OvnK8sPrefix + "/tunnel-keys"
	// Deprecated: we used to set topology version as an annotation on the node. We don't do this anymore.
	OvnK8sTopoAnno            = OvnK8sPrefix + "/" + "topology-version"
	OvnK8sSmallMTUTaintKey    = OvnK8sPrefix + "/" + "mtu-too-small"
	OvnRouteAdvertisementsKey = OvnK8sPrefix + "/route-advertisements"

	// name of the configmap used to synchronize status (e.g. watch for topology changes)
	OvnK8sStatusCMName         = "control-plane-status"
	OvnK8sStatusKeyTopoVersion = "topology-version"

	// Monitoring constants
	SFlowAgent = "ovn-k8s-mp0"

	// OVNKube-Node Node types
	NodeModeFull    = "full"
	NodeModeDPU     = "dpu"
	NodeModeDPUHost = "dpu-host"

	// Gateway interface configuration
	DeriveFromMgmtPort = "derive-from-mgmt-port"

	// Geneve header length for IPv4 (https://github.com/openshift/cluster-network-operator/pull/720#issuecomment-664020823)
	GeneveHeaderLengthIPv4 = 58
	// Geneve header length for IPv6 (https://github.com/openshift/cluster-network-operator/pull/720#issuecomment-664020823)
	GeneveHeaderLengthIPv6 = GeneveHeaderLengthIPv4 + 20

	ClusterPortGroupNameBase    = "clusterPortGroup"
	ClusterRtrPortGroupNameBase = "clusterRtrPortGroup"

	OVSDBTimeout     = 10 * time.Second
	OVSDBWaitTimeout = 0

	ClusterLBGroupName       = "clusterLBGroup"
	ClusterSwitchLBGroupName = "clusterSwitchLBGroup"
	ClusterRouterLBGroupName = "clusterRouterLBGroup"

	// key for network name external-id
	NetworkExternalID = OvnK8sPrefix + "/" + "network"
	// key for node name external-id
	NodeExternalID = OvnK8sPrefix + "/" + "node"
	// key for network role external-id: possible values are "default", "primary", "secondary"
	NetworkRoleExternalID = OvnK8sPrefix + "/" + "role"
	// key for NAD name external-id, only used for secondary logical switch port of a pod
	// key for network name external-id
	NADExternalID = OvnK8sPrefix + "/" + "nad"
	// key for topology type external-id, only used for secondary network logical entities
	TopologyExternalID = OvnK8sPrefix + "/" + "topology"
	// key for load_balancer kind external-id
	LoadBalancerKindExternalID = OvnK8sPrefix + "/" + "kind"
	// key for load_balancer service external-id
	LoadBalancerOwnerExternalID = OvnK8sPrefix + "/" + "owner"
	// key for UDN enabled services routes
	UDNEnabledServiceExternalID = OvnK8sPrefix + "/" + "udn-enabled-default-service"
	// key for management port name, indicating the netdev link name associated with the given management port representor OVS interface
	OvnManagementPortNameExternalID = OvnK8sPrefix + "/management-port-name"
	// RequiredUDNNamespaceLabel is the required namespace label for enabling primary UDNs
	RequiredUDNNamespaceLabel = "k8s.ovn.org/primary-user-defined-network"

	// different user-defined network topology types defined in CNI netconf
	Layer3Topology   = "layer3"
	Layer2Topology   = "layer2"
	LocalnetTopology = "localnet"

	// different types of network roles
	// defined in CNI netconf as a user-defined network
	NetworkRolePrimary   = "primary"
	NetworkRoleSecondary = "secondary"
	NetworkRoleDefault   = "default"
	// NetworkRoleInfrastructure is defined internally by ovnkube to recognize "default"
	// network's role as an "infrastructure-locked" network
	// when a user-defined network is the primary network for
	// the pod which makes "default" network neither primary
	// nor secondary
	NetworkRoleInfrastructure = "infrastructure-locked"
	NetworkRoleNone           = "none"

	// Network transport types - canonical format (lowercase)
	NetworkTransportGeneve    = "geneve"
	NetworkTransportNoOverlay = "no-overlay"
	NetworkTransportEVPN      = "evpn"

	// NoOverlaySNATEnabled enables SNAT for outbound traffic
	NoOverlaySNATEnabled = "enabled"
	// NoOverlaySNATDisabled disables SNAT for outbound traffic
	NoOverlaySNATDisabled = "disabled"

	// db index keys
	// PrimaryIDKey is used as a primary client index
	PrimaryIDKey = OvnK8sPrefix + "/id"

	OvnDefaultZone = "global"

	// EgressService "reserved" hosts - when set on an EgressService they have a special meaning

	EgressServiceNoHost     = ""    // set on services with no allocated node
	EgressServiceNoSNATHost = "ALL" // set on services with sourceIPBy=Network

	// MaxLogicalPortTunnelKey is maximum tunnel key that can be requested for a
	// Logical Switch or Router Port
	MaxLogicalPortTunnelKey = 32767

	// InformerSyncTimeout is used when waiting for the initial informer cache sync
	// (i.e. all existing objects should be listed by the informer).
	// It allows ~5 list() retries with the default reflector exponential backoff config
	// Also considers listing a high number of items on high load scenarios
	// (last observed 4k egress firewall taking > 30s)
	// TODO: consider not using a timeout, potentially shifting to configurable
	// readiness probe
	InformerSyncTimeout = 60 * time.Second

	// HandlerSyncTimeout is used when waiting for initial object handler sync.
	// (i.e. all the ADD events should be processed for the existing objects by the event handler)
	HandlerSyncTimeout = 20 * time.Second

	// GRMACBindingAgeThreshold is the lifetime in seconds of each MAC binding
	// entry for the gateway routers. After this time, the entry is removed and
	// may be refreshed with a new ARP request.
	GRMACBindingAgeThreshold = "300"

	// InvalidID signifies an invalid ID. Currently used for network and tunnel IDs.
	InvalidID = -1

	// NoTunnelID signifies an empty/unset ID. Currently used for tunnel ID (reserved as un-usable when the allocator is created)
	NoTunnelID = 0

	// DefaultNetworkID is reserved for the default network only
	DefaultNetworkID = 0

	// NoNetworkID is used to signal internally that an ID is empty and should, updates
	// with this value should be ignored
	NoNetworkID = -2

	// OVNKubeITPMark is the fwmark used for host->ITP=local svc traffic. Note
	// that the fwmark is not a part of the packet, but just stored by kernel in
	// its memory to track/filter packet. Hence fwmark is lost as soon as packet
	// exits the host. The mark is set with an iptables rule by gateway and used
	// to route to management port.
	OVNKubeITPMark = "0x1745ec" // constant itp(174)-service(5ec)

	// "mgmtport-no-snat-nodeports" is a set containing protocol / nodePort tuples
	// indicating traffic that should not be SNATted when passing through the
	// management port because it is addressed to an `externalTrafficPolicy: Local`
	// NodePort.
	NFTMgmtPortNoSNATNodePorts = "mgmtport-no-snat-nodeports"

	// "mgmtport-no-snat-services-v4" and "mgmtport-no-snat-services-v6" are sets
	// containing loadBalancerIP / protocol / port tuples indicating traffic that
	// should not be SNATted when passing through the management port because it is
	// addressed to an `externalTrafficPolicy: Local` load balancer IP.
	NFTMgmtPortNoSNATServicesV4 = "mgmtport-no-snat-services-v4"
	NFTMgmtPortNoSNATServicesV6 = "mgmtport-no-snat-services-v6"

	// CUDNPrefix of all CUDN network names
	CUDNPrefix = "cluster_udn_"

	// NFTRemoteNodeIPsv4 is a set used to track remote node v4IPs that do not belong to
	// the local node's subnet.
	NFTRemoteNodeIPsv4 = "remote-node-ips-v4"

	// NFTRemoteNodeIPsv6 is a set used to track remote node v6IPs that do not belong to
	// the local node's subnet.
	NFTRemoteNodeIPsv6 = "remote-node-ips-v6"

	// Metrics
	MetricOvnkubeNamespace               = "ovnkube"
	MetricOvnkubeSubsystemController     = "controller"
	MetricOvnkubeSubsystemClusterManager = "clustermanager"
	MetricOvnkubeSubsystemNode           = "node"
	MetricOvnNamespace                   = "ovn"
	MetricOvnSubsystemDB                 = "db"
	MetricOvnSubsystemNorthd             = "northd"
	MetricOvnSubsystemController         = "controller"
	MetricOvsNamespace                   = "ovs"
	MetricOvsSubsystemVswitchd           = "vswitchd"
	MetricOvsSubsystemDB                 = "db"

	// "mgmtport-no-snat-subnets-v4" and "mgmtport-no-snat-subnets-v6" are sets containing
	// subnets, indicating traffic that should not be SNATted when passing through the
	// management port.
	NFTMgmtPortNoSNATSubnetsV4 = "mgmtport-no-snat-subnets-v4"
	NFTMgmtPortNoSNATSubnetsV6 = "mgmtport-no-snat-subnets-v6"
)
