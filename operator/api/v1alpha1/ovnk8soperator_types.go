// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// OVNK8sOperatorSpec defines the desired state of OVNK8sOperator
//
// +kubebuilder:validation:XValidation:rule="!has(self.componentOverrides) || self.componentOverrides.all(o, (self.global.deploymentMode == 'standard' && o.resourceName in ['ovnkubeControlPlane', 'ovnkubeNode', 'ovnkubeIdentity']) || (self.global.deploymentMode == 'dpu-host' && o.resourceName in ['ovnkubeControlPlane', 'ovnkubeNodeDpuHost', 'ovnkubeIdentity']) || (self.global.deploymentMode == 'dpu' && o.resourceName in ['ovnkubeNodeDpu']))",message="componentOverrides[].resourceName must be a component declared by spec.global.deploymentMode"
type OVNK8sOperatorSpec struct {
	// Global defines cluster-wide networking and deployment configuration.
	// +kubebuilder:validation:Required
	Global GlobalConfig `json:"global"`

	// FeatureGates defines OVN-Kubernetes feature gates to enable.
	// Each gate is a typed pointer field. Presence (non-nil) enables the gate;
	// absence (nil/omitted) disables it.
	// +optional
	FeatureGates *FeatureGates `json:"featureGates,omitempty"`

	// ComponentOverrides defines optional per-component configuration overrides
	// (replicas, container images, custom variables, node placement, …).
	// Templates use template-level fallbacks for any field left unset here.
	// +optional
	// +listType=map
	// +listMapKey=resourceName
	ComponentOverrides []ComponentOverride `json:"componentOverrides,omitempty"`

	// TestHooks defines post-deployment test hook execution configuration.
	// +optional
	TestHooks *TestHooksSpec `json:"testHooks,omitempty"`
}

// GlobalConfig defines cluster-wide networking and deployment settings.
// Gateway, subnet, and tuning fields are typed rather than using untyped
// customVariables, providing schema validation and discoverability.
type GlobalConfig struct {
	// K8sAPIServer specifies the Kubernetes API server endpoint.
	// +kubebuilder:validation:Required
	K8sAPIServer string `json:"k8sAPIServer"`

	// PodNetwork specifies one or more pod network CIDRs with per-node host prefix.
	// Each entry defines a subnet range and the prefix length allocated to each node.
	// Multiple entries support dual-stack or multi-subnet configurations.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	PodNetwork []PodNetworkEntry `json:"podNetwork"`

	// ServiceNetwork specifies the service network CIDRs (e.g., ["10.96.0.0/12"]).
	// Single-stack: one entry. Dual-stack: two entries (one IPv4, one IPv6).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="self.all(cidr, isCIDR(cidr))",message="each entry must be a valid CIDR"
	// +kubebuilder:validation:items:MaxLength=48
	ServiceNetwork []string `json:"serviceNetwork"`

	// MTU specifies the maximum transmission unit for the overlay network.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=576
	MTU int32 `json:"mtu"`

	// DeploymentMode selects the operator's deployment topology.
	// "standard" — conventional Interconnect deployment
	// "dpu-host" — host-side deployment with an external DPU dataplane
	// "dpu" — DPU-side deployment
	//
	// Mode determines the component composition declared in the operator's
	// embedded config. Use cases differentiate within a mode via
	// componentOverrides and other CR fields.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=standard;dpu-host;dpu
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.global.deploymentMode is immutable; delete and recreate the CR to change topology"
	DeploymentMode string `json:"deploymentMode"`

	// Namespace specifies the target namespace for OVN-Kubernetes resources.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self != 'ovnk8s-operator-system'",message="spec.global.namespace cannot be the operator's own namespace (ovnk8s-operator-system)"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.global.namespace is immutable; delete and recreate the CR to use a different namespace"
	Namespace string `json:"namespace"`

	// ImagePullSecrets references Secrets in the operator's own namespace
	// that OVN-Kubernetes workload pods use to pull container images.
	//
	// The referenced Secrets are expected to be of type
	// kubernetes.io/dockerconfigjson, as created by
	// `kubectl create secret docker-registry` or equivalent.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self.all(s, has(s.name) && s.name != '')",message="imagePullSecrets entries must have a non-empty name"
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// PropagateImagePullSecrets controls whether the controller copies each
	// Secret listed in ImagePullSecrets from the operator's namespace into
	// the workload namespace (ovn-kubernetes ns) on every reconcile.
	//
	// True (default) — the operator performs the copy. The source Secret
	// must exist in the operator's own namespace.
	// False — the cluster admin guarantees that equivalent Secrets exist in the
	// workload namespace (e.g. managed via GitOps or kubectl);
	// the operator leaves them untouched.
	// +kubebuilder:default=true
	// +optional
	PropagateImagePullSecrets *bool `json:"propagateImagePullSecrets,omitempty"`

	// V4JoinSubnet specifies the IPv4 join subnet (e.g., "100.64.0.0/16").
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == '' || (isCIDR(self) && cidr(self).ip().family() == 4)",message="must be a valid IPv4 CIDR"
	V4JoinSubnet string `json:"v4JoinSubnet,omitempty"`

	// V6JoinSubnet specifies the IPv6 join subnet (e.g., "fd98::/64").
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == '' || (isCIDR(self) && cidr(self).ip().family() == 6)",message="must be a valid IPv6 CIDR"
	V6JoinSubnet string `json:"v6JoinSubnet,omitempty"`

	// V4MasqueradeSubnet specifies the IPv4 masquerade subnet used for SNAT
	// plumbing when pod traffic exits to external networks (e.g., "169.254.0.0/17").
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == '' || (isCIDR(self) && cidr(self).ip().family() == 4)",message="must be a valid IPv4 CIDR"
	V4MasqueradeSubnet string `json:"v4MasqueradeSubnet,omitempty"`

	// V6MasqueradeSubnet specifies the IPv6 masquerade subnet used for SNAT
	// plumbing when pod traffic exits to external networks.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == '' || (isCIDR(self) && cidr(self).ip().family() == 6)",message="must be a valid IPv6 CIDR"
	V6MasqueradeSubnet string `json:"v6MasqueradeSubnet,omitempty"`

	// EmptyLbEvents enables monitoring of empty load balancer events.
	// +optional
	EmptyLbEvents *bool `json:"emptyLbEvents,omitempty"`

	// EnableCoredump enables coredump collection by setting GOTRACEBACK=crash
	// on OVN-Kubernetes containers. Useful for debugging Go panics.
	// +optional
	EnableCoredump *bool `json:"enableCoredump,omitempty"`

	// Metrics defines metrics settings shared by the deployed OVN-Kubernetes
	// components.
	// +optional
	Metrics *MetricsConfig `json:"metrics,omitempty"`

	// RoutingTableIDStart sets the first routing table ID that ovnkube-node
	// allocates for user-defined networks (routing-table-id-start in
	// ovnkube.conf). When unset, the ovnkube-node default is used.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RoutingTableIDStart *int32 `json:"routingTableIDStart,omitempty"`
}

// MetricsConfig defines metrics settings for OVN-Kubernetes.
type MetricsConfig struct {
	// IP is the address the standard node and control-plane metrics servers bind
	// to. When empty, the components use their built-in default.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == '' || isIP(self)",message="must be a valid IP address"
	IP string `json:"ip,omitempty"`
}

// PodNetworkEntry defines the pod network CIDR and per-node host prefix.
type PodNetworkEntry struct {
	// CIDR specifies the pod network address range (e.g., "10.192.0.0/16").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=48
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="must be a valid CIDR"
	CIDR string `json:"cidr"`

	// HostPrefix specifies the subnet prefix length allocated to each node (e.g., 26).
	// Each node gets a /<hostPrefix> subnet from the CIDR for pod IPs.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	HostPrefix int32 `json:"hostPrefix"`
}

// FeatureGates defines OVN-Kubernetes feature gates.
// Each field is a pointer; presence (non-nil) enables the gate, absence disables it.
// Simple toggles use *EmptyConfig. Gates with configuration use typed structs.
// Related gates are grouped (userDefinedNetworks, routing, egress, networkPolicy);
// a child gate is enabled by setting it inside its group.
//
// Only gates that the upstream OVN-Kubernetes Helm chart makes configurable
// are included here. Flags the operator always sets are fixed in its
// templates and are not user-configurable.
type FeatureGates struct {
	// UserDefinedNetworks enables user-defined network segmentation (UDN)
	// and groups the UDN-related gates and settings.
	// +optional
	UserDefinedNetworks *UserDefinedNetworksConfig `json:"userDefinedNetworks,omitempty"`

	// Routing groups the routing-related gates. Setting the group alone
	// enables nothing; enable the individual gates inside it.
	// +optional
	Routing *RoutingConfig `json:"routing,omitempty"`

	// Egress groups the egress-related gates. Setting the group alone
	// enables nothing; enable the individual gates inside it.
	// +optional
	Egress *EgressConfig `json:"egress,omitempty"`

	// NetworkPolicy groups the network-policy-related gates. Setting the
	// group alone enables nothing; enable the individual gates inside it.
	// +optional
	NetworkPolicy *NetworkPolicyConfig `json:"networkPolicy,omitempty"`

	// Interconnect enables multi-zone interconnect.
	// +optional
	Interconnect *InterconnectConfig `json:"interconnect,omitempty"`

	// HybridOverlay enables hybrid overlay networking.
	// +optional
	HybridOverlay *EmptyConfig `json:"hybridOverlay,omitempty"`

	// MultiExternalGateway enables multiple external gateway support and the
	// AdminPolicyBasedExternalRoutes CRD.
	// +optional
	MultiExternalGateway *EmptyConfig `json:"multiExternalGateway,omitempty"`

	// Uplink enables uplink and uplink-state tracking (OVN_UPLINK_ENABLE).
	// +optional
	Uplink *EmptyConfig `json:"uplink,omitempty"`

	// PersistentIPs enables persistent IP addresses for VMs.
	// +optional
	PersistentIPs *EmptyConfig `json:"persistentIPs,omitempty"`

	// DNSNameResolver enables the DNSNameResolver CRD.
	// +optional
	DNSNameResolver *EmptyConfig `json:"dnsNameResolver,omitempty"`

	// NetworkQoS enables the NetworkQoS CRD.
	// +optional
	NetworkQoS *EmptyConfig `json:"networkQoS,omitempty"`

	// LFlowCache configures OVN logical flow caching.
	// +optional
	LFlowCache *LFlowCacheConfig `json:"lflowCache,omitempty"`

	// Multicast enables multicast support.
	// +optional
	Multicast *EmptyConfig `json:"multicast,omitempty"`

	// ServiceTemplateSupport enables the Service template CRD.
	// +optional
	ServiceTemplateSupport *EmptyConfig `json:"serviceTemplateSupport,omitempty"`

	// The identity webhook (ovnkubeIdentity) is deliberately NOT a feature
	// gate: it is an optional component, enabled via
	// componentOverrides[ovnkubeIdentity].enabled=true.
}

// EmptyConfig is used for feature gates that have no additional configuration.
// Presence of the field (non-nil pointer) enables the gate.
type EmptyConfig struct{}

// UserDefinedNetworksConfig enables UDN and groups its sub-gates and settings.
// Presence of the group (non-nil) enables user-defined network segmentation.
type UserDefinedNetworksConfig struct {
	// PreconfiguredAddresses enables pre-configured UDN IP addresses.
	// +optional
	PreconfiguredAddresses *EmptyConfig `json:"preconfiguredAddresses,omitempty"`

	// AdvertisedIsolationMode configures the UDN pod isolation mode.
	// +optional
	// +kubebuilder:validation:Enum=strict;loose
	AdvertisedIsolationMode string `json:"advertisedIsolationMode,omitempty"`

	// NoOverlay enables no-overlay mode for the default network.
	// +optional
	NoOverlay *NoOverlayConfig `json:"noOverlay,omitempty"`

	// DynamicUDNAllocation enables dynamic UDN allocation.
	// +optional
	DynamicUDNAllocation *DynamicUDNAllocationConfig `json:"dynamicUDNAllocation,omitempty"`

	// NetworkConnect enables network connectivity features.
	// +optional
	NetworkConnect *EmptyConfig `json:"networkConnect,omitempty"`
}

// RoutingConfig groups the routing-related feature gates.
type RoutingConfig struct {
	// RouteAdvertisements enables BGP route advertisement.
	// +optional
	RouteAdvertisements *RouteAdvertisementsConfig `json:"routeAdvertisements,omitempty"`

	// EVPN enables EVPN protocol support.
	// +optional
	EVPN *EmptyConfig `json:"evpn,omitempty"`
}

// RouteAdvertisementsConfig configures BGP route advertisement.
type RouteAdvertisementsConfig struct {
	// AdvertiseDefaultNetwork advertises the default pod network by creating
	// a RouteAdvertisements object selecting the default network.
	// +optional
	AdvertiseDefaultNetwork *bool `json:"advertiseDefaultNetwork,omitempty"`
}

// EgressConfig groups the egress-related feature gates.
type EgressConfig struct {
	// IP enables EgressIP CRD support.
	// +optional
	IP *EgressIPConfig `json:"ip,omitempty"`

	// Firewall enables EgressFirewall CRD support.
	// +optional
	Firewall *EmptyConfig `json:"firewall,omitempty"`

	// Qos enables EgressQoS CRD support.
	// +optional
	Qos *EmptyConfig `json:"qos,omitempty"`

	// Service enables EgressService CRD support.
	// +optional
	Service *EmptyConfig `json:"service,omitempty"`
}

// NetworkPolicyConfig groups the network-policy-related feature gates.
type NetworkPolicyConfig struct {
	// Admin enables the AdminNetworkPolicy CRD.
	// +optional
	Admin *EmptyConfig `json:"admin,omitempty"`

	// ICMP allows ICMP and ICMPv6 traffic to bypass network policy
	// enforcement.
	// +optional
	ICMP *EmptyConfig `json:"icmp,omitempty"`
}

// NoOverlayConfig configures no-overlay mode for the default network.
type NoOverlayConfig struct {
	// Snat enables SNAT for outbound traffic in no-overlay mode.
	// When unset/false, outbound SNAT is disabled.
	// +optional
	Snat *bool `json:"snat,omitempty"`

	// ManagedRouting lets OVN-Kubernetes manage the pod network routing
	// via BGP. When unset, routing is unmanaged.
	// +optional
	ManagedRouting *NoOverlayManagedRoutingConfig `json:"managedRouting,omitempty"`
}

// NoOverlayManagedRoutingConfig configures OVN-Kubernetes-managed BGP routing
// for no-overlay networks.
type NoOverlayManagedRoutingConfig struct {
	// ASNumber is the AS number used by the BGP speakers on each node for
	// their default VRF. Defaults to 64512.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ASNumber *uint32 `json:"asNumber,omitempty"`

	// Topology configures the BGP peering topology. Defaults to full-mesh.
	// +optional
	// +kubebuilder:validation:Enum=full-mesh
	Topology string `json:"topology,omitempty"`

	// FrrNamespace is the namespace where FRR-K8s FRRConfiguration resources
	// are created. Defaults to frr-k8s-system.
	// +optional
	FrrNamespace string `json:"frrNamespace,omitempty"`
}

// InterconnectConfig defines configuration for the Interconnect feature gate.
type InterconnectConfig struct {
	// V4TransitSwitchSubnet specifies the IPv4 transit switch subnet for inter-zone communication.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == '' || (isCIDR(self) && cidr(self).ip().family() == 4)",message="must be a valid IPv4 CIDR"
	V4TransitSwitchSubnet string `json:"v4TransitSwitchSubnet,omitempty"`

	// V6TransitSwitchSubnet specifies the IPv6 transit switch subnet for inter-zone communication.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == '' || (isCIDR(self) && cidr(self).ip().family() == 6)",message="must be a valid IPv6 CIDR"
	V6TransitSwitchSubnet string `json:"v6TransitSwitchSubnet,omitempty"`
}

// EgressIPConfig defines configuration for the EgressIP feature gate.
type EgressIPConfig struct {
	// HealthCheckPort specifies the port used for EgressIP node health checks.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	HealthCheckPort *int32 `json:"healthCheckPort,omitempty"`
}

// DynamicUDNAllocationConfig defines configuration for the DynamicUDNAllocation feature gate.
type DynamicUDNAllocationConfig struct {
	// DeletionGracePeriod specifies the grace period in seconds before deleting a dynamic UDN.
	// +optional
	// +kubebuilder:validation:Minimum=0
	DeletionGracePeriod *int32 `json:"deletionGracePeriod,omitempty"`
}

// LFlowCacheConfig defines configuration for the LFlow cache feature gate.
type LFlowCacheConfig struct {
	// CacheLimit specifies the maximum number of logical flow cache entries.
	// +optional
	CacheLimit *int32 `json:"cacheLimit,omitempty"`

	// CacheLimitKb specifies the maximum logical flow cache size in kilobytes.
	// +optional
	CacheLimitKb *int32 `json:"cacheLimitKb,omitempty"`
}

// ComponentOverride defines user-facing configuration overrides for a component.
// Images, resource types, dependencies, and container-to-image mappings
// come from the operator's embedded config — users do not specify those.
type ComponentOverride struct {
	// ResourceName identifies the component to override. Must match a
	// component declared by the chosen deploymentMode in the operator's
	// embedded config (modes section).
	// +kubebuilder:validation:Enum=ovnkubeControlPlane;ovnkubeNode;ovnkubeIdentity;ovnkubeNodeDpuHost;ovnkubeNodeDpu
	ResourceName string `json:"resourceName"`

	// Enabled meaningfully applies to OPTIONAL components only (those listed
	// under a mode's optionalComponents, e.g. ovnkubeIdentity):
	// set enabled=true to deploy them. Required components are always
	// deployed regardless of this field. Leaving it unset (nil) is the
	// implicit "off" for optional components.
	//
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Replicas specifies the desired number of replicas (Deployment only).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// ContainerImages specifies container image overrides for this component.
	// Each entry maps a logical image name to a repository, tag, and optional
	// digest. The only supported logical name currently is "default" (covers all
	// containers in the component) This can be extended to support multiple images.
	// +optional
	// +listType=map
	// +listMapKey=name
	ContainerImages []ContainerImage `json:"containerImages,omitempty"`

	// NodePlacement defines scheduling constraints for this component's pods.
	// +optional
	NodePlacement *NodePlacement `json:"nodePlacement,omitempty"`

	// CustomVariables defines component-specific configuration as key-value pairs,
	// for per-component settings supported by that component's templates.
	// +optional
	CustomVariables []CustomVariable `json:"customVariables,omitempty"`

	// Patches defines escape-hatch patches applied post-render to the component's
	// Kubernetes resource. Supports JSON (RFC 6902) and merge (RFC 7386) patch types.
	// +optional
	Patches []ComponentPatch `json:"patches,omitempty"`
}

// ContainerImage specifies a container image by logical name.
type ContainerImage struct {
	// Name is the logical image name as declared by the operator's embedded
	// config.
	// +kubebuilder:validation:Enum=default
	Name string `json:"name"`

	// Repository specifies the full image repository path (e.g., "ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu").
	Repository string `json:"repository"`

	// Tag specifies the image tag.
	Tag string `json:"tag"`

	// SHA256 specifies the image digest for content-addressable verification,
	// as 64 lowercase hex characters without the "sha256:" prefix.
	// +optional
	// +kubebuilder:validation:Pattern=`^([a-f0-9]{64})?$`
	SHA256 string `json:"sha256,omitempty"`
}

// NodePlacement defines scheduling constraints for component pods.
// Uses standard Kubernetes types for full scheduling expressiveness.
type NodePlacement struct {
	// NodeSelector is a selector which must match a node's labels for the pod to be
	// scheduled on that node.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity specifies the pod's scheduling constraints.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// Tolerations specifies the pod's tolerations.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// PatchType defines the type of patch to apply.
// +kubebuilder:validation:Enum=json;merge
type PatchType string

const (
	// JSONPatchType is a JSON Patch (RFC 6902). Precise — uses array indices
	// for list operations. Best for modifying specific containers or env vars.
	JSONPatchType PatchType = "json"

	// MergePatchType is a JSON Merge Patch (RFC 7386). Deep-merges objects,
	// but replaces entire lists (does not merge by key). Use JSON patch type
	// for list-level precision.
	MergePatchType PatchType = "merge"
)

// ComponentPatch defines a patch to apply to a component's main workload resource
// (Deployment, DaemonSet, StatefulSet). Sibling objects (Services, ConfigMaps) are not patched.
type ComponentPatch struct {
	// Type specifies the patch type: "json" (RFC 6902) or "merge" (RFC 7386).
	// JSON patches are precise and support array index operations.
	// Merge patches deep-merge objects but replace entire lists.
	// +kubebuilder:default=json
	Type PatchType `json:"type,omitempty"`

	// Patch contains the patch content. For merge patches, this is a YAML/JSON
	// object that is deep-merged into the resource. For JSON patches, this is
	// a JSON array of operations (add, remove, replace, etc.).
	Patch string `json:"patch"`
}

// CustomVariable defines a key-value configuration variable.
type CustomVariable struct {
	// Name specifies the variable name.
	Name string `json:"name"`

	// Value specifies the variable value.
	Value string `json:"value"`
}

// TestHooksSpec defines test hook execution configuration.
type TestHooksSpec struct {
	// Enabled specifies whether test hooks should be executed after deployment.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// Tests specifies the list of test hook file names to execute.
	// If empty, all test hooks in the test-hooks directory will be executed.
	// +optional
	Tests []string `json:"tests,omitempty"`

	// Timeout specifies the timeout for test hook execution in seconds.
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=1
	Timeout int32 `json:"timeout,omitempty"`
}

// UpgradeStatus represents the high-level state of an OVN-Kubernetes rollout.
// +kubebuilder:validation:Enum=InProgress;Completed;Failed
type UpgradeStatus string

const (
	// UpgradeStatusInProgress indicates that a rollout is in progress.
	UpgradeStatusInProgress UpgradeStatus = "InProgress"

	// UpgradeStatusCompleted indicates that a rollout completed successfully.
	UpgradeStatusCompleted UpgradeStatus = "Completed"

	// UpgradeStatusFailed indicates that a rollout failed.
	UpgradeStatusFailed UpgradeStatus = "Failed"
)

// OVNK8sOperatorStatus defines the observed state of OVNK8sOperator.
type OVNK8sOperatorStatus struct {
	// Version is the last OVN-Kubernetes release that completed deployment,
	// workload validation, and all enabled post-deployment test hooks.
	// +optional
	Version string `json:"version,omitempty"`

	// TargetVersion is the OVN-Kubernetes release currently being reconciled.
	// +optional
	TargetVersion string `json:"targetVersion,omitempty"`

	// UpgradeStatus is the high-level state of the current rollout. It is
	// absent until the first rollout is observed.
	// +optional
	UpgradeStatus UpgradeStatus `json:"upgradeStatus,omitempty"`

	// Conditions represent the latest available observations of the resource's
	// state. CNIHealthy reflects reconciliation-time validation, not continuous
	// monitoring.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ComponentStatus shows the status of each deployed component.
	// +optional
	ComponentStatus []ComponentStatus `json:"componentStatus,omitempty"`
}

// ComponentPhase represents the phase of a component deployment.
type ComponentPhase string

const (
	// ComponentPhaseDeploying indicates the component is being deployed.
	ComponentPhaseDeploying ComponentPhase = "Deploying"

	// ComponentPhaseReady indicates the component is ready and healthy.
	ComponentPhaseReady ComponentPhase = "Ready"

	// ComponentPhaseFailed indicates the component deployment or validation failed.
	ComponentPhaseFailed ComponentPhase = "Failed"

	// ComponentPhaseUnknown indicates the component status is unknown.
	ComponentPhaseUnknown ComponentPhase = "Unknown"
)

// ComponentStatus represents the observed status of a component.
type ComponentStatus struct {
	// ResourceName identifies the component.
	ResourceName string `json:"resourceName"`

	// ResourceType identifies the resource type (Deployment, DaemonSet).
	ResourceType string `json:"resourceType"`

	// Phase indicates the current phase of the component.
	Phase ComponentPhase `json:"phase"`

	// Ready indicates if the component is ready.
	Ready bool `json:"ready"`

	// Replicas shows the desired number of replicas.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas shows the number of ready replicas.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Message contains a human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`

	// Error contains error information if the component failed.
	// +optional
	Error string `json:"error,omitempty"`

	// LastUpdateTime represents when the status was last updated.
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="OVNK8sOperator must be named 'default' — the operator enforces a singleton CR in the ovnk8s-operator-system namespace"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.version"
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".status.targetVersion"
// +kubebuilder:printcolumn:name="Upgrade",type="string",JSONPath=".status.upgradeStatus"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// OVNK8sOperator is the Schema for the ovnk8soperators API. It defines the
// desired state for an OVN-Kubernetes deployment.
//
// The CRD is namespace-scoped and the controller enforces a singleton
// instance: exactly one CR named "default" reconciled from the
// "ovnk8s-operator-system" namespace. CRs elsewhere are silently ignored by
// the manager's cache restriction; CRs with any other name are rejected at
// admission by a CEL validation rule. Note that spec.global.namespace names
// the namespace where OVN-Kubernetes workloads are deployed, which is
// distinct from the operator's own namespace.
type OVNK8sOperator struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of OVNK8sOperator
	// +required
	Spec OVNK8sOperatorSpec `json:"spec"`

	// status defines the observed state of OVNK8sOperator
	// +optional
	Status OVNK8sOperatorStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// OVNK8sOperatorList contains a list of OVNK8sOperator.
type OVNK8sOperatorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []OVNK8sOperator `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &OVNK8sOperator{}, &OVNK8sOperatorList{})
		return nil
	})
}
