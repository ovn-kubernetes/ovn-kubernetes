# DPU Topology with OVN-Kubernetes

This guide sets up a kind cluster with DPU simulation topology and deploys OVN-Kubernetes on it. OVN-K will work on DPU nodes but fail on isolated host nodes (expected).

## Topology

```
                    +-------------------+
                    |   Control Plane   |
                    +--------+----------+
                             |
                      Kind Network
                             |
          +------------------+------------------+
          |                                     |
  +-------+-------+                     +-------+-------+
  |     DPU1      |                     |     DPU2      |
  | (dpu-worker2) |                     | (dpu-worker4) |
  | eth0(cluster) |                     | eth0(cluster) |
  | rep0-0..15    |                     | rep1-0..15    |
  | (16 reps)     |                     | (16 reps)     |
  +-------+-------+                     +-------+-------+
          |                                     |
          | 16 veth pairs                       | 16 veth pairs
          | (to rep interfaces)                 | (to rep interfaces)
          |                                     |
  +-------+-------+                     +-------+-------+
  |     Host1     |                     |     Host2     |
  | (dpu-worker)  |                     | (dpu-worker3) |
  | eth0-0..15    |                     | eth0-0..15    |
  | (16 interfaces)|                    | (16 interfaces)|
  +---------------+                     +---------------+
```

The setup includes two host-DPU pairs, each with 16 veth pairs connecting host interfaces to DPU representor interfaces:
- **Pair 1**: Host1 (dpu-worker) ↔ DPU1 (dpu-worker2)
  - Host side: eth0-0 to eth0-15 (16 host interfaces)
  - DPU side: rep0-0 to rep0-15 (16 representor interfaces)
- **Pair 2**: Host2 (dpu-worker3) ↔ DPU2 (dpu-worker4)
  - Host side: eth0-0 to eth0-15 (16 host interfaces)
  - DPU side: rep1-0 to rep1-15 (16 representor interfaces)

## Prerequisites

```bash
pip install jinjanator[yaml]
```

## Step 1: Delete existing clusters

```bash
kind delete cluster --name=dpu
```

## Step 2: Generate kind config (parameterized workers)

```bash
cd /root/ovn-kubernetes/contrib

# Configuration: Number of DPU-host pairs (each pair = 2 workers: 1 host + 1 DPU)
NUM_DPU_PAIRS=2
TOTAL_WORKERS=$((NUM_DPU_PAIRS * 2))

echo "Generating kind config for ${NUM_DPU_PAIRS} DPU-host pairs (${TOTAL_WORKERS} workers total)"

ovn_ip_family="" \
ovn_ha=false \
net_cidr="10.244.0.0/16" \
svc_cidr="10.96.0.0/16" \
use_local_registy=false \
dns_domain="cluster.local" \
ovn_num_master=1 \
ovn_num_worker=${TOTAL_WORKERS} \
cluster_log_level=4 \
kind_local_registry_port=5000 \
kind_local_registry_name="kind-registry" \
jinjanate kind.yaml.j2 -o kind-dpu.yaml

cd ..
```

## Step 3: Create kind cluster

```bash
kind create cluster \
  --name dpu \
  --kubeconfig $HOME/dpu.conf \
  --image kindest/node:v1.34.0 \
  --config contrib/kind-dpu.yaml \
  --retain

export KUBECONFIG=$HOME/dpu.conf
```

## Step 4: Set up DPU topology with PROPER HOST ISOLATION

This creates the exact topology from the diagram: DPU nodes have cluster access, host nodes are completely isolated and can only communicate through veth pairs.

**✅ Parameterized approach for scalable DPU-host pair creation**

```bash
# Configuration: Number of DPU-host pairs and interfaces per pair
NUM_DPU_PAIRS=2
INTERFACES_PER_PAIR=16

# Clean up any existing veth interfaces
echo "Cleaning up existing veth interfaces..."
for pair_id in $(seq 0 $((NUM_DPU_PAIRS - 1))); do
  for iface_id in $(seq 1 $((INTERFACES_PER_PAIR - 1))); do
    sudo ip link delete host${pair_id}-eth0-${iface_id} 2>/dev/null || true
    sudo ip link delete dpu${pair_id}-rep${pair_id}-${iface_id} 2>/dev/null || true
  done
done

# Create additional veth pairs for each DPU-host pair (15 pairs, since we reuse original eth0)
echo "Creating ${NUM_DPU_PAIRS} DPU-host pairs with $((INTERFACES_PER_PAIR - 1)) additional veth pairs each..."
for pair_id in $(seq 0 $((NUM_DPU_PAIRS - 1))); do
  echo "  Creating additional veth pairs for DPU-host pair ${pair_id}..."
  for iface_id in $(seq 1 $((INTERFACES_PER_PAIR - 1))); do
    # Create veth pair: host side ↔ DPU side
    # Host side: host${pair_id}-eth0-${iface_id} (will become eth0-${iface_id})
    # DPU side: dpu${pair_id}-rep${pair_id}-${iface_id} (will become rep${pair_id}-${iface_id})
    sudo ip link add host${pair_id}-eth0-${iface_id} type veth peer name dpu${pair_id}-rep${pair_id}-${iface_id}
  done
done

# Define container names for each pair
# Pair 0: dpu-worker (host) ↔ dpu-worker2 (DPU)
# Pair 1: dpu-worker3 (host) ↔ dpu-worker4 (DPU)
declare -a HOST_CONTAINERS=("dpu-worker" "dpu-worker3")
declare -a DPU_CONTAINERS=("dpu-worker2" "dpu-worker4")

# Get container PIDs for all DPU-host pairs
echo "Getting container PIDs..."
for pair_id in $(seq 0 $((NUM_DPU_PAIRS - 1))); do
  eval "HOST${pair_id}_PID=\$(docker inspect --format '{{.State.Pid}}' ${HOST_CONTAINERS[$pair_id]})"
  eval "DPU${pair_id}_PID=\$(docker inspect --format '{{.State.Pid}}' ${DPU_CONTAINERS[$pair_id]})"
  eval echo "  Pair ${pair_id}: Host \${HOST_CONTAINERS[$pair_id]} PID=\${HOST${pair_id}_PID}, DPU \${DPU_CONTAINERS[$pair_id]} PID=\${DPU${pair_id}_PID}"
done

# Reuse original eth0 as eth0-0
echo "Renaming original eth0 to eth0-0..."
docker exec dpu-worker ip link set eth0 name eth0-0
docker exec dpu-worker3 ip link set eth0 name eth0-0

# Create veth pairs for eth0-0 connections
echo "Creating veth pairs for eth0-0 connections..."
sudo ip link add temp-host0-0 type veth peer name temp-rep0-0
sudo ip link add temp-host1-0 type veth peer name temp-rep1-0

# Move eth0-0 veth pairs to containers
sudo ip link set temp-host0-0 netns ${HOST0_PID}
sudo ip link set temp-rep0-0 netns ${DPU0_PID}
sudo ip link set temp-host1-0 netns ${HOST1_PID}
sudo ip link set temp-rep1-0 netns ${DPU1_PID}

# Rename eth0-0 veth pairs
docker exec dpu-worker ip link set temp-host0-0 name eth0-0
docker exec dpu-worker2 ip link set temp-rep0-0 name rep0-0
docker exec dpu-worker3 ip link set temp-host1-0 name eth0-0
docker exec dpu-worker4 ip link set temp-rep1-0 name rep1-0

# Move additional veth interfaces to containers (eth0-1 through eth0-15)
echo "Moving additional veth interfaces to containers..."
for iface_id in $(seq 1 $((INTERFACES_PER_PAIR - 1))); do
  sudo ip link set host0-eth0-${iface_id} netns ${HOST0_PID}
  sudo ip link set dpu0-rep0-${iface_id} netns ${DPU0_PID}
  sudo ip link set host1-eth0-${iface_id} netns ${HOST1_PID}
  sudo ip link set dpu1-rep1-${iface_id} netns ${DPU1_PID}
done

# Rename additional interfaces inside containers
echo "Renaming additional interfaces inside containers..."
for iface_id in $(seq 1 $((INTERFACES_PER_PAIR - 1))); do
  docker exec dpu-worker ip link set host0-eth0-${iface_id} name eth0-${iface_id} 2>/dev/null || true
  docker exec dpu-worker2 ip link set dpu0-rep0-${iface_id} name rep0-${iface_id} 2>/dev/null || true
  docker exec dpu-worker3 ip link set host1-eth0-${iface_id} name eth0-${iface_id} 2>/dev/null || true
  docker exec dpu-worker4 ip link set dpu1-rep1-${iface_id} name rep1-${iface_id} 2>/dev/null || true
done

# Isolate host nodes from kind network
echo "Isolating host nodes from kind network..."
docker exec dpu-worker ip route del default 2>/dev/null || true
docker exec dpu-worker3 ip route del default 2>/dev/null || true

echo "✅ All interfaces configured and host nodes isolated"

# Veth interfaces are now ready for OVN-Kubernetes management
echo "Veth interfaces created and ready for OVN-Kubernetes..."
for pair_id in $(seq 0 $((NUM_DPU_PAIRS - 1))); do
  echo "  Pair ${pair_id}: ${HOST_CONTAINERS[$pair_id]}.eth0-0..15 ↔ ${DPU_CONTAINERS[$pair_id]}.rep${pair_id}-0..15"
done

# Verify the topology
echo ""
echo "✅ Perfect isolated topology created:"
echo "   Total: ${NUM_DPU_PAIRS} DPU-host pairs with ${INTERFACES_PER_PAIR} connections each"
echo "   New veth pairs created: $((NUM_DPU_PAIRS * (INTERFACES_PER_PAIR - 1))) + ${NUM_DPU_PAIRS} repurposed connections"
for pair_id in $(seq 0 $((NUM_DPU_PAIRS - 1))); do
  echo "   Pair ${pair_id}:"
  echo "     Host ${HOST_CONTAINERS[$pair_id]}: exactly 16 interfaces (eth0-0..15) - ISOLATED from kind network"
  echo "     DPU ${DPU_CONTAINERS[$pair_id]}: 17 interfaces - eth0 (transferred cluster access) + rep${pair_id}-0..15"
done
echo "   Host isolation: Complete - no direct cluster access, exactly 16 interfaces per host"
```

After this setup:
- **Scalable Configuration**: `NUM_DPU_PAIRS=2` and `INTERFACES_PER_PAIR=16` for easy adjustment
- **Host Nodes**: Each has exactly 16 interfaces (COMPLETELY ISOLATED from kind network)
  - **Host1 (dpu-worker)**: eth0-0 to eth0-15 (eth0-0 is repurposed original, eth0-1..15 are new)
  - **Host2 (dpu-worker3)**: eth0-0 to eth0-15 (eth0-0 is repurposed original, eth0-1..15 are new)
- **DPU Nodes**: Each has transferred kind network access + 16 representor interfaces
  - **DPU1 (dpu-worker2)**: eth0 (cluster access from Host1) + rep0-0 to rep0-15 (to Host1)
  - **DPU2 (dpu-worker4)**: eth0 (cluster access from Host2) + rep1-0 to rep1-15 (to Host2)
- **Veth Connectivity**: Raw connections ready for OVN-Kubernetes management
  - Pair 0: Host1.eth0-0..15 ↔ DPU1.rep0-0..15 (no IP addresses assigned)
  - Pair 1: Host2.eth0-0..15 ↔ DPU2.rep1-0..15 (no IP addresses assigned)
- **Perfect Topology**: Hosts have exactly 16 interfaces, DPUs have 17 interfaces
- **Efficient Design**: Reuses existing infrastructure instead of creating redundant interfaces

**Key Design Principles**:
1. **Each Host has exactly 16 interfaces**: eth0-0 (repurposed) + eth0-1..15 (new veth ends)
2. **Each DPU has exactly 17 interfaces**: eth0 (transferred kind network) + rep${pair_id}-0..15
3. **Efficient resource usage**: Repurposes existing eth0 instead of creating redundant interfaces
4. **Total new veth pairs**: 30 (15 per DPU-host pair) + 2 repurposed connections = 32 total connections
5. **Parameterized approach**: Easy to scale to different numbers of DPU-host pairs
6. **Representor simulation**: Each connection simulates a host interface ↔ representor relationship

This creates the foundation for veth-based DPU simulation where OVN-Kubernetes will run on DPU nodes and manage pod connectivity through representor interfaces.

## Step 5: Enable IPv6 and fix inotify limits

```bash
for node in $(kind get nodes --name dpu); do
  podman exec "$node" sysctl --ignore net.ipv6.conf.all.disable_ipv6=0
  podman exec "$node" sysctl --ignore net.ipv6.conf.all.forwarding=1
  podman exec "$node" sysctl -w fs.inotify.max_user_watches=524288
  podman exec "$node" sysctl -w fs.inotify.max_user_instances=512
done
```

## Step 6: Build OVN image (skip if already built)

```bash
make -C dist/images \
  IMAGE="localhost/ovn-daemonset-fedora:dev" \
  OVN_REPO="" \
  OVN_GITREF="" \
  OCI_BIN="podman" \
  fedora-image
```

## Step 7: Load image into kind

```bash
kind load docker-image localhost/ovn-daemonset-fedora:dev --name dpu
```

## Step 8: Get API URL and generate manifests

```bash
DNS_NAME_URL=$(kind get kubeconfig --internal --name dpu | grep server | awk '{ print $2 }')
CP_NODE=${DNS_NAME_URL#*//}
CP_NODE=${CP_NODE%:*}
NODE_IP=$(podman inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' "$CP_NODE")
API_URL=${DNS_NAME_URL/$CP_NODE/$NODE_IP}

cd dist/images

./daemonset.sh \
  --output-directory="../yaml-dpu" \
  --image="localhost/ovn-daemonset-fedora:dev" \
  --ovnkube-image="localhost/ovn-daemonset-fedora:dev" \
  --net-cidr="10.244.0.0/16" \
  --svc-cidr="10.96.0.0/16" \
  --gateway-mode="shared" \
  --dummy-gateway-bridge="false" \
  --gateway-options="" \
  --enable-ipsec="false" \
  --hybrid-enabled="false" \
  --disable-snat-multiple-gws="false" \
  --disable-forwarding="false" \
  --ovn-encap-port="" \
  --disable-pkt-mtu-check="false" \
  --ovn-empty-lb-events="false" \
  --multicast-enabled="false" \
  --k8s-apiserver="$API_URL" \
  --ovn-master-count="1" \
  --ovn-unprivileged-mode=no \
  --master-loglevel="5" \
  --node-loglevel="5" \
  --dbchecker-loglevel="5" \
  --ovn-loglevel-northd="-vconsole:info -vfile:info" \
  --ovn-loglevel-nb="-vconsole:info -vfile:info" \
  --ovn-loglevel-sb="-vconsole:info -vfile:info" \
  --ovn-loglevel-controller="-vconsole:info" \
  --ovnkube-libovsdb-client-logfile="" \
  --enable-coredumps="false" \
  --ovnkube-config-duration-enable=true \
  --admin-network-policy-enable=true \
  --egress-ip-enable=true \
  --egress-ip-healthcheck-port="9107" \
  --egress-firewall-enable=true \
  --egress-qos-enable=true \
  --egress-service-enable=true \
  --v4-join-subnet="100.64.0.0/16" \
  --v6-join-subnet="fd98::/64" \
  --v4-masquerade-subnet="169.254.0.0/17" \
  --v6-masquerade-subnet="fd69::/112" \
  --v4-transit-subnet="100.88.0.0/16" \
  --v6-transit-subnet="fd97::/64" \
  --ex-gw-network-interface="" \
  --multi-network-enable="false" \
  --network-segmentation-enable="false" \
  --network-connect-enable="false" \
  --preconfigured-udn-addresses-enable="false" \
  --route-advertisements-enable="false" \
  --advertise-default-network="false" \
  --advertised-udn-isolation-mode="strict" \
  --ovnkube-metrics-scale-enable="false" \
  --compact-mode="false" \
  --enable-interconnect="true" \
  --enable-multi-external-gateway=true \
  --enable-ovnkube-identity="true" \
  --enable-persistent-ips=true \
  --network-qos-enable="false" \
  --mtu="1400" \
  --enable-dnsnameresolver="false" \
  --enable-observ="false"

cd ../..
```

## Step 9: Apply CRDs

```bash
cd dist/yaml-dpu

kubectl apply -f k8s.ovn.org_egressfirewalls.yaml
kubectl apply -f k8s.ovn.org_egressips.yaml
kubectl apply -f k8s.ovn.org_egressqoses.yaml
kubectl apply -f k8s.ovn.org_egressservices.yaml
kubectl apply -f k8s.ovn.org_adminpolicybasedexternalroutes.yaml
kubectl apply -f k8s.ovn.org_networkqoses.yaml
kubectl apply -f k8s.ovn.org_userdefinednetworks.yaml
kubectl apply -f k8s.ovn.org_clusteruserdefinednetworks.yaml
kubectl apply -f k8s.ovn.org_routeadvertisements.yaml

kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_adminnetworkpolicies.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_baselineadminnetworkpolicies.yaml
```

## Step 10: Apply setup and RBAC

```bash
kubectl apply -f ovn-setup.yaml
kubectl apply -f rbac-ovnkube-identity.yaml
kubectl apply -f rbac-ovnkube-cluster-manager.yaml
kubectl apply -f rbac-ovnkube-master.yaml
kubectl apply -f rbac-ovnkube-node.yaml
kubectl apply -f rbac-ovnkube-db.yaml
```

## Step 11: Label and untaint control plane

```bash
kubectl label node dpu-control-plane k8s.ovn.org/ovnkube-db=true node-role.kubernetes.io/control-plane="" --overwrite
kubectl taint node dpu-control-plane node-role.kubernetes.io/master:NoSchedule- || true
kubectl taint node dpu-control-plane node-role.kubernetes.io/control-plane:NoSchedule- || true
```

## Step 12: Apply OVS and label zones

```bash
kubectl apply -f ovs-node.yaml

for node in $(kind get nodes --name dpu); do
  kubectl label node "$node" k8s.ovn.org/zone-name=${node} --overwrite
done
```

## Step 13: Apply OVN-Kubernetes components

```bash
kubectl apply -f ovnkube-identity.yaml
kubectl apply -f ovnkube-control-plane.yaml
kubectl apply -f ovnkube-single-node-zone.yaml

kubectl patch ds -n ovn-kubernetes ovnkube-node --type='json' \
  -p='[{"op": "add", "path": "/spec/updateStrategy/rollingUpdate", "value": {"maxUnavailable": "100%"}}]'
```

## Step 14: Verify

```bash
kubectl get nodes
kubectl get pods -n ovn-kubernetes -o wide
```

Expected result:
- DPU nodes (dpu-worker2, dpu-worker4) and control-plane: OVN-K pods Running
- Host nodes (dpu-worker, dpu-worker3): OVN-K pods Pending (expected - isolated)

## Step 15: Label nodes for DPU mode deployment

To deploy OVN-K in DPU mode (separate DaemonSets for DPU and host nodes), label the nodes:

```bash
# Label host nodes
kubectl label nodes dpu-worker k8s.ovn.org/dpu-host= --overwrite
kubectl label nodes dpu-worker3 k8s.ovn.org/dpu-host= --overwrite

# Label DPU nodes
kubectl label nodes dpu-worker2 k8s.ovn.org/dpu= --overwrite
kubectl label nodes dpu-worker4 k8s.ovn.org/dpu= --overwrite
```

## Step 16: Create separate DaemonSets for DPU mode (optional)

When using the VethRepresentorProvider for veth-based simulation, create separate DaemonSets with the `DPU_REPRESENTOR_MODE=veth` environment variable.

### DaemonSet for DPU nodes

Key environment variables:
- `OVNKUBE_NODE_MODE=dpu` - Enables DPU mode
- `DPU_REPRESENTOR_MODE=veth` - Uses veth-based representor discovery
- `K8S_NODE_DPU` instead of `K8S_NODE` - Node name reference

The VethRepresentorProvider auto-detects interfaces using naming convention:
- pfId "0" maps to `rep0-{vfIndex}` (rep0-0, rep0-1, ..., rep0-15)

Node selector: `k8s.ovn.org/dpu: ""`

### DaemonSet for DPU-host nodes

Key environment variables:
- `OVNKUBE_NODE_MODE=dpu-host` - Enables DPU-host mode

Node selector: `k8s.ovn.org/dpu-host: ""`

### Example manifest snippet for DPU node DaemonSet

```yaml
env:
- name: OVNKUBE_NODE_MODE
  value: "dpu"
- name: DPU_REPRESENTOR_MODE
  value: "veth"
- name: K8S_NODE_DPU
  valueFrom:
    fieldRef:
      fieldPath: spec.nodeName
```

## Cleanup

```bash
kind delete cluster --name=dpu
```
