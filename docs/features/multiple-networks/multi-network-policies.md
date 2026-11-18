# MultiNetworkPolicies
## Introduction
[multi-networkpolicy](https://github.com/k8snetworkplumbingwg/multi-networkpolicy) provides
[network policy](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
features for secondary networks, and allows enhanced traffic security.

## Motivation
In Kubernetes the paradigm is that by default, all pods can reach other pods, and security is provided by
implementing [Network Policy](https://kubernetes.io/docs/concepts/services-networking/network-policies/).
MultiNetworkPolicies provide the same security solution to secondary interfaces.
For users seeking network security using multi-homing - traffic restrictions based on user network configuration is a
recommended practice.

For example, by defining policies that specify which services a workload can access, they mitigate supply chain attacks
and reduce the risk of lateral movement.
Even if a component is compromised, it won’t be able to jeopardize other components of the system.

## User-Stories
As a project administrator, I want to enforce network policies that must be adhered to by all workloads on
my namespace. This will secure my namespace’s network traffic.

## User-case
- Limit access between workloads running on the cluster connected on a flat layer2 topology:
  In this case example, VMA is a data processor, VMC is a real-time data producer, and VMB is a monitoring and alerting unit.
  The data producer (VMC) can send data to the data processor (VMA), but cannot communicate with the monitoring unit (VMB).
  The data processor (VMA) can send alerts and metrics to the monitoring unit (VMB).
  ![multi-homing-use-case-layer2](multi-homing-use-case-layer2.png)
  ![micro-segmentation-use-case](micro-segmentation-layer2-use-case.png)
- Limit access to a database set outside the cluster, connected to VM and Pod workloads via localnet overlay network:
  In this case example, only the VM has access to the database.
  ![multi-homing-use-case-localnet](multi-homing-use-case-localnet.png)
  ![micro-segmentation-use-case](micro-segmentation-localnet-use-case.png)

## How to enable this feature on an OVN-Kubernetes cluster?
The `multi-network-policies` feature must be enabled in the OVN-Kubernetes configuration.
Enable both `enable-multi-network` and `enable-multi-networkpolicy` under `OVNKubernetesFeatureConfig`.

Always check the dependencies on the [Requirements page](../requirements.md)

## Workflow Description
To configure pod isolation, the user must:
- provision a `network-attachment-definition`.
- provision a `MultiNetworkPolicy` indicating to which secondary networks it
  applies via the
  [policy-for](https://github.com/k8snetworkplumbingwg/multi-networkpolicy#policy-for-annotation)
  annotation.

Please refer to the following example:

```yaml
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: tenant-blue
spec:
    config: '{
        "cniVersion": "0.4.0",
        "name": "tenant-blue-net",
        "netAttachDefName": "default/tenant-blue",
        "topology": "layer2",
        "type": "ovn-k8s-cni-overlay",
        "subnets": "192.168.100.0/24"
    }'
---
apiVersion: k8s.cni.cncf.io/v1beta1
kind: MultiNetworkPolicy
metadata:
  annotations:
    k8s.v1.cni.cncf.io/policy-for: default/tenant-blue # indicates the net-attach-defs this policy applies to
  name: allow-ports-same-ns
spec:
  podSelector:
    matchLabels:
      app: stuff-doer # the policy will **apply** to all pods with this label
  ingress:
  - ports:
    - port: 9000
      protocol: TCP
    from:
    - namespaceSelector:
        matchLabels:
          role: trusted # only pods on namespaces with this label will be allowed on port 9000
  policyTypes:
    - Ingress
```

Please note the `MultiNetworkPolicy` has the **exact same** API of the native
`networking.k8s.io/v1` `NetworkPolicy`object; check its documentation for more
information.

> [!NOTE]
> `net-attach-def`s referred to by the `k8s.v1.cni.cncf.io/policy-for`
annotation without the subnet attribute defined are possible if the policy
**only features** `ipBlock` peers. If the `net-attach-def` features the
`subnet` attribute, it can also feature `namespaceSelectors` and `podSelectors`.

## Validation of `ipBlock` peers
The `MultiNetworkPolicy` CRD does not validate the `cidr` and `except`
fields of an `ipBlock` peer, so OVN-Kubernetes validates them when it
translates the policy. The rules are:

- `cidr` must be a CIDR (`10.10.0.0/16`) or a bare IP address (`10.10.1.42`).
  A bare IP is treated as a single-host CIDR (`/32` or `/128`).
- Each entry in `except` must also be a CIDR or a bare IP address, must be
  contained in `cidr`, and must be a strict subset of it.
- A CIDR with host bits set, such as `10.10.1.42/24`, is accepted and
  canonicalized to its network address, `10.10.1.0/24`.
- IPv4-mapped IPv6 values, such as `::ffff:10.10.1.42`, are not allowed, as they are
  for `NetworkPolicy` since Kubernetes 1.36.

A peer that fails validation is **ignored**, not rejected: the policy is still
applied without that peer, and a warning is logged by `ovnkube-controller`
naming the policy and the reason. If every peer of a rule is invalid, the whole
rule is ignored. Because `policyTypes` still selects the pods for isolation,
dropping a rule never turns the policy into an allow-all: a rule with no valid
peers simply allows nothing.

## User facing API Changes
There are no user facing API Changes.
