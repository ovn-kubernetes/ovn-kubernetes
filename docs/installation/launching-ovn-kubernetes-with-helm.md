# Launching OVN-Kubernetes using Helm Charts

## Introduction

This helm chart supports deploying OVN K8s CNI in a K8s cluster.

Open Virtual Networking (OVN) Kubernetes CNI is an open source networking and
network security solution for Kubernetes workloads. It leverages a distributed
OVN SDN control plane and per-node Open vSwitch (OVS) to provide network
virtualization and network connectivity to K8s Pods. It does so by creating a logical
network topology using logical constructs such as logical switches (Layer 2) and
logical routers (Layer 3). The Pod interfaces are represented by logical ports on
the logical switches. On these logical switch ports, one can specify IP network
information (IP address and MAC address), anti-spoofing rules (MAC and IP),
Security Groups, QoS configuration, and so on.

A port, either physical SR-IOV VF or virtual VETH, assigned to a Pod will be associated
with a corresponding logical port, this will result in applying all the logical port
configuration onto the physical port. The logical port becomes the API for
configuring the physical port.

In addition to providing overlay network connectivity for Pods in the K8s cluster,
OVN K8s CNI supports a plethora of advanced networking features, such as

```
- Optimized and Accelerated K8s Network Policy on Pod's traffic
- Optimized and Accelerated K8s Service Implementation (aka Load Balancers and NAT)
- Optimized and Accelerated Policy Based Routing
- Multi-Home Pods with an option for Secondary networks to be on a Layer-2
  Overlay (flat network), Layer-2 Underlay (VLAN-based) on private or public
  subnets.
- Optimized and Accelerated K8s Network Policy on Pod's secondary networks
```

Most of these services are distributed and implemented via a pipeline (series
of OpenFlow tables with OpenFlow flows) on local OVS switches. These OVS
pipelines are very amenable to offloading to NIC hardware, which should result
in the best possible networking performance and CPU savings on the host.

The OVN K8s CNI architecture is a layered architecture with OVS at the bottom,
followed by OVN, and finally OVN K8s CNI at the top. Each layer has several
K8s components - deployments, daemonsets, and statefulsets. Each component at
every layer is a subchart by itself. Based on the deployment needs, all or
some of these subcharts are installed to provide the aforementioned OVN K8s
CNI features, this can be done by editing `tags` section in values.yaml file.

## Pre-requisites

This guide assumes you already have a running Kubernetes cluster reachable via `KUBECONFIG`, with **no CNI installed** and **kube-proxy disabled** — OVN-Kubernetes provides both.

If you don't have such a cluster, you can create one with:

- [Launching OVN-Kubernetes with kubeadm](INSTALL.KUBEADM.md) — multi-VM walkthrough on a `kubeadm`-bootstrapped cluster.

## Quickstart

Once you have a cluster matching the pre-requisites above, run `helm/basic-deploy.sh` from the repo to install OVN-Kubernetes via Helm against the cluster `KUBECONFIG` points at.

The chart uses `values-single-node-zone.yaml` by default.

## Step-by-step install


- Set the zone label on each node (required for interconnect mode):
```
for n in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
  kubectl label node "${n}" k8s.ovn.org/zone-name=${n} --overwrite
done
```

- Run `helm install` with the appropriate `k8sAPIServer`, image repo and tag:
```
# cd helm/ovn-kubernetes
# helm install ovn-kubernetes . -f values-single-node-zone.yaml --set k8sAPIServer="https://$(kubectl get pods -n kube-system -l component=kube-apiserver -o jsonpath='{.items[0].status.hostIP}'):6443" --set global.image.repository=ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu --set global.image.tag=master
```

- Optional: build a custom OVN-Kubernetes image, push it to a registry the cluster nodes can pull from, and point `helm install` at it via `--set global.image.repository=...` / `--set global.image.tag=...`:
```
# cd dist/images
# make ubuntu-image
# docker tag ovn-kube-ubuntu:latest <your-registry>/ovn-kube-ubuntu:<tag>
# docker push <your-registry>/ovn-kube-ubuntu:<tag>
```

## Alternative Configurations

The deprecated central mode topology is available via `-f values-no-ic.yaml`.

## Values

See the [helm chart README](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/helm/ovn-kubernetes/README.md#values)
for the full list of helm values supported by this chart.
