# API Reference

## Packages
- [k8s.ovn.org/v1alpha1](#k8sovnorgv1alpha1)


## k8s.ovn.org/v1alpha1

Package v1alpha1 contains API Schema definitions for the k8s v1alpha1 API group.

### Resource Types
- [OVNK8sOperator](#ovnk8soperator)
- [OVNK8sOperatorList](#ovnk8soperatorlist)



#### ComponentOverride



ComponentOverride defines user-facing configuration overrides for a component.
Images, resource types, dependencies, and container-to-image mappings
come from the operator's embedded config — users do not specify those.



_Appears in:_
- [OVNK8sOperatorSpec](#ovnk8soperatorspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `resourceName` _string_ | ResourceName identifies the component to override. Must match a<br />component declared by the chosen deploymentMode in the operator's<br />embedded config (modes section). |  | Enum: [ovnkubeControlPlane ovnkubeNode ovnkubeIdentity ovnkubeNodeDpuHost ovnkubeNodeDpu] <br /> |
| `enabled` _boolean_ | Enabled meaningfully applies to OPTIONAL components only (those listed<br />under a mode's optionalComponents, e.g. ovnkubeIdentity):<br />set enabled=true to deploy them. Required components are always<br />deployed regardless of this field. Leaving it unset (nil) is the<br />implicit "off" for optional components. |  | Optional: \{\} <br /> |
| `replicas` _integer_ | Replicas specifies the desired number of replicas (Deployment only). |  | Minimum: 1 <br />Optional: \{\} <br /> |
| `containerImages` _[ContainerImage](#containerimage) array_ | ContainerImages specifies container image overrides for this component.<br />Each entry maps a logical image name to a repository, tag, and optional<br />digest. The only supported logical name currently is "default" (covers all<br />containers in the component) This can be extended to support multiple images. |  | Optional: \{\} <br /> |
| `nodePlacement` _[NodePlacement](#nodeplacement)_ | NodePlacement defines scheduling constraints for this component's pods. |  | Optional: \{\} <br /> |
| `customVariables` _[CustomVariable](#customvariable) array_ | CustomVariables defines component-specific configuration as key-value pairs,<br />for per-component settings supported by that component's templates. |  | Optional: \{\} <br /> |
| `patches` _[ComponentPatch](#componentpatch) array_ | Patches defines escape-hatch patches applied post-render to the component's<br />Kubernetes resource. Supports JSON (RFC 6902) and merge (RFC 7386) patch types. |  | Optional: \{\} <br /> |


#### ComponentPatch



ComponentPatch defines a patch to apply to a component's main workload resource
(Deployment, DaemonSet, StatefulSet). Sibling objects (Services, ConfigMaps) are not patched.



_Appears in:_
- [ComponentOverride](#componentoverride)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[PatchType](#patchtype)_ | Type specifies the patch type: "json" (RFC 6902) or "merge" (RFC 7386).<br />JSON patches are precise and support array index operations.<br />Merge patches deep-merge objects but replace entire lists. | json | Enum: [json merge] <br /> |
| `patch` _string_ | Patch contains the patch content. For merge patches, this is a YAML/JSON<br />object that is deep-merged into the resource. For JSON patches, this is<br />a JSON array of operations (add, remove, replace, etc.). |  |  |


#### ComponentPhase

_Underlying type:_ _string_

ComponentPhase represents the phase of a component deployment.



_Appears in:_
- [ComponentStatus](#componentstatus)

| Field | Description |
| --- | --- |
| `Deploying` | ComponentPhaseDeploying indicates the component is being deployed.<br /> |
| `Ready` | ComponentPhaseReady indicates the component is ready and healthy.<br /> |
| `Failed` | ComponentPhaseFailed indicates the component deployment or validation failed.<br /> |
| `Unknown` | ComponentPhaseUnknown indicates the component status is unknown.<br /> |


#### ComponentStatus



ComponentStatus represents the observed status of a component.



_Appears in:_
- [OVNK8sOperatorStatus](#ovnk8soperatorstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `resourceName` _string_ | ResourceName identifies the component. |  |  |
| `resourceType` _string_ | ResourceType identifies the resource type (Deployment, DaemonSet). |  |  |
| `phase` _[ComponentPhase](#componentphase)_ | Phase indicates the current phase of the component. |  |  |
| `ready` _boolean_ | Ready indicates if the component is ready. |  |  |
| `replicas` _integer_ | Replicas shows the desired number of replicas. |  | Optional: \{\} <br /> |
| `readyReplicas` _integer_ | ReadyReplicas shows the number of ready replicas. |  | Optional: \{\} <br /> |
| `message` _string_ | Message contains a human-readable status message. |  | Optional: \{\} <br /> |
| `error` _string_ | Error contains error information if the component failed. |  | Optional: \{\} <br /> |
| `lastUpdateTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#time-v1-meta)_ | LastUpdateTime represents when the status was last updated. |  | Optional: \{\} <br /> |


#### ContainerImage



ContainerImage specifies a container image by logical name.



_Appears in:_
- [ComponentOverride](#componentoverride)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the logical image name as declared by the operator's embedded<br />config. |  | Enum: [default] <br /> |
| `repository` _string_ | Repository specifies the full image repository path (e.g., "ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu"). |  |  |
| `tag` _string_ | Tag specifies the image tag. |  |  |
| `sha256` _string_ | SHA256 specifies the image digest for content-addressable verification,<br />as 64 lowercase hex characters without the "sha256:" prefix. |  | Pattern: `^([a-f0-9]\{64\})?$` <br />Optional: \{\} <br /> |


#### CustomVariable



CustomVariable defines a key-value configuration variable.



_Appears in:_
- [ComponentOverride](#componentoverride)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name specifies the variable name. |  |  |
| `value` _string_ | Value specifies the variable value. |  |  |


#### DynamicUDNAllocationConfig



DynamicUDNAllocationConfig defines configuration for the DynamicUDNAllocation feature gate.



_Appears in:_
- [UserDefinedNetworksConfig](#userdefinednetworksconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `deletionGracePeriod` _integer_ | DeletionGracePeriod specifies the grace period in seconds before deleting a dynamic UDN. |  | Minimum: 0 <br />Optional: \{\} <br /> |


#### EgressConfig



EgressConfig groups the egress-related feature gates.



_Appears in:_
- [FeatureGates](#featuregates)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ip` _[EgressIPConfig](#egressipconfig)_ | IP enables EgressIP CRD support. |  | Optional: \{\} <br /> |
| `firewall` _[EmptyConfig](#emptyconfig)_ | Firewall enables EgressFirewall CRD support. |  | Optional: \{\} <br /> |
| `qos` _[EmptyConfig](#emptyconfig)_ | Qos enables EgressQoS CRD support. |  | Optional: \{\} <br /> |
| `service` _[EmptyConfig](#emptyconfig)_ | Service enables EgressService CRD support. |  | Optional: \{\} <br /> |


#### EgressIPConfig



EgressIPConfig defines configuration for the EgressIP feature gate.



_Appears in:_
- [EgressConfig](#egressconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `healthCheckPort` _integer_ | HealthCheckPort specifies the port used for EgressIP node health checks. |  | Maximum: 65535 <br />Minimum: 1 <br />Optional: \{\} <br /> |


#### EmptyConfig



EmptyConfig is used for feature gates that have no additional configuration.
Presence of the field (non-nil pointer) enables the gate.



_Appears in:_
- [EgressConfig](#egressconfig)
- [FeatureGates](#featuregates)
- [NetworkPolicyConfig](#networkpolicyconfig)
- [RoutingConfig](#routingconfig)
- [UserDefinedNetworksConfig](#userdefinednetworksconfig)



#### FeatureGates



FeatureGates defines OVN-Kubernetes feature gates.
Each field is a pointer; presence (non-nil) enables the gate, absence disables it.
Simple toggles use *EmptyConfig. Gates with configuration use typed structs.
Related gates are grouped (userDefinedNetworks, routing, egress, networkPolicy);
a child gate is enabled by setting it inside its group.

Only gates that the upstream OVN-Kubernetes Helm chart makes configurable
are included here. Flags the operator always sets are fixed in its
templates and are not user-configurable.



_Appears in:_
- [OVNK8sOperatorSpec](#ovnk8soperatorspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `userDefinedNetworks` _[UserDefinedNetworksConfig](#userdefinednetworksconfig)_ | UserDefinedNetworks enables user-defined network segmentation (UDN)<br />and groups the UDN-related gates and settings. |  | Optional: \{\} <br /> |
| `routing` _[RoutingConfig](#routingconfig)_ | Routing groups the routing-related gates. Setting the group alone<br />enables nothing; enable the individual gates inside it. |  | Optional: \{\} <br /> |
| `egress` _[EgressConfig](#egressconfig)_ | Egress groups the egress-related gates. Setting the group alone<br />enables nothing; enable the individual gates inside it. |  | Optional: \{\} <br /> |
| `networkPolicy` _[NetworkPolicyConfig](#networkpolicyconfig)_ | NetworkPolicy groups the network-policy-related gates. Setting the<br />group alone enables nothing; enable the individual gates inside it. |  | Optional: \{\} <br /> |
| `interconnect` _[InterconnectConfig](#interconnectconfig)_ | Interconnect enables multi-zone interconnect. |  | Optional: \{\} <br /> |
| `hybridOverlay` _[EmptyConfig](#emptyconfig)_ | HybridOverlay enables hybrid overlay networking. |  | Optional: \{\} <br /> |
| `multiExternalGateway` _[EmptyConfig](#emptyconfig)_ | MultiExternalGateway enables multiple external gateway support and the<br />AdminPolicyBasedExternalRoutes CRD. |  | Optional: \{\} <br /> |
| `uplink` _[EmptyConfig](#emptyconfig)_ | Uplink enables uplink and uplink-state tracking (OVN_UPLINK_ENABLE). |  | Optional: \{\} <br /> |
| `persistentIPs` _[EmptyConfig](#emptyconfig)_ | PersistentIPs enables persistent IP addresses for VMs. |  | Optional: \{\} <br /> |
| `dnsNameResolver` _[EmptyConfig](#emptyconfig)_ | DNSNameResolver enables the DNSNameResolver CRD. |  | Optional: \{\} <br /> |
| `networkQoS` _[EmptyConfig](#emptyconfig)_ | NetworkQoS enables the NetworkQoS CRD. |  | Optional: \{\} <br /> |
| `lflowCache` _[LFlowCacheConfig](#lflowcacheconfig)_ | LFlowCache configures OVN logical flow caching. |  | Optional: \{\} <br /> |
| `multicast` _[EmptyConfig](#emptyconfig)_ | Multicast enables multicast support. |  | Optional: \{\} <br /> |
| `serviceTemplateSupport` _[EmptyConfig](#emptyconfig)_ | ServiceTemplateSupport enables the Service template CRD. |  | Optional: \{\} <br /> |


#### GlobalConfig



GlobalConfig defines cluster-wide networking and deployment settings.
Gateway, subnet, and tuning fields are typed rather than using untyped
customVariables, providing schema validation and discoverability.



_Appears in:_
- [OVNK8sOperatorSpec](#ovnk8soperatorspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `k8sAPIServer` _string_ | K8sAPIServer specifies the Kubernetes API server endpoint. |  | Required: \{\} <br /> |
| `podNetwork` _[PodNetworkEntry](#podnetworkentry) array_ | PodNetwork specifies one or more pod network CIDRs with per-node host prefix.<br />Each entry defines a subnet range and the prefix length allocated to each node.<br />Multiple entries support dual-stack or multi-subnet configurations. |  | MaxItems: 16 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `serviceNetwork` _string array_ | ServiceNetwork specifies the service network CIDRs (e.g., ["10.96.0.0/12"]).<br />Single-stack: one entry. Dual-stack: two entries (one IPv4, one IPv6). |  | MaxItems: 2 <br />MinItems: 1 <br />Required: \{\} <br />items:MaxLength: 48 <br /> |
| `mtu` _integer_ | MTU specifies the maximum transmission unit for the overlay network. |  | Minimum: 576 <br />Required: \{\} <br /> |
| `deploymentMode` _string_ | DeploymentMode selects the operator's deployment topology.<br />"standard" — conventional Interconnect deployment<br />"dpu-host" — host-side deployment with an external DPU dataplane<br />"dpu" — DPU-side deployment<br />Mode determines the component composition declared in the operator's<br />embedded config. Use cases differentiate within a mode via<br />componentOverrides and other CR fields. |  | Enum: [standard dpu-host dpu] <br />Required: \{\} <br /> |
| `gatewayMode` _string_ | GatewayMode selects how pod traffic leaves the cluster.<br />"shared" — egress stays in the OVN/OVS datapath and leaves through the<br />node's gateway bridge; hardware offloadable.<br />"local" — egress enters the host networking stack through the<br />management port and follows the host's routes. | shared | Enum: [shared local] <br />Optional: \{\} <br /> |
| `namespace` _string_ | Namespace specifies the target namespace for OVN-Kubernetes resources. |  | Required: \{\} <br /> |
| `imagePullSecrets` _[LocalObjectReference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#localobjectreference-v1-core) array_ | ImagePullSecrets references Secrets in the operator's own namespace<br />that OVN-Kubernetes workload pods use to pull container images.<br />The referenced Secrets are expected to be of type<br />kubernetes.io/dockerconfigjson, as created by<br />`kubectl create secret docker-registry` or equivalent. |  | Optional: \{\} <br /> |
| `propagateImagePullSecrets` _boolean_ | PropagateImagePullSecrets controls whether the controller copies each<br />Secret listed in ImagePullSecrets from the operator's namespace into<br />the workload namespace (ovn-kubernetes ns) on every reconcile.<br />True (default) — the operator performs the copy. The source Secret<br />must exist in the operator's own namespace.<br />False — the cluster admin guarantees that equivalent Secrets exist in the<br />workload namespace (e.g. managed via GitOps or kubectl);<br />the operator leaves them untouched. | true | Optional: \{\} <br /> |
| `v4JoinSubnet` _string_ | V4JoinSubnet specifies the IPv4 join subnet (e.g., "100.64.0.0/16"). |  | Optional: \{\} <br /> |
| `v6JoinSubnet` _string_ | V6JoinSubnet specifies the IPv6 join subnet (e.g., "fd98::/64"). |  | Optional: \{\} <br /> |
| `v4MasqueradeSubnet` _string_ | V4MasqueradeSubnet specifies the IPv4 masquerade subnet used for SNAT<br />plumbing when pod traffic exits to external networks (e.g., "169.254.0.0/17"). |  | Optional: \{\} <br /> |
| `v6MasqueradeSubnet` _string_ | V6MasqueradeSubnet specifies the IPv6 masquerade subnet used for SNAT<br />plumbing when pod traffic exits to external networks. |  | Optional: \{\} <br /> |
| `emptyLbEvents` _boolean_ | EmptyLbEvents enables monitoring of empty load balancer events. |  | Optional: \{\} <br /> |
| `enableCoredump` _boolean_ | EnableCoredump enables coredump collection by setting GOTRACEBACK=crash<br />on OVN-Kubernetes containers. Useful for debugging Go panics. |  | Optional: \{\} <br /> |
| `metrics` _[MetricsConfig](#metricsconfig)_ | Metrics defines metrics settings shared by the deployed OVN-Kubernetes<br />components. |  | Optional: \{\} <br /> |
| `routingTableIDStart` _integer_ | RoutingTableIDStart sets the first routing table ID that ovnkube-node<br />allocates for user-defined networks (routing-table-id-start in<br />ovnkube.conf). When unset, the ovnkube-node default is used. |  | Minimum: 1 <br />Optional: \{\} <br /> |


#### InterconnectConfig



InterconnectConfig defines configuration for the Interconnect feature gate.



_Appears in:_
- [FeatureGates](#featuregates)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `v4TransitSwitchSubnet` _string_ | V4TransitSwitchSubnet specifies the IPv4 transit switch subnet for inter-zone communication. |  | Optional: \{\} <br /> |
| `v6TransitSwitchSubnet` _string_ | V6TransitSwitchSubnet specifies the IPv6 transit switch subnet for inter-zone communication. |  | Optional: \{\} <br /> |


#### LFlowCacheConfig



LFlowCacheConfig defines configuration for the LFlow cache feature gate.



_Appears in:_
- [FeatureGates](#featuregates)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cacheLimit` _integer_ | CacheLimit specifies the maximum number of logical flow cache entries. |  | Optional: \{\} <br /> |
| `cacheLimitKb` _integer_ | CacheLimitKb specifies the maximum logical flow cache size in kilobytes. |  | Optional: \{\} <br /> |


#### MetricsConfig



MetricsConfig defines metrics settings for OVN-Kubernetes.



_Appears in:_
- [GlobalConfig](#globalconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ip` _string_ | IP is the address the standard node and control-plane metrics servers bind<br />to. When empty, the components use their built-in default. |  | Optional: \{\} <br /> |


#### NetworkPolicyConfig



NetworkPolicyConfig groups the network-policy-related feature gates.



_Appears in:_
- [FeatureGates](#featuregates)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `admin` _[EmptyConfig](#emptyconfig)_ | Admin enables the AdminNetworkPolicy CRD. |  | Optional: \{\} <br /> |
| `icmp` _[EmptyConfig](#emptyconfig)_ | ICMP allows ICMP and ICMPv6 traffic to bypass network policy<br />enforcement. |  | Optional: \{\} <br /> |


#### NoOverlayConfig



NoOverlayConfig configures no-overlay mode for the default network.



_Appears in:_
- [UserDefinedNetworksConfig](#userdefinednetworksconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `snat` _boolean_ | Snat enables SNAT for outbound traffic in no-overlay mode.<br />When unset/false, outbound SNAT is disabled. |  | Optional: \{\} <br /> |
| `managedRouting` _[NoOverlayManagedRoutingConfig](#nooverlaymanagedroutingconfig)_ | ManagedRouting lets OVN-Kubernetes manage the pod network routing<br />via BGP. When unset, routing is unmanaged. |  | Optional: \{\} <br /> |


#### NoOverlayManagedRoutingConfig



NoOverlayManagedRoutingConfig configures OVN-Kubernetes-managed BGP routing
for no-overlay networks.



_Appears in:_
- [NoOverlayConfig](#nooverlayconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `asNumber` _integer_ | ASNumber is the AS number used by the BGP speakers on each node for<br />their default VRF. Defaults to 64512. |  | Minimum: 0 <br />Optional: \{\} <br /> |
| `topology` _string_ | Topology configures the BGP peering topology. Defaults to full-mesh. |  | Enum: [full-mesh] <br />Optional: \{\} <br /> |
| `frrNamespace` _string_ | FrrNamespace is the namespace where FRR-K8s FRRConfiguration resources<br />are created. Defaults to frr-k8s-system. |  | Optional: \{\} <br /> |


#### NodePlacement



NodePlacement defines scheduling constraints for component pods.
Uses standard Kubernetes types for full scheduling expressiveness.



_Appears in:_
- [ComponentOverride](#componentoverride)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeSelector` _object (keys:string, values:string)_ | NodeSelector is a selector which must match a node's labels for the pod to be<br />scheduled on that node. |  | Optional: \{\} <br /> |
| `affinity` _[Affinity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#affinity-v1-core)_ | Affinity specifies the pod's scheduling constraints. |  | Optional: \{\} <br /> |
| `tolerations` _[Toleration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#toleration-v1-core) array_ | Tolerations specifies the pod's tolerations. |  | Optional: \{\} <br /> |


#### OVNK8sOperator



OVNK8sOperator is the Schema for the ovnk8soperators API. It defines the
desired state for an OVN-Kubernetes deployment.

The CRD is namespace-scoped and the controller enforces a singleton
instance: exactly one CR named "default" reconciled from the
"ovnk8s-operator-system" namespace. CRs elsewhere are silently ignored by
the manager's cache restriction; CRs with any other name are rejected at
admission by a CEL validation rule. Note that spec.global.namespace names
the namespace where OVN-Kubernetes workloads are deployed, which is
distinct from the operator's own namespace.



_Appears in:_
- [OVNK8sOperatorList](#ovnk8soperatorlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1alpha1` | | |
| `kind` _string_ | `OVNK8sOperator` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[OVNK8sOperatorSpec](#ovnk8soperatorspec)_ | spec defines the desired state of OVNK8sOperator |  | Required: \{\} <br /> |
| `status` _[OVNK8sOperatorStatus](#ovnk8soperatorstatus)_ | status defines the observed state of OVNK8sOperator |  | Optional: \{\} <br /> |


#### OVNK8sOperatorList



OVNK8sOperatorList contains a list of OVNK8sOperator.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1alpha1` | | |
| `kind` _string_ | `OVNK8sOperatorList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[OVNK8sOperator](#ovnk8soperator) array_ |  |  |  |


#### OVNK8sOperatorSpec



OVNK8sOperatorSpec defines the desired state of OVNK8sOperator



_Appears in:_
- [OVNK8sOperator](#ovnk8soperator)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `global` _[GlobalConfig](#globalconfig)_ | Global defines cluster-wide networking and deployment configuration. |  | Required: \{\} <br /> |
| `featureGates` _[FeatureGates](#featuregates)_ | FeatureGates defines OVN-Kubernetes feature gates to enable.<br />Each gate is a typed pointer field. Presence (non-nil) enables the gate;<br />absence (nil/omitted) disables it. |  | Optional: \{\} <br /> |
| `componentOverrides` _[ComponentOverride](#componentoverride) array_ | ComponentOverrides defines optional per-component configuration overrides<br />(replicas, container images, custom variables, node placement, …).<br />Templates use template-level fallbacks for any field left unset here. |  | Optional: \{\} <br /> |
| `testHooks` _[TestHooksSpec](#testhooksspec)_ | TestHooks defines post-deployment test hook execution configuration. |  | Optional: \{\} <br /> |


#### OVNK8sOperatorStatus



OVNK8sOperatorStatus defines the observed state of OVNK8sOperator.



_Appears in:_
- [OVNK8sOperator](#ovnk8soperator)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `version` _string_ | Version is the last OVN-Kubernetes release that completed deployment,<br />workload validation, and all enabled post-deployment test hooks. |  | Optional: \{\} <br /> |
| `targetVersion` _string_ | TargetVersion is the OVN-Kubernetes release currently being reconciled. |  | Optional: \{\} <br /> |
| `upgradeStatus` _[UpgradeStatus](#upgradestatus)_ | UpgradeStatus is the high-level state of the current rollout. It is<br />absent until the first rollout is observed. |  | Enum: [InProgress Completed Failed] <br />Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ | Conditions represent the latest available observations of the resource's<br />state. CNIHealthy reflects reconciliation-time validation, not continuous<br />monitoring. |  | Optional: \{\} <br /> |
| `componentStatus` _[ComponentStatus](#componentstatus) array_ | ComponentStatus shows the status of each deployed component. |  | Optional: \{\} <br /> |


#### PatchType

_Underlying type:_ _string_

PatchType defines the type of patch to apply.

_Validation:_
- Enum: [json merge]

_Appears in:_
- [ComponentPatch](#componentpatch)

| Field | Description |
| --- | --- |
| `json` | JSONPatchType is a JSON Patch (RFC 6902). Precise — uses array indices<br />for list operations. Best for modifying specific containers or env vars.<br /> |
| `merge` | MergePatchType is a JSON Merge Patch (RFC 7386). Deep-merges objects,<br />but replaces entire lists (does not merge by key). Use JSON patch type<br />for list-level precision.<br /> |


#### PodNetworkEntry



PodNetworkEntry defines the pod network CIDR and per-node host prefix.



_Appears in:_
- [GlobalConfig](#globalconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cidr` _string_ | CIDR specifies the pod network address range (e.g., "10.192.0.0/16"). |  | MaxLength: 48 <br />Required: \{\} <br /> |
| `hostPrefix` _integer_ | HostPrefix specifies the subnet prefix length allocated to each node (e.g., 26).<br />Each node gets a /<hostPrefix> subnet from the CIDR for pod IPs. |  | Maximum: 128 <br />Minimum: 1 <br />Required: \{\} <br /> |


#### RouteAdvertisementsConfig



RouteAdvertisementsConfig configures BGP route advertisement.



_Appears in:_
- [RoutingConfig](#routingconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `advertiseDefaultNetwork` _boolean_ | AdvertiseDefaultNetwork advertises the default pod network by creating<br />a RouteAdvertisements object selecting the default network. |  | Optional: \{\} <br /> |


#### RoutingConfig



RoutingConfig groups the routing-related feature gates.



_Appears in:_
- [FeatureGates](#featuregates)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `routeAdvertisements` _[RouteAdvertisementsConfig](#routeadvertisementsconfig)_ | RouteAdvertisements enables BGP route advertisement. |  | Optional: \{\} <br /> |
| `evpn` _[EmptyConfig](#emptyconfig)_ | EVPN enables EVPN protocol support. |  | Optional: \{\} <br /> |


#### TestHooksSpec



TestHooksSpec defines test hook execution configuration.



_Appears in:_
- [OVNK8sOperatorSpec](#ovnk8soperatorspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | Enabled specifies whether test hooks should be executed after deployment. | false |  |
| `tests` _string array_ | Tests specifies the list of test hook file names to execute.<br />If empty, all test hooks in the test-hooks directory will be executed. |  | Optional: \{\} <br /> |
| `timeout` _integer_ | Timeout specifies the timeout for test hook execution in seconds. | 600 | Minimum: 1 <br /> |


#### UpgradeStatus

_Underlying type:_ _string_

UpgradeStatus represents the high-level state of an OVN-Kubernetes rollout.

_Validation:_
- Enum: [InProgress Completed Failed]

_Appears in:_
- [OVNK8sOperatorStatus](#ovnk8soperatorstatus)

| Field | Description |
| --- | --- |
| `InProgress` | UpgradeStatusInProgress indicates that a rollout is in progress.<br /> |
| `Completed` | UpgradeStatusCompleted indicates that a rollout completed successfully.<br /> |
| `Failed` | UpgradeStatusFailed indicates that a rollout failed.<br /> |


#### UserDefinedNetworksConfig



UserDefinedNetworksConfig enables UDN and groups its sub-gates and settings.
Presence of the group (non-nil) enables user-defined network segmentation.



_Appears in:_
- [FeatureGates](#featuregates)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `preconfiguredAddresses` _[EmptyConfig](#emptyconfig)_ | PreconfiguredAddresses enables pre-configured UDN IP addresses. |  | Optional: \{\} <br /> |
| `advertisedIsolationMode` _string_ | AdvertisedIsolationMode configures the UDN pod isolation mode. |  | Enum: [strict loose] <br />Optional: \{\} <br /> |
| `noOverlay` _[NoOverlayConfig](#nooverlayconfig)_ | NoOverlay enables no-overlay mode for the default network. |  | Optional: \{\} <br /> |
| `dynamicUDNAllocation` _[DynamicUDNAllocationConfig](#dynamicudnallocationconfig)_ | DynamicUDNAllocation enables dynamic UDN allocation. |  | Optional: \{\} <br /> |
| `networkConnect` _[EmptyConfig](#emptyconfig)_ | NetworkConnect enables network connectivity features. |  | Optional: \{\} <br /> |


