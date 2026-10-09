#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# Run the bgp-ra-density lane against a local kind cluster, the way the CI lane
# runs it. Mirrors .github/workflows/performance-test.yml for the matrix entry
# {"target": "bgp", "perf-test": "bgp-ra-density", ...}.
#
# Stages are independent so the metric-name loop stays fast: once the cluster is
# up, re-running the workload is seconds, not twenty minutes.
#
#   ./run-local-lane.sh install   install the pinned kube-burner
#   ./run-local-lane.sh build     rebuild the image and reload it into kind
#   ./run-local-lane.sh cluster   create the kind cluster (slow, once)
#   ./run-local-lane.sh prom      install Prometheus and start the port-forward
#   ./run-local-lane.sh workload  run kube-burner
#   ./run-local-lane.sh check     report which metric queries returned data
#   ./run-local-lane.sh all       install + cluster + prom + workload + check

set -eu

REPO=$(cd "$(dirname "$0")/../.." && pwd)
WORKLOAD=${WORKLOAD:-bgp-ra-density}
# Pinned to match both workflows. The release asset spells the version with a
# capital V while the tag does not.
KUBE_BURNER_VERSION=${KUBE_BURNER_VERSION:-v2.5.0}
KUBE_BURNER_SHA256=${KUBE_BURNER_SHA256:-47c03fab8d88db8ffb9deb796b45d6777bca5648d5a9f81e3c3592ce78678d28}
# ~/.local/bin rather than /usr/local/bin so this needs no sudo.
BIN_DIR=${BIN_DIR:-${HOME}/.local/bin}
# Two fewer workers than CI: CI adds KIND_NUM_INFRA=2 on top, so both end at a
# six-node total only if you also pass -inf 2. Node count changes the generated
# object count, so keep it in mind when comparing numbers against a CI run.
NUM_WORKERS=${NUM_WORKERS:-3}

stage_install() {
  if command -v kube-burner >/dev/null && kube-burner version 2>&1 | grep -q "${KUBE_BURNER_VERSION#v}"; then
    echo "== kube-burner ${KUBE_BURNER_VERSION} already installed =="
    return
  fi
  echo "== installing kube-burner ${KUBE_BURNER_VERSION} =="
  local url tmp
  url="https://github.com/kube-burner/kube-burner/releases/download/${KUBE_BURNER_VERSION}/kube-burner-V${KUBE_BURNER_VERSION#v}-linux-x86_64.tar.gz"
  tmp=$(mktemp -d)
  trap 'rm -rf "${tmp}"' RETURN
  curl -fL -o "${tmp}/kube-burner.tar.gz" "${url}"
  echo "${KUBE_BURNER_SHA256}  ${tmp}/kube-burner.tar.gz" | sha256sum -c - || {
    echo "checksum mismatch for ${url}" >&2; return 1; }
  tar xzf "${tmp}/kube-burner.tar.gz" -C "${tmp}" kube-burner
  mkdir -p "${BIN_DIR}"
  install -m 0755 "${tmp}/kube-burner" "${BIN_DIR}/kube-burner"
  echo "  installed to ${BIN_DIR}/kube-burner"
  case ":${PATH}:" in
    *":${BIN_DIR}:"*) ;;
    *) echo "  note: ${BIN_DIR} is not on PATH" >&2 ;;
  esac
}

stage_build() {
  echo "== building ovnkube image =="
  make -C "${REPO}/go-controller" build
  make -C "${REPO}/dist/images" fedora-image
  docker tag ovn-daemonset-fedora:latest localhost/ovn-daemonset-fedora:dev
  kind load docker-image localhost/ovn-daemonset-fedora:dev --name ovn
  kubectl -n ovn-kubernetes rollout restart ds/ovnkube-node deploy/ovnkube-control-plane
  kubectl -n ovn-kubernetes rollout status deploy/ovnkube-control-plane --timeout=300s
}

stage_cluster() {
  echo "== creating kind cluster =="
  # -rae is what installs frr-k8s, the external frr container and the
  # receive-all FRRConfiguration the workload's RouteAdvertisements select.
  # Without it every RA is Accepted but generates nothing.
  # -sm sets --metrics-enable-scale, without which the workqueue, ovsdb,
  # RouteAdvertisements and route-import families are never registered.
  # -mip 0.0.0.0 is required: Prometheus scrapes the pod IP, and the default
  # loopback bind makes every ovnkube_*, ovn_* and ovs_* query return nothing.
  cd "${REPO}/contrib"
  ./kind.sh -rae -mne -nse -sm -gm local -wk "${NUM_WORKERS}" -mip 0.0.0.0
}

stage_prom() {
  echo "== installing Prometheus =="
  cd "${REPO}"
  ./contrib/install-prometheus-infra.sh
  pkill -f "port-forward.*9090:9090" 2>/dev/null || true
  kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090 >/dev/null 2>&1 &
  for _ in $(seq 30); do
    curl -sf http://localhost:9090/-/ready >/dev/null 2>&1 && break
    sleep 2
  done
  curl -sf http://localhost:9090/-/ready >/dev/null && echo "  prometheus ready" || {
    echo "  prometheus not reachable on :9090" >&2; return 1; }
}

stage_workload() {
  command -v kube-burner >/dev/null || {
    echo "kube-burner not installed; run: $0 install" >&2
    return 1; }
  echo "== running ${WORKLOAD} =="
  cd "${REPO}/contrib/perf"   # cwd matters: every path in the configs is relative to here
  kube-burner init --config "workloads/${WORKLOAD}.yml" -e metric-endpoint-local.yml
}

# The reason this script exists. A query whose metric name is wrong does not
# error, it silently indexes zero documents, which is how a wrong name survived
# three CI runs. This prints the document count per query so a rename shows up
# as an explicit EMPTY rather than as a missing file nobody looks for.
stage_check() {
  cd "${REPO}/contrib/perf"
  echo "== documents indexed per query =="
  local empty=0
  while read -r name; do
    [ -z "$name" ] && continue
    local f="metrics/${name}.json" n
    if [ -f "$f" ]; then
      n=$(python3 -Ic "import json;print(len(json.load(open('$f'))))" 2>/dev/null || echo 0)
    else
      n=0
    fi
    if [ "$n" = "0" ]; then
      printf '  EMPTY  %s\n' "$name"; empty=$((empty + 1))
    else
      printf '  %5s  %s\n' "$n" "$name"
    fi
  done < <(grep -oP '(?<=metricName: ).*' metrics-ovnk.yml)
  echo
  echo "  ${empty} empty. Each one is either a wrong metric name, a family that"
  echo "  needs --metrics-enable-scale, or a genuinely idle subsystem."
}

case "${1:-all}" in
  install)  stage_install ;;
  build)    stage_build ;;
  cluster)  stage_cluster ;;
  prom)     stage_prom ;;
  workload) stage_workload ;;
  check)    stage_check ;;
  all)      stage_install; stage_cluster; stage_prom; stage_workload; stage_check ;;
  *)        echo "usage: $0 {install|build|cluster|prom|workload|check|all}" >&2; exit 2 ;;
esac
