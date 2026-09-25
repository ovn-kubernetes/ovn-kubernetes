# Running e2e tests against an existing cluster

The `kube` infrastructure provider runs the existing Go e2e suite against a
cluster already running OVN-Kubernetes. It does not create or destroy it.

Kind remains the default provider. To select an existing cluster:

```console
cd test/e2e
export KUBECONFIG=/path/to/kubeconfig
export OVN_TEST_INFRA_PROVIDER=kube

go test -timeout=60m -v . \
  -ginkgo.v \
  -ginkgo.fail-on-empty \
  -ginkgo.focus='<focus>'
```

Some scopes read `KUBECONFIG` directly, so set it even when passing the
framework's `-kubeconfig` flag. The credentials must be able to manage the
resources created by the selected specs. Except for DPU uplink runs, suite
startup requires access to list Nodes and valid OVN-Kubernetes
primary-interface annotations. When the Nodes have no shared range, specs that
need an address from it skip. Node commands require get, list, create, and
delete access to Pods plus create access to `pods/exec` in the OVN-Kubernetes
namespace. The cluster must admit a privileged Pod using host PID, host network,
and a host-root `hostPath` volume.

## Dependencies

Core OVN-Kubernetes tests require Kubernetes and a healthy
OVN-Kubernetes CNI installation. Provider operations that require external
containers, infrastructure networks, Node lifecycle control, or spare underlay
interfaces skip because this provider does not manage them.

The existing cluster must be able to pull the suite's test images, or have them
preloaded or mirrored. Unlike the Kind provider, the Kubernetes provider does
not preload images into the Nodes.

| Variable | Behavior when unset |
| --- | --- |
| `OVN_TEST_OVNK_NAMESPACE` | `ovn-kubernetes` |
| `OVN_TEST_FRRK8S_NAMESPACE` | `frr-k8s-system` |
| `OVN_TEST_EXTERNAL_BRIDGE` | Required by specs that use the gateway bridge |
| `OVN_TEST_PRIMARY_INTERFACE` | Required by specs that use the primary interface |
| `OVN_TEST_NBDB_CONTAINER` | `nb-ovsdb` |
| `OVN_TEST_L3_UDN_MULTI_SUBNET` | Disabled; set to `true` when the deployment supports it |

## Node commands

The provider executes Node commands through one privileged Pod per Node. Each
Pod uses the host PID and network namespaces and chroots into the host. The
image is borrowed from the OVN-Kubernetes Node Pod already running there, and
the shell Pods are deleted after the suite. Node commands invoke a local
`kubectl` executable, which must be on `PATH`.

This path depends on the kubelet. The provider therefore refuses
`systemctl stop kubelet.service`. Tests that restart or reconfigure the kubelet
can still break their own exec and cleanup path.

## Remaining portability limits

Known cases that are not portable through the current provider interface
include:

- localnet tests that call `SetupUnderlay` skip because the generic provider
  does not mutate OVS bridges or take ownership of host interfaces;
- the static Pod test invokes local `docker cp` and `docker exec` directly;
- Node IP and MAC migration code invokes local `docker exec` directly;
- some service and network tests use local `sudo`, network namespaces, routes,
  and iptables on the test runner;
- KubeVirt scopes execute `/tmp/virtctl`; when absent, the suite downloads a
  Linux/amd64 binary there, requiring a writable `/tmp`, outbound access, and a
  compatible test runner unless a suitable binary is already cached;
- the Node shutdown/startup scenario explicitly selects only the Kind provider;
- tests that assume systemd manages `kubelet.service` do not work on every
  Kubernetes distribution;
- the default report path is still `/tmp/kind/logs`, although it can be
  overridden with `-report-path`.
