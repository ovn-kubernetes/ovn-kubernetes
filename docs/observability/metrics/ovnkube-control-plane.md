# OVN-Kubernetes control plane metrics

Base metrics are registered by every cluster-manager instance. Functional
metrics are registered after the instance wins leader election; they may
therefore be absent from a standby instance.

## Cluster Manager Metrics

All metrics in this section are rooted at `ovnkube_clustermanager_`.

### Base metrics

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `build_info` | Gauge | `version`, `revision`, `branch`, `build_user`, `build_date`, `goversion` | Build information. The value is always `1`. |
| `leader` | Gauge | None | Whether this instance is the leader: `1` for leader and `0` otherwise. |
| `ready_duration_seconds` | Gauge | None | Time for the cluster manager to become ready. |

### Network and subnet metrics

These metrics are registered on the leader.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `num_v4_host_subnets` | Gauge | `network_name` | Total number of possible IPv4 host subnets per network. |
| `num_v6_host_subnets` | Gauge | `network_name` | Total number of possible IPv6 host subnets per network. |
| `allocated_v4_host_subnets` | Gauge | `network_name` | Number of currently allocated IPv4 host subnets per network. |
| `allocated_v6_host_subnets` | Gauge | `network_name` | Number of currently allocated IPv6 host subnets per network. |
| `user_defined_networks` | Gauge | `role`, `topology` | Number of UserDefinedNetwork resources. |
| `cluster_user_defined_networks` | Gauge | `role`, `topology`, `transport` | Number of ClusterUserDefinedNetwork resources. |
| `cluster_user_defined_network_condition` | Gauge | `name`, `condition`, `status` | CUDN status condition. Both `status="true"` and `status="false"` are emitted; the active status is `1`. |
| `udn_nodes_rendered` | Gauge | `network_name` | Number of nodes on which a UDN or CUDN is rendered. Registered only when dynamic UDN allocation is enabled. |
| `route_advertisement_condition` | Gauge | `name`, `condition`, `status` | RouteAdvertisements status condition. Registered only when route advertisements are enabled. |
| `vtep_condition` | Gauge | `name`, `condition`, `status` | VTEP status condition. Registered only when EVPN is enabled. |

### Egress IP metrics

These metrics are registered only when EgressIP is enabled.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `num_egress_ips` | Gauge | None | Number of defined egress IP addresses. |
| `egress_ips_node_unreachable_total` | Counter | None | Number of times assigned egress IPs were unreachable. |
| `egress_ips_rebalance_total` | Counter | None | Number of assigned egress IPs moved to a different node. |

### Scale metrics

These metrics are registered only when `--metrics-enable-scale` is enabled.
The `name` label on UDN histograms identifies the network.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `udn_update_node_annotation_duration_seconds` | Histogram | `name` | Time spent updating node annotations during UDN network allocation. |
| `udn_nad_sync_duration_seconds` | Histogram | `name` | Time spent syncing NetworkAttachmentDefinitions during UDN reconciliation. |
| `workqueue_depth` | Gauge | `name` | Current workqueue depth. |
| `workqueue_adds_total` | Counter | `name` | Items added to a workqueue. |
| `workqueue_queue_duration_seconds` | Histogram | `name` | Time an item remains queued before processing. |
| `workqueue_work_duration_seconds` | Histogram | `name` | Time spent processing an item. |
| `workqueue_unfinished_work_seconds` | Gauge | `name` | Total in-progress work not yet observed by the work-duration metric. |
| `workqueue_longest_running_processor_seconds` | Gauge | `name` | Runtime of the longest-running workqueue processor. |
| `workqueue_retries_total` | Counter | `name` | Workqueue retries. |

## Shared OVN-Kubernetes Metrics

All metrics in this section are rooted at `ovnkube_`.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `resource_retry_failures_total` | Counter | None | Number of Kubernetes resources that reached the maximum reconciliation retry limit and are no longer processed. This family is shared with ovnkube-controller and ovnkube-node. |
