# OVN-Kubernetes Operator

Installs and manages OVN-Kubernetes through a single `OVNK8sOperator` custom
resource. The design is described in the OVN-Kubernetes operator enhancement proposal (OKEP).

The operator is a separate Go module, scaffolded with
[Kubebuilder](https://book.kubebuilder.io/).

## Quick install

### On kind

`make kind` creates a kind cluster with upstream images, installs OVS and the
operator, and creates a CR for the cluster. It needs the same tools as
`contrib/kind-helm.sh`. `hack/kind.sh --test-uninstall` also deletes the CR
afterwards and checks that the operator cleaned up.

### On an existing cluster

For a Kubernetes cluster you created yourself, for example with kubeadm,
without a CNI installed:

1. Make sure every node runs OVS, for example the upstream `ovs-node`
   DaemonSet from the Helm chart. The operator runs on the control-plane host
   network and reaches the API server at `https://127.0.0.1:6443`; change
   `KUBE_API_SERVER` in `config/manager/manager.yaml` if yours differs.
2. Build and push the operator image, then install the operator:

   ```sh
   make docker-build docker-push IMG=<registry>/ovnk8s-operator:<tag>
   make deploy IMG=<registry>/ovnk8s-operator:<tag>
   ```

3. Edit `config/samples/k8s_v1alpha1_ovnk8soperator.yaml` for your cluster
   (`k8sAPIServer` must be reachable from every node, plus the pod and service
   networks and MTU), then create it:

   ```sh
   kubectl apply -f config/samples/k8s_v1alpha1_ovnk8soperator.yaml
   ```

4. Wait for the installation to complete:

   ```sh
   kubectl -n ovnk8s-operator-system wait ovnk8soperator/default \
     --for=condition=UpgradeComplete --timeout=10m
   kubectl -n ovn-kubernetes get pods
   ```

To uninstall, delete the CR; the operator removes everything it deployed,
including the OVN-Kubernetes CRDs and all resources created from them. Then
`make undeploy` removes the operator and the namespaces its install manifests
created.

## TODO

- Deploy OVS (`ovs-node`) with the operator.
- The `dpu-host` and `dpu` deployment modes.
- Keep the OVN-Kubernetes CRDs on uninstall.
- Publish the operator image and a Helm chart for it.

## Development

Run `make help` to list all targets. The common ones are:

- `make manifests generate` regenerates the CRD, RBAC, and DeepCopy code.
- `make test` runs the unit and envtest tests.
- `make lint` runs golangci-lint.
- `make build` builds the manager binary.
- `make kind` creates a kind cluster with OVN-Kubernetes installed by the
  operator, using the upstream `ovn-kube-ubuntu:master` image.
