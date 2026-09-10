#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

set -ex

export KUBECONFIG=${KUBECONFIG:-${HOME}/ovn.conf}
export OVN_IMAGE_FAMILY=${OVN_IMAGE_FAMILY:-fedora}
export OVN_IMAGE=${OVN_IMAGE:-ovn-daemonset-${OVN_IMAGE_FAMILY}:pr}
export KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn}

# UPGRADE_STAGE selects which components this invocation upgrades:
#   all                 - upgrade everything in one go (default). The
#                         control-plane Deployment is stopped first so nodes
#                         upgrade without a cluster-manager, then it comes back
#                         with the new image.
#
# Staged upgrades come in pairs; run e2e between the two stages to test the
# version skew each pair leaves behind, then again after the second stage.
#
#   nodes-first         - upgrade the DaemonSets only. The ovnkube-control-plane
#                         Deployment is paused so helm can update its template
#                         without rolling its pods: OLD cluster-manager, NEW nodes.
#   control-plane-last  - resume the paused Deployment so the cluster-manager
#                         rolls to the new image.
#
#   control-plane-first - upgrade the cluster-manager only. The ovnkube-node
#                         DaemonSet is switched to the OnDelete update strategy
#                         so helm can update its template without rolling its
#                         pods: NEW cluster-manager, OLD nodes.
#   nodes-last          - switch the DaemonSet back to RollingUpdate so the
#                         nodes roll to the new image.
UPGRADE_STAGE=${UPGRADE_STAGE:-all}
CONTROL_PLANE_DEPLOYMENT=ovnkube-control-plane
NODE_DAEMONSETS="ovnkube-node ovnkube-single-node-zone"

SCRIPT_DIR="$(cd -- "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"

control_plane_exists() {
  kubectl -n ovn-kubernetes get deployment "$CONTROL_PLANE_DEPLOYMENT" >/dev/null 2>&1
}

# Image currently used by the running control-plane pods (not the Deployment
# template, which helm rewrites while the Deployment is paused).
control_plane_running_image() {
  local selector
  selector=$(kubectl -n ovn-kubernetes get deployment "$CONTROL_PLANE_DEPLOYMENT" -o json \
    | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')
  kubectl -n ovn-kubernetes get pod -l "$selector" -o jsonpath='{.items[*].spec.containers[0].image}' | tr ' ' '\n' | sort -u
}

# Images used by the running ovnkube-node pods, selected through each
# DaemonSet's own selector so chart label changes don't break the check.
node_running_images() {
  local selector
  for ds in $NODE_DAEMONSETS; do
    if kubectl -n ovn-kubernetes get daemonset "$ds" >/dev/null 2>&1; then
      selector=$(kubectl -n ovn-kubernetes get daemonset "$ds" -o json \
        | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')
      kubectl -n ovn-kubernetes get pod -l "$selector" \
        -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.image}{"\n"}{end}{end}'
    fi
  done | sort -u
}

# helm's three-way merge leaves fields alone that the chart did not change
# between releases, so a live updateStrategy patch survives `helm upgrade`.
set_node_daemonset_strategy() {
  local strategy=$1
  for ds in $NODE_DAEMONSETS; do
    if kubectl -n ovn-kubernetes get daemonset "$ds" >/dev/null 2>&1; then
      kubectl -n ovn-kubernetes patch daemonset "$ds" --type merge \
        -p "{\"spec\":{\"updateStrategy\":{\"type\":\"${strategy}\"}}}"
    fi
  done
}

# Stash current replica counts and scale the controller Deployments to 0 so
# their old pods exit before helm re-creates them with the new image.
# DaemonSets are left to roll via their RollingUpdate strategy.
declare -A SAVED_REPLICAS
scale_down_control_plane() {
  for d in "$CONTROL_PLANE_DEPLOYMENT"; do
    if ! kubectl -n ovn-kubernetes get deployment "$d" >/dev/null 2>&1; then
      continue
    fi
    SAVED_REPLICAS[$d]=$(kubectl -n ovn-kubernetes get deployment "$d" -o=jsonpath='{.spec.replicas}')
    kubectl -n ovn-kubernetes scale deployment "$d" --replicas=0
  done

  # Let the downscaled pods terminate before helm upgrade re-renders the
  # Deployment spec. `.status.replicas` is elided (not 0) once the Deployment
  # reaches zero, so waiting on that jsonpath never matches. Wait for the pods
  # themselves to be deleted using each Deployment's own selector.
  for d in "${!SAVED_REPLICAS[@]}"; do
    selector=$(kubectl -n ovn-kubernetes get deployment "$d" -o json \
      | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")' 2>/dev/null) || selector=""
    if [[ -n "$selector" ]]; then
      kubectl -n ovn-kubernetes wait pod -l "$selector" \
        --for=delete --timeout=120s || true
    fi
  done
}

# Belt-and-braces: if the chart's replicas differ from what was running
# before (or if helm already restored them), make sure they match the
# pre-upgrade count so subsequent e2e doesn't see an unexpected topology.
restore_control_plane() {
  for d in "${!SAVED_REPLICAS[@]}"; do
    desired=${SAVED_REPLICAS[$d]}
    current=$(kubectl -n ovn-kubernetes get deployment "$d" -o=jsonpath='{.spec.replicas}' 2>/dev/null || echo "")
    if [[ -n "$desired" && "$current" != "$desired" ]]; then
      kubectl -n ovn-kubernetes scale deployment "$d" --replicas="$desired"
    fi
    kubectl -n ovn-kubernetes rollout status deployment "$d" --timeout=300s
  done
}

# Run the helm upgrade. contrib/kind-helm.sh --deploy loads the PR image into
# KIND and runs `helm upgrade --install ovn-kubernetes` with current workflow
# env vars (OVN_GATEWAY_MODE, PLATFORM_IPV{4,6}_SUPPORT, ...).
# Chart is re-rendered from the PR branch, so chart/value changes land too.
helm_deploy() {
  # Pin ovs-node's DaemonSet updateStrategy to OnDelete through the helm
  # upgrade. helm rewrites every DS pod template with the new global.image.tag
  # (the chart has a single image setting shared by every subchart), which
  # otherwise rolls ovs-node concurrently with ovnkube-node. When the ovs
  # container on a node restarts, /var/run/openvswitch/db.sock vanishes; the
  # still-running old ovnkube-node on that node loses its ovsdb connection,
  # sees the eth0-on-breth0 binding disappear, and crashes with "phys port eth0
  # ofport changed from 1 to". The DS rollout then stalls because the first new
  # ovnkube-node pod can't reach Ready either (same torn-down OVS state).
  #
  # kind-helm.sh passes OVS_NODE_UPDATE_STRATEGY through as
  # `--set ovs-node.updateStrategy=<value>`, so the chart renders the DS with
  # OnDelete and helm's reconcile doesn't revert it. OVS keeps running on the
  # existing pods; ovnkube-node rolls against live ovsdb state.
  export OVS_NODE_UPDATE_STRATEGY=OnDelete
  "${SCRIPT_DIR}/../../contrib/kind-helm.sh" --deploy
}

wait_daemonsets() {
  for ds in $NODE_DAEMONSETS; do
    if kubectl -n ovn-kubernetes get daemonset "$ds" >/dev/null 2>&1; then
      kubectl -n ovn-kubernetes rollout status daemonset "$ds" --timeout=600s
    fi
  done
}

# Remove the control-plane taint so e2e shard-conformance workloads can
# schedule on the control-plane node (unchanged from the original script;
# kind-helm.sh's fresh-install path does the same).
remove_control_plane_taint() {
  KIND_REMOVE_TAINT=${KIND_REMOVE_TAINT:-true}
  if [ "$KIND_REMOVE_TAINT" == true ]; then
    for node in $(kubectl get nodes -l node-role.kubernetes.io/control-plane -o name); do
      kubectl taint node "$node" node-role.kubernetes.io/control-plane:NoSchedule- || true
    done
  fi
}

# Refresh the e2e test binary if the disk copy is stale.
refresh_e2e_binary() {
  ARCH=""
  case $(uname -m) in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64" ;;
  esac
  K8S_VERSION="v1.36.2"
  E2E_VERSION=$(/usr/local/bin/e2e.test --version)
  if [[ "$E2E_VERSION" != "$K8S_VERSION" ]]; then
    echo "found version $E2E_VERSION of e2e binary, need version $K8S_VERSION; downloading"
    curl -LO https://dl.k8s.io/${K8S_VERSION}/kubernetes-test-linux-${ARCH}.tar.gz
    tar xvzf kubernetes-test-linux-${ARCH}.tar.gz
    sudo mv kubernetes/test/bin/e2e.test /usr/local/bin/e2e.test
    sudo mv kubernetes/test/bin/ginkgo /usr/local/bin/ginkgo
    rm kubernetes-test-linux-${ARCH}.tar.gz
  fi
}

upgrade_all() {
  scale_down_control_plane
  helm_deploy
  restore_control_plane
  wait_daemonsets
  remove_control_plane_taint
  refresh_e2e_binary
}

upgrade_nodes_first() {
  local old_image=""
  if control_plane_exists; then
    old_image=$(control_plane_running_image)
    # A paused Deployment accepts template changes without creating a new
    # ReplicaSet, so helm can rewrite the image while the old pods keep
    # running. The old cluster-manager stays up throughout the node rollout.
    kubectl -n ovn-kubernetes rollout pause deployment "$CONTROL_PLANE_DEPLOYMENT"
  fi
  # kind-helm.sh deletes these pods after helm returns so nothing keeps the old
  # image on a no-op redeploy. Leave the control-plane out: deleting its pod
  # would only restart the old cluster-manager, which is pointless churn.
  export OVN_DEPLOY_PODS="ovnkube-identity ovnkube-node"
  helm_deploy
  wait_daemonsets
  if [[ -n "$old_image" ]]; then
    # Prove the skew we are about to test: nodes new, cluster-manager old.
    new_image=$(control_plane_running_image)
    if [[ "$new_image" != "$old_image" ]]; then
      echo "control-plane pods changed image during node-only upgrade: ${old_image} -> ${new_image}" >&2
      exit 1
    fi
    echo "control-plane still running ${old_image}; nodes upgraded to ${OVN_IMAGE}"
  fi
  remove_control_plane_taint
  refresh_e2e_binary
}

upgrade_control_plane_last() {
  if ! control_plane_exists; then
    echo "no ${CONTROL_PLANE_DEPLOYMENT} Deployment found, nothing to upgrade"
    return
  fi
  kubectl -n ovn-kubernetes rollout resume deployment "$CONTROL_PLANE_DEPLOYMENT"
  kubectl -n ovn-kubernetes rollout status deployment "$CONTROL_PLANE_DEPLOYMENT" --timeout=300s
  running=$(control_plane_running_image)
  if [[ "$running" != "$OVN_IMAGE" ]]; then
    echo "control-plane pods run ${running}, expected ${OVN_IMAGE}" >&2
    exit 1
  fi
}

upgrade_control_plane_first() {
  local old_images
  old_images=$(node_running_images)
  # OnDelete lets helm rewrite the DaemonSet template without rolling pods;
  # the old ovnkube-node pods keep running against the new cluster-manager.
  set_node_daemonset_strategy OnDelete
  # Refresh only the pods that are supposed to change in this stage. Deleting
  # ovnkube-node pods here would recreate them from the new template.
  export OVN_DEPLOY_PODS="ovnkube-identity ovnkube-control-plane"
  helm_deploy
  if control_plane_exists; then
    kubectl -n ovn-kubernetes rollout status deployment "$CONTROL_PLANE_DEPLOYMENT" --timeout=300s
    running=$(control_plane_running_image)
    if [[ "$running" != "$OVN_IMAGE" ]]; then
      echo "control-plane pods run ${running}, expected ${OVN_IMAGE}" >&2
      exit 1
    fi
  fi
  # Prove the skew we are about to test: cluster-manager new, nodes old.
  new_images=$(node_running_images)
  if [[ "$new_images" != "$old_images" ]]; then
    echo "node pods changed image during control-plane-only upgrade:" >&2
    echo "before: ${old_images}" >&2
    echo "after:  ${new_images}" >&2
    exit 1
  fi
  echo "nodes still running ${old_images}; control-plane upgraded to ${OVN_IMAGE}"
  remove_control_plane_taint
  refresh_e2e_binary
}

upgrade_nodes_last() {
  # Switching back to RollingUpdate makes the DaemonSet controller roll the
  # pods to the template helm already installed in the previous stage.
  set_node_daemonset_strategy RollingUpdate
  wait_daemonsets
  running=$(node_running_images)
  if [[ "$running" != "$OVN_IMAGE" ]]; then
    echo "node pods run ${running}, expected ${OVN_IMAGE}" >&2
    exit 1
  fi
}

case "$UPGRADE_STAGE" in
  all)                 upgrade_all ;;
  nodes-first)         upgrade_nodes_first ;;
  control-plane-last)  upgrade_control_plane_last ;;
  control-plane-first) upgrade_control_plane_first ;;
  nodes-last)          upgrade_nodes_last ;;
  *)
    echo "unknown UPGRADE_STAGE '${UPGRADE_STAGE}', expected all, nodes-first, control-plane-last, control-plane-first or nodes-last" >&2
    exit 2
    ;;
esac
