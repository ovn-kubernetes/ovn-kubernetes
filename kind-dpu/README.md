# Kind DPU Simulation Topology

This directory contains tools and instructions for setting up a 5-node kind cluster with manual veth rewiring to simulate DPU (Data Processing Unit) connectivity patterns.

## Overview

The setup creates:
- 5 kind nodes with CNI disabled (1 control-plane + 4 workers: 2 hosts + 2 DPUs)
- Point-to-point veth connections between host and DPU nodes
- DPUs maintain Kind cluster connectivity while hosts are isolated to DPU connections only
- Only host-networked pods can function (regular pods fail due to missing CNI)

## Final Network Configuration

| Node | Role | Interface Count | Interfaces |
|------|------|----------------|------------|
| **dpu-test-worker** | Host1 | **2** | `lo + eth0` (to DPU1) |
| **dpu-test-worker2** | DPU1 | **3** | `lo + eth0` (Kind cluster) + `eth1` (from Host1) |
| **dpu-test-worker3** | Host2 | **2** | `lo + eth0` (to DPU2) |
| **dpu-test-worker4** | DPU2 | **3** | `lo + eth0` (Kind cluster) + `eth1` (from Host2) |

## Interleaved Node Mapping

**Note:** Kind will create containers with default names, arranged in an interleaved pattern:
- `dpu-test-worker` → **Host1** (host-worker-1)
- `dpu-test-worker2` → **DPU1** (dpu-worker-1)
- `dpu-test-worker3` → **Host2** (host-worker-2)
- `dpu-test-worker4` → **DPU2** (dpu-worker-2)

## Point-to-Point Connections

- **Host1 eth0** (192.168.1.1) ↔ **DPU1 eth1** (192.168.1.2)
- **Host2 eth0** (192.168.2.1) ↔ **DPU2 eth1** (192.168.2.2)

## Step 1: Create 5-Node Kind Cluster (CNI Disabled)

Save the following as `kind-config.yaml`:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  # Disable default CNI so we can test with error-cni or manual setup
  disableDefaultCNI: true
  podSubnet: "10.244.0.0/16"
  serviceSubnet: "10.96.0.0/12"
nodes:
- role: control-plane
  image: kindest/node:v1.29.0
  extraMounts:
  - hostPath: ./error-cni
    containerPath: /opt/cni/bin/error-cni
- role: worker
  image: kindest/node:v1.29.0
  extraMounts:
  - hostPath: ./error-cni
    containerPath: /opt/cni/bin/error-cni
- role: worker
  image: kindest/node:v1.29.0
  extraMounts:
  - hostPath: ./error-cni
    containerPath: /opt/cni/bin/error-cni
- role: worker
  image: kindest/node:v1.29.0
  extraMounts:
  - hostPath: ./error-cni
    containerPath: /opt/cni/bin/error-cni
```

Create the cluster:
```bash
kind create cluster --config kind-config.yaml --name dpu-test
```

## Step 2: Configure Point-to-Point DPU Connections

After the cluster is running, we'll configure point-to-point connections between hosts and DPUs.

### Verify Initial State

```bash
# Get container names for the 5 nodes
docker ps --format "table {{.Names}}\t{{.Image}}" | grep dpu-test

# Check that all nodes are running
kubectl get nodes --context kind-dpu-test
```

### Configure Point-to-Point Connections

The configuration process:

1. **Hosts** get isolated eth0 interfaces (lose Kind cluster connectivity)
2. **DPUs** keep Kind cluster connectivity on eth0 + gain eth1 for host communication

**Target connections:**
- Host1 eth0 (192.168.1.1) ↔ DPU1 eth1 (192.168.1.2)
- Host2 eth0 (192.168.2.1) ↔ DPU2 eth1 (192.168.2.2)

```bash
# Step 1: Create point-to-point veth pairs
sudo ip link add host1-veth type veth peer name dpu1-veth
sudo ip link add host2-veth type veth peer name dpu2-veth

# Step 2: Configure Host1 with isolated eth0
# Remove Kind networking from Host1 and replace with point-to-point
podman exec dpu-test-worker ip addr flush dev eth0
podman exec dpu-test-worker ip link set eth0 down

# Replace Host1 eth0 with point-to-point connection
sudo ip link set host1-veth netns $(podman inspect -f "{{.State.Pid}}" dpu-test-worker)
podman exec dpu-test-worker ip link set host1-veth name eth0-new
podman exec dpu-test-worker ip link delete eth0
podman exec dpu-test-worker ip link set eth0-new name eth0
podman exec dpu-test-worker ip addr add 192.168.1.1/24 dev eth0
podman exec dpu-test-worker ip link set eth0 up

# Step 3: Add eth1 to DPU1 (keep Kind cluster on eth0)
sudo ip link set dpu1-veth netns $(podman inspect -f "{{.State.Pid}}" dpu-test-worker2)
podman exec dpu-test-worker2 ip link set dpu1-veth name eth1
podman exec dpu-test-worker2 ip addr add 192.168.1.2/24 dev eth1
podman exec dpu-test-worker2 ip link set eth1 up

# Step 4: Configure Host2 with isolated eth0
# Remove Kind networking from Host2 and replace with point-to-point
podman exec dpu-test-worker3 ip addr flush dev eth0
podman exec dpu-test-worker3 ip link set eth0 down

# Replace Host2 eth0 with point-to-point connection
sudo ip link set host2-veth netns $(podman inspect -f "{{.State.Pid}}" dpu-test-worker3)
podman exec dpu-test-worker3 ip link set host2-veth name eth0-new
podman exec dpu-test-worker3 ip link delete eth0
podman exec dpu-test-worker3 ip link set eth0-new name eth0
podman exec dpu-test-worker3 ip addr add 192.168.2.1/24 dev eth0
podman exec dpu-test-worker3 ip link set eth0 up

# Step 5: Add eth1 to DPU2 (keep Kind cluster on eth0)
sudo ip link set dpu2-veth netns $(podman inspect -f "{{.State.Pid}}" dpu-test-worker4)
podman exec dpu-test-worker4 ip link set dpu2-veth name eth1
podman exec dpu-test-worker4 ip addr add 192.168.2.2/24 dev eth1
podman exec dpu-test-worker4 ip link set eth1 up
```

### Verify Configuration

```bash
# Check interface configuration
echo "=== Host1 (dpu-test-worker) ==="
podman exec dpu-test-worker ip addr show | grep -E "^[0-9]|inet "

echo "=== DPU1 (dpu-test-worker2) ==="
podman exec dpu-test-worker2 ip addr show | grep -E "^[0-9]|inet "

echo "=== Host2 (dpu-test-worker3) ==="
podman exec dpu-test-worker3 ip addr show | grep -E "^[0-9]|inet "

echo "=== DPU2 (dpu-test-worker4) ==="
podman exec dpu-test-worker4 ip addr show | grep -E "^[0-9]|inet "
```

### Test Connectivity

```bash
# Test Host-DPU point-to-point connections
echo "Host1 → DPU1:"
podman exec dpu-test-worker ip route get 192.168.1.2 >/dev/null 2>&1 && echo "✓ Route exists" || echo "✗ No route"

echo "DPU1 → Host1:"
podman exec dpu-test-worker2 ip route get 192.168.1.1 >/dev/null 2>&1 && echo "✓ Route exists" || echo "✗ No route"

echo "Host2 → DPU2:"
podman exec dpu-test-worker3 ip route get 192.168.2.2 >/dev/null 2>&1 && echo "✓ Route exists" || echo "✗ No route"

echo "DPU2 → Host2:"
podman exec dpu-test-worker4 ip route get 192.168.2.1 >/dev/null 2>&1 && echo "✓ Route exists" || echo "✗ No route"

# Test DPU Kind cluster connectivity
echo "DPU1 Kind cluster:"
podman exec dpu-test-worker2 ip route get 10.89.1.1 >/dev/null 2>&1 && echo "✓ Kind network accessible" || echo "✗ Kind network not accessible"

echo "DPU2 Kind cluster:"
podman exec dpu-test-worker4 ip route get 10.89.1.1 >/dev/null 2>&1 && echo "✓ Kind network accessible" || echo "✗ Kind network not accessible"
```

## Testing with Error-CNI

With this setup:
- **Host-networked pods** will work on all nodes
- **Regular pods** will fail because error-cni is mounted as the CNI plugin

Deploy test pods to verify:

```bash
# Host-networked pod (should work)
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: host-net-test
spec:
  hostNetwork: true
  containers:
  - name: test
    image: busybox
    command: ["sleep", "3600"]
EOF

# Regular pod (should fail)
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: regular-test
spec:
  containers:
  - name: test
    image: busybox
    command: ["sleep", "3600"]
EOF
```

## Topology Summary

```
                    ┌─────────────────┐
                    │   Control Plane │
                    │  (Kind cluster) │
                    └─────────┬───────┘
                              │
                       Kind Network
                     (10.89.1.x/24)
                              │
                ┌─────────────┴─────────────┐
                │                           │
        ┌───────────────┐           ┌───────────────┐
        │     DPU1      │           │     DPU2      │
        │ eth0(cluster) │           │ eth0(cluster) │
        │ eth1(host)    │           │ eth1(host)    │
        │(192.168.1.2)  │           │(192.168.2.2)  │
        └───────┬───────┘           └───────┬───────┘
                │                           │
                │ veth                      │ veth
                │                           │
        ┌───────────────┐           ┌───────────────┐
        │     Host1     │           │     Host2     │
        │     eth0      │           │     eth0      │
        │(192.168.1.1)  │           │(192.168.2.1)  │
        └───────────────┘           └───────────────┘
           (isolated)                  (isolated)
```

**Key Design:**
- **Hosts**: Completely isolated, only connected to their paired DPU
- **DPUs**: Connected to both Kind cluster AND their paired host
- **Cluster access**: Hosts must route through DPUs to reach cluster resources

This simulates a DPU environment where:
- **Control plane**: Manages the cluster (no special connectivity)
- **Host1 & Host2**: Act as "host" nodes with workloads
- **DPU1 & DPU2**: Act as "DPU" nodes for offloaded processing
- **Point-to-point connectivity**: Simulates dedicated DPU-host links
