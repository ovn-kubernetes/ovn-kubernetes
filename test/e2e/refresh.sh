#!/bin/bash
#
set -euo pipefail
docker rm -f $(docker ps -a -q --filter ancestor=httpd --format="{{.ID}}") || true
pushd ../../contrib
./kind.sh --enable-interconnect
kind load docker-image --name ovn registry.k8s.io/e2e-test-images/agnhost:2.26
popd
