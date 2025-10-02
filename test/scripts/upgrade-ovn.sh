#!/usr/bin/env bash

# always exit on errors
set -ex

DIR="$( cd -- "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
export DIR

source ${DIR}/../../contrib/kind-common

export KUBECONFIG=${KUBECONFIG:-${HOME}/ovn.conf}
export OVN_IMAGE=${OVN_IMAGE:-ovn-daemonset-fedora:pr}

set -ex
ARCH=""
case $(uname -m) in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64"   ;;
esac

install_ovn_image() {
  kind load docker-image "${OVN_IMAGE}" --name "${KIND_CLUSTER_NAME}"
}

upgrade_ovn_kubernetes_helm() {
  # Extract image repository and tag from OVN_IMAGE
  IMAGE_REPO=$(echo $OVN_IMAGE | cut -d: -f1)
  IMAGE_TAG=$(echo $OVN_IMAGE | cut -d: -f2)
  
  # Determine which values file to use based on interconnect mode
  if [ "${OVN_ENABLE_INTERCONNECT}" == true ]; then
    VALUES_FILE="values-single-node-zone.yaml"
  else
    VALUES_FILE="values-no-ic.yaml"
  fi
  
  echo "Upgrading OVN-Kubernetes using Helm with values file: $VALUES_FILE"
  
  # Perform the Helm upgrade
  helm upgrade ovn-kubernetes ../helm/ovn-kubernetes \
    -f ../helm/ovn-kubernetes/${VALUES_FILE} \
    --set k8sAPIServer="${API_URL}" \
    --set podNetwork="${NET_CIDR}" \
    --set serviceNetwork="${SVC_CIDR}" \
    --set global.image.repository="${IMAGE_REPO}" \
    --set global.image.tag="${IMAGE_TAG}" \
    --set global.gatewayMode="${OVN_GATEWAY_MODE}" \
    --set global.enableInterconnect="${OVN_ENABLE_INTERCONNECT}" \
    --set global.enableHybridOverlay="${OVN_HYBRID_OVERLAY_ENABLE}" \
    --set global.disableSnatMultipleGws="${OVN_DISABLE_SNAT_MULTIPLE_GWS}" \
    --set global.disableForwarding="${OVN_DISABLE_FORWARDING}" \
    --set global.disablePktMTUCheck="${OVN_DISABLE_PKT_MTU_CHECK}" \
    --set global.emptyLbEvents="${OVN_EMPTY_LB_EVENTS}" \
    --set global.enableMulticast="${OVN_MULTICAST_ENABLE}" \
    --set global.enableEgressIp=true \
    --set global.enableEgressFirewall=true \
    --set global.enableEgressQos=true \
    --set global.enableAdminNetworkPolicy=true \
    --set global.mtu="${OVN_MTU:-1400}" \
    --set global.encapPort="${OVN_ENCAP_PORT:-6081}" \
    --set global.v4JoinSubnet="${JOIN_SUBNET_IPV4}" \
    --set global.v6JoinSubnet="${JOIN_SUBNET_IPV6}" \
    --set global.extGatewayNetworkInterface="${OVN_EX_GW_NETWORK_INTERFACE}" \
    --set global.masterLogLevel="${MASTER_LOG_LEVEL}" \
    --set global.nodeLogLevel="${NODE_LOG_LEVEL}" \
    --set global.dbcheckerLogLevel="${DBCHECKER_LOG_LEVEL}" \
    --set global.ovnLogLevelNorthd="${OVN_LOG_LEVEL_NORTHD}" \
    --set global.ovnLogLevelNb="${OVN_LOG_LEVEL_NB}" \
    --set global.ovnLogLevelSb="${OVN_LOG_LEVEL_SB}" \
    --set global.ovnLogLevelController="${OVN_LOG_LEVEL_CONTROLLER}" \
    --wait \
    --timeout=900s
}

set_default_ovn_manifest_params() {
  # Set default values
  # kind configs
  PLATFORM_IPV4_SUPPORT=${PLATFORM_IPV4_SUPPORT:-true}
  PLATFORM_IPV6_SUPPORT=${PLATFORM_IPV6_SUPPORT:-false}
  OVN_HA=${OVN_HA:-false}
  OVN_ENABLE_OVNKUBE_IDENTITY=${OVN_ENABLE_OVNKUBE_IDENTITY:-true}
  # ovn configs 
  OVN_GATEWAY_MODE=${OVN_GATEWAY_MODE:-shared}
  OVN_ENABLE_INTERCONNECT=${OVN_ENABLE_INTERCONNECT:-true}
  OVN_HYBRID_OVERLAY_ENABLE=${OVN_HYBRID_OVERLAY_ENABLE:-false}
  OVN_DISABLE_SNAT_MULTIPLE_GWS=${OVN_DISABLE_SNAT_MULTIPLE_GWS:-false}
  OVN_DISABLE_FORWARDING=${OVN_DISABLE_FORWARDING:-false}
  OVN_DISABLE_PKT_MTU_CHECK=${OVN_DISABLE_PKT_MTU_CHECK:-false}
  OVN_EMPTY_LB_EVENTS=${OVN_EMPTY_LB_EVENTS:-false}
  OVN_MULTICAST_ENABLE=${OVN_MULTICAST_ENABLE:-false}
  OVN_IMAGE=${OVN_IMAGE:-local}
  MASTER_LOG_LEVEL=${MASTER_LOG_LEVEL:-5}
  NODE_LOG_LEVEL=${NODE_LOG_LEVEL:-5}
  DBCHECKER_LOG_LEVEL=${DBCHECKER_LOG_LEVEL:-5}
  OVN_LOG_LEVEL_NORTHD=${OVN_LOG_LEVEL_NORTHD:-"-vconsole:info -vfile:info"}
  OVN_LOG_LEVEL_NB=${OVN_LOG_LEVEL_NB:-"-vconsole:info -vfile:info"}
  OVN_LOG_LEVEL_SB=${OVN_LOG_LEVEL_SB:-"-vconsole:info -vfile:info"}
  OVN_LOG_LEVEL_CONTROLLER=${OVN_LOG_LEVEL_CONTROLLER:-"-vconsole:info"}
  OVN_ENABLE_EX_GW_NETWORK_BRIDGE=${OVN_ENABLE_EX_GW_NETWORK_BRIDGE:-false}
  OVN_EX_GW_NETWORK_INTERFACE=""
  if [ "$OVN_ENABLE_EX_GW_NETWORK_BRIDGE" == true ]; then
    OVN_EX_GW_NETWORK_INTERFACE="eth1"
  fi
  # Additional parameters for Helm upgrade
  OVN_MTU=${OVN_MTU:-1400}
  OVN_ENCAP_PORT=${OVN_ENCAP_PORT:-6081}
  API_URL=${API_URL:-https://172.18.0.4:6443}
  # Input not currently validated. Modify outside script at your own risk.
  # These are the same values defaulted to in KIND code (kind/default.go).
  # NOTE: KIND NET_CIDR_IPV6 default use a /64 but OVN have a /64 per host
  # so it needs to use a larger subnet
  #  Upstream - NET_CIDR_IPV6=fd00:10:244::/64 SVC_CIDR_IPV6=fd00:10:96::/112
  NET_CIDR_IPV4=${NET_CIDR_IPV4:-10.244.0.0/16}
  NET_SECOND_CIDR_IPV4=${NET_SECOND_CIDR_IPV4:-172.19.0.0/16}
  SVC_CIDR_IPV4=${SVC_CIDR_IPV4:-10.96.0.0/16}
  NET_CIDR_IPV6=${NET_CIDR_IPV6:-fd00:10:244::/48}
  SVC_CIDR_IPV6=${SVC_CIDR_IPV6:-fd00:10:96::/112}
  JOIN_SUBNET_IPV4=${JOIN_SUBNET_IPV4:-100.64.0.0/16}
  JOIN_SUBNET_IPV6=${JOIN_SUBNET_IPV6:-fd98::/64}
  KIND_NUM_MASTER=1
  if [ "$OVN_HA" == true ]; then
    KIND_NUM_MASTER=3
    KIND_NUM_WORKER=${KIND_NUM_WORKER:-0}
  else
    KIND_NUM_WORKER=${KIND_NUM_WORKER:-2}
  fi
  OVN_HOST_NETWORK_NAMESPACE=${OVN_HOST_NETWORK_NAMESPACE:-ovn-host-network}
  OCI_BIN=${KIND_EXPERIMENTAL_PROVIDER:-docker}
}

print_ovn_manifest_params() {
     echo "Using these parameters to build upgraded ovn-k manifests"
     echo ""
     echo "PLATFORM_IPV4_SUPPORT = $PLATFORM_IPV4_SUPPORT"
     echo "PLATFORM_IPV6_SUPPORT = $PLATFORM_IPV6_SUPPORT"
     echo "OVN_HA = $OVN_HA"
     echo "OVN_GATEWAY_MODE = $OVN_GATEWAY_MODE"
     echo "OVN_ENABLE_INTERCONNECT = $OVN_ENABLE_INTERCONNECT"
     echo "OVN_HYBRID_OVERLAY_ENABLE = $OVN_HYBRID_OVERLAY_ENABLE"
     echo "OVN_DISABLE_SNAT_MULTIPLE_GWS = $OVN_DISABLE_SNAT_MULTIPLE_GWS"
     echo "OVN_DISABLE_FORWARDING = $OVN_DISABLE_FORWARDING"
     echo "OVN_DISABLE_PKT_MTU_CHECK = $OVN_DISABLE_PKT_MTU_CHECK"
     echo "OVN_NETFLOW_TARGETS = $OVN_NETFLOW_TARGETS"
     echo "OVN_SFLOW_TARGETS = $OVN_SFLOW_TARGETS"
     echo "OVN_IPFIX_TARGETS = $OVN_IPFIX_TARGETS"
     echo "OVN_EMPTY_LB_EVENTS = $OVN_EMPTY_LB_EVENTS"
     echo "OVN_MULTICAST_ENABLE = $OVN_MULTICAST_ENABLE"
     echo "OVN_IMAGE = $OVN_IMAGE"
     echo "MASTER_LOG_LEVEL = $MASTER_LOG_LEVEL"
     echo "NODE_LOG_LEVEL = $NODE_LOG_LEVEL"
     echo "DBCHECKER_LOG_LEVEL = $DBCHECKER_LOG_LEVEL"
     echo "OVN_LOG_LEVEL_NORTHD = $OVN_LOG_LEVEL_NORTHD"
     echo "OVN_LOG_LEVEL_NB = $OVN_LOG_LEVEL_NB"
     echo "OVN_LOG_LEVEL_SB = $OVN_LOG_LEVEL_SB"
     echo "OVN_LOG_LEVEL_CONTROLLER = $OVN_LOG_LEVEL_CONTROLLER"
     echo "OVN_HOST_NETWORK_NAMESPACE = $OVN_HOST_NETWORK_NAMESPACE"
     echo "OVN_ENABLE_EX_GW_NETWORK_BRIDGE = $OVN_ENABLE_EX_GW_NETWORK_BRIDGE"
     echo "OVN_EX_GW_NETWORK_INTERFACE = $OVN_EX_GW_NETWORK_INTERFACE"
     echo "OVN_ENABLE_OVNKUBE_IDENTITY =  $OVN_ENABLE_OVNKUBE_IDENTITY"
     echo "OVN_MTU = $OVN_MTU"
     echo "OVN_ENCAP_PORT = $OVN_ENCAP_PORT"
     echo "API_URL = $API_URL"
     echo ""
}

set_cluster_cidr_ip_families() {
  if [ "$PLATFORM_IPV4_SUPPORT" == true ] && [ "$PLATFORM_IPV6_SUPPORT" == false ]; then
    IP_FAMILY=""
    NET_CIDR=$NET_CIDR_IPV4
    SVC_CIDR=$SVC_CIDR_IPV4
    echo "IPv4 Only Support: API_IP=$API_IP --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  elif [ "$PLATFORM_IPV4_SUPPORT" == false ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    IP_FAMILY="ipv6"
    NET_CIDR=$NET_CIDR_IPV6
    SVC_CIDR=$SVC_CIDR_IPV6
    echo "IPv6 Only Support: API_IP=$API_IP --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  elif [ "$PLATFORM_IPV4_SUPPORT" == true ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    IP_FAMILY="dual"
    NET_CIDR=$NET_CIDR_IPV4,$NET_CIDR_IPV6
    SVC_CIDR=$SVC_CIDR_IPV4,$SVC_CIDR_IPV6
    echo "Dual Stack Support: API_IP=$API_IP --net-cidr=$NET_CIDR --svc-cidr=$SVC_CIDR"
  else
    echo "Invalid setup. PLATFORM_IPV4_SUPPORT and/or PLATFORM_IPV6_SUPPORT must be true."
    exit 1
  fi
}

# This script is responsible for upgrading the ovn-kubernetes related resources 
# within a running cluster built from master, to new resources built from the 
# checked-out branch.

KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-ovn} # Set default values

# setup env needed for regenerating ovn resources from the checked-out branch
# this will use the new OVN image as well: ovn-daemonset-fedora:pr
set_default_ovn_manifest_params
print_ovn_manifest_params

# Set up cluster CIDR and API URL
set_cluster_cidr_ip_families

# Detect API server URL
detect_apiserver_url

# Load the new OVN image into the cluster
install_ovn_image

# Perform Helm upgrade
upgrade_ovn_kubernetes_helm

# Wait for all pods to be ready
kubectl_wait_pods

# Verify the upgrade was successful
echo "Upgrade completed successfully!"
run_kubectl get all -n ovn-kubernetes

KIND_REMOVE_TAINT=${KIND_REMOVE_TAINT:-true}
MASTER_NODES=$(kubectl get nodes -l node-role.kubernetes.io/control-plane -o name)
# We want OVN HA not Kubernetes HA
# leverage the kubeadm well-known label node-role.kubernetes.io/control-plane=
# to choose the nodes where ovn master components will be placed
for node in $MASTER_NODES; do
  if [ "$KIND_REMOVE_TAINT" == true ]; then
    # do not error if it fails to remove the taint
    kubectl taint node "$node" node-role.kubernetes.io/control-plane:NoSchedule- || true
  fi
done

# redownload the e2e test binaries if their version differs
K8S_VERSION="v1.33.1"
E2E_VERSION=$(/usr/local/bin/e2e.test --version)
if [[ "$E2E_VERSION" != "$K8S_VERSION" ]]; then
   echo "found version $E2E_VERSION of e2e binary, need version $K8S_VERSION ; will download it."
   # Install e2e test binary and ginkgo
   curl -LO https://dl.k8s.io/${K8S_VERSION}/kubernetes-test-linux-${ARCH}.tar.gz
   tar xvzf kubernetes-test-linux-${ARCH}.tar.gz
   sudo mv kubernetes/test/bin/e2e.test /usr/local/bin/e2e.test
   sudo mv kubernetes/test/bin/ginkgo /usr/local/bin/ginkgo
   rm kubernetes-test-linux-${ARCH}.tar.gz
fi
