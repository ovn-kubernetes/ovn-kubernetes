#!/usr/bin/env bash

set -eo pipefail

# Returns the full directory name of the script
DIR="$( cd -- "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
export DIR

export OCI_BIN=${KIND_EXPERIMENTAL_PROVIDER:-docker}

# Source the kind-common file from the same directory where this script is located
source "${DIR}/kind-common"

set_default_params() {
  set_common_default_params

  # Set default values
  export KIND_CONFIG=${KIND_CONFIG:-}
  export KIND_INSTALL_INGRESS=${KIND_INSTALL_INGRESS:-false}
  export KIND_INSTALL_METALLB=${KIND_INSTALL_METALLB:-false}
  export KIND_INSTALL_PLUGINS=${KIND_INSTALL_PLUGINS:-false}
  export KIND_INSTALL_KUBEVIRT=${KIND_INSTALL_KUBEVIRT:-false}
  export OVN_HA=${OVN_HA:-false}
  export OVN_MULTICAST_ENABLE=${OVN_MULTICAST_ENABLE:-false}
  export OVN_HYBRID_OVERLAY_ENABLE=${OVN_HYBRID_OVERLAY_ENABLE:-false}
  export OVN_OBSERV_ENABLE=${OVN_OBSERV_ENABLE:-false}
  export OVN_EMPTY_LB_EVENTS=${OVN_EMPTY_LB_EVENTS:-false}
  export KIND_REMOVE_TAINT=${KIND_REMOVE_TAINT:-true}
  export ENABLE_MULTI_NET=${ENABLE_MULTI_NET:-false}
  export ENABLE_NETWORK_SEGMENTATION=${ENABLE_NETWORK_SEGMENTATION:-false}
  export ENABLE_PRE_CONF_UDN_ADDR=${ENABLE_PRE_CONF_UDN_ADDR:-false}
  export OVN_NETWORK_QOS_ENABLE=${OVN_NETWORK_QOS_ENABLE:-false}
  export KIND_NUM_WORKER=${KIND_NUM_WORKER:-2}
  export KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn}
  export OVN_IMAGE=${OVN_IMAGE:-'ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu:latest'}

  # Setup KUBECONFIG patch based on cluster-name
  export KUBECONFIG=${KUBECONFIG:-${HOME}/${KIND_CLUSTER_NAME}.conf}

  # IPv4/IPv6 support
  export PLATFORM_IPV4_SUPPORT=${PLATFORM_IPV4_SUPPORT:-true}
  export PLATFORM_IPV6_SUPPORT=${PLATFORM_IPV6_SUPPORT:-false}

  # Network configuration
  export OVN_DISABLE_SNAT_MULTIPLE_GWS=${OVN_DISABLE_SNAT_MULTIPLE_GWS:-false}
  export OVN_DISABLE_PKT_MTU_CHECK=${OVN_DISABLE_PKT_MTU_CHECK:-false}
  export OVN_DISABLE_FORWARDING=${OVN_DISABLE_FORWARDING:-false}
  export OVN_ENCAP_PORT=${OVN_ENCAP_PORT:-6081}
  export OVN_GATEWAY_MODE=${OVN_GATEWAY_MODE:-shared}
  export OVN_DUMMY_GATEWAY_BRIDGE=${OVN_DUMMY_GATEWAY_BRIDGE:-false}

  # Image and build configuration
  export OVN_REPO=${OVN_REPO:-""}
  export OVN_GITREF=${OVN_GITREF:-""}

  # Logging configuration
  export MASTER_LOG_LEVEL=${MASTER_LOG_LEVEL:-5}
  export NODE_LOG_LEVEL=${NODE_LOG_LEVEL:-5}
  export DBCHECKER_LOG_LEVEL=${DBCHECKER_LOG_LEVEL:-5}
  export OVN_LOG_LEVEL_NORTHD=${OVN_LOG_LEVEL_NORTHD:-"-vconsole:info -vfile:info"}
  export OVN_LOG_LEVEL_NB=${OVN_LOG_LEVEL_NB:-"-vconsole:info -vfile:info"}
  export OVN_LOG_LEVEL_SB=${OVN_LOG_LEVEL_SB:-"-vconsole:info -vfile:info"}
  export OVN_LOG_LEVEL_CONTROLLER=${OVN_LOG_LEVEL_CONTROLLER:-"-vconsole:info"}
  export LIBOVSDB_CLIENT_LOGFILE=${LIBOVSDB_CLIENT_LOGFILE:-}

  # Network monitoring
  export OVN_NETFLOW_TARGETS=${OVN_NETFLOW_TARGETS:-}
  export OVN_SFLOW_TARGETS=${OVN_SFLOW_TARGETS:-}
  export OVN_IPFIX_TARGETS=${OVN_IPFIX_TARGETS:-}
  export OVN_IPFIX_SAMPLING=${OVN_IPFIX_SAMPLING:-400}
  export OVN_IPFIX_CACHE_MAX_FLOWS=${OVN_IPFIX_CACHE_MAX_FLOWS:-0}
  export OVN_IPFIX_CACHE_ACTIVE_TIMEOUT=${OVN_IPFIX_CACHE_ACTIVE_TIMEOUT:-60}

  # System configuration
  export KIND_ALLOW_SYSTEM_WRITES=${KIND_ALLOW_SYSTEM_WRITES:-false}
  export OVN_ENABLE_EX_GW_NETWORK_BRIDGE=${OVN_ENABLE_EX_GW_NETWORK_BRIDGE:-false}
  export KIND_LOCAL_REGISTRY=${KIND_LOCAL_REGISTRY:-false}
  export KIND_LOCAL_REGISTRY_NAME=${KIND_LOCAL_REGISTRY_NAME:-kind-registry}
  export KIND_LOCAL_REGISTRY_PORT=${KIND_LOCAL_REGISTRY_PORT:-5000}
  export KIND_DNS_DOMAIN=${KIND_DNS_DOMAIN:-"cluster.local"}
  export RUN_IN_CONTAINER=${RUN_IN_CONTAINER:-false}
  export OVN_EGRESSIP_HEALTHCHECK_PORT=${OVN_EGRESSIP_HEALTHCHECK_PORT:-9107}

  # Advanced features
  export ENABLE_IPSEC=${ENABLE_IPSEC:-false}
  export OVN_METRICS_SCALE_ENABLE=${OVN_METRICS_SCALE_ENABLE:-false}
  export OVN_COMPACT_MODE=${OVN_COMPACT_MODE:-false}
  export OVN_ISOLATED=${OVN_ISOLATED:-false}
  export OVN_MTU=${OVN_MTU:-1400}

  # Route advertisements
  export ENABLE_ROUTE_ADVERTISEMENTS=${ENABLE_ROUTE_ADVERTISEMENTS:-false}
  export ADVERTISE_DEFAULT_NETWORK=${ADVERTISE_DEFAULT_NETWORK:-false}
  export ADVERTISED_UDN_ISOLATION_MODE=${ADVERTISED_UDN_ISOLATION_MODE:-strict}

  # Cluster management
  export KIND_CREATE=${KIND_CREATE:-true}
  export KIND_ADD_NODES=${KIND_ADD_NODES:-false}
  export KIND_CLUSTER_LOGLEVEL=${KIND_CLUSTER_LOGLEVEL:-4}

  # Validated params that work
  export MASQUERADE_SUBNET_IPV4=${MASQUERADE_SUBNET_IPV4:-169.254.0.0/17}
  export MASQUERADE_SUBNET_IPV6=${MASQUERADE_SUBNET_IPV6:-fd69::/112}

  # Input not currently validated. Modify outside script at your own risk.
  # These are the same values defaulted to in KIND code (kind/default.go).
  # NOTE: KIND NET_CIDR_IPV6 default use a /64 but OVN have a /64 per host
  # so it needs to use a larger subnet
  #  Upstream - NET_CIDR_IPV6=fd00:10:244::/64 SVC_CIDR_IPV6=fd00:10:96::/112
  export NET_CIDR_IPV4=${NET_CIDR_IPV4:-10.244.0.0/16}
  export NET_SECOND_CIDR_IPV4=${NET_SECOND_CIDR_IPV4:-172.19.0.0/16}
  export SVC_CIDR_IPV4=${SVC_CIDR_IPV4:-10.96.0.0/16}
  export NET_CIDR_IPV6=${NET_CIDR_IPV6:-fd00:10:244::/48}
  export SVC_CIDR_IPV6=${SVC_CIDR_IPV6:-fd00:10:96::/112}
  export JOIN_SUBNET_IPV4=${JOIN_SUBNET_IPV4:-100.64.0.0/16}
  export JOIN_SUBNET_IPV6=${JOIN_SUBNET_IPV6:-fd98::/64}
  export TRANSIT_SWITCH_SUBNET_IPV4=${TRANSIT_SWITCH_SUBNET_IPV4:-100.88.0.0/16}
  export TRANSIT_SWITCH_SUBNET_IPV6=${TRANSIT_SWITCH_SUBNET_IPV6:-fd97::/64}
  export METALLB_CLIENT_NET_SUBNET_IPV4=${METALLB_CLIENT_NET_SUBNET_IPV4:-172.22.0.0/16}
  export METALLB_CLIENT_NET_SUBNET_IPV6=${METALLB_CLIENT_NET_SUBNET_IPV6:-fc00:f853:ccd:e792::/64}

  export KIND_NUM_MASTER=1
  if [ "$OVN_HA" == true ]; then
    KIND_NUM_MASTER=3
  fi

  OVN_ENABLE_INTERCONNECT=${OVN_ENABLE_INTERCONNECT:-true}
  if [ "$OVN_COMPACT_MODE" == true ] && [ "$OVN_ENABLE_INTERCONNECT" != false ]; then
     echo "Compact mode cannot be used together with Interconnect"
     exit 1
  fi

  if [ "$OVN_ENABLE_INTERCONNECT" == true ]; then
    KIND_NUM_NODES_PER_ZONE=${KIND_NUM_NODES_PER_ZONE:-1}
    TOTAL_NODES=$((KIND_NUM_WORKER + KIND_NUM_MASTER))
    if [[ ${KIND_NUM_NODES_PER_ZONE} -gt 1 ]] && [[ $((TOTAL_NODES % KIND_NUM_NODES_PER_ZONE)) -ne 0 ]]; then
      echo "(Total k8s nodes / number of nodes per zone) should be zero"
      exit 1
    fi
  else
    KIND_NUM_NODES_PER_ZONE=0
  fi

  export OVN_ENABLE_DNSNAMERESOLVER=${OVN_ENABLE_DNSNAMERESOLVER:-false}
}

usage() {
    echo "usage: kind.sh [--delete]"
    echo "       [ -cf  | --config-file <file> ]"
    echo "       [ -kt  | --keep-taint ]"
    echo "       [ -ha  | --ha-enabled ]"
    echo "       [ -me  | --multicast-enabled ]"
    echo "       [ -ho  | --hybrid-enabled ]"
    echo "       [ -el  | --ovn-empty-lb-events ]"
    echo "       [ -ii  | --install-ingress ]"
    echo "       [ -mlb | --install-metallb ]"
    echo "       [ -pl  | --install-cni-plugins ]"
    echo "       [ -ikv | --install-kubevirt ]"
    echo "       [ -mne | --multi-network-enable ]"
    echo "       [ -nse | --network-segmentation-enable ]"
    echo "       [ -uae | --preconfigured-udn-addresses-enable ]"
    echo "       [ -nqe | --network-qos-enable ]"
    echo "       [ -wk  | --num-workers <num> ]"
    echo "       [ -ic  | --enable-interconnect]"
    echo "       [ -npz | --node-per-zone ]"
    echo "       [ -cn  | --cluster-name ]"
    echo "       [ -n4  | --no-ipv4 ]"
    echo "       [ -i6  | --ipv6 ]"
    echo "       [ -ds  | --disable-snat-multiple-gws ]"
    echo "       [ -dp  | --disable-pkt-mtu-check ]"
    echo "       [ -df  | --disable-forwarding ]"
    echo "       [ -ecp | --encap-port <port> ]"
    echo "       [ -gm  | --gateway-mode <mode> ]"
    echo "       [ -dgb | --dummy-gateway-bridge ]"
    echo "       [ -ov  | --ovn-image <image> ]"
    echo "       [ -ovr | --ovn-repo <repo> ]"
    echo "       [ -ovg | --ovn-gitref <ref> ]"
    echo "       [ -ml  | --master-loglevel <level> ]"
    echo "       [ -nl  | --node-loglevel <level> ]"
    echo "       [ -dbl | --dbchecker-loglevel <level> ]"
    echo "       [ -ndl | --ovn-loglevel-northd <level> ]"
    echo "       [ -nbl | --ovn-loglevel-nb <level> ]"
    echo "       [ -sbl | --ovn-loglevel-sb <level> ]"
    echo "       [ -cl  | --ovn-loglevel-controller <level> ]"
    echo "       [ -lcl | --libovsdb-client-logfile <file> ]"
    echo "       [ -ep  | --experimental-provider <provider> ]"
    echo "       [ -nf  | --netflow-targets <targets> ]"
    echo "       [ -sf  | --sflow-targets <targets> ]"
    echo "       [ -if  | --ipfix-targets <targets> ]"
    echo "       [ -ifs | --ipfix-sampling <num> ]"
    echo "       [ -ifm | --ipfix-cache-max-flows <num> ]"
    echo "       [ -ifa | --ipfix-cache-active-timeout <num> ]"
    echo "       [ -sw  | --allow-system-writes ]"
    echo "       [ -eb  | --egress-gw-separate-bridge ]"
    echo "       [ -lr  | --local-kind-registry ]"
    echo "       [ -dd  | --dns-domain <domain> ]"
    echo "       [ -kc  | --kubeconfig <path> ]"
    echo "       [ -ric | --run-in-container ]"
    echo "       [ -ehp | --egress-ip-healthcheck-port <port> ]"
    echo "       [ -is  | --ipsec ]"
    echo "       [ -sm  | --scale-metrics ]"
    echo "       [ -cm  | --compact-mode ]"
    echo "       [ -ce  | --enable-central ]"
    echo "       [ -mtu | --mtu <size> ]"
    echo "       [ --isolated ]"
    echo "       [ --deploy ]"
    echo "       [ --add-nodes ]"
    echo "       [ -rae | --enable-route-advertisements ]"
    echo "       [ -adv | --advertise-default-network ]"
    echo "       [ -rud | --routed-udn-isolation-disable ]"
    echo "       [ -h ]"
    echo ""
    echo "--delete                                      Delete current cluster"
    echo "-cf  | --config-file                          Name of the KIND configuration file"
    echo "-kt  | --keep-taint                           Do not remove taint components"
    echo "                                              DEFAULT: Remove taint components"
    echo "-me  | --multicast-enabled                    Enable multicast. DEFAULT: Disabled"
    echo "-ho  | --hybrid-enabled                       Enable hybrid overlay. DEFAULT: Disabled"
    echo "-obs | --observability                        Enable observability. DEFAULT: Disabled"
    echo "-el  | --ovn-empty-lb-events                  Enable empty-lb-events generation for LB without backends. DEFAULT: Disabled"
    echo "-ii  | --install-ingress                      Flag to install Ingress Components."
    echo "                                              DEFAULT: Don't install ingress components."
    echo "-mlb | --install-metallb                      Install metallb to test service type LoadBalancer deployments"
    echo "-pl  | --install-cni-plugins                  Install CNI plugins"
    echo "-ikv | --install-kubevirt                     Install kubevirt"
    echo "-mne | --multi-network-enable                 Enable multi networks. DEFAULT: Disabled"
    echo "-nse | --network-segmentation-enable          Enable network segmentation. DEFAULT: Disabled"
    echo "-uae | --preconfigured-udn-addresses-enable   Enable connecting workloads with preconfigured network to user-defined networks. DEFAULT: Disabled"
    echo "-nqe | --network-qos-enable                   Enable network QoS. DEFAULT: Disabled"
    echo "-ha  | --ha-enabled                           Enable high availability. DEFAULT: HA Disabled"
    echo "-wk  | --num-workers                          Number of worker nodes. DEFAULT: 2 workers"
    echo "-cn  | --cluster-name                         Configure the kind cluster's name"
    echo "-dns | --enable-dnsnameresolver               Enable DNSNameResolver for resolving the DNS names used in the DNS rules of EgressFirewall."
    echo "-ce  | --enable-central                       Deploy with OVN Central (Legacy Architecture)"
    echo "-npz | --nodes-per-zone                        Specify number of nodes per zone (Default 0, which means global zone; >0 means interconnect zone, where 1 for single-node zone, >1 for multi-node zone). If this value > 1, then (total k8s nodes (workers + 1) / num of nodes per zone) should be zero."
    echo "-n4  | --no-ipv4                              Disable IPv4. DEFAULT: IPv4 Enabled."
    echo "-i6  | --ipv6                                 Enable IPv6. DEFAULT: IPv6 Disabled."
    echo "-ds  | --disable-snat-multiple-gws             Disable SNAT for multiple gws. DEFAULT: Disabled."
    echo "-dp  | --disable-pkt-mtu-check                 Disable checking packet size greater than MTU. Default: Disabled"
    echo "-df  | --disable-forwarding                   Disable forwarding on OVNK managed interfaces. Default: Disabled"
    echo "-ecp | --encap-port                            UDP port used for geneve overlay. DEFAULT: 6081"
    echo "-gm  | --gateway-mode                          Enable 'shared' or 'local' gateway mode."
    echo "                                              DEFAULT: shared."
    echo "-dgb | --dummy-gateway-bridge                  Enable dummy gateway bridge"
    echo "-ov  | --ovn-image                             Use the specified docker image instead of building locally. DEFAULT: local build."
    echo "-ovr | --ovn-repo                              Specify the repository to build OVN from"
    echo "-ovg | --ovn-gitref                            Specify the branch, tag or commit id to build OVN from"
    echo "-ml  | --master-loglevel                       Log level for ovnkube (master), DEFAULT: 5."
    echo "-nl  | --node-loglevel                         Log level for ovnkube (node), DEFAULT: 5"
    echo "-dbl | --dbchecker-loglevel                    Log level for ovn-dbchecker (ovnkube-db), DEFAULT: 5."
    echo "-ndl | --ovn-loglevel-northd                    Log config for ovn northd, DEFAULT: '-vconsole:info -vfile:info'."
    echo "-nbl | --ovn-loglevel-nb                       Log config for northbound DB DEFAULT: '-vconsole:info -vfile:info'."
    echo "-sbl | --ovn-loglevel-sb                       Log config for southbound DB DEFAULT: '-vconsole:info -vfile:info'."
    echo "-cl  | --ovn-loglevel-controller               Log config for ovn-controller DEFAULT: '-vconsole:info'."
    echo "-lcl | --libovsdb-client-logfile               Separate logs for libovsdb client into provided file. DEFAULT: do not separate."
    echo "-ep  | --experimental-provider                 Use an experimental OCI provider such as podman, instead of docker. DEFAULT: Disabled."
    echo "-eb  | --egress-gw-separate-bridge             The external gateway traffic uses a separate bridge."
    echo "-lr  | --local-kind-registry                   Configure kind to use a local docker registry rather than manually loading images"
    echo "-dd  | --dns-domain                           Configure a custom dnsDomain for k8s services, Defaults to 'cluster.local'"
    echo "-kc  | --kubeconfig                           Specify kubeconfig path"
    echo "-ric | --run-in-container                      Configure the script to be run from a docker container"
    echo "-ehp | --egress-ip-healthcheck-port           TCP port used for gRPC session by egress IP node check. DEFAULT: 9107"
    echo "-is  | --ipsec                                Enable IPsec encryption (spawns ovn-ipsec pods)"
    echo "-sm  | --scale-metrics                        Enable scale metrics"
    echo "-cm  | --compact-mode                          Enable compact mode, ovnkube master and node run in the same process."
    echo "-mtu | --mtu                                   Define the overlay mtu"
    echo "--isolated                                    Deploy with an isolated environment (no default gateway)"
    echo "--deploy                                       Deploy ovn kubernetes without restarting kind"
    echo "--add-nodes                                    Adds nodes to an existing cluster"
    echo "-rae | --enable-route-advertisements            Enable route advertisements"
    echo "-adv | --advertise-default-network             Applies a RouteAdvertisements configuration to advertise the default network on all nodes"
    echo "-rud | --routed-udn-isolation-disable          Disable isolation across BGP-advertised UDNs (sets advertised-udn-isolation-mode=loose). DEFAULT: strict."
    echo ""

}

parse_args() {
    while [ "$1" != "" ]; do
        case $1 in
            --delete )                            delete
                                                  exit
                                                  ;;
            -cf | --config-file )                 shift
                                                  if test ! -f "$1"; then
                                                      echo "$1 does not  exist"
                                                      usage
                                                      exit 1
                                                  fi
                                                  KIND_CONFIG=$1
                                                  ;;
            -kt | --keep-taint )                  KIND_REMOVE_TAINT=false
                                                  ;;
            -me | --multicast-enabled)            OVN_MULTICAST_ENABLE=true
                                                  ;;
            -ho | --hybrid-enabled )              OVN_HYBRID_OVERLAY_ENABLE=true
                                                  ;;
            -obs | --observability )              OVN_OBSERV_ENABLE=true
                                                  ;;
            -el | --ovn-empty-lb-events )         OVN_EMPTY_LB_EVENTS=true
                                                  ;;
            -ii | --install-ingress )             KIND_INSTALL_INGRESS=true
                                                  ;;
            -mlb | --install-metallb )            KIND_INSTALL_METALLB=true
                                                  ;;
            -pl | --install-cni-plugins )         KIND_INSTALL_PLUGINS=true
                                                  ;;
            -ikv | --install-kubevirt)            KIND_INSTALL_KUBEVIRT=true
                                                  ;;
            -mne | --multi-network-enable )       ENABLE_MULTI_NET=true
                                                  ;;
            -nse | --network-segmentation-enable) ENABLE_NETWORK_SEGMENTATION=true
                                                  ;;
            -uae | --preconfigured-udn-addresses-enable)    ENABLE_PRE_CONF_UDN_ADDR=true
                                                  ;;
            -nqe | --network-qos-enable )         OVN_NETWORK_QOS_ENABLE=true
                                                  ;;
            -ha | --ha-enabled )                  OVN_HA=true
                                                  KIND_NUM_MASTER=3
                                                  ;;
            -wk | --num-workers )                 shift
                                                  if ! [[ "$1" =~ ^[0-9]+$ ]]; then
                                                      echo "Invalid num-workers: $1"
                                                      usage
                                                      exit 1
                                                  fi
                                                  KIND_NUM_WORKER=$1
                                                  ;;
            -cn | --cluster-name )                shift
                                                  KIND_CLUSTER_NAME=$1
                                                  # Setup KUBECONFIG
                                                  set_default_params
                                                  ;;
            -dns | --enable-dnsnameresolver )     OVN_ENABLE_DNSNAMERESOLVER=true
                                                  ;;
            -ce | --enable-central )              OVN_ENABLE_INTERCONNECT=false
                                                  CENTRAL_ARG_PROVIDED=true
                                                  ;;
            -ic | --enable-interconnect )         OVN_ENABLE_INTERCONNECT=true
                                                  IC_ARG_PROVIDED=true
                                                  ;;
            -npz | --nodes-per-zone )             shift
                                                  if ! [[ "$1" =~ ^[0-9]+$ ]]; then
                                                      echo "Invalid num-nodes-per-zone: $1"
                                                      usage
                                                      exit 1
                                                  fi
                                                  KIND_NUM_NODES_PER_ZONE=$1
                                                  ;;
            -n4 | --no-ipv4 )                     PLATFORM_IPV4_SUPPORT=false
                                                  ;;
            -i6 | --ipv6 )                        PLATFORM_IPV6_SUPPORT=true
                                                  ;;
            -ds | --disable-snat-multiple-gws )   OVN_DISABLE_SNAT_MULTIPLE_GWS=true
                                                  ;;
            -dp | --disable-pkt-mtu-check )       OVN_DISABLE_PKT_MTU_CHECK=true
                                                  ;;
            -df | --disable-forwarding )          OVN_DISABLE_FORWARDING=true
                                                  ;;
            -ecp | --encap-port )                 shift
                                                  OVN_ENCAP_PORT=$1
                                                  ;;
            -gm | --gateway-mode )                shift
                                                  if [ "$1" != "local" ] && [ "$1" != "shared" ]; then
                                                      echo "Invalid gateway mode: $1"
                                                      usage
                                                      exit 1
                                                  fi
                                                  OVN_GATEWAY_MODE=$1
                                                  ;;
            -dgb | --dummy-gateway-bridge)        OVN_DUMMY_GATEWAY_BRIDGE=true
                                                  ;;
            -ov | --ovn-image )                    shift
                                                  OVN_IMAGE=$1
                                                  ;;
            -ovr | --ovn-repo )                    shift
                                                  OVN_REPO=$1
                                                  ;;
            -ovg | --ovn-gitref)                  shift
                                                  OVN_GITREF=$1
                                                  ;;
            -ml  | --master-loglevel )            shift
                                                  if ! [[ "$1" =~ ^[0-9]$ ]]; then
                                                      echo "Invalid master-loglevel: $1"
                                                      usage
                                                      exit 1
                                                  fi
                                                  MASTER_LOG_LEVEL=$1
                                                  ;;
            -nl  | --node-loglevel )              shift
                                                  if ! [[ "$1" =~ ^[0-9]$ ]]; then
                                                      echo "Invalid node-loglevel: $1"
                                                      usage
                                                      exit 1
                                                  fi
                                                  NODE_LOG_LEVEL=$1
                                                  ;;
            -dbl | --dbchecker-loglevel )         shift
                                                  if ! [[ "$1" =~ ^[0-9]$ ]]; then
                                                      echo "Invalid dbchecker-loglevel: $1"
                                                      usage
                                                      exit 1
                                                  fi
                                                  DBCHECKER_LOG_LEVEL=$1
                                                  ;;
            -ndl | --ovn-loglevel-northd )        shift
                                                  OVN_LOG_LEVEL_NORTHD=$1
                                                  ;;
            -nbl | --ovn-loglevel-nb )            shift
                                                  OVN_LOG_LEVEL_NB=$1
                                                  ;;
            -sbl | --ovn-loglevel-sb )            shift
                                                  OVN_LOG_LEVEL_SB=$1
                                                  ;;
            -cl  | --ovn-loglevel-controller )    shift
                                                  OVN_LOG_LEVEL_CONTROLLER=$1
                                                  ;;
            -lcl | --libovsdb-client-logfile )   shift
                                                  LIBOVSDB_CLIENT_LOGFILE=$1
                                                  ;;
            -ep | --experimental-provider )       shift
                                                  export KIND_EXPERIMENTAL_PROVIDER=$1
                                                  ;;
            -nf | --netflow-targets )             shift
                                                  OVN_NETFLOW_TARGETS=$1
                                                  ;;
            -sf | --sflow-targets )               shift
                                                  OVN_SFLOW_TARGETS=$1
                                                  ;;
            -if | --ipfix-targets )               shift
                                                  OVN_IPFIX_TARGETS=$1
                                                  ;;
            -ifs | --ipfix-sampling )             shift
                                                  OVN_IPFIX_SAMPLING=$1
                                                  ;;
            -ifm | --ipfix-cache-max-flows )      shift
                                                  OVN_IPFIX_CACHE_MAX_FLOWS=$1
                                                  ;;
            -ifa | --ipfix-cache-active-timeout ) shift
                                                  OVN_IPFIX_CACHE_ACTIVE_TIMEOUT=$1
                                                  ;;
            -sw | --allow-system-writes )         KIND_ALLOW_SYSTEM_WRITES=true
                                                  ;;
            -eb | --egress-gw-separate-bridge )    OVN_ENABLE_EX_GW_NETWORK_BRIDGE=true
                                                  ;;
            -lr | --local-kind-registry )         KIND_LOCAL_REGISTRY=true
                                                  ;;
            -dd | --dns-domain )                  shift
                                                  KIND_DNS_DOMAIN=$1
                                                  ;;
            -kc | --kubeconfig )                  shift
                                                  KUBECONFIG=$1
                                                  ;;
            -ric | --run-in-container )           RUN_IN_CONTAINER=true
                                                  ;;
            -ehp | --egress-ip-healthcheck-port ) shift
                                                  if ! [[ "$1" =~ ^[0-9]+$ ]]; then
                                                      echo "Invalid egress-ip-healthcheck-port: $1"
                                                      usage
                                                      exit 1
                                                  fi
                                                  OVN_EGRESSIP_HEALTHCHECK_PORT=$1
                                                  ;;
            -is | --ipsec )                       ENABLE_IPSEC=true
                                                  ;;
            -sm  | --scale-metrics )              OVN_METRICS_SCALE_ENABLE=true
                                                  ;;
            -cm  | --compact-mode )               OVN_COMPACT_MODE=true
                                                  ;;
            -mtu )                                shift
                                                  OVN_MTU=$1
                                                  ;;
            --isolated )                          OVN_ISOLATED=true
                                                  ;;
            --deploy )                            KIND_CREATE=false
                                                  ;;
            --add-nodes )                         KIND_ADD_NODES=true
                                                  KIND_CREATE=false
                                                  ;;
            -rae | --enable-route-advertisements) ENABLE_ROUTE_ADVERTISEMENTS=true
                                                  ;;
            -adv | --advertise-default-network)   ADVERTISE_DEFAULT_NETWORK=true
                                                  ;;
            -rud | --routed-udn-isolation-disable) ADVERTISED_UDN_ISOLATION_MODE=loose
                                                  ;;
            -h | --help )                         usage
                                                  exit
                                                  ;;
            * )                                   echo "Invalid option: $1"
                                                  usage
                                                  exit 1
        esac
        shift
    done

    if [[ -n "${CENTRAL_ARG_PROVIDED:-}" && -n "${IC_ARG_PROVIDED:-}" ]]; then
      echo "Cannot specify both --enable-central and --enable-interconnect" >&2
      exit 1
    fi
}

print_params() {
     echo "Using these parameters to deploy KIND + helm"
     echo ""
     echo "KUBECONFIG = $KUBECONFIG"
     echo "KIND_INSTALL_INGRESS = $KIND_INSTALL_INGRESS"
     echo "KIND_INSTALL_METALLB = $KIND_INSTALL_METALLB"
     echo "KIND_INSTALL_PLUGINS = $KIND_INSTALL_PLUGINS"
     echo "KIND_INSTALL_KUBEVIRT = $KIND_INSTALL_KUBEVIRT"
     echo "OVN_HA = $OVN_HA"
     echo "OVN_MULTICAST_ENABLE = $OVN_MULTICAST_ENABLE"
     echo "OVN_HYBRID_OVERLAY_ENABLE = $OVN_HYBRID_OVERLAY_ENABLE"
     echo "OVN_OBSERV_ENABLE = $OVN_OBSERV_ENABLE"
     echo "OVN_EMPTY_LB_EVENTS = $OVN_EMPTY_LB_EVENTS"
     echo "KIND_CLUSTER_NAME = $KIND_CLUSTER_NAME"
     echo "KIND_REMOVE_TAINT = $KIND_REMOVE_TAINT"
     echo "ENABLE_MULTI_NET = $ENABLE_MULTI_NET"
     echo "ENABLE_NETWORK_SEGMENTATION = $ENABLE_NETWORK_SEGMENTATION"
     echo "ENABLE_PRE_CONF_UDN_ADDR = $ENABLE_PRE_CONF_UDN_ADDR"
     echo "OVN_NETWORK_QOS_ENABLE = $OVN_NETWORK_QOS_ENABLE"
     echo "OVN_IMAGE = $OVN_IMAGE"
     echo "KIND_NUM_MASTER = $KIND_NUM_MASTER"
     echo "KIND_NUM_WORKER = $KIND_NUM_WORKER"
     echo "OVN_ENABLE_DNSNAMERESOLVER= $OVN_ENABLE_DNSNAMERESOLVER"
     echo "OVN_ENABLE_INTERCONNECT = $OVN_ENABLE_INTERCONNECT"
     echo "PLATFORM_IPV4_SUPPORT = $PLATFORM_IPV4_SUPPORT"
     echo "PLATFORM_IPV6_SUPPORT = $PLATFORM_IPV6_SUPPORT"
     echo "OVN_DISABLE_SNAT_MULTIPLE_GWS = $OVN_DISABLE_SNAT_MULTIPLE_GWS"
     echo "OVN_DISABLE_PKT_MTU_CHECK = $OVN_DISABLE_PKT_MTU_CHECK"
     echo "OVN_DISABLE_FORWARDING = $OVN_DISABLE_FORWARDING"
     echo "OVN_ENCAP_PORT = $OVN_ENCAP_PORT"
     echo "OVN_GATEWAY_MODE = $OVN_GATEWAY_MODE"
     echo "OVN_DUMMY_GATEWAY_BRIDGE = $OVN_DUMMY_GATEWAY_BRIDGE"
     echo "OVN_REPO = $OVN_REPO"
     echo "OVN_GITREF = $OVN_GITREF"
     echo "MASTER_LOG_LEVEL = $MASTER_LOG_LEVEL"
     echo "NODE_LOG_LEVEL = $NODE_LOG_LEVEL"
     echo "DBCHECKER_LOG_LEVEL = $DBCHECKER_LOG_LEVEL"
     echo "OVN_LOG_LEVEL_NORTHD = $OVN_LOG_LEVEL_NORTHD"
     echo "OVN_LOG_LEVEL_NB = $OVN_LOG_LEVEL_NB"
     echo "OVN_LOG_LEVEL_SB = $OVN_LOG_LEVEL_SB"
     echo "OVN_LOG_LEVEL_CONTROLLER = $OVN_LOG_LEVEL_CONTROLLER"
     echo "LIBOVSDB_CLIENT_LOGFILE = $LIBOVSDB_CLIENT_LOGFILE"
     echo "OVN_NETFLOW_TARGETS = $OVN_NETFLOW_TARGETS"
     echo "OVN_SFLOW_TARGETS = $OVN_SFLOW_TARGETS"
     echo "OVN_IPFIX_TARGETS = $OVN_IPFIX_TARGETS"
     echo "OVN_IPFIX_SAMPLING = $OVN_IPFIX_SAMPLING"
     echo "OVN_IPFIX_CACHE_MAX_FLOWS = $OVN_IPFIX_CACHE_MAX_FLOWS"
     echo "OVN_IPFIX_CACHE_ACTIVE_TIMEOUT = $OVN_IPFIX_CACHE_ACTIVE_TIMEOUT"
     echo "KIND_ALLOW_SYSTEM_WRITES = $KIND_ALLOW_SYSTEM_WRITES"
     echo "OVN_ENABLE_EX_GW_NETWORK_BRIDGE = $OVN_ENABLE_EX_GW_NETWORK_BRIDGE"
     echo "KIND_LOCAL_REGISTRY = $KIND_LOCAL_REGISTRY"
     echo "KIND_LOCAL_REGISTRY_NAME = $KIND_LOCAL_REGISTRY_NAME"
     echo "KIND_LOCAL_REGISTRY_PORT = $KIND_LOCAL_REGISTRY_PORT"
     echo "KIND_DNS_DOMAIN = $KIND_DNS_DOMAIN"
     echo "RUN_IN_CONTAINER = $RUN_IN_CONTAINER"
     echo "OVN_EGRESSIP_HEALTHCHECK_PORT = $OVN_EGRESSIP_HEALTHCHECK_PORT"
     echo "ENABLE_IPSEC = $ENABLE_IPSEC"
     echo "OVN_METRICS_SCALE_ENABLE = $OVN_METRICS_SCALE_ENABLE"
     echo "OVN_COMPACT_MODE = $OVN_COMPACT_MODE"
     echo "OVN_ISOLATED = $OVN_ISOLATED"
     echo "OVN_MTU = $OVN_MTU"
     echo "ENABLE_ROUTE_ADVERTISEMENTS = $ENABLE_ROUTE_ADVERTISEMENTS"
     echo "ADVERTISE_DEFAULT_NETWORK = $ADVERTISE_DEFAULT_NETWORK"
     echo "ADVERTISED_UDN_ISOLATION_MODE = $ADVERTISED_UDN_ISOLATION_MODE"
     echo "KIND_CREATE = $KIND_CREATE"
     echo "KIND_ADD_NODES = $KIND_ADD_NODES"
     if [[ $OVN_ENABLE_INTERCONNECT == true ]]; then
       echo "KIND_NUM_NODES_PER_ZONE = $KIND_NUM_NODES_PER_ZONE"
       if [ "${KIND_NUM_NODES_PER_ZONE}" -gt 1 ] && [ "${OVN_ENABLE_OVNKUBE_IDENTITY}" = "true" ]; then
         echo "multi_node_zone is not compatible with ovnkube_identity, disabling ovnkube_identity"
         OVN_ENABLE_OVNKUBE_IDENTITY="false"
       fi
     fi
     echo ""
}

check_dependencies() {
    if ! command_exists kubectl ; then
      echo "'kubectl' not found, installing"
      setup_kubectl_bin
    fi

    for cmd in "$OCI_BIN" kind helm go ; do \
         if ! command_exists "$cmd" ; then
           echo "Dependency not met: $cmd"
           exit 1
        fi
    done
}

helm_prereqs() {
    # increase fs.inotify.max_user_watches if current value is less than desired
    CURRENT_WATCHES=$(sysctl -n fs.inotify.max_user_watches)
    if [ "$CURRENT_WATCHES" -lt 524288 ]; then
        echo "Increasing fs.inotify.max_user_watches from $CURRENT_WATCHES to 524288"
        sudo sysctl fs.inotify.max_user_watches=524288
    else
        echo "fs.inotify.max_user_watches is already $CURRENT_WATCHES (>= 524288)"
    fi

    # increase fs.inotify.max_user_instances if current value is less than desired
    CURRENT_INSTANCES=$(sysctl -n fs.inotify.max_user_instances)
    if [ "$CURRENT_INSTANCES" -lt 512 ]; then
        echo "Increasing fs.inotify.max_user_instances from $CURRENT_INSTANCES to 512"
        sudo sysctl fs.inotify.max_user_instances=512
    else
        echo "fs.inotify.max_user_instances is already $CURRENT_INSTANCES (>= 512)"
    fi
}

check_ipv6() {
  if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    # Collect additional IPv6 data on test environment
    ERROR_FOUND=false
    TMPVAR=$(sysctl net.ipv6.conf.all.forwarding | awk '{print $3}')
    echo "net.ipv6.conf.all.forwarding is equal to $TMPVAR"
    if [ "$TMPVAR" != 1 ]; then
      if [ "$KIND_ALLOW_SYSTEM_WRITES" == true ]; then
	sudo sysctl -w net.ipv6.conf.all.forwarding=1
      else
	echo "RUN: 'sudo sysctl -w net.ipv6.conf.all.forwarding=1' to use IPv6."
	ERROR_FOUND=true
      fi
    fi
    TMPVAR=$(sysctl net.ipv6.conf.all.disable_ipv6 | awk '{print $3}')
    echo "net.ipv6.conf.all.disable_ipv6 is equal to $TMPVAR"
    if [ "$TMPVAR" != 0 ]; then
      if [ "$KIND_ALLOW_SYSTEM_WRITES" == true ]; then
	sudo sysctl -w net.ipv6.conf.all.disable_ipv6=0
      else
	echo "RUN: 'sudo sysctl -w net.ipv6.conf.all.disable_ipv6=0' to use IPv6."
	ERROR_FOUND=true
      fi
    fi
    if [ -f /proc/net/if_inet6 ]; then
      echo "/proc/net/if_inet6 exists so IPv6 supported in kernel."
    else
      echo "/proc/net/if_inet6 does not exists so no IPv6 support found! Compile the kernel!!"
      ERROR_FOUND=true
    fi
    if "$ERROR_FOUND"; then
      exit 2
    fi
  fi
}

set_cluster_cidr_ip_families() {
  if [ "$PLATFORM_IPV4_SUPPORT" == true ] && [ "$PLATFORM_IPV6_SUPPORT" == false ]; then
    IP_FAMILY=""
    NET_CIDR=$NET_CIDR_IPV4
    SVC_CIDR=$SVC_CIDR_IPV4
    echo "IPv4 Only Support: --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  elif [ "$PLATFORM_IPV4_SUPPORT" == false ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    IP_FAMILY="ipv6"
    NET_CIDR=$NET_CIDR_IPV6
    SVC_CIDR=$SVC_CIDR_IPV6
    echo "IPv6 Only Support: --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  elif [ "$PLATFORM_IPV4_SUPPORT" == true ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    IP_FAMILY="dual"
    NET_CIDR="$NET_CIDR_IPV4,$NET_CIDR_IPV6"
    SVC_CIDR="$SVC_CIDR_IPV4,$SVC_CIDR_IPV6"
    # For Helm --set, we need to escape commas
    NET_CIDR_HELM="$NET_CIDR_IPV4\,$NET_CIDR_IPV6"
    SVC_CIDR_HELM="$SVC_CIDR_IPV4\,$SVC_CIDR_IPV6"
    echo "Dual Stack Support: --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  else
    echo "Invalid setup. PLATFORM_IPV4_SUPPORT and/or PLATFORM_IPV6_SUPPORT must be true."
    exit 1
  fi
}

create_local_registry() {
    # create registry container unless it already exists
    if [ "$($OCI_BIN inspect -f '{{.State.Running}}' "${KIND_LOCAL_REGISTRY_NAME}" 2>/dev/null || true)" != 'true' ]; then
      $OCI_BIN run \
        -d --restart=always -p "127.0.0.1:${KIND_LOCAL_REGISTRY_PORT}:5000" --name "${KIND_LOCAL_REGISTRY_NAME}" \
        registry:2
    fi
}

connect_local_registry() {
    # connect the registry to the cluster network if not already connected
    if [ "$($OCI_BIN inspect -f='{{json .NetworkSettings.Networks.kind}}' "${KIND_LOCAL_REGISTRY_NAME}")" = 'null' ]; then
      $OCI_BIN network connect "kind" "${KIND_LOCAL_REGISTRY_NAME}"
    fi

    # Reference docs for local registry:
    # - https://kind.sigs.k8s.io/docs/user/local-registry/
    # - https://github.com/kubernetes/enhancements/tree/master/keps/sig-cluster-lifecycle/generic/1755-communicating-a-local-registry
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${KIND_LOCAL_REGISTRY_PORT}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF

}

# run_script_in_container should be used when kind.sh is run nested in a container
# and makes sure the control-plane node is reachable by substituting 127.0.0.1
# with the control-plane container's IP
run_script_in_container() {
  if [ "$PLATFORM_IPV4_SUPPORT" == true ]; then
    local master_ip
    master_ip=$($OCI_BIN inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${KIND_CLUSTER_NAME}"-control-plane | head -n 1)
    sed -i -- "s/server: .*/server: https:\/\/$master_ip:6443/g" "$KUBECONFIG"
  else
    local master_ipv6
    master_ipv6=$($OCI_BIN inspect -f '{{range .NetworkSettings.Networks}}{{.GlobalIPv6Address}}{{end}}' "${KIND_CLUSTER_NAME}"-control-plane | head -n 1)
    sed -i -- "s/server: .*/server: https:\/\/[$master_ipv6]:6443/g" "$KUBECONFIG"
  fi
  chmod a+r "$KUBECONFIG"
}

# fixup_config_names should be used to ensure kind clusters are named based off
# provided values, essentially it removes the 'kind' prefix from the cluster names
fixup_kubeconfig_names() {
  sed -i -- "s/user: kind-.*/user: ${KIND_CLUSTER_NAME}/g" "$KUBECONFIG"
  sed -i -- "s/name: kind-.*/name: ${KIND_CLUSTER_NAME}/g" "$KUBECONFIG"
  sed -i -- "s/cluster: kind-.*/cluster: ${KIND_CLUSTER_NAME}/g" "$KUBECONFIG"
  sed -i -- "s/current-context: .*/current-context: ${KIND_CLUSTER_NAME}/g" "$KUBECONFIG"
}

docker_create_second_interface() {
  echo "adding second interfaces to nodes"

  # Create the network as dual stack, regardless of the type of the deployment. Ignore if already exists.
  "$OCI_BIN" network create --ipv6 --driver=bridge xgw --subnet=172.19.0.0/16 --subnet=fc00:f853:ccd:e798::/64 || true

  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}")
  for n in $KIND_NODES; do
    "$OCI_BIN" network connect xgw "$n"
  done
}

remove_default_route() {
  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}")
  for n in $KIND_NODES; do
    $OCI_BIN exec "$n" ip route delete default
  done
}

add_dns_hostnames() {
  dns="$'"
  KIND_NODES=$(kind get nodes --name "${KIND_CLUSTER_NAME}")
  # find all IPs and build dns entries
  for n in $KIND_NODES; do
	ip=$($OCI_BIN container inspect -f '{{ .NetworkSettings.Networks.kind.IPAddress }}' "$n")
        dns+="$ip $n \n"
        ip=$($OCI_BIN container inspect -f '{{ .NetworkSettings.Networks.kind.GlobalIPv6Address }}' "$n")
	dns+="$ip $n \n"
  done

  dns+="'"

  # update dns on each node
  for n in $KIND_NODES; do
	  $OCI_BIN exec "$n" bash -c "echo  $dns >> /etc/hosts"
  done
}

build_ovn_image() {
    if [ "${SKIP_OVN_IMAGE_REBUILD}" == "true" ]; then
      echo "Explicitly instructed not to rebuild ovn image: ${OVN_IMAGE}"
      return
    fi

    # Build ovn image
    pushd "${DIR}"/../go-controller
    make
    popd

    # Build ovn kube image
    pushd "${DIR}"/../dist/images
    # Find all built executables, but ignore the 'windows' directory if it exists
    find ../../go-controller/_output/go/bin/ -maxdepth 1 -type f -exec cp -f {} . \;
    echo "ref: $(git rev-parse  --symbolic-full-name HEAD)  commit: $(git rev-parse  HEAD)" > git_info
    $OCI_BIN build \
      --build-arg http_proxy="${http_proxy:-}" \
      --build-arg https_proxy="${https_proxy:-}" \
      --network=host \
      -t "${OVN_IMAGE}" \
      -f Dockerfile.fedora .
    popd
}

get_image() {
    local image_and_tag="${1:-$OVN_IMAGE}"  # Use $1 if provided, otherwise use $OVN_IMAGE
    local image="${image_and_tag%%:*}"  # Extract everything before the first colon
    echo "$image"
}

get_tag() {
    local image_and_tag="${1:-$OVN_IMAGE}"  # Use $1 if provided, otherwise use $OVN_IMAGE
    local tag="${image_and_tag##*:}"  # Extract everything after the last colon
    echo "$tag"
}

create_kind_cluster() {
  KIND_CONFIG=${KIND_CONFIG:-${DIR}/kind.yaml.gotmpl}
  KIND_CONFIG_LCL=${DIR}/kind-${KIND_CLUSTER_NAME}.yaml

  # Generate the KIND configuration using Go templating with CLI flags
  go run "${DIR}/generate-kind-config.go" \
    --net-cidr="${NET_CIDR}" \
    --svc-cidr="${SVC_CIDR}" \
    --ip-family="${IP_FAMILY}" \
    --use-local-registry="${KIND_LOCAL_REGISTRY}" \
    --dns-domain="${KIND_DNS_DOMAIN}" \
    --ovn-ha="${OVN_HA}" \
    --num-master="${KIND_NUM_MASTER}" \
    --num-worker="${KIND_NUM_WORKER}" \
    --cluster-log-level="${KIND_CLUSTER_LOGLEVEL:-4}" \
    --kind-local-registry-port="${KIND_LOCAL_REGISTRY_PORT}" \
    --kind-local-registry-name="${KIND_LOCAL_REGISTRY_NAME}" \
    --template="${KIND_CONFIG}" \
    --output="${KIND_CONFIG_LCL}"

  # Create KIND cluster. For additional debug, add '--verbosity <int>': 0 None .. 3 Debug
  if kind get clusters | grep "${KIND_CLUSTER_NAME}"; then
    delete
  fi

  if [[ "${KIND_LOCAL_REGISTRY}" == true ]]; then
    create_local_registry
  fi

  kind create cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" --image "${KIND_IMAGE}":"${K8S_VERSION}" --config="${KIND_CONFIG_LCL}" --retain
  kind load docker-image --name "$KIND_CLUSTER_NAME" "$OVN_IMAGE"

  # When using HA, label nodes to host db.
  if [ "$OVN_HA" == true ]; then
    kubectl label nodes k8s.ovn.org/ovnkube-db=true --overwrite \
            -l node-role.kubernetes.io/control-plane
    if [ "$KIND_NUM_WORKER" -ge 2 ]; then
      for n in ovn-worker ovn-worker2; do
          # We want OVN HA not Kubernetes HA
          # leverage the kubeadm well-known label node-role.kubernetes.io/control-plane=
          # to choose the nodes where ovn master components will be placed
          kubectl label node "$n" k8s.ovn.org/ovnkube-db=true node-role.kubernetes.io/control-plane="" --overwrite
      done
    fi
  fi

  # Remove taint, so control-plane nodes can also schedule regular pods
  if [ "$KIND_REMOVE_TAINT" == true ]; then
    kubectl taint node "$n" node-role.kubernetes.io/master:NoSchedule- \
            -l node-role.kubernetes.io/control-plane ||:
    kubectl taint node "$n" node-role.kubernetes.io/control-plane:NoSchedule- \
            -l node-role.kubernetes.io/control-plane ||:
  fi
}

label_ovn_single_node_zones() {
  KIND_NODES=$(kind_get_nodes)
  for n in $KIND_NODES; do
    kubectl label node "${n}" k8s.ovn.org/zone-name="${n}" --overwrite
  done
}

label_ovn_multiple_nodes_zones() {
  KIND_NODES=$(kind_get_nodes | sort)
  zone_idx=1
  n=1
  for node in $KIND_NODES; do
    zone="zone-${zone_idx}"
    kubectl label node "${node}" k8s.ovn.org/zone-name=${zone} --overwrite
    if [ "${n}" == "1" ]; then
      # Mark 1st node of each zone as zone control plane
      kubectl label node "${node}" node-role.kubernetes.io/zone-controller="" --overwrite
    fi

    if [ "${n}" == "${KIND_NUM_NODES_PER_ZONE}" ]; then
      n=1
      zone_idx=$((zone_idx+1))
    else
      n=$((n+1))
    fi
  done
}

create_ovn_kubernetes() {
    cd "${DIR}"/../helm/ovn-kubernetes
    MASTER_REPLICAS=$(kubectl get node -l node-role.kubernetes.io/control-plane --no-headers | wc -l)
    if [[ $KIND_NUM_NODES_PER_ZONE == 1 ]]; then
      label_ovn_single_node_zones
      value_file="values-single-node-zone.yaml"
      ovnkube_db_options=""
    elif [[ $KIND_NUM_NODES_PER_ZONE -gt 1 ]]; then
      label_ovn_multiple_nodes_zones
      value_file="values-multi-node-zone.yaml"
      ovnkube_db_options=""
    else
      value_file="values-no-ic.yaml"
      if [ "$OVN_HA" == true ]; then
        # Use raft version for HA
        ovnkube_db_options="--set tags.ovnkube-db-raft=true \
                            --set tags.ovnkube-db=false"
      else
        # Use standalone version for non-HA
        ovnkube_db_options="--set tags.ovnkube-db-raft=false \
                            --set tags.ovnkube-db=true"
      fi
    fi
    echo "value_file=${value_file}"
    cmd=$(cat <<EOF
helm install ovn-kubernetes . -f "${value_file}" \
          --set k8sAPIServer=${API_URL} \
          --set podNetwork="${NET_CIDR_HELM:-${NET_CIDR}}" \
          --set serviceNetwork="${SVC_CIDR_HELM:-${SVC_CIDR}}" \
          --set ovnkube-master.replicas=${MASTER_REPLICAS} \
          --set global.image.repository=$(get_image) \
          --set global.image.tag=$(get_tag) \
          --set global.enableAdminNetworkPolicy=true \
          --set global.enableMulticast=${OVN_MULTICAST_ENABLE} \
          --set global.enableMultiNetwork=${ENABLE_MULTI_NET} \
          --set global.enableNetworkSegmentation=${ENABLE_NETWORK_SEGMENTATION} \
          --set global.enablePreconfiguredUDNAddresses=${ENABLE_PRE_CONF_UDN_ADDR} \
          --set global.enableHybridOverlay=${OVN_HYBRID_OVERLAY_ENABLE} \
          --set global.enableObservability=${OVN_OBSERV_ENABLE} \
          --set global.emptyLbEvents=${OVN_EMPTY_LB_EVENTS} \
          --set global.enableDNSNameResolver=${OVN_ENABLE_DNSNAMERESOLVER} \
          --set global.enableNetworkQos=${OVN_NETWORK_QOS_ENABLE} \
          --set global.gatewayMode=${OVN_GATEWAY_MODE} \
          --set global.encapPort=${OVN_ENCAP_PORT} \
          --set global.disableSNATMultipleGWs=${OVN_DISABLE_SNAT_MULTIPLE_GWS} \
          --set global.disablePktMTUCheck=${OVN_DISABLE_PKT_MTU_CHECK} \
          --set global.disableForwarding=${OVN_DISABLE_FORWARDING} \
          --set global.mtu=${OVN_MTU} \
          --set global.enableIPsec=${ENABLE_IPSEC} \
          --set global.enableCompactMode=${OVN_COMPACT_MODE} \
          --set global.enableInterconnect=${OVN_ENABLE_INTERCONNECT} \
          --set global.enableRouteAdvertisements=${ENABLE_ROUTE_ADVERTISEMENTS} \
          --set global.advertiseDefaultNetwork=${ADVERTISE_DEFAULT_NETWORK} \
          --set global.advertisedUDNIsolationMode=${ADVERTISED_UDN_ISOLATION_MODE} \
          ${ovnkube_db_options}
EOF
       )
    echo "${cmd}"
    eval "${cmd}"
}

delete() {
  if [ "$KIND_INSTALL_METALLB" == true ]; then
    destroy_metallb
  fi
  helm uninstall ovn-kubernetes && sleep 5 ||:
  kind delete cluster --name "${KIND_CLUSTER_NAME:-ovn}"
}

install_online_ovn_kubernetes_crds() {
  # NOTE: When you update vendoring versions for the ANP & BANP APIs, we must update the version of the CRD we pull from in the below URL
  run_kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_adminnetworkpolicies.yaml
  run_kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_baselineadminnetworkpolicies.yaml
}

check_dependencies
parse_args "$@"
set_default_params
print_params
check_ipv6
set_cluster_cidr_ip_families
helm_prereqs
build_ovn_image
create_kind_cluster
detect_apiserver_url
echo "API_URL: $API_URL"
if [ "$RUN_IN_CONTAINER" == true ]; then
  run_script_in_container
fi
# if cluster name is specified fixup kubeconfig
if [ "$KIND_CLUSTER_NAME" != "ovn" ]; then
  fixup_kubeconfig_names
fi
if [[ "${KIND_LOCAL_REGISTRY}" == true ]]; then
  connect_local_registry
fi
docker_disable_ipv6
if [ "$OVN_ENABLE_EX_GW_NETWORK_BRIDGE" == true ]; then
  docker_create_second_interface
fi
coredns_patch
if [ "$OVN_ISOLATED" == true ]; then
  remove_default_route
  add_dns_hostnames
fi
if [ "$OVN_ENABLE_DNSNAMERESOLVER" == true ]; then
    build_dnsnameresolver_images
    install_dnsnameresolver_images
    install_dnsnameresolver_operator
    update_clusterrole_coredns
    add_ocp_dnsnameresolver_to_coredns_config
    update_coredns_deployment_image
fi
if [ "$ENABLE_ROUTE_ADVERTISEMENTS" == true ]; then
  deploy_frr_external_container
  deploy_bgp_external_server
fi
create_ovn_kubernetes

install_online_ovn_kubernetes_crds
if [ "$KIND_INSTALL_INGRESS" == true ]; then
  install_ingress
fi

if [ "$ENABLE_MULTI_NET" == true ]; then
  enable_multi_net
fi

# if ! kubectl wait -n ovn-kubernetes --for=condition=ready pods --all --timeout=300s ; then
#  echo "some pods in the system are not running"
#  kubectl get pods -A -o wide || true
#  kubectl describe po -A
#  exit 1
# fi

kubectl_wait_pods

if [ "$OVN_ENABLE_DNSNAMERESOLVER" == true ]; then
    kubectl_wait_dnsnameresolver_pods
fi
sleep_until_pods_settle

if [ "$KIND_INSTALL_METALLB" == true ]; then
  install_metallb
fi
if [ "$KIND_INSTALL_PLUGINS" == true ]; then
  install_plugins
fi
if [ "$KIND_INSTALL_KUBEVIRT" == true ]; then
  install_kubevirt
fi

if [ "$ENABLE_IPSEC" == true ]; then
  install_ipsec
fi

interconnect_arg_check
