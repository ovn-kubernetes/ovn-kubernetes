# OKEP-6224: DHCP IPAM Support for Localnet Networks

* Issue: [#6224](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6224)

## Problem Statement

OVN-Kubernetes currently supports only two IPAM modes for localnet CUDN: `Enabled` (OVN assigns IPs from configured subnets) and `Disabled` (no IP, user configures manually). Currently, obtaining an IP address from an external DHCP server requires a DHCP client to be running inside the pod or VM and executed from its network namespace. OVN-Kubernetes has no mechanism to delegate IP assignment to an external DHCP server on behalf of the workload during the CNI lifecycle. This OKEP introduces a single new `DHCP` IPAM mode that delegates IP assignment to external DHCP infrastructure.

## Goals

* Enable pods on localnet secondary networks to obtain IP addresses from an external DHCP server, by delegating lease acquisition and lifecycle management to the CNI DHCP plugin daemon, which will in turn report the pod IP to OVN-Kubernetes.
* Enable VMs (including VFIO passthrough) on localnet secondary networks to obtain an initial IP via DHCP during CNI ADD, and enable OVN-Kubernetes to detect the IP during CNI ADD.
* Enable OVN-Kubernetes to use the DHCP-learned pod IPs and annotate the pod information during CNI ADD time, in order to enable features like MultiNetworkPolicy and Network QoS that rely on the pod IP.


## Non-Goals

* Implementing a DHCP server within OVN-Kubernetes. This feature delegates to external DHCP infrastructure.
* Providing automatic discovery or validation that the external DHCP server's address pool is consistent with the configured `subnets` range. The administrator is responsible for ensuring alignment.
* Implementing DHCP lease lifecycle management (renewal, release) within OVN-Kubernetes itself. For pods, lease maintenance is delegated to the upstream CNI DHCP plugin daemon. For KubeVirt VMs, OVN-K performs a one-shot lease acquisition to discover the IP, but ongoing renewal and release are the responsibility of the VM's own DHCP client.

## Future Goals

* Extend DHCP IPAM support to Layer 3 topologies.
* Supporting DHCP IPAM in unprivileged ovnkube-node mode.

## Introduction

In environments where IP address management is handled by external DHCP servers, Kubernetes workloads attached to physical networks via OVN-Kubernetes localnet topology need to obtain their IP addresses from this existing DHCP infrastructure rather than relying on Kubernetes-native IPAM.

A single `DHCP` IPAM mode covers both pods and VMs. OVN-Kubernetes differentiates the two at CNI ADD time based on pod ownership and selects the appropriate lease-handling behavior:

1. **Pods/Containers:** The pod might not have a DHCP client of its own. OVN-Kubernetes must perform the DHCP handshake, apply the IP to the pod's interface, and ensure the DHCP lease is renewed for the pod's lifetime. This is achieved by delegating to the standard [CNI DHCP IPAM plugin](https://www.cni.dev/plugins/current/ipam/dhcp/), which runs a daemon that maintains leases via a background goroutine inside the container's network namespace.

2. **KubeVirt VMs:** The VM has its own operating system with a DHCP client (e.g., dhclient via cloud-init). OVN-Kubernetes performs a one-shot DHCP lease acquisition during CNI ADD only to discover the IP address that the DHCP server will assign to this MAC, reports it in the CNI result, and does no lease maintenance. This IP is reported so that multus can annotate the pod and ovnkube-controller can program the OVN NBDB. The VM's own DHCP client takes over lease management once it boots. For VFIO passthrough VMs, the DHCP lease acquisition is performed during a temporary driver handoff window where the VF is briefly bound to the kernel driver, making a netdev available.

OVN-Kubernetes distinguishes the two cases with the existing `kubevirt.IsPodOwnedByVirtualMachine` helper, which returns true when the pod carries the `vm.kubevirt.io/name` label that KubeVirt sets on every virt-launcher (VM) pod.

### CNI DHCP Plugin Architecture

The CNI DHCP plugin consists of two components:

```text
/opt/cni/bin/dhcp              (short-lived binary, exec'd per ADD/DEL)
       │
       │ RPC over unix socket
       ▼
/opt/cni/bin/dhcp daemon       (long-running process, maintains leases)
       │
       │ raw socket inside container netns
       ▼
physical network ←→ DHCP server
```

The dhcp binary is a thin RPC client. The daemon enters the container's network namespace, opens a raw socket on the pod's interface, and performs the DHCP handshake. A dedicated goroutine per lease remains inside the container netns for the pod's lifetime, handling renewals at T1 (50% of lease time) and T2 (87.5%).

### OVN-Kubernetes CNI Architecture

OVN-Kubernetes uses a split CNI architecture:

```text
kubelet → CNI shim → HTTP POST over unix socket → CNI server (ovnkube-node, long-lived)
```

The shim is a thin pass-through. The server running in ovnkube-node has access to OVS, OVN, Kubernetes API, and the container network namespace. DHCP delegation is performed on the server side to keep the shim minimal and because the server already handles all interface configuration.

## User-Stories/Use-Cases

### Story 1: Pod with DHCP on Localnet

As a cluster administrator, I want pods attached to a physical network to automatically receive IP addresses from the existing enterprise DHCP server,
so that IP allocation integrates seamlessly with the data center's DHCP infrastructure without requiring manual configuration or a separate Kubernetes-managed IP pool.

### Story 2: VM with VFIO Passthrough on Localnet

For KubeVirt VMs with SR-IOV VFIO passthrough, I want the VM's DHCP-assigned IP to be discovered by OVN-Kubernetes during CNI ADD,
so that the IP is reported in the CNI result and programmed on the OVN NBDB logical switch port, even though the VF is passed directly to the VM.

### Story 3: Pods and VMs Coexisting on the Same Physical Network

As a cluster administrator, I want pods and VMs to share a single localnet network and obtain IPs from the same external DHCP pool,
so that I configure one network and OVN-Kubernetes automatically does the right thing for each workload type.

### Story 4: VM on Localnet over KubeVirt Bridge Binding with Live Migration

As a KubeVirt VM owner, I want the guest's localnet interface attached over KubeVirt's bridge binding to be configured by the external DHCP server seamlessly, and the VM to retain its network settings (including MAC and IP addresses) when it is live-migrated to another node,
so that the VM keeps the same address and connectivity across migration without any manual reconfiguration.

## Proposed Solution

### API Details

#### New IPAM Mode

Extend the `IPAMMode` enum with a single new value:

```go
// +kubebuilder:validation:Enum=Enabled;Disabled;DHCP
type IPAMMode string

const (
    IPAMEnabled  IPAMMode = "Enabled"
    IPAMDisabled IPAMMode = "Disabled"
    IPAMDHCP     IPAMMode = "DHCP"
)
```

| Mode | Description | Lease Management | Subnets Field |
|------|-------------|-----------------|---------------|
| `Enabled` | OVN-K allocates IPs from subnets (existing) | N/A | Required |
| `Disabled` | No IP assignment (existing) | N/A | Forbidden |
| `DHCP` | External DHCP; behavior selected at runtime by workload type | Pods: CNI DHCP daemon. VMs: guest OS | Optional |

#### Field Descriptions

- **`DHCP`**: OVN-Kubernetes delegates IP assignment to the external DHCP server and selects the lease-handling behavior at CNI ADD time based on the workload type:
  - **Pods** (not owned by a VirtualMachineInstance): OVN-Kubernetes delegates to the CNI DHCP IPAM plugin daemon, which performs the full DHCP handshake and maintains the lease (renewals at T1/T2) for the pod's lifetime. The lease is released on CNI DEL.
  - **KubeVirt VMs** (owned by a VirtualMachineInstance): OVN-Kubernetes performs a one-shot DHCP lease acquisition during CNI ADD to discover the IP address assigned by the external DHCP server. No lease maintenance is performed; the VM runs its own DHCP client and manages the lease lifecycle. The discovered IP is reported in the CNI result for multus annotation and NBDB programming.

#### CRD Validation Rules

```yaml
# Localnet topology validations:
- message: "DHCP ipam.mode is only supported for Localnet Secondary networks"
  rule: '!has(self.ipam) || !has(self.ipam.mode) || self.ipam.mode != "DHCP" || self.role == "Secondary"'

# Layer2 topology validation:
- message: "DHCP ipam.mode is not supported for Layer2 topology, use Localnet topology instead"
  rule: '!has(self.ipam) || !has(self.ipam.mode) || self.ipam.mode != "DHCP"'
```

#### Example ClusterUserDefinedNetwork

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: localnet-dhcp-ipam
spec:
  namespaceSelector:
    matchLabels:
      localnet: "true"
  network:
    topology: Localnet
    localnet:
      role: Secondary
      physicalNetworkName: physnet
      subnets:
      - "172.18.0.0/16"
      ipam:
        mode: DHCP
```

#### Generated NAD Config

The UDN controller generates the NAD automatically. The `ipam` field is set directly in the OVN plugin config (single-plugin format, not a confList). The generated NAD is identical for pods and VMs — there is only one config, and OVN-Kubernetes selects the pod-vs-VM behavior at runtime:

```json
{
  "cniVersion": "1.0.0",
  "name": "localnet-dhcp",
  "type": "ovn-k8s-cni-overlay",
  "topology": "localnet",
  "role": "secondary",
  "physicalNetworkName": "physnet",
  "subnets": "172.18.0.0/16",
  "ipam": { "type": "dhcp" }
}
```

#### Pod Network Annotation Changes

The `k8s.ovn.org/pod-networks` annotation is also consumed by components outside ovn-kubernetes, most notably KubeVirt cloud-init sidecars that build the guest network configuration before a VM boots. To let those components know that an interface should be configured via DHCP inside the guest, this OKEP introduces a new optional field, `ipam_mode`, on each per-network entry. It is set **only for KubeVirt VM pods on a `DHCP` secondary network**.

* `ipam_mode: "dhcp"` is set by ovnkube-node during CNI ADD only when the corresponding NAD has `ipam.type: "dhcp"` **and** the pod is a KubeVirt VM (`kubevirt.IsPodOwnedByVirtualMachine` is true).
* Absent for plain pods (their lease is fully managed by the CNI DHCP daemon, so no guest-side configuration is needed) and for all non-DHCP modes (`Enabled`, `Disabled`).

**Why this field is needed.** The `ipam_mode` field gives KubeVirt cloud-init sidecars an explicit signal from OVN-Kubernetes to emit DHCP configuration for the interface in the guest cloud-init network config, so the guest runs its own DHCP client over the VF and can renew/re-acquire its lease independently.

**Example annotation** (`DHCP` VFIO secondary on a VM pod):

```json
{
  "default": {
    "ip_addresses": ["10.220.2.18/24"],
    "mac_address": "0a:58:0a:dc:02:12",
    "gateway_ips": ["10.220.2.1"],
    "role": "primary"
  },

  "default/ovn-localnet-dhcp-vfio": {
    "ip_addresses": ["10.1.192.213/24"],
    "mac_address": "02:00:00:00:20:08",
    "gateway_ips": ["10.1.192.1"],
    "mtu": "1500",
    "role": "secondary",
    "ipam_mode": "dhcp"
  }
}
```

### Implementation Details

#### Workload Differentiation at CNI ADD

There is a single `ipam.type: "dhcp"` NAD config. ovnkube-node selects the lease-handling behavior at CNI ADD time using the existing `kubevirt.IsPodOwnedByVirtualMachine(pod)` helper, which returns true when the pod carries the `vm.kubevirt.io/name` label that KubeVirt sets on every virt-launcher (VM) pod.

| Workload | Detection | DHCP behavior |
|----------|-----------|---------------|
| Normal pod (veth) | `IsPodOwnedByVirtualMachine == false` | Delegate to the DHCP CNI plugin daemon, full lease maintenance (renew at T1/T2, release on DEL) |
| KubeVirt VM - VFIO passthrough | VM-owned, VF bound to `vfio-pci` | Temporary driver handoff, one-shot DORA, `AddrAdd`/`RouteAdd` on the temp netdev, rebind to `vfio-pci` |
| KubeVirt VM - non-VFIO (`l2bridge`/`managedTap`) | VM-owned, no passthrough device | One-shot DORA on the pod netns netdev; **no** `AddrAdd`/`RouteAdd` on the pod interface |

#### End-to-End Flow (Pods case - delegated to CNI DHCP plugin)

The following describes the detailed call chain when a pod (not owned by a VirtualMachineInstance) is created with DHCP IPAM:

1. **Multus Delegation:** When a pod is created, kubelet invokes Multus, which reads the NetworkAttachmentDefinition (NAD). Multus identifies the CNI plugin as `ovn-k8s-cni-overlay` and executes the OVN-K CNI shim binary with the NAD config on stdin and CNI environment variables (`CNI_COMMAND=ADD`, `CNI_NETNS`, `CNI_IFNAME`, etc.).

2. **Shim Pass-Through:** The OVN-K CNI shim binary (`/opt/cni/bin/ovn-k8s-cni-overlay`) does not perform any networking itself. It packages the CNI config and environment into a JSON request and forwards it via HTTP POST over a Unix socket (`/var/run/ovn-kubernetes/cni/ovn-cni-server.sock`) to the ovnkube-node daemon running on the same node.

3. **Interface Setup (ovnkube-node Server Side):** The ovnkube-node CNI server receives the request, parses the NAD config, and calls `ConfigureInterface()` which sets up the pod interface, either a veth pair for standard pods, or a VF moved into the pod network namespace with its VF representor on the host side when SR-IOV is present. It configures the MAC address and attaches the host-side interface (veth-end or VF representor) to OVS `br-int` via `ConfigureOVS()`. Since IPAM is DHCP, `setupNetwork()` skips IP configuration.

4. **DHCP Delegation (Server Side):** ovnkube-node detects `ipam.type == "dhcp"` in the NAD config and, because the pod is not owned by a VirtualMachineInstance, executes the standard CNI DHCP plugin binary (`/opt/cni/bin/dhcp`) with the appropriate CNI arguments (netns path, interface name, container ID). This delegates IP allocation entirely to the external DHCP IPAM plugin following the same delegation pattern used by the SR-IOV CNI and the CNI bridge plugin.

5. **DHCP Lease Acquisition:** The DHCP plugin binary connects to the DHCP daemon (a long-running process on the node, listening on `/run/cni/dhcp.sock`) via RPC. The daemon enters the pod's network namespace, performs a full DHCP DORA exchange (Discover, Offer, Request, Ack) with the external DHCP server on the physical network, and returns the assigned IP, subnet mask, gateway, and routes.

6. **Result Assembly, Pod Annotation, and Return:** ovnkube-node applies the DHCP result to the pod interface inside the network namespace (`AddrAdd` for the IP, `RouteAdd` for static routes and default gateway), and patches the pod with the DHCP-learned IP via the `k8s.ovn.org/pod-networks` annotation. ovnkube-controller observes this annotation and programs the corresponding logical switch port in the OVN NBDB; ovnkube-node then merges the DHCP-assigned IPs and routes into the CNI Result and returns it back through the chain: ovnkube-node server -> HTTP response -> shim -> stdout -> Multus. Multus writes the result into the pod's `k8s.v1.cni.cncf.io/network-status` annotation.

7. **Lease Lifecycle:** The DHCP daemon manages lease renewal in the background for the lifetime of the pod. On pod deletion, kubelet calls Multus with CNI DEL, which follows the same path (Multus -> shim -> ovnkube-node server), and ovnkube-node executes the DHCP plugin with CNI DEL, triggering the daemon to release the lease back to the DHCP server before tearing down the interface and removing the OVS port.


The following diagram illustrates the complete pod (delegated) flow, showing the interaction between Multus, the CNI shim, ovnkube-node, the DHCP plugin, the DHCP daemon, and the external DHCP server:

![CNI DHCP Allocation Flow](../images/CNI%20DHCP%20Allocation%20Flow-2026-04-14-000110.png)

#### End-to-End Flow (KubeVirt VMs — for both VFIO and non VFIO case)

For KubeVirt VMs, OVN-Kubernetes performs a one-shot DHCP discovery instead of delegating to the CNI DHCP plugin daemon, branching on the VFIO vs non-VFIO binding as summarized in the table above. In the non-VFIO (`l2bridge`/`managedTap`) case, OVN-Kubernetes only reports the discovered IP via the pod annotation and must not configure it on the pod interface. Configuring the IP on the pod interface would cause KubeVirt to start an in-pod DHCP server (dnsmasq), which would conflict with the external DHCP server and prevent the guest from renewing its lease externally.

The following describes the detailed call chain when a VM is created with DHCP IPAM:

1. **Multus Delegation:** When a VM pod is created, kubelet invokes Multus, which reads the NetworkAttachmentDefinition (NAD). Multus identifies the CNI plugin as `ovn-k8s-cni-overlay` and executes the OVN-K CNI shim binary with the NAD config on stdin and CNI environment variables.

2. **Shim Pass-Through:** The OVN-K CNI shim binary packages the CNI config and environment into a JSON request and forwards it via HTTP POST over a Unix socket to the ovnkube-node daemon running on the same node.

3. **Interface Setup (ovnkube-node Server Side):** The ovnkube-node CNI server receives the request, parses the NAD config, and calls `ConfigureInterface()` which sets up the interface and attaches it to OVS `br-int` via `ConfigureOVS()`. Since IPAM is DHCP, `setupNetwork()` skips IP configuration. For **VFIO** VMs, the VF is temporarily unbound from `vfio-pci` and bound to `mlx5_core` to expose a netdev for the DHCP exchange. For **non-VFIO** VMs (`l2bridge`/`managedTap` binding), the netdev already present in the pod netns is used as-is and no driver handoff is needed.

4. **One-Shot DHCP Lease Acquisition (Server Side):** ovnkube-node detects `ipam.type == "dhcp"` in the NAD config and, because the pod is owned by a VirtualMachineInstance (`kubevirt.IsPodOwnedByVirtualMachine` is true), calls `doOneShotDHCP()` directly using the `insomniacslk/dhcp` Go library. Unlike the pod case, no external DHCP plugin binary or daemon is involved. ovnkube-node performs a full DHCP DORA exchange (Discover, Offer, Request, Ack) with the external DHCP server on the physical network to discover the IP address. For VFIO VMs, this exchange happens on the temporary netdev in the host namespace during the driver handoff window; for non-VFIO VMs it happens on the pod netns interface.

5. **Result Assembly, Pod Annotation, and Return:** ovnkube-node patches the pod's `k8s.ovn.org/pod-networks` annotation with the DHCP-learned `ip_addresses` and `gateway_ips` (so that ovnkube-controller can program the corresponding logical switch port in the OVN NBDB and other features such as MultiNetworkPolicy and Network QoS can consume the IP) and an `ipam_mode: "dhcp"` flag (so that KubeVirt cloud-init sidecars use this field to enable dhcp for this interface in the guest cloud-init network config, see the [Pod Network Annotation Changes](#pod-network-annotation-changes) section for details). Whether the IP is configured on the interface depends on the binding:
   - **VFIO:** ovnkube-node applies the DHCP result to the temporary netdev (`AddrAdd` for the IP, `RouteAdd` for static routes and the default gateway), then rebinds the VF to `vfio-pci` before the result is returned.
   - **Non-VFIO (`l2bridge`/`managedTap`):** ovnkube-node does **not** call `AddrAdd`/`RouteAdd` on the pod interface. Only the annotation and CNI result carry the IP.

   ovnkube-node then returns the result back through the chain: ovnkube-node server -> HTTP response -> shim -> stdout -> Multus. Multus writes the result into the pod's `k8s.v1.cni.cncf.io/network-status` annotation.

6. **No Lease Maintenance:** Unlike the pod case, no lease renewal goroutine or daemon is started. The VM's own operating system (e.g., dhclient via cloud-init or NetworkManager) is expected to run its own DHCP client after boot and take over lease management. On CNI DEL, no DHCP RELEASE is sent — the VM's DHCP client handles its own lifecycle.


The following diagram illustrates the complete KubeVirt VM (one-shot discovery) flow, covering both the VFIO passthrough case (with the driver-handoff window) and the non-VFIO `l2bridge`/`managedTap` case (where the IP is discovered but not configured on the pod interface), along with the `IsPodOwnedByVirtualMachine` check:

![CNI DHCP KubeVirt VMI Pod Allocation Flow](../images/CNI%20DHCP%20KubeVirt%20VMI%20Pod%20Allocation%20Flow.png)

#### NAD Controller Validation

The existing validation in `multi_network.go` that rejects any `IPAM.Type` must be updated to allow `"dhcp"`:

```go
if netconf.IPAM.Type != "" && netconf.IPAM.Type != "dhcp" {
    return fmt.Errorf("error parsing NAD %s: %w", nadName, ErrorUnsupportedIPAMKey)
}
```

#### NBDB Programming

ovnkube-controller reads the DHCP-assigned IP from the pod's `k8s.ovn.org/pod-networks` annotation and programs the logical switch port accordingly. The DHCP-assigned IP is propagated via:

1. CNI result → multus `network-status` annotation → pod annotation
2. ovnkube-controller reads the pod annotation and sets the IP on the logical switch port
3. Port security and ACLs are configured based on the logical switch port's IP

#### Prerequisites

- **DHCP daemon** (`/opt/cni/bin/dhcp daemon`) must run on each node (for the pod delegation path)
- **ovnkube-node DaemonSet** must mount:
  - `/opt/cni/bin` from host (for dhcp binary)
  - `/run/cni` from host (for dhcp daemon socket — new mount required for the pod delegation path)

### Testing Details

* Unit tests covering CRD validation rules for the `DHCP` mode (topology and role constraints) and NAD template generation with DHCP IPAM config.
* Unit tests covering the CNI ADD workload differentiation — verifying that a plain pod is delegated to the CNI DHCP plugin while a KubeVirt VM pod (`IsPodOwnedByVirtualMachine`) takes the one-shot discovery path and gets the `ipam_mode: "dhcp"` annotation.
* E2E tests with a DHCP server (dnsmasq) on the localnet physical network verifying pods receive DHCP-assigned IPs, have connectivity, and that leases are renewed for the pod's lifetime.
* E2E tests verifying pod deletion triggers DHCP lease release and interface teardown.
* E2E tests for **MultiNetworkPolicy** on a DHCP IPAM localnet network exercising both `podSelector` and `namespaceSelector` rules, and verifying that policies are enforced against the DHCP-learned pod IPs (i.e. policy enforcement becomes effective only after the IP is reported via the pod annotation, confirming the control plane consumes the DHCP-learned IP).
* E2E tests for **Network QoS** on a DHCP IPAM localnet network, verifying that QoS rules selecting pods by label are programmed against the DHCP-learned pod IPs and applied to traffic on the localnet interface.

### Documentation Details

- New section in ovn-kubernetes.io: "DHCP IPAM for Localnet Networks"
  - Configuration guide for the `DHCP` mode, including how pods and KubeVirt VMs are handled differently at runtime
  - Prerequisites (DHCP daemon, server, DaemonSet mounts)
- Update existing localnet documentation to reference the DHCP IPAM mode

## Risks, Known Limitations and Mitigations

### Known Limitation: DHCP RELEASE with dnsmasq (pod delegation path)

The upstream CNI DHCP plugin sends DHCP RELEASE packets with source IP `0.0.0.0` instead of the client's assigned IP. This is because the plugin uses raw broadcast sockets (`nclient4.NewRawUDPConn`) for all operations, but RELEASE should use unicast per RFC 2131. dnsmasq validates the source IP and silently drops RELEASE packets with `0.0.0.0`. The lease expires naturally on the DHCP server side.

**Mitigation:**  A fix is required in the upstream DHCP CNI plugin to use `nclient4.WithUnicast()` when creating the client for RELEASE operations.

### Known Limitation: Lease Loss on DHCP Daemon Restart (pod delegation path)

The CNI DHCP daemon stores leases in memory only. If the daemon process restarts, all active leases are lost. No renewal will occur for existing pods.

**Mitigation:** A fix is required in the upstream DHCP CNI plugin to persist lease state to a file on disk, so that the daemon can recover active leases and resume renewals after a restart. In the interim, configure the DHCP server with static MAC-to-IP reservations so that even if a lease expires, no other host gets the same IP. Pods can be restarted to obtain fresh leases.


## OVN-Kubernetes Version Skew

DHCP IPAM for localnet networks will be delivered in version 1.4.

## Alternatives

N/A

## Backwards Compatibility

### API Compatibility
- The new `DHCP` enum value is additive. Existing `Enabled` and `Disabled` modes are unchanged.

## References

- [CNI DHCP IPAM Plugin Documentation](https://www.cni.dev/plugins/current/ipam/dhcp/)
- [CNI Specification v1.0.0 — IPAM Delegation](https://github.com/containernetworking/cni/blob/spec-v1.0.0/SPEC.md)
- [containernetworking/plugins — DHCP source code](https://github.com/containernetworking/plugins/tree/main/plugins/ipam/dhcp)
