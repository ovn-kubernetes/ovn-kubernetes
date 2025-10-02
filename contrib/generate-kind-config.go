package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"text/template"
)

type KindConfig struct {
	NetCIDR               string
	SvcCIDR               string
	IPFamily              string
	UseLocalRegistry      bool
	KindLocalRegistryPort string
	KindLocalRegistryName string
	ClusterLogLevel       string
	DNSDomain             string
	OVNHA                 bool
	NumMaster             int
	NumWorker             int
	AdditionalMasterNodes []int
	WorkerNodes           []int
}

func main() {
	// Define CLI flags
	var (
		netCIDR               = flag.String("net-cidr", "", "Pod network CIDR")
		svcCIDR               = flag.String("svc-cidr", "", "Service network CIDR")
		ipFamily              = flag.String("ip-family", "", "IP family (ipv4/ipv6/dual)")
		useLocalRegistry      = flag.Bool("use-local-registry", false, "Enable local registry")
		kindLocalRegistryPort = flag.String("kind-local-registry-port", "5000", "Local registry port")
		kindLocalRegistryName = flag.String("kind-local-registry-name", "kind-registry", "Local registry name")
		clusterLogLevel       = flag.String("cluster-log-level", "4", "Kubernetes log level")
		dnsDomain             = flag.String("dns-domain", "cluster.local", "DNS domain")
		ovnHA                 = flag.Bool("ovn-ha", false, "Enable HA mode")
		numMaster             = flag.Int("num-master", 1, "Number of master nodes")
		numWorker             = flag.Int("num-worker", 2, "Number of worker nodes")
		templateFile          = flag.String("template", "kind.yaml.gotmpl", "Template file path")
		outputFile            = flag.String("output", "", "Output file path")
		help                  = flag.Bool("help", false, "Show help")
	)

	flag.Parse()

	if *help {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] -output <file>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		os.Exit(0)
	}

	if *outputFile == "" {
		fmt.Fprintf(os.Stderr, "Error: -output flag is required\n")
		fmt.Fprintf(os.Stderr, "Use -help for usage information\n")
		os.Exit(1)
	}

	if *templateFile == "" {
		fmt.Fprintf(os.Stderr, "Error: -template flag is required\n")
		fmt.Fprintf(os.Stderr, "Use -help for usage information\n")
		os.Exit(1)
	}

	// Create config from CLI flags
	config := &KindConfig{
		NetCIDR:               *netCIDR,
		SvcCIDR:               *svcCIDR,
		IPFamily:              *ipFamily,
		UseLocalRegistry:      *useLocalRegistry,
		KindLocalRegistryPort: *kindLocalRegistryPort,
		KindLocalRegistryName: *kindLocalRegistryName,
		ClusterLogLevel:       *clusterLogLevel,
		DNSDomain:             *dnsDomain,
		OVNHA:                 *ovnHA,
		NumMaster:             *numMaster,
		NumWorker:             *numWorker,
	}

	// Generate additional master nodes for HA (excluding the first one which is control-plane)
	if config.OVNHA && config.NumMaster > 1 {
		config.AdditionalMasterNodes = make([]int, config.NumMaster-1)
		for i := range config.AdditionalMasterNodes {
			config.AdditionalMasterNodes[i] = i
		}
	}

	// Generate worker nodes
	config.WorkerNodes = make([]int, config.NumWorker)
	for i := range config.WorkerNodes {
		config.WorkerNodes[i] = i
	}

	// Parse template
	tmpl, err := template.ParseFiles(*templateFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing template: %v\n", err)
		os.Exit(1)
	}

	// Execute template
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, config); err != nil {
		fmt.Fprintf(os.Stderr, "Error executing template: %v\n", err)
		os.Exit(1)
	}

	// Write to output file
	if err := os.WriteFile(*outputFile, buf.Bytes(), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output file: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Generated kind config: %s\n", *outputFile)
}
