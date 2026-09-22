# Open vSwitch metrics

OVN-Kubernetes collects metrics from ovs-vswitchd and the Open_vSwitch
database. These metrics are available on Linux when OVS metrics export is
enabled.

OVS `coverage/show` values are exposed as Prometheus gauges even though the
underlying events are cumulative. Daemon restarts can reset them. A failed
collection can leave the previous value in place, while an event that has
never occurred is reported as zero.

## Open vSwitch Metrics

All metrics in this section are rooted at `ovs_`.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `build_info` | Gauge | `version` | Open vSwitch version information. The value is always `1`. |

## OVS Vswitchd Metrics

All metrics in this section are rooted at `ovs_vswitchd_`.

### Datapath Metrics

Several cumulative datapath statistics are gauges because they are snapshots
read from OVS.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `dp_total` | Gauge | None | Number of datapaths. |
| `dp` | Gauge | `datapath`, `type` | Datapath identity. Each present datapath has value `1`. |
| `dp_if_total` | Gauge | `datapath` | Ports connected to a datapath. |
| `dp_flows_total` | Gauge | `datapath` | Current flows in a datapath. |
| `dp_flows_lookup_hit` | Gauge | `datapath` | Packets that matched an existing datapath flow. |
| `dp_flows_lookup_missed` | Gauge | `datapath` | Packets that missed existing datapath flows and required userspace processing. |
| `dp_flows_lookup_lost` | Gauge | `datapath` | Missed packets dropped before reaching userspace. |
| `dp_packets_total` | Gauge | `datapath` | Sum of datapath lookup hits and misses. |
| `dp_masks_hit` | Gauge | `datapath` | Masks visited while matching packets. |
| `dp_masks_total` | Gauge | `datapath` | Masks in a datapath. |
| `dp_masks_hit_ratio` | Gauge | `datapath` | Average masks visited per packet. |

### Bridge Metrics

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `bridge_total` | Gauge | None | Number of OVS bridges. |
| `bridge` | Gauge | `bridge` | Bridge identity. Each present bridge has value `1`. |
| `bridge_ports_total` | Gauge | `bridge` | Ports on a bridge. |
| `bridge_flows_total` | Gauge | `bridge` | Current OpenFlow flows on a bridge. |

### Interface Metrics

These metrics do not provide per-interface statistics. The error and reset
families aggregate cumulative OVSDB interface statistics across all interfaces
on the node and are represented as gauges.

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `interfaces_total` | Gauge | None | Number of Open vSwitch interfaces. |
| `interface_resets_total` | Gauge | None | Link-state changes observed across interfaces. |
| `interface_rx_dropped_total` | Gauge | None | Received packets dropped across interfaces. |
| `interface_tx_dropped_total` | Gauge | None | Transmitted packets dropped across interfaces. |
| `interface_rx_errors_total` | Gauge | None | Receive errors across interfaces. |
| `interface_tx_errors_total` | Gauge | None | Transmit errors across interfaces. |
| `interface_collisions_total` | Gauge | None | Transmit collisions across interfaces. |
| `interface_up_wait_seconds_total` | Counter | None | Cumulative time spent waiting for pod OVS interfaces to become available. |

### Thread and Hardware-Offload Metrics

| Name | Type | Labels | Description |
| --- | --- | --- | --- |
| `handlers_total` | Gauge | None | Number of handler threads that process datapath upcalls. |
| `revalidators_total` | Gauge | None | Number of threads that revalidate datapath flows. |
| `hw_offload` | Gauge | None | Whether hardware flow offload is enabled: `1` for enabled and `0` otherwise. |
| `tc_policy` | Gauge | None | TC offload policy: `0` none, `1` `skip_sw`, and `2` `skip_hw`. |

### Coverage Metrics

| Name | Type | Description |
| --- | --- | --- |
| `netlink_sent` | Gauge | Netlink messages sent to the kernel. |
| `netlink_received` | Gauge | Netlink messages received from the kernel. |
| `netlink_recv_jumbo` | Gauge | Netlink messages larger than the allocated receive buffer. |
| `netlink_overflow` | Gauge | Netlink messages dropped because the daemon buffer overflowed. |
| `rconn_sent` | Gauge | Messages sent on reliable OpenFlow connections. |
| `rconn_queued` | Gauge | Messages queued on reliable OpenFlow connections. |
| `rconn_discarded` | Gauge | Queued messages discarded during reconnection. |
| `rconn_overflow` | Gauge | Messages dropped because the reliable connection queue overflowed. |
| `vconn_open` | Gauge | Attempts to connect to an OpenFlow device. |
| `vconn_sent` | Gauge | Messages sent to OpenFlow devices. |
| `vconn_received` | Gauge | Messages received from OpenFlow devices. |
| `pstream_open` | Gauge | Passive connections opened for remote peers. |
| `stream_open` | Gauge | Attempts to connect to a remote peer. |
| `txn_success` | Gauge | Successful OVSDB transactions. |
| `txn_error` | Gauge | OVSDB transaction errors. |
| `txn_uncommitted` | Gauge | Uncommitted OVSDB transactions. |
| `txn_unchanged` | Gauge | OVSDB transactions that made no database change. |
| `txn_incomplete` | Gauge | Incomplete OVSDB transactions that required retry. |
| `txn_aborted` | Gauge | Aborted OVSDB transactions. |
| `txn_try_again` | Gauge | OVSDB transactions that failed and requested retry. |
| `dpif_port_add` | Gauge | Netdevs added as datapath interface ports. |
| `dpif_port_del` | Gauge | Netdevs removed from datapath interface ports. |
| `dpif_flow_flush` | Gauge | Datapath flow flushes. |
| `dpif_flow_get` | Gauge | Datapath flow retrievals. |
| `dpif_flow_put` | Gauge | Flows added to the datapath. |
| `dpif_flow_del` | Gauge | Flows deleted from the datapath. |
| `dpif_execute` | Gauge | OpenFlow actions executed in userspace for the datapath. This aggregates executions with and without help. |
| `bridge_reconfigure` | Gauge | OVS bridge reconfigurations. |
| `xlate_actions` | Gauge | OpenFlow actions translated into datapath actions. |
| `xlate_actions_oversize` | Gauge | Translated datapath actions too large for a netlink attribute. |
| `xlate_actions_too_many_output` | Gauge | Translations with more output actions than the kernel can handle reliably. |
| `packet_in` | Gauge | Packet-ins handled on behalf of the kernel datapath. |
| `packet_in_drop` | Gauge | Packet-ins dropped because of resource constraints. |
| `ofproto_dpif_expired` | Gauge | Flows removed because of timeout, delete, or eviction. |
| `ofproto_flush` | Gauge | Flushes of all flows from ofproto flow tables. |
| `ofproto_packet_out` | Gauge | Packets injected into the kernel datapath by a controller. |
| `ofproto_recv_openflow` | Gauge | OpenFlow messages handled. |
| `ofproto_reinit_ports` | Gauge | Reinitializations of all OpenFlow ports. |
| `upcall_flow_limit_kill` | Gauge | Events where datapath flow count reached twice the dynamic flow limit. |
| `upcall_flow_limit_hit` | Gauge | Events where the datapath reached the dynamic flow limit. |
