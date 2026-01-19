# Running OVN-Kubernetes with RPM Packages

This guide explains how to install and configure OVN-Kubernetes using RPM packages on Red Hat-based systems including RHEL, CentOS, Fedora, and derivatives.

## Overview

RPM package installation provides:

- **System Integration**: Native systemd services and configuration
- **Dependency Management**: Automatic dependency resolution
- **Security Updates**: Integrated with system package management
- **Enterprise Support**: Backed by Red Hat Enterprise Linux
- **Simplified Management**: Standard package management tools

## Prerequisites

### Supported Distributions

- **Red Hat Enterprise Linux (RHEL)**: 8.x, 9.x
- **CentOS Stream**: 8, 9
- **Fedora**: 35+
- **Rocky Linux**: 8.x, 9.x
- **AlmaLinux**: 8.x, 9.x

### System Requirements

- **Kubernetes**: 1.22+ installed and configured
- **Memory**: Minimum 4GB RAM (8GB+ recommended)
- **CPU**: Minimum 2 cores
- **Storage**: 20GB+ available space
- **Network**: Internet access for package downloads

## Package Repositories

### RHEL/CentOS with OpenShift

OVN-Kubernetes is included in OpenShift repositories:

```bash
# Enable OpenShift repositories (RHEL 8)
sudo subscription-manager repos --enable=rhocp-4.12-for-rhel-8-x86_64-rpms

# For CentOS Stream
sudo dnf config-manager --add-repo https://mirror.openshift.com/pub/openshift-v4/clients/ocp/stable/
```

### Fedora

OVN-Kubernetes is available in Fedora repositories:

```bash
# Update package cache
sudo dnf update

# Search for available packages
sudo dnf search ovn-kubernetes
```

### Third-Party Repositories

For the latest versions, you can use community repositories:

```bash
# Add OVN-Kubernetes COPR repository
sudo dnf copr enable @ovn/ovn-kubernetes

# Update package cache
sudo dnf update
```

## Installation Methods

### Method 1: Standard DNF/YUM Installation

#### Install Core Components

```bash
# Install OVN-Kubernetes packages
sudo dnf install -y ovn-kubernetes-master ovn-kubernetes-node ovn-kubernetes-cni

# Install OVN and OVS dependencies
sudo dnf install -y openvswitch ovn-central ovn-host

# Install additional utilities
sudo dnf install -y ovn-kubernetes-utils
```

#### Verify Installation

```bash
# Check installed packages
rpm -qa | grep ovn-kubernetes
rpm -qa | grep openvswitch
rpm -qa | grep ovn

# Check binary locations
which ovnkube
which ovn-k8s-cni-overlay
which ovn-kube-util
```

### Method 2: Package Groups

```bash
# Install all OVN-Kubernetes components
sudo dnf groupinstall "OVN-Kubernetes Networking"

# Or install specific groups
sudo dnf groupinstall "OVN-Kubernetes Master"
sudo dnf groupinstall "OVN-Kubernetes Node"
```

### Method 3: Local RPM Installation

Download and install RPM packages directly:

```bash
# Download RPM packages
wget https://example.com/packages/ovn-kubernetes-master-1.0.0-1.el8.x86_64.rpm
wget https://example.com/packages/ovn-kubernetes-node-1.0.0-1.el8.x86_64.rpm
wget https://example.com/packages/ovn-kubernetes-cni-1.0.0-1.el8.x86_64.rpm

# Install with dependencies
sudo dnf localinstall -y *.rpm
```

## Configuration

### Configure OVN Central (Master Nodes)

#### Database Configuration

```bash
# Configure OVN central services
sudo systemctl enable ovn-northd
sudo systemctl enable ovsdb-server-nb
sudo systemctl enable ovsdb-server-sb

# Create database directories
sudo mkdir -p /var/lib/ovn
sudo chown ovn:ovn /var/lib/ovn

# Initialize databases
sudo -u ovn /usr/share/ovn/scripts/ovn-ctl start_northd
```

#### OVN-Kubernetes Master Configuration

```bash
# Create configuration file
sudo mkdir -p /etc/ovn-kubernetes
cat > /etc/ovn-kubernetes/ovnkube.conf << EOF
[default]
cluster-subnets=10.244.0.0/16
service-cluster-ip-range=10.96.0.0/12
encap-type=geneve
gateway-mode=shared
enable-lflow-cache=true

[kubernetes]
k8s-kubeconfig=/etc/kubernetes/admin.conf
k8s-apiserver=https://$(hostname -I | awk '{print $1}'):6443
k8s-cacert=/etc/kubernetes/pki/ca.crt

[ovnkube-master]
nb-address=ssl:$(hostname -I | awk '{print $1}'):6641
sb-address=ssl:$(hostname -I | awk '{print $1}'):6642
enable-ssl=true

[logging]
loglevel=4
logfile=/var/log/ovn-kubernetes/ovnkube-master.log
logfile-maxsize=100
logfile-maxbackups=5
logfile-maxage=5
EOF

# Set proper permissions
sudo chown ovn:ovn /etc/ovn-kubernetes/ovnkube.conf
sudo chmod 640 /etc/ovn-kubernetes/ovnkube.conf
```

### Configure OVN Host (Worker Nodes)

#### OVS Configuration

```bash
# Enable and start Open vSwitch
sudo systemctl enable openvswitch
sudo systemctl start openvswitch

# Configure OVS
sudo ovs-vsctl set Open_vSwitch . external_ids:ovn-bridge=br-int
sudo ovs-vsctl set Open_vSwitch . external_ids:ovn-encap-type=geneve
sudo ovs-vsctl set Open_vSwitch . external_ids:ovn-encap-ip=$(hostname -I | awk '{print $1}')
```

#### OVN-Kubernetes Node Configuration

```bash
# Create node configuration
cat > /etc/ovn-kubernetes/ovnkube-node.conf << EOF
[default]
cluster-subnets=10.244.0.0/16
service-cluster-ip-range=10.96.0.0/12
encap-type=geneve
gateway-mode=shared

[kubernetes]
k8s-kubeconfig=/etc/kubernetes/kubelet.conf
k8s-apiserver=https://MASTER_IP:6443
k8s-cacert=/etc/kubernetes/pki/ca.crt

[ovnkube-node]
nb-address=ssl:MASTER_IP:6641
sb-address=ssl:MASTER_IP:6642
enable-ssl=true
mtu=1400

[logging]
loglevel=4
logfile=/var/log/ovn-kubernetes/ovnkube-node.log
logfile-maxsize=100
logfile-maxbackups=5
logfile-maxage=5
EOF

# Set permissions
sudo chown ovn:ovn /etc/ovn-kubernetes/ovnkube-node.conf
sudo chmod 640 /etc/ovn-kubernetes/ovnkube-node.conf
```

## SSL Certificate Configuration

### Generate Certificates

```bash
# Create certificate directory
sudo mkdir -p /etc/ovn-kubernetes/certs
cd /etc/ovn-kubernetes/certs

# Generate CA private key
sudo openssl genrsa -out ca-privkey.pem 2048

# Generate CA certificate
sudo openssl req -new -x509 -key ca-privkey.pem -out ca-cert.pem -days 365 \
  -subj "/C=US/ST=State/L=City/O=Organization/CN=OVN-Kubernetes-CA"

# Generate server private key
sudo openssl genrsa -out server-privkey.pem 2048

# Generate server certificate request
sudo openssl req -new -key server-privkey.pem -out server-req.pem \
  -subj "/C=US/ST=State/L=City/O=Organization/CN=$(hostname -f)"

# Sign server certificate
sudo openssl x509 -req -in server-req.pem -CA ca-cert.pem -CAkey ca-privkey.pem \
  -CAcreateserial -out server-cert.pem -days 365

# Set permissions
sudo chown -R ovn:ovn /etc/ovn-kubernetes/certs
sudo chmod 600 /etc/ovn-kubernetes/certs/*-privkey.pem
sudo chmod 644 /etc/ovn-kubernetes/certs/*-cert.pem
```

### Configure SSL in OVN

```bash
# Configure OVN northbound database SSL
sudo ovn-nbctl set-ssl /etc/ovn-kubernetes/certs/server-privkey.pem \
  /etc/ovn-kubernetes/certs/server-cert.pem \
  /etc/ovn-kubernetes/certs/ca-cert.pem

# Configure OVN southbound database SSL
sudo ovn-sbctl set-ssl /etc/ovn-kubernetes/certs/server-privkey.pem \
  /etc/ovn-kubernetes/certs/server-cert.pem \
  /etc/ovn-kubernetes/certs/ca-cert.pem
```

## Service Management

### Master Node Services

#### Enable and Start Services

```bash
# Enable OVN central services
sudo systemctl enable ovn-northd
sudo systemctl enable ovsdb-server-nb
sudo systemctl enable ovsdb-server-sb
sudo systemctl enable ovnkube-master

# Start services in order
sudo systemctl start ovsdb-server-nb
sudo systemctl start ovsdb-server-sb
sudo systemctl start ovn-northd
sudo systemctl start ovnkube-master

# Check service status
sudo systemctl status ovnkube-master
```

#### Service Configuration Files

```bash
# Edit master service configuration
sudo systemctl edit ovnkube-master

# Add configuration overrides
[Service]
Environment="CONFIG_FILE=/etc/ovn-kubernetes/ovnkube.conf"
ExecStart=
ExecStart=/usr/bin/ovnkube --init-master $(hostname) --config-file=${CONFIG_FILE}
```

### Worker Node Services

#### Enable and Start Services

```bash
# Enable services
sudo systemctl enable openvswitch
sudo systemctl enable ovn-controller
sudo systemctl enable ovnkube-node

# Start services
sudo systemctl start openvswitch
sudo systemctl start ovn-controller
sudo systemctl start ovnkube-node

# Check status
sudo systemctl status ovnkube-node
```

#### CNI Configuration

The RPM installation automatically configures CNI:

```bash
# Verify CNI configuration
cat /etc/cni/net.d/10-ovn-kubernetes.conf
ls -la /opt/cni/bin/ovn-k8s-cni-overlay
```

## Firewall Configuration

### Configure Firewalld

```bash
# Open required ports for OVN
sudo firewall-cmd --permanent --add-port=6641/tcp  # OVN NB
sudo firewall-cmd --permanent --add-port=6642/tcp  # OVN SB
sudo firewall-cmd --permanent --add-port=6081/udp  # Geneve
sudo firewall-cmd --permanent --add-port=6080/tcp  # OVS Manager

# Open Kubernetes ports
sudo firewall-cmd --permanent --add-port=6443/tcp  # API Server
sudo firewall-cmd --permanent --add-port=10250/tcp # Kubelet
sudo firewall-cmd --permanent --add-port=10251/tcp # Scheduler
sudo firewall-cmd --permanent --add-port=10252/tcp # Controller Manager

# Reload firewall
sudo firewall-cmd --reload

# Verify rules
sudo firewall-cmd --list-ports
```

### SELinux Configuration

```bash
# Check SELinux status
sestatus

# Install SELinux policy modules (if available)
sudo dnf install -y ovn-kubernetes-selinux

# Set SELinux contexts
sudo restorecon -R /etc/ovn-kubernetes/
sudo restorecon -R /var/log/ovn-kubernetes/
sudo restorecon -R /opt/cni/bin/

# Check for SELinux denials
sudo ausearch -m avc -ts recent | grep ovn
```

## Logging and Monitoring

### Configure Logging

```bash
# Create log directories
sudo mkdir -p /var/log/ovn-kubernetes
sudo chown ovn:ovn /var/log/ovn-kubernetes

# Configure logrotate
cat > /etc/logrotate.d/ovn-kubernetes << EOF
/var/log/ovn-kubernetes/*.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    create 644 ovn ovn
    postrotate
        /bin/systemctl reload ovnkube-master ovnkube-node 2>/dev/null || true
    endscript
}
EOF
```

### Configure Journald

```bash
# View OVN-Kubernetes logs
sudo journalctl -u ovnkube-master -f
sudo journalctl -u ovnkube-node -f
sudo journalctl -u ovn-northd -f

# Configure persistent logging
sudo mkdir -p /var/log/journal
sudo systemd-tmpfiles --create --prefix /var/log/journal
```

## Troubleshooting

### Common Issues

#### Package Dependencies

```bash
# Check for missing dependencies
sudo dnf check
sudo dnf deplist ovn-kubernetes-master

# Resolve broken dependencies
sudo dnf install --allowerasing
```

#### Service Failures

```bash
# Check service status
sudo systemctl status ovnkube-master
sudo systemctl status ovnkube-node

# View detailed logs
sudo journalctl -u ovnkube-master --no-pager
sudo journalctl -u ovnkube-node --no-pager

# Check configuration
sudo ovnkube --config-file=/etc/ovn-kubernetes/ovnkube.conf --help
```

#### Network Connectivity

```bash
# Test OVN database connectivity
sudo ovn-nbctl --db=ssl:localhost:6641 show
sudo ovn-sbctl --db=ssl:localhost:6642 show

# Check OVS status
sudo ovs-vsctl show
sudo ovs-ofctl dump-flows br-int
```

### Debug Commands

```bash
# Check package versions
rpm -qi ovn-kubernetes-master
rpm -qi ovn-kubernetes-node

# Verify file permissions
ls -la /etc/ovn-kubernetes/
ls -la /opt/cni/bin/

# Test connectivity
ping -c 3 MASTER_IP
telnet MASTER_IP 6641
```

## Updates and Maintenance

### Package Updates

```bash
# Check for updates
sudo dnf check-update ovn-kubernetes\*

# Update packages
sudo dnf update ovn-kubernetes\*

# Restart services after update
sudo systemctl restart ovnkube-master
sudo systemctl restart ovnkube-node
```

### Backup and Recovery

```bash
# Backup configuration
sudo tar -czf ovn-kubernetes-backup-$(date +%Y%m%d).tar.gz \
  /etc/ovn-kubernetes/ \
  /etc/cni/net.d/ \
  /var/lib/ovn/

# Backup OVN databases
sudo ovn-nbctl backup > nb-backup-$(date +%Y%m%d).db
sudo ovn-sbctl backup > sb-backup-$(date +%Y%m%d).db
```

### Automated Maintenance

```bash
# Create maintenance script
cat > /usr/local/bin/ovn-maintenance.sh << 'EOF'
#!/bin/bash

# Log rotation
/usr/sbin/logrotate /etc/logrotate.d/ovn-kubernetes

# Database maintenance
ovn-nbctl --db=ssl:localhost:6641 --timeout=10 ls > /dev/null
ovn-sbctl --db=ssl:localhost:6642 --timeout=10 ls > /dev/null

# Service health check
systemctl is-active --quiet ovnkube-master || systemctl restart ovnkube-master
systemctl is-active --quiet ovnkube-node || systemctl restart ovnkube-node

echo "$(date): OVN-Kubernetes maintenance completed"
EOF

chmod +x /usr/local/bin/ovn-maintenance.sh

# Add to cron
echo "0 2 * * * root /usr/local/bin/ovn-maintenance.sh >> /var/log/ovn-maintenance.log 2>&1" > /etc/cron.d/ovn-maintenance
```

## Integration with OpenShift

### OpenShift Deployment

```bash
# Install OpenShift with OVN-Kubernetes
openshift-install create cluster --dir=cluster-config

# Check OVN-Kubernetes status in OpenShift
oc get pods -n openshift-ovn-kubernetes
oc logs -n openshift-ovn-kubernetes -l app=ovnkube-master
```

### Managing with oc Commands

```bash
# Check network configuration
oc get network.config cluster -o yaml

# View cluster network operator
oc get clusteroperator network

# Check OVN configuration
oc get configmap -n openshift-ovn-kubernetes ovn-config -o yaml
```

## Performance Tuning

### System Optimization

```bash
# Tune kernel parameters
cat > /etc/sysctl.d/99-ovn-kubernetes.conf << EOF
net.core.rmem_max = 134217728
net.core.wmem_max = 134217728
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 1
net.bridge.bridge-nf-call-iptables = 0
net.bridge.bridge-nf-call-ip6tables = 0
EOF

sudo sysctl -p /etc/sysctl.d/99-ovn-kubernetes.conf
```

### OVS Performance

```bash
# Enable multi-queue
sudo ovs-vsctl set Open_vSwitch . other_config:n-handler-threads=4
sudo ovs-vsctl set Open_vSwitch . other_config:n-revalidator-threads=4

# Configure DPDK (if supported)
sudo ovs-vsctl set Open_vSwitch . other_config:dpdk-init=true
sudo ovs-vsctl set Open_vSwitch . other_config:dpdk-socket-mem="2048,2048"
```

## Security Hardening

### Service Security

```bash
# Run services with minimal privileges
sudo systemctl edit ovnkube-master
# Add:
[Service]
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/ovn-kubernetes
```

### Network Security

```bash
# Restrict access to OVN databases
sudo firewall-cmd --permanent --add-rich-rule='rule family="ipv4" source address="10.0.0.0/8" port protocol="tcp" port="6641" accept'
sudo firewall-cmd --permanent --add-rich-rule='rule family="ipv4" source address="10.0.0.0/8" port protocol="tcp" port="6642" accept'
sudo firewall-cmd --reload
```

## Next Steps

- Configure [Network Policies](../features/network-security-controls/)
- Set up [Monitoring](../observability/)
- Deploy [Sample Applications](./example-pod-creation.md)
- Learn [Troubleshooting](../troubleshooting/)
- Explore [Advanced Features](../features/)

## Support Resources

- **Red Hat Support**: For RHEL/OpenShift customers
- **Documentation**: Official OVN-Kubernetes docs
- **Community**: Slack and mailing lists
- **Bug Reports**: GitHub issues

For enterprise deployments, consider Red Hat OpenShift which includes supported OVN-Kubernetes packages and professional support.
