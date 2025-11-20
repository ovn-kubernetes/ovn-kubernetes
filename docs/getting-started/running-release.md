# Running OVN-Kubernetes from Release

This guide explains how to deploy OVN-Kubernetes using official release binaries and container images, providing a stable and tested deployment option for production environments.

## Overview

OVN-Kubernetes releases provide:

- **Pre-built container images**: Ready-to-use images from GitHub Container Registry
- **Release binaries**: Compiled binaries for different platforms
- **Helm charts**: Versioned Helm charts for easy deployment
- **Manifests**: YAML manifests for manual deployment
- **Release notes**: Detailed changelog and upgrade instructions

## Release Channels

### Stable Releases

Stable releases are thoroughly tested and recommended for production:

- **Latest Stable**: Current production-ready version
- **LTS Releases**: Long-term support versions
- **Patch Releases**: Bug fixes and security updates

### Release Schedule

- **Major releases**: Every 6 months
- **Minor releases**: Monthly or as needed
- **Patch releases**: Weekly or as needed for critical fixes

## Finding Releases

### GitHub Releases

Visit the [OVN-Kubernetes Releases](https://github.com/ovn-org/ovn-kubernetes/releases) page to find:

- Release notes and changelogs
- Container image tags
- Binary downloads
- Helm chart versions

### Container Images

Official images are available at:

```bash
# GitHub Container Registry
ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:v1.0.0

# Check available tags
docker pull ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:latest
docker images | grep ovn-kubernetes
```

## Method 1: Using Release Manifests

### Step 1: Download Release Manifests

```bash
# Set release version
export OVN_VERSION="v1.0.0"

# Download manifests
curl -LO https://github.com/ovn-org/ovn-kubernetes/releases/download/${OVN_VERSION}/ovnkube-db.yaml
curl -LO https://github.com/ovn-org/ovn-kubernetes/releases/download/${OVN_VERSION}/ovnkube-master.yaml
curl -LO https://github.com/ovn-org/ovn-kubernetes/releases/download/${OVN_VERSION}/ovnkube-node.yaml

# Verify downloads
ls -la ovnkube-*.yaml
```

### Step 2: Configure Network Parameters

Edit the manifests to match your environment:

```bash
# Set your network configuration
export POD_CIDR="10.244.0.0/16"
export SERVICE_CIDR="10.96.0.0/12"
export API_SERVER="https://$(hostname -I | awk '{print $1}'):6443"

# Update manifests with your configuration
sed -i "s|--cluster-subnets=.*|--cluster-subnets=${POD_CIDR}|g" ovnkube-*.yaml
sed -i "s|--service-cluster-ip-range=.*|--service-cluster-ip-range=${SERVICE_CIDR}|g" ovnkube-*.yaml
sed -i "s|--k8s-apiserver=.*|--k8s-apiserver=${API_SERVER}|g" ovnkube-*.yaml
```

### Step 3: Deploy Components

```bash
# Create namespace
kubectl create namespace ovn-kubernetes

# Deploy database first
kubectl apply -f ovnkube-db.yaml

# Wait for database to be ready
kubectl wait --for=condition=ready pod -l app=ovnkube-db -n ovn-kubernetes --timeout=300s

# Deploy master components
kubectl apply -f ovnkube-master.yaml

# Wait for master to be ready
kubectl wait --for=condition=ready pod -l app=ovnkube-master -n ovn-kubernetes --timeout=300s

# Deploy node components
kubectl apply -f ovnkube-node.yaml

# Wait for nodes to be ready
kubectl wait --for=condition=ready pod -l app=ovnkube-node -n ovn-kubernetes --timeout=300s
```

## Method 2: Using Helm Charts

### Step 1: Add Helm Repository

```bash
# Add the OVN-Kubernetes Helm repository
helm repo add ovn-kubernetes https://ovn-org.github.io/ovn-kubernetes/
helm repo update

# List available chart versions
helm search repo ovn-kubernetes --versions
```

### Step 2: Create Values File

```yaml
# values-production.yaml
global:
  image:
    repository: ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u
    tag: "v1.0.0"  # Specify exact version
    pullPolicy: IfNotPresent

networking:
  netCIDR: "10.244.0.0/16"
  svcCIDR: "10.96.0.0/12"
  joinSubnetCIDR: "100.64.0.0/16"
  gatewayMode: "shared"

# Database configuration
ovnkube-db:
  replicas: 1
  persistence:
    enabled: true
    size: "20Gi"
    storageClass: "fast-ssd"
  resources:
    limits:
      cpu: "2"
      memory: "4Gi"
    requests:
      cpu: "1"
      memory: "2Gi"

# Master configuration
ovnkube-master:
  replicas: 1
  resources:
    limits:
      cpu: "2"
      memory: "2Gi"
    requests:
      cpu: "500m"
      memory: "1Gi"

# Node configuration
ovnkube-node:
  enabled: true
  resources:
    limits:
      cpu: "1"
      memory: "1Gi"
    requests:
      cpu: "250m"
      memory: "512Mi"

# Security settings
security:
  ssl:
    enabled: true
  rbac:
    create: true
  podSecurityPolicy:
    enabled: false

# Monitoring
monitoring:
  enabled: true
  prometheus:
    enabled: true
    port: 9409
```

### Step 3: Install with Helm

```bash
# Install OVN-Kubernetes
helm install ovn-kubernetes ovn-kubernetes/ovn-kubernetes \
  --namespace ovn-kubernetes \
  --create-namespace \
  --values values-production.yaml \
  --version v1.0.0

# Check installation status
helm status ovn-kubernetes -n ovn-kubernetes

# List all releases
helm list -n ovn-kubernetes
```

## Method 3: Using Release Binaries

### Step 1: Download Binaries

```bash
# Set architecture and version
export ARCH="amd64"  # or arm64
export OVN_VERSION="v1.0.0"

# Download binaries
curl -LO https://github.com/ovn-org/ovn-kubernetes/releases/download/${OVN_VERSION}/ovnkube-linux-${ARCH}
curl -LO https://github.com/ovn-org/ovn-kubernetes/releases/download/${OVN_VERSION}/ovn-k8s-cni-overlay-linux-${ARCH}
curl -LO https://github.com/ovn-org/ovn-kubernetes/releases/download/${OVN_VERSION}/ovn-kube-util-linux-${ARCH}

# Make binaries executable
chmod +x ovnkube-linux-${ARCH}
chmod +x ovn-k8s-cni-overlay-linux-${ARCH}
chmod +x ovn-kube-util-linux-${ARCH}

# Install binaries
sudo mv ovnkube-linux-${ARCH} /usr/local/bin/ovnkube
sudo mv ovn-k8s-cni-overlay-linux-${ARCH} /opt/cni/bin/ovn-k8s-cni-overlay
sudo mv ovn-kube-util-linux-${ARCH} /usr/local/bin/ovn-kube-util
```

### Step 2: Create Configuration

```bash
# Create configuration directory
sudo mkdir -p /etc/ovn-kubernetes

# Create main configuration file
cat > /etc/ovn-kubernetes/ovnkube.conf << EOF
[default]
cluster-subnets=10.244.0.0/16
service-cluster-ip-range=10.96.0.0/12
encap-type=geneve

[kubernetes]
k8s-kubeconfig=/etc/kubernetes/admin.conf
k8s-apiserver=https://$(hostname -I | awk '{print $1}'):6443

[ovnkube-node]
ovn-empty-lb-events=true

[logging]
loglevel=4
logfile=/var/log/ovn-kubernetes/ovnkube.log
logfile-maxsize=100
logfile-maxbackups=5
logfile-maxage=5
EOF
```

### Step 3: Create Systemd Services

#### Master Node Service

```bash
# Create ovnkube-master service
cat > /etc/systemd/system/ovnkube-master.service << EOF
[Unit]
Description=OVN-Kubernetes Master
After=network.target

[Service]
Type=exec
ExecStart=/usr/local/bin/ovnkube \
  --init-master \$(hostname) \
  --config-file=/etc/ovn-kubernetes/ovnkube.conf \
  --loglevel=4 \
  --logfile=/var/log/ovn-kubernetes/ovnkube-master.log
Restart=always
RestartSec=10
User=root

[Install]
WantedBy=multi-user.target
EOF

# Enable and start service
sudo systemctl daemon-reload
sudo systemctl enable ovnkube-master
sudo systemctl start ovnkube-master
```

#### Worker Node Service

```bash
# Create ovnkube-node service
cat > /etc/systemd/system/ovnkube-node.service << EOF
[Unit]
Description=OVN-Kubernetes Node
After=network.target

[Service]
Type=exec
ExecStart=/usr/local/bin/ovnkube \
  --init-node \$(hostname) \
  --config-file=/etc/ovn-kubernetes/ovnkube.conf \
  --loglevel=4 \
  --logfile=/var/log/ovn-kubernetes/ovnkube-node.log
Restart=always
RestartSec=10
User=root

[Install]
WantedBy=multi-user.target
EOF

# Enable and start service
sudo systemctl daemon-reload
sudo systemctl enable ovnkube-node
sudo systemctl start ovnkube-node
```

## Method 4: Cloud-Specific Deployments

### AWS EKS with Release Images

```bash
# Create EKS cluster
eksctl create cluster \
  --name ovn-prod \
  --version 1.24 \
  --region us-west-2 \
  --nodegroup-name ovn-nodes \
  --node-type m5.large \
  --nodes 3 \
  --vpc-cidr 10.0.0.0/16

# Deploy specific release version
kubectl apply -f https://github.com/ovn-org/ovn-kubernetes/releases/download/v1.0.0/ovnkube-db.yaml
kubectl apply -f https://github.com/ovn-org/ovn-kubernetes/releases/download/v1.0.0/ovnkube-master.yaml
kubectl apply -f https://github.com/ovn-org/ovn-kubernetes/releases/download/v1.0.0/ovnkube-node.yaml
```

### GKE with Release Images

```bash
# Create GKE cluster
gcloud container clusters create ovn-prod \
  --machine-type n1-standard-2 \
  --num-nodes 3 \
  --zone us-central1-a \
  --cluster-version latest \
  --enable-ip-alias \
  --cluster-ipv4-cidr=10.244.0.0/16

# Deploy release version
kubectl apply -f https://github.com/ovn-org/ovn-kubernetes/releases/download/v1.0.0/ovnkube-db.yaml
kubectl apply -f https://github.com/ovn-org/ovn-kubernetes/releases/download/v1.0.0/ovnkube-master.yaml
kubectl apply -f https://github.com/ovn-org/ovn-kubernetes/releases/download/v1.0.0/ovnkube-node.yaml
```

## Verification and Validation

### Check Release Version

```bash
# Check container image versions
kubectl get pods -n ovn-kubernetes -o jsonpath='{.items[*].spec.containers[*].image}' | tr ' ' '\n' | sort -u

# Check binary versions
kubectl exec -n ovn-kubernetes -l app=ovnkube-master -- ovnkube --version
kubectl exec -n ovn-kubernetes -l app=ovnkube-node -- ovnkube --version
```

### Validate Installation

```bash
# Check all pods are running
kubectl get pods -n ovn-kubernetes

# Verify network functionality
kubectl run test-pod --image=busybox:1.35 --rm -it -- sh
# Inside pod, test connectivity:
ping -c 3 8.8.8.8
nslookup kubernetes.default.svc.cluster.local

# Check OVN databases
kubectl exec -n ovn-kubernetes -l app=ovnkube-master -- \
  ovn-nbctl --db=ssl:ovnkube-db.ovn-kubernetes.svc.cluster.local:6641 show
```

## Monitoring Release Health

### Prometheus Metrics

```bash
# Enable metrics collection
kubectl port-forward -n ovn-kubernetes svc/ovnkube-master 9409:9409

# Access metrics
curl http://localhost:9409/metrics | grep ovn_

# Common metrics to monitor:
# - ovn_controller_integration_bridge_openflow_rules
# - ovn_nb_e2e_timestamp
# - ovn_sb_e2e_timestamp
```

### Health Checks

```bash
# Create health check script
cat > check-ovn-health.sh << 'EOF'
#!/bin/bash

echo "=== OVN-Kubernetes Health Check ==="

# Check pod status
echo "Checking pod status..."
kubectl get pods -n ovn-kubernetes

# Check node status
echo "Checking node status..."
kubectl get nodes

# Test connectivity
echo "Testing pod connectivity..."
kubectl run health-test --image=busybox:1.35 --rm -it -- \
  ping -c 1 8.8.8.8 && echo "External connectivity: OK" || echo "External connectivity: FAIL"

# Check OVN databases
echo "Checking OVN databases..."
kubectl exec -n ovn-kubernetes -l app=ovnkube-master -- \
  ovn-nbctl --db=ssl:ovnkube-db.ovn-kubernetes.svc.cluster.local:6641 list logical_switch | head -5

echo "=== Health Check Complete ==="
EOF

chmod +x check-ovn-health.sh
./check-ovn-health.sh
```

## Upgrading Between Releases

### Preparation

```bash
# Backup current configuration
kubectl get all -n ovn-kubernetes -o yaml > ovn-backup-$(date +%Y%m%d).yaml

# Check current version
kubectl get pods -n ovn-kubernetes -o jsonpath='{.items[0].spec.containers[0].image}'

# Read release notes for the target version
curl -s https://api.github.com/repos/ovn-org/ovn-kubernetes/releases/tags/v1.1.0 | jq '.body'
```

### Rolling Update

```bash
# Update to new release version
export NEW_VERSION="v1.1.0"

# Update database first
kubectl patch statefulset ovnkube-db -n ovn-kubernetes \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"ovnkube-db","image":"ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:'${NEW_VERSION}'"}]}}}}'

# Wait for database update
kubectl rollout status statefulset/ovnkube-db -n ovn-kubernetes

# Update master
kubectl patch deployment ovnkube-master -n ovn-kubernetes \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"ovnkube-master","image":"ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:'${NEW_VERSION}'"}]}}}}'

# Wait for master update
kubectl rollout status deployment/ovnkube-master -n ovn-kubernetes

# Update nodes
kubectl patch daemonset ovnkube-node -n ovn-kubernetes \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"ovnkube-node","image":"ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-u:'${NEW_VERSION}'"}]}}}}'

# Monitor node update
kubectl rollout status daemonset/ovnkube-node -n ovn-kubernetes
```

### Validation After Upgrade

```bash
# Verify new version
kubectl get pods -n ovn-kubernetes -o jsonpath='{.items[*].spec.containers[*].image}' | tr ' ' '\n' | sort -u

# Test functionality
kubectl run upgrade-test --image=busybox:1.35 --rm -it -- sh
# Test network connectivity

# Check for any issues
kubectl get events -n ovn-kubernetes --sort-by=.metadata.creationTimestamp
```

## Rollback Procedures

### Quick Rollback

```bash
# Rollback to previous version
kubectl rollout undo daemonset/ovnkube-node -n ovn-kubernetes
kubectl rollout undo deployment/ovnkube-master -n ovn-kubernetes
kubectl rollout undo statefulset/ovnkube-db -n ovn-kubernetes

# Monitor rollback
kubectl rollout status daemonset/ovnkube-node -n ovn-kubernetes
kubectl rollout status deployment/ovnkube-master -n ovn-kubernetes
kubectl rollout status statefulset/ovnkube-db -n ovn-kubernetes
```

### Complete Restoration

```bash
# Restore from backup if needed
kubectl delete namespace ovn-kubernetes
kubectl apply -f ovn-backup-$(date +%Y%m%d).yaml
```

## Best Practices for Production

### Version Management

- **Pin specific versions**: Avoid using `latest` tags in production
- **Test upgrades**: Always test in staging environment first
- **Monitor after upgrades**: Watch metrics and logs closely
- **Have rollback plan**: Prepare rollback procedures before upgrading

### Release Monitoring

- **Subscribe to releases**: Watch the GitHub repository for new releases
- **Read release notes**: Understand changes and potential impacts
- **Security updates**: Prioritize security-related releases
- **Compatibility**: Verify Kubernetes version compatibility

### Automation

```bash
# Create automated deployment script
cat > deploy-ovn-release.sh << 'EOF'
#!/bin/bash

set -e

VERSION=${1:-"latest"}
NAMESPACE="ovn-kubernetes"

echo "Deploying OVN-Kubernetes ${VERSION}..."

# Download manifests
curl -LO https://github.com/ovn-org/ovn-kubernetes/releases/download/${VERSION}/ovnkube-db.yaml
curl -LO https://github.com/ovn-org/ovn-kubernetes/releases/download/${VERSION}/ovnkube-master.yaml
curl -LO https://github.com/ovn-org/ovn-kubernetes/releases/download/${VERSION}/ovnkube-node.yaml

# Create namespace if it doesn't exist
kubectl create namespace ${NAMESPACE} --dry-run=client -o yaml | kubectl apply -f -

# Deploy components
kubectl apply -f ovnkube-db.yaml
kubectl wait --for=condition=ready pod -l app=ovnkube-db -n ${NAMESPACE} --timeout=300s

kubectl apply -f ovnkube-master.yaml
kubectl wait --for=condition=ready pod -l app=ovnkube-master -n ${NAMESPACE} --timeout=300s

kubectl apply -f ovnkube-node.yaml
kubectl wait --for=condition=ready pod -l app=ovnkube-node -n ${NAMESPACE} --timeout=300s

echo "Deployment complete!"
EOF

chmod +x deploy-ovn-release.sh
```

## Support and Resources

### Getting Help

- **GitHub Issues**: Report bugs and request features
- **Documentation**: Official documentation and guides
- **Community**: Slack channel and mailing list
- **Commercial Support**: Available from Red Hat and other vendors

### Useful Commands

```bash
# Check release information
kubectl get configmap -n ovn-kubernetes ovn-config -o yaml

# Monitor resource usage
kubectl top pods -n ovn-kubernetes
kubectl top nodes

# Collect logs for support
kubectl logs -n ovn-kubernetes -l app=ovnkube-master --previous > ovn-master.log
kubectl logs -n ovn-kubernetes -l app=ovnkube-node --previous > ovn-node.log
```

## Next Steps

- Set up [Monitoring and Alerts](../observability/)
- Configure [Network Policies](../features/network-security-controls/)
- Implement [Backup and Recovery](../operations/backup-recovery.md)
- Learn [Troubleshooting](../troubleshooting/)
- Explore [Advanced Features](../features/)

For more information about specific releases, visit the [OVN-Kubernetes Releases](https://github.com/ovn-org/ovn-kubernetes/releases) page.
