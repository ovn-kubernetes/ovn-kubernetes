#!/usr/bin/env bash

# Helper usage
function usage() {
  echo "Usage: $0 [OPTIONS]"
  echo ""
  echo "This script deploys a kind cluster and configures it to use OVN-Kubernetes CNI."
  echo ""
  echo "Options:"
  echo "  BUILD_IMAGE=${BUILD_IMAGE:-false}      Set to true to build the Docker image instead of pulling it."
  echo "  OVN_INTERCONNECT=${OVN_INTERCONNECT:-true}  Set to false to use a non-interconnect deployment (values-no-ic.yaml)."
  echo ""
  echo "Example: BUILD_IMAGE=true OVN_INTERCONNECT=false $0"
  exit 1
}

# Default values for flags
BUILD_IMAGE=${BUILD_IMAGE:-false}
OVN_INTERCONNECT=${OVN_INTERCONNECT:-true}

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
check_command docker
check_command kubectl
check_command kind

export DIR="$( cd -- "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
cd $DIR

if [[ "$1" == "--help" || "$1" == "-h" ]]; then
  usage
fi

IMG_PREFIX='ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu'
TAG='master'
IMG="${IMG_PREFIX}:${TAG}"

if [[ "$BUILD_IMAGE" == "true" ]]; then
  check_command go # Only check for Go when building the image

  # Build image
  echo "Building Docker image..."
  pushd ../dist/images
  make ubuntu-image
  popd
  docker tag ovn-kube-ubuntu:latest $IMG
else
  # Pull image from GitHub
  echo "Pulling Docker image..."
  docker pull $IMG
fi

# Configure system parameters
set -euxo pipefail
sudo sysctl fs.inotify.max_user_watches=524288
sudo sysctl fs.inotify.max_user_instances=512

# Create a kind cluster
kind_cluster_name=ovn-helm
kind delete clusters $kind_cluster_name || true
cat <<EOF | kind create cluster --name $kind_cluster_name --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
- role: worker
networking:
  disableDefaultCNI: true
  kubeProxyMode: none
EOF

kind load docker-image --name $kind_cluster_name $IMG

# Node labeling based on OVN_INTERCONNECT
if [[ "$OVN_INTERCONNECT" == "true" ]]; then
  for n in $(kind get nodes --name "${kind_cluster_name}"); do
    kubectl label node "${n}" k8s.ovn.org/zone-name=${n} --overwrite
  done
fi

# Deploy OVN-Kubernetes using Helm
cd ${DIR}/ovn-kubernetes
helm install ovn-kubernetes . -f ${VALUES_FILE} \
    --set k8sAPIServer="https://$(kubectl get pods -n kube-system -l component=kube-apiserver -o jsonpath='{.items[0].status.hostIP}'):6443" \
    --set global.image.repository=${IMG_PREFIX} \
    --set global.image.tag=${TAG}

