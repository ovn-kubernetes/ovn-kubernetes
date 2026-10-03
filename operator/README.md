# OVN-Kubernetes operator

Installs and manages OVN-Kubernetes through a single `OVNK8sOperator` custom
resource. This is a work in progress; the design is described in the
OVN-Kubernetes operator enhancement proposal (OKEP).

The operator is a separate Go module, scaffolded with
[Kubebuilder](https://book.kubebuilder.io/).

## Development

Run `make help` to list all targets. The common ones are:

- `make manifests generate` regenerates the CRD, RBAC, and DeepCopy code.
- `make test` runs the unit and envtest tests.
- `make lint` runs golangci-lint.
- `make build` builds the manager binary.
- `make kind` creates a kind cluster with OVN-Kubernetes installed by the
  operator, using the upstream `ovn-kube-ubuntu:master` image.
