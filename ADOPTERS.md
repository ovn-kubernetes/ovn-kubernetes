# OVN-Kubernetes Adopters

This page contains a list of organizations and projects who are adopters of OVN-Kubernetes.

**NOTE:** For adding your organization or project to this list (alphabetical order), fork the repository and open a PR with the required change. And don't forget to say hi on Slack as well. 
Adopters at **any stage** are welcome — production, dev, testing, or trialing. Your experience helps the community grow! 
See list of adopter types at the bottom of this page.

## Organizations

| Organization | Adopter Type | Details |
|--------------|--------------|---------|
| [Internet Initiative Japan Inc.](https://www.iij.ad.jp/) | End-User | Uses OVN-Kubernetes in their on-premise Kubernetes platform. [Read the blog](https://ovn-kubernetes.io/master/blog/2026/09/01/why-iij-chose-ovn-kubernetes-for-its-vm-workload-migration-platform-and-how-we-use-it-today/) |
| [Nutanix](https://www.nutanix.com/) | Service Provider | Builds Flow CNI on OVN-Kubernetes, integrated with Nutanix Flow and VPC networking. |
| [NVIDIA](https://www.nvidia.com/) | End-User | Uses OVN-Kubernetes in their production environments. |
| [Red Hat, LLC](https://www.redhat.com/) | Service Provider | Uses OVN-Kubernetes as their default CNI in [Red Hat OpenShift](https://www.redhat.com/en/technologies/cloud-computing/openshift). |
| [SAIC Motor Corp. Ltd](https://www.saicmotor.com/) | End-User | Uses OVN-Kubernetes as a networking solution to build a multi-tenant private cloud. [Read the blog](https://ovn-kubernetes.io/master/blog/2026/09/03/saic-motors-kubernetes-based-multi-tenant-networking-practice-building-a-unified-network-foundation-with-ovn-kubernetes/) |

## Projects

| Project | Details |
|---------|---------|
| [KubeStellar Console](https://console.kubestellar.io) | Provides a [guided install mission for OVN-Kubernetes](https://console.kubestellar.io/missions/install-ovn-kubernetes) via an open-source Kubernetes dashboard with AI-assisted operations. |
| [Submariner](https://submariner.io/) | [Uses OVN-Kubernetes CNI for multicluster networking](https://submariner.io/getting-started/architecture/networkplugin-syncer/ovn-kubernetes/). |

## Adopter Types

See CNCF [definition of an adopter](https://github.com/cncf/toc/blob/main/FAQ.md#what-is-the-definition-of-an-adopter)

- **End-User** - Companies and organizations that use OVN-Kubernetes internally, or build upon a cloud native open source project but do not sell the cloud native project externally as a service offering (those are Service Providers).
- **Service Provider** - Organizations that repackage OVN-Kubernetes as a core component of a service offering or sells cloud native services externally. A Service Provider’s customers are considered transitive adopters and should be excluded from identification within the ADOPTERS.md file.
- **Open source project** - Open source projects that leverage OVN-Kubernetes as part of their solution or integrate with it for compatibility and interoperability
