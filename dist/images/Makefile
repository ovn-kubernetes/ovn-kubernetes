# build image for ovn overlay network cni plugin

# ovnkube-db.yaml, ovnkube-node.yaml, and onvkube-master.yaml use this image.
# This image is built from files in this directory and pushed to
# a docker registry that is accesseble on each node.

# For a user created registry, the registry must be setup ahead of time.
# The registry is configured in /etc/containers/registries.conf
# on each node in both "registries:" and "insecure_registries:" sections.

all: ubuntu fedora

SLASH = -
ARCH ?= $(subst aarch64,arm64,$(subst x86_64,amd64,$(patsubst i%86,386,$(shell uname -m))))
IMAGE_ARCH = $(SLASH)$(ARCH)
DOCKERFILE_ARCH = .$(ARCH)
IMAGE ?= ovn-daemonset-fedora

OVN_REPO ?= https://github.com/ovn-org/ovn
OVN_GITREF ?=
ifeq ($(OVN_GITREF),)
OVN_FROM := koji
else
OVN_FROM := source
OVN_GITSHA := $(shell git ls-remote "${OVN_REPO}" "${OVN_GITREF}" | sort -k2  -V  |tail -1 | awk '{ print $$1 }')
endif

GO_VERSION ?= 1.24
GO_IMAGE = quay.io/projectquay/golang:${GO_VERSION}

OCI_BIN ?= docker

# Legacy ubuntu image build - uses pre-built binaries from 'bld' target
# The image of ovnkube/ovn-daemonset-ubuntu should be multi-arched before using it on arm64
ubuntu-image: bld
	${OCI_BIN} build -t ovn-kube-ubuntu$(IMAGE_ARCH) -f Dockerfile.ubuntu$(DOCKERFILE_ARCH) .
ifeq ($(ARCH),amd64)
	${OCI_BIN} tag "ovn-kube-ubuntu$(IMAGE_ARCH):latest" \
              "ovn-kube-ubuntu:latest"
endif

# Multi-arch ubuntu image build using multi-stage approach (similar to fedora-image)
# This builds ovn-kubernetes binaries inside the container for proper cross-platform support
ubuntu-image-multiarch:
	${OCI_BIN} build \
		--build-arg OVN_KUBERNETES_DIR=${OVN_KUBERNETES_DIR} \
		--build-arg BUILDER_IMAGE=${GO_IMAGE} \
		-t ovn-kube-ubuntu-multiarch \
		-f Dockerfile.ubuntu ./../..

ubuntu-shared-gw-deployment: ubuntu-image
	# This is the default in the ovnkube*.yaml files
	# ${OCI_BIN} login -u ovnkube docker.io/ovnkube
	# ${OCI_BIN} push docker.io/ovnkube/ovn-daemonset-ubuntu:latest
	./daemonset.sh --image=docker.io/ovnkube/ovn-daemonset-ubuntu:latest

fedora-image: bld
	${OCI_BIN} build \
		--build-arg OVN_FROM=${OVN_FROM} \
		--build-arg OVN_REPO=${OVN_REPO} \
		--build-arg OVN_GITREF=${OVN_GITSHA} \
		-t ${IMAGE} \
		-f Dockerfile.fedora .

fedora-shared-gw-deployment: fedora-image
	# ${OCI_BIN} login -u ovnkube docker.io/ovnkube
	# ${OCI_BIN} push docker.io/ovnkube/ovn-daemonset-fedora:latest
	./daemonset.sh --image=docker.io/ovnkube/${IMAGE}

fedora-dev-local-gw-deployment: fedora-image
	# ${OCI_BIN} login -u ovnkube docker.io/ovnkube
	# ${OCI_BIN} push docker.io/ovnkube/ovn-daemonset-fedora:latest
	./daemonset.sh --image=docker.io/ovnkube/${IMAGE} \
                    --net-cidr=10.244.0.0/16 \
                    --svc-cidr=10.96.0.0/12 \
                    --gateway-mode="local" \
                    --master-loglevel="5" \
                    --node-loglevel="5" \
                    --ovn-loglevel-northd="-vconsole:info -vfile:info" \
                    --ovn-loglevel-nb="-vconsole:info -vfile:info" \
                    --ovn-loglevel-sb="-vconsole:info -vfile:info" \
                    --ovn-loglevel-controller="-vconsole:info" \
                    --ovn_nb_raft_election_timer="1000" \
                    --ovn_sb_raft_election_timer="1000"

DOCKER_IMAGE_TAG = latest

# Multi-arch the ubuntu based image with fat-manifest
ubuntu-image-multi-arch:
	./push_manifest.sh ovn-daemonset-ubuntu $(DOCKER_IMAGE_TAG)

# This target expands the daemonset yaml templates into final form
# Use CLI flags or environment variables to customize its behavior.
daemonsetyaml:
	./daemonset.sh

.PHONY: ../../go-controller/_output/go/bin/ovnkube

../../go-controller/_output/go/bin/ovnkube:
	cd ../../go-controller ; make

BRANCH = $(shell git rev-parse  --symbolic-full-name HEAD)
COMMIT = $(shell git rev-parse  HEAD)
bld: ../../go-controller/_output/go/bin/ovnkube
	find ../../go-controller/_output/go/bin/ -maxdepth 1 -type f -exec cp -f {} . \;
	echo "ref: ${BRANCH}  commit: ${COMMIT}" > git_info
