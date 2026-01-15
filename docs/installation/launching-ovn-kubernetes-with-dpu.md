# Launching OVN-Kubernetes in DPU-Accelerated environment in interconnect mode

## OVN K8s cluster setup

OVN K8s CNI in a DPU-Accelerated environment is deployed using two Kubernetes clusters, one for the hosts and other for the DPUs.

DPUs in the DPU cluster will watch DPU Host cluster for K8s resources such as Pods, Namespaces, NetworkAttachmentDefinitions, Services, and Endpoints and act on updates to those resources. Hence they require credentials to access DPU host cluster. Each DPU will have a setting denoting the DPU host to which it is associated.  

## K8s Settings on DPU Host

The following node labels must be set on the DPU Host prior to installing OVN K8s CNI

```
k8s.ovn.org/dpu-host= 
k8s.ovn.org/zone-name=<dpu-host node name>
```

### Launching OVN K8s DPU Host cluster

The helm charts are under helm/ovn-kubernetes
Use values-single-node-zone.yaml with ovnkube-node-dpu-host tag set to true
The field OVNKUBE_NODE_MGMT_PORT_NETDEV or OVNKUBE_NODE_MGMT_PORT_DP_RESOURCE_NAME needs to be set in the ovnkube-node-dpu-host.yaml indicating the VF device or VF resource to be used for management port.

Launch OVN K8s using
```
helm install ovn-kubernetes . -f values-single-node-zone.yaml
```

## K8s Settings on DPU

The following node label is required on DPUs prior to installing OVN K8s CNI
```
k8s.ovn.org/dpu= 
```

## OVS settings on DPU
The following OVS settings are required on the DPU to enable hardware offloads and connect to the right DPU-host in the DPU-host cluster.

```
ovs-vsctl set Open_vSwitch . other_config:hw-offload=true
ovs-vsctl set Open_vSwitch . external-ids:host-k8s-nodename=""    -> Name of DPU-Host node
ovs-vsctl set Open_vSwitch . external-ids:ovn-gw-interface=""     -> Interface on the DPU that will be the gateway interface
ovs-vsctl set Open_vSwitch . external-ids:ovn-gw-nexthop=""       -> Default gateway address for the DPU-Host network
ovs-vsctl set Open_vSwitch . external_ids:ovn-gw-router-subnet="" -> Subnet to be used for the gateway router if DPU is in a different subnet than DPU-Host network
```

### Launching OVN K8s DPU cluster

Once the DPU-host cluster is deployed, the credentials to access that cluster is needed for DPU cluster deployment. It also requires additional information regarding OVN K8s configuration. 

Use values-single-node-zone-dpu.yaml for deploying the DPU cluster by setting the following fields

```
dpuHostClusterK8sAPIServer  - Endpoint of DPU Host cluster's K8s API server
dpuHostClusterK8sToken      - DPU Host cluster's K8s Access Token (scope limited to ovnkube-node serviceaccount)
dpuHostClusterK8sCACertData - DPU Host cluster's K8s Access Certs Data
dpuHostClusterNetworkCIDR   - DPU Host cluster's Network CIDR
dpuHostClusterServiceCIDR   - DPU Host cluster's Service CIDR
mtu                         - MTU of network interface in K8s pod
```

Launch OVN K8s using
```
helm install ovn-kubernetes . -f values-single-node-zone-dpu.yaml
```
