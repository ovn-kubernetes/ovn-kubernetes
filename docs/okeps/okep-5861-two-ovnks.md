OKEP-5861: Dual OVN-Kubernetes Instances on DPU Nodes

Problem Statement

ovn-kubernetes cannot currently run one instance in DPU mode and another in full mode on the same node.

This blocks use cases where a DPU card needs to run two ovn-kubernetes instances simultaneously:
 - one in DPU mode to manage networking for pods on the host (which belongs to a separate Kubernetes cluster)
 - another in full mode for pods running locally on the DPU itself.
​

Goals and Non-Goals

Goals

Support two independent ovn-kubernetes instances on the same node:

- One in **DPU mode**: manages networking for pods on behalf of the host (which is also part of a separate Kubernetes cluster).

- One in **full mode**: manages networking for pods running directly on the DPU itself.
​

Avoid conflicts over configuration paths, sockets, and OVS/OVN resources.

Keep single-instance deployments fully compatible.


Non-Goals

Running more than two instances per node.

Redesigning DPU support or defining new multi-tenant abstractions.

Providing a full management/operator framework for dual instances.
​

High-Level Design

Introduce an instance identifier (e.g., `OVN_K8S_INSTANCE_ID`) to isolate each instance's file paths and avoid conflicts:

Config: /etc/ovn-kubernetes/$INSTANCE_ID

Runtime: /var/run/ovn-kubernetes/$INSTANCE_ID

Logs: /var/log/ovn-kubernetes/$INSTANCE_ID

When `INSTANCE_ID` is unset or empty, paths remain unchanged (e.g., `/etc/ovn-kubernetes/`), preserving backward compatibility with single-instance deployments.

Deploy two DaemonSets on DPU nodes:

ovnkube-node-dpu: OVN_K8S_NODE_MODE=dpu, OVN_K8S_INSTANCE_ID=dpu

ovnkube-node-full: OVN_K8S_NODE_MODE=full, OVN_K8S_INSTANCE_ID=dpu-full

Separate ownership of OVS/OVN objects via naming or external_ids, so each instance reconciles only its own ports and logical ports.
​
​
Detailed Design

Instance Identity
New env var: `OVN_K8S_INSTANCE_ID` (optional, defaults to empty string).

If unset, behavior and paths remain as today for backward compatibility.

If set, all file paths, sockets, and PID files are derived from the instance ID.
​

OVS/OVN 

Shared OVS on the DPU may serve both instances.

The relevant bits for OVN inside the OVSDB are the following external_ids:

```
external_ids:ovn-remote -> external_ids:ovn-remote-<OVN_K8S_INSTANCE_ID>=
external_ids:ovn-encap-type -> external_ids:ovn-encap-type-<OVN_K8S_INSTANCE_ID>=
external_ids:ovn-encap-ip -> external_ids:ovn-encap-ip-<OVN_K8S_INSTANCE_ID>=
external_ids:ovn-bridge -> external_ids:ovn-bridge-<OVN_K8S_INSTANCE_ID>=
external-ids:ovn-bridge-datapath-type -> external_ids:ovn-bridge-datapath-type-<OVN_K8S_INSTANCE_ID>=
```

Each instance will generate the right values. To note here the ovn-bridge-<OVN_K8S_INSTANCE_ID>=, should have a value of max 15 chars.

The `OVN_K8S_INSTANCE_ID` will also be sent to ovn-controller via the argument:
`--ovn-controller-system-id=STRING          set chassis name (system-id) for ovn-controller (passed as -n)` once the OVN stack will be brought up.


Documentation and Testing

Extend DPU documentation with:

Dual-instance overview.

Example DaemonSets and CNI snippets.
​

Add CI/e2e coverage for:

A node running two ovn-kubernetes instances side by side (both in full mode), verifying pod connectivity for each instance independently.
