# OKEP-6434: Support VFIO passthrough for Kata Containers

* Issue: [#6434](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6434)

## Problem Statement

Kata Containers improves workload isolation by running Pods inside lightweight VMs, but this model
adds networking overhead compared to standard container networking.

To recover performance, users need SR-IOV passthrough for Kata Pods. In practice, a NAD
uses `k8s.v1.cni.cncf.io/resourceName` to request an SR-IOV resource, which is typically one of:

1. VF resource
2. VFIO resource

For VF resources, OVN-Kubernetes can use the existing SR-IOV integration path. On the Kata side,
the runtime must enable `cold_plug_vfio` to use VF resources for passthrough into the guest VM.

The main gap is VFIO resources. OVN-Kubernetes cannot configure networking directly on a host-side
VFIO device, while Kata does not consume OVN Pod annotations alone for this path. Instead, Kata
expects network configuration through DAN (Directly Attachable Network), which can be generated
by OVN-Kubernetes.

## Goals

* Support SR-IOV VF attachment for Kata Pods with the same functional behavior as non-Kata Pods.
* Support SR-IOV VFIO passthrough for Kata Pods by generating DAN configuration consumable by Kata runtime.
* Keep existing SR-IOV behavior unchanged for workloads that do not use Kata/VFIO.
* Support multi-network (multiple NAD) scenarios in DAN generation.

## Non-Goals

* Defining or changing Kata runtime behavior (`cold_plug_vfio`, `hot_plug_vfio`, VM device lifecycle).
* Introducing new Kubernetes CRDs/APIs for DAN configuration.
* Changing SR-IOV device plugin allocation logic.
* Altering OVN datapath behavior for Kata Pods.

## Introduction

Kata supports attaching SR-IOV devices in two modes:

* **Cold plug**: device is attached during VM creation.
* **Hot plug**: device is attached after VM boot, typically using VFIO.

For a VF-backed resource, OVN-Kubernetes configures the host-side VF interface similarly to a
regular Pod interface. Kata runtime retrieves network configuration by scanning the VF interface, so
DAN is not needed for this path. The only required OVN-Kubernetes change is to set the VF interface
MAC address on the VF representor, which was addressed in
[PR #6388](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6388).

For a VFIO-backed resource, OVN-Kubernetes cannot apply interface configuration directly to VFIO in
the host namespace. The required in-guest network information must be delivered through DAN JSON that
Kata runtime reads and applies to the interface inside the VM.

Therefore, this enhancement adds DAN generation in OVN-Kubernetes CNI for Kata + VFIO workloads.

## User-Stories/Use-Cases

Story 1: SR-IOV VF for Kata Pods

**As a** user running Kata Pods, **I want** to create a Pod with one or more NADs that request SR-IOV VF resources,
**so that** the Pod gets hardware-accelerated networking while preserving Kata isolation.

Story 2: SR-IOV VFIO for Kata Pods

**As a** user running Kata Pods, **I want** to create a Pod with one or more NADs that request SR-IOV VFIO resources,
**so that** OVN-Kubernetes can provide the network configuration required by the runtime, the interfaces can be
configured, and the Pod can communicate on the requested network.


## Proposed Solution

OVN-Kubernetes CNI will generate and manage per-sandbox DAN JSON when all of the following are true:

* the Pod uses a Kata runtime: `spec.runtimeClassName` is set and its value **starts with** the prefix
  `kata` (case-sensitive); see [Kata runtime detection](#kata-runtime-detection) below.
* the requested device is VFIO, and
* DAN support is enabled/configured in OVN-Kubernetes.

The DAN file contains the network information already computed by OVN-Kubernetes CNI (IPs, routes,
MTU, MAC/interface metadata, and device PCI context) but encoded in the format expected by Kata.

#### Kata runtime detection

OVN-Kubernetes does not inspect the RuntimeClass handler or CRI runtime configuration. It uses a
deliberate, name-based heuristic: a Pod is treated as Kata when `spec.runtimeClassName` is non-empty
and starts with the prefix `kata` (case-sensitive).

[kata-deploy](https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy)
is the typical way to install Kata on Kubernetes. Its Helm chart creates one RuntimeClass per
enabled shim using the pattern `kata-<shim>` (see
[runtimeclasses.yaml](https://github.com/kata-containers/kata-containers/blob/main/tools/packaging/kata-deploy/helm-chart/kata-deploy/templates/runtimeclasses.yaml)),
for example `kata-qemu`, `kata-clh`, and `kata-fc`. These default names satisfy the prefix rule, so
DAN generation works out of the box for Pods that reference them.

Cluster administrators who override RuntimeClass names -- via kata-deploy's `runtimeClasses.defaultName`,
`nameOverride`, or a custom RuntimeClass manifest—must keep the `kata` prefix unless they intend to
opt out of DAN generation. A RuntimeClass that selects the Kata handler but uses a different name
(for example `my-kata` or `KataContainers`) will not trigger DAN generation.

Examples:

| `spec.runtimeClassName` | Treated as Kata |
| --- | --- |
| `kata` | yes |
| `kata-qemu` | yes (kata-deploy default) |
| `kata-clh` | yes (kata-deploy default) |
| `kata-fc` | yes (kata-deploy default) |
| `KataContainers` | no |
| `my-kata` | no |
| unset or empty | no |

### API Details

No Kubernetes API changes are proposed.

The feature is controlled through OVN-Kubernetes configuration:

* Helm values to toggle and configure DAN support:
  * `global.kata.enableDAN` (default `false`)
  * `global.kata.danConfigDir` (default `/run/kata-containers/dans`), which sets the host path where
    Kata runtime loads DAN config files.
  * This host path is mounted into ovnkube-controller container at a well-known internal path where OVN-Kubernetes
    writes DAN JSON.
* ovnkube-controller environment variable:
  * `OVNKUBE_ENABLE_KATA_DAN=true` when enabled by deployment manifests/charts.

OVN-Kubernetes introduces Go structs matching Kata DAN schema so the generated JSON is compatible
with Kata runtime expectations. Reference schema:
[Kata DanConfig](https://github.com/kata-containers/kata-containers/blob/main/src/runtime-rs/crates/resource/src/network/dan.rs).

### Implementation Details

The implementation is expected to include:

1. **DAN type model support**
   * Add DAN config/data structures in CNI types package.
   * Include device/network fields needed by Kata guest configuration.

2. **CNI ADD path integration**
   * During `cmdAdd`, detect Kata + VFIO requests.
   * Build DAN device/network entries from computed CNI result data.
   * Write or update a per-sandbox DAN JSON file in configured directory.
   * Support multiple network attachments in a single sandbox DAN config.

3. **CNI DEL path integration**
   * During `cmdDel`, remove DAN config for the sandbox.
   * Treat missing file as non-fatal to keep teardown idempotent.

4. **Deployment wiring**
   * Add Helm values and templates to mount DAN config directory.
   * Propagate enablement knobs into ovnkube-node / single-node-zone / dpu-host manifests.

#### High-level flow

1. Pod requests SR-IOV resource via NAD annotation `k8s.v1.cni.cncf.io/resourceName`.
2. SR-IOV plugin allocates device and passes device data in CNI ADD.
3. OVN-Kubernetes configures network state as usual.
4. If request is Kata + VFIO and DAN is enabled, OVN-Kubernetes writes DAN JSON.
5. Kata runtime reads DAN file and applies networking to the in-guest interface.

### Testing Details

* **Unit Testing**
  * Validate DAN JSON generation for single-network and multi-network cases.
  * Validate DAN merge/update behavior for repeated ADD/idempotency scenarios.
  * Validate cleanup behavior in DEL, including file-not-found cases.


### Documentation Details

* Add user-facing docs for enabling Kata DAN support in OVN-Kubernetes deployment (flags/env/Helm values).
* Document the Kata RuntimeClass naming requirement (`runtimeClassName` must start with `kata`).
* Add examples for NAD/Pod configurations covering VF and VFIO Kata use-cases.
* Add this OKEP to `mkdocs.yml`.

## Risks, Known Limitations and Mitigations

* **Risk:** DAN schema drift between Kata and OVN-Kubernetes.
  * **Mitigation:** Keep DAN type mapping aligned with Kata source schema and add unit tests against fixtures.
* **Limitation:** `cold_plug_vfio` with VF resources can conflict with RDMA device mounts.
  * In this mode, Kata runtime unbinds a VF from its original driver and rebinds it to VFIO before creating the sandbox.
  * Kubelet still forwards device paths returned by the device plugin into CRI `CreateContainer` requests.
  * Containerd then resolves each CRI device `HostPath`; if a path no longer exists (for example
    `/dev/infiniband/uverbsNNN`), container creation fails with:
    `failed to generate container "... " spec: failed to generate spec: lstat /dev/infiniband/uverbsNNN: no such file or directory`.
  * Practical root cause: a stale/non-existent RDMA `DeviceSpec.HostPath` is propagated from device-plugin
    allocation through kubelet into containerd.
  * **Mitigation/Workaround:** use a dedicated SR-IOV resource pool for Kata VF workloads with
    `isRdma=false` so RDMA devices are not injected for these Pods.


## OVN-Kubernetes Version Skew

Targeting the next minor release after merge.

## Backwards Compatibility

This enhancement is backwards compatible:

* No Kubernetes API changes.
* Feature is disabled by default.
* Existing non-Kata and non-VFIO networking behavior is unchanged.
* Existing SR-IOV VF behavior is preserved.

## Alternatives

1. **Do nothing for VFIO**
   * Rejected because Kata cannot consume OVN Pod annotations directly for VFIO guest configuration.


## References

* OKEP issue: [Support VFIO passthrough for Kata container #6434](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6434)
* Implementation PR: [Add DAN support for Kata Containers with VFIO interfaces #6407](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6407)
* Related fix for VF path MAC handling: [#6388](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/6388)
* Kata RFC: [[RFC] Direct Attachable CNIs For Kata Containers #1922](https://github.com/kata-containers/kata-containers/issues/1922)
