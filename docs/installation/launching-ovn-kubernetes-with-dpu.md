# Launching OVN-Kubernetes in DPU-Accelerated environment in interconnect mode

## OVN K8s cluster setup

OVN K8s CNI in a DPU-Accelerated environment is deployed using two Kubernetes clusters, one for the hosts and other for the DPUs.

DPUs in the DPU cluster will watch DPU Host cluster for K8s resources such as Pods, Namespaces, NetworkAttachmentDefinitions, Services, and Endpoints and act on updates to those resources. Hence they require credentials to access DPU host cluster. Each DPU will have a setting denoting the DPU host to which it is associated.  

## SR-IOV settings on DPU Host

Follow [OVS Acceleration with Kernel datapath](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/features/hardware-offload/ovs-kernel.md) or [OVS Acceleration with DOCA datapath](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/features/hardware-offload/ovs-doca.md) to enable Open vSwitch hardware offloading feature on DPU hosts.

A single VF net-device or a group of VF net-devices (configured as SR-IOV device plugin resource pool) need to be setup separately to create management port(s).

## K8s Settings on DPU Host

The following node labels must be set on the DPU Host prior to installing OVN K8s CNI

```
k8s.ovn.org/dpu-host= 
k8s.ovn.org/zone-name="dpu-host node name"
```

## Launching OVN K8s DPU Host cluster using helm
OVN K8s CNI can be deployed using helm charts provided under [OVN K8s Helm Charts](https://github.com/ovn-kubernetes/ovn-kubernetes/tree/master/helm/ovn-kubernetes). Refer [Launching OVN-Kubernetes using Helm Charts](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/installation/launching-ovn-kubernetes-with-helm.md) for general instructions on using helm charts and explanation of common values used in various subcharts.

For DPU Hosts cluster use values-single-node-zone.yaml by setting the following fields as specified. The other fields in the file can be set as needed.

```
tags:
  ovnkube-node-dpu-host: true      # Removing this line will also enable applying ovnkube-node-dpu-host subchart
  ovs-node: false                  # Disable ovs-node subchart, as OVS is already provided by the corresponding DPU
global:
  enableOvnKubeIdentity: false     # This feature is not supported currently for clusters with DPU/DPU-Hosts
```

ovn-kubernetes image to be used in the containers should be provided in the image section
```
global:
  image:
    repository: ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-fedora
    tag: master
```

Management port netdevice information should be provided in values.yaml file under helm/ovn-kubernetes/charts/ovnkube-node-dpu-host
```
nodeMgmtPortNetdev: "enp1s0f0v0"         # Single VF net-device to be used for management port or
mgmtPortVFResourceName: "mgmtport_vfs"   # SR-IOV device plugin resource pool from which VF net-device(s) can be selected.
```

mgmtPortVFResourceName will be prioritized over nodeMgmtPortNetdev if both are specified.

Launch OVN K8s using
```
helm install ovn-kubernetes . -f values-single-node-zone.yaml
```

## Generating credentials for accessing this cluster from DPU

After deploying the CNI, create a secret in this cluster for service account ovnkube-node by applying the following
```
apiVersion: v1
kind: Secret
metadata:
 name: ovnkube-node-sa-for-dpu
 namespace: ovn-kubernetes
 annotations:
   kubernetes.io/service-account.name: ovnkube-node
type: kubernetes.io/service-account-token
```

Get the value of ca.crt and token, which will be used in the DPU cluster. The token should be base64 decoded, but the encoded ca.crt should be used as is.

## K8s Settings on DPU

The following node label is required on DPUs prior to installing OVN K8s CNI
```
k8s.ovn.org/dpu= 
```

## OVS settings on DPU
Some OVS settings are required on the DPU to enable hardware offloads, connect to the right DPU-host in the DPU-host cluster and correctly steer traffic flows.

Consider an example ovs bridge configuration on DPU

```
ovs-vsctl show
    Bridge brp0
        fail_mode: standalone
        Port pf0hpf
            tag: 3
            Interface pf0hpf
                type: system
        Port p0
            Interface p0
                type: system
        Port vtep0
            tag: 2
            Interface vtep0
                type: internal
        Port brp0
            Interface brp0
                type: internal
```

With the ip addresses configured as

```
$ ip addr show dev brp0
4: brp0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP group default qlen 1000
    link/ether 52:54:00:a1:b2:c3 brd ff:ff:ff:ff:ff:ff
    inet 192.0.2.10/24 brd 192.0.2.255 scope global brp0
       valid_lft forever preferred_lft forever

$ ip addr show dev vtep0
5: vtep0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1450 qdisc noqueue state UNKNOWN group default qlen 1000
    link/ether 52:54:00:d4:e5:f6 brd ff:ff:ff:ff:ff:ff
    inet 198.51.100.10/24 brd 198.51.100.255 scope global vtep0
       valid_lft forever preferred_lft forever
```

On the DPU host with node name dpu-host, the IP address is set as

```
$ ip addr show dev enp1s0f0
2: enp1s0f0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP group default qlen 1000
    link/ether 52:54:00:12:34:56 brd ff:ff:ff:ff:ff:ff
    inet 203.0.113.10/24 brd 203.0.113.255 scope global eth0
       valid_lft forever preferred_lft forever

$ ip route show default
default via 203.0.113.1 dev enp1s0f0 proto static

with router subnet 203.0.113.0/24
```

The required OVS settings are as below, with values taken from the above example

```
other_config:hw-offload=true                        - enable hardware offloading
external-ids:host-k8s-nodename="dpu-host"           - name of DPU-Host node
external_ids:hostname="chassis-dpu"                 - OVN Chassis hostname of the DPU
external_ids:ovn-encap-ip="198.51.100.10"           - encapsulation IP of the DPU
external_ids:ovn-encap-type="geneve"                - supported encapsulation type
external-ids:ovn-gw-interface="brp0"                - interface on the DPU that serves as gateway interface
external-ids:ovn-gw-nexthop="203.0.113.1"           - default gateway address for the DPU-Host network
external_ids:ovn-gw-router-subnet="203.0.113.0/24"  - subnet to be used for the gateway router if DPU is in a different subnet than DPU-Host network
external_ids:ovn-gw-vlanid="3"                      - optional setting if VLAN id of gateway is not on native VLAN
```

## Launching OVN K8s DPU cluster

Once the DPU-host cluster is deployed, the credentials to access that cluster is needed for DPU cluster deployment. It also requires additional information regarding OVN K8s configuration.

Use values-single-node-zone-dpu.yaml for deploying the DPU cluster by setting the following fields as specified. Update other fields as needed.
```
tags:
  ovs-node: false                 # Disable ovs-node subchart, as ovs is already running on DPUs
global
  enableOvnKubeIdentity: false    # This feature is not supported currently for clusters with DPU/DPU-Hosts
```

The following DPU Host cluster related information must be set correctly.
```
global:
  dpuHostClusterK8sAPIServer: "https://172.25.0.2:6443"    # Endpoint of DPU Host cluster's K8s API server
  dpuHostClusterK8sToken: ""                               # DPU Host cluster's K8s Access Token base64 decoded
  dpuHostClusterK8sCACertData: ""                          # DPU Host cluster's encoded K8s Access Certs Data
  dpuHostClusterNetworkCIDR: "10.244.0.0/16/24"            # DPU Host cluster's Network CIDR
  dpuHostClusterServiceCIDR: "10.96.0.0/16"                # DPU Host cluster's Service CIDR
  mtu: "1400"                                              # MTU of network interface in K8s pod
```

ovn-kubernetes image to be used in the containers should be provided in the dpuImage section. It should be built for arm64 architecture.
```
global:
  dpuImage:
    repository: ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu
    tag: master
```
Launch OVN K8s using
```
helm install ovn-kubernetes . -f values-single-node-zone-dpu.yaml
```
