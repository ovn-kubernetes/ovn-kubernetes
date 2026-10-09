#!/bin/sh
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# Capture the BGP quantities that cannot reasonably be Prometheus series, and
# write them next to the kube-burner output for the run.
#
# Invoked from a kube-burner job hook. Never fails the run: a missing CRD or an
# unreachable FRR container is reported as null rather than a non-zero exit, so
# a reporting gap cannot turn a passing benchmark into a red lane.
#
# POSIX sh, not bash, and no pipefail. kube-burner does not execute the file:
# util.RunShellCmd reads it and pipes the contents to "/bin/sh -s -", so the
# shebang is ignored and on Ubuntu runners /bin/sh is dash. A bashism here
# fails the whole run rather than just the hook.
#
# Usage: collect-bgp-state.sh <label>

set -u

LABEL="${1:-unlabelled}"
OUT_DIR="${BGP_STATE_DIR:-.}"
OUT="${OUT_DIR}/bgp-state-${LABEL}.json"
FRR_CONTAINER="${FRR_CONTAINER_NAME:-frr}"
SETTLE_TIMEOUT="${BGP_STATE_SETTLE_TIMEOUT:-300}"
SETTLE_INTERVAL="${BGP_STATE_SETTLE_INTERVAL:-5}"
# Largest per-node spread, in permille of the mean, still counted as converged.
# Measured over four lanes across two CI runs the distribution is bimodal with
# nothing in between: converged snapshots sat at 1 to 5 permille, snapshots
# taken mid-reload at 138 to 311. Twenty is comfortably inside the gap.
SETTLE_SPREAD="${BGP_STATE_SETTLE_SPREAD:-20}"
# kubectl's default request timeout is unlimited, so without this a stalled
# apiserver holds the settle loop past SETTLE_TIMEOUT and the lane only ends
# at the workflow timeout.
REQUEST_TIMEOUT="${BGP_STATE_REQUEST_TIMEOUT:-30s}"

mkdir -p "${OUT_DIR}"

kget() {
  kubectl get --request-timeout="${REQUEST_TIMEOUT}" "$@" 2>/dev/null
}

# jq exits 0 and prints nothing when its input is empty, so a collector whose
# kubectl or docker call fails without output would hand an empty string to
# --argjson below and lose the whole snapshot rather than one field. Normalise
# empty output to null so a gap is reported as null, as the contract says.
nz() {
  out="$(cat)"
  if [ -n "${out}" ]; then
    printf '%s\n' "${out}"
  else
    echo 'null'
  fi
}

# FRRNodeState.status.runningConfig holds the whole rendered FRR configuration
# as one string, per node. It grows with VRF and neighbour count and the
# default etcd object size limit is 1.5MB, so its size is the measurement that
# bounds how many advertised networks a cluster can carry.
frrnodestate_sizes() {
  kget frrnodestates -o json \
    | jq -c '[.items[] | {node: .metadata.name,
                          bytes: (.status.runningConfig // "" | length),
                          reload: .status.lastReloadResult}]' 2>/dev/null \
    | nz
}

# generated = RAs x nodes x matching source FRRConfigurations
generated_frrconfigs() {
  kget frrconfigurations -A -l k8s.ovn.org/route-advertisements -o json \
    | jq -c '{count: (.items | length),
              total_bytes: ([.items[] | tojson | length] | add // 0),
              max_bytes:   ([.items[] | tojson | length] | max // 0)}' 2>/dev/null \
    | nz
}

# one per (node, peer, VRF); the cardinality is multiplicative.
#
# Broken down by status and by peer because the first two CI runs both showed
# twice as many objects as nodes with only half established, while the peer
# itself reported every session up and no failures. The counts alone cannot
# say whether that is a second configured neighbour that never comes up, a
# per-address-family artifact, or stale objects, so record enough to tell.
bgpsessionstates() {
  kget bgpsessionstates -A -o json \
    | jq -c '{count: (.items | length),
              established: ([.items[] | select(.status.bgpStatus == "Established")] | length),
              byStatus: ([.items[] | .status.bgpStatus // "unknown"] | group_by(.)
                         | map({key: .[0], value: length}) | from_entries),
              byPeer:   ([.items[] | .metadata.labels["frrk8s.metallb.io/peer"] // "none"] | group_by(.)
                         | map({key: .[0], value: length}) | from_entries),
              byVRF:    ([.items[] | .metadata.labels["frrk8s.metallb.io/vrf"] // "none"] | group_by(.)
                         | map({key: (.[0] | if . == "" then "default" else . end), value: length}) | from_entries),
              nodes:    ([.items[] | .metadata.labels["frrk8s.metallb.io/node"]] | unique | length)}' 2>/dev/null \
    | nz
}

# How many neighbours the source configurations actually ask each node to peer
# with. If this is 2 then the half-established session count is the harness
# configuring a second neighbour, not an ovn-kubernetes scale property.
source_neighbors() {
  kget frrconfigurations -A -o json \
    | jq -c '[.items[] | select(.metadata.labels["k8s.ovn.org/route-advertisements"] == null)
              | {name: .metadata.name,
                 routers: [.spec.bgp.routers[]? | {vrf: (.vrf // "default"),
                                                   neighbors: [.neighbors[]?.address]}]}]' 2>/dev/null \
    | nz
}

routeadvertisements() {
  kget routeadvertisements -o json \
    | jq -c '{count: (.items | length),
              accepted: ([.items[] | select(.status.conditions[]? | select(.type=="Accepted" and .status=="True"))] | length)}' 2>/dev/null \
    | nz
}

# Nothing inside the cluster observes the peer's table. Reduced to the counts
# that matter: the full vtysh output is per-peer and would dominate the
# artifact once the ladder reaches hundreds of sessions.
#
# Reported per address family. FRR keeps ipv4Unicast and ipv6Unicast apart in
# the summary, and the kind harness peers over both, so reading one family
# would show an IPv6 or dual-stack lane as half its sessions missing.
peer_bgp_summary() {
  if command -v docker >/dev/null 2>&1 && docker inspect "${FRR_CONTAINER}" >/dev/null 2>&1; then
    docker exec "${FRR_CONTAINER}" vtysh -c "show bgp summary json" 2>/dev/null \
      | jq -c 'def family(f): (.[f] // {})
                 | {ribCount, peerCount, failedPeers,
                    prefixesReceivedFromNodes: ([.peers[]?.pfxRcd] | add // 0),
                    prefixesSentToNodes: ([.peers[]?.pfxSnt] | add // 0)};
               {ipv4Unicast: family("ipv4Unicast"), ipv6Unicast: family("ipv6Unicast")}' 2>/dev/null \
      | nz
  else
    echo 'null'
  fi
}

# kube-burner runs beforeCleanup straight after object creation and before
# jobPause, so without this the snapshot is taken mid-convergence. The first CI
# run proved it: the peer had received 54 of 120 advertised prefixes and
# runningConfig was a fifth of its settled size.
#
# Settle on the two quantities that move last and need nothing but kubectl: the
# generated FRRConfiguration count and the total runningConfig size. Both have
# to hold still for two consecutive polls. A timeout only means the snapshot is
# taken anyway, matching this script's contract of never failing the run.
settle_signature() {
  printf '%s/%s' \
    "$(kget frrconfigurations -A -l k8s.ovn.org/route-advertisements -o json \
         | jq -r '.items | length' 2>/dev/null || echo x)" \
    "$(kget frrnodestates -o json \
         | jq -r '[.items[].status.runningConfig // "" | length] | add // 0' 2>/dev/null || echo x)"
}

# Spread of runningConfig size across nodes, in permille of the mean.
#
# This is the check that a stable total cannot make. Every node in these lanes
# carries every advertised network, so at convergence they must agree; while
# frr-k8s is still reloading them in waves they do not, and the total holds
# still between waves for long enough to look settled. Two CI runs snapshotted
# four lanes mid-reload for exactly that reason, one of them a lane that had
# converged in the previous run, so it is a race rather than a size threshold.
#
# Reports 1000, a full 100%, when the value cannot be computed, so a failed
# query is never mistaken for agreement.
node_spread_permille() {
  kget frrnodestates -o json \
    | jq -r '[.items[].status.runningConfig // "" | length] as $b
             | if ($b | length) == 0 or (($b | add) // 0) == 0 then 1000
               else ((($b | max) - ($b | min)) * 1000 / (($b | add) / ($b | length))) | floor
               end' 2>/dev/null \
    || echo 1000
}

# Set by wait_for_settle for the snapshot to record.
SETTLE_OBSERVED_SPREAD=1000
SETTLE_WAITED=0
SETTLE_CONVERGED=false

wait_for_settle() {
  elapsed=0
  previous=""
  stable=0
  spread=1000
  while [ "${elapsed}" -lt "${SETTLE_TIMEOUT}" ]; do
    current="$(settle_signature)"
    spread="$(node_spread_permille)"
    if [ "${current}" = "${previous}" ]; then
      stable=$((stable + 1))
    else
      stable=0
    fi
    # Both conditions: the totals have stopped moving and the nodes agree.
    if [ "${stable}" -ge 2 ] && [ "${spread}" -le "${SETTLE_SPREAD}" ]; then
      SETTLE_OBSERVED_SPREAD="${spread}"
      SETTLE_WAITED="${elapsed}"
      SETTLE_CONVERGED=true
      echo "settled after ${elapsed}s at ${current}, node spread ${spread}/1000"
      return 0
    fi
    previous="${current}"
    sleep "${SETTLE_INTERVAL}"
    elapsed=$((elapsed + SETTLE_INTERVAL))
  done
  # Snapshot anyway rather than failing the run, but say so, and record the
  # spread so the reading can be discarded on inspection. A workload whose
  # nodes legitimately differ, dynamic UDN allocation or a populated
  # nodeSelector, lands here every time and needs BGP_STATE_SETTLE_SPREAD
  # raised rather than this treated as a defect.
  SETTLE_OBSERVED_SPREAD="${spread}"
  SETTLE_WAITED="${elapsed}"
  SETTLE_CONVERGED=false
  echo "did not settle within ${SETTLE_TIMEOUT}s, snapshotting anyway at ${previous}," \
       "node spread ${spread}/1000 against a threshold of ${SETTLE_SPREAD}/1000"
  return 0
}

wait_for_settle

jq -n \
  --arg label "${LABEL}" \
  --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson frrnodestates "$(frrnodestate_sizes)" \
  --argjson generated "$(generated_frrconfigs)" \
  --argjson sessions "$(bgpsessionstates)" \
  --argjson ras "$(routeadvertisements)" \
  --argjson peer "$(peer_bgp_summary)" \
  --argjson srcneigh "$(source_neighbors)" \
  --argjson settle "{\"converged\": ${SETTLE_CONVERGED}, \"waitedSeconds\": ${SETTLE_WAITED}, \"nodeSpreadPermille\": ${SETTLE_OBSERVED_SPREAD}, \"thresholdPermille\": ${SETTLE_SPREAD}}" \
  '{label: $label, timestamp: $ts,
    settle: $settle,
    routeAdvertisements: $ras,
    generatedFRRConfigurations: $generated,
    frrNodeStates: $frrnodestates,
    bgpSessionStates: $sessions,
    sourceFRRConfigurations: $srcneigh,
    peerBGPSummary: $peer}' \
  > "${OUT}" 2>/dev/null || echo '{}' > "${OUT}"

echo "wrote ${OUT}"
exit 0
