# OVN-Kubernetes Container Images

This file covers the container images available for OVN-Kubernetes and how to build them.

## Images / Packages

There are Ubuntu and Fedora-based images available in [GitHub's Registry](https://github.com/orgs/ovn-kubernetes/packages?repo_name=ovn-kubernetes). They are automatically generated upon merges via [a workflow](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/.github/workflows/docker.yml).
Prior to release-1.0, they were called ovn-kube-f (for the Fedora-based image) and ovnkube-u (for the Ubuntu-based image). From release 1.0 and beyond, these have been renamed to ovn-kube-fedora and ovn-kube-ubuntu, respectively.

Therefore, use the following images and tags to obtain these images:

- ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-fedora:master
- ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-fedora:release-1.0

- ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu:master
- ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu:release-1.0

## Building Images

To build images locally, use the following [Makefile](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/dist/images/Makefile) and their respective Dockerfiles from the [dist/images](https://github.com/ovn-kubernetes/ovn-kubernetes/tree/master/dist/images) folder in this repository.

```bash
$ cd dist/images
$ make fedora-image
$ make ubuntu-image
```

The build will create an image called ovn-kube-fedora:latest or ovn-kube-ubuntu:latest, which can be re-tagged.
For example: `${OCI_BIN} tag ovn-kube-fedora:latest ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-fedora:master`

