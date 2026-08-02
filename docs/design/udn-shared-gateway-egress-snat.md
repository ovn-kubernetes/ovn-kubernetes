# UDN Shared Gateway Egress SNAT

## Problem

Primary user defined networks in shared gateway mode need egress traffic to
leave the node with the Kubernetes node IP. The original UDN datapath did this
with two source NAT operations:

```text
pod IP -> UDN gateway-router masquerade IP -> node IP
```

The first NAT is performed by the UDN gateway router. The second NAT is
performed on the shared gateway bridge in conntrack zone 64000 before the
packet exits the node.

This works functionally, but it creates a datapath that is difficult for some
hardware offload implementations to offload because the same connection is
committed with source NAT in both the UDN gateway-router conntrack zone and
the shared gateway bridge conntrack zone.

## Design

For new UDN egress connections in shared gateway mode, the UDN gateway router
SNATs directly to the node IP:

```text
pod IP -> node IP
```

The shared gateway bridge still commits the connection in zone 64000 with the
UDN conntrack mark, but it uses all-zero SNAT instead of a second explicit
node-IP source NAT:

```text
ct(commit,zone=64000,nat(src=0.0.0.0),exec(set_field:<udn mark>->ct_mark))
```

This keeps the reverse-path zone 64000 state without applying another explicit
node-IP translation.

## Upgrade Compatibility

Existing connections can be using the old gateway-router NAT entry:

```text
external_ip=<UDN gateway-router masquerade IP>
logical_ip=<UDN subnet>
```

Those connections need the old row to remain present because OVN generates the
unSNAT path from the NAT row external IP. If the row is replaced in place with
the node IP, replies to the old masquerade IP can no longer be unSNATed.

To avoid that disruption, ovnkube keeps the legacy row and adds a second,
versioned row for new traffic:

```text
external_ip=<node IP>
logical_ip=<UDN subnet>
external_ids:k8s.ovn.org/udn-egress-snat-version=v2
```

Rows without the version external ID are treated as v1. The v2 row has a
higher NAT priority and a non-empty NAT match. OVN only uses NAT priority when
`NAT.match` is set, so unconditioned UDN subnet SNATs use a family match
(`ip4` or `ip6`) on the v2 row.

During upgrade:

1. Old connections continue to use the v1 masquerade-IP NAT state.
2. Reply traffic to the old masquerade IP still has a matching unSNAT flow.
3. New connections match the higher-priority v2 node-IP NAT row.
4. The bridge supports both source addresses:
   - masquerade IP uses the legacy `nat(src=<node IP>)` bridge flow.
   - node IP uses the v2 all-zero SNAT bridge flow.

## Cleanup

The v1 row is intentionally not deleted by this migration. It is required for
existing long-lived connections and is harmless once those connections age out
because the v2 row has higher priority for new traffic.

Future cleanup can remove unversioned UDN egress SNAT rows after a release
boundary where all nodes are known to have drained old conntrack state.

## Test Coverage

Unit coverage verifies:

- shared gateway UDN gateway routers contain both v1 and v2 subnet SNAT rows.
- the v2 row is tagged with the SNAT version external ID.
- the v2 row has a non-empty match and higher priority.
- the shared gateway bridge node-IP path uses all-zero SNAT in zone 64000.

E2E upgrade coverage should keep a UDN pod and an active egress connection
alive across an ovnkube-node restart or rollout, then verify that the old pod
can still reach the external target after the new v2 SNAT row is installed.
