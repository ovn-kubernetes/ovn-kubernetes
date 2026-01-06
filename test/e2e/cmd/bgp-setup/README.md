# BGP Setup Tool

A standalone Go program that sets up BGP infrastructure for route advertisement testing in OVN-Kubernetes KinD clusters.

## Overview

This tool implements the functionality of the following shell functions from `contrib/kind-common`:
- `deploy_frr_external_container()` - Deploys an external FRR container for BGP peering
- `deploy_bgp_external_server()` - Creates a BGP server container for testing
- `install_frr_k8s()` - Installs frr-k8s operator and configurations
- Creates FRRConfiguration for the default network

This tool reuses packages from the e2e test framework:
- `containerengine` - for container runtime abstraction
- `images` - for container images

## Network Topology

```
-----------------               ------------------                         ---------------------
|               | bgpnet        |                |       kind network      | ovn-control-plane |
|   bgpserver   |<------------- |   FRR router   |<------ KIND cluster --  ---------------------
|   (agnhost)   |               |  (frr:10.4.1)  |                         |    ovn-worker     |
-----------------               ------------------                         ---------------------
                                                                           |    ovn-worker2    |
                                                                           ---------------------
```

## Building

From the `test/e2e` directory:

```bash
go build -o bgp-setup ./cmd/bgp-setup
```

Or from the repository root:

```bash
cd test/e2e && go build -o bgp-setup ./cmd/bgp-setup
```

## Usage

### Basic Usage

Run with default settings (IPv4 only, strict isolation):
```bash
./bgp-setup
```

### With IPv6 Support

```bash
./bgp-setup --ipv4 --ipv6
```

### Custom Network Configuration

```bash
./bgp-setup \
  --bgp-peer-subnet-ipv4 172.36.0.0/16 \
  --bgp-peer-subnet-ipv6 fc00:f853:ccd:36::/64 \
  --bgp-server-subnet-ipv4 172.26.0.0/16 \
  --bgp-server-subnet-ipv6 fc00:f853:ccd:e796::/64
```

### Loose Isolation Mode

For UDN loose isolation mode (sets nodes' default gateway to FRR router):
```bash
./bgp-setup --isolation-mode loose
```

### Cleanup

To remove all BGP infrastructure:
```bash
./bgp-setup --cleanup
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--phase` | `all` | Phase to run: `all`, `deploy-frr`, `deploy-bgp-server`, `deploy-containers`, or `install-frr-k8s` |
| `--container-runtime` | `docker` | Container runtime to use (docker/podman) |
| `--bgp-peer-subnet-ipv4` | `172.36.0.0/16` | IPv4 CIDR for BGP peer network |
| `--bgp-peer-subnet-ipv6` | `fc00:f853:ccd:36::/64` | IPv6 CIDR for BGP peer network |
| `--bgp-server-subnet-ipv4` | `172.26.0.0/16` | IPv4 CIDR for BGP server network |
| `--bgp-server-subnet-ipv6` | `fc00:f853:ccd:e796::/64` | IPv6 CIDR for BGP server network |
| `--ipv4` | `true` | Enable IPv4 support |
| `--ipv6` | `false` | Enable IPv6 support |
| `--frr-k8s-version` | `v0.0.21` | Version of frr-k8s to use |
| `--network-name` | `default` | Name for the BGP network |
| `--kubeconfig` | `~/.kube/config` | Path to kubeconfig file |
| `--advertise-default-network` | `true` | Advertise the default network |
| `--isolation-mode` | `strict` | UDN isolation mode: strict or loose |
| `--cleanup` | `false` | Only cleanup existing BGP infrastructure |

### Phase Options

The `--phase` flag allows running specific parts of the setup:

- **`all`** (default): Run all phases in sequence - useful for standalone testing
- **`deploy-frr`**: Deploy FRR external container only
- **`deploy-bgp-server`**: Deploy BGP server container only
- **`deploy-containers`**: Deploy both FRR container and BGP server (combines deploy-frr + deploy-bgp-server)
- **`install-frr-k8s`**: Install frr-k8s operator and create FRRConfiguration

#### Running All Phases (Standalone Usage)

When running `bgp-setup` outside of `kind.sh`, use phase `all` to set up everything at once:

```bash
# Run all phases - deploys FRR, BGP server, and installs frr-k8s
./bgp-setup --phase all

# Or simply (all is the default):
./bgp-setup
```

#### Running Individual Phases (Integration with kind.sh)

When integrated with `kind.sh`, phases are run separately because some must execute before OVN installation and others after:

```bash
# Phase 1a: Deploy FRR container (before OVN installation)
./bgp-setup --phase deploy-frr

# Phase 1b: Deploy BGP server (before OVN installation)  
./bgp-setup --phase deploy-bgp-server

# Phase 2: Install frr-k8s (after OVN is installed and cluster is ready)
./bgp-setup --phase install-frr-k8s
```

## Environment Variables

All flags can also be set via environment variables:

| Environment Variable | Flag |
|---------------------|------|
| `CONTAINER_RUNTIME` | `--container-runtime` |
| `BGP_PEER_SUBNET_IPV4` | `--bgp-peer-subnet-ipv4` |
| `BGP_PEER_SUBNET_IPV6` | `--bgp-peer-subnet-ipv6` |
| `BGP_SERVER_NET_SUBNET_IPV4` | `--bgp-server-subnet-ipv4` |
| `BGP_SERVER_NET_SUBNET_IPV6` | `--bgp-server-subnet-ipv6` |
| `PLATFORM_IPV4_SUPPORT` | `--ipv4` |
| `PLATFORM_IPV6_SUPPORT` | `--ipv6` |
| `FRR_K8S_VERSION` | `--frr-k8s-version` |
| `NETWORK_NAME` | `--network-name` |
| `KUBECONFIG` | `--kubeconfig` |
| `ADVERTISE_DEFAULT_NETWORK` | `--advertise-default-network` |
| `ADVERTISED_UDN_ISOLATION_MODE` | `--isolation-mode` |

## Integration with kind.sh

This tool is integrated into `contrib/kind.sh` via the `run_bgp_setup` function.
The shell script calls the tool in separate phases:

```bash
# Phase 1a: Before OVN installation - deploy FRR container
if [ "$ENABLE_ROUTE_ADVERTISEMENTS" == true ]; then
  run_bgp_setup deploy-frr
fi

# Phase 1b: Before OVN installation - deploy BGP server
if [ "$ENABLE_ROUTE_ADVERTISEMENTS" == true ]; then
  run_bgp_setup deploy-bgp-server
fi

# ... OVN installation happens here ...

# Phase 2: After OVN installation - install frr-k8s and create FRRConfiguration  
if [ "$ENABLE_ROUTE_ADVERTISEMENTS" == true ]; then
  run_bgp_setup install-frr-k8s
fi
```

The `run_bgp_setup` function in kind.sh automatically builds the tool if needed
and passes the appropriate configuration flags.

## What It Does

1. **Apply FRR-K8s CRDs** - Installs the FRRConfiguration CRD directly from GitHub (no git clone required)
2. **Deploy FRR container** - Creates an FRR router container attached to the kind network
3. **Create bgpnet network** - Creates a separate network for the BGP server
4. **Deploy bgpserver** - Creates an agnhost container acting as an external server
5. **Configure routing** - Sets up routing between containers
6. **Install frr-k8s** - Deploys the frr-k8s operator directly from GitHub
7. **Apply FRRConfiguration** - Creates BGP peering configuration for the cluster
8. **Add pod network routes** - Optionally adds routes for pod networks (requires sudo)

## Dependencies

- Go 1.24+
- kubectl configured with cluster access
- Docker or Podman
- A running KinD cluster with OVN-Kubernetes

## Template Files

This tool uses the shared templates from `test/e2e/testdata/routeadvertisements/`.
The templates are loaded from the filesystem at runtime using `runtime.Caller()` to
determine the source file location and find the templates relative to it.

The same templates are also used by the route advertisement e2e tests in
`test/e2e/route_advertisements.go`.
