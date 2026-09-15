# Uplink API reference

`Uplink` and `UplinkState` are cluster-scoped resources in `k8s.ovn.org/v1alpha1`.
See [Uplinks for UserDefinedNetworks](../features/user-defined-networks/uplinks.md)
for configuration examples, discovery behavior and gateway-limit rationale.

## Uplink

| Field | Description | Validation |
| --- | --- | --- |
| `spec.nodeConfigs` | Atomic list mapping nodes to uplink configurations. | Required; 1–64 entries. |
| `spec.nodeConfigs[].type` | Node configuration type. | Required; `OVSBridge`. |
| `spec.nodeConfigs[].nodeSelector` | Kubernetes label selector; an empty selector matches all nodes. A node may match at most one entry. | Required. |
| `spec.nodeConfigs[].hostInterfaceName` | Host interface carrying the gateway L3 identity. | Required; 1–15 characters, without `/` or whitespace. |
| `status.conditions` | Aggregate state using Kubernetes conditions, keyed by condition type. | Optional. |

## UplinkState

The controllers maintain one state per node and Uplink. Status is written
through the main resource endpoint; there is no separate status subresource.

| Field | Description | Validation |
| --- | --- | --- |
| `spec.uplinkName` | Associated Uplink name. | Required; immutable; 1–253 characters. |
| `spec.nodeName` | Associated node name. | Required; immutable; 1–253 characters. |
| `status.type` | Matched node configuration type. | Optional; `OVSBridge`. |
| `status.hostInterfaceName` | Selected host interface. | Optional; 1–15 characters, without `/` or whitespace. |
| `status.ovsBridge.name` | Resolved OVS bridge. | Optional; 1–15 characters, without `/` or whitespace. |
| `status.macAddress` | Gateway interface MAC address. | Optional; six colon-separated hexadecimal octets. |
| `status.hostFunction.pfID` | Physical function index. | Required when `hostFunction` is present; nonnegative integer. |
| `status.hostFunction.vfID` | Virtual function index; absent for a physical function. | Optional; nonnegative integer. |
| `status.ipAddresses` | Host gateway addresses with CIDR prefixes. | Optional atomic list; at most 2 valid CIDRs, each at most 64 characters. |
| `status.defaultGateways` | Discovered default-route next-hop IPs through the selected host interface. | Optional atomic list; at most **256 total across IPv4 and IPv6**, each a valid IP address of at most 64 characters. |
| `status.conditions` | Node discovery and gateway state using Kubernetes conditions, keyed by condition type. | Optional. |

Discovery selects the lowest-metric default routes per IP family through the
selected interface, keeps only the next hops with the highest weight within
each family, then deduplicates and sorts them. Weights are not represented, so
lighter next hops of an unequal-weight multipath route are omitted. Discovery
errors retry with exponential backoff capped at 30 seconds per item. An empty
gateway list is valid.
More than 256 distinct gateways causes discovery to report
`GatewayInfoUnavailable` and retry without publishing a subset. The limit is
per node and Uplink, and is an API bound rather than a guarantee of forwarding
or hardware offload capacity. It accommodates 128 next hops per family without
imposing separate per-family quotas.
