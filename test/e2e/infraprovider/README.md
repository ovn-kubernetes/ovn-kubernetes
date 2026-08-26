# Infra Provider
Infra Provider provides test dependencies using an infrastructure agnostic API.

## Motivation
Previous to this API, our tests relied directly on upstream "KinD" to provision networks and launch external hosts.
This prevented downstream consumption of upstream tests.

## Description
Providers external to the cluster resources including adding external hosts [1] and provisioning networks,
attaching networks, etc.

[1] deployed as containers on KinD provider but may be deployed as host-networked container downstream.

Known implementations:

- KinD
- OpenShift
- SSH (remote KinD)

## SSH provider

Runs Docker or Podman commands on an SSH host using the shared container engine.
Only remote KinD is supported: cluster nodes must be containers on that host.
This is not a per-node SSH provider for VM, bare-metal or cloud clusters.

The test machine needs a kubeconfig with a reachable API endpoint and a `kind-*`
current context. The SSH user needs key-based authentication and permission to
run the selected runtime without an interactive password.

| Variable | Default | Purpose |
|----------|---------|---------|
| `OVN_TEST_INFRA_PROVIDER` | `kind` | Set to `ssh` |
| `OVN_TEST_SSH_HOST` | Required | Runtime host IP or hostname, without a port |
| `OVN_TEST_SSH_USER` | Required | SSH user |
| `OVN_TEST_SSH_KEY` | Required | Private key path (`~` supported) |
| `OVN_TEST_SSH_PORT` | `22` | SSH port |
| `OVN_TEST_SSH_KNOWN_HOSTS` | `~/.ssh/known_hosts` | File containing the trusted host key |
| `OVN_TEST_SSH_INSECURE_HOST_KEY` | Off | Set to `1` to disable verification for ephemeral testing |
| `OVN_TEST_PRIMARY_NETWORK` | `kind` | Primary container network |
| `CONTAINER_RUNTIME` | `docker` | `docker` or `podman` on the SSH host |

For an existing remote OVN-Kubernetes KinD cluster, run from the repository root:

```bash
export KUBECONFIG=~/.kube/ovn-remote.conf
kubectl config use-context kind-ovn
export OVN_TEST_INFRA_PROVIDER=ssh
export OVN_TEST_SSH_HOST=192.0.2.10
export OVN_TEST_SSH_USER=ovnci
export OVN_TEST_SSH_KEY=~/.ssh/id_ed25519
export OVN_TEST_SSH_KNOWN_HOSTS=~/.ssh/known_hosts
make -C test control-plane WHAT="Pod to external server PMTUD.*TCP"
```

Host-key verification is required unless explicitly disabled. The legacy
four-argument `runner.NewSSHRunner` retains its insecure behavior; new callers
should use `runner.NewSSHRunnerWithOptions`.

Limitations:

- CI exercises only the PMTUD TCP spec above, using SSH to localhost rather than
  a separate host. Other specs are not covered by the SSH CI job.
- Image preload works only with a local SSH host (`localhost`, `127.0.0.1` or
  `::1`). For a remote host, required images must be in the KinD nodes'
  containerd stores or pullable by the nodes. On that host, use
  `kind load docker-image <image> --name ovn` to load images from its runtime;
  having them only in the host's Docker/Podman store is insufficient.
- Tests requiring `SetupUnderlay` are skipped; the SSH provider does not
  implement localnet OVS wiring.
- Direct runtime calls still run locally. Locally generated bind-mount sources
  are not copied to the SSH host (for example, FRR configs in route-advertisement
  tests).
