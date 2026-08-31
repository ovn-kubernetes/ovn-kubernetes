# Why IIJ Chose OVN-Kubernetes for Its VM Workload Migration Platform and How We Use It Today

My name is Taichi Shimotori, and I work in the SRE Promotion Department of the Network Division at Internet Initiative Japan Inc. (IIJ).

At IIJ, we are developing a Kubernetes platform that uses OVN-Kubernetes as a destination for migrating VM workloads.
We currently use the platform in development and SRE environments, and we plan to build a production environment during fiscal year 2026.
In this article, I explain why we adopted OVN-Kubernetes and how we use it today.

## IIJ and IKE

IIJ was founded in 1992 and became the first Japanese company to launch a commercial Internet service.
Since then, we have used our expertise in networking, cloud computing, security, mobile services, and IoT to provide IT services and systems integration to approximately 16,000 corporate customers in Japan and overseas, as well as individual users.

IIJ develops and operates IKE, the IIJ Kubernetes Engine, as one of the platforms used to build and deliver these services.
The SRE Promotion Department is responsible for IKE.

IKE provides the capabilities required to use Kubernetes as an application platform in an on-premises environment, including networking, storage, observability, and sensitive information management.
It also integrates with IIJ's backbone network and back-office systems, providing IIJ-specific capabilities to internal service development teams as part of the platform.

IKE has been in operation since 2018 and now comprises multiple production clusters.

## Migrating VM Workloads

IIJ has built many of its services on VMware-based VMs.
However, rising licensing costs increased the need to migrate existing VMs to another platform.

IKE emerged as a candidate destination.
We already operate a KubeVirt-based VM platform on IKE, allowing us to apply both our operational experience with Kubernetes and the platform capabilities provided by IKE.
Managing VMs and containers through the same Kubernetes API and operational platform also lets us choose the most suitable execution model for each workload.

## Network Requirements for the VM Migration

Migrating VMs to IKE requires us to carry over not only compute resources but also the network requirements of the existing environment.

In our conventional IKE clusters, the primary connection for workloads has been a Layer 3 pod network spanning the cluster.
In IIJ's existing VM environment, however, a single VM connects to multiple Layer 2 networks, with separate networks for different applications and tenants.

The migration platform therefore needed to provide the following capabilities:

- Connect a single VM to multiple networks
- Create Layer 2 networks
- Isolate networks by application or tenant
- Connect to existing physical networks

## Why We Adopted OVN-Kubernetes

To meet these network requirements, we adopted OVN-Kubernetes as the CNI plugin for the destination cluster.
We named this cluster IKE VPC, short for Virtual Private Cluster.

OVN-Kubernetes lets us define networks for different purposes as Kubernetes custom resources, separately from the cluster's default pod network.
It provides two resources for defining these networks:

- **UserDefinedNetwork (UDN):** A namespace-scoped resource that creates a network confined to its namespace and isolated from other namespaces.
- **ClusterUserDefinedNetwork (CUDN):** A cluster-scoped resource that selects multiple namespaces and attaches them to the same network.

Both UDN and CUDN support Layer 3 and Layer 2 network topologies.
These are overlay networks contained within the cluster and require no advance configuration of the physical network.

CUDN also supports Localnet.
Localnet connects VMs or pods to a physical network preconfigured on the nodes.
However, Localnet can be specified only with CUDN, and its role must be Secondary.
In other words, it is used as an additional interface rather than as the pod's primary network.

Connecting a single VM or pod to multiple networks also requires a meta-plugin that invokes multiple CNI plugins in sequence.
OVN-Kubernetes relies on Multus for this purpose.
Multus is a separate project developed by the Kubernetes Network Plumbing Working Group; it is not part of OVN-Kubernetes itself.

In IKE VPC, we use these components as follows.

### UDN Is Available to Users

IKE is a multi-tenant platform in which users are hosted by namespace, so UDN's namespace scope directly matches our service model.
Each tenant can define an isolated network within its own namespace.
Tenants can also choose either a Layer 2 or Layer 3 topology according to their requirements.

### CUDN Is Managed by the SRE Team

When a tenant network needs to connect to a particular network, the SRE team builds a router host to serve as its gateway.
We use CUDN to attach both the router host and the tenant's namespace to the same network.
Only CUDN can define a network that spans multiple namespaces.
Because CUDN is also a cluster-scoped resource, it is managed by the SRE team rather than by users.

### Localnet Is Also Managed by the SRE Team

We use Localnet for two purposes.
The first is to connect the router host described above to a particular physical network.
The second is to connect VMs in the SRE-only cluster described later to operations and global networks.
Because both uses connect directly to existing physical networks, the SRE team manages their configuration.

### Multus Is Available to Users

Multus directly satisfies the requirement to connect a single VM or pod to multiple networks, as in our existing VM environment.

This division of responsibilities lets us express every capability required for migrating our existing VMs as a Kubernetes resource.
Multus provides connections to multiple networks, UDN and CUDN provide Layer 2 networking and per-tenant isolation, and CUDNs with the Localnet topology provide connections to existing physical networks.

## Current Validation and Adoption Status

We currently provide service development teams with an IKE VPC development environment.
The teams use this environment to determine whether they can migrate existing services to IKE VPC or deliver new services on it.

We collect feedback from users in the development environment to identify missing platform capabilities and unexpected defects before production adoption.

The IKE platform itself also includes components that run as VMs, such as Kubernetes control plane nodes and load balancers.
As a migration destination for these VMs, we built a separate SRE-only Kubernetes cluster that also uses OVN-Kubernetes.
VMs that make up the user-facing development cluster and provide its platform capabilities have now completed migration and are running on the SRE-only cluster.
These VMs connect to existing physical networks, including operations and global networks, through CUDNs with the Localnet topology managed by the SRE team.

The VMs that make up the production clusters have not yet been migrated.
We plan to migrate them incrementally, applying the configuration and operational knowledge gained from the development environment migration.

## Working with Upstream

While adopting OVN-Kubernetes, we found two problems in its container image build pipeline and reported them in [Issue #5841](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5841).
The maintainers responded promptly, and after discussing the problems, we created a separate pull request for each fix.

The first problem was an incorrect reference to the builder image.
We corrected the reference in [Pull Request #5849](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/5849).

The second problem was that running a multi-platform build on a single runner exhausted its disk space.
In [Pull Request #5850](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/5850), we split the build across multiple runners.

Both pull requests have been merged upstream.

At KubeCon Japan 2026, we also had the opportunity to speak with maintainers in person.
At the project pavilion, we met not only other OVN-Kubernetes users but also attendees who were discovering the project for the first time.

## Production Adoption and Future Contributions

IIJ plans to build a production environment using OVN-Kubernetes during fiscal year 2026.
As the number of workloads grows, we will expand the clusters and networks incrementally.

We will report issues discovered during production operation upstream and propose code changes where appropriate.
We also plan to improve and translate documentation and to continue contributing to the OVN-Kubernetes community.
