#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0


# Helper usage
function usage() {
  echo "Usage: $0 [OPTIONS]"
  echo ""
  echo "This script installs OVN-Kubernetes via Helm onto an existing Kubernetes"
  echo "cluster. The cluster must have no CNI installed and kube-proxy disabled —"
  echo "OVN-Kubernetes provides both. KUBECONFIG (or ~/.kube/config) must point at"
  echo "the target cluster."
  echo ""
  echo "Options:"
  echo "  BUILD_IMAGE=${BUILD_IMAGE:-false}      Set to true to build the OVN-Kubernetes image locally instead of pulling from ghcr.io."
  echo "  OVN_INTERCONNECT=${OVN_INTERCONNECT:-true}  Set to false to use the legacy non-interconnect (central) deployment (deprecated)."
  echo ""
  echo "Example: BUILD_IMAGE=true OVN_INTERCONNECT=false $0"
  echo ""
  echo "To create a Kind cluster *and* install OVN-Kubernetes in one step, see"
  echo "contrib/kind-helm.sh."
  exit 1
}

# Default values for flags
BUILD_IMAGE=${BUILD_IMAGE:-false}
OVN_INTERCONNECT=${OVN_INTERCONNECT:-true}

# Show usage and exit before running any preflight checks.
if [[ "$1" == "--help" || "$1" == "-h" ]]; then
  usage
fi

# Determine the values file based on OVN_INTERCONNECT
if [[ "$OVN_INTERCONNECT" == "true" ]]; then
  VALUES_FILE="values-single-node-zone.yaml"
else
  VALUES_FILE="values-no-ic.yaml"
fi

# Verify dependencies
check_command() {
  command -v "$1" >/dev/null 2>&1 || { echo "$1 not found, please install it."; exit 1; }
}
check_command kubectl
check_command helm
if [[ "$BUILD_IMAGE" == "true" ]]; then
  check_command docker
  check_command go
fi

# Verify the cluster is reachable before doing anything
if ! kubectl cluster-info >/dev/null 2>&1; then
  echo "Cannot reach a Kubernetes cluster — set KUBECONFIG to point at an existing cluster."
  exit 1
fi

export DIR="$( cd -- "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
cd $DIR

IMG_PREFIX="${IMG_PREFIX:-ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu}"
TAG="${TAG:-master}"
IMG="${IMG_PREFIX}:${TAG}"

if [[ "$BUILD_IMAGE" == "true" && "$IMG_PREFIX" == "ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu" && "$TAG" == "master" ]]; then
  echo "BUILD_IMAGE=true requires IMG_PREFIX/TAG to point at a registry you can push to."
  echo "Set them, push the built image after the build step, then re-run this script."
  exit 1
fi

set -euxo pipefail

if [[ "$BUILD_IMAGE" == "true" ]]; then
  echo "Building OVN-Kubernetes image..."
  pushd ../dist/images
  make ubuntu-image
  popd
  docker tag ovn-kube-ubuntu:latest "$IMG"
fi

# Node labeling for interconnect mode
if [[ "$OVN_INTERCONNECT" == "true" ]]; then
  for n in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
    kubectl label node "${n}" k8s.ovn.org/zone-name=${n} --overwrite
  done
fi

# Deploy OVN-Kubernetes using Helm
cd ${DIR}/ovn-kubernetes
helm install ovn-kubernetes . -f ${VALUES_FILE} \
    --set k8sAPIServer="https://$(kubectl get pods -n kube-system -l component=kube-apiserver -o jsonpath='{.items[0].status.hostIP}'):6443" \
    --set global.image.repository=${IMG_PREFIX} \
    --set global.image.tag=${TAG}
