#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# Staged OVN-Kubernetes upgrade for the kind cluster created by
# contrib/kind-helm.sh, modelled on how production GitOps upgrades work: a new
# chart version and image tag are handed to helm, and every component rolls
# according to its own update strategy. No pods are scaled down or deleted by
# hand, and nothing is re-provisioned. The only kind-specific step is loading
# the image into the nodes, which stands in for pulling it from a registry.
#
# Two things are needed to test a half-upgraded cluster:
#  - hold back the control plane: `kubectl rollout pause` on the Deployment.
#    helm updates the pod template, the Deployment controller does not act on
#    it until `rollout resume`.
#  - hold back the nodes: switch the DaemonSet to the OnDelete update strategy.
#    helm updates the template, pods keep running until deleted; switching back
#    to RollingUpdate rolls them. helm's three-way merge leaves the live
#    strategy alone because the chart did not change that field.
#
# UPGRADE_STAGE:
#   all                 - single helm upgrade, everything rolls
#   nodes-first         - nodes roll, control plane paused on the old image
#   control-plane-last  - resume the control plane
#   control-plane-first - control plane rolls, nodes held on the old image
#   nodes-last          - roll the nodes
#
# Run e2e between the two stages of a pair to exercise the version skew, and
# again after the second stage.
#
# Env:
#   OVN_IMAGE           image to upgrade to (default ovn-daemonset-fedora:pr)
#   KIND_CLUSTER_NAME   kind cluster to load the image into (default ovn)
#   HELM_RELEASE        helm release name used at install (default ovn-kubernetes)
#   PIN_OVS_NODE        keep ovs-node on OnDelete so OVS does not restart under a
#                       running ovnkube-node (default true, see below)

set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-${HOME}/ovn.conf}
OVN_IMAGE_FAMILY=${OVN_IMAGE_FAMILY:-fedora}
OVN_IMAGE=${OVN_IMAGE:-ovn-daemonset-${OVN_IMAGE_FAMILY}:pr}
KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn}
HELM_RELEASE=${HELM_RELEASE:-ovn-kubernetes}
PIN_OVS_NODE=${PIN_OVS_NODE:-true}

NAMESPACE=ovn-kubernetes
CONTROL_PLANE_DEPLOYMENT=ovnkube-control-plane
NODE_DAEMONSETS="ovnkube-node ovnkube-single-node-zone"

SCRIPT_DIR="$(cd -- "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
CHART_DIR="${SCRIPT_DIR}/../../helm/ovn-kubernetes"

log() { echo ">>> $*"; }

exists() { kubectl -n "$NAMESPACE" get "$1" "$2" >/dev/null 2>&1; }

# Selector of a workload as a kubectl -l argument.
selector_of() {
  kubectl -n "$NAMESPACE" get "$1" "$2" -o json \
    | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")'
}

# Unique set of images used by the running pods of a workload.
running_images() {
  kubectl -n "$NAMESPACE" get pod -l "$(selector_of "$1" "$2")" \
    -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.image}{"\n"}{end}{end}' | sort -u
}

node_daemonsets() {
  for ds in $NODE_DAEMONSETS; do
    exists daemonset "$ds" && echo "$ds"
  done
}

node_running_images() {
  for ds in $(node_daemonsets); do running_images daemonset "$ds"; done | sort -u
}

set_node_strategy() {
  for ds in $(node_daemonsets); do
    kubectl -n "$NAMESPACE" patch daemonset "$ds" --type merge \
      -p "{\"spec\":{\"updateStrategy\":{\"type\":\"$1\"}}}"
  done
}

wait_nodes() {
  for ds in $(node_daemonsets); do
    kubectl -n "$NAMESPACE" rollout status daemonset "$ds" --timeout=600s
  done
}

wait_control_plane() {
  exists deployment "$CONTROL_PLANE_DEPLOYMENT" || return 0
  kubectl -n "$NAMESPACE" rollout status deployment "$CONTROL_PLANE_DEPLOYMENT" --timeout=300s
}

expect_images() {
  # expect_images <what> <actual> <expected>
  if [[ "$2" != "$3" ]]; then
    echo "$1 run image(s) [$2], expected [$3]" >&2
    exit 1
  fi
}

# The upgrade itself: make the image available to the nodes and hand the new
# chart and tag to helm. --reuse-values keeps whatever values the release was
# installed with, the same way GitOps keeps pointing at the same values files
# while bumping the chart version.
helm_upgrade() {
  log "loading ${OVN_IMAGE} into kind cluster ${KIND_CLUSTER_NAME}"
  kind load docker-image "$OVN_IMAGE" --name "$KIND_CLUSTER_NAME"

  local extra=()
  if [[ "$PIN_OVS_NODE" == true ]]; then
    # The chart has one image tag for every DaemonSet, so ovs-node would roll
    # together with ovnkube-node. Restarting OVS under a running ovnkube-node
    # removes /var/run/openvswitch/db.sock and the breth0 port state; the old
    # ovnkube-node crashes and the new one cannot become Ready against the
    # torn-down OVS, so the rollout stalls. OVS is upgraded separately.
    extra+=(--set ovs-node.updateStrategy=OnDelete)
  fi

  log "helm upgrade ${HELM_RELEASE} to ${OVN_IMAGE}"
  helm upgrade "$HELM_RELEASE" "$CHART_DIR" --reuse-values \
    --set "global.image.repository=${OVN_IMAGE%:*}" \
    --set "global.image.tag=${OVN_IMAGE##*:}" \
    ${extra[@]+"${extra[@]}"}
}

stage_all() {
  helm_upgrade
  wait_control_plane
  wait_nodes
  expect_images "node pods" "$(node_running_images)" "$OVN_IMAGE"
  if exists deployment "$CONTROL_PLANE_DEPLOYMENT"; then
    expect_images "control-plane pods" "$(running_images deployment "$CONTROL_PLANE_DEPLOYMENT")" "$OVN_IMAGE"
  fi
}

stage_nodes_first() {
  local old=""
  if exists deployment "$CONTROL_PLANE_DEPLOYMENT"; then
    old=$(running_images deployment "$CONTROL_PLANE_DEPLOYMENT")
    kubectl -n "$NAMESPACE" rollout pause deployment "$CONTROL_PLANE_DEPLOYMENT"
  fi
  helm_upgrade
  wait_nodes
  expect_images "node pods" "$(node_running_images)" "$OVN_IMAGE"
  if [[ -n "$old" ]]; then
    expect_images "control-plane pods" "$(running_images deployment "$CONTROL_PLANE_DEPLOYMENT")" "$old"
    log "nodes on ${OVN_IMAGE}, control plane still on ${old}"
  fi
}

stage_control_plane_last() {
  exists deployment "$CONTROL_PLANE_DEPLOYMENT" || { log "no ${CONTROL_PLANE_DEPLOYMENT}, nothing to do"; return; }
  kubectl -n "$NAMESPACE" rollout resume deployment "$CONTROL_PLANE_DEPLOYMENT"
  wait_control_plane
  expect_images "control-plane pods" "$(running_images deployment "$CONTROL_PLANE_DEPLOYMENT")" "$OVN_IMAGE"
}

stage_control_plane_first() {
  local old
  old=$(node_running_images)
  set_node_strategy OnDelete
  helm_upgrade
  wait_control_plane
  if exists deployment "$CONTROL_PLANE_DEPLOYMENT"; then
    expect_images "control-plane pods" "$(running_images deployment "$CONTROL_PLANE_DEPLOYMENT")" "$OVN_IMAGE"
  fi
  expect_images "node pods" "$(node_running_images)" "$old"
  log "control plane on ${OVN_IMAGE}, nodes still on ${old}"
}

stage_nodes_last() {
  set_node_strategy RollingUpdate
  wait_nodes
  expect_images "node pods" "$(node_running_images)" "$OVN_IMAGE"
}

UPGRADE_STAGE=${1:-${UPGRADE_STAGE:-}}
case "$UPGRADE_STAGE" in
  all)                 stage_all ;;
  nodes-first)         stage_nodes_first ;;
  control-plane-last)  stage_control_plane_last ;;
  control-plane-first) stage_control_plane_first ;;
  nodes-last)          stage_nodes_last ;;
  "")
    echo "usage: $0 <all|nodes-first|control-plane-last|control-plane-first|nodes-last>" >&2
    exit 2 ;;
  *)
    echo "unknown UPGRADE_STAGE '${UPGRADE_STAGE}': all, nodes-first, control-plane-last, control-plane-first, nodes-last" >&2
    exit 2 ;;
esac
