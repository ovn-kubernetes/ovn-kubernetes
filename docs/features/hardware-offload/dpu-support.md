# OVS Acceleration with DPUs

[Data Processing Units](https://blogs.nvidia.com/blog/2020/05/20/whats-a-dpu-data-processing-unit/) (DPU) combine the advanced capabilities
of a Smart-NIC (such as Mellanox ConnectX-6DX NIC) with a general purpose embedded CPU and a high-speed memory controller.

Similar to Smart-NICs, a DPU follows the kernel switchdev model.
In this model, every VF/PF net-device on the host has a corresponding representor net-device existing on the embedded CPU.

It is desirable to leverage DPU in OVN-kubernetes to accelerate networking and secure the network control plane.

## Supported DPUs

The following manufacturers are known to work:

- [Mellanox Bluefield-2](https://www.mellanox.com/products/bluefield2-overview)

## Prerequisites

- Linux Kernel 5.7.0 or above
- Open vSwitch 2.13 or above
- iproute >= 4.12
- sriov-device-plugin
- multus-cni

## Configuring hosts with DPUs in OVN-Kubernetes clusters
The hosts and their corresponding DPUs are added as worker nodes in the OVN-Kubernetes cluster. The steps required to configure SR-IOV and enable multihoming on the DPU-Host are provided below.

## Multus CNI configuration
Deploy multus CNI as daemonset to enable attaching multiple network interfaces to pods. Refer https://github.com/intel/multus-cni

## DPU-Host worker node SR-IOV configuration
In order to enable Open vSwitch hardware offloading, the following steps are required. Please make sure you have root privileges to run the commands below.

Check the number of VFs supported on the NIC

```
cat /sys/class/net/enp2s0f0np0/device/sriov_totalvfs
16
```

Create the VFs

```
echo '4' > /sys/class/net/enp2s0f0np0/device/sriov_numvfs
```

Verify that VFs are created

```
sudo ip link show enp2s0f0np0
3: enp2s0f0np0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc mq state UP mode DEFAULT group default qlen 1000
    link/ether 10:70:fd:09:81:aa brd ff:ff:ff:ff:ff:ff
    vf 0  link/ether 00:00:00:00:00:00 brd ff:ff:ff:ff:ff:ff, spoof checking off, link-state auto, trust off, query_rss off
    vf 1  link/ether 00:00:00:00:00:00 brd ff:ff:ff:ff:ff:ff, spoof checking off, link-state auto, trust off, query_rss off
    vf 2  link/ether 00:00:00:00:00:00 brd ff:ff:ff:ff:ff:ff, spoof checking off, link-state auto, trust off, query_rss off
    vf 3  link/ether 00:00:00:00:00:00 brd ff:ff:ff:ff:ff:ff, spoof checking off, link-state auto, trust off, query_rss off
```

Setup the PF to be up

```
sudo ip link set enp2s0f0np0 up
```

Get the network device information to create the SR-IOV device plugin configuration

```
lspci -nnvD
0000:02:00.4 Ethernet controller [0200]: Mellanox Technologies ConnectX Family mlx5Gen Virtual Function [15b3:101e] (rev 01)
    Subsystem: Mellanox Technologies ConnectX Family mlx5Gen Virtual Function [15b3:0062]
    Flags: bus master, fast devsel, latency 0
    Memory at 96200000 (64-bit, prefetchable) [virtual] [size=2M]
    Capabilities: [60] Express Endpoint, MSI 00
    Capabilities: [9c] MSI-X: Enable+ Count=12 Masked-
    Capabilities: [100] Vendor Specific Information: ID=0000 Rev=0 Len=00c <?>
    Capabilities: [150] Alternative Routing-ID Interpretation (ARI)
    Kernel driver in use: mlx5_core
    Kernel modules: mlx5_core
```


# DPU-Host worker node SR-IOV network device plugin configuration

This plugin creates device plugin endpoints based on the configurations given in file /etc/pcidp/config.json. This configuration file is in json format as shown below:

```
{
    "resourceList": [
         {
            "resourceName": "mlnx_sriov_cx6",
            "selectors": {
                "vendors": ["15b3"],
                "devices": ["101e"]
                "pfNames": ["enp2s0f0np0#1-3"]
            }
        }
    ]
}
```

The `pfNames` specify which VFs can be allocated as resources for pods created on this node. One of the VFs should be reserved for creating the ovn-k8s-mp0 management port and is excluded from this list.

Deploy SR-IOV network device plugin as daemonset. Refer https://github.com/intel/sriov-network-device-plugin

Verify that the VFs are available on the DPU-Host worker node.
```
kubectl describe node DPU-Host
...
Capacity:
  mellanox.com/mlnx_sriov_cx6:  3
Allocatable:
  mellanox.com/mlnx_sriov_cx6:  3
...
```

## DPU worker node configuration
The steps required to enable Open vSwitch hardware offloading and associate the DPU worker node with the corresponding DPU-Host worker node in the cluster are provided below.
OVS software will be running on the embedded CPU of the DPU and it will bridge the packets from the host to underlay. One port of the OVS bridge will be the
DPU port (for example, p0), and the other port will be the representor port (for example, pf0hpf). Following are the steps to create this OVS bridge configuration.

```
# ovs-vsctl add-br brp0
# ovs-vsctl add-port brp0 p0
# ovs-vsctl add-port brp0 pf0hpf
# ovs-vsctl show
aa35917b-16f3-43a8-869a-e169d6a117e3
    Bridge brp0
        Port p0
            Interface p0
        Port pf0hpf
            Interface pf0hpf
        Port brp0
            Interface brp0
                type: internal
```

## Open vSwitch settings
The following OVS settings are required on the DPU to enable hardware offloads and connect the DPU-Host with the DPU in the OVN-Kubernetes cluster.

```
ovs-vsctl set Open_vSwitch . other_config:hw-offload=true
ovs-vsctl set Open_vSwitch . external-ids:host-k8s-nodename="dpu-host"   -> Name of DPU-Host node
ovs-vsctl set Open_vSwitch . external-ids:ovn-gw-interface="gw-interface"    -> Interface on the DPU that will be the gateway interface
ovs-vsctl set Open_vSwitch . external-ids:ovn-gw-nexthop="gw-nexthop"      -> Default gateway address for the DPU-Host network
ovs-vsctl set Open_vSwitch . external_ids:ovn-gw-router-subnet="router-subnet" -> Subnet to be used for the gateway router if DPU is in a different subnet than DPU-Host network
```

Then restart Open vSwitch service.

# Installing OVN-kubernetes CNI

Helm charts are available for installing OVN-Kubernetes cluster in https://github.com/ovn-kubernetes/ovn-kubernetes/tree/master/helm/ovn-kubernetes. 
The daemonsets ovnkube-node-dpu-host and ovnkube-node-dpu are provided to launch the corresponding OVN-Kubernetes networking components.

## Adding DPU-Host worker node to OVN-Kubernetes cluster
The following node label is required on the DPU-Host for the ovnkube-node-dpu-host daemonset's node selection.

```
k8s.ovn.org/dpu-host= 
```

The field OVNKUBE_NODE_MGMT_PORT_NETDEV needs to be set in the ovnkube-node-dpu-host.yaml indicating the VF device to be used for management port. For example, "enp2s0f0np0v0"

## Adding DPU worker node to OVN-Kubernetes cluster
The following node label is required on the DPU for the ovnkube-node-dpu daemonset's node selection.

```
k8s.ovn.org/dpu= 
```

In addition, a node label referred to as "noHostSubnetLabel" in the values.yaml file in the helm charts is required to indicate to the nodes running ovnkube-master that the DPU worker node is managing its own network. By default in the ovnkube-master deployments this is set to
 
```
"k8s.ovn.org/ovn-managed=false"
```

The same label needs to be set on the DPU worker node.

## Additional Custom Resource Definitions
Some basic CRDs are already provided under helm/ovn-kubernetes/crds. If additional CRDs are required, they should be placed in that location. For example, to support multihoming, the following will be required.

```
k8s.cni.cncf.io-network-attachment-definitions - https://github.com/k8snetworkplumbingwg/network-attachment-definition-client/blob/master/artifacts/networks-crd.yaml
```

## Network Attachment Definitions
If Network Attachment Definitions are used, they should be placed under helm/ovn-kubernetes/templates. Refer https://ovn-kubernetes.io/features/multiple-networks/multi-homing/

## Helm Installation
To use DPU-Hosts with basic features, install OVN-Kubernetes CNI using helm with https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/helm/ovn-kubernetes/values-no-ic.yaml with the following settings.

```
tags.ovs-node=false
tags.ovnkube-node-dpu-host=true
tags.ovnkube-node-dpu=true
global.enableMultiNetwork=true
global.disableRequestedchassis=true
global.enableOvnKubeIdentity=false
global.enableMultiExternalGateway=false
global.enableEgressService=false
global.enableEgressFirewall=false
global.enableEgressQos=false
global.enableEgressIp=false
```

## Deploy pod with OVS hardware-offload
Example pod spec

```
apiVersion: v1
kind: Pod
metadata:
  name: ovs-offload-pod1
  namespace: ovn-kubernetes
  annotations:
    v1.multus-cni.io/default-network: default
spec:
  containers:
  - name: appcntr1
    image: alpine
    command:
      - "sleep"
      - "604800"
    imagePullPolicy: IfNotPresent
    resources:
      requests:
        mellanox.com/mlnx_sriov_cx6: '1'
      limits:
        mellanox.com/mlnx_sriov_cx6: '1'
```

The OVS output on the DPU will look like this when the pod is deployed
```
# ovs-vsctl show
aa35917b-16f3-43a8-869a-e169d6a117e3
    Bridge brp0
        Port pf0hpf
            Interface pf0hpf
        Port brp0
            Interface brp0
                type: internal
        Port patch-brp0_dpu-host-to-br-int
            Interface patch-brp0_dpu-host-to-br-int
                type: patch
                options: {peer=patch-br-int-to-brp0_dpu-host}
        Port p0
            Interface p0
    Bridge br-int
        fail_mode: secure
        datapath_type: system
        Port patch-br-int-to-brp0_dpu-host
            Interface patch-br-int-to-brp0_dpu-host
                type: patch
                options: {peer=patch-brp0_dpu-host-to-br-int}
        Port pf0vf1
            Interface pf0vf1
        Port ovn-8c1016-0
            Interface ovn-8c1016-0
                type: geneve
                options: {csum="true", key=flow, local_ip="192.168.41.127", remote_ip="192.168.40.111"}
        Port ovn-k8s-mp0
            Interface ovn-k8s-mp0
        Port br-int
            Interface br-int
                type: internal
```

Verify the offloaded packets using this command on the DPU

```
# ovs-appctl dpctl/dump-flows type=offloaded
```
