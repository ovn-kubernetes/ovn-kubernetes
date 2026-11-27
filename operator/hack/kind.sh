#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Bring up a kind cluster with OVN-Kubernetes installed by the operator.
#
# The cluster comes from contrib/kind-common.sh, so it matches the upstream
# kind workflow (no default CNI, no kube-proxy). On top of it this script:
#   1. builds the operator image and installs the operator,
#   2. installs the upstream OVS DaemonSet (the Helm chart's ovs-node
#      subchart), since OVS is a prerequisite the operator does not manage,
#   3. creates the OVNK8sOperator CR for this cluster, and
#   4. waits for OVN-Kubernetes, kube-system and the CR to become ready.
#
# Usage: hack/kind.sh [--test-uninstall]
#   --test-uninstall  after the install, delete the CR and check that the
#                     operator removed its resources while the namespaces and
#                     OVS remain.
#
# Environment (defaults in brackets):
#   OVN_IMAGE        [ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu:master]
#   OPERATOR_IMAGE   [localhost/ovnk8s-operator:dev]
#   KIND_CLUSTER_NAME, K8S_VERSION, KIND_NUM_WORKER, OVN_GATEWAY_MODE,
#   PLATFORM_IPV4_SUPPORT, PLATFORM_IPV6_SUPPORT: as for contrib/kind-helm.sh
#
# Requires the same tools as contrib/kind-helm.sh.
# Delete the cluster with: kind delete cluster --name "${KIND_CLUSTER_NAME:-ovn}"

# Not -u: contrib/kind-common.sh reads optional variables that may be unset.
set -eo pipefail

TEST_UNINSTALL=false
case "$1" in
  "") ;;
  --test-uninstall) TEST_UNINSTALL=true ;;
  *)
    echo "usage: $0 [--test-uninstall]" >&2
    exit 2
    ;;
esac

OPERATOR_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIR="$(cd "$OPERATOR_DIR/../contrib" && pwd)"
export DIR
# shellcheck source=../../contrib/kind-common.sh
source "$DIR/kind-common.sh"
# shellcheck source=../../contrib/kind-dpu-sim-lib.sh
source "$DIR/kind-dpu-sim-lib.sh"

OVN_IMAGE=${OVN_IMAGE:-ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu:master}
OPERATOR_IMAGE=${OPERATOR_IMAGE:-localhost/ovnk8s-operator:dev}

check_common_dependencies
if ! command_exists helm; then
  echo "ERROR: required command not found: helm" >&2
  exit 1
fi

set_common_default_params
check_ipv6
set_cluster_cidr_ip_families

create_kind_cluster
docker_disable_ipv6
coredns_patch
detect_apiserver_url
if [ "$KIND_REMOVE_TAINT" == true ]; then
  remove_no_schedule_taint
fi

echo "==> Loading images"
docker pull "$OVN_IMAGE"
install_image "$OVN_IMAGE"
make -C "$OPERATOR_DIR" docker-build IMG="$OPERATOR_IMAGE" \
  OVNKUBE_IMAGE_REPO="${OVN_IMAGE%:*}" OVNKUBE_IMAGE_TAG="${OVN_IMAGE##*:}"
install_image "$OPERATOR_IMAGE"

echo "==> Installing the operator"
kubectl kustomize "$OPERATOR_DIR/config/default" |
  sed "s|image: controller:latest|image: ${OPERATOR_IMAGE}|" |
  kubectl apply --server-side -f -

echo "==> Installing OVS"
helm template ovn-kubernetes "$OPERATOR_DIR/../helm/ovn-kubernetes" \
  -f "$OPERATOR_DIR/../helm/ovn-kubernetes/values-single-node-zone.yaml" \
  --show-only charts/ovs-node/templates/ovs-node.yaml \
  --set global.image.repository="${OVN_IMAGE%:*}" \
  --set global.image.tag="${OVN_IMAGE##*:}" |
  kubectl apply --server-side -f -

echo "==> Creating the OVNK8sOperator CR"
pod_network=""
for cidr in ${NET_CIDR//,/ }; do
  host_prefix=24
  if [[ "$cidr" == *:* ]]; then
    host_prefix=64
  fi
  pod_network+="      - cidr: ${cidr}"$'\n'"        hostPrefix: ${host_prefix}"$'\n'
done
service_network=""
for cidr in ${SVC_CIDR//,/ }; do
  service_network+="      - ${cidr}"$'\n'
done
kubectl apply -f - <<EOF
apiVersion: k8s.ovn.org/v1alpha1
kind: OVNK8sOperator
metadata:
  name: default
  namespace: ovnk8s-operator-system
spec:
  global:
    k8sAPIServer: ${API_URL}
    podNetwork:
${pod_network}    serviceNetwork:
${service_network}    mtu: ${OVN_MTU}
    gatewayMode: ${OVN_GATEWAY_MODE}
    deploymentMode: standard
    namespace: ovn-kubernetes
  featureGates:
    egress:
      ip: {}
      firewall: {}
      qos: {}
      service: {}
    multiExternalGateway: {}
    serviceTemplateSupport: {}
    lflowCache: {}
    persistentIPs: {}
  componentOverrides:
    - resourceName: ovnkubeIdentity
      enabled: true
EOF

echo "==> Waiting for OVN-Kubernetes"
# The operator creates components in dependency order, so wait for it to
# finish before checking pods that may not exist yet.
kubectl -n ovnk8s-operator-system wait ovnk8soperator/default \
  --for=condition=UpgradeComplete --timeout=600s
kubectl_wait_pods
echo "==> Cluster ${KIND_CLUSTER_NAME} is ready (KUBECONFIG=${KUBECONFIG})"

if [ "$TEST_UNINSTALL" == true ]; then
  echo "==> Testing uninstall"
  kubectl -n ovnk8s-operator-system delete ovnk8soperator default --timeout=300s
  leftover=$(kubectl get daemonsets,deployments,configmaps,secrets,serviceaccounts,roles,rolebindings,clusterroles,clusterrolebindings,validatingwebhookconfigurations,validatingadmissionpolicies,validatingadmissionpolicybindings \
    -A -l app.kubernetes.io/managed-by=ovnk8s-operator -o name)
  if [ -n "$leftover" ]; then
    echo "ERROR: operator-managed resources left after uninstall:" >&2
    echo "$leftover" >&2
    exit 1
  fi
  kubectl get namespace ovn-kubernetes ovn-host-network >/dev/null
  kubectl -n ovn-kubernetes get daemonset ovs-node >/dev/null
  echo "==> Uninstall removed the operator's resources and kept the namespaces and OVS"
fi
