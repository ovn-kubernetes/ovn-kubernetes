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
  # fetch the golangci-lint installer and install the required version
  mkdir -p /tmp/local/bin/
  tmp_installer="$(mktemp)"
  if ! curl -sSL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh -o "${tmp_installer}"; then
    echo "failed to download golangci-lint installer" >&2
    exit 1
  fi
  chmod +x "${tmp_installer}"
  if ! "${tmp_installer}" -b /tmp/local/bin/ "${GOLANGCI_LINT_VERSION}"; then
    echo "failed to install golangci-lint ${GOLANGCI_LINT_VERSION}" >&2
    exit 1
  fi
  rm -f "${tmp_installer}"

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

