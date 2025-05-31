# Building OVN-Kubernetes

This guide covers how to build OVN-Kubernetes from source code on different platforms and environments.

## Prerequisites

### System Requirements

- **Operating System**: Linux (Ubuntu 18.04+, CentOS 7+, RHEL 8+, Fedora 30+)
- **Memory**: Minimum 4GB RAM (8GB+ recommended for development)
- **Storage**: At least 10GB free disk space
- **Network**: Internet access for downloading dependencies

### Required Dependencies

#### Go Programming Language
OVN-Kubernetes requires Go 1.19 or later.

```bash
# Download and install Go
wget https://go.dev/dl/go1.21.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.21.0.linux-amd64.tar.gz

# Add to your shell profile (.bashrc, .zshrc, etc.)
export PATH=$PATH:/usr/local/go/bin
export GOPATH=$HOME/go
export PATH=$PATH:$GOPATH/bin
```

#### Git
```bash
# Ubuntu/Debian
sudo apt-get update && sudo apt-get install -y git

# CentOS/RHEL/Fedora
sudo yum install -y git
# or for newer versions
sudo dnf install -y git
```

#### Make and Build Tools
```bash
# Ubuntu/Debian
sudo apt-get install -y build-essential

# CentOS/RHEL
sudo yum groupinstall -y "Development Tools"

# Fedora
sudo dnf groupinstall -y "Development Tools"
```

#### Docker (Optional, for containerized builds)
```bash
# Install Docker following official instructions for your distribution
curl -fsSL https://get.docker.com -o get-docker.sh
sudo sh get-docker.sh
sudo usermod -aG docker $USER
```

## Getting the Source Code

### Clone the Repository

```bash
# Clone the main repository
git clone https://github.com/ovn-org/ovn-kubernetes.git
cd ovn-kubernetes

# Or clone your fork for development
git clone https://github.com/YOUR_USERNAME/ovn-kubernetes.git
cd ovn-kubernetes
```

### Understanding the Repository Structure

```
ovn-kubernetes/
├── go-controller/          # Main Go controller code
├── dist/                   # Distribution files and manifests
├── helm/                   # Helm charts
├── docs/                   # Documentation
├── contrib/                # Contributed scripts and tools
├── test/                   # Test suites
└── Makefile               # Build automation
```

## Building OVN-Kubernetes

### Standard Build

#### Build All Components

```bash
# Build all binaries
make

# Or build specific targets
make ovnkube
make ovn-k8s-cni-overlay
```

#### Build with Specific Options

```bash
# Build with race detection (for development)
make RACE=true

# Build with specific Go version
GO=/usr/local/go/bin/go make

# Build with verbose output
make V=1
```

### Cross-Platform Building

#### Building for Different Architectures

```bash
# Build for ARM64
make GOARCH=arm64

# Build for different OS/Architecture combinations
make GOOS=linux GOARCH=amd64
make GOOS=linux GOARCH=arm64
make GOOS=windows GOARCH=amd64
```

### Containerized Builds

#### Using Docker

```bash
# Build container image
make docker-build

# Build with specific tag
make docker-build IMAGE_TAG=my-custom-tag

# Build for multiple architectures
make docker-build-multi-arch
```

#### Using Podman

```bash
# If you prefer Podman over Docker
make docker-build CONTAINER_RUNTIME=podman
```

## Build Targets

### Available Make Targets

| Target | Description |
|--------|-------------|
| `make` | Build all binaries |
| `make ovnkube` | Build main ovnkube binary |
| `make ovn-k8s-cni-overlay` | Build CNI plugin |
| `make windows` | Build Windows binaries |
| `make check` | Run static checks and linting |
| `make test` | Run unit tests |
| `make docker-build` | Build container image |
| `make clean` | Clean build artifacts |

### Binary Outputs

After a successful build, binaries are located in:

```bash
go-controller/_output/go/bin/
├── ovnkube                    # Main controller
├── ovn-k8s-cni-overlay       # CNI plugin
├── ovn-kube-util             # Utility commands
└── ovndbchecker              # Database checker
```

## Development Builds

### Fast Development Builds

```bash
# Skip some checks for faster iteration
make fast-build

# Build only changed components
make incremental
```

### Debug Builds

```bash
# Build with debug symbols
make DEBUG=true

# Build with additional debugging information
make GCFLAGS="-N -l"
```

## Testing Your Build

### Unit Tests

```bash
# Run all unit tests
make test

# Run tests with coverage
make test-coverage

# Run specific test packages
go test ./go-controller/pkg/ovn/...
```

### Integration Tests

```bash
# Run integration tests (requires kind)
make test-integration

# Run end-to-end tests
make test-e2e
```

### Smoke Test

```bash
# Quick verification that binaries work
./_output/go/bin/ovnkube --help
./_output/go/bin/ovn-k8s-cni-overlay --help
```

## Platform-Specific Notes

### Ubuntu/Debian

```bash
# Install additional dependencies
sudo apt-get install -y libnl-3-dev libnl-genl-3-dev

# If building OVS from source
sudo apt-get install -y autotools-dev dh-autoreconf libssl-dev
```

### CentOS/RHEL

```bash
# Enable EPEL repository
sudo yum install -y epel-release

# Install development dependencies
sudo yum install -y libnl3-devel openssl-devel
```

### Fedora

```bash
# Install dependencies
sudo dnf install -y libnl3-devel openssl-devel
```

## Troubleshooting Build Issues

### Common Issues

#### Go Version Issues
```bash
# Check Go version
go version

# If version is too old, update Go installation
```

#### Permission Issues
```bash
# Ensure proper ownership of GOPATH
sudo chown -R $USER:$USER $GOPATH

# Fix Docker permissions
sudo usermod -aG docker $USER
newgrp docker
```

#### Memory Issues
```bash
# Reduce parallel compilation
make -j2

# Or build without parallelization
make -j1
```

#### Dependency Issues
```bash
# Clean and rebuild dependencies
go clean -modcache
go mod download
make clean && make
```

### Getting Help

If you encounter build issues:

1. Check the [GitHub Issues](https://github.com/ovn-org/ovn-kubernetes/issues)
2. Join our [Slack channel](https://cloud-native.slack.com/archives/C08452HR8V6)
3. Post on the [mailing list](https://groups.google.com/g/ovn-kubernetes)

## Advanced Build Options

### Custom Build Flags

```bash
# Build with custom ldflags
make LDFLAGS="-X main.version=custom"

# Build with CGO disabled
make CGO_ENABLED=0

# Build with specific build tags
make BUILD_TAGS="debug,profiling"
```

### Building Documentation

```bash
# Build documentation locally
make docs

# Serve documentation locally
make serve-docs
```

## Contributing Your Changes

After building and testing your changes:

1. Run the full test suite: `make check test`
2. Ensure code formatting: `make fmt`
3. Update documentation if needed
4. Submit a pull request

For more details, see our [Contributing Guide](../governance/CONTRIBUTING.md).
