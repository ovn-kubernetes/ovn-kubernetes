#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

AGNHOST_IMAGE=${AGNHOST_IMAGE:-registry.k8s.io/e2e-test-images/agnhost:2.59}
CLIENT_POD=${UDN_UPGRADE_SNAT_CLIENT_POD:-client}
DEFAULT_NS=${UDN_UPGRADE_SNAT_DEFAULT_NS:-udn-snat-upgrade-default}
NAD_NAME=${UDN_UPGRADE_SNAT_NAD:-tenant-upgrade}
PING_LOG=${UDN_UPGRADE_SNAT_PING_LOG:-/tmp/udn-snat-upgrade-ping.log}
PING_PID=${UDN_UPGRADE_SNAT_PING_PID:-/tmp/udn-snat-upgrade-ping.pid}
SERVER_POD=${UDN_UPGRADE_SNAT_SERVER_POD:-backend}
UDN_NS=${UDN_UPGRADE_SNAT_NS:-udn-snat-upgrade}

enabled() {
  [[ "${OVN_GATEWAY_MODE:-}" == "shared" ]] &&
    [[ "${ENABLE_MULTI_NET:-}" == "true" ]] &&
    [[ "${ENABLE_NETWORK_SEGMENTATION:-}" == "true" ]]
}

skip_if_disabled() {
  if enabled; then
    return
  fi

  echo "Skipping shared gateway UDN SNAT upgrade check"
  exit 0
}

setup() {
  skip_if_disabled

  kubectl delete namespace "$UDN_NS" "$DEFAULT_NS" \
    --ignore-not-found=true --wait=true

  kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${UDN_NS}
  labels:
    k8s.ovn.org/primary-user-defined-network: ""
---
apiVersion: v1
kind: Namespace
metadata:
  name: ${DEFAULT_NS}
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: ${NAD_NAME}
  namespace: ${UDN_NS}
spec:
  config: |
    {
      "cniVersion": "0.3.0",
      "name": "udn-snat-upgrade",
      "type": "ovn-k8s-cni-overlay",
      "topology": "layer3",
      "subnets": "172.31.0.0/16/24",
      "mtu": 1300,
      "netAttachDefName": "${UDN_NS}/${NAD_NAME}",
      "role": "primary"
    }
---
apiVersion: v1
kind: Pod
metadata:
  name: ${SERVER_POD}
  namespace: ${DEFAULT_NS}
  labels:
    app: udn-snat-upgrade-backend
spec:
  terminationGracePeriodSeconds: 0
  containers:
  - name: agnhost
    image: ${AGNHOST_IMAGE}
    command: ["/agnhost"]
    args: ["netexec", "--http-port=8080"]
    ports:
    - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: backend
  namespace: ${DEFAULT_NS}
spec:
  type: NodePort
  selector:
    app: udn-snat-upgrade-backend
  ports:
  - name: http
    protocol: TCP
    port: 8080
    targetPort: 8080
---
apiVersion: v1
kind: Pod
metadata:
  name: ${CLIENT_POD}
  namespace: ${UDN_NS}
spec:
  terminationGracePeriodSeconds: 0
  containers:
  - name: agnhost
    image: ${AGNHOST_IMAGE}
    command: ["/agnhost"]
    args: ["pause"]
EOF

  kubectl -n "$DEFAULT_NS" wait pod "$SERVER_POD" \
    --for=condition=Ready --timeout=180s
  kubectl -n "$UDN_NS" wait pod "$CLIENT_POD" \
    --for=condition=Ready --timeout=240s

  verify_connectivity "before upgrade"
  start_persistent_ping
}

target_node_ip() {
  local client_node
  local node
  local node_ip
  local node_jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}'

  client_node=$(kubectl -n "$UDN_NS" get pod "$CLIENT_POD" \
    -o=jsonpath='{.spec.nodeName}')

  while read -r node node_ip; do
    if [[ "$node" != "$client_node" && -n "$node_ip" ]]; then
      echo "$node_ip"
      return
    fi
  done < <(kubectl get nodes -o=jsonpath="$node_jsonpath")

  node_ip=$(kubectl get node "$client_node" \
    -o=jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}'
  )
  if [[ -z "$node_ip" ]]; then
    echo "failed to find node InternalIP" >&2
    exit 1
  fi
  echo "$node_ip"
}

verify_connectivity() {
  local phase=$1
  local node_ip
  local node_port
  local output

  node_ip=$(target_node_ip)
  node_port=$(kubectl -n "$DEFAULT_NS" get service backend \
    -o=jsonpath='{.spec.ports[0].nodePort}')

  for _ in {1..30}; do
    output=$(kubectl -n "$UDN_NS" exec "$CLIENT_POD" -- \
      curl -g -q -s --max-time 2 \
      "http://${node_ip}:${node_port}/hostname" 2>&1) || true
    if [[ "$output" == "$SERVER_POD" ]]; then
      echo "UDN SNAT upgrade connectivity succeeded ${phase}"
      return
    fi
    sleep 2
  done

  echo "UDN SNAT upgrade connectivity failed ${phase}: ${output}"
  exit 1
}

start_persistent_ping() {
  local node_ip
  local output

  node_ip=$(target_node_ip)
  kubectl -n "$UDN_NS" exec "$CLIENT_POD" -- /bin/sh -c \
    "rm -f '$PING_LOG' '$PING_PID'; ping -n -i 1 '$node_ip' > '$PING_LOG' 2>&1 & echo \$! > '$PING_PID'"

  for _ in {1..30}; do
    output=$(kubectl -n "$UDN_NS" exec "$CLIENT_POD" -- \
      /bin/sh -c "cat '$PING_LOG' 2>&1" || true)
    if [[ "$output" == *"bytes from"* ]]; then
      echo "UDN SNAT upgrade persistent ping started to ${node_ip}"
      return
    fi
    sleep 1
  done

  echo "UDN SNAT upgrade persistent ping did not receive replies:"
  echo "$output"
  exit 1
}

verify_persistent_ping() {
  local output

  output=$(kubectl -n "$UDN_NS" exec "$CLIENT_POD" -- /bin/sh -c "
    if [ -f '$PING_PID' ]; then
      kill -INT \"\$(cat '$PING_PID')\" 2>/dev/null || true
      sleep 1
    fi
    cat '$PING_LOG' 2>&1
  " || true)

  echo "$output"
  if ! grep -Eq '(^|, )0(\.0+)?% packet loss(,|$)' <<< "$output"; then
    echo "UDN SNAT upgrade persistent ping had packet loss"
    exit 1
  fi

  echo "UDN SNAT upgrade persistent ping had no packet loss"
}

verify() {
  skip_if_disabled

  kubectl -n "$DEFAULT_NS" wait pod "$SERVER_POD" \
    --for=condition=Ready --timeout=180s
  kubectl -n "$UDN_NS" wait pod "$CLIENT_POD" \
    --for=condition=Ready --timeout=240s

  verify_persistent_ping
  verify_connectivity "after upgrade"
}

cleanup() {
  skip_if_disabled

  kubectl delete namespace "$UDN_NS" "$DEFAULT_NS" \
    --ignore-not-found=true --wait=false
}

case "${1:-}" in
  setup)
    setup
    ;;
  verify)
    verify
    ;;
  cleanup)
    cleanup
    ;;
  *)
    echo "usage: $0 {setup|verify|cleanup}" >&2
    exit 2
    ;;
esac
