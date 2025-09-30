#!/usr/bin/env bash
VERSION=v1.64.8
: "${GOLANGCI_LINT_VERSION:=$VERSION}"
extra_flags=(--verbose --print-resources-usage --modules-download-mode=vendor --timeout=15m0s)
if [ "$#" -ne 1 ]; then
  if [ "$#" -eq 2 ] && [ "$2" == "fix" ]; then
    extra_flags+=(--fix)
  else
    echo "Expected command line argument - container runtime (docker/podman) or 'run-natively'; got $# arguments: $*"
    exit 1
  fi
fi

if [ "$1" = "run-natively" ]; then
  mkdir -p /tmp/local/bin/
  curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b /tmp/local/bin/ "${GOLANGCI_LINT_VERSION}"
  mkdir -p /tmp/golangci-cache
  export GOLANGCI_LINT_CACHE=/tmp/golangci-cache
  /tmp/local/bin/golangci-lint run "${extra_flags[@]}" && \
  echo "lint OK!"
else
  $1 run --security-opt label=disable --rm \
    -v  "${HOME}"/.cache/golangci-lint:/cache -e GOLANGCI_LINT_CACHE=/cache \
    -v "$(pwd)":/app -w /app -e GO111MODULE=on docker.io/golangci/golangci-lint:"${VERSION}" \
    golangci-lint run "${extra_flags[@]}" && \
    echo "lint OK!"
fi

