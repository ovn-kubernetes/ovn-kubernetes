if [ "${BASH_SOURCE[0]}" -ef "$0" ]
then
    >&2 echo 'This file contains bash helper functions that are common to'
    >&2 echo 'kind.sh and kind-helm.sh scripts and is meant to be sourced'
    >&2 echo 'by them upon invocation. In other words, it is not useful'
    >&2 echo 'when executed as a standalone script.'
    >&2 echo 'Please source this script, do not execute it!'
    exit 1
fi

ARCH=""
case $(uname -m) in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64"   ;;
esac

if_error_exit() {
    ###########################################################################
    # Description:                                                            #
    # Validate if previous command failed and show an error msg (if provided) #
    #                                                                         #
    # Arguments:                                                              #
    #   $1 - error message if not provided, it will just exit                 #
    ###########################################################################
    if [ "$?" != "0" ]; then
        if [ -n "$1" ]; then
            RED="\e[31m"
            ENDCOLOR="\e[0m"
            echo -e "[ ${RED}FAILED${ENDCOLOR} ] ${1}"
        fi
        exit 1
    fi
}

set_common_default_params() {
  KIND_IMAGE=${KIND_IMAGE:-kindest/node}
  K8S_VERSION=${K8S_VERSION:-v1.34.0}
  KIND_SETTLE_DURATION=${KIND_SETTLE_DURATION:-30}

  BGP_SERVER_NET_SUBNET_IPV4=${BGP_SERVER_NET_SUBNET_IPV4:-172.26.0.0/16}
  BGP_SERVER_NET_SUBNET_IPV6=${BGP_SERVER_NET_SUBNET_IPV6:-fc00:f853:ccd:e796::/64}
}

set_ovn_image() {
  if [ "${KIND_LOCAL_REGISTRY:-false}" == true ]; then
    OVN_IMAGE="localhost:5000/ovn-daemonset-fedora:latest"
  else
    OVN_IMAGE="localhost/ovn-daemonset-fedora:dev"
  fi
}

build_ovn_image() {
  local push_args=""
  if [ "$OCI_BIN" == "podman" ]; then
    # docker doesn't perform tls check by default only podman does, hence we need to disable it for podman.
    push_args="--tls-verify=false"
  fi

  if [ "$OVN_IMAGE" == local ]; then
    set_ovn_image

    # Build image
    make -C ${DIR}/../dist/images IMAGE="${OVN_IMAGE}" OVN_REPO="${OVN_REPO}" OVN_GITREF="${OVN_GITREF}" OCI_BIN="${OCI_BIN}" fedora-image

    # store in local registry
    if [ "$KIND_LOCAL_REGISTRY" == true ];then
      echo "Pushing built image to local $OCI_BIN registry"
      $OCI_BIN push $push_args "$OVN_IMAGE"
    fi
  # We should push to local registry if image is not remote
  elif [[ -n "${OVN_IMAGE}" && "${KIND_LOCAL_REGISTRY}" == true && "${OVN_IMAGE}" != */* ]]; then
    local local_registry_ovn_image="localhost:5000/${OVN_IMAGE}"
    $OCI_BIN tag "$OVN_IMAGE" $local_registry_ovn_image
    OVN_IMAGE=$local_registry_ovn_image
    $OCI_BIN push $push_args "$OVN_IMAGE"
  fi
}

run_kubectl() {
  local retries=0
  local attempts=10
  while true; do
    if kubectl "$@"; then
      break
    fi

    ((retries += 1))
    if [[ "${retries}" -gt ${attempts} ]]; then
      echo "error: 'kubectl $*' did not succeed, failing"
      exit 1
    fi
    echo "info: waiting for 'kubectl $*' to succeed..."
    sleep 1
  done
}

setup_kubectl_bin() {
    ###########################################################################
    # Description:                                                            #
    # setup kubectl for querying the cluster                                  #
    #                                                                         #
    # Arguments:                                                              #
    #   $1 - error message if not provided, it will just exit                 #
    ###########################################################################
    if [ ! -d "./bin" ]
    then
        mkdir -p ./bin
        if_error_exit "Failed to create bin dir!"
    fi

    if [[ "$OSTYPE" == "linux-gnu" ]]; then
        OS_TYPE="linux"
    elif [[ "$OSTYPE" == "darwin"* ]]; then
        OS_TYPE="darwin"
    fi

    pushd ./bin
       if [ ! -f ./kubectl ]; then
           curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/${OS_TYPE}/${ARCH}/kubectl"
           if_error_exit "Failed to download kubectl failed!"
       fi
    popd

    chmod +x ./bin/kubectl
    export PATH=${PATH}:$(pwd)/bin
}

command_exists() {
  cmd="$1"
  command -v ${cmd} >/dev/null 2>&1
}

detect_apiserver_url() {
  # Detect API_URL used for in-cluster communication
  #
  # This will return apiserver address in format https://<node-name>:<port>
  DNS_NAME_URL=$(kind get kubeconfig --internal --name "${KIND_CLUSTER_NAME}" | grep server | awk '{ print $2 }')
  # cut https:// from the URL
  CP_NODE=${DNS_NAME_URL#*//}
  # cut port from the URL
  CP_NODE=${CP_NODE%:*}
  # find node IP address in the kind network
  if [ "$PLATFORM_IPV4_SUPPORT" == false ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    NODE_IP="[$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.kind.GlobalIPv6Address}}' $CP_NODE)]"
  else
    NODE_IP=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' "$CP_NODE")
  fi
  # replace node name with node IP address
  API_URL=${DNS_NAME_URL/$CP_NODE/$NODE_IP}
}

docker_disable_ipv6() {
  # Docker disables IPv6 globally inside containers except in the eth0 interface.
  # Kind enables IPv6 globally the containers ONLY for dual-stack and IPv6 deployments.
  # Ovnkube-node tries to move all global addresses from the gateway interface to the
  # bridge interface it creates. This breaks on KIND with IPv4 only deployments, because the new
  # internal bridge has IPv6 disable and can't move the IPv6 from the eth0 interface.
  # We can enable IPv6 always in the container, since the docker setup with IPv4 only
  # is not very common.
  KIND_NODES=$(kind_get_nodes)
  for n in $KIND_NODES; do
    $OCI_BIN exec "$n" sysctl --ignore net.ipv6.conf.all.disable_ipv6=0
    $OCI_BIN exec "$n" sysctl --ignore net.ipv6.conf.all.forwarding=1
  done
}

coredns_patch() {
  dns_server="8.8.8.8"
  # No need for ipv6 nameserver for dual stack, it will ask for 
  # A and AAAA records
  if [ "$IP_FAMILY" == "ipv6" ]; then
    dns_server="2001:4860:4860::8888"
  fi

  # Patch CoreDNS to work
  # 1. Github CI doesn´t offer IPv6 connectivity, so CoreDNS should be configured
  # to work in an offline environment:
  # https://github.com/coredns/coredns/issues/2494#issuecomment-457215452
  # 2. Github CI adds following domains to resolv.conf search field:
  # .net.
  # CoreDNS should handle those domains and answer with NXDOMAIN instead of SERVFAIL
  # otherwise pods stops trying to resolve the domain.
  # Get the current config
  original_coredns=$(kubectl get -oyaml -n=kube-system configmap/coredns)
  echo "Original CoreDNS config:"
  echo "${original_coredns}"
  # Patch it
  fixed_coredns=$(
    printf '%s' "${original_coredns}" | sed \
      -e 's/^.*kubernetes cluster\.local/& net/' \
      -e '/^.*upstream$/d' \
      -e '/^.*fallthrough.*$/d' \
      -e 's/^\(.*forward \.\).*$/\1 '"$dns_server"' {/' \
      -e '/^.*loop$/d' \
  )
  echo "Patched CoreDNS config:"
  echo "${fixed_coredns}"
  printf '%s' "${fixed_coredns}" | kubectl apply -f -
}

install_ingress() {
  run_kubectl apply -f "${DIR}/ingress/mandatory.yaml"
  run_kubectl apply -f "${DIR}/ingress/service-nodeport.yaml"
}

METALLB_DIR="/tmp/metallb"
install_metallb() {
  # Using latest v0.14.9 as the commit we were using would not build and this
  # version is the one having least issues for dual stack. However tests might
  # have to workaround these two outstanding issue until fixed
  # https://github.com/metallb/metallb/issues/2723
  # https://github.com/metallb/metallb/issues/2724
  local metallb_version=v0.14.9
  mkdir -p /tmp/metallb
  local builddir
  builddir=$(mktemp -d "${METALLB_DIR}/XXXXXX")

  pushd "${builddir}"
  git clone https://github.com/metallb/metallb.git
  cd metallb
  git checkout $metallb_version

  # kindest/node image v1.32+ that we use is only compatible with kind v0.27+
  # when using 'kind load' command however metallb builds and uses older
  # incompatible kind version patch it so that it uses our own kind install
  # instead of their build
  patch tasks.py << 'EOF'
@@ -29,7 +29,7 @@ extra_network = "network2"
-controller_gen_version = "v0.16.3"
+controller_gen_version = "v0.19.0"
 build_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "build")
 kubectl_path = os.path.join(build_path, "kubectl")
-kind_path = os.path.join(build_path, "kind")
+kind_path = "kind"
 ginkgo_path = os.path.join(build_path, "bin", "ginkgo")
 controller_gen_path = os.path.join(build_path, "bin", "controller-gen")
 kubectl_version = "v1.31.0"
EOF

  pip install -r dev-env/requirements.txt

  local ip_family ipv6_network
  if [ "$PLATFORM_IPV4_SUPPORT" == true ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    ip_family="dual"
    ipv6_network="--ipv6 --subnet=${METALLB_CLIENT_NET_SUBNET_IPV6}"
  elif  [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    ip_family="ipv6"
    ipv6_network="--ipv6 --subnet=${METALLB_CLIENT_NET_SUBNET_IPV6}"
  else
    ip_family="ipv4"
    ipv6_network=""
  fi
  # Override GOBIN until https://github.com/metallb/metallb/issues/2218 is fixed.
  GOBIN="" inv dev-env -n ovn -b frr -p bgp -i "${ip_family}"

  $OCI_BIN network rm -f clientnet
  $OCI_BIN network create --subnet="${METALLB_CLIENT_NET_SUBNET_IPV4}" ${ipv6_network} --driver bridge clientnet
  $OCI_BIN network connect clientnet frr
  if  [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    # Enable IPv6 forwarding in FRR
    $OCI_BIN exec frr sysctl -w net.ipv6.conf.all.forwarding=1
  fi
  # Note: this image let's us use it also for creating load balancer backends that can send big packets
  $OCI_BIN rm -f lbclient
  $OCI_BIN run  --cap-add NET_ADMIN --user 0  -d --network clientnet  --rm  --name lbclient  quay.io/itssurya/dev-images:metallb-lbservice
  popd
  delete_metallb_dir

  # The metallb commit https://github.com/metallb/metallb/commit/1a8e52c393d40efd17f28491616f6f9f7790a522
  # removes control plane node from acting as a bgp speaker for service routes.
  # Hence remove node.kubernetes.io/exclude-from-external-load-balancers label from control-plane nodes
  # so that they are also available for advertising bgp routes which are needed for ovnkube's service
  # specific e2e tests.
  MASTER_NODES=$(kind_get_nodes | sort | head -n "${KIND_NUM_MASTER}")
  for n in $MASTER_NODES; do
    kubectl label node "$n" node.kubernetes.io/exclude-from-external-load-balancers-
  done

  kind_network_v4=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' frr)
  echo "FRR kind network IPv4: ${kind_network_v4}"
  kind_network_v6=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.kind.GlobalIPv6Address}}' frr)
  echo "FRR kind network IPv6: ${kind_network_v6}"
  local client_network_v4 client_network_v6
  client_network_v4=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.clientnet.IPAddress}}' frr)
  echo "FRR client network IPv4: ${client_network_v4}"
  client_network_v6=$($OCI_BIN inspect -f '{{.NetworkSettings.Networks.clientnet.GlobalIPv6Address}}' frr)
  echo "FRR client network IPv6: ${client_network_v6}"

  local client_subnets
  client_subnets=$($OCI_BIN network inspect clientnet -f '{{range .IPAM.Config}}{{.Subnet}}#{{end}}')
  echo "${client_subnets}"
  local client_subnets_v4 client_subnets_v6
  client_subnets_v4=$(echo "${client_subnets}" | cut -d '#' -f 1)
  echo "client subnet IPv4: ${client_subnets_v4}"
  client_subnets_v6=$(echo "${client_subnets}" | cut -d '#' -f 2)
  echo "client subnet IPv6: ${client_subnets_v6}"

  KIND_NODES=$(kind_get_nodes)
  for n in ${KIND_NODES}; do
    if [ "$PLATFORM_IPV4_SUPPORT" == true ]; then
        $OCI_BIN exec "${n}" ip route add "${client_subnets_v4}" via "${kind_network_v4}"
    fi
    if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
        $OCI_BIN exec "${n}" ip -6 route add "${client_subnets_v6}" via "${kind_network_v6}"
    fi
  done

  # for now, we only run one test with metalLB load balancer for which this
  # one svcVIP (192.168.10.0/fc00:f853:ccd:e799::) is more than enough since at a time we will only
  # have one load balancer service
  if [ "$PLATFORM_IPV4_SUPPORT" == true ]; then
    $OCI_BIN exec lbclient ip route add 192.168.10.0 via "${client_network_v4}" dev eth0
  fi
  if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    $OCI_BIN exec lbclient ip -6 route add fc00:f853:ccd:e799:: via "${client_network_v6}" dev eth0
  fi
  sleep 30
}

install_plugins() {
  git clone https://github.com/containernetworking/plugins.git
  pushd plugins
  CGO_ENABLED=0 ./build_linux.sh
  KIND_NODES=$(kind_get_nodes)
  # Opted for not overwritting the existing plugins
  for node in $KIND_NODES; do
    for plugin in bandwidth bridge dhcp dummy firewall host-device ipvlan macvlan sbr static tuning vlan vrf; do
      $OCI_BIN cp ./bin/$plugin $node:/opt/cni/bin/
    done
  done
  popd
  rm -rf plugins
}

destroy_metallb() {
  if $OCI_BIN ps --format '{{.Names}}' | grep -Eq '^lbclient$'; then
      $OCI_BIN stop lbclient
  fi
  if $OCI_BIN ps --format '{{.Names}}' | grep -Eq '^frr$'; then
      $OCI_BIN stop frr
  fi
  if $OCI_BIN network ls --format '{{.Name}}' | grep -q '^clientnet$'; then
      $OCI_BIN network rm clientnet
  fi
  delete_metallb_dir
}

delete_metallb_dir() {
  if ! [ -d "${METALLB_DIR}" ]; then
      return
  fi

  # The build directory will contain read only directories after building. Files cannot be deleted, even by the owner.
  # Therefore, set all dirs to u+rwx.
  find "${METALLB_DIR}" -type d -exec chmod u+rwx "{}" \;
  rm -rf "${METALLB_DIR}"
}

# kubectl_wait_pods will set a total timeout of 300s for IPv4 and 480s for IPv6. It will first wait for all
# DaemonSets to complete with kubectl rollout. This command will block until all pods of the DS are actually up.
# Next, it iterates over all pods with name=ovnkube-db and ovnkube-master and waits for them to post "Ready".
# Last, it will do the same with all pods in the kube-system namespace.
kubectl_wait_pods() {
  # IPv6 cluster seems to take a little longer to come up, so extend the wait time.
  OVN_TIMEOUT=300
  if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    OVN_TIMEOUT=480
  fi

  # We will make sure that we timeout all commands at current seconds + the desired timeout.
  endtime=$(( SECONDS + OVN_TIMEOUT ))

  for ds in ovnkube-node ovs-node; do
    timeout=$(calculate_timeout ${endtime})
    echo "Waiting for k8s to launch all ${ds} pods (timeout ${timeout})..."
    kubectl rollout status daemonset -n ovn-kubernetes ${ds} --timeout ${timeout}s
  done

  pods=""
  if [ "$OVN_ENABLE_INTERCONNECT" == true ]; then
    pods="ovnkube-control-plane"
  else
    pods="ovnkube-master ovnkube-db"
  fi
  for name in ${pods}; do
    timeout=$(calculate_timeout ${endtime})
    echo "Waiting for k8s to create ${name} pods (timeout ${timeout})..."
    kubectl wait pods -n ovn-kubernetes -l name=${name} --for condition=Ready --timeout=${timeout}s
  done

  timeout=$(calculate_timeout ${endtime})
  if ! kubectl wait -n kube-system --for=condition=ready pods --all --timeout=${timeout}s ; then
    echo "some pods in the system are not running"
    kubectl get pods -A -o wide || true
    exit 1
  fi
}

# calculate_timeout takes an absolute endtime in seconds (based on bash script runtime, see
# variable $SECONDS) and calculates a relative timeout value. Should the calculated timeout
# be <= 0, return one second.
calculate_timeout() {
  endtime=$1
  timeout=$(( endtime - SECONDS ))
  if [ ${timeout} -le 0 ]; then
      timeout=1
  fi
  echo ${timeout}
}

sleep_until_pods_settle() {
  echo "Pods are all up, allowing things settle for ${KIND_SETTLE_DURATION} seconds..."
  sleep ${KIND_SETTLE_DURATION}
}

is_nested_virt_enabled() {
    local kvm_nested="unknown"
    if [ -f "/sys/module/kvm_intel/parameters/nested" ]; then
        kvm_nested=$( cat /sys/module/kvm_intel/parameters/nested )
    elif [ -f "/sys/module/kvm_amd/parameters/nested" ]; then
        kvm_nested=$( cat /sys/module/kvm_amd/parameters/nested )
    fi
    [ "$kvm_nested" == "1" ] || [ "$kvm_nested" == "Y" ] || [ "$kvm_nested" == "y" ]
}

install_kubevirt() {
    # possible values:
    # stable - install newest stable (default)
    # vX.Y.Z - install specific stable (i.e v1.3.1)
    # nightly - install newest nightly
    # nightly tag - install specific nightly (i.e 20240910)
    # KUBEVIRT_VERSION=${KUBEVIRT_VERSION:-"stable"}

    KUBEVIRT_VERSION=${KUBEVIRT_VERSION:-"v1.6.2"}

    for node in $(kubectl get node --no-headers  -o custom-columns=":metadata.name"); do
        $OCI_BIN exec -t $node bash -c "echo 'fs.inotify.max_user_watches=1048576' >> /etc/sysctl.conf"
        $OCI_BIN exec -t $node bash -c "echo 'fs.inotify.max_user_instances=512' >> /etc/sysctl.conf"
        $OCI_BIN exec -i $node bash -c "sysctl -p /etc/sysctl.conf"
        if [[ "${node}" =~ worker ]]; then
            kubectl label nodes $node node-role.kubernetes.io/worker="" --overwrite=true
        fi
    done

    if [ "$(kubectl get kubevirts -n kubevirt kubevirt -ojsonpath='{.status.phase}')" != "Deployed" ]; then
      local kubevirt_release_url=$(get_kubevirt_release_url "$KUBEVIRT_VERSION")
      echo "Deploying Kubevirt from $kubevirt_release_url"
      kubectl apply -f "${kubevirt_release_url}/kubevirt-operator.yaml"
      kubectl apply -f "${kubevirt_release_url}/kubevirt-cr.yaml"
      if ! is_nested_virt_enabled; then
        kubectl -n kubevirt patch kubevirt kubevirt --type=merge --patch '{"spec":{"configuration":{"developerConfiguration":{"useEmulation":true}}}}'
      fi
    fi

    kubectl -n kubevirt patch kubevirt kubevirt --type=json --patch '[
        {"op":"add","path":"/spec/configuration/virtualMachineOptions","value":{}},
        {"op":"add","path":"/spec/configuration/virtualMachineOptions/disableSerialConsoleLog","value":{}},
        {"op":"add","path":"/spec/configuration/developerConfiguration","value":{"featureGates":[]}},
        {"op":"add","path":"/spec/configuration/developerConfiguration/featureGates/-","value":"NetworkBindingPlugins"},
        {"op":"add","path":"/spec/configuration/developerConfiguration/featureGates/-","value":"DynamicPodInterfaceNaming"},
        {"op":"add","path":"/spec/configuration/network","value":{}},
        {"op":"add","path":"/spec/configuration/network/binding","value":{"l2bridge":{"domainAttachmentType":"managedTap","migration":{}}}}
    ]'

    if ! kubectl wait -n kubevirt kv kubevirt --for condition=Available --timeout 15m; then
        kubectl get pod -n kubevirt -l || true
        kubectl describe pod -n kubevirt -l || true
        for p in $(kubectl get pod -n kubevirt -l -o name |sed "s#pod/##"); do
            kubectl logs -p --all-containers=true -n kubevirt $p || true
            kubectl logs --all-containers=true -n kubevirt $p || true
        done
    fi
}

install_cert_manager() {
  local cert_manager_version="v1.14.4"
  echo "Installing cert-manager ..."
  manifest="https://github.com/cert-manager/cert-manager/releases/download/${cert_manager_version}/cert-manager.yaml"
  run_kubectl apply -f "$manifest"
}

install_kubevirt_ipam_controller() {
  echo "Installing KubeVirt IPAM controller manager ..."
  manifest="https://github.com/kubevirt/ipam-extensions/releases/download/v0.3.1/install.yaml"
  run_kubectl apply -f "$manifest"
  kubectl wait -n kubevirt-ipam-controller-system deployment kubevirt-ipam-controller-manager --for condition=Available --timeout 2m
}

install_multus() {
  local version="v4.1.3"
  echo "Installing multus-cni $version daemonset ..."
  wget -qO- "https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/${version}/deployments/multus-daemonset.yml" |\
    sed -e "s|multus-cni:snapshot|multus-cni:${version}|g" |\
    run_kubectl apply -f -
}

install_mpolicy_crd() {
  echo "Installing multi-network-policy CRD ..."
  mpolicy_manifest="https://raw.githubusercontent.com/k8snetworkplumbingwg/multi-networkpolicy/refs/tags/v1.0.1/scheme.yml"
  run_kubectl apply -f "$mpolicy_manifest"
}

install_ipamclaim_crd() {
  echo "Installing IPAMClaim CRD ..."
  ipamclaims_manifest="https://raw.githubusercontent.com/k8snetworkplumbingwg/ipamclaims/v0.5.1-alpha/artifacts/k8s.cni.cncf.io_ipamclaims.yaml"
  run_kubectl apply -f "$ipamclaims_manifest"
}

docker_create_second_disconnected_interface() {
  echo "adding second interfaces to nodes"
  local bridge_name="${1:-xgw}"
  echo "bridge: $bridge_name"

  if [ "${OCI_BIN}" = "podman" ]; then
    # docker and podman do different things with the --internal parameter:
    # - docker installs iptables rules to drop traffic on a different subnet
    #   than the bridge and we don't want that.
    # - podman does not set the bridge as default gateway and we want that.
    # So we need it with podman but not with docker. Neither allows us to create
    # a bridge network without IPAM which would be ideal, so perhaps the best
    # option would be a manual setup.
    local podman_params="--internal"
  fi

  # Create the network without subnets; ignore if already exists.
  "$OCI_BIN" network create --driver=bridge ${podman_params-} "$bridge_name" || true

  KIND_NODES=$(kind_get_nodes)
  for n in $KIND_NODES; do
    "$OCI_BIN" network connect "$bridge_name" "$n" || true
  done
}

enable_multi_net() {
  install_multus
  install_mpolicy_crd
  install_ipamclaim_crd
  docker_create_second_disconnected_interface "underlay"  # localnet scenarios require an extra interface
}

kind_get_nodes() {
  kind get nodes --name "${KIND_CLUSTER_NAME}" | grep -v external-load-balancer
}

set_dnsnameresolver_images() {
  if [ "$KIND_LOCAL_REGISTRY" == true ];then
    COREDNS_WITH_OCP_DNSNAMERESOLVER="localhost:5000/coredns-with-ocp-dnsnameresolver:latest"
    DNSNAMERESOLVER_OPERATOR="localhost:5000/dnsnameresolver-operator:latest"
  else
    COREDNS_WITH_OCP_DNSNAMERESOLVER="localhost/coredns-with-ocp-dnsnameresolver:dev"
    DNSNAMERESOLVER_OPERATOR="localhost/dnsnameresolver-operator:dev"
  fi
}

# build_image accepts three arguments. The first argument is the absolute path to the directory
# which contains the Dockerfile. The second argument is the image name along with the tag. The
# third argument is the name of the Dockerfile to use for building the image. 
build_image() {
  pushd ${1}
  $OCI_BIN build -t "${2}" -f ${3} .

  # store in local registry
  if [ "$KIND_LOCAL_REGISTRY" == true ];then
    echo "Pushing built image (${2}) to local $OCI_BIN registry"
    $OCI_BIN push "${2}"
  fi
  popd
}

build_dnsnameresolver_images() {
  set_dnsnameresolver_images
  rm -rf /tmp/coredns-ocp-dnsnameresolver
  git clone https://github.com/openshift/coredns-ocp-dnsnameresolver.git /tmp/coredns-ocp-dnsnameresolver
  pushd /tmp/coredns-ocp-dnsnameresolver
  git checkout release-4.21
  popd
 
  build_image /tmp/coredns-ocp-dnsnameresolver ${COREDNS_WITH_OCP_DNSNAMERESOLVER} Dockerfile.upstream

  build_image /tmp/coredns-ocp-dnsnameresolver/operator ${DNSNAMERESOLVER_OPERATOR} Dockerfile
}

# install_image accepts the image name along with the tag as an argument and installs it.
install_image() {
  # If local registry is being used push image there for consumption by kind cluster
  if [ "$KIND_LOCAL_REGISTRY" == true ]; then
    echo "${1} should already be avaliable in local registry, not loading"
  else
    if [ "$OCI_BIN" == "podman" ]; then
      # podman: cf https://github.com/kubernetes-sigs/kind/issues/2027
      rm -f /tmp/image.tar
      podman save -o /tmp/image.tar "${1}"
      kind load image-archive /tmp/image.tar --name "${KIND_CLUSTER_NAME}"
    else
      kind load docker-image "${1}" --name "${KIND_CLUSTER_NAME}"
    fi
  fi
}

install_dnsnameresolver_images() {
  install_image ${COREDNS_WITH_OCP_DNSNAMERESOLVER}
  install_image ${DNSNAMERESOLVER_OPERATOR}
}

install_dnsnameresolver_operator() {
  pushd /tmp/coredns-ocp-dnsnameresolver/operator
  
  # Before installing DNSNameResolver operator, update the args so that the operator
  # is configured with the correct values.
  sed -i -e 's/^\(.*--coredns-namespace=\).*/\1kube-system/' \
    -e 's/^\(.*--coredns-service-name=\).*/\1kube-dns/' \
    -e 's/^\(.*--dns-name-resolver-namespace=\).*/\1ovn-kubernetes/' \
    -e 's/^\(.*--coredns-port=\).*/\153/' config/default/manager_auth_proxy_patch.yaml

  make install CONTROLLER_TOOLS_VERSION=v0.19.0
  make deploy IMG=${DNSNAMERESOLVER_OPERATOR} CONTROLLER_TOOLS_VERSION=v0.19.0
  popd
}

update_clusterrole_coredns() {
  original_clusterrole=$(kubectl get clusterrole system:coredns -oyaml)
  echo "Original CoreDNS clusterrole:"
  echo "${original_clusterrole}"
  additional_permissions=$(printf '%s' '
- apiGroups:
  - network.openshift.io
  resources:
  - dnsnameresolvers
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - network.openshift.io
  resources:
  - dnsnameresolvers/status
  verbs:
  - update
  - get
  - patch
')
  updated_clusterrole="${original_clusterrole}${additional_permissions}"
  echo "Patched CoreDNS clusterrole:"
  echo "${updated_clusterrole}"
  printf '%s' "${updated_clusterrole}" | kubectl apply -f -
}

add_ocp_dnsnameresolver_to_coredns_config() {
  original_corefile=$(kubectl get -n=kube-system configmap/coredns -o=jsonpath="{.data['Corefile']}")
  if ! grep -wq "ocp_dnsnameresolver" <<< ${original_corefile}; then
    echo "Original CoreDNS Corefile:"
    echo "${original_corefile}"
    updated_corefile=$(
      printf '%s' "${original_corefile}" | sed -e 's/^\(.*\)\(forward.*\)/\1ocp_dnsnameresolver {\n\1   namespaces ovn-kubernetes\n\1}\n\1\2/'
    )
    echo "Patched CoreDNS Corefile:"
    echo "${updated_corefile}"
    printf '%s' "${updated_corefile}" > /tmp/Corefile.json
    updated_coredns=$(kubectl create configmap coredns -n=kube-system --from-file=Corefile=/tmp/Corefile.json -oyaml --dry-run=client)
    echo "Patched CoreDNS config:"
    echo "${updated_coredns}"
    printf '%s' "${updated_coredns}" | kubectl apply -f -
  fi
}

update_coredns_deployment_image() {
  kubectl -n=kube-system set image deploy/coredns coredns=${COREDNS_WITH_OCP_DNSNAMERESOLVER}
}

# kubectl_wait_dnsnameresolver_pods will set a total timeout of 60s and wait for the pods
# related to the dns name resolver feature to become "Ready".
kubectl_wait_dnsnameresolver_pods() {
  TIMEOUT=60

  # We will make sure that we timeout all commands at current seconds + the desired timeout.
  endtime=$(( SECONDS + TIMEOUT ))

  timeout=$(calculate_timeout ${endtime})
  echo "Waiting for pods in dnsnameresolver-operator namespace to become ready (timeout ${timeout})..."
  kubectl wait -n dnsnameresolver-operator --for=condition=ready pods --all --timeout=${timeout}s
}

get_kubevirt_release_url() {
    local VERSION="$1"

    local kubevirt_version
    local kubevirt_release_url

    if [[ "$VERSION" == "stable" ]]; then
        kubevirt_version=$(curl -sL https://storage.googleapis.com/kubevirt-prow/release/kubevirt/kubevirt/stable.txt)
        kubevirt_release_url="https://github.com/kubevirt/kubevirt/releases/download/${kubevirt_version}"
    elif [[ "$VERSION" == v* ]]; then
        kubevirt_version="$VERSION"
        kubevirt_release_url="https://github.com/kubevirt/kubevirt/releases/download/${kubevirt_version}"
    elif [[ "$VERSION" == "nightly" ]]; then
        kubevirt_version=$(curl -sL https://storage.googleapis.com/kubevirt-prow/devel/nightly/release/kubevirt/kubevirt/latest)
        kubevirt_release_url="https://storage.googleapis.com/kubevirt-prow/devel/nightly/release/kubevirt/kubevirt/${kubevirt_version}"
    elif [[ "$VERSION" =~ ^[0-9]{8}$ ]]; then
        kubevirt_version="$VERSION"
        kubevirt_release_url="https://storage.googleapis.com/kubevirt-prow/devel/nightly/release/kubevirt/kubevirt/${kubevirt_version}"
    else
        echo "Unsupported KUBEVIRT_VERSION value $VERSION (use either stable, vX.Y.Z, nightly or nightly tag)"
        exit 1
    fi

    echo "$kubevirt_release_url"
}

readonly FRR_K8S_VERSION=v0.0.21
readonly FRR_TMP_DIR=$(mktemp -d -u)

clone_frr() {
  [ -d "$FRR_TMP_DIR" ] || {
    mkdir -p "$FRR_TMP_DIR" && trap 'rm -rf $FRR_TMP_DIR' EXIT
    pushd "$FRR_TMP_DIR" || exit 1
    git clone --depth 1 --branch $FRR_K8S_VERSION https://github.com/metallb/frr-k8s

    # Download the patches
    curl -Ls https://github.com/jcaamano/frr-k8s/archive/refs/heads/ovnk-bgp-v0.0.21.tar.gz | tar xzvf - frr-k8s-ovnk-bgp-v0.0.21/patches --strip-components 1

    # Change into the cloned repo directory before applying patches
    pushd frr-k8s
    git apply ../patches/*
    popd

    popd || exit 1
  }
}

deploy_frr_external_container() {
  echo "Deploying FRR external container ..."
  clone_frr
 
  pushd "$FRR_TMP_DIR" || exit 1
  run_kubectl apply -f frr-k8s/charts/frr-k8s/charts/crds/templates/frrk8s.metallb.io_frrconfigurations.yaml
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
  ./demo.sh
  popd || exit 1
  if  [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    # Enable IPv6 forwarding in FRR
    $OCI_BIN exec frr sysctl -w net.ipv6.conf.all.forwarding=1
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
  local ip_family ipv6_network
  if [ "$PLATFORM_IPV4_SUPPORT" == true ] && [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    ip_family="dual"
    ipv6_network="--ipv6 --subnet=${BGP_SERVER_NET_SUBNET_IPV6}"
  elif  [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    ip_family="ipv6"
    ipv6_network="--ipv6 --subnet=${BGP_SERVER_NET_SUBNET_IPV6}"
  else
    ip_family="ipv4"
    ipv6_network=""
  fi
  $OCI_BIN rm -f bgpserver
  $OCI_BIN network rm -f bgpnet
  $OCI_BIN network create --subnet="${BGP_SERVER_NET_SUBNET_IPV4}" ${ipv6_network} --driver bridge bgpnet
  $OCI_BIN network connect bgpnet frr
  $OCI_BIN run  --cap-add NET_ADMIN --user 0  -d --network bgpnet  --rm  --name bgpserver -p 8080:8080  registry.k8s.io/e2e-test-images/agnhost:2.45 netexec
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

install_ffr_k8s() {
  echo "Installing frr-k8s ..."
  clone_frr

  # apply frr-k8s
  kubectl apply -f "${FRR_TMP_DIR}"/frr-k8s/config/all-in-one/frr-k8s.yaml
  kubectl wait -n frr-k8s-system deployment frr-k8s-statuscleaner --for condition=Available --timeout 2m
  kubectl rollout status -n frr-k8s-system daemonset frr-k8s-daemon --timeout 2m

  # apply a BGP peer configration with the external gateway that does not
  # exchange routes
  pushd "${FRR_TMP_DIR}"/frr-k8s/hack/demo/configs || exit 1
  sed 's/mode: all/mode: filtered/g' receive_all.yaml > receive_filtered.yaml
  # Allow receiving the bgp external server's prefix
  sed -i '/mode: filtered/a\            prefixes:\n            - prefix: '"${BGP_SERVER_NET_SUBNET_IPV4}"'' receive_filtered.yaml
  # If IPv6 is enabled, add the IPv6 prefix as well
  if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
    # Find all line numbers where the IPv4 prefix is defined
    IPv6_LINE="            - prefix: ${BGP_SERVER_NET_SUBNET_IPV6}"
    # Process each occurrence of the IPv4 prefix
    for LINE_NUM in $(grep -n "prefix: ${BGP_SERVER_NET_SUBNET_IPV4}" receive_filtered.yaml | cut -d ':' -f 1); do
      # Insert the IPv6 prefix after each IPv4 prefix line
      sed -i "${LINE_NUM}a\\${IPv6_LINE}" receive_filtered.yaml
    done
  fi
  
  # frr-k8s webhook is declaring readiness before its endpoint is serving.
  # Let's do our own probing. Also will print logs in case of failure so we get
  # insights on why this is hapenning 
  local r
  r=0
  timeout 60s bash -x <<EOF || r=$?
echo "Attempting to reach frr-k8s webhook"
kind export kubeconfig --name ovn
while true; do
CLUSTER_IP=\$(kubectl get svc -n frr-k8s-system frr-k8s-webhook-service -o jsonpath='{.spec.clusterIP}')
# Wrap IPv6 addresses in brackets for URL syntax
[[ \${CLUSTER_IP} =~ : ]] && CLUSTER_IP="[\${CLUSTER_IP}]"
$OCI_BIN exec ovn-control-plane curl -ksS --connect-timeout 0.1 https://\${CLUSTER_IP}
[ \$? -eq 0 ] && exit 0
echo "Couldn't reach frr-k8s webhook, trying in 1s..."
sleep 1s
done
EOF
  echo "r=$r"
  if [ "$r" -ne "0" ]; then
    kubectl describe pod -n frr-k8s-system -l app=frr-k8s-webhook-server
    kubectl logs -n frr-k8s-system -l app=frr-k8s-webhook-server
  fi

  kubectl apply -n frr-k8s-system -f receive_filtered.yaml
  popd || exit 1

  rm -rf "${FRR_TMP_DIR}"
  # Add routes for pod networks dynamically into the github runner for return traffic to pass back
  if [ "$ADVERTISE_DEFAULT_NETWORK" = "true" ]; then
    echo "Adding routes for Kubernetes pod networks..."
    NODES=$(kubectl get nodes -o jsonpath='{.items[*].metadata.name}')
    echo "Found nodes: $NODES"
    for node in $NODES; do
      # Get the addresses
      node_ips=$(kubectl get node $node -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
      # Get subnet information
      subnet_json=$(kubectl get node $node -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/node-subnets}')
      
      if [ "$PLATFORM_IPV4_SUPPORT" == true ]; then
        # Extract IPv4 address (first address)
        node_ipv4=$(echo "$node_ips" | awk '{print $1}')
        ipv4_subnet=$(echo "$subnet_json" | jq -r '.default[0]')
        
        # Add IPv4 route
        if [ -n "$ipv4_subnet" ] && [ -n "$node_ipv4" ]; then
          echo "Adding IPv4 route for $node ($node_ipv4): $ipv4_subnet"
          sudo ip route replace $ipv4_subnet via $node_ipv4
        fi
      fi

      # Add IPv6 route if enabled
      if [ "$PLATFORM_IPV6_SUPPORT" == true ]; then
        # Extract IPv6 address (second address, if present)
        node_ipv6=$(echo "$node_ips" | awk '{print $2}')
        ipv6_subnet=$(echo "$subnet_json" | jq -r '.default[1] // empty')
        
        if [ -n "$ipv6_subnet" ] && [ -n "$node_ipv6" ]; then
          echo "Adding IPv6 route for $node ($node_ipv6): $ipv6_subnet"
          sudo ip -6 route replace $ipv6_subnet via $node_ipv6
        fi
      fi
    done
  fi
}

interconnect_arg_check() {
  if [ "${IC_ARG_PROVIDED:-}" = "true" ]; then
    echo "INFO: Interconnect mode is now the default mode, you do not need to use pass -ic or --enable-interconnect anymore"
  fi
}

setup_coredumps() {
  # Setup core dump collection
  #
  # Core dumps will be saved on the HOST at /tmp/kind/logs/coredumps (not inside containers)
  # because kernel.core_pattern is a kernel-level setting shared across all containers.
  #
  # - Using a pipe instead of a file path avoids needing to mount
  #   /tmp/kind/logs/coredumps into every container that might crash
  # - The pipe executes in the host's namespace, so /tmp/kind/logs/coredumps
  #   automatically refers to the host path
  #
  # Location: /tmp/kind/logs is used to ensure coredumps are exported in CI
  # Use container exec to avoid asking for root permissions

  mkdir -p "/tmp/kind/logs/coredumps"
  ulimit -c unlimited
  for node in $(kind get nodes --name "${KIND_CLUSTER_NAME}"); do
    # Core dump filename pattern variables:
    #   %P - global PID
    #   %e - executable filename
    #   %h - hostname (container hostname)
    #   %s - signal number that caused dump
    ${OCI_BIN} exec "$node" sysctl -w kernel.core_pattern="|/bin/dd of=/tmp/kind/logs/coredumps/core.%P.%e.%h.%s bs=1M status=none"
  done
}
