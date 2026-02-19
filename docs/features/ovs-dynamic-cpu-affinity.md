# OVS Dynamic CPU Affinity

## Introduction

OVS Dynamic CPU Affinity is a feature that enables ovnkube-node pod to manage the CPU
affinity of `ovs-vswitchd` and `ovsdb-server` processes dynamically. When enabled,
ovnkube-controller container (running in the ovnkube-node pod) continuously monitors the available (non-exclusively-pinned) CPUs on
the node and aligns the OVS daemon processes to use those CPUs. This allows OVS
daemons to access more CPU cycles when needed to cope with network load spikes.

## Motivation

In Kubernetes clusters with performance-sensitive workloads, the kubelet can be
configured with the static CPU Manager policy to provide exclusive CPU allocation
to Guaranteed QoS pods.

When using static CPU Manager, administrators configure `reservedSystemCPUs` which
are dedicated for housekeeping tasks of the system, such as systemd services.
The `ovs-vswitchd` and `ovsdb-server` daemons are such housekeeping processes.
However, giving them access only to `reservedSystemCPUs` may not be sufficient
when network load increases and OVS requires more CPU cycles.

This feature addresses two complementary needs:

1. **Expanding CPU access for OVS**: When the network load rises, OVS daemons can
   span across all non-pinned CPUs (not just reserved ones), giving them access to
   more processing power when needed.

2. **Protecting Guaranteed workloads**: When a new Guaranteed QoS pod is admitted
   and assigned to a specific CPU set, the OVS daemons must be moved off those CPUs
   to avoid interrupting the guaranteed workload's exclusive CPU access.

### User-Stories/Use-Cases

#### Story 1: Dynamic CPU scaling for OVS under load

As a cluster administrator running workloads with static CPU Manager policy,
I want OVS daemons to automatically have access to all available non-pinned CPUs,
so that OVS can handle network load spikes without being constrained to a fixed
CPU set.

#### Story 2: Per-node enablement control

As a cluster administrator, I want to enable OVS dynamic CPU affinity on specific
nodes where I expect high network load, so that I can selectively apply this
feature where it's most beneficial without affecting the entire cluster.

#### Story 3: Runtime feature toggle

As a cluster administrator, I want to be able to enable or disable this feature
at runtime by creating or deleting a file on the node, so that I can quickly
respond to changing workload requirements without restarting pods.

## How to enable this feature on an OVN-Kubernetes cluster?

This feature is enabled on a **per-node basis** by creating a non-empty file at
the following path on the host filesystem:

```text
/etc/openvswitch/enable_dynamic_cpu_affinity
```

### Enabling the feature

To enable the feature on a specific node:

```bash
# On the node (or via SSH/kubectl debug)
# The file must be non-empty to enable the feature
echo 1 > /etc/openvswitch/enable_dynamic_cpu_affinity
```

After creating the file, the ovnkube-node pod watches for this specific file and activates the feature.

### Disabling the feature

To disable the feature:

```bash
# Remove or empty the file
rm /etc/openvswitch/enable_dynamic_cpu_affinity
# or
truncate -s 0 /etc/openvswitch/enable_dynamic_cpu_affinity
```

The feature uses `fsnotify` to watch for changes to this file. When the file
is removed or emptied, the CPU affinity updates will stop (though existing
affinity settings on OVS processes remain until changed by another mechanism).

### Prerequisites

1. **Linux platform**: This feature is only supported on Linux nodes.

2. **Kubelet Pod Resources API**: The kubelet must expose the Pod Resources API
   socket at `/var/lib/kubelet/pod-resources/kubelet.sock`. This is enabled by
   default in Kubernetes.

3. **Static CPU Manager policy** (recommended): While not strictly required, this
   feature is most beneficial when the kubelet is configured with the static CPU
   Manager policy:

   ```yaml
   # In kubelet configuration
   cpuManagerPolicy: static
   reservedSystemCPUs: "0-1"  # Example: reserve CPUs 0 and 1 for system
   ```

4. **Host filesystem access**: The ovnkube-node pod must have access to:
   - `/etc/openvswitch/` (for the enable file)
   - `/var/lib/kubelet/pod-resources/` (for the Pod Resources API)
   - `/host/etc/kubernetes/kubelet.conf` (for reading kubelet configuration)

## Workflow Description

The following diagram illustrates how the OVS Dynamic CPU Affinity feature works:

```text
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Kubernetes Node                                 │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌──────────────────────┐     ┌───────────────────────────────────────────┐ │
│  │    ovnkube-node      │     │              Kubelet                       │ │
│  │    (ovspinning)      │     │                                           │ │
│  │                      │     │  ┌─────────────────────────────────────┐  │ │
│  │  1. Check enabler    │     │  │      Pod Resources API              │  │ │
│  │     file on startup  │     │  │  /var/lib/kubelet/pod-resources/    │  │ │
│  │                      │     │  │                                     │  │ │
│  │  2. Read reserved    │◄────┼──┤  - GetAllocatableResources()        │  │ │
│  │     CPUs from        │     │  │  - ListPodResources()               │  │ │
│  │     kubelet.conf     │     │  │                                     │  │ │
│  │                      │     │  └─────────────────────────────────────┘  │ │
│  │  3. Every second:    │     │                                           │ │
│  │     - Get non-pinned │     │  cpuManagerPolicy: static                 │ │
│  │       CPUs from      │     │  reservedSystemCPUs: "0-1"                │ │
│  │       PodResources   │     │                                           │ │
│  │       API            │     └───────────────────────────────────────────┘ │
│  │     - Add reserved   │                                                   │
│  │       CPUs           │                                                   │
│  │     - Set affinity   │                                                   │
│  │       on OVS procs   │                                                   │
│  │                      │                                                   │
│  └──────────┬───────────┘                                                   │
│             │                                                                │
│             │  sched_setaffinity()                                          │
│             ▼                                                                │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │                         OVS Daemons                                   │   │
│  │                                                                       │   │
│  │   ┌─────────────────┐              ┌─────────────────┐               │   │
│  │   │  ovs-vswitchd   │              │  ovsdb-server   │               │   │
│  │   │                 │              │                 │               │   │
│  │   │  CPU Affinity:  │              │  CPU Affinity:  │               │   │
│  │   │  0-1,4-7        │              │  0-1,4-7        │               │   │
│  │   │  (non-pinned +  │              │  (non-pinned +  │               │   │
│  │   │   reserved)     │              │   reserved)     │               │   │
│  │   └─────────────────┘              └─────────────────┘               │   │
│  │                                                                       │   │
│  └──────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │                    CPU Allocation Example                             │   │
│  │                                                                       │   │
│  │  CPU 0-1: Reserved for system (reservedSystemCPUs)                   │   │
│  │  CPU 2-3: Exclusively pinned to Guaranteed QoS pod                   │   │
│  │  CPU 4-7: Available for BestEffort/Burstable pods + OVS              │   │
│  │                                                                       │   │
│  │  OVS affinity mask = {0,1,4,5,6,7} = reserved ∪ non-pinned           │   │
│  │                                                                       │   │
│  └──────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Workflow Steps

1. **Feature enablement check**: On startup, ovnkube-node checks if
   `/etc/openvswitch/enable_dynamic_cpu_affinity` exists and is non-empty.

2. **Reserved CPUs detection**: The feature reads `reservedSystemCPUs` from
   the kubelet configuration file (`/host/etc/kubernetes/kubelet.conf`). If
   this fails, it falls back to calculating reserved CPUs as the difference
   between online CPUs and allocatable CPUs from the Pod Resources API.
   This process is done once at startup. If both methods fail, the feature logs a
   warning and exits without applying CPU affinity.

3. **Continuous monitoring**: A ticker runs every second to:
   - Query the Pod Resources API for allocatable CPUs and currently pinned CPUs.
   - Calculate non-pinned CPUs as: `allocatable - used_by_guaranteed_containers`.
   - Add reserved system CPUs to the set.
   - Apply this CPU set as the affinity mask for both `ovs-vswitchd` and
     `ovsdb-server` processes (including all their threads).

4. **File watcher**: An `fsnotify` watcher monitors the enabler file for changes.
   If the file is removed or emptied, affinity updates stop.

## Implementation Details

### OVN-Kubernetes Implementation Details

The feature is implemented in the `ovspinning` package under
`go-controller/pkg/node/ovspinning/`. It runs as a goroutine started by the
`DefaultNodeNetworkController` during node initialization.

#### Key Components

- **ovspinning_linux.go**: Main implementation for Linux systems
- **ovspinning_noop.go**: No-op implementation for non-Linux platforms
- **podresourcesapi package**: Client for the Kubelet Pod Resources API

#### CPU Set Calculation

The CPU affinity for OVS daemons is calculated as:

```text
OVS CPU Affinity = (Allocatable CPUs - Exclusively Pinned CPUs) ∪ Reserved System CPUs
```

Where:

- **Allocatable CPUs**: CPUs available for pod scheduling (from Kubelet perspective)
- **Exclusively Pinned CPUs**: CPUs assigned to QoS class guaranteed containers with exclusive CPU access.
- **Reserved System CPUs**: CPUs reserved for system processes and housekeeping tasks (from kubelet config).

#### Thread-Level Affinity

The feature sets CPU affinity at the thread level, iterating through all threads
of each OVS daemon process (`/proc/<pid>/task/`) to ensure both the main process
and all spawned threads are properly pinned.

#### Code Flow

```go
// Simplified flow from ovspinning_linux.go

func Run(ctx context.Context, stopCh <-chan struct{}, podResCli podresourcesapi.PodResourcesListerClient) {
    // Check if feature is enabled
    if !isFileNotEmpty("/etc/openvswitch/enable_dynamic_cpu_affinity") {
        return
    }
    
    // Get reserved CPUs (from kubelet config or fallback)
    reservedCPUs := getReservedCPUs(kubeletConfigFilePath)
    
    // Main loop - runs every second
    for {
        // Get non-pinned CPUs from Pod Resources API
        cpus := getNonPinnedCPUs(ctx, podResCli)
        
        // Add reserved CPUs
        cpus = cpus.Union(reservedCPUs)
        
        // Set affinity on OVS processes
        setOvsVSwitchdCPUAffinity(&cpus)
        setOvsDBServerCPUAffinity(&cpus)
    }
}
```

## Troubleshooting

### Verifying the feature is enabled

Check the ovnkube-node logs for the following messages:

```bash
kubectl logs -n ovn-kubernetes <ovnkube-node-pod> | grep -i "ovspinning\|cpu pinning"
```

Feature enabled:

```text
I0115 10:00:00.000000  Starting OVS daemon CPU pinning
I0115 10:00:00.000000  OVS CPU dynamic pinning reservedSystemCPUs set: 0-1
```

Feature disabled:

```text
I0115 10:00:00.000000  OVS CPU affinity pinning disabled
```

### Checking current OVS CPU affinity

On the node, check the CPU affinity of OVS processes:

```bash
# Get ovs-vswitchd PID
OVS_PID=$(pidof ovs-vswitchd)

# Check current affinity
taskset -cp $OVS_PID

# Example output:
# pid 1234's current affinity list: 0-1,4-7
```

### Common issues

#### Issue: "Failed to get reservedSystemCPUs from kubelet config file"

This warning indicates the kubelet configuration file couldn't be read. The
feature falls back to detecting reserved CPUs from the Pod Resources API.
Ensure `/host/etc/kubernetes/kubelet.conf` is accessible from the pod.

#### Issue: "GetAllocatableResources failed"

Verify the Pod Resources API is available:

```bash
ls -la /var/lib/kubelet/pod-resources/kubelet.sock
```

Ensure the ovnkube-node pod has the socket mounted.

#### Issue: CPU affinity not being updated

1. Verify the enabler file exists and is non-empty:

   ```bash
   ls -la /etc/openvswitch/enable_dynamic_cpu_affinity
   cat /etc/openvswitch/enable_dynamic_cpu_affinity
   ```

2. Check for errors in the logs related to setting affinity

3. Verify ovs-vswitchd and ovsdb-server are running:

   ```bash
   pidof ovs-vswitchd ovsdb-server
   ```

### Metrics and alerts

Currently, this feature does not expose dedicated Prometheus metrics. The CPU
affinity changes are logged and can be observed in the ovnkube-node logs.

## Best Practices

1. **Use with static CPU Manager policy**: This feature is most beneficial when
   the kubelet is configured with `cpuManagerPolicy: static` and you have
   Guaranteed QoS pods with exclusive CPU access.

2. **Set appropriate reservedSystemCPUs**: Ensure your kubelet configuration
   includes `reservedSystemCPUs` to guarantee OVS always has access to some CPUs.

3. **Enable selectively**: Only enable this feature on nodes where you expect
   high network load or have many pods with exclusive CPU allocation.

4. **Monitor OVS performance**: After enabling, monitor OVS packet drops and
   latency to verify the feature is having the desired effect.

## Known Limitations

- **Linux only**: This feature is only supported on Linux nodes. On other
  platforms, the feature logs a message and exits gracefully.

- **No Windows/macOS support**: Due to the use of Linux-specific syscalls
  (`sched_setaffinity`), this feature is not portable to other operating systems.

- **Affinity persists after disabling**: When the feature is disabled (by
  removing the enabler file), the existing CPU affinity on OVS processes is not
  reset. The affinity will remain until changed by another mechanism or process
  restart.

- **Brief interference window**: The affinity is updated every second, plus up to
  5 seconds for the CPU Manager reconciliation loop to update the exclusive CPUs
  set. During this window, OVS processes may still run on CPUs that have been
  assigned to a newly admitted Guaranteed pod, potentially causing brief
  interruptions to sensitive workloads.

## References

- [Kubelet CPU Manager Policy](https://kubernetes.io/docs/tasks/administer-cluster/cpu-management-policies/)
- [Pod Resources API](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/#monitoring-device-plugin-resources)
- [Linux sched_setaffinity](https://man7.org/linux/man-pages/man2/sched_setaffinity.2.html)
- [Original PR #3542: OVS Dynamic CPU Affinity](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/3542)
- [PR #5270: Using PodResourcesAPI to get affinity mask](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/5270)
