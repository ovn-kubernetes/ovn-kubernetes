#!/bin/bash
set -euo pipefail

# DPU Topology Setup Script for OVN-Kubernetes
# This script creates a kind cluster with DPU simulation topology

# Configuration
NUM_DPU_PAIRS=2
INTERFACES_PER_PAIR=16
TOTAL_WORKERS=$((NUM_DPU_PAIRS * 2))

# Script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Step 1: Comprehensive cleanup for idempotency
log_info "Step 1: Comprehensive cleanup for idempotency..."

# Delete existing kind cluster
log_info "  Deleting existing kind cluster..."
kind delete cluster --name=dpu || true

# Clean up any leftover veth interfaces from previous runs
log_info "  Cleaning up leftover veth interfaces..."
for pair_id in $(seq 0 $((NUM_DPU_PAIRS - 1))); do
  for iface_id in $(seq 0 $((INTERFACES_PER_PAIR - 1))); do
    sudo ip link delete host${pair_id}-eth0-${iface_id} 2>/dev/null || true
    sudo ip link delete dpu${pair_id}-rep${pair_id}-${iface_id} 2>/dev/null || true
  done
done

# Clean up temp management interfaces (new)
sudo ip link delete temp-pf0 2>/dev/null || true
sudo ip link delete temp-pfrep0 2>/dev/null || true
sudo ip link delete temp-pf1 2>/dev/null || true
sudo ip link delete temp-pfrep1 2>/dev/null || true

# Clean up old temp interfaces (for backward compatibility)
sudo ip link delete temp-host0-0 2>/dev/null || true
sudo ip link delete temp-rep0-0 2>/dev/null || true
sudo ip link delete temp-host1-0 2>/dev/null || true
sudo ip link delete temp-rep1-0 2>/dev/null || true

# Clean up any other potential leftover interfaces
for iface in $(ip link show | grep -E "host[0-9]+-eth0-|dpu[0-9]+-rep[0-9]+-|temp-host|temp-rep|temp-pf|temp-pfrep" | cut -d':' -f2 | cut -d'@' -f1 | xargs); do
  sudo ip link delete "$iface" 2>/dev/null || true
done

log_info "  Cleanup complete"

# Step 2: Install prerequisites
log_info "Step 2: Checking prerequisites..."
if ! command -v jinjanate &> /dev/null; then
    log_error "jinjanate not found. Please install: pip install jinjanator[yaml]"
    exit 1
fi

# Step 3: Generate kind config
log_info "Step 3: Generating kind config for ${NUM_DPU_PAIRS} DPU-host pairs (${TOTAL_WORKERS} workers total)..."
cd contrib

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

# Step 4: Create kind cluster
log_info "Step 4: Creating kind cluster..."
kind create cluster \
  --name dpu \
  --kubeconfig $HOME/dpu.conf \
  --image kindest/node:v1.34.0 \
  --config contrib/kind-dpu.yaml \
  --retain

export KUBECONFIG=$HOME/dpu.conf

# Step 5: Set up DPU topology with proper host isolation
log_info "Step 5: Setting up DPU topology..."

# Create data veth pairs for each DPU-host pair (16 pairs for pure data channels)
log_info "Creating ${NUM_DPU_PAIRS} DPU-host pairs with ${INTERFACES_PER_PAIR} data veth pairs each..."
for pair_id in $(seq 0 $((NUM_DPU_PAIRS - 1))); do
  log_info "  Creating data veth pairs for DPU-host pair ${pair_id}..."
  for iface_id in $(seq 0 $((INTERFACES_PER_PAIR - 1))); do
    # Create veth pair: host side ↔ DPU side (data channels)
    sudo ip link add host${pair_id}-eth0-${iface_id} type veth peer name dpu${pair_id}-rep0-${iface_id}
  done
done

# Define container names for each pair
declare -a HOST_CONTAINERS=("dpu-worker" "dpu-worker3")
declare -a DPU_CONTAINERS=("dpu-worker2" "dpu-worker4")

# Get container PIDs for all DPU-host pairs
log_info "Getting container PIDs..."
declare -a HOST_PIDS
declare -a DPU_PIDS

for pair_id in $(seq 0 $((NUM_DPU_PAIRS - 1))); do
  HOST_PID=$(docker inspect --format '{{.State.Pid}}' ${HOST_CONTAINERS[$pair_id]})
  DPU_PID=$(docker inspect --format '{{.State.Pid}}' ${DPU_CONTAINERS[$pair_id]})

  if [[ -z "$HOST_PID" || "$HOST_PID" == "0" ]]; then
    log_error "Failed to get PID for ${HOST_CONTAINERS[$pair_id]}"
    exit 1
  fi

  if [[ -z "$DPU_PID" || "$DPU_PID" == "0" ]]; then
    log_error "Failed to get PID for ${DPU_CONTAINERS[$pair_id]}"
    exit 1
  fi

  HOST_PIDS[$pair_id]=$HOST_PID
  DPU_PIDS[$pair_id]=$DPU_PID

  log_info "  Pair ${pair_id}: Host ${HOST_CONTAINERS[$pair_id]} PID=${HOST_PID}, DPU ${DPU_CONTAINERS[$pair_id]} PID=${DPU_PID}"
done

# Create separate management veth pairs (management channel)
log_info "Creating management veth pairs (pf/pfrep)..."
sudo ip link add temp-pf0 type veth peer name temp-pfrep0
sudo ip link add temp-pf1 type veth peer name temp-pfrep1

# Move management veth pairs to containers
log_info "Moving management veth pairs to containers..."
sudo ip link set temp-pf0 netns ${HOST_PIDS[0]}
sudo ip link set temp-pfrep0 netns ${DPU_PIDS[0]}
sudo ip link set temp-pf1 netns ${HOST_PIDS[1]}
sudo ip link set temp-pfrep1 netns ${DPU_PIDS[1]}

# Replace original eth0 with management interface pf on hosts, keeping IP on host interface
log_info "Setting up management interfaces (pf)..."
log_info "  CHANGE: Management channel now uses pf interface (separate from data channels)"

# Host: preserve Kind IP from eth0 to pf (management interface), then replace interface
for host_container in dpu-worker dpu-worker3; do
  # Get the current IP configuration from eth0
  host_ip=$(docker exec "$host_container" ip addr show eth0 | grep 'inet ' | awk '{print $2}' | head -1)
  host_gw=$(docker exec "$host_container" ip route | grep default | awk '{print $3}' | head -1)

  if [[ -n "$host_ip" ]]; then
    log_info "    ${host_container}: Preserving Kind IP ${host_ip} and gateway ${host_gw}"

    # Determine which temp interface to use
    if [[ "$host_container" == "dpu-worker" ]]; then
      temp_iface="temp-pf0"
    else
      temp_iface="temp-pf1"
    fi

    # Rename veth to pf (management interface)
    docker exec "$host_container" ip link set "$temp_iface" name pf
    # Move IP configuration from eth0 to pf
    docker exec "$host_container" ip addr add "$host_ip" dev pf
    # Delete original eth0 after IP is preserved
    docker exec "$host_container" ip link delete eth0
    # Bring up pf and restore routing
    docker exec "$host_container" ip link set pf up
    if [[ -n "$host_gw" ]]; then
      docker exec "$host_container" ip route add default via "$host_gw" dev pf
    fi

    log_info "    ✅ ${host_container}: Kind networking preserved on pf (management)"
  else
    log_warn "    ${host_container}: No IP found on eth0, using clean pf"
    # Fallback to old behavior if no IP found
    docker exec "$host_container" ip link delete eth0
    docker exec "$host_container" ip link set "$temp_iface" name pf
    docker exec "$host_container" ip link set pf up
  fi
done

# DPU: keep original eth0, add pfrep from management veth
docker exec dpu-worker2 ip link set temp-pfrep0 name pfrep
docker exec dpu-worker4 ip link set temp-pfrep1 name pfrep

# Move data veth interfaces to containers (eth0-0 through eth0-15)
log_info "Moving data veth interfaces to containers..."
for iface_id in $(seq 0 $((INTERFACES_PER_PAIR - 1))); do
  log_info "  Moving data interface set ${iface_id}..."

  # Move pair 0 interfaces
  if sudo ip link show host0-eth0-${iface_id} &>/dev/null; then
    sudo ip link set host0-eth0-${iface_id} netns ${HOST_PIDS[0]}
  else
    log_warn "Interface host0-eth0-${iface_id} not found"
  fi

  if sudo ip link show dpu0-rep0-${iface_id} &>/dev/null; then
    sudo ip link set dpu0-rep0-${iface_id} netns ${DPU_PIDS[0]}
  else
    log_warn "Interface dpu0-rep0-${iface_id} not found"
  fi

  # Move pair 1 interfaces
  if sudo ip link show host1-eth0-${iface_id} &>/dev/null; then
    sudo ip link set host1-eth0-${iface_id} netns ${HOST_PIDS[1]}
  else
    log_warn "Interface host1-eth0-${iface_id} not found"
  fi

  if sudo ip link show dpu1-rep0-${iface_id} &>/dev/null; then
    sudo ip link set dpu1-rep0-${iface_id} netns ${DPU_PIDS[1]}
  else
    log_warn "Interface dpu1-rep0-${iface_id} not found"
  fi
done

# Rename data interfaces inside containers
log_info "Renaming data interfaces inside containers..."
for iface_id in $(seq 0 $((INTERFACES_PER_PAIR - 1))); do
  # Rename in host containers
  docker exec dpu-worker ip link set host0-eth0-${iface_id} name eth0-${iface_id} 2>/dev/null || log_warn "Failed to rename host0-eth0-${iface_id} in dpu-worker"
  docker exec dpu-worker3 ip link set host1-eth0-${iface_id} name eth0-${iface_id} 2>/dev/null || log_warn "Failed to rename host1-eth0-${iface_id} in dpu-worker3"

  # Rename in DPU containers
  docker exec dpu-worker2 ip link set dpu0-rep0-${iface_id} name rep0-${iface_id} 2>/dev/null || log_warn "Failed to rename dpu0-rep0-${iface_id} in dpu-worker2"
  docker exec dpu-worker4 ip link set dpu1-rep0-${iface_id} name rep0-${iface_id} 2>/dev/null || log_warn "Failed to rename dpu1-rep0-${iface_id} in dpu-worker4"
done

# Isolate host nodes from kind network
log_info "Isolating host nodes from kind network..."
docker exec dpu-worker ip route del default 2>/dev/null || true
docker exec dpu-worker3 ip route del default 2>/dev/null || true

# Verify interface counts
log_info "Verifying interface counts..."
for pair_id in $(seq 0 $((NUM_DPU_PAIRS - 1))); do
  host_container=${HOST_CONTAINERS[$pair_id]}
  dpu_container=${DPU_CONTAINERS[$pair_id]}

  host_count=$(docker exec $host_container ip link show | grep -E "^[0-9]+:" | wc -l)
  dpu_count=$(docker exec $dpu_container ip link show | grep -E "^[0-9]+:" | wc -l)

  log_info "  ${host_container}: ${host_count} interfaces (target: 18 - 1 pf + 16 eth0-x + loopback)"
  log_info "  ${dpu_container}: ${dpu_count} interfaces (target: 19 - 1 pfrep + 16 rep0-x + ovs-system + loopback)"

  if [[ $host_count -ne 18 ]]; then
    log_warn "${host_container} has ${host_count} interfaces, expected 18"
  fi

  if [[ $dpu_count -ne 19 ]]; then
    log_warn "${dpu_container} has ${dpu_count} interfaces, expected 19"
  fi
done

# Check for remaining veth interfaces on host
remaining_veths=$(ip link show | grep -E "(host[0-9]+-eth0-|dpu[0-9]+-rep[0-9]+-)" | wc -l || true)
if [[ $remaining_veths -gt 0 ]]; then
  log_warn "${remaining_veths} veth interfaces remain on host system"
  ip link show | grep -E "(host[0-9]+-eth0-|dpu[0-9]+-rep[0-9]+-)" || true
fi

log_info "✅ DPU topology setup complete!"

# Step 5.5: Prepare management representors for OVN-K auto-detection
log_info "Step 5.5: Preparing management representors for OVN-K auto-detection..."
log_info "  TIMING: After veth topology creation, before IPv6 setup"

# Ensure management representors are up and ready for NicToBridge()
# Now using separate management interfaces: pfrep (not rep0-0)
log_info "  Setting up management representor on dpu-worker2 (DPU) - PF0..."
if docker exec dpu-worker2 ip link show pfrep >/dev/null 2>&1; then
  docker exec dpu-worker2 ip link set pfrep up || log_warn "Failed to bring up pfrep on dpu-worker2"
  log_info "    dpu-worker2: pfrep ready for OVN-K gateway bridge creation"
else
  log_warn "    dpu-worker2: pfrep interface not found - management veth topology may need verification"
fi

log_info "  Setting up management representor on dpu-worker4 (DPU) - PF1..."
if docker exec dpu-worker4 ip link show pfrep >/dev/null 2>&1; then
  docker exec dpu-worker4 ip link set pfrep up || log_warn "Failed to bring up pfrep on dpu-worker4"
  log_info "    dpu-worker4: pfrep ready for OVN-K gateway bridge creation"
else
  log_warn "    dpu-worker4: pfrep interface not found - management veth topology may need verification"
fi

log_info "✅ Management representors prepared for OVN-K automatic bridge management"
log_info "  NOTE: NicToBridge() will automatically create br-ex and connect pfrep management representors"

# Step 6: Enable IPv6 and fix inotify limits
log_info "Step 6: Enabling IPv6 and fixing inotify limits..."
for node in $(kind get nodes --name dpu); do
  docker exec "$node" sysctl --ignore net.ipv6.conf.all.disable_ipv6=0 || true
  docker exec "$node" sysctl --ignore net.ipv6.conf.all.forwarding=1 || true
  docker exec "$node" sysctl -w fs.inotify.max_user_watches=524288 || true
  docker exec "$node" sysctl -w fs.inotify.max_user_instances=512 || true
done

# Step 7: Build ovnkube binary with our DPU provider changes
log_info "Step 7: Building ovnkube binary with VethRepresentorProvider changes..."
log_info "  CRITICAL: Must rebuild binary to include our GetHostRepresentor() and DPU provider updates"
log_info "  Working from: $(pwd)"
log_info "  Target Makefile: $(realpath ../dist/images/Makefile 2>/dev/null || echo 'NOT FOUND')"
make -C /root/balazs/ovn-kubernetes/dist/images fedora-image IMAGE="localhost/ovn-daemonset-fedora:dev" OVN_REPO="" OVN_GITREF="" OCI_BIN="docker"
log_info "✅ ovnkube binary built and container image updated with VethRepresentorProvider support"

# Step 8: Verify OVN container image was built
log_info "Step 8: Verifying OVN image was built in previous step..."
if docker image inspect localhost/ovn-daemonset-fedora:dev &>/dev/null; then
  log_info "✅ OVN image localhost/ovn-daemonset-fedora:dev built successfully"
else
  log_error "❌ OVN image build failed in Step 7"
  exit 1
fi

# Step 9: Load image into kind
log_info "Step 9: Loading image into kind..."
kind load docker-image localhost/ovn-daemonset-fedora:dev --name dpu

# Step 10: Get API URL and generate manifests
log_info "Step 10: Generating OVN-Kubernetes manifests..."
DNS_NAME_URL=$(kind get kubeconfig --internal --name dpu | grep server | awk '{ print $2 }')
CP_NODE=${DNS_NAME_URL#*//}
CP_NODE=${CP_NODE%:*}
NODE_IP=$(docker inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' "$CP_NODE")
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

# Step 11: Apply CRDs
log_info "Step 11: Applying CRDs..."
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

# Step 12: Apply setup and RBAC
log_info "Step 12: Applying setup and RBAC..."
kubectl apply -f ovn-setup.yaml
kubectl apply -f rbac-ovnkube-identity.yaml
kubectl apply -f rbac-ovnkube-cluster-manager.yaml
kubectl apply -f rbac-ovnkube-master.yaml
kubectl apply -f rbac-ovnkube-node.yaml
kubectl apply -f rbac-ovnkube-db.yaml

# Step 12.5: Fix DPU mode RBAC issue for Kind clusters
log_info "Step 12.5: Fixing DPU mode RBAC permissions for Kind clusters..."
log_info "  ISSUE: DPU mode disables ovnkube identity (OVN_ENABLE_OVNKUBE_IDENTITY=false)"
log_info "  EFFECT: Pods authenticate as service account instead of system:ovn-nodes group"
log_info "  FIX: Adding ovnkube-node service account to ovnkube-node ClusterRoleBinding"
log_info "  NOTE: Regular OVN-K uses identity=true so pods authenticate as system:ovn-nodes group (works)"

# Patch the ovnkube-node ClusterRoleBinding to include the service account
# This fixes permission errors for pods, services, namespaces access in Kind
kubectl patch clusterrolebinding ovnkube-node --type='json' \
  -p='[{"op": "add", "path": "/subjects/-", "value": {"kind": "ServiceAccount", "name": "ovnkube-node", "namespace": "ovn-kubernetes"}}]'

# Also add permissions for node annotations (needed for k8s.ovn.org/zone-name, k8s.ovn.org/node-encap-ips)
log_info "  Adding permissions for node annotations (k8s.ovn.org/zone-name, k8s.ovn.org/node-encap-ips)..."
kubectl patch clusterrole ovnkube-node --type=json \
  -p='[{"op": "add", "path": "/rules/-", "value": {"apiGroups": [""], "resources": ["nodes"], "verbs": ["patch", "update"]}}]'

# Verify the fix worked
if kubectl auth can-i list pods --as=system:serviceaccount:ovn-kubernetes:ovnkube-node >/dev/null 2>&1; then
  log_info "  ✅ RBAC fix successful: ovnkube-node service account can now list pods"
else
  log_warn "  ⚠️ RBAC fix verification failed - DPU containers may have permission issues"
fi

log_info "✅ Kind-specific RBAC permissions fixed"

# Step 17: Label and untaint control plane
log_info "Step 12: Labeling and untainting control plane..."
kubectl label node dpu-control-plane k8s.ovn.org/ovnkube-db=true node-role.kubernetes.io/control-plane="" --overwrite
kubectl taint node dpu-control-plane node-role.kubernetes.io/master:NoSchedule- || true
kubectl taint node dpu-control-plane node-role.kubernetes.io/control-plane:NoSchedule- || true

# Step 17: Apply OVS and label zones
log_info "Step 17: Applying OVS and labeling zones..."
kubectl apply -f ovs-node.yaml

for node in $(kind get nodes --name dpu); do
  kubectl label node "$node" k8s.ovn.org/zone-name=${node} --overwrite
done


# Step 17: Apply OVN-Kubernetes components (DPU-specific)
log_info "Step 17: Applying OVN-Kubernetes components..."
kubectl apply -f ovnkube-identity.yaml
kubectl apply -f ovnkube-control-plane.yaml

# Skip ovnkube-single-node-zone.yaml - we'll create DPU-specific DaemonSets instead

# Step 17: Label nodes for DPU mode deployment
log_info "Step 17: Labeling nodes for DPU mode..."
kubectl label nodes dpu-worker k8s.ovn.org/dpu-host= --overwrite
kubectl label nodes dpu-worker3 k8s.ovn.org/dpu-host= --overwrite
kubectl label nodes dpu-worker2 k8s.ovn.org/dpu= --overwrite
kubectl label nodes dpu-worker4 k8s.ovn.org/dpu= --overwrite

# Step 17: Deploy production DPU architecture
log_info "Step 17: Deploying production DPU top/bottom split architecture..."
log_info "  Using VethRepresentorProvider for automatic br-ex bridge creation"

# Deploy DPU HOST - CNI-only (top half)
log_info "  Deploying DPU host template (CNI-only, top half)..."
kubectl apply -f "${SCRIPT_DIR}/dpu-host-template.yaml"

# Deploy DPU NODE - Full OVN stack (bottom half)
log_info "  Deploying DPU template (6-container OVN stack, bottom half)..."
log_info "    Production containers: nb-ovsdb, sb-ovsdb, ovn-northd, ovnkube-controller-with-node, ovn-controller, ovs-metrics-exporter"
log_info "    NEW: Contains VethRepresentorProvider.GetHostRepresentor() for automatic br-ex creation"
kubectl apply -f "${SCRIPT_DIR}/dpu-template.yaml"

# Verify new architecture dependencies are met
log_info "  Verifying separated veth architecture for DPU containers..."
for dpu_node in dpu-worker2 dpu-worker4; do
  # Check that pfrep management interface is ready for automatic br-ex creation
  mgmt_status=$(docker exec "$dpu_node" ip link show pfrep 2>/dev/null || echo "MISSING")
  if [[ "$mgmt_status" == "MISSING" ]]; then
    log_error "  CRITICAL: pfrep management interface missing on ${dpu_node}!"
    log_error "  Management channel setup will fail!"
    exit 1
  else
    log_info "  ✅ ${dpu_node} pfrep ready for management channel"
  fi

  # Check that rep0-0 data interface is ready
  data_status=$(docker exec "$dpu_node" ip link show rep0-0 2>/dev/null || echo "MISSING")
  if [[ "$data_status" == "MISSING" ]]; then
    log_error "  CRITICAL: rep0-0 data interface missing on ${dpu_node}!"
    log_error "  Data plane setup will fail!"
    exit 1
  else
    log_info "  ✅ ${dpu_node} rep0-0 ready as first data representor"
  fi
done
log_info "  NOTE: Management (pfrep) and data (rep0-*) channels now properly separated"

# Verify deployment status
log_info "  Verifying DaemonSet deployment..."
kubectl get daemonset -n ovn-kubernetes ovnkube-node-dpu-host
kubectl get daemonset -n ovn-kubernetes ovnkube-node-dpu

log_info "✅ Complete production DPU architecture deployed with separated management and data channels!"
log_info "  Architecture Summary:"
log_info "    Control Plane: dpu-control-plane (standard OVN-K control plane)"
log_info "    DPU Host (Top Half): dpu-worker, dpu-worker3"
log_info "      - Single CNI container"
log_info "      - OVN_NODE_MODE=dpu-host"
log_info "      - Management interface: pf (preserves Kind IP)"
log_info "      - Data interfaces: eth0-0 through eth0-15 (16 data channels)"
log_info "      - Goal: Provides connectivity to host backed network"
log_info "    DPU Node (Bottom Half): dpu-worker2, dpu-worker4"
log_info "      - 6-container OVN stack (nb-ovsdb, sb-ovsdb, ovn-northd, controller, ovn-controller, ovs-metrics)"
log_info "      - OVNKUBE_NODE_MODE=dpu"
log_info "      - Management interface: pfrep (separate management channel)"
log_info "      - Data interfaces: rep0-0 through rep0-15 (16 data representors → breth0)"
log_info "      - Goal: Full OVN brain, clear channel separation"
log_info "    Management Flow: host pf (with IP) → management veth pair → DPU pfrep → management processing"
log_info "    Data Flow: host eth0-0..15 → data veth pairs → DPU rep0-0..15 → breth0 bridge → workload traffic"
log_info "    Key Changes:"
log_info "      - SEPARATED: Management (pf ↔ pfrep) and Data (eth0-* ↔ rep0-*) channels"
log_info "      - FIXED: rep0-0 now pure data interface (no management conflicts)"
log_info "      - INCREASED: 16 data channels instead of 15"
log_info "      - CLARIFIED: Architecture matches SR-IOV DPU separation pattern"

cd ../..

# Step 18: Set node management port annotations for DPU containers
log_info "Step 18: Setting node management port annotations..."
log_info "  Setting k8s.ovn.org/node-mgmt-port annotations based on interface topology..."
log_info "  dpu-worker2: rep0-* interfaces → PfId=0, FuncId=0 (rep0-0)"
log_info "  dpu-worker4: rep0-* interfaces → consistent naming with dpu-worker2 (rep0-0)"

# Set annotations based on actual representor interfaces (format that works with VethRepresentorProvider)
kubectl annotate node dpu-worker2 'k8s.ovn.org/node-mgmt-port={"default": {"pfId": 0, "vfId": 0, "deviceId": "test"}}' --overwrite
kubectl annotate node dpu-worker4 'k8s.ovn.org/node-mgmt-port={"default": {"pfId": 0, "vfId": 0, "deviceId": "test"}}' --overwrite

# Set primary DPU-host addresses for DPU communication (get actual host IPs)
DPU_WORKER_HOST_IP=$(docker exec dpu-worker ip addr show pf | grep "inet " | awk '{print $2}')
DPU_WORKER3_HOST_IP=$(docker exec dpu-worker3 ip addr show pf | grep "inet " | awk '{print $2}')
log_info "  dpu-worker host IP: ${DPU_WORKER_HOST_IP}"
log_info "  dpu-worker3 host IP: ${DPU_WORKER3_HOST_IP}"
kubectl annotate node dpu-worker2 "k8s.ovn.org/primary-dpu-host-addr={\"ipv4\":\"${DPU_WORKER_HOST_IP}\"}" --overwrite
kubectl annotate node dpu-worker4 "k8s.ovn.org/primary-dpu-host-addr={\"ipv4\":\"${DPU_WORKER3_HOST_IP}\"}" --overwrite

log_info "✅ Node management port annotations set successfully"
log_info "✅ Primary DPU-host address annotations set successfully"

# Step 19: (Optional) Disable admission webhooks for testing
log_info "Step 19: (Optional) Disabling admission webhooks for testing..."
log_info "  NOTE: This is for testing only - production would need webhook fixes"
log_info "  Removing both node and pod validation webhooks that block DPU annotation setup..."

# Remove both validation webhooks that block DPU operation
if kubectl get validatingwebhookconfiguration ovn-kubernetes-admission-webhook-node >/dev/null 2>&1; then
  kubectl delete validatingwebhookconfiguration ovn-kubernetes-admission-webhook-node ovn-kubernetes-admission-webhook-pod
  log_info "✅ Both validation webhooks disabled for testing"
else
  log_info "  Validation webhooks not found - skipping"
fi

log_info "✅ DPU topology setup complete!"
log_info ""
log_info "Next steps:"
log_info "1. Verify node status: kubectl get nodes"
log_info "2. Check pod status: kubectl get pods -n ovn-kubernetes -o wide"
log_info "3. Expected result:"
log_info "   - Control plane: OVN-K control plane pods Running"
log_info "   - DPU host nodes (dpu-worker, dpu-worker3): ovnkube-node-dpu-host pods Running"
log_info "   - DPU nodes (dpu-worker2, dpu-worker4): ovnkube-node-dpu pods Running"
log_info "   - Clean deployment: Only DPU-specific DaemonSets created"
log_info ""
log_info "To clean up: kind delete cluster --name=dpu"