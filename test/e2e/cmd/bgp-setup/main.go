// bgp-setup is a standalone program that sets up BGP infrastructure for route advertisement testing.
// It
//   - deploys an external FRR container for BGP peering
//   - deploy bgp server container for testing
//   - install frr-k8s and create FRRConfiguration for default network
//
// Usage:
//
//	bgp-setup [flags]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/containerengine"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/frr"
)

// getTestdataPath returns the path to the shared testdata templates.
// These templates are used by both the route_advertisements tests and this setup tool.
// The path is determined relative to this source file's location.
func getTestdataPath() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("failed to get current file path")
	}
	// thisFile is .../test/e2e/cmd/bgp-setup/main.go
	// We need .../test/e2e/testdata/routeadvertisements
	// Go up 3 levels: bgp-setup -> cmd -> e2e
	cmdDir := filepath.Dir(thisFile)
	e2eCmdDir := filepath.Dir(cmdDir)
	e2eDir := filepath.Dir(e2eCmdDir)
	return filepath.Join(e2eDir, "testdata", "routeadvertisements")
}

const (
	// Container and network names
	kindNetwork              = "kind"
	frrK8sNS                 = "frr-k8s-system"
	frrK8sDeploymentName     = "frr-k8s-statuscleaner"
	frrK8sDaemonsetName      = "frr-k8s-daemon"
	frrK8sWebhookServiceName = "frr-k8s-webhook-service"
	frrContainerName         = "frr"
	controlPlaneNodeName     = "ovn-control-plane"

	// BGP server configuration
	bgpServerContainerName = "bgpserver"
	bgpNetworkName         = "bgpnet"
	bgpServerPortMapping   = "8080:8080"

	// Persistent directory for FRR configuration files
	// This directory persists for the cluster lifetime and is cleaned up with the cluster
	frrConfigDir = "/tmp/bgp-setup/frr-config"

	// Environment variable names
	envContainerRuntime        = "CONTAINER_RUNTIME"
	envBGPPeerSubnetIPv4       = "BGP_PEER_SUBNET_IPV4"
	envBGPPeerSubnetIPv6       = "BGP_PEER_SUBNET_IPV6"
	envBGPServerSubnetIPv4     = "BGP_SERVER_NET_SUBNET_IPV4"
	envBGPServerSubnetIPv6     = "BGP_SERVER_NET_SUBNET_IPV6"
	envIPv4Support             = "PLATFORM_IPV4_SUPPORT"
	envIPv6Support             = "PLATFORM_IPV6_SUPPORT"
	envFRRK8sVersion           = "FRR_K8S_VERSION"
	envNetworkName             = "NETWORK_NAME"
	envKubeconfig              = "KUBECONFIG"
	envAdvertiseDefaultNetwork = "ADVERTISE_DEFAULT_NETWORK"
	envIsolationMode           = "ADVERTISED_UDN_ISOLATION_MODE"

	// Default configuration values
	defaultContainerRuntime    = "docker"
	defaultBGPPeerSubnetIPv4   = "172.36.0.0/16"
	defaultBGPPeerSubnetIPv6   = "fc00:f853:ccd:36::/64"
	defaultBGPServerSubnetIPv4 = "172.26.0.0/16"
	defaultBGPServerSubnetIPv6 = "fc00:f853:ccd:e796::/64"
	defaultFRRK8sVersion       = "v0.0.21"
	defaultNetworkName         = "default"
	defaultIsolationMode       = "strict"

	// Timeouts and intervals
	pollInterval     = time.Second
	containerTimeout = 60 * time.Second
	deployTimeout    = "2m"

	// Phase constants for running specific parts of the setup
	PhaseAll              = "all"
	PhaseDeployContainers = "deploy-containers" // Phase 1: deploy FRR container and BGP server (combined)
	PhaseDeployFRR        = "deploy-frr"        // Phase 1a: deploy FRR external container only
	PhaseDeployBGPServer  = "deploy-bgp-server" // Phase 1b: deploy BGP server container only
	PhaseInstallFRRK8s    = "install-frr-k8s"   // Phase 2: install frr-k8s and create FRRConfiguration
)

// frrK8sRemoteURL returns the raw GitHub URL for a file in the frr-k8s repo at a specific version
func frrK8sRemoteURL(version, path string) string {
	return fmt.Sprintf("https://raw.githubusercontent.com/metallb/frr-k8s/%s/%s", version, path)
}

// Config holds the configuration for BGP setup
type Config struct {
	ContainerRuntime        string
	BGPPeerSubnetIPv4       string
	BGPPeerSubnetIPv6       string
	BGPServerSubnetIPv4     string
	BGPServerSubnetIPv6     string
	IPv4Enabled             bool
	IPv6Enabled             bool
	FRRK8sVersion           string
	NetworkName             string
	Kubeconfig              string
	AdvertiseDefaultNetwork bool
	IsolationMode           string
	CleanupOnly             bool
	Phase                   string
	UseDirectAPI            bool
}

// frrTemplateSource provides access to FRR templates from the filesystem
var frrTemplateSource = frr.NewFilesystemSource(getTestdataPath())

func main() {
	cfg := parseFlags()

	// Set kubeconfig for kubectl commands
	kubeconfig = cfg.Kubeconfig

	// Get control plane IP for direct API server access
	// Only enabled when --use-direct-api=true, as it requires Docker bridge network to be routable
	if cfg.UseDirectAPI {
		if err := setupKubectlServer(); err != nil {
			fmt.Printf("Warning: could not get control plane IP, kubectl may fail: %v\n", err)
		}
	}

	// Set container engine based on config
	if cfg.ContainerRuntime != "" {
		os.Setenv("CONTAINER_RUNTIME", cfg.ContainerRuntime)
	}

	if cfg.CleanupOnly {
		fmt.Println("Cleaning up BGP infrastructure...")
		if err := cleanup(); err != nil {
			fmt.Fprintf(os.Stderr, "Cleanup failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Cleanup completed successfully")
		return
	}

	fmt.Println("Setting up BGP infrastructure for route advertisement testing...")

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("BGP setup completed successfully!")
}

func parseFlags() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.ContainerRuntime, "container-runtime", getEnvOrDefault(envContainerRuntime, defaultContainerRuntime), "Container runtime to use (docker/podman)")
	flag.StringVar(&cfg.BGPPeerSubnetIPv4, "bgp-peer-subnet-ipv4", getEnvOrDefault(envBGPPeerSubnetIPv4, defaultBGPPeerSubnetIPv4), "IPv4 CIDR for BGP peer network")
	flag.StringVar(&cfg.BGPPeerSubnetIPv6, "bgp-peer-subnet-ipv6", getEnvOrDefault(envBGPPeerSubnetIPv6, defaultBGPPeerSubnetIPv6), "IPv6 CIDR for BGP peer network")
	flag.StringVar(&cfg.BGPServerSubnetIPv4, "bgp-server-subnet-ipv4", getEnvOrDefault(envBGPServerSubnetIPv4, defaultBGPServerSubnetIPv4), "IPv4 CIDR for BGP server network")
	flag.StringVar(&cfg.BGPServerSubnetIPv6, "bgp-server-subnet-ipv6", getEnvOrDefault(envBGPServerSubnetIPv6, defaultBGPServerSubnetIPv6), "IPv6 CIDR for BGP server network")
	flag.BoolVar(&cfg.IPv4Enabled, "ipv4", getBoolEnvOrDefault(envIPv4Support, true), "Enable IPv4 support")
	flag.BoolVar(&cfg.IPv6Enabled, "ipv6", getBoolEnvOrDefault(envIPv6Support, false), "Enable IPv6 support")
	flag.StringVar(&cfg.FRRK8sVersion, "frr-k8s-version", getEnvOrDefault(envFRRK8sVersion, defaultFRRK8sVersion), "Version of frr-k8s to use")
	flag.StringVar(&cfg.NetworkName, "network-name", getEnvOrDefault(envNetworkName, defaultNetworkName), "Name for the BGP network")
	flag.StringVar(&cfg.Kubeconfig, "kubeconfig", getEnvOrDefault(envKubeconfig, filepath.Join(os.Getenv("HOME"), ".kube", "config")), "Path to kubeconfig file")
	flag.BoolVar(&cfg.AdvertiseDefaultNetwork, "advertise-default-network", getBoolEnvOrDefault(envAdvertiseDefaultNetwork, true), "Advertise the default network")
	flag.StringVar(&cfg.IsolationMode, "isolation-mode", getEnvOrDefault(envIsolationMode, defaultIsolationMode), "UDN isolation mode: strict or loose")
	flag.BoolVar(&cfg.CleanupOnly, "cleanup", false, "Only cleanup existing BGP infrastructure")
	flag.StringVar(&cfg.Phase, "phase", PhaseAll, "Phase to run: 'all', 'deploy-containers' (FRR + BGP server), 'deploy-frr' (FRR only), 'deploy-bgp-server' (BGP server only), or 'install-frr-k8s' (frr-k8s + FRRConfiguration)")
	flag.BoolVar(&cfg.UseDirectAPI, "use-direct-api", false, "Use direct API server address (control plane container IP) instead of kubeconfig server. Only works when Docker bridge network is routable from host.")

	flag.Parse()
	return cfg
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getBoolEnvOrDefault(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		return strings.ToLower(value) == "true"
	}
	return defaultValue
}

func run(cfg *Config) error {
	// Create Kubernetes client
	clientset, err := createK8sClient(cfg.Kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	// Get supported IP families based on cluster configuration
	ipv4Supported, ipv6Supported := detectClusterIPFamilies(clientset)
	if cfg.IPv4Enabled {
		cfg.IPv4Enabled = ipv4Supported
	}
	if cfg.IPv6Enabled {
		cfg.IPv6Enabled = ipv6Supported
	}
	fmt.Printf("IP family support - IPv4: %v, IPv6: %v\n", cfg.IPv4Enabled, cfg.IPv6Enabled)

	// Get all cluster node names
	nodes, err := getClusterNodes(clientset)
	if err != nil {
		return fmt.Errorf("failed to get cluster nodes: %w", err)
	}
	fmt.Printf("Found %d cluster nodes: %v\n", len(nodes), nodeNames(nodes))

	// Determine which phases to run based on --phase flag
	runDeployFRR := cfg.Phase == PhaseAll || cfg.Phase == PhaseDeployContainers || cfg.Phase == PhaseDeployFRR
	runDeployBGPServer := cfg.Phase == PhaseAll || cfg.Phase == PhaseDeployContainers || cfg.Phase == PhaseDeployBGPServer
	runInstallFRRK8s := cfg.Phase == PhaseAll || cfg.Phase == PhaseInstallFRRK8s

	// Phase 1a: Deploy FRR external container
	if runDeployFRR {
		fmt.Println("\n====================== Deploying FRR external container ======================")
		if err := deployFRRExternalContainer(cfg, nodes); err != nil {
			return fmt.Errorf("failed to deploy FRR external container: %w", err)
		}
	}

	// Phase 1b: Deploy BGP external server
	if runDeployBGPServer {
		fmt.Println("\n====================== Deploying BGP external server ======================")
		if err := deployBGPExternalServer(cfg, nodes); err != nil {
			return fmt.Errorf("failed to deploy BGP external server: %w", err)
		}
	}

	// Phase 2: Install frr-k8s (install_frr_k8s + create FRRConfiguration)
	if runInstallFRRK8s {
		fmt.Println("\n====================== Installing frr-k8s ======================")
		if err := installFRRK8s(cfg); err != nil {
			return fmt.Errorf("failed to install frr-k8s: %w", err)
		}

		// Create FRRConfiguration for default network
		if cfg.AdvertiseDefaultNetwork {
			fmt.Println("\n====================== Creating FRRConfiguration for default network ======================")
			if err := createDefaultNetworkFRRConfiguration(cfg); err != nil {
				return fmt.Errorf("failed to create FRRConfiguration: %w", err)
			}
		}

		// Add routes for pod networks if advertising default network
		if cfg.AdvertiseDefaultNetwork {
			fmt.Println("\n====================== Adding routes for pod networks ======================")
			if err := addPodNetworkRoutes(cfg, clientset); err != nil {
				fmt.Printf("Warning: failed to add pod network routes: %v\n", err)
			}
		}
	}

	return nil
}

func createK8sClient(kubeconfig string) (*kubernetes.Clientset, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build config from kubeconfig %q: %w", kubeconfig, err)
	}
	return kubernetes.NewForConfig(config)
}

func detectClusterIPFamilies(clientset *kubernetes.Clientset) (ipv4, ipv6 bool) {
	nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		fmt.Printf("Warning: failed to list nodes for IP family detection: %v\n", err)
		return true, false // Default to IPv4 only
	}

	for _, node := range nodes.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				ip := net.ParseIP(addr.Address)
				if ip == nil {
					continue
				}
				if ip.To4() != nil {
					ipv4 = true
				} else {
					ipv6 = true
				}
			}
		}
	}

	if !ipv4 && !ipv6 {
		ipv4 = true // Default to IPv4 if nothing detected
	}
	return
}

func getClusterNodes(clientset *kubernetes.Clientset) ([]corev1.Node, error) {
	nodeList, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return nodeList.Items, nil
}

func nodeNames(nodes []corev1.Node) []string {
	names := make([]string, len(nodes))
	for i, node := range nodes {
		names[i] = node.Name
	}
	return names
}

// containerRuntime returns the container runtime command
func containerRuntime() string {
	return containerengine.Get().String()
}

func runCmd(name string, args ...string) error {
	fmt.Printf("Running: %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runCmdOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// kubeconfig stores the path to kubeconfig for kubectl commands
var kubeconfig string

// kubectlServer stores the direct API server address (control plane IP)
var kubectlServer string

// setupKubectlServer gets the control plane container's IP for direct API server access
func setupKubectlServer() error {
	runtime := containerRuntime()
	// Get control plane container IP on the kind network specifically
	output, err := runCmdOutput(runtime, "inspect", "-f", "{{.NetworkSettings.Networks.kind.IPAddress}}", "ovn-control-plane")
	if err != nil {
		return fmt.Errorf("failed to get control plane IP: %w", err)
	}
	ip := strings.TrimSpace(output)
	if ip == "" {
		return fmt.Errorf("control plane IP is empty")
	}
	kubectlServer = fmt.Sprintf("https://%s:6443", ip)
	fmt.Printf("Using direct API server address: %s\n", kubectlServer)
	return nil
}

// runKubectl runs kubectl with the configured kubeconfig and server
func runKubectl(args ...string) error {
	kubectlArgs := []string{"--kubeconfig", kubeconfig}
	if kubectlServer != "" {
		kubectlArgs = append(kubectlArgs, "--server", kubectlServer)
	}
	kubectlArgs = append(kubectlArgs, args...)
	fmt.Printf("Running: kubectl %s\n", strings.Join(kubectlArgs, " "))
	cmd := exec.Command("kubectl", kubectlArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	return cmd.Run()
}

// runKubectlOutput runs kubectl with --kubeconfig and optional --server flags and returns the output
func runKubectlOutput(args ...string) (string, error) {
	kubectlArgs := []string{"--kubeconfig", kubeconfig}
	if kubectlServer != "" {
		kubectlArgs = append(kubectlArgs, "--server", kubectlServer)
	}
	kubectlArgs = append(kubectlArgs, args...)
	cmd := exec.Command("kubectl", kubectlArgs...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// runKubectlWithRetry runs kubectl with retries for transient failures
func runKubectlWithRetry(maxRetries int, args ...string) error {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			fmt.Printf("Retrying kubectl command (attempt %d/%d)...\n", i+1, maxRetries)
			time.Sleep(5 * time.Second)
		}
		err := runKubectl(args...)
		if err == nil {
			return nil
		}
		lastErr = err
		// Always retry - exec.ExitError doesn't contain the actual error message
		// The kubectl error output (with "connection refused" etc.) goes to stderr
	}
	return lastErr
}

// waitForAPIServer waits for the Kubernetes API server to be ready
func waitForAPIServer(timeout time.Duration) error {
	fmt.Println("Waiting for Kubernetes API server to be ready...")
	fmt.Printf("Using kubeconfig: %s\n", kubeconfig)
	start := time.Now()
	var lastErr string
	for {
		if time.Since(start) > timeout {
			return fmt.Errorf("timeout waiting for API server to be ready (last error: %s)", lastErr)
		}
		// Try a simple kubectl command to check API server connectivity
		kubectlArgs := []string{"--kubeconfig", kubeconfig}
		if kubectlServer != "" {
			kubectlArgs = append(kubectlArgs, "--server", kubectlServer)
		}
		kubectlArgs = append(kubectlArgs, "get", "nodes", "--no-headers")
		cmd := exec.Command("kubectl", kubectlArgs...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		output, err := cmd.CombinedOutput()
		if err == nil {
			fmt.Println("API server is ready")
			return nil
		}
		lastErr = strings.TrimSpace(string(output))
		if lastErr == "" {
			lastErr = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
}

func containerExists(name string) bool {
	runtime := containerRuntime()
	out, _ := runCmdOutput(runtime, "ps", "-a", "-f", fmt.Sprintf("name=^%s$", name), "--format", "{{.Names}}")
	return strings.Contains(out, name)
}

func networkExists(name string) bool {
	runtime := containerRuntime()
	out, _ := runCmdOutput(runtime, "network", "ls", "--format", "{{.Name}}")
	for _, n := range strings.Split(out, "\n") {
		if strings.TrimSpace(n) == name {
			return true
		}
	}
	return false
}

// cleanupStaleResources removes any leftover BGP containers and networks from previous runs.
// This ensures a clean state even if the previous cluster was deleted with "kind delete cluster"
// instead of using the proper cleanup procedure.
func cleanupStaleResources() {
	runtime := containerRuntime()

	// Stop and remove bgpserver container if it exists
	if containerExists(bgpServerContainerName) {
		fmt.Println("Cleaning up stale bgpserver container...")
		runCmd(runtime, "stop", bgpServerContainerName)
		runCmd(runtime, "rm", "-f", bgpServerContainerName)
	}

	// Stop and remove FRR container if it exists
	// First disconnect from kind network if connected, then stop and remove
	if containerExists(frrContainerName) {
		fmt.Println("Cleaning up stale FRR container...")
		if networkExists(kindNetwork) {
			runCmdOutput(runtime, "network", "disconnect", "-f", kindNetwork, frrContainerName)
		}
		runCmd(runtime, "stop", frrContainerName)
		runCmd(runtime, "rm", "-f", frrContainerName)
	}

	// Remove bgpnet network if it exists
	if networkExists(bgpNetworkName) {
		fmt.Println("Cleaning up stale bgpnet network...")
		runCmd(runtime, "network", "rm", bgpNetworkName)
	}

	// Clean up persistent FRR config directory
	if _, err := os.Stat(frrConfigDir); err == nil {
		fmt.Println("Cleaning up stale FRR config directory...")
		os.RemoveAll(frrConfigDir)
	}
}

func deployFRRExternalContainer(cfg *Config, nodes []corev1.Node) error {
	runtime := containerRuntime()

	// Clean up any stale resources from previous runs
	cleanupStaleResources()

	// Wait for the API server to be ready before running kubectl commands
	// This handles cases where the cluster was just created and API server may not be fully ready
	if err := waitForAPIServer(90 * time.Second); err != nil {
		return fmt.Errorf("API server not ready: %w", err)
	}

	// Apply FRR-K8s CRDs from remote URL (no need to clone the repo)
	crdURL := frrK8sRemoteURL(cfg.FRRK8sVersion, "charts/frr-k8s/charts/crds/templates/frrk8s.metallb.io_frrconfigurations.yaml")
	fmt.Printf("Applying FRR-K8s CRDs from %s...\n", crdURL)
	if err := runKubectlWithRetry(3, "apply", "--validate=false", "-f", crdURL); err != nil {
		return fmt.Errorf("failed to apply FRR-K8s CRDs: %w", err)
	}

	// Get node IPs on the kind network for FRR neighbor configuration
	var nodeIPsV4, nodeIPsV6 []string
	for _, node := range nodes {
		ipv4, ipv6, err := getContainerNetworkIPs(node.Name, kindNetwork)
		if err != nil {
			fmt.Printf("Warning: failed to get IPs for node %s: %v\n", node.Name, err)
			continue
		}
		if cfg.IPv4Enabled && ipv4 != "" {
			nodeIPsV4 = append(nodeIPsV4, ipv4)
		}
		if cfg.IPv6Enabled && ipv6 != "" {
			nodeIPsV6 = append(nodeIPsV6, ipv6)
		}
	}

	fmt.Printf("Node IPs for BGP peering - IPv4: %v, IPv6: %v\n", nodeIPsV4, nodeIPsV6)

	// Create persistent directory for FRR configuration files
	if err := os.MkdirAll(frrConfigDir, 0755); err != nil {
		return fmt.Errorf("failed to create FRR config directory: %w", err)
	}

	// Generate config files
	if err := generateFRRConfigFiles(cfg, nodeIPsV4, nodeIPsV6, frrConfigDir); err != nil {
		return fmt.Errorf("failed to generate FRR configuration: %w", err)
	}

	// Remove existing FRR container if present
	if containerExists(frrContainerName) {
		fmt.Println("Removing existing FRR container...")
		runCmd(runtime, "rm", "-f", frrContainerName)
	}

	// Create and run FRR container with config files mounted
	fmt.Println("Creating FRR container with configuration...")
	args := []string{
		"run", "-d", "--privileged",
		"--name", frrContainerName,
		"--network", kindNetwork,
		"--hostname", frrContainerName,
		"-v", fmt.Sprintf("%s/frr.conf:/etc/frr/frr.conf:ro", frrConfigDir),
		"-v", fmt.Sprintf("%s/daemons:/etc/frr/daemons:ro", frrConfigDir),
	}
	// Enable IPv6 forwarding at container start if needed
	if cfg.IPv6Enabled {
		args = append(args, "--sysctl", "net.ipv6.conf.all.forwarding=1")
	}
	args = append(args, frr.Image)
	if err := runCmd(runtime, args...); err != nil {
		return fmt.Errorf("failed to create FRR container: %w", err)
	}

	// Wait for container to be running and FRR daemons to be ready
	fmt.Println("Waiting for FRR container and daemons to be ready...")
	if err := waitForContainer(frrContainerName); err != nil {
		return fmt.Errorf("FRR container failed to start: %w", err)
	}

	if err := waitForFRRDaemons(frrContainerName); err != nil {
		return fmt.Errorf("FRR daemons failed to become ready: %w", err)
	}

	fmt.Println("FRR external container deployed successfully")
	return nil
}

func getContainerNetworkIPs(containerName, networkName string) (ipv4, ipv6 string, err error) {
	runtime := containerRuntime()

	// Get IPv4 container IP
	ipv4Tmpl := fmt.Sprintf("{{ with index .NetworkSettings.Networks %q }}{{ .IPAddress }}{{ end }}", networkName)
	ipv4, _ = runCmdOutput(runtime, "inspect", "-f", ipv4Tmpl, containerName)
	ipv4 = strings.Trim(ipv4, "'\"")
	if ipv4 == "<no value>" {
		ipv4 = ""
	}

	// Get IPv6 container IP
	ipv6Tmpl := fmt.Sprintf("{{ with index .NetworkSettings.Networks %q }}{{ .GlobalIPv6Address }}{{ end }}", networkName)
	ipv6, _ = runCmdOutput(runtime, "inspect", "-f", ipv6Tmpl, containerName)
	ipv6 = strings.Trim(ipv6, "'\"")
	if ipv6 == "<no value>" {
		ipv6 = ""
	}

	return ipv4, ipv6, nil
}

// generateFRRConfigFiles generates FRR configuration files in the specified directory
func generateFRRConfigFiles(cfg *Config, neighborsIPv4, neighborsIPv6 []string, outputDir string) error {
	// Combine IPv4 and IPv6 neighbors
	var neighborIPs []string
	neighborIPs = append(neighborIPs, neighborsIPv4...)
	neighborIPs = append(neighborIPs, neighborsIPv6...)

	// Prepare networks to advertise (the BGP server network)
	var advertiseNetworks []string
	if cfg.IPv4Enabled {
		advertiseNetworks = append(advertiseNetworks, cfg.BGPServerSubnetIPv4)
	}
	if cfg.IPv6Enabled {
		advertiseNetworks = append(advertiseNetworks, cfg.BGPServerSubnetIPv6)
	}

	if err := frr.WriteConfigToDir(frrTemplateSource, outputDir, neighborIPs, advertiseNetworks); err != nil {
		return err
	}

	fmt.Printf("Generated FRR configuration in %s\n", outputDir)
	return nil
}

func waitForContainer(name string) error {
	runtime := containerRuntime()
	return wait.PollUntilContextTimeout(context.Background(), pollInterval, containerTimeout, true, func(ctx context.Context) (bool, error) {
		out, err := runCmdOutput(runtime, "inspect", "-f", "{{.State.Running}}", name)
		if err != nil {
			return false, nil
		}
		return strings.TrimSpace(out) == "true", nil
	})
}

// waitForFRRDaemons waits for FRR daemons (zebra and bgpd) to be fully operational
// by checking if vtysh can successfully query the running configuration.
func waitForFRRDaemons(containerName string) error {
	runtime := containerRuntime()
	return wait.PollUntilContextTimeout(context.Background(), pollInterval, containerTimeout, true, func(ctx context.Context) (bool, error) {
		// Check if vtysh can connect and query BGP - this confirms both zebra and bgpd are running
		out, err := runCmdOutput(runtime, "exec", containerName, "vtysh", "-c", "show bgp summary")
		if err != nil {
			// Daemon not ready yet
			return false, nil
		}
		// If we get any output (even "No BGP neighbors"), the daemons are operational
		return len(strings.TrimSpace(out)) > 0, nil
	})
}

func deployBGPExternalServer(cfg *Config, nodes []corev1.Node) error {
	runtime := containerRuntime()

	// Remove existing bgpserver container if present
	if containerExists(bgpServerContainerName) {
		fmt.Println("Removing existing bgpserver container...")
		runCmd(runtime, "rm", "-f", bgpServerContainerName)
	}

	// Remove existing bgpnet network if present
	if networkExists(bgpNetworkName) {
		fmt.Println("Removing existing bgpnet network...")
		runCmd(runtime, "network", "rm", bgpNetworkName)
	}

	// Create bgpnet network
	fmt.Println("Creating bgpnet network...")
	networkArgs := []string{"network", "create", "--driver", "bridge", "--subnet", cfg.BGPServerSubnetIPv4}
	if cfg.IPv6Enabled {
		networkArgs = append(networkArgs, "--ipv6", "--subnet", cfg.BGPServerSubnetIPv6)
	}
	networkArgs = append(networkArgs, bgpNetworkName)
	if err := runCmd(runtime, networkArgs...); err != nil {
		return fmt.Errorf("failed to create bgpnet network: %w", err)
	}

	// Connect FRR container to bgpnet
	fmt.Println("Connecting FRR container to bgpnet...")
	if err := runCmd(runtime, "network", "connect", bgpNetworkName, frrContainerName); err != nil {
		return fmt.Errorf("failed to connect FRR to bgpnet: %w", err)
	}

	// Create bgpserver container using the images package
	fmt.Println("Creating bgpserver container...")
	agnhost := images.AgnHost()
	serverArgs := []string{
		"run", "-d",
		"--cap-add", "NET_ADMIN",
		"--user", "0",
		"--network", bgpNetworkName,
		"--rm",
		"--name", bgpServerContainerName,
		"-p", bgpServerPortMapping,
		agnhost,
		"netexec",
	}
	if err := runCmd(runtime, serverArgs...); err != nil {
		return fmt.Errorf("failed to create bgpserver container: %w", err)
	}

	// Wait for container to be running
	if err := waitForContainer(bgpServerContainerName); err != nil {
		return fmt.Errorf("bgpserver container failed to start: %w", err)
	}

	// Get FRR's IP on bgpnet and set as default gateway for bgpserver
	frrIPv4, frrIPv6, err := getContainerNetworkIPs(frrContainerName, bgpNetworkName)
	if err != nil {
		return fmt.Errorf("failed to get FRR IPs on bgpnet: %w", err)
	}

	if cfg.IPv4Enabled && frrIPv4 != "" {
		fmt.Printf("Setting bgpserver default IPv4 gateway via FRR (%s)...\n", frrIPv4)
		if err := runCmd(runtime, "exec", bgpServerContainerName, "ip", "route", "replace", "default", "via", frrIPv4); err != nil {
			return fmt.Errorf("failed to set IPv4 default gateway: %w", err)
		}
	}

	if cfg.IPv6Enabled && frrIPv6 != "" {
		fmt.Printf("Setting bgpserver default IPv6 gateway via FRR (%s)...\n", frrIPv6)
		if err := runCmd(runtime, "exec", bgpServerContainerName, "ip", "-6", "route", "replace", "default", "via", frrIPv6); err != nil {
			return fmt.Errorf("failed to set IPv6 default gateway: %w", err)
		}
	}

	// Handle isolation mode specific setup
	if cfg.IsolationMode == "loose" {
		// In loose mode, set nodes' default gateway to FRR router
		frrKindIPv4, frrKindIPv6, err := getContainerNetworkIPs(frrContainerName, kindNetwork)
		if err != nil {
			return fmt.Errorf("failed to get FRR IPs on kind network: %w", err)
		}

		for _, node := range nodes {
			if cfg.IPv4Enabled && frrKindIPv4 != "" {
				fmt.Printf("Setting node %s default IPv4 gateway to FRR (%s)...\n", node.Name, frrKindIPv4)
				runCmd(runtime, "exec", node.Name, "ip", "route", "replace", "default", "via", frrKindIPv4)
			}
			if cfg.IPv6Enabled && frrKindIPv6 != "" {
				fmt.Printf("Setting node %s default IPv6 gateway to FRR (%s)...\n", node.Name, frrKindIPv6)
				runCmd(runtime, "exec", node.Name, "ip", "-6", "route", "replace", "default", "via", frrKindIPv6)
			}
		}
	} else {
		// In strict mode, disable default routes on FRR
		fmt.Println("Disabling default routes on FRR container (strict mode)...")
		runCmd(runtime, "exec", frrContainerName, "ip", "route", "delete", "default")
		if cfg.IPv6Enabled {
			runCmd(runtime, "exec", frrContainerName, "ip", "-6", "route", "delete", "default")
		}
	}

	fmt.Println("BGP external server deployed successfully")
	return nil
}

func installFRRK8s(cfg *Config) error {
	// Wait for API server to be ready
	if err := waitForAPIServer(60 * time.Second); err != nil {
		return fmt.Errorf("API server not ready: %w", err)
	}

	// Apply frr-k8s deployment from remote URL
	frrK8sURL := frrK8sRemoteURL(cfg.FRRK8sVersion, "config/all-in-one/frr-k8s.yaml")
	fmt.Printf("Applying frr-k8s deployment from %s...\n", frrK8sURL)
	if err := runKubectlWithRetry(3, "apply", "--validate=false", "-f", frrK8sURL); err != nil {
		return fmt.Errorf("failed to apply frr-k8s: %w", err)
	}

	// Wait for statuscleaner deployment
	fmt.Println("Waiting for frr-k8s statuscleaner deployment...")
	if err := runKubectl("wait", "-n", frrK8sNS, "deployment", frrK8sDeploymentName, "--for", "condition=Available", "--timeout", deployTimeout); err != nil {
		return fmt.Errorf("frr-k8s statuscleaner did not become ready: %w", err)
	}

	// Wait for daemon rollout
	fmt.Println("Waiting for frr-k8s daemon rollout...")
	if err := runKubectl("rollout", "status", "-n", frrK8sNS, "daemonset", frrK8sDaemonsetName, "--timeout", deployTimeout); err != nil {
		return fmt.Errorf("frr-k8s daemon rollout failed: %w", err)
	}

	// Wait for webhook endpoint to be actually serving
	fmt.Println("Probing frr-k8s webhook endpoint...")
	if err := waitForFRRK8sWebhook(); err != nil {
		fmt.Printf("Warning: webhook probe failed: %v\n", err)
	}

	fmt.Println("frr-k8s installed successfully")
	return nil
}

func waitForFRRK8sWebhook() error {
	runtime := containerRuntime()
	return wait.PollUntilContextTimeout(context.Background(), pollInterval, containerTimeout, true, func(ctx context.Context) (bool, error) {
		// Get webhook service cluster IP
		clusterIP, err := runKubectlOutput("get", "svc", "-n", frrK8sNS, frrK8sWebhookServiceName, "-o", "jsonpath={.spec.clusterIP}")
		if err != nil {
			return false, nil
		}
		clusterIP = strings.TrimSpace(clusterIP)
		if clusterIP == "" {
			return false, nil
		}

		// Wrap IPv6 addresses in brackets
		if strings.Contains(clusterIP, ":") {
			clusterIP = "[" + clusterIP + "]"
		}

		// Try to curl the webhook from control plane
		url := fmt.Sprintf("https://%s", clusterIP)
		_, err = runCmdOutput(runtime, "exec", controlPlaneNodeName, "curl", "-ksS", "--connect-timeout", "1", url)
		if err != nil {
			return false, nil
		}
		return true, nil
	})
}

func createDefaultNetworkFRRConfiguration(cfg *Config) error {
	// Get FRR container IPs on the primary (kind) network for BGP peering
	frrIPv4, frrIPv6, err := getContainerNetworkIPs(frrContainerName, kindNetwork)
	if err != nil {
		return fmt.Errorf("failed to get FRR IPs on kind network: %w", err)
	}

	var neighborIPs []string
	if cfg.IPv4Enabled && frrIPv4 != "" {
		neighborIPs = append(neighborIPs, frrIPv4)
	}
	if cfg.IPv6Enabled && frrIPv6 != "" {
		neighborIPs = append(neighborIPs, frrIPv6)
	}

	fmt.Printf("FRR container IPs for BGP peering: %v\n", neighborIPs)

	// Prepare receive networks (the BGP server subnet)
	var receiveNetworks []string
	if cfg.IPv4Enabled {
		receiveNetworks = append(receiveNetworks, cfg.BGPServerSubnetIPv4)
	}
	if cfg.IPv6Enabled {
		receiveNetworks = append(receiveNetworks, cfg.BGPServerSubnetIPv6)
	}

	// Generate FRRConfiguration YAML
	frrConfigDir, err := generateFRRk8sConfiguration(cfg.NetworkName, neighborIPs, receiveNetworks)
	if err != nil {
		return fmt.Errorf("failed to generate FRR-k8s configuration: %w", err)
	}
	defer os.RemoveAll(frrConfigDir)

	// Apply the FRRConfiguration
	frrConfPath := filepath.Join(frrConfigDir, "frrconf.yaml")
	if err := runKubectl("apply", "--validate=false", "-n", frrK8sNS, "-f", frrConfPath); err != nil {
		return fmt.Errorf("failed to apply FRRConfiguration: %w", err)
	}

	fmt.Println("FRRConfiguration for default network created successfully")
	return nil
}

func generateFRRk8sConfiguration(networkName string, neighborIPs, receiveNetworks []string) (string, error) {
	// Labels must include "name: receive-all" to match the RouteAdvertisements selector
	// which expects frrConfigurationSelector.matchLabels.name: receive-all
	labels := map[string]string{
		"network": networkName,
		"name":    "receive-all",
	}

	// For the default network, don't specify a VRF (it uses the main routing table).
	// For other networks (UDNs), the VRF should be the network name.
	var vrf string
	if networkName != "default" {
		vrf = networkName
	}

	tmpDir, err := frr.GenerateK8sConfigurationWithVRF(frrTemplateSource, networkName, vrf, labels, neighborIPs, receiveNetworks)
	if err != nil {
		return "", err
	}

	fmt.Printf("Generated FRR-k8s configuration in %s\n", tmpDir)
	return tmpDir, nil
}

func addPodNetworkRoutes(cfg *Config, clientset *kubernetes.Clientset) error {
	var errs []error

	nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	for _, node := range nodes.Items {
		// Get node's internal IPs
		var nodeIPv4, nodeIPv6 string
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				ip := net.ParseIP(addr.Address)
				if ip == nil {
					continue
				}
				if ip.To4() != nil {
					nodeIPv4 = addr.Address
				} else {
					nodeIPv6 = addr.Address
				}
			}
		}

		// Get node subnets from annotations
		subnetAnnotation := node.Annotations["k8s.ovn.org/node-subnets"]
		if subnetAnnotation == "" {
			fmt.Printf("Warning: Node %s has no subnet annotation\n", node.Name)
			continue
		}

		// Parse subnet annotation (JSON format like {"default":["10.244.0.0/24"]})
		subnets := parseSubnetAnnotation(subnetAnnotation)

		for _, subnet := range subnets {
			_, ipNet, err := net.ParseCIDR(subnet)
			if err != nil {
				continue
			}

			isIPv6 := ipNet.IP.To4() == nil
			var via string
			if isIPv6 && cfg.IPv6Enabled && nodeIPv6 != "" {
				via = nodeIPv6
			} else if !isIPv6 && cfg.IPv4Enabled && nodeIPv4 != "" {
				via = nodeIPv4
			}

			if via != "" {
				fmt.Printf("Adding route for %s via %s (node %s)\n", subnet, via, node.Name)
				args := []string{"route", "replace", subnet, "via", via}
				if isIPv6 {
					args = append([]string{"-6"}, args...)
				}
				// This requires root privileges
				if err := runCmd("sudo", append([]string{"ip"}, args...)...); err != nil {
					errs = append(errs, fmt.Errorf("failed to add route for %s via %s (node %s): %w", subnet, via, node.Name, err))
				}
			}
		}
	}

	return errors.Join(errs...)
}

func parseSubnetAnnotation(annotation string) []string {
	var subnets []string

	// Parse the JSON annotation which has format: {"default":["10.244.0.0/24"]}
	// It's a map of network names to arrays of subnet strings
	var networkSubnets map[string][]string
	if err := json.Unmarshal([]byte(annotation), &networkSubnets); err != nil {
		return subnets
	}

	// Collect all subnets from all networks
	for _, nets := range networkSubnets {
		subnets = append(subnets, nets...)
	}

	return subnets
}

func cleanup() error {
	runtime := containerRuntime()

	// Remove bgpserver container
	if containerExists(bgpServerContainerName) {
		fmt.Println("Removing bgpserver container...")
		runCmd(runtime, "stop", bgpServerContainerName)
		runCmd(runtime, "rm", "-f", bgpServerContainerName)
	}

	// Remove FRR container
	if containerExists(frrContainerName) {
		fmt.Println("Removing FRR container...")
		runCmd(runtime, "stop", frrContainerName)
		runCmd(runtime, "rm", "-f", frrContainerName)
	}

	// Remove bgpnet network
	if networkExists(bgpNetworkName) {
		fmt.Println("Removing bgpnet network...")
		runCmd(runtime, "network", "rm", bgpNetworkName)
	}

	// Delete FRRConfiguration resources
	fmt.Println("Deleting FRRConfiguration resources...")
	runKubectl("delete", "frrconfigurations", "--all", "-n", frrK8sNS)

	// Delete frr-k8s deployment
	fmt.Println("Deleting frr-k8s deployment...")
	runKubectl("delete", "-n", frrK8sNS, "deployment", frrK8sDeploymentName, "--ignore-not-found")
	runKubectl("delete", "-n", frrK8sNS, "daemonset", frrK8sDaemonsetName, "--ignore-not-found")

	// Clean up persistent FRR config directory
	if _, err := os.Stat(frrConfigDir); err == nil {
		fmt.Println("Removing FRR config directory...")
		os.RemoveAll(frrConfigDir)
	}

	return nil
}
