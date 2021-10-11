#!/usr/bin/env bash

set -ex

# setting this env prevents ginkgo e2e from trying to run provider setup
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${HOME}/admin.conf

# Skip tests which are not IPv6 ready yet (see description of https://github.com/ovn-org/ovn-kubernetes/pull/2276)
IPV6_SKIPPED_TESTS="Should be allowed by externalip services|\
should provide connection to external host by DNS name from a pod|\
Should validate NetFlow data of br-int is sent to an external gateway|\
test tainting a node according to its defaults interface MTU size"

SKIPPED_TESTS=""
if [ "$KIND_IPV4_SUPPORT" == true ] && [ "$KIND_IPV6_SUPPORT" == true ]; then
    # No support for these features in dual-stack yet
    SKIPPED_TESTS="hybrid.overlay|external.gateway"
fi

if [ "$OVN_HA" == false ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
  	SKIPPED_TESTS+="|"
  fi
  # No support for these features in no-ha mode yet
  # TODO streamline the db delete tests
  SKIPPED_TESTS+="recovering from deleting db files while maintaining connectivity|\
Should validate connectivity before and after deleting all the db-pods at once in HA mode"
else 
  if [ "$SKIPPED_TESTS" != "" ]; then
  	SKIPPED_TESTS+="|"
  fi

  SKIPPED_TESTS+="Should validate connectivity before and after deleting all the db-pods at once in Non-HA mode"
fi

if [ "$KIND_IPV6_SUPPORT" == true ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
  	SKIPPED_TESTS+="|"
  fi
  # No support for these tests in IPv6 mode yet
  SKIPPED_TESTS+=$IPV6_SKIPPED_TESTS
fi

if [ "$OVN_DISABLE_SNAT_MULTIPLE_GWS" == true ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
  	SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+="Should validate the egress IP functionality against remote hosts"
fi

if [ "$OVN_GATEWAY_MODE" == "local" ]; then
  if [ "$SKIPPED_TESTS" != "" ]; then
  	SKIPPED_TESTS+="|"
  fi
  SKIPPED_TESTS+="Should be allowed to node local cluster-networked endpoints by nodeport services with externalTrafficPolicy=local|\
e2e ingress to host-networked pods traffic validation|\
host to host-networked pods traffic validation"
fi

# setting these is required to make RuntimeClass tests work ... :/
export KUBE_CONTAINER_RUNTIME=remote
export KUBE_CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock
export KUBE_CONTAINER_RUNTIME_NAME=containerd
export NUM_NODES=2

FOCUS=$(echo ${@:1} | sed 's/ /\\s/g')

pushd e2e

go mod download
go test -timeout=0 -v . \
        -ginkgo.v \
        -ginkgo.focus ${FOCUS:-.} \
        -ginkgo.flakeAttempts ${FLAKE_ATTEMPTS:-2} \
        -ginkgo.skip="${SKIPPED_TESTS}" \
        -provider skeleton \
        -kubeconfig ${KUBECONFIG} \
        ${CONTAINER_RUNTIME:+"--container-runtime=${CONTAINER_RUNTIME}"} \
        ${NUM_NODES:+"--num-nodes=${NUM_NODES}"} \
        ${E2E_REPORT_DIR:+"--report-dir=${E2E_REPORT_DIR}"} \
        ${E2E_REPORT_PREFIX:+"--report-prefix=${E2E_REPORT_PREFIX}"}
popd
