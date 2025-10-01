# build image for ovn overlay network cni plugin

# ovnkube-db.yaml, ovnkube-node.yaml, and onvkube-master.yaml use this image.
# This image is built from files in this directory and pushed to
# a docker registry that is accesseble on each node.

# For a user created registry, the registry must be setup ahead of time.
# The registry is configured in /etc/containers/registries.conf
# on each node in both "registries:" and "insecure_registries:" sections.

all: ubuntu fedora

SLASH = -
ARCH = $(subst aarch64,arm64,$(subst x86_64,amd64,$(patsubst i%86,386,$(shell uname -m))))
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


OCI_BIN ?= docker

# The image of ovnkube/ovn-daemonset-ubuntu should be multi-arched before using it on arm64
ubuntu-image: bld
	${OCI_BIN} build -t ovn-kube-ubuntu$(IMAGE_ARCH) -f Dockerfile.ubuntu$(DOCKERFILE_ARCH) .
ifeq ($(ARCH),amd64)
	${OCI_BIN} tag "ovn-kube-ubuntu$(IMAGE_ARCH):latest" \
              "ovn-kube-ubuntu:latest"
endif

fedora-image: bld
	${OCI_BIN} build \
		--build-arg OVN_FROM=${OVN_FROM} \
		--build-arg OVN_REPO=${OVN_REPO} \
		--build-arg OVN_GITREF=${OVN_GITSHA} \
		-t ${IMAGE} \
		-f Dockerfile.fedora .

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
	echo "ref: ${BRANCH}  commit: ${COMMIT}" > git_info
