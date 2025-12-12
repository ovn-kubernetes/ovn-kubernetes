# OVN-Kubernetes CLI Guide

This guide covers the command-line tools and utilities provided by OVN-Kubernetes for administration, debugging, and troubleshooting.

## Overview

OVN-Kubernetes provides several CLI tools:

- **ovnkube**: Main controller daemon
- **ovn-kube-util**: Administrative utility tool
- **ovndbchecker**: Database consistency checker
- **ovn-k8s-cni-overlay**: CNI plugin binary
- **ovnkube-trace**: Network tracing utility

## ovnkube

The main OVN-Kubernetes controller daemon that manages the integration between Kubernetes and OVN.

### Usage

```bash
ovnkube [flags]
```

### Common Flags

#### Core Configuration
```bash
# Basic configuration
--cluster-subnets=<CIDR>           # Pod subnet (e.g., 10.244.0.0/16)
--service-cluster-ip-range=<CIDR>  # Service subnet (e.g., 10.96.0.0/12)
--k8s-kubeconfig=<path>            # Path to kubeconfig file
--k8s-apiserver=<URL>              # Kubernetes API server URL

# Node configuration
--init-node=<name>                 # Initialize specific node
--nodeport                         # Enable NodePort support
--nb-address=<URL>                 # OVN Northbound database address
--sb-address=<URL>                 # OVN Southbound database address
```

#### Networking Options
```bash
# Gateway configuration
--gateway-mode=<mode>              # Gateway mode: shared|local
--gateway-interface=<interface>    # Host interface for gateway
--gateway-nexthop=<IP>            # Gateway next hop IP
--gateway-v4-join-subnet=<CIDR>   # IPv4 join subnet
--gateway-v6-join-subnet=<CIDR>   # IPv6 join subnet

# Multi-homing
--enable-multi-network            # Enable multiple networks
--enable-multi-networkpolicy      # Enable multi-network policies

# Advanced features
--enable-egress-ip               # Enable EgressIP feature
--enable-egress-firewall         # Enable EgressFirewall feature
--enable-egress-qos              # Enable EgressQoS feature
```

#### Debugging and Logging
```bash
# Logging configuration
--loglevel=<level>               # Log level: 0-5 (0=panic, 5=trace)
--logfile=<path>                 # Log file path
--logfile-maxsize=<MB>           # Maximum log file size
--logfile-maxbackups=<num>       # Number of log file backups
--logfile-maxage=<days>          # Maximum age of log files

# Debug options
--ovn-empty-lb-events           # Log empty load balancer events
--metrics-enable-pprof          # Enable pprof metrics endpoint
--enable-profiling              # Enable profiling
```

### Example Configurations

#### Master Node
```bash
ovnkube \
  --cluster-subnets=10.244.0.0/16 \
  --service-cluster-ip-range=10.96.0.0/12 \
  --k8s-kubeconfig=/etc/kubernetes/admin.conf \
  --init-master=k8s-master \
  --nb-address=ssl:192.168.1.10:6641 \
  --sb-address=ssl:192.168.1.10:6642 \
  --nodeport \
  --loglevel=4
```

#### Worker Node
```bash
ovnkube \
  --cluster-subnets=10.244.0.0/16 \
  --service-cluster-ip-range=10.96.0.0/12 \
  --k8s-kubeconfig=/etc/kubernetes/kubelet.conf \
  --init-node=k8s-worker1 \
  --nb-address=ssl:192.168.1.10:6641 \
  --sb-address=ssl:192.168.1.10:6642 \
  --nodeport \
  --loglevel=4
```

## ovn-kube-util

Administrative utility for OVN-Kubernetes operations and debugging.

### Usage

```bash
ovn-kube-util [command] [flags]
```

### Available Commands

#### Network Information
```bash
# Show node subnet information
ovn-kube-util nics

# Display pod network information
ovn-kube-util pod-networks

# Show service information
ovn-kube-util services

# Display network policy information
ovn-kube-util network-policies
```

#### Database Operations
```bash
# Show OVN database information
ovn-kube-util show-db

# Backup OVN databases
ovn-kube-util backup-db --output-dir=/backup

# Restore OVN databases
ovn-kube-util restore-db --backup-dir=/backup

# Check database consistency
ovn-kube-util check-db
```

#### Node Operations
```bash
# Initialize a node
ovn-kube-util init-node <node-name>

# Clean up node resources
ovn-kube-util cleanup-node <node-name>

# Show node gateway information
ovn-kube-util node-gateway <node-name>
```

#### Network Policy Operations
```bash
# List network policies
ovn-kube-util list-network-policies

# Show network policy details
ovn-kube-util show-network-policy <namespace>/<name>

# Validate network policy
ovn-kube-util validate-network-policy <file.yaml>
```

### Example Usage

```bash
# Check node network interfaces
ovn-kube-util nics

# Show all pod networks
ovn-kube-util pod-networks --all-namespaces

# Display service information for specific namespace
ovn-kube-util services --namespace=kube-system

# Initialize a new worker node
ovn-kube-util init-node worker-node-02

# Show detailed network policy information
ovn-kube-util show-network-policy default/deny-all
```

## ovndbchecker

Database consistency checker for OVN databases.

### Usage

```bash
ovndbchecker [flags]
```

### Common Flags

```bash
--nb-address=<URL>              # OVN Northbound database address
--sb-address=<URL>              # OVN Southbound database address
--k8s-kubeconfig=<path>         # Path to kubeconfig file
--check-interval=<duration>     # Check interval (default: 30s)
--fix-inconsistencies          # Automatically fix found issues
--dry-run                      # Show what would be fixed without making changes
```

### Example Usage

```bash
# Basic consistency check
ovndbchecker \
  --nb-address=ssl:192.168.1.10:6641 \
  --sb-address=ssl:192.168.1.10:6642 \
  --k8s-kubeconfig=/etc/kubernetes/admin.conf

# Check and fix inconsistencies
ovndbchecker \
  --nb-address=ssl:192.168.1.10:6641 \
  --sb-address=ssl:192.168.1.10:6642 \
  --k8s-kubeconfig=/etc/kubernetes/admin.conf \
  --fix-inconsistencies

# Dry run to see what would be fixed
ovndbchecker \
  --nb-address=ssl:192.168.1.10:6641 \
  --sb-address=ssl:192.168.1.10:6642 \
  --k8s-kubeconfig=/etc/kubernetes/admin.conf \
  --fix-inconsistencies \
  --dry-run
```

## ovnkube-trace

Network path tracing utility for debugging connectivity issues.

### Usage

```bash
ovnkube-trace [flags]
```

### Common Operations

```bash
# Trace pod-to-pod connectivity
ovnkube-trace \
  --src-namespace=default \
  --src-pod=pod1 \
  --dst-namespace=default \
  --dst-pod=pod2

# Trace pod-to-service connectivity
ovnkube-trace \
  --src-namespace=default \
  --src-pod=client-pod \
  --service=web-service \
  --dst-namespace=default

# Trace with specific protocol and port
ovnkube-trace \
  --src-namespace=default \
  --src-pod=client-pod \
  --dst-namespace=default \
  --dst-pod=server-pod \
  --tcp \
  --dst-port=8080

# Trace external connectivity
ovnkube-trace \
  --src-namespace=default \
  --src-pod=client-pod \
  --dst-ip=8.8.8.8 \
  --udp \
  --dst-port=53
```

For detailed ovnkube-trace usage, see the [OVNKube Trace Documentation](../troubleshooting/ovnkube-trace.md).

## CNI Plugin (ovn-k8s-cni-overlay)

The CNI plugin binary is typically invoked by the container runtime and kubelet.

### Manual Invocation

```bash
# Check CNI plugin version
ovn-k8s-cni-overlay --version

# Validate CNI configuration
cat /etc/cni/net.d/10-ovn-kubernetes.conf | ovn-k8s-cni-overlay
```

### CNI Configuration

Example CNI configuration file:

```json
{
  "cniVersion": "0.4.0",
  "name": "ovn-kubernetes",
  "type": "ovn-k8s-cni-overlay",
  "ipam": {},
  "dns": {},
  "logFile": "/var/log/ovn-kubernetes/ovn-k8s-cni-overlay.log",
  "logLevel": "5",
  "logfile-maxsize": 100,
  "logfile-maxbackups": 5,
  "logfile-maxage": 5
}
```

## Environment Variables

Common environment variables used by OVN-Kubernetes tools:

### Database Configuration
```bash
export OVN_NB_DB="ssl:192.168.1.10:6641"
export OVN_SB_DB="ssl:192.168.1.10:6642"
export OVN_NB_DB_PRIVKEY="/etc/ovn/ovn-nb-db-privkey.pem"
export OVN_NB_DB_CERT="/etc/ovn/ovn-nb-db-cert.pem"
export OVN_NB_DB_CACERT="/etc/ovn/ovn-nb-db-cacert.pem"
```

### Kubernetes Configuration
```bash
export KUBECONFIG="/etc/kubernetes/admin.conf"
export K8S_APISERVER="https://192.168.1.10:6443"
export K8S_TOKEN="<token>"
export K8S_CACERT="/etc/kubernetes/pki/ca.crt"
```

### Logging Configuration
```bash
export OVN_LOG_LEVEL="4"
export OVN_LOGFILE="/var/log/ovn-kubernetes/ovnkube.log"
export OVN_LOGFILE_MAXSIZE="100"
export OVN_LOGFILE_MAXBACKUPS="5"
```

## Configuration Files

### Main Configuration File

OVN-Kubernetes can use a configuration file instead of command-line flags:

```yaml
# /etc/ovn-kubernetes/ovnkube.conf
default:
  cluster-subnets: "10.244.0.0/16"
  service-cluster-ip-range: "10.96.0.0/12"
  
logging:
  loglevel: 4
  logfile: "/var/log/ovn-kubernetes/ovnkube.log"
  logfile-maxsize: 100
  logfile-maxbackups: 5
  logfile-maxage: 5

ovnkube-master:
  nb-address: "ssl:192.168.1.10:6641"
  sb-address: "ssl:192.168.1.10:6642"
  
ovnkube-node:
  nodeport: true
  gateway-mode: "shared"
```

Use with: `ovnkube --config-file=/etc/ovn-kubernetes/ovnkube.conf`

## Common CLI Patterns

### Health Checks

```bash
# Check if ovnkube is running
ps aux | grep ovnkube

# Check ovnkube logs
tail -f /var/log/ovn-kubernetes/ovnkube.log

# Verify database connectivity
ovn-kube-util show-db

# Check node status
ovn-kube-util nics
```

### Troubleshooting

```bash
# Debug network connectivity
ovnkube-trace --src-pod=pod1 --dst-pod=pod2

# Check for database inconsistencies
ovndbchecker --dry-run

# Verify CNI configuration
cat /etc/cni/net.d/10-ovn-kubernetes.conf

# Check pod network assignments
ovn-kube-util pod-networks --namespace=default
```

### Maintenance Operations

```bash
# Backup databases
ovn-kube-util backup-db --output-dir=/backup/$(date +%Y%m%d)

# Clean up node (before removal)
ovn-kube-util cleanup-node <node-name>

# Restart with increased logging
ovnkube --loglevel=5 --config-file=/etc/ovn-kubernetes/ovnkube.conf
```

## Best Practices

### Logging
- Use log level 4 for production (info level)
- Use log level 5 for debugging (debug level)
- Configure log rotation to prevent disk space issues
- Monitor log files for errors and warnings

### Database Operations
- Regular database backups before maintenance
- Use dry-run flag before fixing inconsistencies
- Monitor database connectivity and performance

### Monitoring
- Set up monitoring for ovnkube processes
- Monitor OVN database health
- Track network policy changes and applications

### Security
- Use SSL/TLS for database connections
- Properly configure certificates and keys
- Limit access to administrative tools
- Regularly rotate certificates

For more detailed troubleshooting information, see the [Troubleshooting Guide](../troubleshooting/debugging.md).
