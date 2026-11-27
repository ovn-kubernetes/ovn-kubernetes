# OVN-Kubernetes Security

## Protecting controller-owned annotations

OVN-Kubernetes uses Kubernetes annotations to record network allocations and
other controller-managed state. Its security approach is to restrict writes to
the annotations it owns to the identities responsible for maintaining them.

## Protected pod annotations

The pod admission webhook protects these controller-owned annotations:

- `k8s.ovn.org/pod-networks`: network allocation results.
- `k8s.ovn.org/dpu.connection-details`: connection details written by the host CNI.
- `k8s.ovn.org/dpu.connection-status`: connection status written by the DPU controller.

These components write the annotations after the pod exists.

The `k8s.ovn.org/pod-networks` annotation records controller-generated network
allocation results. Allowing a pod creator to supply those results would let the
caller claim an allocation instead of obtaining one through the network
controllers.

The OVN-Kubernetes pod admission webhook enforces these rules:

- Pod creation must not supply any of the protected annotations, even with an empty value.
  This also applies to workload templates and restored pod manifests.
- After creation, only authorized network identities may add, change, or remove
  these annotations, through either `pods` or `pods/status`.
- Other users may update a pod while retaining the protected annotations unchanged.

Static address requests and IPAM claim references remain supported through
network selection inputs. Controllers populate the allocation result afterward.
During KubeVirt migration, OVN-Kubernetes copies the source allocation to the
target pod after creation. This annotation protection does not authenticate
KubeVirt-owned identity or migration metadata.

These protections require the OVN-Kubernetes admission webhook to be enabled.
Custom webhook configurations must register `CREATE` and `UPDATE` operations for
`pods`, and `UPDATE` for `pods/status`. Updating the webhook binary alone does not
extend admission coverage. Self-pod exclusions must not bypass validation when
either the old or new pod contains a protected annotation.

The checks apply to admission requests; they do not retroactively validate
annotations on existing pods.

## Per-node security

[Node identity](node-identity.md) gives each `ovnkube-node` a distinct client
certificate and restricts its annotation writes to its own node and pods hosted
there. Its admission checks also limit which annotations that identity may
modify and validate selected annotation values. See the node identity document
for certificate provisioning, webhook deployment, and per-node restrictions.

### Pod network entries written by a node

`k8s.ovn.org/pod-networks` holds one entry per network the pod is attached to.
An `ovnkube-node` identity has to write this annotation for pods on its node,
but it is not the allocator of every network. Other zones and cluster manager
act on these entries, so a compromised node must not be able to claim addresses
or tunnel keys of pods on other nodes.

The pod admission webhook therefore checks every entry that an `ovnkube-node`
identity adds, changes, or removes, according to who allocates on that network:

| Network of the entry | Allocator | Rule for `ovnkube-node` |
|----------------------|-----------|-------------------------|
| Default network | Node | IPs must belong to the node's subnet |
| Layer3 user defined network | Node | IPs must belong to the node's subnet of that network |
| Layer2 user defined network | Cluster manager | The entry cannot be added, changed, or removed |
| Localnet network with subnets | Cluster manager | The entry cannot be added, changed, or removed |
| Localnet network without subnets or with DHCP IPAM | Node | Not validated, there is no subnet to check against |

Entries that a request leaves unchanged are not checked, so a node can still
set the default network entry of a pod whose layer2 entry was written by
cluster manager. The node subnet checks are skipped for KubeVirt live
migratable pods, whose IPs can cross node subnets. The layer2 and localnet
rules still apply to them. An entry that references a
NetworkAttachmentDefinition that does not exist is rejected.

Cluster manager and other `--extra-allowed-user` identities are not subject to
the per-network rules.

The webhook reads the network topology from NetworkAttachmentDefinitions. Start
`ovnkube-identity` with `--enable-multi-network` on clusters that run
OVN-Kubernetes with multi-network or network segmentation enabled; the Helm
chart does this from `global.enableMultiNetwork` and
`global.enableNetworkSegmentation`. Without the flag the entries of user
defined networks are not validated. `ovnkube-identity` needs `get`, `list`, and
`watch` permissions on NetworkAttachmentDefinitions.

These checks do not validate the MAC address, routes, or gateways of an entry.
