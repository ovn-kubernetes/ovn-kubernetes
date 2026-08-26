# Infra Provider
Infra Provider provides test dependencies using an infrastructure agnostic API.

## Motivation
Previous to this API, our tests relied directly on upstream "KinD" to provision networks and launch external hosts.
This prevented downstream consumption of upstream tests.

## Description
Providers manage resources external to the cluster, including adding external hosts [1], provisioning networks,
and attaching networks.

[1] External hosts are deployed as containers by the KinD provider but may be deployed as host-networked containers downstream.

Known implementations:

- KinD
- OpenShift

## Remote container runtime over SSH

The KinD provider takes a container runtime and an `api.Runner`, so the same
provider serves a cluster whose node containers run on this machine and one
whose runtime is on another host. `OVN_TEST_INFRA_PROVIDER=ssh` selects an SSH
runner; everything the provider does with the runtime then happens on the SSH
host.

Only remote KinD is supported: cluster nodes must be containers on that host.
This is not a per-node SSH provider for VM, bare-metal or cloud clusters.

The test machine needs a kubeconfig with a reachable API endpoint and a `kind-*`
current context. The SSH host needs the `kind` binary and the selected runtime,
and the SSH user needs key-based authentication and permission to run both
without an interactive password.

| Variable | Default | Purpose |
|----------|---------|---------|
| `OVN_TEST_INFRA_PROVIDER` | `kind` | Set to `ssh` |
| `OVN_TEST_SSH_HOST` | Required | Runtime host IP or hostname, without a port |
| `OVN_TEST_SSH_USER` | Required | SSH user |
| `OVN_TEST_SSH_KEY` | Required | Private key path (`~` supported) |
| `OVN_TEST_SSH_PORT` | `22` | SSH port |
| `OVN_TEST_SSH_KNOWN_HOSTS` | `~/.ssh/known_hosts` | File containing the trusted host key (`~` supported) |
| `OVN_TEST_SSH_INSECURE_HOST_KEY` | Off | Set to `1` to disable verification for ephemeral testing |
| `CONTAINER_RUNTIME` | `docker` | `docker` or `podman` holding the node containers |

For an existing remote OVN-Kubernetes KinD cluster, run from the repository root:

```bash
export KUBECONFIG=~/.kube/ovn-remote.conf
kubectl config use-context kind-ovn
export OVN_TEST_INFRA_PROVIDER=ssh
export OVN_TEST_SSH_HOST=192.0.2.10
export OVN_TEST_SSH_USER=ovnci
export OVN_TEST_SSH_KEY=~/.ssh/id_ed25519
export OVN_TEST_SSH_KNOWN_HOSTS=~/.ssh/known_hosts
make -C test control-plane WHAT="Pod to external server PMTUD.*TCP"   # or omit WHAT for the whole suite
```

Host-key verification is required unless explicitly disabled. The legacy
four-argument `runner.NewSSHRunner` retains its insecure behavior; new callers
should use `runner.NewSSHRunnerFromConfig` or `runner.NewSSHRunnerWithOptions`.

Limitations:

- CI runs the full control-plane suite over SSH, as one entry in the e2e matrix,
  but it SSHes to the runner itself rather than to a separate host. Reaching a
  genuinely remote runtime is therefore not covered by CI.
- Image preload runs `<runtime> pull` and `kind load` on the SSH host, so both
  must be installed there. Images already present only in the local runtime's
  store are not transferred.
- Locally generated bind-mount sources are not copied to the SSH host (for
  example, FRR configs in route-advertisement tests).
