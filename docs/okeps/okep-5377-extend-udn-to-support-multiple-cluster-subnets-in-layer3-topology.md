# OKEP-5377: Extend Primary UDN/CUDN to Support Multiple Cluster Subnets in Layer3 Topology

* Issue: [#5377](https://github.com/ovn-org/ovn-kubernetes/issues/5377)

## Problem Statement

UDN currently supports only one cluster subnet per IP family in Layer3 topology. This limitation
reduces flexibility in IP planning for multi-tenant environments. When IP overlapping is not allowed,
assigning a large CIDR to a tenant’s P-UDN may cause significant IP waste, especially for small
tenants that do not require many node subnets.

To address this, supporting multiple subnets per IP family is necessary for several reasons:
- **IP exhaustion:** A single CIDR may not be large enough to cover all nodes, especially in large-scale deployments.
- **Progressive growth:** Clusters may be deployed with limited IP space initially and need to grow
  incrementally over time without recreating the network.
- **Pod density variance:** Nodes with different resource capacities may need subnets of different
  sizes. Supporting multiple subnets enables better IP utilization by allocating appropriately sized
  subnets per node (based on node hints).
  _Note: Using different `hostSubnet` sizes is out of scope for this OKEP._

## Goals

* Support multiple cluster subnets per IP family in Layer3 topology for Primary UDN/CUDN.

## Non-Goals

* Supporting multiple subnets for Layer3 Secondary UDN/CUDN.
* Supporting multiple subnets in Layer2 or Localnet topologies.
* Supporting deletion of existing subnets.
* Supporting different `hostSubnet` size.

## Future-Goals

* Support subnets with different `hostSubnet` sizes.
  When the maximum number of Pods on a node is limited by hardware resources (e.g., GPUs), the subnet
  allocator can assign the smallest fitting subnet to the node by leveraging a node-specific hint.

## Introduction

When using UDN to define a network in Layer3 topology, each node gets a subnet allocated from the
UDN's `subnets` list per IP family. Currently, UDN limits `subnets` to a single subnet per IP
family, enforced by CRD validation and full immutability of the UDN spec.

In multi-tenant environments where IP overlapping is not allowed, each tenant’s Primary UDN should
use a small CIDR to avoid wasting IP space. As the cluster scales, operators need the ability to
append additional subnets to the Primary UDN to expand the available IP pool without disruption.

## User-Stories/Use-Cases

### Story 1: Expanding subnet pool for growing clusters

**As a** cluster operator,
**I want** to expand the available IP address pool for a Primary Layer3 network,
**so that** I can scale the cluster without redeploying or disrupting workloads.


## Proposed Solution

### API Details

1. Update `UDN`/`CUDN` CRD to allow `layer3.subnets` to accept multiple subnets per IP family.
2. Allow updates to the `layer3.subnets` field, but only to **append** new entries.
   - Reject updates that attempt to remove or modify existing subnet entries.
   - Reject new entries that overlap with existing subnets or use a different `hostSubnet`.
   - Allow adding subnets only to Primary UDN/CUDN.
   - IPv4 and IPv6 subnets can be added independently.


### Implementation Details

### Node Subnet Allocation

When a new subnet is appended to a UDN in Layer3 topology, the subnet allocator must preserve
existing node-to-subnet assignments to ensure network stability. Each node continues to use the
subnet it was originally allocated, and no reassignment or disruption occurs for existing nodes or
Pods.

Only newly added nodes (or nodes that do not yet have an assigned subnet) will allocate their
subnets from the newly added ranges. This ensures seamless expansion of the cluster’s IP space
without requiring controller restarts or reallocation of existing node subnets.

The node subnet allocator needs to ensure that:
- All previously allocated subnets remain valid.
- Newly added subnets are appended to the available pool for future allocations.

### OVN Network Topology

Adding new subnets to a Layer3 network simply extends its cluster subnet allocation pool. Each node
continues to allocate one subnet from the available pool. The overall OVN network topology remains
largely unchanged. However, one additional change is required in the routing policies of the OVN
Cluster Router: the `DefaultNoReroutePriority` (priority 102) policies.

Currently it only allows east-west traffic within the individual cluster subnet.

For example, for a network with cluster subnets `10.1.0.0/16` and `10.2.0.0/16`, the OVN cluster
router policy list includes:
```console
$ ovn-nbctl lr-policy-list  udn_udn.primary.layer3_ovn_cluster_router
Routing Policies
      ...
      102   ip4.src == 10.1.0.0/16 && ip4.dst == 10.1.0.0/16    allow
      102   ip4.src == 10.2.0.0/16 && ip4.dst == 10.2.0.0/16    allow
```

To enable east-west traffic between the subnets, the following additional policies must be added:
```console
$ ovn-nbctl lr-policy-list  udn_udn.primary.layer3_ovn_cluster_router
Routing Policies
      ...
      102   ip4.src == 10.1.0.0/16 && ip4.dst == 10.1.0.0/16    allow
      102   ip4.src == 10.1.0.0/16 && ip4.dst == 10.2.0.0/16    allow
      102   ip4.src == 10.2.0.0/16 && ip4.dst == 10.2.0.0/16    allow
      102   ip4.src == 10.2.0.0/16 && ip4.dst == 10.1.0.0/16    allow
```


### Feature Compatibility

#### ClusterNetworkConnect

When multiple UDNs are connected together via a `ClusterNetworkConnect` CR, a `connect-router` is created with
routes to steer traffic to the correct destination. For Layer3 networks, the connect-router maintains
per-node-subnet static routes (e.g., `103.103.0.0/24 -> 192.168.0.12`) to route traffic to the appropriate node.
Since each node only has one subnet per IP family (even when the cluster has multiple subnets), the static routes
on the connect-router do not need to be updated when a new cluster subnet is added.

However, the router policies on each connected network's `ovn_cluster_router` do need to be updated. These policies
match all connected network subnets and point to the connect-router as nexthop. When a new subnet is added to a
connected network, all other connected networks need to update their router policies to include the new subnet in the
match criteria (for the same IP family). For example, if green network (`104.104.0.0/16`) adds a new subnet
`104.105.0.0/16`, the router policies on blue network's `ovn_cluster_router` need to be updated to match both
subnets.

A unit test should be added to ensure that when a UDN is part of a `ClusterNetworkConnect` and a new subnet is
added, the router policies on all connected network cluster routers are updated with the correct match criteria
for the new subnet.

#### Egress Firewall

Whenever an IP block is provided as match criteria for Egress Firewall, we calculate if it overlaps with the current pod subnet.
If so, then we add an exclusion match criteria to the ACL to ensure east/west traffic is not affected by the firewall.
Today when a NAD changes, a handleNetworkEvent callback is made from the NAD Controller to the Egress Firewall controller.
This callback causes any Egress Firewall in the NAD namespace to be reconciled. This should force the ACLs to be regenerated
if the new subnet overlaps with the IP Block in the Egress Firewall.

A new e2e test or unit test should be added to pick an IP Block for an Egress Firewall that does not overlap. Then the UDN
is updated with a subnet that would overlap, and ensure that the Egress Firewall gets updated with the proper ACL.

#### BGP RouteAdvertisement (RA)

Whenever a BGP RA is configured to advertise the pod subnet, it generates an FRR-K8S configuration that includes the subnet.
When a NAD changes today, we reconcile all BGP RAs. However, in the reconciliation, we check raNeedsUpdate, which only checks
if the RA changed. We will need to fix this code and add a unit test to make sure that when an RA exists, with pod subnet
advertisement, that adding a subnet updates the FRR-K8S configuration with the new subnet.

#### BGP Route Importing

Whenever we enabled BGP RA, we also optionally import routes. This is done by the Route Import Manager, which is plugged
into the Layer 3 or Layer 2 UDN Network Controller as a reconciler. Whenever the network controller changes, which should happen
when the NetInfo is updated with the extra subnet, then Reconcile will be called, which will call NeedsReconciliation in
Route Import Manager. There we need to update the logic to account for a new subnet being added. The new subnet is used
for ignoring route import in normal mode, but with no-overlay mode, they will be imported.

A unit test/e2e test should be added to ensure that advertised routes with the new subnet are not imported.

#### Egress IP

Egress IP generates a route for the pod subnet towards the Gateway Router (GR). This path is triggered when a node add/update
happens. Right now in the BaseNetworkController->reconcile, when a network changes it will check if local nodes should be
added to the retry framework to be updated. It only checks if route advertisement changed, but we should update this to
also accomodate for a new subnet being added.

A unit test should be added to make sure that when Egress IP is being used and a new subnet is added, then the default
route gets created correctly for each subnet.

There are also Logical Route Policies created for east/west traffic that are initiated by the EgressNodeType in the retry
framework. We should ensure with unit tests that policies are also created for the new subnet when it is added.

#### BGP Network Isolation

With BGP Network Isolation we add ACLs for isolation to not allow UDNs to talk to other UDNs. This is done by the 
addAdvertisedNetworkIsolation function, which is driven by add/update local node events. As with the Egress IP changes,
BaseNetworkController->reconcile should be updated to queue local nodes to the retry framework when the subnet changes.

A unit test should be added to ensure the isolation ACLs get updated correctly with the new subnet.

#### Egress Service

Currently not supported by UDN, so this will not be handled in this OKEP.

#### UDN Gateway

With OpenFlow Manager we add flows for pod subnets so that traffic coming into OVS from outside (addressed to the pod) can be
forwarded correctly to the GR. Since this flow only needs to exist for the subnet of the current node, it should not need to
be updated on previous nodes when a new Subnet is added.

### API Validations

The `subnets` field in the current CRD has the following validation rules:
```go
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="size(self) != 2 || !isCIDR(self[0].cidr) || !isCIDR(self[1].cidr) || cidr(self[0].cidr).ip().family() != cidr(self[1].cidr).ip().family()", message="When 2 CIDRs are set, they must be from different IP families"
	Subnets []Layer3Subnet `json:"subnets,omitempty"`
```

* `MaxItems` is required to keep CEL evaluation costs below threshold. It has to be
  retained but adjusted to a reasonable value (the max value is 400 for the new rules defined below).
* The `XValidation:rule` must be removed to allow more than one subnet per IP family.

New validation logic should be added to enforce the following constraints on `layer3.subnets`:
* Prevent removal or modification of existing subnets:
  ```yaml
    - message: Removing existing subnets is not allowed
      rule: '!has(oldSelf.subnets) || oldSelf.subnets.all(old, self.subnets.exists(new,
        new.cidr == old.cidr))'
  ```
* Prevent overlapping or nested subnets:
  ```yaml
    - message: Subnets must not overlap or contain each other
      rule: '!has(self.subnets) || self.subnets.size() == 1 || !self.subnets.exists(i,
        self.subnets.exists(j, i != j && cidr(i.cidr).containsCIDR(j.cidr)))'
  ```
* Ensure all subnets use the same hostSubnet size:
  ```yaml
    - message: All subnets must use the same hostSubnet value
      rule: '!has(self.subnets) || self.subnets.size() == 1 || self.subnets.all(i,
        i.hostSubnet == self.subnets[0].hostSubnet)'
  ```

Additionally, the validation rule that makes the entire UDN `spec` immutable must be removed:
```
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="Spec is immutable"
	Spec UserDefinedNetworkSpec `json:"spec"`
```
Instead, immutability should be enforced at the sub-field level to preserve immutability where
needed.

After a UDN or CUDN is created on the Kubernetes API server, the ovnkube-cluster-manager validates
whether its subnets overlap with internal or reserved networks, such as ClusterSubnets,
ServiceCIDRs, join subnet, etc. If a conflict is detected, the error should be reported in the
UDN/CUDN’s status, and the underlying NAD should not be updated, ensuring that the node subnet
allocator continues to use the validated old subnets. Once the conflict is resolved and the UDN/CUDN
spec becomes valid, the NAD should be automatically reconciled to reflect the latest configuration.

### Testing Details

* **Unit Tests:** Extend subnet allocator tests to cover multiple subnets and dynamic expansion scenarios.
* **E2E Tests:** Test UDN behavior when new subnets are appended, and verify that nodes receive
  allocations from the correct ranges.
* **API Tests:** Validate CRD updates for append-only behavior and ensure invalid changes (e.g., removal
  or modification of existing subnets) are correctly rejected.


### Documentation Details

UDN feature documentation will be updated to declare support for multiple subnets.

## Risks, Known Limitations and Mitigations

### Limitations

1. **Subnets are append-only**: Removing or modifying existing subnets after creation is not
   supported.

2. **Kubernetes version requirement**: The new CRD validation rules use `containsCIDR()`, which
   requires Kubernetes 1.21 or newer.


3. **Uniform `hostSubnet` size**: All subnets must use the same `hostSubnet` size.
  Supporting subnets with different `hostSubnet` sizes is currently downstream-only and depends on
  OVN commit [27cc274](https://github.com/ovn-org/ovn/commit/27cc274e66acd9e0ed13525f9ea2597804107348)
  *northd: Use lower priority for all src routes*. This limitation may be lifted in the future.

4. **Missing routes in existing Pods for new subnet (Secondary Layer3 networks only)**:
   When a new subnet is added to a secondary layer3 network, the existing Pods don't have the route
   for reaching the IPs in the new subnet, so the traffics targeting the new subnet will go via primary
   interface. For example, create UDN with one cluster subnet:

   When a new subnet is added to a secondary Layer3 network, existing Pods do not receive updated
   routes to reach the new subnet. As a result, traffic to IPs in the new subnet is routed via the
   primary network interface. The issue is depicted as below:

   - A Secondary Layer3 network is created with one cluster subnet:
      ```yaml
      apiVersion: k8s.ovn.org/v1
      kind: UserDefinedNetwork
      metadata:
         name: udn-secondary-layer3
      spec:
         topology: Layer3
         layer3:
            role: Secondary
            subnets:
            - cidr: 10.10.0.0/16
            hostSubnet: 17
      ```
   - Node A allocates a subnet from `10.10.0.0/16`:
      ```bash
         k8s.ovn.org/node-subnets: '{...,"udn_udn-secondary-layer3":["10.10.128.0/17"]}'
      ```
   - Pod A is created on Node A and gets the following secondary network config:
      ```
      k8s.ovn.org/pod-networks: '{
         ...,
         "udn/udn-secondary-layer3":{
            "ip_addresses":["10.10.128.4/17"],"mac_address":"0a:58:0a:0a:80:04",
            "routes":[
                  {"dest":"10.10.0.0/16","nextHop":"10.10.128.1"}],
            "ip_address":"10.10.128.4/17","role":"secondary"}}'
      ```
   - A new subnet is added to the UDN:
      ```
      apiVersion: k8s.ovn.org/v1
      kind: UserDefinedNetwork
      metadata:
      name: udn-secondary-layer3
      namespace: udn
      spec:
      layer3:
         role: Secondary
         subnets:
         - cidr: 10.10.0.0/16
            hostSubnet: 17
         - cidr: 10.20.0.0/16
            hostSubnet: 17
      topology: Layer3
      ```
   - Node B allocates a subnet from `10.20.0.0/16`:
      ```
      k8s.ovn.org/node-subnets: '{"default":["10.192.2.0/24"],"udn_udn-primary-layer3":["10.2.0.0/17"],"udn_udn-secondary-layer3":["10.20.0.0/17"]}'
      ```
   - Pod B is created on Node B and receives both route entries:
      ```
      k8s.ovn.org/pod-networks: '{
         ...,
         "udn/udn-secondary-layer3":{
            "ip_addresses":["10.20.0.4/17"],"mac_address":"0a:58:0a:14:00:04",
            "routes":[
                  {"dest":"10.10.0.0/16","nextHop":"10.20.0.1"},
                  {"dest":"10.20.0.0/16","nextHop":"10.20.0.1"}],
            "ip_address":"10.20.0.4/17","role":"secondary"}}'
      ```
   - traffic from Pod A to Pod B is routed via the primary network:
      ```bash
      root@client-a:/# ip route get 10.20.0.4
      10.20.0.4 via 10.1.0.1 dev ovn-udn1 src 10.1.0.10 uid 0
      cache
      ```

   This issue occurs because the existing Pod (Pod A) lacks route updates to the newly added
   subnet. It can be resolved by recreating affected Pods.


   **Note**: This issue does not affect the primary network, as its interface is always the default
   gateway:
   ```
   root@client-a:/# ip route
   default via 10.1.0.1 dev ovn-udn1
   ...
   ```

## OVN Kubernetes Version Skew

To be updated based on reviewer feedback.

## Alternatives

* Recreate UDN with new subnet list — disruptive and impractical for large clusters.
* Use NAD for Layer3 — not applicable for users who standardize on UDN.

