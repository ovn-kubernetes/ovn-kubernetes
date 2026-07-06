#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# Deploy the external BGP peer used by the route-advertisements e2e tests: an
# FRR router container acting as BGP route reflector for the kind cluster and
# a BGP server container behind it (see the topology diagram below, in
# deploy_bgp_external_server).
#
# This file is sourced by kind-common.sh for the kind-helm.sh execution path,
# and can also be a standalone entrypoint for downstream CI test frameworks
# that manage the cluster themselves:
#
#   setup-bgp-peer.sh deploy    deploy the external FRR router and BGP server
#   setup-bgp-peer.sh destroy   remove them
#
# Standalone invocations operate on an existing cluster (KUBECONFIG must point
# at it) and honor the same environment knobs as the kind flows, notably:
# KIND_CLUSTER_NAME, PLATFORM_IPV4_SUPPORT, PLATFORM_IPV6_SUPPORT,
# BGP_SERVER_NET_SUBNET_IPV4/IPV6, BGP_SERVER_HOST_PORT, ENABLE_EVPN,
# ADVERTISED_UDN_ISOLATION_MODE, FRR_EXTERNAL_DEMO_IMAGE and FRR_TMP_DIR.
# Export the same FRR_TMP_DIR to this script and to setup-frr-k8s.sh to share
# one frr-k8s checkout across separate invocations.

# Include guard: kind-common.sh sources both setup scripts, setup-bgp-peer.sh
# sources setup-frr-k8s.sh, and standalone invocations source kind-common.sh
# back, so every file in the cycle is reached more than once. Without the
# guard, a standalone invocation would even recurse endlessly (the standalone
# check below cannot tell an executed file from the same file re-sourced);
# return early instead and make re-sourcing a no-op.
if [ -n "${__SETUP_BGP_PEER_SH_SOURCED:-}" ]; then
  return 0
fi
__SETUP_BGP_PEER_SH_SOURCED=1

SETUP_BGP_PEER_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

# The external FRR container is deployed through the demo shipped in the
# pinned frr-k8s checkout, so share clone_frr_k8s and the version pins.
source "${SETUP_BGP_PEER_DIR}/setup-frr-k8s.sh"

deploy_frr_external_container() {
  echo "Deploying FRR external container ..."
  clone_frr_k8s

  pushd "$FRR_TMP_DIR" || exit 1
  run_kubectl apply -f frr-k8s/charts/frr-k8s/charts/crds/templates/
  popd || exit 1

  # apply the demo which will deploy an external FRR container that the cluster
  # can peer with acting as BGP (reflector) external gateway
  pushd "${FRR_TMP_DIR}"/frr-k8s/hack/demo || exit 1
  # modify config template to configure neighbors as route reflector clients
  # First check if IPv4 network already exists
  grep -q 'network '"${BGP_SERVER_NET_SUBNET_IPV4}" frr/frr.conf.tmpl || \
    sed -i '/address-family ipv4 unicast/a \ \ network '"${BGP_SERVER_NET_SUBNET_IPV4}"'' frr/frr.conf.tmpl

  # Add route reflector client config
  sed -i '/remote-as 64512/a \ neighbor {{ . }} route-reflector-client' frr/frr.conf.tmpl

  if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    # Check if IPv6 address-family section exists
    if ! grep -q 'address-family ipv6 unicast' frr/frr.conf.tmpl; then
      # Add IPv6 address-family section if it doesn't exist
      sed -i '/exit-address-family/a \ \
  address-family ipv6 unicast\
    network '"${BGP_SERVER_NET_SUBNET_IPV6}"'\
  exit-address-family' frr/frr.conf.tmpl
    else
      # Add network to existing IPv6 section
      sed -i '/address-family ipv6 unicast/a \ \ network '"${BGP_SERVER_NET_SUBNET_IPV6}"'' frr/frr.conf.tmpl
    fi

    # Add route-reflector-client for IPv6 neighbors
    sed -i '/neighbor fc00.*remote-as 64512/a \ neighbor {{ . }} route-reflector-client' frr/frr.conf.tmpl
  fi
  if [ "${OCI_BIN}" == "podman" ]; then
    # frr-k8s' demo script prefers docker when both docker and podman are
    # installed. Force its podman path, and avoid its host-network fallback
    # because podman cannot later attach a host-network container to bgpnet.
    replace_in_file_or_exit \
      ./demo.sh \
      'CLI=docker' \
      'CLI=podman'
    replace_in_file_or_exit \
      ./demo.sh \
      'CLI_BR_NET_BY_SUBNET_FN="docker_get_br_net_by_subnet"' \
      'CLI_BR_NET_BY_SUBNET_FN="podman_get_br_net_by_subnet"'
    sed -i '/^pushd \.\/frr\/ && {/i\
if [ "$CLI" = "podman" ]; then\
    NETWORK=${FRR_K8S_DEMO_NETWORK:-kind}\
fi\
' ./demo.sh
  fi
  ./demo.sh
  popd || exit 1
  if frr_k8s_remote_enabled && [ -n "${DPU_SIM_GATEWAY_NETWORK:-}" ]; then
    configure_dpu_sim_frr_gateway_peers
  fi
  if  [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    # Enable IPv6 forwarding in FRR
    $OCI_BIN exec frr sysctl -w net.ipv6.conf.all.forwarding=1
    # Enable keep_addr_on_down to preserve IPv6 addresses during VRF enslavement.
    # Without this, IPv6 global addresses are removed when interfaces are moved to a VRF,
    # causing FRR/zebra to fail creating FIB nexthop groups ("no fib nhg" bug).
    # See: https://docs.kernel.org/networking/vrf.html (section 4: Enslave L3 interfaces)
    #      https://github.com/FRRouting/frr/issues/1666
    $OCI_BIN exec frr sysctl -w net.ipv6.conf.all.keep_addr_on_down=1
  fi

  if [ "$ENABLE_EVPN" == true ]; then
    echo "Configuring global EVPN BGP on external FRR (advertise-all-vni + neighbor activation)..."
    # Enable l2vpn evpn address-family, activate all neighbors, and advertise-all-vni.
    # Neighbors are already configured by demo.sh; extract them from the running config.
    # This is cluster-level infrastructure shared across all EVPN tests; configured once
    # at install time so individual tests don't need to manage it.
    # Wait for FRR daemons to be ready ("Not all daemons are up, cannot write config").
    local attempts=0 daemon_status
    while ! daemon_status=$($OCI_BIN exec frr vtysh -c "show daemons" 2>&1); do
      if (( ++attempts > 30 )); then
        echo "error: FRR daemons did not become ready after 30 attempts"
        echo "last daemon status: $daemon_status"
        exit 1
      fi
      sleep 1
    done
    local bgp_neighbors vtysh_cmds
    bgp_neighbors=$($OCI_BIN exec frr vtysh -c "show running-config" | grep "^ neighbor.*remote-as" | awk '{print $2}')
    vtysh_cmds=(-c "configure terminal" -c "router bgp 64512" -c "address-family l2vpn evpn")
    for neighbor in $bgp_neighbors; do
      vtysh_cmds+=(-c "neighbor $neighbor activate")
      vtysh_cmds+=(-c "neighbor $neighbor route-reflector-client")
    done
    vtysh_cmds+=(-c "advertise-all-vni" -c "exit-address-family" -c "end" -c "write memory")
    $OCI_BIN exec frr vtysh "${vtysh_cmds[@]}"
    echo "Global EVPN BGP config complete on external FRR"
  fi
}

deploy_bgp_external_server() {
  # We create an external docker container that acts as the server (or client) outside the cluster
  # in the e2e tests that levergae router advertisements.
  # This container will be connected to the frr container deployed above to simulate a realistic
  # network topology
  # -----------------               ------------------                         ---------------------
  # |               | 172.26.0.0/16 |                |       172.18.0.0/16     | ovn-control-plane |
  # |   external    |<------------- |   FRR router   |<------ KIND cluster --  ---------------------
  # |    server     |               |                |                         |    ovn-worker     |   (client pod advertised
  # -----------------               ------------------                         ---------------------    using RouteAdvertisements
  #                                                                            |    ovn-worker2    |    from default pod network)
  #                                                                            ---------------------
  local ipv6_network
  if [ "$PLATFORM_IPV4_SUPPORT" == true ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    ipv6_network="--ipv6 --subnet=${BGP_SERVER_NET_SUBNET_IPV6}"
  elif  [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    ipv6_network="--ipv6 --subnet=${BGP_SERVER_NET_SUBNET_IPV6}"
  else
    ipv6_network=""
  fi
  $OCI_BIN rm -f bgpserver || true
  $OCI_BIN network rm -f bgpnet || true
  $OCI_BIN network create --subnet="${BGP_SERVER_NET_SUBNET_IPV4}" ${ipv6_network} --driver bridge bgpnet
  $OCI_BIN network connect bgpnet frr
  $OCI_BIN run  --cap-add NET_ADMIN --user 0  -d --network bgpnet  --rm  --name bgpserver -p "${BGP_SERVER_HOST_PORT}:8080"  registry.k8s.io/e2e-test-images/agnhost:2.45 netexec
  # let's make the bgp external server have its default route towards FRR router so that we don't need to add routes during tests back to the pods in the
  # cluster for return traffic
  local bgp_network_frr_v4 bgp_network_frr_v6 kind_network_frr_v4 kind_network_frr_v6
  bgp_network_frr_v4=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.bgpnet.IPAddress}}' frr)
  echo "FRR bgp network IPv4: ${bgp_network_frr_v4}"
  $OCI_BIN exec bgpserver ip route replace default via "$bgp_network_frr_v4"
  if  [ "$PLATFORM_IPV6_SUPPORT" == true ] ; then
    bgp_network_frr_v6=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.bgpnet.GlobalIPv6Address}}' frr)
    echo "FRR bgp network IPv6: ${bgp_network_frr_v6}"
    $OCI_BIN exec bgpserver ip -6 route replace default via "$bgp_network_frr_v6"
  fi
  if [ "$ADVERTISED_UDN_ISOLATION_MODE" == "loose" ]; then
    kind_network_frr_v4=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' frr)
    echo "FRR kind network IPv4: ${kind_network_frr_v4}"
    # If UDN isolation is in loose disabled, we need to set the default gateway for the nodes in the cluster
    # to the FRR router so that cross-UDN traffic can be routed back to the pods in the cluster in the loose mode.
    echo "Setting default gateway for nodes in the cluster to FRR router IPv4: ${kind_network_frr_v4}"
    set_nodes_default_gw "$kind_network_frr_v4"
    if  [ "$PLATFORM_IPV6_SUPPORT" == true ] ; then
      kind_network_frr_v6=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.kind.GlobalIPv6Address}}' frr)
      echo "FRR kind network IPv6: ${kind_network_frr_v6}"
      set_nodes_default_gw "$kind_network_frr_v6"
    fi
  else
    # disable the default route to make sure the container only routes accross
    # directly connected or learnt networks (doing this at the very end since
    # docker changes the routing table when a new network is connected)
    $OCI_BIN exec frr ip route delete default
    $OCI_BIN exec frr ip route
    $OCI_BIN exec frr ip -6 route delete default
    $OCI_BIN exec frr ip -6 route
  fi
}

set_nodes_default_gw() {
  local gw="$1"
  local ip_cmd="ip"
  local route_cmd="route replace default via"

  # Check if $gw is IPv6 (contains ':')
  if [[ "$gw" == *:* ]]; then
    ip_cmd="ip -6"
  fi

  KIND_NODES=$(kind_get_nodes)
  for node in $KIND_NODES; do
    $OCI_BIN exec "$node" $ip_cmd $route_cmd "$gw"
  done
}

destroy_bgp() {
  if $OCI_BIN ps --format '{{.Names}}' | grep -Eq '^bgpserver$'; then
      $OCI_BIN stop bgpserver
  fi
  if $OCI_BIN ps --format '{{.Names}}' | grep -Eq '^frr$'; then
      $OCI_BIN stop frr
  fi
  if $OCI_BIN network ls --format '{{.Name}}' | grep -q '^bgpnet$'; then
      $OCI_BIN network rm bgpnet
  fi
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  set -eo pipefail
  # Operate on an existing cluster; kind-common's defaults would otherwise
  # scrub KUBECONFIG, and route advertisements imply multi-network there.
  KIND_CREATE=${KIND_CREATE:-false}
  ENABLE_MULTI_NET=${ENABLE_MULTI_NET:-true}
  DIR="${SETUP_BGP_PEER_DIR}"
  source "${DIR}/kind-common.sh"
  source "${DIR}/kind-dpu-sim-lib.sh"
  set_common_default_params

  case "${1:-}" in
    deploy)
      deploy_frr_external_container
      deploy_bgp_external_server
      ;;
    destroy)
      destroy_bgp
      ;;
    *)
      echo "Usage: $0 deploy | destroy" >&2
      exit 2
      ;;
  esac
fi
