#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
OVN_KUBERNETES_PATH=${OVN_KUBERNETES_PATH:-$(cd "${SCRIPT_DIR}/.." && pwd)}

resolve_dpu_sim_path() {
  if [ -n "${DPU_SIM_PATH:-}" ]; then
    if [ ! -d "${DPU_SIM_PATH}" ]; then
      echo "error: DPU_SIM_PATH does not exist: ${DPU_SIM_PATH}" >&2
      exit 1
    fi
    cd "${DPU_SIM_PATH}" && pwd
    return
  fi

  local candidates=(
    "${OVN_KUBERNETES_PATH}/../dpu-simulator"
    "${OVN_KUBERNETES_PATH}/../../ovn-kubernetes/dpu-simulator"
  )

  local gopath=${GOPATH:-}
  if [ -z "${gopath}" ] && command -v go >/dev/null 2>&1; then
    gopath=$(go env GOPATH 2>/dev/null || true)
  fi
  if [ -n "${gopath}" ]; then
    local entry
    local -a gopath_entries
    IFS=: read -ra gopath_entries <<< "${gopath}"
    for entry in "${gopath_entries[@]}"; do
      candidates+=("${entry}/src/github.com/ovn-kubernetes/dpu-simulator")
    done
  fi

  local path
  for path in "${candidates[@]}"; do
    if [ -d "${path}" ]; then
      cd "${path}" && pwd
      return
    fi
  done

  echo "error: could not locate dpu-simulator checkout" >&2
  echo "set DPU_SIM_PATH to the dpu-simulator repository path" >&2
  exit 1
}

resolve_kind_provider() {
  if [ -n "${KIND_EXPERIMENTAL_PROVIDER:-}" ]; then
    echo "${KIND_EXPERIMENTAL_PROVIDER}"
    return
  fi

  if command -v docker >/dev/null 2>&1; then
    echo "docker"
    return
  fi

  if command -v podman >/dev/null 2>&1; then
    echo "podman"
    return
  fi

  echo "error: could not locate podman or docker" >&2
  echo "set KIND_EXPERIMENTAL_PROVIDER to the Kind provider to use" >&2
  exit 1
}

detect_kind_external_host_ipv4() {
  ip -j route get 1 2>/dev/null | jq -r '.[0].prefsrc // empty' 2>/dev/null || true
}

add_ipv4_external_prefix() {
  local prefix=$1

  case " ${BGP_ADDITIONAL_EXTERNAL_PREFIXES_IPV4:-} " in
    *" ${prefix} "*) return ;;
  esac

  if [ -n "${BGP_ADDITIONAL_EXTERNAL_PREFIXES_IPV4:-}" ]; then
    BGP_ADDITIONAL_EXTERNAL_PREFIXES_IPV4="${BGP_ADDITIONAL_EXTERNAL_PREFIXES_IPV4} ${prefix}"
  else
    BGP_ADDITIONAL_EXTERNAL_PREFIXES_IPV4="${prefix}"
  fi
}

cleanup_bgp_artifacts() {
  local provider=$1

  if ! command -v "${provider}" >/dev/null 2>&1; then
    return
  fi

  if "${provider}" ps -a --format '{{.Names}}' | grep -Eq '^bgpserver$'; then
    "${provider}" rm -f bgpserver
  fi
  if "${provider}" ps -a --format '{{.Names}}' | grep -Eq '^frr$'; then
    "${provider}" rm -f frr
  fi
  if "${provider}" network ls --format '{{.Name}}' | grep -Eq '^bgpnet$'; then
    "${provider}" network rm bgpnet || true
  fi
}

DPU_SIM_PATH=$(resolve_dpu_sim_path)
KIND_EXPERIMENTAL_PROVIDER=$(resolve_kind_provider)
export KIND_EXPERIMENTAL_PROVIDER
KIND_HELM_OVN_TIMEOUT=${KIND_HELM_OVN_TIMEOUT:-900}
export KIND_HELM_OVN_TIMEOUT
BGP_SERVER_NET_SUBNET_IPV4=${BGP_SERVER_NET_SUBNET_IPV4:-172.27.0.0/16}
BGP_SERVER_NET_SUBNET_IPV6=${BGP_SERVER_NET_SUBNET_IPV6:-fc00:f853:ccd:e797::/64}
export BGP_SERVER_NET_SUBNET_IPV4
export BGP_SERVER_NET_SUBNET_IPV6
# Advertise the host address Kind uses for external traffic so host-published
# external servers are reachable through the no-overlay FRR path.
KIND_EXTERNAL_HOST_IPV4=${KIND_EXTERNAL_HOST_IPV4:-$(detect_kind_external_host_ipv4)}
if [ -n "${KIND_EXTERNAL_HOST_IPV4}" ]; then
  add_ipv4_external_prefix "${KIND_EXTERNAL_HOST_IPV4}/32"
  export BGP_ADDITIONAL_EXTERNAL_PREFIXES_IPV4
else
  echo "warning: could not determine Kind external host IPv4" >&2
fi
DPU_SIM_CONFIG=${DPU_SIM_CONFIG:-config-kind-ovnk-offload.yaml}
HOST_CLUSTER=${HOST_CLUSTER:-dpu-sim-host}
DPU_CLUSTER=${DPU_CLUSTER:-dpu-sim-dpu}
HOST_KUBECONFIG="${DPU_SIM_PATH}/kubeconfig/${HOST_CLUSTER}.kubeconfig"
DPU_KUBECONFIG="${DPU_SIM_PATH}/kubeconfig/${DPU_CLUSTER}.kubeconfig"
HOST_VALUES="${DPU_SIM_PATH}/kubeconfig/helm-values/${HOST_CLUSTER}-ovn-kubernetes-dpu-host-values.yaml"
DPU_VALUES="${DPU_SIM_PATH}/kubeconfig/helm-values/${DPU_CLUSTER}-ovn-kubernetes-dpu-values.yaml"
FRR_ENV="${DPU_SIM_PATH}/kubeconfig/helm-values/${DPU_CLUSTER}-frr-k8s.env"

kubectl_host() {
  kubectl --kubeconfig "${HOST_KUBECONFIG}" "$@"
}

kubectl_dpu() {
  kubectl --kubeconfig "${DPU_KUBECONFIG}" "$@"
}

cluster_command_arg() {
  local kubeconfig=$1
  local selector=$2
  local arg_name=$3
  local command

  command=$(kubectl --kubeconfig "${kubeconfig}" -n kube-system get pod \
    -l "${selector}" -o jsonpath='{.items[0].spec.containers[0].command}')
  printf '%s\n' "${command}" | tr '",' '\n' | awk -v prefix="--${arg_name}=" '
    index($0, prefix) == 1 {
      print substr($0, length(prefix) + 1)
      exit
    }
  '
}

host_cluster_cidrs() {
  local net_cidr svc_cidr

  net_cidr=$(cluster_command_arg "${HOST_KUBECONFIG}" "component=kube-controller-manager" "cluster-cidr")
  svc_cidr=$(cluster_command_arg "${HOST_KUBECONFIG}" "component=kube-apiserver" "service-cluster-ip-range")

  if [ -z "${net_cidr}" ] || [ -z "${svc_cidr}" ]; then
    echo "error: could not determine host cluster pod/service CIDRs" >&2
    exit 1
  fi

  echo "${net_cidr} ${svc_cidr}"
}

install_ovnk_host() {
  local cidrs host_net_cidr host_svc_cidr

  cidrs=$(host_cluster_cidrs)
  read -r host_net_cidr host_svc_cidr <<< "${cidrs}"
  echo "Using host cluster pod CIDR ${host_net_cidr}"
  echo "Using host cluster service CIDR ${host_svc_cidr}"

  pushd "${OVN_KUBERNETES_PATH}"
  NET_CIDR_IPV4="${host_net_cidr}" \
  SVC_CIDR_IPV4="${host_svc_cidr}" \
    ./contrib/kind-helm.sh \
    --deploy \
    --cluster-name "${HOST_CLUSTER}" \
    --kubeconfig "${HOST_KUBECONFIG}" \
    --dpu-mode host \
    --network-segmentation-enable \
    --multi-network-enable \
    --route-advertisements-enable \
    --no-overlay-enable \
    --advertise-default-network \
    --extra-values "${HOST_VALUES}"
  popd
}

install_ovnk_dpu() {
  # shellcheck disable=SC1090
  source "${FRR_ENV}"
  export DPU_SIM_GATEWAY_NETWORK
  export DPU_SIM_GATEWAY_SUBNET

  pushd "${OVN_KUBERNETES_PATH}"
  ./contrib/kind-helm.sh \
    --deploy \
    --cluster-name "${DPU_CLUSTER}" \
    --kubeconfig "${DPU_KUBECONFIG}" \
    --dpu-mode dpu \
    --multi-network-enable \
    --network-segmentation-enable \
    --route-advertisements-enable \
    --no-overlay-enable \
    --advertise-default-network \
    --extra-values "${DPU_VALUES}" \
    --frr-k8s-host-kubeconfig "${FRR_K8S_HOST_KUBECONFIG}" \
    --frr-k8s-remote-kubeconfig "${FRR_K8S_REMOTE_KUBECONFIG}" \
    --frr-k8s-remote-node-map "${FRR_K8S_REMOTE_NODE_MAP}"
  popd
}

wait_for_ovn() {
  kubectl_host wait --for=condition=Ready nodes --all --timeout=25m
  kubectl_dpu wait --for=condition=Ready nodes --all --timeout=25m
  kubectl_host -n ovn-kubernetes wait --for=condition=Ready pods --all --timeout=10m
  kubectl_dpu -n ovn-kubernetes wait --for=condition=Ready pods --all --timeout=10m
  kubectl_dpu -n frr-k8s-system wait --for=condition=Ready pods --all --timeout=10m
}

if [ ! -x "${DPU_SIM_PATH}/bin/dpu-sim" ]; then
  echo "error: ${DPU_SIM_PATH}/bin/dpu-sim does not exist or is not executable" >&2
  echo "run 'make build' in the dpu-simulator repository first" >&2
  exit 1
fi

echo "Using KIND_EXPERIMENTAL_PROVIDER=${KIND_EXPERIMENTAL_PROVIDER}"
echo "Using KIND_HELM_OVN_TIMEOUT=${KIND_HELM_OVN_TIMEOUT}"
echo "Using BGP_SERVER_NET_SUBNET_IPV4=${BGP_SERVER_NET_SUBNET_IPV4}"
echo "Using KIND_EXTERNAL_HOST_IPV4=${KIND_EXTERNAL_HOST_IPV4:-}"
echo "Using BGP_ADDITIONAL_EXTERNAL_PREFIXES_IPV4=${BGP_ADDITIONAL_EXTERNAL_PREFIXES_IPV4:-}"

cleanup_bgp_artifacts "${KIND_EXPERIMENTAL_PROVIDER}"

pushd "${DPU_SIM_PATH}"
./bin/dpu-sim \
  --config "${DPU_SIM_CONFIG}" \
  --ovn-kubernetes-path "${OVN_KUBERNETES_PATH}" \
  --ovnk-mode values-only

install_ovnk_host

./bin/dpu-sim ovnk host-access \
  --config "${DPU_SIM_CONFIG}" \
  --cluster "${HOST_CLUSTER}"
./bin/dpu-sim ovnk values \
  --config "${DPU_SIM_CONFIG}" \
  --cluster "${DPU_CLUSTER}" \
  --require-host-credentials

install_ovnk_dpu
wait_for_ovn
popd
