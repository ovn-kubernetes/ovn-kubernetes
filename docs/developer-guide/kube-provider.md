# Running the e2e suite against an existing cluster

The `kube` infra provider runs the e2e tests against a cluster that already exists and already
runs OVN-Kubernetes. It reaches that cluster through `KUBECONFIG` instead of building a KinD
cluster first.

KinD stays the default. If you do not set `OVN_TEST_INFRA_PROVIDER`, nothing changes.

## Running it

You need:

- a cluster running OVN-Kubernetes
- a kubeconfig with cluster-admin
- `kubectl` on `PATH`, which is how the suite runs commands on nodes

```bash
export KUBECONFIG=/path/to/kubeconfig
export OVN_TEST_INFRA_PROVIDER=kube

cd test/e2e
go test -test.timeout 60m -v . \
    -ginkgo.v \
    -ginkgo.fail-on-empty \
    -ginkgo.focus "Network Segmentation: API validations" \
    -provider skeleton \
    -kubeconfig "${KUBECONFIG}"
```

Start with that focus. Those specs only create resources and check that the API rejects bad ones,
so they never touch a node. Try `Services` next, then widen as specs come back clean.

Always pass `-ginkgo.fail-on-empty`. Without it, a focus matching no specs exits `SUCCESS` with
everything skipped, which looks like a passing run.

## If your cluster is not laid out like upstream

The suite reads where OVN-K lives from the cluster rather than asking you: the namespace from the
`app=ovnkube-node` pods, and the gateway bridge and its uplink from OVS on one of those nodes.
Set a variable when what it reads is wrong, or when your cluster has more than one answer.

| Variable | If you leave it unset | What it is |
| --- | --- | --- |
| `OVN_TEST_OVNK_NAMESPACE` | namespace of the `app=ovnkube-node` pods | Namespace OVN-K runs in |
| `OVN_TEST_FRRK8S_NAMESPACE` | namespace of the `app=frr-k8s` pods, or `frr-k8s-system` if there are none | Namespace FRR-K8s runs in |
| `OVN_TEST_EXTERNAL_BRIDGE` | the bridge OVN-K recorded an uplink on | OVS gateway bridge |
| `OVN_TEST_PRIMARY_INTERFACE` | the uplink recorded on that bridge | Uplink OVN-K owns |
| `OVN_TEST_PRIMARY_IP_POOL` | derived from the Node subnets | CIDR to allocate test addresses from |

The bridge and the uplink are read together, from the `bridge-uplink` external-id that OVN-K
writes on the bridge it moved the uplink onto. That is the same thing OVN-K reads back to find
its own uplink. If no bridge carries it, which happens when the bridge was provisioned before
OVN-K rather than by it, or if more than one does, the specs that need the uplink skip and name
both variables. They skip rather than guess because several of them run `ip addr add` against this
name, and a wrong name means they configure nothing and fail later for no visible reason.

Both are one value for the whole cluster. Uplink-dependent specs may use any schedulable Node, so
clusters whose Nodes use different bridge-uplink pairs are unsupported.

Naming the namespace also narrows where the suite looks for those pods, so it is the way to pick
between two OVN-K installs on one cluster.

### When your Nodes do not share a subnet

Some specs need a spare address on the Node network, usually to assign as an egress IP. By
default the suite takes one from the subnet that every Node is on.

That works when the Nodes sit on one L2. It does not work when each Node has its own routed link:
disjoint subnets have no address in common, and a `/31` has no spare address in it. On a cluster
like that, those specs skip and the rest of the run is unaffected.

Set `OVN_TEST_PRIMARY_IP_POOL` to hand them a range instead. It has to be routable to the cluster
and free for the suite to use:

- `10.100.0.0/24` for a single family
- `10.100.0.0/24,fd00:10:100::/64` for dual stack, one entry per family

Specs asking for a family you did not give will skip. Allocation starts at the first usable
address after the one you write: last-octet `0` and `1` are skipped, and so is the IPv4
broadcast, so `10.100.0.0/24` begins at `10.100.0.2`. It stops at the end of the CIDR, so
`10.100.0.128/24` uses the top half of that `/24`. Every parallel Ginkgo process starts from
the same address, so run a single process.

## Specs that need a container outside the cluster

Some specs need a container that is not part of the cluster: an external gateway, a BGP peer, or
an off-cluster HTTP server. KinD runs these on the same
container runtime it built the cluster with. Here you have to say where they can run.

| Variable | Default | What it is |
| --- | --- | --- |
| `OVN_TEST_CONTAINER_HOST` | unset, so those specs skip | Host whose container runtime the suite may use |
| `OVN_TEST_PRIMARY_NETWORK` | none | Network on that host that reaches the Nodes |
| `OVN_TEST_CONTAINER_HOST_USER` | `root` | SSH user |
| `OVN_TEST_CONTAINER_HOST_PORT` | `22` | SSH port |
| `OVN_TEST_CONTAINER_HOST_KEY` | none | Private key file for that user; required when the host is set |
| `CONTAINER_RUNTIME` | `docker` | `docker` or `podman`, as for KinD |

The host has to sit next to the Nodes on the network, not just be able to reach the API server.
The specs route traffic through these containers and expect the Nodes to answer on the same L2.
If your Nodes are VMs, their hypervisor is usually the host you want.

`OVN_TEST_PRIMARY_NETWORK` is the network on that host that the containers attach to. It is the
equivalent of KinD's `kind` network.

The suite checks one part of this: if the Nodes hold no address inside
`OVN_TEST_PRIMARY_NETWORK`, it tells you the variable is wrong. It cannot check the rest, so a
host that is reachable but not adjacent shows up as failing specs.

Specs that call the provider's generic network-attachment operation skip. That API does not say
whether its target is a Node or an external container, and this provider cannot safely attach an
interface to a Node. Reading a Node's existing interface does work, but only for
`OVN_TEST_PRIMARY_NETWORK`.

## Reading the results

Skips are the normal outcome here. This provider cannot power nodes off, cannot treat a Node as a
container, and cannot run external containers unless you give it a host. Each skip tells you
which case it hit:

- something this provider will never do names the operation
- something you have not configured yet names the variable that would enable it

If the run fails in `BeforeSuite` with `k8s.ovn.org/node-primary-ifaddr annotation not found`,
OVN-K is not healthy on the cluster and no focus will get past it. Check the CNI, not the
provider.

## Before you widen the focus

**Some specs touch your nodes before they skip.** The egress IP specs restore kubelet and
iptables state on every node during setup, then ask for something the provider cannot do. They
are reported as skipped, but they have already run against your cluster.

**Specs that restart the kubelet cannot recover.** Node commands go over `kubectl exec`, which
the kubelet proxies. Restarting or reconfiguring the kubelet cuts that path, and the rollback
needs the same path to undo it. Under KinD these ran over `docker exec`, which does not depend on
the kubelet. Keep them out of your focus:

- the node IP and MAC migration specs
- the kubelet restart in the network segmentation specs
