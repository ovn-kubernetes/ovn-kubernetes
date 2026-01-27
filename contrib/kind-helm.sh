#!/usr/bin/env bash

set -eo pipefail

# Returns the full directory name of the script
export DIR="$( cd -- "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"

# Source the kind-common.sh file from the same directory where this script is located
source "${DIR}/kind-common.sh"

set_default_params() {
  set_common_default_params

  # Set default values
  export KIND_CONFIG=${KIND_CONFIG:-}
  # Validated params that work

  # Hard code ipv4 support until IPv6 is implemented
  if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    echo "kind-helm.sh does not support IPv6 yet"
    exit 1
  fi
  if [ "$PLATFORM_IPV4_SUPPORT" != true ]; then
    echo "kind-helm.sh only supports IPv4, must set PLATFORM_IPV4_SUPPORT to true "
    exit 1
  fi
}

usage() {
    echo "usage: kind-helm.sh [--delete]"
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
    echo "       [ -nce | --network-connect-enable ]"
    echo "       [ -uae | --preconfigured-udn-addresses-enable ]"
    echo "       [ -rae | --route-advertisements-enable ]"
    echo "       [ -evpn | --evpn-enable ]"
    echo "       [-dudn | --dynamic-udn-allocation]"
    echo "       [-dug | --dynamic-udn-removal-grace-period]"
    echo "       [-adv | --advertise-default-network]"
    echo "       [-rud | --routed-udn-isolation-disable]"
    echo "       [ -nqe | --network-qos-enable ]"
    echo "       [ -wk  | --num-workers <num> ]"
    echo "       [ -ic  | --enable-interconnect]"
    echo "       [ -npz | --node-per-zone ]"
    echo "       [ -ov  | --ovn-image <image> ]"
    echo "       [ -ovr | --ovn-repo <repo> ]"
    echo "       [ -ovg | --ovn-gitref <ref> ]"
    echo "       [ -cn  | --cluster-name ]"
    echo "       [ -mip | --metrics-ip <ip> ]"
    echo "       [ --enable-coredumps ]"
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
    echo "-nce | --network-connect-enable               Enable network connect (requires network segmentation). DEFAULT: Disabled"
    echo "-uae | --preconfigured-udn-addresses-enable   Enable connecting workloads with preconfigured network to user-defined networks. DEFAULT: Disabled"
    echo "-rae | --route-advertisements-enable          Enable route advertisements"
    echo "-evpn | --evpn-enable                         Enable EVPN"
    echo "-dudn | --dynamic-udn-allocation              Enable dynamic UDN allocation. DEFAULT: Disabled"
    echo "-dug | --dynamic-udn-removal-grace-period     Configure the grace period in seconds for dynamic UDN removal. DEFAULT: 120 seconds"
    echo "-adv | --advertise-default-network            Applies a RouteAdvertisements configuration to advertise the default network on all nodes"
    echo "-rud | --routed-udn-isolation-disable         Disable isolation across BGP-advertised UDNs (sets advertised-udn-isolation-mode=loose). DEFAULT: strict."
    echo "-nqe | --network-qos-enable                   Enable network QoS. DEFAULT: Disabled"
    echo "-ha  | --ha-enabled                           Enable high availability. DEFAULT: HA Disabled"
    echo "-wk  | --num-workers                          Number of worker nodes. DEFAULT: 2 workers"
    echo "-ov  | --ovn-image                            Use the specified docker image instead of building locally. DEFAULT: local build."
    echo "-ovr | --ovn-repo                             Specify the repository to build OVN from"
    echo "-ovg | --ovn-gitref                           Specify the branch, tag or commit id to build OVN from, it can be a pattern like 'branch-*' it will order results and use the first one"
    echo "-cn  | --cluster-name                         Configure the kind cluster's name"
    echo "-mip | --metrics-ip                           IP address to bind metrics endpoints. DEFAULT: K8S_NODE_IP or 0.0.0.0"
    echo "--enable-coredumps                            Enable coredump collection on kind nodes. DEFAULT: Disabled"
    echo "-dns | --enable-dnsnameresolver               Enable DNSNameResolver for resolving the DNS names used in the DNS rules of EgressFirewall."
    echo "-ce  | --enable-central                       [DEPRECATED] Deploy with OVN Central (Legacy Architecture)"
    echo "-npz | --nodes-per-zone                       Specify number of nodes per zone (Default 0, which means global zone; >0 means interconnect zone, where 1 for single-node zone, >1 for multi-node zone). If this value > 1, then (total k8s nodes (workers + 1) / num of nodes per zone) should be zero."
    echo "-mps | --multi-pod-subnet                     Use multiple subnets for the default cluster network"
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
            -nce | --network-connect-enable )     ENABLE_NETWORK_CONNECT=true
                                                  ;;
            -uae | --preconfigured-udn-addresses-enable)    ENABLE_PRE_CONF_UDN_ADDR=true
                                                  ;;
            -rae | --route-advertisements-enable) ENABLE_ROUTE_ADVERTISEMENTS=true
                                                  ;;
            -evpn | --evpn-enable)                ENABLE_EVPN=true
                                                  ;;
            -adv | --advertise-default-network)   ADVERTISE_DEFAULT_NETWORK=true
                                                  ;;
            -rud | --routed-udn-isolation-disable) ADVERTISED_UDN_ISOLATION_MODE=loose
                                                  ;;
            -dudn | --dynamic-udn-allocation)     DYNAMIC_UDN_ALLOCATION=true
                                                  ;;
            -dug  | --dynamic-udn-removal-grace-period) shift
                                                  if [[ -z "${1:-}" || "${1:-}" == -* ]]; then
                                                    echo "Missing value for --dynamic-udn-removal-grace-period" >&2
                                                    usage
                                                    exit 1
                                                  fi
                                                  DYNAMIC_UDN_GRACE_PERIOD=$1
                                                  if [[ "$DYNAMIC_UDN_GRACE_PERIOD" =~ ^[0-9]+$ ]]; then
                                                    DYNAMIC_UDN_GRACE_PERIOD="${DYNAMIC_UDN_GRACE_PERIOD}s"
                                                  fi
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
            -ov | --ovn-image )                   shift
                                                  OVN_IMAGE=$1
                                                  ;;
            -ovr | --ovn-repo )                   shift
                                                  OVN_REPO=$1
                                                  ;;
            -ovg | --ovn-gitref )                 shift
                                                  OVN_GITREF=$1
                                                  ;;
            -cn | --cluster-name )                shift
                                                  KIND_CLUSTER_NAME=$1
                                                  # Setup KUBECONFIG
                                                  set_default_params
                                                  ;;
            -dns | --enable-dnsnameresolver )     OVN_ENABLE_DNSNAMERESOLVER=true
                                                  ;;
            -ce | --enable-central )              echo "WARNING: --enable-central is deprecated. OVN Central (Legacy Architecture) will be removed in a future release." >&2
                                                  OVN_ENABLE_INTERCONNECT=false
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
            -mps| --multi-pod-subnet )            MULTI_POD_SUBNET=true
                                                  ;;
            -mip | --metrics-ip ) shift
                                                  METRICS_IP="$1"
                                                  ;;
            --enable-coredumps )                  ENABLE_COREDUMPS=true
                                                  ;;
            * )                                   usage
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
     echo "KIND_CONFIG_FILE = $KIND_CONFIG"
     echo "KUBECONFIG = $KUBECONFIG"
     echo "OCI_BIN = $OCI_BIN"
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
     echo "ENABLE_NETWORK_CONNECT = $ENABLE_NETWORK_CONNECT"
     echo "ENABLE_PRE_CONF_UDN_ADDR = $ENABLE_PRE_CONF_UDN_ADDR"
     echo "ENABLE_ROUTE_ADVERTISEMENTS = $ENABLE_ROUTE_ADVERTISEMENTS"
     echo "ENABLE_EVPN = $ENABLE_EVPN"
     echo "ADVERTISE_DEFAULT_NETWORK = $ADVERTISE_DEFAULT_NETWORK"
     echo "ADVERTISED_UDN_ISOLATION_MODE = $ADVERTISED_UDN_ISOLATION_MODE"
     echo "OVN_NETWORK_QOS_ENABLE = $OVN_NETWORK_QOS_ENABLE"
     echo "OVN_IMAGE = $OVN_IMAGE"
     echo "OVN_REPO = $OVN_REPO"
     echo "OVN_GITREF = $OVN_GITREF"
     echo "KIND_NUM_MASTER = $KIND_NUM_MASTER"
     echo "KIND_NUM_WORKER = $KIND_NUM_WORKER"
     echo "OVN_ENABLE_DNSNAMERESOLVER= $OVN_ENABLE_DNSNAMERESOLVER"
     echo "MULTI_POD_SUBNET= $MULTI_POD_SUBNET"
     echo "OVN_ENABLE_INTERCONNECT = $OVN_ENABLE_INTERCONNECT"
     echo "DYNAMIC_UDN_ALLOCATION = $DYNAMIC_UDN_ALLOCATION"
     echo "DYNAMIC_UDN_GRACE_PERIOD =  $DYNAMIC_UDN_GRACE_PERIOD"
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

    # check for currently unsupported features
    if [ "${PLATFORM_IPV6_SUPPORT:-}" = "true" ]; then
        echo "Fatal: PLATFORM_IPV6_SUPPORT support not implemented yet"
        exit 1
    fi
}

helm_prereqs() {
    # increate fs.inotify.max_user_watches
    sudo sysctl fs.inotify.max_user_watches=524288
    # increase fs.inotify.max_user_instances
    sudo sysctl fs.inotify.max_user_instances=512
}

create_kind_cluster() {
  [ -n "${KIND_CONFIG}" ] || {
    KIND_CONFIG='/tmp/kind.yaml'

    # Start of the kind configuration
    cat <<EOT > /tmp/kind.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: InitConfiguration
    nodeRegistration:
      kubeletExtraArgs:
        node-labels: "ingress-ready=true"
        authorization-mode: "AlwaysAllow"
EOT
  }

    # Add control-plane nodes based on OVN_HA status. If there are 2 or more worker nodes, use
    # 2 of them them to host databases instead of creating additional control plane nodes.
    if [ "$OVN_HA" == true ] && [ "$KIND_NUM_WORKER" -lt 2 ]; then
        for i in {2..3}; do  # Have 3 control-plane nodes for HA
            echo "- role: control-plane" >> /tmp/kind.yaml
        done
    fi

    # Add worker nodes based on KIND_NUM_WORKER
    for i in $(seq 1 $KIND_NUM_WORKER); do
        echo "- role: worker" >> /tmp/kind.yaml
    done
    # kind only allows single subnet for pod network, while ovn-kubernetes supports multiple subnets.
    # So we pick the first subnet from the provided list for kind configuration and store it in KIND_CIDR.
    # remove host subnet mask info for kind configuration (when the subnet is set as 10.0.0.0/16/14)
    KIND_CIDR_IPV4=$(echo "${NET_CIDR_IPV4}"| cut -d',' -f1 | cut -d'/' -f1,2 )

    # Add networking configuration
    cat <<EOT >> /tmp/kind.yaml
networking:
  disableDefaultCNI: true
  kubeProxyMode: none
  podSubnet: $KIND_CIDR_IPV4
  serviceSubnet: $SVC_CIDR_IPV4
EOT

    kind delete clusters $KIND_CLUSTER_NAME ||:
    kind create cluster --name $KIND_CLUSTER_NAME --image "${KIND_IMAGE}":"${K8S_VERSION}" --config "${KIND_CONFIG}" --retain
    kind load docker-image --name $KIND_CLUSTER_NAME $OVN_IMAGE

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
    kubectl label node "${n}" k8s.ovn.org/zone-name=${n} --overwrite
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
    cd ${DIR}/../helm/ovn-kubernetes
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
      ovnkube_db_options="--set tags.ovnkube-db-raft=$(if [ "${OVN_HA}" == "true" ]; then echo "true"; else echo "false"; fi) \
                          --set tags.ovnkube-db=$(if [ "${OVN_HA}" == "false" ]; then echo "true"; else echo "false"; fi)"
    fi
    echo "value_file=${value_file}"
    # For multi-pod-subnet case, NET_CIDR_IPV4 is a list of CIDRs separated by comma.
    # When Helm encounters a comma within a string value in a --set argument, it attempts to parse the comma as a separator
    # for multiple values (like a list or a map), not as part of a single string value.
    set -x
    ESCAPED_NET_CIDR_IPV4="${NET_CIDR_IPV4//,/\\,}"
    cmd=$(cat <<EOF
helm install ovn-kubernetes . -f "${value_file}" \
          --set k8sAPIServer=${API_URL} \
          --set podNetwork="${ESCAPED_NET_CIDR_IPV4}" \
          --set serviceNetwork=${SVC_CIDR_IPV4} \
          --set ovnkube-master.replicas=${MASTER_REPLICAS} \
          --set global.image.repository=${OVN_IMAGE%%:*} \
          --set global.image.tag=${OVN_IMAGE##*:} \
          --set global.enableAdminNetworkPolicy=true \
          --set global.enableMulticast=$(if [ "${OVN_MULTICAST_ENABLE}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableMultiNetwork=$(if [ "${ENABLE_MULTI_NET}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableNetworkSegmentation=$(if [ "${ENABLE_NETWORK_SEGMENTATION}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableNetworkConnect=$(if [ "${ENABLE_NETWORK_CONNECT}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableDynamicUDNAllocation=$(if [ "${DYNAMIC_UDN_ALLOCATION}" == "true" ]; then echo "true"; else echo "false"; fi) \
          $( [ -n "$DYNAMIC_UDN_GRACE_PERIOD" ] && echo "--set global.dynamicUDNGracePeriod=$DYNAMIC_UDN_GRACE_PERIOD" ) \
          --set global.enablePreconfiguredUDNAddresses=$(if [ "${ENABLE_PRE_CONF_UDN_ADDR}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableRouteAdvertisements=$(if [ "${ENABLE_ROUTE_ADVERTISEMENTS}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableEVPN=$(if [ "${ENABLE_EVPN}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.advertiseDefaultNetwork=$(if [ "${ADVERTISE_DEFAULT_NETWORK}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.advertisedUDNIsolationMode="${ADVERTISED_UDN_ISOLATION_MODE}" \
          --set global.enableHybridOverlay=$(if [ "${OVN_HYBRID_OVERLAY_ENABLE}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableObservability=$(if [ "${OVN_OBSERV_ENABLE}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.emptyLbEvents=$(if [ "${OVN_EMPTY_LB_EVENTS}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableDNSNameResolver=$(if [ "${OVN_ENABLE_DNSNAMERESOLVER}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableNetworkQos=$(if [ "${OVN_NETWORK_QOS_ENABLE}" == "true" ]; then echo "true"; else echo "false"; fi) \
          --set global.enableCoredumps=$(if [ "${ENABLE_COREDUMPS}" == "true" ]; then echo "true"; else echo "false"; fi) \
          ${ovnkube_db_options}
EOF
       )
    echo "${cmd}"
    eval "${cmd}"
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
helm_prereqs
build_ovn_image
create_kind_cluster
if [ "$ENABLE_COREDUMPS" == true ]; then
  setup_coredumps
fi
detect_apiserver_url
docker_disable_ipv6
coredns_patch
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

if [ "$ENABLE_ROUTE_ADVERTISEMENTS" == true ]; then
  install_frr_k8s
fi

interconnect_arg_check
