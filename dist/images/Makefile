# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# build image for ovn overlay network cni plugin

# ovnkube-db.yaml, ovnkube-node.yaml, and onvkube-master.yaml use this image.
# This image is built from files in this directory and pushed to
# a docker registry that is accesseble on each node.

# For a user created registry, the registry must be setup ahead of time.
# The registry is configured in /etc/containers/registries.conf
# on each node in both "registries:" and "insecure_registries:" sections.

all: ubuntu-image fedora-image

SLASH = -
ARCH ?= $(subst aarch64,arm64,$(subst x86_64,amd64,$(patsubst i%86,386,$(shell uname -m))))
IMAGE_ARCH = $(SLASH)$(ARCH)
DOCKERFILE_ARCH =
ifeq ($(ARCH),arm64)
        DOCKERFILE_ARCH=.arm64
endif
IMAGE ?= ovn-daemonset-fedora

OVN_REPO ?= https://github.com/ovn-org/ovn
OVN_GITREF ?=
ifeq ($(OVN_GITREF),)
OVN_FROM := koji
else
OVN_FROM := source
OVN_GITSHA := $(shell git ls-remote "${OVN_REPO}" "${OVN_GITREF}" | sort -k2  -V  |tail -1 | awk '{ print $$1 }')
endif
GO_VERSION ?= 1.25
GO_IMAGE = quay.io/projectquay/golang:${GO_VERSION}

OCI_BIN ?= docker

# The image of ovnkube/ovn-daemonset-ubuntu should be multi-arched before using it on arm64
ubuntu-image: bld
	${OCI_BIN} build -t ovn-kube-ubuntu$(IMAGE_ARCH) -f Dockerfile.ubuntu$(DOCKERFILE_ARCH) .
ifeq ($(ARCH),amd64)
	${OCI_BIN} tag "ovn-kube-ubuntu$(IMAGE_ARCH):latest" \
              "ovn-kube-ubuntu:latest"
endif

FEDORA_BASE_IMAGE ?= quay.io/fedora/fedora:43

fedora-image:
	${OCI_BIN} pull --platform=linux/${ARCH} ${FEDORA_BASE_IMAGE} 2>/dev/null || true; \
	FEDORA_BASE_DIGEST=$$(${OCI_BIN} inspect --format='{{index .RepoDigests 0}}' ${FEDORA_BASE_IMAGE} 2>/dev/null || echo ""); \
	${OCI_BIN} build \
		--build-arg OVN_FROM=${OVN_FROM} \
		--build-arg OVN_REPO=${OVN_REPO} \
		--build-arg OVN_GITREF=${OVN_GITSHA} \
		--build-arg OVN_KUBERNETES_DIR=${OVN_KUBERNETES_DIR} \
		--build-arg BUILDER_IMAGE=${GO_IMAGE} \
		--build-arg FEDORA_BASE_DIGEST=$${FEDORA_BASE_DIGEST} \
		--platform=linux/${ARCH} \
		-t ${IMAGE} \
		-f Dockerfile.fedora ./../..

DOCKER_IMAGE_TAG = latest

# Multi-arch the ubuntu based image with fat-manifest
ubuntu-image-multi-arch:
	./push_manifest.sh ovn-daemonset-ubuntu $(DOCKER_IMAGE_TAG)

.PHONY: ../../go-controller/_output/go/bin/ovnkube

../../go-controller/_output/go/bin/ovnkube:
	cd ../../go-controller ; make

BRANCH = $(shell git rev-parse  --symbolic-full-name HEAD)
COMMIT = $(shell git rev-parse  HEAD)
bld: ../../go-controller/_output/go/bin/ovnkube
	find ../../go-controller/_output/go/bin/ -maxdepth 1 -type f -exec cp -f {} . \;
	rm -rf LICENSES
	cp -R ../../LICENSES .
	echo "ref: ${BRANCH}  commit: ${COMMIT}" > git_info
