#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0


set -ex

# setting this env prevents ginkgo e2e from trying to run provider setup
export KUBERNETES_CONFORMANCE_TEST=y
export KUBECONFIG=${KUBECONFIG:-${HOME}/ovn.conf}

# setting these is required to make RuntimeClass tests work ... :/
export KUBE_CONTAINER_RUNTIME=remote
export KUBE_CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock
export KUBE_CONTAINER_RUNTIME_NAME=containerd

pushd conformance
go mod download
# The suite takes about 15 minutes in CI, which runs it in its own 30 minute
# step. Keep this below that step budget so the Go timeout reports before
# GitHub ends the step. Minutes; a trailing "m" is accepted.
CONFORMANCE_TEST_TIMEOUT=${CONFORMANCE_TEST_TIMEOUT:-25}
go test -timeout="${CONFORMANCE_TEST_TIMEOUT%m}m" -v \
        -kubeconfig ${KUBECONFIG}
popd