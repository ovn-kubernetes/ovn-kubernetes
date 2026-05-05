#!/bin/bash
# test-mnp-flat-overlay.sh
#
# Mirrors: test_negative_ingress_multi_network_policy (CNV-10645) from
# openshift-virtualization-tests, using KubeVirt VMs with cloud-init.
#
# Usage:
#   export KUBECONFIG=/path/to/kubeconfig
#   ./test-mnp-flat-overlay.sh
#
set -euo pipefail

NAMESPACE="test-mnp-flat-overlay"
NAD_NAME="flat-l2-nad"
NETWORK_NAME="flat-l2-network"
VMA_IP="10.200.0.1"
VMB_IP="10.200.0.2"
NON_EXISTENT_IP="10.200.0.123/32"
VM_IMAGE="${VM_IMAGE:-quay.io/containerdisks/fedora:41}"
VM_USER="fedora"
VM_PASS="fedora"
TIMEOUT_VM_READY=300
TIMEOUT_PING=180
OVN_NAMESPACE="${OVN_NAMESPACE:-openshift-ovn-kubernetes}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*"; }
pass()  { echo -e "${GREEN}[PASS]${NC}  $*"; }

MNP_WAS_ENABLED=""
SSH_KEY_DIR=$(mktemp -d)
SSH_KEY="${SSH_KEY_DIR}/id_test"

cleanup() {
    info "Cleaning up namespace ${NAMESPACE}..."
    kubectl delete namespace "${NAMESPACE}" --ignore-not-found --wait=false 2>/dev/null || true
    rm -rf "${SSH_KEY_DIR}" 2>/dev/null || true

    if [ -n "${MNP_WAS_ENABLED}" ] && [ "${MNP_WAS_ENABLED}" != "true" ]; then
        info "Restoring useMultiNetworkPolicy to '${MNP_WAS_ENABLED}'..."
        kubectl patch network.operator.openshift.io cluster --type=merge \
            -p "{\"spec\":{\"useMultiNetworkPolicy\":${MNP_WAS_ENABLED}}}" 2>/dev/null || true
    fi
}

trap cleanup EXIT

# ── Generate temporary SSH key pair ──────────────────────────────────
info "Generating temporary SSH key pair..."
ssh-keygen -t ed25519 -f "${SSH_KEY}" -N "" -q
SSH_PUB_KEY=$(cat "${SSH_KEY}.pub")
info "  Key: ${SSH_KEY}"

# ── Helper: run command in VM via virtctl ssh ────────────────────────
run_in_vm() {
    local vm="$1"
    shift
    virtctl ssh \
        --local-ssh=true \
        -t "-o StrictHostKeyChecking=no" \
        -t "-o UserKnownHostsFile=/dev/null" \
        -t "-o LogLevel=ERROR" \
        -i "${SSH_KEY}" \
        -l "${VM_USER}" \
        -n "${NAMESPACE}" \
        --command "$*" \
        "${vm}" 2>/dev/null
}

# ── Enable multi-network policy ─────────────────────────────────────
info "Checking useMultiNetworkPolicy status..."
MNP_WAS_ENABLED=$(kubectl get network.operator.openshift.io cluster \
    -o jsonpath='{.spec.useMultiNetworkPolicy}' 2>/dev/null || echo "false")

if [ "${MNP_WAS_ENABLED}" != "true" ]; then
    info "useMultiNetworkPolicy is '${MNP_WAS_ENABLED}', enabling it..."
    kubectl patch network.operator.openshift.io cluster --type=merge \
        -p '{"spec":{"useMultiNetworkPolicy":true}}'

    info "Waiting for MultiNetworkPolicy CRD to appear..."
    elapsed=0
    while ! kubectl get crd multi-networkpolicies.k8s.cni.cncf.io &>/dev/null; do
        if [ ${elapsed} -ge 180 ]; then
            fail "MultiNetworkPolicy CRD did not appear within 180s"; exit 1
        fi
        sleep 5; elapsed=$((elapsed + 5))
    done
    info "CRD is available"

    info "Waiting for ovnkube configmap to have enable-multi-networkpolicy=true..."
    elapsed=0
    while true; do
        if kubectl get configmap -n "${OVN_NAMESPACE}" ovnkube-config \
            -o jsonpath='{.data.ovnkube\.conf}' 2>/dev/null \
            | grep -q 'enable-multi-networkpolicy=true'; then
            info "ovnkube-config configmap updated"; break
        fi
        if [ ${elapsed} -ge 180 ]; then
            fail "ovnkube-config was not updated within 180s"; exit 1
        fi
        info "  Waiting... (${elapsed}s)"; sleep 10; elapsed=$((elapsed + 10))
    done

    info "Waiting for all ovnkube-node pods to roll out..."
    kubectl rollout status daemonset/ovnkube-node -n "${OVN_NAMESPACE}" --timeout=600s
else
    info "useMultiNetworkPolicy is already 'true'"
    if ! kubectl get crd multi-networkpolicies.k8s.cni.cncf.io &>/dev/null; then
        fail "useMultiNetworkPolicy=true but CRD is missing."; exit 1
    fi
fi

info "Multi-network policy is enabled and ready"

# ── Create namespace ─────────────────────────────────────────────────
info "Cleaning up any previous run..."
kubectl delete namespace "${NAMESPACE}" --ignore-not-found --wait=true 2>/dev/null || true
info "Creating namespace ${NAMESPACE}..."
kubectl create namespace "${NAMESPACE}"
kubectl label namespace "${NAMESPACE}" \
    pod-security.kubernetes.io/enforce=privileged \
    pod-security.kubernetes.io/warn=privileged \
    --overwrite

# ── Create IPAMless layer2 NAD ──────────────────────────────────────
info "Creating IPAMless layer2 NetworkAttachmentDefinition..."
kubectl apply -f - <<HEREDOC
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: ${NAD_NAME}
  namespace: ${NAMESPACE}
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "${NETWORK_NAME}",
      "type": "ovn-k8s-cni-overlay",
      "topology": "layer2",
      "netAttachDefName": "${NAMESPACE}/${NAD_NAME}"
    }
HEREDOC

sleep 3

# ── Create VMs ───────────────────────────────────────────────────────
# Note: Python test forces both VMs on worker_node1 via nodeSelector.
# We omit nodeSelector to avoid scheduling failures on resource-
# constrained nodes; for layer2 overlay the test is valid cross-node.
info "Creating vm-a (${VMA_IP}/24) and vm-b (${VMB_IP}/24)..."
kubectl apply -f - <<HEREDOC
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: vm-a
  namespace: ${NAMESPACE}
  labels:
    kubevirt.io/vm: vm-a
spec:
  running: true
  template:
    metadata:
      labels:
        kubevirt.io/vm: vm-a
        kubevirt.io/domain: vm-a
    spec:
      terminationGracePeriodSeconds: 30
      domain:
        cpu:
          cores: 1
        memory:
          guest: 1Gi
        devices:
          rng: {}
          disks:
          - disk:
              bus: virtio
            name: containerdisk
          - disk:
              bus: virtio
            name: cloudinitdisk
          interfaces:
          - masquerade: {}
            name: default
          - bridge: {}
            name: ${NAD_NAME}
      networks:
      - name: default
        pod: {}
      - name: ${NAD_NAME}
        multus:
          networkName: ${NAD_NAME}
      volumes:
      - containerDisk:
          image: ${VM_IMAGE}
        name: containerdisk
      - name: cloudinitdisk
        cloudInitNoCloud:
          userData: |
            #cloud-config
            user: ${VM_USER}
            password: ${VM_PASS}
            chpasswd:
              expire: false
            ssh_pwauth: true
            ssh_authorized_keys:
            - ${SSH_PUB_KEY}
          networkData: |
            version: 2
            ethernets:
              enp2s0:
                addresses:
                - ${VMA_IP}/24
---
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: vm-b
  namespace: ${NAMESPACE}
  labels:
    kubevirt.io/vm: vm-b
spec:
  running: true
  template:
    metadata:
      labels:
        kubevirt.io/vm: vm-b
        kubevirt.io/domain: vm-b
    spec:
      terminationGracePeriodSeconds: 30
      domain:
        cpu:
          cores: 1
        memory:
          guest: 1Gi
        devices:
          rng: {}
          disks:
          - disk:
              bus: virtio
            name: containerdisk
          - disk:
              bus: virtio
            name: cloudinitdisk
          interfaces:
          - masquerade: {}
            name: default
          - bridge: {}
            name: ${NAD_NAME}
      networks:
      - name: default
        pod: {}
      - name: ${NAD_NAME}
        multus:
          networkName: ${NAD_NAME}
      volumes:
      - containerDisk:
          image: ${VM_IMAGE}
        name: containerdisk
      - name: cloudinitdisk
        cloudInitNoCloud:
          userData: |
            #cloud-config
            user: ${VM_USER}
            password: ${VM_PASS}
            chpasswd:
              expire: false
            ssh_pwauth: true
            ssh_authorized_keys:
            - ${SSH_PUB_KEY}
          networkData: |
            version: 2
            ethernets:
              enp2s0:
                addresses:
                - ${VMB_IP}/24
HEREDOC

# ── Wait for VMs to be ready ────────────────────────────────────────
info "Waiting for VMs to be Ready..."
for vm in vm-a vm-b; do
    elapsed=0
    while true; do
        ready=$(kubectl get vm "${vm}" -n "${NAMESPACE}" -o jsonpath='{.status.ready}' 2>/dev/null || echo "false")
        if [ "${ready}" = "true" ]; then
            info "  ${vm} is Ready"; break
        fi
        if [ ${elapsed} -ge ${TIMEOUT_VM_READY} ]; then
            fail "${vm} did not become ready within ${TIMEOUT_VM_READY}s"
            kubectl get vmi "${vm}" -n "${NAMESPACE}" 2>/dev/null || true
            exit 1
        fi
        sleep 5; elapsed=$((elapsed + 5))
        if [ $((elapsed % 30)) -eq 0 ]; then info "  Waiting for ${vm}... (${elapsed}s)"; fi
    done
done

info "VM placement:"
kubectl get vmi -n "${NAMESPACE}" -o custom-columns='NAME:.metadata.name,NODE:.status.nodeName,IP:.status.interfaces[0].ipAddress'

info "Waiting for cloud-init + SSH to be ready..."
sleep 30

# ── Verify SSH and IPs ──────────────────────────────────────────────
info "Verifying SSH connectivity and IP configuration..."
for vm in vm-a vm-b; do
    elapsed=0
    while true; do
        if run_in_vm "${vm}" "ip -4 addr show" >/dev/null 2>&1; then
            info "  ${vm}: SSH OK"
            run_in_vm "${vm}" "ip -4 addr show dev eth1 2>/dev/null || ip -4 addr show dev enp2s0 2>/dev/null || ip -4 addr show" || true
            break
        fi
        if [ ${elapsed} -ge 60 ]; then
            warn "  ${vm}: SSH not reachable after 60s, continuing anyway..."
            break
        fi
        sleep 10; elapsed=$((elapsed + 10))
    done
done

# ── Verify baseline connectivity ────────────────────────────────────
info "Verifying baseline connectivity (vmA -> vmB ping)..."
elapsed=0
baseline_ok=false
while [ ${elapsed} -lt ${TIMEOUT_PING} ]; do
    if run_in_vm "vm-a" "ping -c 3 -W 2 ${VMB_IP}" >/dev/null 2>&1; then
        baseline_ok=true
        break
    fi
    info "  Retrying baseline ping... (${elapsed}s)"
    sleep 10; elapsed=$((elapsed + 10))
done

if [ "${baseline_ok}" = "false" ]; then
    fail "Baseline connectivity failed: vmA cannot ping vmB before any policy."
    info "Debug: vmA network:"
    run_in_vm "vm-a" "ip addr; ip route" || true
    info "Debug: vmB network:"
    run_in_vm "vm-b" "ip addr; ip route" || true
    exit 1
fi
pass "Baseline connectivity OK: vmA can ping vmB"

# ── Create ingress MultiNetworkPolicy ───────────────────────────────
info "Creating ingress MultiNetworkPolicy on vmB (allow only ${NON_EXISTENT_IP})..."
kubectl apply -f - <<HEREDOC
apiVersion: k8s.cni.cncf.io/v1beta1
kind: MultiNetworkPolicy
metadata:
  name: vmb-ingress-mnp
  namespace: ${NAMESPACE}
  annotations:
    k8s.v1.cni.cncf.io/policy-for: ${NAMESPACE}/${NAD_NAME}
spec:
  podSelector:
    matchLabels:
      kubevirt.io/domain: vm-b
  policyTypes:
  - Ingress
  ingress:
  - from:
    - ipBlock:
        cidr: ${NON_EXISTENT_IP}
HEREDOC

info "Waiting for policy to take effect..."
sleep 15

# ── Verify ping is blocked ──────────────────────────────────────────
info "Verifying ingress is blocked (vmA -> vmB ping should FAIL)..."
blocked=false
for attempt in 1 2 3 4 5 6; do
    if run_in_vm "vm-a" "ping -c 3 -W 2 ${VMB_IP}" >/dev/null 2>&1; then
        warn "  Attempt ${attempt}/6: ping still succeeds (MNP not yet enforced)"
        sleep 10
    else
        blocked=true
        break
    fi
done

echo ""
echo "======================================================================"
if [ "${blocked}" = "true" ]; then
    pass "TEST PASSED: Ingress traffic is blocked by MultiNetworkPolicy"
    echo "  The MNP correctly denies ping from vmA (${VMA_IP}) to vmB (${VMB_IP})"
    echo "  because the ingress rule only allows from ${NON_EXISTENT_IP}."
else
    fail "TEST FAILED: Ingress traffic is NOT blocked by MultiNetworkPolicy"
    echo "  vmA can still ping vmB despite the ingress MNP."
    echo "  This means the MNP is not being enforced on VMs with IPAMless L2."
    echo ""
    echo "  Debug info:"
    echo "  --- MultiNetworkPolicy ---"
    kubectl get multi-networkpolicies -n "${NAMESPACE}" -o yaml 2>/dev/null
    echo ""
    echo "  --- Virt-launcher pod network status ---"
    for vm in vm-a vm-b; do
        pod=$(kubectl get pods -n "${NAMESPACE}" -l "kubevirt.io/domain=${vm}" -o name 2>/dev/null | head -1)
        if [ -n "${pod}" ]; then
            echo "  --- ${pod} ---"
            kubectl get "${pod}" -n "${NAMESPACE}" \
                -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/network-status}' 2>/dev/null \
                | python3 -m json.tool 2>/dev/null || true
        fi
    done
fi
echo "======================================================================"

[ "${blocked}" = "true" ]
