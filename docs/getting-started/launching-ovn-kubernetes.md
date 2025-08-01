# Launching OVN-Kubernetes

This guide provides multiple methods for deploying OVN-Kubernetes as your CNI in different environments, from local development to production clusters.

## Overview

OVN-Kubernetes can be deployed in several ways:

- **Kind Clusters**: Local development and testing (see [KIND Guide](./launching-ovn-kubernetes-on-kind.md))
- **Kubeadm Clusters**: Manual cluster setup
- **Cloud Providers**: AWS, GCP, Azure deployments
- **Bare Metal**: Physical servers and VMs
- **Container Platforms**: OpenShift, Rancher, etc.

## Prerequisites

### System Requirements

- **Kubernetes**: Version 1.22 or later
- **Operating System**: Linux (Ubuntu 18.04+, CentOS 7+, RHEL 8+)
- **Memory**: Minimum 2GB per node (4GB+ recommended)
- **CPU**: Minimum 2 cores per node
- **Network**: Open ports for OVN communication

### Required Components

- **OVN**: Open Virtual Network (automatically managed)
- **OVS**: Open vSwitch (automatically installed)
- **Container Runtime**: Docker, containerd, or CRI-O

## Method 1: Fresh Kubeadm Cluster

### Step 1: Prepare Nodes

On all nodes (master and workers):

```bash
# Update system
sudo apt-get update && sudo apt-get upgrade -y

# Install Docker
curl -fsSL https://get.docker.com -o get-docker.sh
sudo sh get-docker.sh
sudo usermod -aG docker $USER

# Install kubeadm, kubelet, and kubectl
sudo apt-get update
sudo apt-get install -y apt-transport-https ca-certificates curl
curl -fsSLo /usr/share/keyrings/kubernetes-archive-keyring.gpg https://packages.cloud.google.com/apt/doc/apt-key.gpg
echo "deb [signed-by=/usr/share/keyrings/kubernetes-archive-keyring.gpg] https://apt.kubernetes.io/ kubernetes-xenial main" | sudo tee /etc/apt/sources.list.d/kubernetes.list
sudo apt-get update
sudo apt-get install -y kubelet kubeadm kubectl
sudo apt-mark hold kubelet kubeadm kubectl
```

### Step 2: Initialize Master Node

```bash
# Initialize cluster without CNI
sudo kubeadm init \
  --pod-network-cidr=10.244.0.0/16 \
  --service-cidr=10.96.0.0/12 \
  --skip-phases=addon/kube-proxy

# Set up kubectl for current user
mkdir -p $HOME/.kube
sudo cp -i /etc/kubernetes/admin.conf $HOME/.kube/config
sudo chown $(id -u):$(id -g) $HOME/.kube/config

# Remove master node taint (for single-node clusters)
kubectl taint nodes --all node-role.kubernetes.io/control-plane-
```

### Step 3: Deploy OVN-Kubernetes

```bash
# Download OVN-Kubernetes manifests
wget https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/daemonset.sh
chmod +x daemonset.sh

# Deploy OVN-Kubernetes
./daemonset.sh \
  --image=ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:master \
  --net-cidr=10.244.0.0/16 \
  --svc-cidr=10.96.0.0/12 \
  --gateway-mode=shared \
  --k8s-apiserver=https://$(hostname -I | awk '{print $1}'):6443
```

### Step 4: Join Worker Nodes

On each worker node, use the join command from kubeadm init output:

```bash
# Example join command (use your actual token and hash)
sudo kubeadm join MASTER_IP:6443 \
  --token TOKEN \
  --discovery-token-ca-cert-hash sha256:HASH
```

## Method 2: Existing Cluster Migration

### Step 1: Backup Current CNI

```bash
# Backup existing CNI configuration
mkdir -p ~/cni-backup
kubectl get pods -n kube-system -o yaml > ~/cni-backup/pods.yaml
kubectl get daemonsets -n kube-system -o yaml > ~/cni-backup/daemonsets.yaml

# Save current CNI configs
sudo cp -r /etc/cni/net.d/ ~/cni-backup/
```

### Step 2: Remove Existing CNI

```bash
# Remove current CNI (example for Flannel)
kubectl delete -f https://raw.githubusercontent.com/flannel-io/flannel/master/Documentation/kube-flannel.yml

# Remove CNI configurations
sudo rm -rf /etc/cni/net.d/*
sudo rm -rf /opt/cni/bin/*

# Restart kubelet on all nodes
sudo systemctl restart kubelet
```

### Step 3: Deploy OVN-Kubernetes

```bash
# Deploy OVN-Kubernetes
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-db.yaml
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-master.yaml
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-node.yaml

# Wait for pods to be ready
kubectl wait --for=condition=ready pod -l app=ovnkube-master -n ovn-kubernetes --timeout=300s
kubectl wait --for=condition=ready pod -l app=ovnkube-node -n ovn-kubernetes --timeout=300s
```

## Method 3: Helm Deployment

### Step 1: Install Helm

```bash
# Install Helm 3
curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

# Add OVN-Kubernetes Helm repository
helm repo add ovn-kubernetes https://ovn-org.github.io/ovn-kubernetes/
helm repo update
```

### Step 2: Configure Values

Create a values file for your environment:

```yaml
# values.yaml
global:
  image:
    repository: ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u
    tag: master
  
networking:
    netCIDR: "10.244.0.0/16"
    svcCIDR: "10.96.0.0/12"
    joinSubnetCIDR: "100.64.0.0/16"
    gatewayMode: "shared"

ovnkube-master:
  replicas: 1
  
ovnkube-node:
  enabled: true
  
ovnkube-db:
  replicas: 1
  storage:
    size: "10Gi"
    storageClass: "standard"
```

### Step 3: Deploy with Helm

```bash
# Deploy OVN-Kubernetes
helm install ovn-kubernetes ovn-kubernetes/ovn-kubernetes \
  -n ovn-kubernetes \
  --create-namespace \
  -f values.yaml

# Check deployment status
helm status ovn-kubernetes -n ovn-kubernetes
kubectl get pods -n ovn-kubernetes
```

## Method 4: Cloud Provider Deployments

### AWS EKS

```bash
# Create EKS cluster without CNI
eksctl create cluster \
  --name ovn-cluster \
  --region us-west-2 \
  --nodes 3 \
  --nodes-min 1 \
  --nodes-max 4 \
  --without-nodegroup \
  --vpc-cidr 10.0.0.0/16

# Create managed node group
eksctl create nodegroup \
  --cluster ovn-cluster \
  --region us-west-2 \
  --name ovn-nodes \
  --node-type m5.large \
  --nodes 3 \
  --nodes-min 1 \
  --nodes-max 4 \
  --disable-pod-imds

# Deploy OVN-Kubernetes
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-db.yaml
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-master.yaml
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-node.yaml
```

### GKE (Google Kubernetes Engine)

```bash
# Create GKE cluster with no default CNI
gcloud container clusters create ovn-cluster \
  --machine-type n1-standard-2 \
  --num-nodes 3 \
  --zone us-central1-a \
  --network-policy \
  --enable-ip-alias \
  --cluster-ipv4-cidr=10.244.0.0/16 \
  --services-ipv4-cidr=10.96.0.0/12

# Get credentials
gcloud container clusters get-credentials ovn-cluster --zone us-central1-a

# Deploy OVN-Kubernetes
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-db.yaml
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-master.yaml
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-node.yaml
```

### Azure AKS

```bash
# Create AKS cluster
az aks create \
  --resource-group myResourceGroup \
  --name ovn-cluster \
  --node-count 3 \
  --node-vm-size Standard_DS2_v2 \
  --network-plugin none \
  --pod-cidr 10.244.0.0/16 \
  --service-cidr 10.96.0.0/12

# Get credentials
az aks get-credentials --resource-group myResourceGroup --name ovn-cluster

# Deploy OVN-Kubernetes
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-db.yaml
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-master.yaml
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-node.yaml
```

## Method 5: Bare Metal Deployment

### Step 1: Prepare Infrastructure

```bash
# On all nodes, ensure required kernel modules
sudo modprobe openvswitch
sudo modprobe geneve
sudo modprobe ip_tables
sudo modprobe ip6_tables

# Add to startup
echo 'openvswitch' | sudo tee -a /etc/modules-load.d/ovn-kubernetes.conf
echo 'geneve' | sudo tee -a /etc/modules-load.d/ovn-kubernetes.conf
echo 'ip_tables' | sudo tee -a /etc/modules-load.d/ovn-kubernetes.conf
echo 'ip6_tables' | sudo tee -a /etc/modules-load.d/ovn-kubernetes.conf
```

### Step 2: Network Configuration

```bash
# Configure firewall rules for OVN
sudo ufw allow 6641/tcp  # OVN Northbound DB
sudo ufw allow 6642/tcp  # OVN Southbound DB
sudo ufw allow 6643/tcp  # OVSDB Manager
sudo ufw allow 6081/udp  # Geneve
```

### Step 3: Deploy OVN-Kubernetes

```bash
# Create configuration file
cat > ovn-config.yaml << EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ovn-config
  namespace: ovn-kubernetes
data:
  net_cidr: "10.244.0.0/16"
  svc_cidr: "10.96.0.0/12"
  gateway_mode: "shared"
  enable_ssl: "false"
EOF

# Apply configuration and deploy
kubectl create namespace ovn-kubernetes
kubectl apply -f ovn-config.yaml
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-db.yaml
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-master.yaml
kubectl apply -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-node.yaml
```

## Verification and Testing

### Check OVN-Kubernetes Status

```bash
# Check all OVN-Kubernetes pods
kubectl get pods -n ovn-kubernetes

# Check node status
kubectl get nodes

# Verify CNI installation
kubectl get pods -n kube-system

# Check ovn-kubernetes logs
kubectl logs -n ovn-kubernetes -l app=ovnkube-master
kubectl logs -n ovn-kubernetes -l app=ovnkube-node
```

### Network Connectivity Tests

```bash
# Create test pods
kubectl run test1 --image=busybox:1.35 --command -- sleep 3600
kubectl run test2 --image=busybox:1.35 --command -- sleep 3600

# Test pod-to-pod connectivity
kubectl exec test1 -- ping -c 3 $(kubectl get pod test2 -o jsonpath='{.status.podIP}')

# Test DNS resolution
kubectl exec test1 -- nslookup kubernetes.default.svc.cluster.local

# Test external connectivity
kubectl exec test1 -- ping -c 3 8.8.8.8
```

### Performance Validation

```bash
# Network performance test
kubectl run iperf-server --image=networkstatic/iperf3 -- iperf3 -s
kubectl run iperf-client --image=networkstatic/iperf3 --rm -it -- \
  iperf3 -c $(kubectl get pod iperf-server -o jsonpath='{.status.podIP}')
```

## Configuration Options

### Common Configuration Parameters

```yaml
# ConfigMap example for advanced configuration
apiVersion: v1
kind: ConfigMap
metadata:
  name: ovn-config
  namespace: ovn-kubernetes
data:
  # Network CIDRs
  net_cidr: "10.244.0.0/16"
  svc_cidr: "10.96.0.0/12"
  
  # Gateway configuration
  gateway_mode: "shared"  # or "local"
  gateway_opts: ""
  
  # OVN Database
  db_nb_addr: "ssl:ovnkube-db.ovn-kubernetes.svc.cluster.local:6641"
  db_sb_addr: "ssl:ovnkube-db.ovn-kubernetes.svc.cluster.local:6642"
  
  # Features
  enable_multicast: "false"
  enable_egress_ip: "true"
  enable_egress_firewall: "true"
  
  # Logging
  loglevel: "4"
  logfile_maxsize: "100"
  logfile_maxage: "5"
  logfile_maxbackups: "5"
```

## Troubleshooting

### Common Issues

#### Pods Stuck in ContainerCreating

```bash
# Check CNI logs
sudo journalctl -u kubelet | grep CNI

# Check OVN-Kubernetes logs
kubectl logs -n ovn-kubernetes -l app=ovnkube-node

# Verify CNI binary and config
ls -la /opt/cni/bin/
ls -la /etc/cni/net.d/
```

#### OVN Database Connection Issues

```bash
# Check database pods
kubectl get pods -n ovn-kubernetes -l app=ovnkube-db

# Test database connectivity
kubectl exec -n ovn-kubernetes -l app=ovnkube-master -- \
  ovn-nbctl --db=ssl:ovnkube-db.ovn-kubernetes.svc.cluster.local:6641 show
```

#### Network Connectivity Problems

```bash
# Use ovnkube-trace for debugging
kubectl exec -n ovn-kubernetes -l app=ovnkube-master -- \
  ovnkube-trace --src-namespace=default --src-pod=test1 \
                --dst-namespace=default --dst-pod=test2

# Check OVS status on nodes
kubectl exec -n ovn-kubernetes -l app=ovnkube-node -- ovs-vsctl show
```

## Upgrade Procedures

### Rolling Update

```bash
# Update image tag in daemonsets
kubectl patch daemonset ovnkube-node -n ovn-kubernetes \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"ovnkube-node","image":"ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:v1.0.0"}]}}}}'

# Monitor rollout
kubectl rollout status daemonset/ovnkube-node -n ovn-kubernetes

# Update master deployment
kubectl patch deployment ovnkube-master -n ovn-kubernetes \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"ovnkube-master","image":"ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:v1.0.0"}]}}}}'
```

## Cleanup

### Uninstall OVN-Kubernetes

```bash
# Remove OVN-Kubernetes components
kubectl delete -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-node.yaml
kubectl delete -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-master.yaml
kubectl delete -f https://raw.githubusercontent.com/ovn-org/ovn-kubernetes/master/dist/images/ovnkube-db.yaml

# Remove namespace
kubectl delete namespace ovn-kubernetes

# Clean up node configurations
sudo rm -rf /etc/cni/net.d/10-ovn-kubernetes.conf
sudo rm -rf /opt/cni/bin/ovn-k8s-cni-overlay
```

## Next Steps

- Configure [Advanced Features](../features/)
- Set up [Monitoring and Observability](../observability/)
- Implement [Network Security](../features/network-security-controls/)
- Deploy [Sample Applications](./example-pod-creation.md)
- Learn about [Troubleshooting](../troubleshooting/)

For production deployments, also review the [Configuration Guide](./configuration.md) and [Best Practices](../developer-guide/best-practices.md).
