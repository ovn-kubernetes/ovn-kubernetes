# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

OVN-Kubernetes is a Kubernetes networking platform that provides CNI (Container Network Interface) functionality using OVN (Open Virtual Networking) and Open vSwitch. The main implementation is in Go and consists of multiple executables that work together to provide pod networking, service load balancing, and network policy enforcement.

## Architecture

### Core Components

**Main Executables** (located in `go-controller/_output/go/bin/` after build):
- `ovnkube`: All-in-one controller for master/node operations with multiple modes:
  - `--init-master`: Cluster manager + OVN controller (watches pods, services, policies)
  - `--init-cluster-manager`: Allocates node subnets from cluster CIDR
  - `--init-ovnkube-controller`: Manages OVN logical resources
  - `--init-node`: Initializes node networking via OVS
- `ovn-k8s-cni-overlay`: CNI plugin binary for pod network interface management
- `ovnkube-trace`: Network debugging and tracing tool
- `ovnkube-identity`: Identity management component
- `ovndbchecker`: OVN database health checker
- `ovn-kube-util`: Utility commands

**Architecture Pattern**:
- Uses Kubernetes controller pattern with watchers for nodes, pods, services, and network policies
- Master operations create logical routers, switches, and load balancers in OVN northbound database
- Node operations manage OVS resources and pod network interfaces
- CNI integration for pod lifecycle events

### Key Directories

- `go-controller/`: Main Go implementation
  - `cmd/`: Executable entry points
  - `pkg/`: Core business logic packages
  - `pkg/clustermanager/`: Node subnet allocation and cluster-wide management
  - `pkg/controller/`: OVN resource controllers for pods, services, policies
  - `pkg/cni/`: CNI plugin implementation
  - `pkg/crd/`: Custom Resource Definitions (EgressIP, EgressFirewall, etc.)
- `dist/images/`: Docker image build configurations
- `contrib/`: Helper scripts and tools

## Development Commands

### Building

**Primary build** (from `go-controller/`):
```bash
make clean
make
```
Outputs to: `go-controller/_output/go/bin/`

**Windows build**:
```bash
make clean
make windows
```
Outputs to: `go-controller/_output/go/windows/`

**Install CNI plugin**:
```bash
make install
```
Copies `ovn-k8s-cni-overlay` to `/opt/cni/bin/`

### Testing

**Unit tests**:
```bash
make check
# or with coverage:
COVERALLS=1 make check
```

**Run tests in container**:
```bash
make test
```

**Specific test focus**:
```bash
GINKGO_FOCUS="your test pattern" make check
```

### Code Quality

**Linting**:
```bash
make lint
```

**Format checking**:
```bash
make gofmt
```

**Dependency verification**:
```bash
make verify-go-mod-vendor
```

**Code generation** (after modifying CRDs or OVN schemas):
```bash
make codegen
make modelgen
```

### CI/CD Integration

The project uses GitHub Actions with comprehensive testing matrices. Key workflow files:
- `.github/workflows/test.yml`: Main CI pipeline with e2e tests across multiple configurations
- Tests run in KIND clusters with various gateway modes, IP families, and feature combinations
- Builds both master and PR images for upgrade testing

## Configuration

**Default config file**: `/etc/openvswitch/ovn_k8s.conf`

**Environment variables** (override config file):
- `KUBECONFIG`: Path to kubeconfig file
- `K8S_APISERVER`: Kubernetes API server URL
- `K8S_CACERT`: CA certificate path
- `K8S_TOKEN`: Authentication token

**Command-line options** override both config file and environment variables.

## Go Module Structure

- Go version requirement: 1.24+
- Single main module at `go-controller/go.mod`
- Uses vendoring for dependency management
- Key dependencies: Kubernetes client libraries, OVN bindings, CNI libraries

## Development Notes

- Tests use Ginkgo/Gomega framework
- Extensive use of Kubernetes controller-runtime for watchers and reconciliation
- OVN database interactions through libovsdb
- Support for multiple IP families (IPv4, IPv6, dual-stack)
- Network policy enforcement through OVN ACLs
- Service load balancing via OVN load balancers

## Testing Strategy

E2E tests cover multiple scenarios including:
- Gateway modes (local/shared)
- IP families (IPv4/IPv6/dual-stack)
- High availability configurations
- Network segmentation and multi-homing
- Upgrade scenarios from master to PR branches
- Traffic flow testing and network policy validation