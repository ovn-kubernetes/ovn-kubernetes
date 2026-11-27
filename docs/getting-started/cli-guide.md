# CLI Guide

This guide describes the OVN-Kubernetes command-line tools: how to build
them, how configuration is resolved, and the most common modes and
options. For deeper flag detail, run each binary with `--help`, or consult
the troff man pages checked in under `docs/installation/` in the
repository.

In a typical cluster deployment (Helm, KinD, kubeadm), these binaries
already run inside the `ovnkube-control-plane` and `ovnkube-node` pods.
You mainly need this guide when building from source, debugging a
deployment, or understanding how the containers are started.

## Binaries

Build all tools from `go-controller/`:

```bash
cd go-controller
make clean
make
```

Executables land in `go-controller/_output/go/bin/`. The important ones:

| Binary | Role |
|--------|------|
| `ovnkube` | Main process — runs as cluster-manager, ovnkube-controller, and/or ovnkube-node |
| `ovn-k8s-cni-overlay` | CNI plugin invoked by the container runtime for pod ADD/DEL |
| `ovn-kube-util` | Helper commands (NIC/bridge conversion, readiness probes, OVS metrics exporter) |
| `ovnkube-trace` | Packet path tracing through OVN and OVS |
| `ovnkube-identity` | Certificate-based node identity and admission webhook |
| `ovnkube-observ` | Observability / flow sampling helper |
| `hybrid-overlay-node` | Hybrid overlay agent for mixed Windows/Linux VXLAN clusters |

See [Architecture](../design/architecture.md) for how `ovnkube` maps onto
the control-plane and node pods.

## Configuration precedence

OVN-Kubernetes resolves settings in this order (highest wins):

1. Command-line flags
2. Configuration file (`--config-file`, default `/etc/openvswitch/ovn_k8s.conf`)
3. Environment variables
4. Kubernetes service-account files (CA cert and token)
5. Built-in defaults

Exceptions:

- A flag that still equals its saved built-in default is treated as
  unset and does **not** override a value from the config file (or a
  lower layer). For example, with `mtu=1450` in the file, passing
  `--mtu=1400` leaves MTU at `1450` because `1400` is the code default.
  Non-default flag values still override. To restore a built-in default
  when the file sets something else, change or remove that key in the
  file.
- `--cnilogfile` is different. The flag's CLI default
  (`/var/log/ovn-kubernetes/ovn-k8s-cni-overlay.log`) is not the same
  as the saved logging default (empty), so omitting the flag still
  applies that path and **replaces** any `cnilogfile` set in the config
  file. Pass `--cnilogfile=` (empty) to keep the config-file path. To
  disable shim file logging, pass `--cnilogfile=` and leave
  `cnilogfile` unset in the file.

Kubernetes API access can therefore come from `--k8s-kubeconfig`, from
`--k8s-apiserver` / `--k8s-cacert` / `--k8s-token`, from `KUBECONFIG` /
`K8S_*` environment variables, or from the in-pod service-account mount
at `/var/run/secrets/kubernetes.io/serviceaccount/`.

## `ovnkube`

`ovnkube` is a single binary that selects its role from init flags. You
must pass at least one mode. Allowed combinations:

| Mode flag | Component | Notes |
|-----------|-----------|-------|
| `--init-cluster-manager <name>` | Cluster manager | Watches nodes; allocates per-node subnets. Does **not** connect to OVN DBs. |
| `--init-ovnkube-controller <name>` | ovnkube-controller | Watches pods/services/policies and programs OVN Northbound DB. |
| `--init-node <name>` | ovnkube-node | Programs OVS, gateway, host networking, and serves CNI. |
| `--cleanup-node <name>` | Node cleanup | Tears down OVS resources after the node DaemonSet is removed. Cannot be combined with other modes. |

`<name>` must be the Kubernetes node name (the same value `kubectl get
nodes` shows). When combining modes in one process, every init flag must
use the **same** identity.

Supported multi-mode combination:

- `--init-ovnkube-controller` **with** `--init-node` (common in
  `ovnkube-node` pods)

Not supported in one process:

- Cluster manager with ovnkube-controller (controllers run co-located
  with the node today)
- Cluster manager with node-only mode
- Cleanup with any other mode

List every flag category with:

```bash
ovnkube --help
```

Help groups flags into Generic, CNI, Feature, Kubernetes, OVN North/South,
Gateway, Cluster Manager, Node, Monitoring, Metrics, TLS, Egress IP
health-check, and Hybrid Overlay options.

### Commonly used flags

| Flag | Purpose | Default (code) |
|------|---------|----------------|
| `--config-file` | Path to the INI config file | `/etc/openvswitch/ovn_k8s.conf` |
| `--cluster-subnets` | Comma-separated cluster pod networks as `IP/prefix/hostsubnet-prefix` | `10.128.0.0/14/23` |
| `--k8s-service-cidrs` | Service ClusterIP CIDR(s); must match kube-apiserver `--service-cluster-ip-range` | `172.16.1.0/24` |
| `--gateway-mode` | Node north-south gateway: `shared` or `local`. If omitted, gateway functionality is disabled at the binary level. | *(unset / disabled)* |
| `--gateway-interface` | Uplink used in shared mode; defaults to the interface that owns the default route | |
| `--nodeport` | Enable NodePort service support on gateways | |
| `--mtu` | Overlay MTU | `1400` |
| `--conntrack-zone` | Conntrack zone for gateway conntrack flows | `64000` |
| `--encap-type` | Overlay encapsulation (`geneve`, `vxlan`, `gre`) | `geneve` |
| `--loglevel` | klog verbosity (info/warn/error/fatal always print; use `5` for debug) | `4` |
| `--logfile` | Optional file for ovnkube logs | |
| `--cnilogfile` | CNI shim log path; must be under `/var/log/ovn-kubernetes` | `/var/log/ovn-kubernetes/ovn-k8s-cni-overlay.log` |
| `--cni-conf-dir` | Directory for the written CNI config | `/etc/cni/net.d` |
| `--cni-plugin` | CNI plugin name written into that config | `ovn-k8s-cni-overlay` |
| `--metrics-bind-address` | Address:port for the metrics HTTP server | |
| `--ovnkube-node-mode` | Node operating mode: `full`, `dpu`, or `dpu-host` | `full` |
| `--unprivileged-mode` | Run node without `SYS_ADMIN`; CNI shim creates the veth (only with `--init-node`) | |
| `--enable-multicast` | Enable multicast support (ovnkube-controller) | |
| `--pidfile` | Optional PID file path | |

`--k8s-service-cidr` and `--service-cluster-ip-range` remain accepted as
deprecated aliases of `--k8s-service-cidrs`.

`--cluster-subnets` entries use
`[IP address/prefix-length/hostsubnet-prefix-length]`. The hostsubnet
prefix selects how large a slice each node receives. For IPv4 it
defaults to `/24` if omitted; for IPv6 it must be `/64`. Entries must
not overlap.

Gateway mode details are in [Gateway Modes](../design/gateway-modes.md).
Shared gateway is the default in standard Helm/KinD manifests even
though omitting the flag disables gateway setup at the raw CLI level.

### Environment variables

| Variable | Overridden by |
|----------|---------------|
| `KUBECONFIG` | `--k8s-kubeconfig` |
| `K8S_APISERVER` | `--k8s-apiserver` |
| `K8S_CACERT` | `--k8s-cacert` |
| `K8S_TOKEN` | `--k8s-token` |

### Example: cluster manager

```bash
ovnkube --init-cluster-manager <node-name> \
  --k8s-cacert </path/to/ca.crt> \
  --k8s-token <token> \
  --k8s-apiserver https://<api-host>:6443 \
  --cluster-subnets 10.128.0.0/14/23 \
  --k8s-service-cidrs 172.16.1.0/24
```

The cluster manager watches nodes and allocates host subnets (and related
join/transit addresses) from the configured cluster ranges.

### Example: ovnkube-controller

```bash
ovnkube --init-ovnkube-controller <node-name> \
  --k8s-cacert </path/to/ca.crt> \
  --k8s-token <token> \
  --k8s-apiserver https://<api-host>:6443 \
  --cluster-subnets 10.128.0.0/14/23 \
  --k8s-service-cidrs 172.16.1.0/24
```

The controller initializes OVN logical topology for its zone and watches
nodes, pods, services/endpoints, NetworkPolicies, and related resources.

### Example: node (with gateway)

Install the CNI binary first (`make install` from `go-controller/`, or
copy `ovn-k8s-cni-overlay` into the node CNI bin directory, commonly
`/opt/cni/bin`).

```bash
ovnkube --init-node <node-name> \
  --k8s-cacert </path/to/ca.crt> \
  --k8s-token <token> \
  --k8s-apiserver https://<api-host>:6443 \
  --gateway-mode shared \
  --nodeport
```

Combined controller + node (as used by many DaemonSet entrypoints):

```bash
ovnkube \
  --init-ovnkube-controller <node-name> \
  --init-node <node-name> \
  --gateway-mode shared \
  --nodeport \
  ...
```

### Example: clean up a node

After removing the ovnkube-node DaemonSet from a host:

```bash
ovnkube --cleanup-node <node-name> --gateway-mode shared
```

## Configuration file

The default path is `/etc/openvswitch/ovn_k8s.conf`. Override it with
`--config-file`. The file uses INI-style `key=value` sections. A sample
shipped with the tree is `go-controller/etc/ovn_k8s.conf`. The man page
source `docs/installation/ovn_k8s.conf.5` documents the sections in more
detail.

Example:

```ini
[default]
mtu=1400
conntrack-zone=64000
cluster-subnets=10.128.0.0/14/23

[logging]
loglevel=4
logfile=/var/log/ovnkube.log
cnilogfile=/var/log/ovn-kubernetes/ovn-k8s-cni-overlay.log

[cni]
conf-dir=/etc/cni/net.d
plugin=ovn-k8s-cni-overlay

[kubernetes]
apiserver=https://1.2.3.4:6443
cacert=/etc/kubernetes/ca.crt
token=
service-cidrs=172.16.1.0/24

[gateway]
mode=shared
nodeport=true

[ovnnorth]
run-dir=/var/run/ovn/
db-location=/etc/ovn/ovnnb_db.db

[ovnsouth]
run-dir=/var/run/ovn/
db-location=/etc/ovn/ovnsb_db.db

[egressip-healthcheck-tls]
client-privkey=/path/to/client.key
client-cert=/path/to/client.crt
client-cacert=/path/to/client-ca.crt
cert-common-name=egress-health-check.example.com

[ovnkubenode]
# Optional; see ovn_k8s.conf(5) for routing-table-id-start and
# kubelet-cgroup-path used by UDN / EgressIP / probe isolation.
```

Notes:

- Section and key names are case-insensitive in practice (`[Default]` and
  `[default]` both work).
- OVN Northbound and Southbound connections use local Unix sockets under
  `run-dir` in interconnect deployments.
- Egress IP health-check TLS options are also accepted under `[ovnnorth]`
  for compatibility; `[egressip-healthcheck-tls]` wins if both are set.
- `cnilogfile` must resolve to a path under `/var/log/ovn-kubernetes`.
  Because omitting `--cnilogfile` applies the flag default path, use
  `--cnilogfile=` to honor the config-file value, and use
  `--cnilogfile=` with `cnilogfile` unset in the file to disable shim
  file logging. See also `docs/installation/ovn_k8s.conf.5`.

Additional knobs (disable-forwarding, feature gates, and more) are
covered in the [Configuration Guide](configuration.md).

## `ovn-k8s-cni-overlay`

This is the CNI plugin binary. The kubelet / CRI (often via Multus)
invokes it for each pod network attachment. It is a thin shim that
talks to the CNI server inside ovnkube-node over a Unix socket. Place
it in the node CNI bin directory before starting kubelet with
OVN-Kubernetes as the network plugin.

## `ovn-kube-util`

Utility CLI used by container entrypoints and operators:

```text
ovn-kube-util [global options] COMMAND [command options] [arguments...]
```

| Command | Purpose |
|---------|---------|
| `nics-to-bridge <nics...>` | Create an OVS bridge for each NIC and move addressing onto the bridge |
| `bridges-to-nic <bridges...>` | Delete the OVS bridge and restore IP/routes onto the underlying NIC |
| `readiness-probe --target <name>` | Check readiness of a daemon used by the deployment |
| `ovs-exporter` | Export OVS metrics (`--metrics-bind-address`, default `0.0.0.0:9310`) |

`readiness-probe` targets:

- `ovn-controller`
- `ovnnb-db`
- `ovnsb-db`
- `ovn-northd`
- `ovs-daemons`
- `ovnkube-node`

Example:

```bash
ovn-kube-util readiness-probe --target ovn-controller
```

## `ovnkube-trace`

Traces simulated UDP/TCP traffic between pods (or to an external IP /
Service) using `ovn-trace`, `ovs-appctl ofproto/trace`, and optionally
`ovn-detrace`. Full usage and examples live in
[ovnkube-trace](../troubleshooting/ovnkube-trace.md).

```bash
ovnkube-trace \
  -src-namespace default \
  -src <src-pod> \
  -dst-namespace default \
  -dst <dst-pod> \
  -udp -dst-port 53 \
  -loglevel 0
```

## Other binaries

- **`ovnkube-identity`** — runs on control-plane nodes and provides
  certificate-based node identity / admission. See
  [Node Identity](../features/infrastructure-security-controls/node-identity.md).
- **`ovnkube-observ`** — observability helper for flow sampling; see
  [Observability](../observability/index.md).
- **`hybrid-overlay-node`** — VXLAN hybrid overlay dataplane for mixed
  Windows/Linux clusters. See [Hybrid Overlay](../features/hybrid-overlay.md).

## Using the tools in a running cluster

Container images already contain these binaries. Useful patterns:

```bash
# List ovn-kubernetes pods
kubectl -n ovn-kubernetes get pods -o wide

# Inspect how ovnkube was started
kubectl -n ovn-kubernetes get pod <ovnkube-node-pod> \
  -o jsonpath='{.spec.containers[*].command}{"\n"}{.spec.containers[*].args}{"\n"}'

# Run ovn-nbctl / ovn-sbctl against the local zone DB
kubectl -n ovn-kubernetes exec -it <ovnkube-node-pod> -c nbdb -- ovn-nbctl show
kubectl -n ovn-kubernetes exec -it <ovnkube-node-pod> -c sbdb -- ovn-sbctl show

# Trace connectivity from a host that has kubeconfig and the binary
ovnkube-trace -src <pod-a> -dst <pod-b> -tcp -dst-port 80 -loglevel 2
```

For install-time and runtime checks (unique `system-id`, chassis
registration, overlay ports, and more), see
[Debugging](../troubleshooting/debugging.md).

## Man pages

Troff man page sources live next to the install docs in the repository.
Prefer `ovnkube --help` and this guide when they disagree with older
pages (for example, `--remove-node` appears in older man text but is no
longer a supported `ovnkube` mode; use `--cleanup-node` instead):

| File | Topic |
|------|-------|
| `docs/installation/ovnkube.1` | Main binary options |
| `docs/installation/ovn-kube-util.1` | Utility commands |
| `docs/installation/ovn_k8s.conf.5` | Configuration file |

## Related docs

- [Configuration Guide](configuration.md)
- [Architecture](../design/architecture.md)
- [Gateway Modes](../design/gateway-modes.md)
- [Launching on KinD](../installation/launching-ovn-kubernetes-on-kind.md)
- [Launching with Helm](../installation/launching-ovn-kubernetes-with-helm.md)
- [ovnkube-trace](../troubleshooting/ovnkube-trace.md)
