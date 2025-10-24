#!/usr/bin/env bash
VERSION=v2.5.0
extra_flags=""
if [ "$#" -ne 1 ]; then
  if [ "$#" -eq 2 ] && [ "$2" == "fix" ]; then
    extra_flags="--fix"
  else
    echo "Expected command line argument - container runtime (docker/podman) got $# arguments: $@"
    exit 1
  fi
fi

$1 run --security-opt label=disable --rm \
  -v  ${HOME}/.cache/golangci-lint:/cache -e GOLANGCI_LINT_CACHE=/cache \
  -v $(pwd):/app -w /app -e GO111MODULE=on docker.io/golangci/golangci-lint:${VERSION} \
	golangci-lint run --verbose --print-resources-usage \
	--modules-download-mode=vendor --timeout=15m0s ${extra_flags} && \
	echo "lint OK!"
