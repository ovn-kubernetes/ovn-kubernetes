#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Export debug logs from the dpu-simulator dual-cluster Kind setup.
#
# dpu-simulator creates two Kubernetes clusters backed by two Kind clusters:
#   - dpu-sim-host (dpu-host mode): ovnkube-node-dpu-host, ovnkube-node, ovs-node
#   - dpu-sim-dpu  (dpu mode):     ovnkube-node-dpu
#
# Kubeconfigs (default):
#   ${DPU_SIM_PATH}/kubeconfig/dpu-sim-host.kubeconfig
#   ${DPU_SIM_PATH}/kubeconfig/dpu-sim-dpu.kubeconfig
#
# Archive layout (example: /tmp/kind-dpu-offload-logs):
#   README.txt
#   kind/
#     dpu-sim-host/                 # kind export logs for host Kind cluster
#     dpu-sim-dpu/                  # kind export logs for DPU Kind cluster
#   kubernetes/
#     dpu-sim-host/                 # kubectl logs via host kubeconfig
#       nodes/
#       events/
#       ovn-kubernetes/
#       kube-system/
#       ...
#     dpu-sim-dpu/                  # kubectl logs via DPU kubeconfig
#
# Usage: ./export-dpu-sim-logs.sh [logs_dir]
# Environment:
#   DPU_SIM_PATH   path to dpu-simulator checkout
#   HOST_CLUSTER   host cluster name (default: dpu-sim-host)
#   DPU_CLUSTER    DPU cluster name (default: dpu-sim-dpu)
#   KIND_CLUSTERS  optional space-separated Kind cluster names to export

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OVN_KUBERNETES_PATH="${OVN_KUBERNETES_PATH:-$(cd "${SCRIPT_DIR}/.." && pwd)}"
LOGS_DIR="${1:-/tmp/kind-dpu-logs}"
DPU_SIM_PATH="${DPU_SIM_PATH:-${OVN_KUBERNETES_PATH}/../dpu-simulator}"
HOST_CLUSTER="${HOST_CLUSTER:-dpu-sim-host}"
DPU_CLUSTER="${DPU_CLUSTER:-dpu-sim-dpu}"
TAIL_LINES="${TAIL_LINES:--1}"

NODE_STATUS_COLUMNS='NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,REASON:.status.conditions[?(@.type=="Ready")].reason,MESSAGE:.status.conditions[?(@.type=="Ready")].message,SCHEDULABLE:.spec.unschedulable,CPU:.status.capacity.cpu,MEMORY:.status.capacity.memory,INTERNAL-IP:.status.addresses[?(@.type=="InternalIP")].address,HOSTNAME:.metadata.labels.kubernetes\.io/hostname'
POD_STATUS_COLUMNS='NAMESPACE:.metadata.namespace,NAME:.metadata.name,NODE:.spec.nodeName,PHASE:.status.phase,READY:.status.conditions[?(@.type=="Ready")].status,RESTARTS:.status.containerStatuses[*].restartCount,REASON:.status.reason,MESSAGE:.status.message'
NS_POD_STATUS_COLUMNS='NAME:.metadata.name,NODE:.spec.nodeName,PHASE:.status.phase,READY:.status.conditions[?(@.type=="Ready")].status,RESTARTS:.status.containerStatuses[*].restartCount,REASON:.status.reason,MESSAGE:.status.message'

HOST_KUBECONFIG="${HOST_KUBECONFIG:-${DPU_SIM_PATH}/kubeconfig/${HOST_CLUSTER}.kubeconfig}"
DPU_KUBECONFIG="${DPU_KUBECONFIG:-${DPU_SIM_PATH}/kubeconfig/${DPU_CLUSTER}.kubeconfig}"

# Namespaces that matter for DPU simulator debugging. Missing namespaces are skipped.
DEBUG_NAMESPACES=(
  ovn-kubernetes
  kube-system
  frr-k8s-system
  local-path-storage
  default
)

mkdir -p "${LOGS_DIR}/kind" "${LOGS_DIR}/kubernetes"

sanitize_filename() {
  echo "$1" | tr '/:' '_'
}

discover_kind_clusters() {
  if [ -n "${KIND_CLUSTERS:-}" ]; then
    printf '%s\n' ${KIND_CLUSTERS}
    return
  fi

  local clusters=()
  local cluster
  for cluster in "${HOST_CLUSTER}" "${DPU_CLUSTER}"; do
    if kind get clusters 2>/dev/null | grep -Fxq "${cluster}"; then
      clusters+=("${cluster}")
    fi
  done

  if [ "${#clusters[@]}" -eq 0 ]; then
    while IFS= read -r cluster; do
      [ -n "${cluster}" ] && clusters+=("${cluster}")
    done < <(kind get clusters 2>/dev/null | grep -E '^dpu-sim-' || true)
  fi

  printf '%s\n' "${clusters[@]}"
}

write_archive_readme() {
  cat > "${LOGS_DIR}/README.txt" <<EOF
dpu-simulator debug archive
===========================

Host Kubernetes cluster : ${HOST_CLUSTER}
  kubeconfig            : ${HOST_KUBECONFIG}
  Kind cluster name     : ${HOST_CLUSTER}

DPU Kubernetes cluster  : ${DPU_CLUSTER}
  kubeconfig            : ${DPU_KUBECONFIG}
  Kind cluster name     : ${DPU_CLUSTER}

Kind export logs        : kind/<cluster-name>/
Kubernetes API dumps    : kubernetes/<cluster-name>/
  nodes/                : node status, describe, yaml
  events/               : cluster-wide events (all + warnings)
  <namespace>/          : per-namespace pod status, events, logs
  not-ready/            : pods not Running/Succeeded

Pod annotations       : *.yaml and *.describe.txt (not in *-status.txt)

OVN-Kubernetes pod logs : kubernetes/*/ovn-kubernetes/
  host cluster          : ovnkube-node-dpu-host-*, ovnkube-node-*, ovnkube-control-plane-*
  DPU cluster           : ovnkube-node-dpu-*

Start here for failures   :
  kubernetes/${HOST_CLUSTER}/events/warning-events.txt
  kubernetes/${HOST_CLUSTER}/kube-system/coredns-*.events.txt
  kubernetes/${DPU_CLUSTER}/ovn-kubernetes/ovnkube-node-dpu-*.log
EOF
}

echo "Exporting dpu-simulator debug logs to ${LOGS_DIR}"
echo "  DPU_SIM_PATH=${DPU_SIM_PATH}"
echo "  HOST_CLUSTER=${HOST_CLUSTER} (${HOST_KUBECONFIG})"
echo "  DPU_CLUSTER=${DPU_CLUSTER} (${DPU_KUBECONFIG})"
write_archive_readme

export_kind_logs() {
  local kind_cluster=$1
  local out_dir="${LOGS_DIR}/kind/${kind_cluster}"

  if ! kind get clusters 2>/dev/null | grep -Fxq "${kind_cluster}"; then
    echo "Skipping Kind export for missing cluster ${kind_cluster}"
    return 0
  fi

  echo "Exporting Kind logs for ${kind_cluster} -> ${out_dir}"
  mkdir -p "${out_dir}"
  kind export logs --name "${kind_cluster}" --verbosity 4 "${out_dir}" || true
}

while IFS= read -r kind_cluster; do
  [ -n "${kind_cluster}" ] && export_kind_logs "${kind_cluster}"
done < <(discover_kind_clusters)

kubectl_with_kubeconfig() {
  local kubeconfig=$1
  shift

  if [ ! -f "${kubeconfig}" ]; then
    echo "Missing kubeconfig: ${kubeconfig}" >&2
    return 1
  fi

  kubectl --kubeconfig "${kubeconfig}" "$@"
}

write_cluster_metadata() {
  local cluster_name=$1
  local kubeconfig=$2
  local out_dir="${LOGS_DIR}/kubernetes/${cluster_name}"

  mkdir -p "${out_dir}/nodes" "${out_dir}/events"
  {
    echo "cluster=${cluster_name}"
    echo "kubeconfig=${kubeconfig}"
    echo "host_cluster=${HOST_CLUSTER}"
    echo "dpu_cluster=${DPU_CLUSTER}"
    echo "kind_cluster=${cluster_name}"
  } > "${out_dir}/metadata.txt"

  if [ ! -f "${kubeconfig}" ]; then
    echo "kubeconfig not found: ${kubeconfig}" > "${out_dir}/kubeconfig-missing.txt"
    return 0
  fi

  kubectl_with_kubeconfig "${kubeconfig}" get nodes -o wide \
    > "${out_dir}/nodes/nodes-wide.txt" 2>&1 || true
  kubectl_with_kubeconfig "${kubeconfig}" get nodes -o "custom-columns=${NODE_STATUS_COLUMNS}" \
    > "${out_dir}/nodes/nodes-status.txt" 2>&1 || true
  kubectl_with_kubeconfig "${kubeconfig}" get nodes -o yaml \
    > "${out_dir}/nodes/nodes.yaml" 2>&1 || true

  local node safe_node
  for node in $(kubectl_with_kubeconfig "${kubeconfig}" get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    safe_node=$(sanitize_filename "${node}")
    kubectl_with_kubeconfig "${kubeconfig}" describe node "${node}" \
      > "${out_dir}/nodes/${safe_node}.describe.txt" 2>&1 || true
  done

  kubectl_with_kubeconfig "${kubeconfig}" get pods -A -o wide \
    > "${out_dir}/pods-all-wide.txt" 2>&1 || true
  kubectl_with_kubeconfig "${kubeconfig}" get pods -A -o "custom-columns=${POD_STATUS_COLUMNS}" \
    > "${out_dir}/pods-all-status.txt" 2>&1 || true
  kubectl_with_kubeconfig "${kubeconfig}" get pods -A -o yaml \
    > "${out_dir}/pods-all.yaml" 2>&1 || true

  kubectl_with_kubeconfig "${kubeconfig}" get events -A --sort-by='.lastTimestamp' \
    > "${out_dir}/events/all-events.txt" 2>&1 || true
  kubectl_with_kubeconfig "${kubeconfig}" get events -A --field-selector type=Warning --sort-by='.lastTimestamp' \
    > "${out_dir}/events/warning-events.txt" 2>&1 || true

  kubectl_with_kubeconfig "${kubeconfig}" get daemonsets,deployments,statefulsets -A \
    > "${out_dir}/workloads.txt" 2>&1 || true
  kubectl_with_kubeconfig "${kubeconfig}" get svc,endpoints,endpointslices -A \
    > "${out_dir}/services-endpoints.txt" 2>&1 || true
}

collect_pod_events() {
  local kubeconfig=$1
  local namespace=$2
  local pod=$3
  local out_file=$4

  kubectl_with_kubeconfig "${kubeconfig}" get events -n "${namespace}" \
    --field-selector "involvedObject.kind=Pod,involvedObject.name=${pod}" \
    --sort-by='.lastTimestamp' \
    > "${out_file}" 2>&1 || true
}

collect_namespace_pod_logs() {
  local cluster_name=$1
  local kubeconfig=$2
  local namespace=$3
  local out_dir="${LOGS_DIR}/kubernetes/${cluster_name}/${namespace}"

  if ! kubectl_with_kubeconfig "${kubeconfig}" get namespace "${namespace}" >/dev/null 2>&1; then
    echo "Namespace ${namespace} not found on ${cluster_name}" >&2
    return 0
  fi

  mkdir -p "${out_dir}"

  kubectl_with_kubeconfig "${kubeconfig}" get pods -n "${namespace}" -o wide \
    > "${out_dir}/pods-wide.txt" 2>&1 || true
  kubectl_with_kubeconfig "${kubeconfig}" get pods -n "${namespace}" -o "custom-columns=${NS_POD_STATUS_COLUMNS}" \
    > "${out_dir}/pods-status.txt" 2>&1 || true
  kubectl_with_kubeconfig "${kubeconfig}" get pods -n "${namespace}" -o yaml \
    > "${out_dir}/pods.yaml" 2>&1 || true
  kubectl_with_kubeconfig "${kubeconfig}" get events -n "${namespace}" --sort-by='.lastTimestamp' \
    > "${out_dir}/namespace-events.txt" 2>&1 || true

  local pods
  pods=$(kubectl_with_kubeconfig "${kubeconfig}" get pods -n "${namespace}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
  if [ -z "${pods}" ]; then
    return 0
  fi

  local pod safe_pod
  for pod in ${pods}; do
    safe_pod=$(sanitize_filename "${pod}")
    kubectl_with_kubeconfig "${kubeconfig}" describe pod -n "${namespace}" "${pod}" \
      > "${out_dir}/${safe_pod}.describe.txt" 2>&1 || true
    collect_pod_events "${kubeconfig}" "${namespace}" "${pod}" "${out_dir}/${safe_pod}.events.txt"
    kubectl_with_kubeconfig "${kubeconfig}" logs -n "${namespace}" "${pod}" \
      --all-containers=true --tail="${TAIL_LINES}" \
      > "${out_dir}/${safe_pod}.log" 2>&1 || true
    kubectl_with_kubeconfig "${kubeconfig}" logs -n "${namespace}" "${pod}" \
      --all-containers=true --previous --tail="${TAIL_LINES}" \
      > "${out_dir}/${safe_pod}.previous.log" 2>&1 || true
  done
}

collect_not_ready_pod_logs() {
  local cluster_name=$1
  local kubeconfig=$2
  local out_dir="${LOGS_DIR}/kubernetes/${cluster_name}/not-ready"

  mkdir -p "${out_dir}"

  local entries
  entries=$(kubectl_with_kubeconfig "${kubeconfig}" get pods -A --field-selector=status.phase!=Running,status.phase!=Succeeded \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
  if [ -z "${entries}" ]; then
    echo "No non-running/non-succeeded pods found" > "${out_dir}/summary.txt"
    return 0
  fi

  printf '%s\n' ${entries} > "${out_dir}/summary.txt"

  local entry namespace pod safe_name
  for entry in ${entries}; do
    namespace="${entry%%/*}"
    pod="${entry#*/}"
    safe_name=$(sanitize_filename "${namespace}_${pod}")
    kubectl_with_kubeconfig "${kubeconfig}" get pod -n "${namespace}" "${pod}" -o yaml \
      > "${out_dir}/${safe_name}.yaml" 2>&1 || true
    kubectl_with_kubeconfig "${kubeconfig}" describe pod -n "${namespace}" "${pod}" \
      > "${out_dir}/${safe_name}.describe.txt" 2>&1 || true
    collect_pod_events "${kubeconfig}" "${namespace}" "${pod}" \
      "${out_dir}/${safe_name}.events.txt"
    kubectl_with_kubeconfig "${kubeconfig}" logs -n "${namespace}" "${pod}" \
      --all-containers=true --tail="${TAIL_LINES}" \
      > "${out_dir}/${safe_name}.log" 2>&1 || true
    kubectl_with_kubeconfig "${kubeconfig}" logs -n "${namespace}" "${pod}" \
      --all-containers=true --previous --tail="${TAIL_LINES}" \
      > "${out_dir}/${safe_name}.previous.log" 2>&1 || true
  done
}

collect_cluster_logs() {
  local cluster_name=$1
  local kubeconfig=$2
  local namespace

  echo "Collecting Kubernetes logs for ${cluster_name} (${kubeconfig})"
  write_cluster_metadata "${cluster_name}" "${kubeconfig}"

  if [ ! -f "${kubeconfig}" ]; then
    return 0
  fi

  for namespace in "${DEBUG_NAMESPACES[@]}"; do
    collect_namespace_pod_logs "${cluster_name}" "${kubeconfig}" "${namespace}"
  done
  collect_not_ready_pod_logs "${cluster_name}" "${kubeconfig}"
}

collect_cluster_logs "${HOST_CLUSTER}" "${HOST_KUBECONFIG}"
collect_cluster_logs "${DPU_CLUSTER}" "${DPU_KUBECONFIG}"

echo "Finished exporting dpu-simulator debug logs to ${LOGS_DIR}"
