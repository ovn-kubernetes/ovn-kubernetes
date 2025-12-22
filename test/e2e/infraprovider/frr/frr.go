// Package frr provides shared FRR configuration types and utilities
// for generating FRR and FRR-K8s configuration files.
// This package is used by both route_advertisements tests and the bgp-setup tool.
package frr

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"text/template"

	utilnet "k8s.io/utils/net"
)

const (
	// Image is the default FRR container image used for BGP routing
	Image = "quay.io/frrouting/frr:10.4.1"
)

// RouterConfig contains router configuration for FRR templates
type RouterConfig struct {
	VRF           string
	NeighborsIPv4 []string
	NeighborsIPv6 []string
	NetworksIPv4  []string
	NetworksIPv6  []string
}

// Config contains FRR configuration data for templates
type Config struct {
	// Name and Labels are used for FRRConfiguration (FRR-K8s) metadata
	Name    string
	Labels  map[string]string
	Routers []RouterConfig
}

// TemplateSource provides an abstraction over template loading from either
// an embedded filesystem (for tests) or a real filesystem (for standalone tools)
type TemplateSource interface {
	// ParseTemplates parses all .tmpl files in the given subdirectory
	ParseTemplates(subdir string) (*template.Template, error)
}

// EmbedFSSource loads templates from an embedded filesystem
type EmbedFSSource struct {
	FS      embed.FS
	BaseDir string // e.g., "testdata/routeadvertisements"
}

// ParseTemplates implements TemplateSource
func (e *EmbedFSSource) ParseTemplates(subdir string) (*template.Template, error) {
	pattern := filepath.Join(e.BaseDir, subdir, "*.tmpl")
	return template.ParseFS(e.FS, pattern)
}

// FilesystemSource loads templates from the real filesystem
type FilesystemSource struct {
	BaseDir string // Absolute path to template directory
}

// ParseTemplates implements TemplateSource
func (f *FilesystemSource) ParseTemplates(subdir string) (*template.Template, error) {
	pattern := filepath.Join(f.BaseDir, subdir, "*.tmpl")
	return template.ParseGlob(pattern)
}

// GenerateConfiguration generates external FRR configuration files for BGP routing.
// It returns a temporary directory containing the configuration files.
//
// The generated files are:
//   - frr.conf: FRR BGP configuration
//   - daemons: FRR daemon configuration
//
// Parameters:
//   - src: Template source (embedded or filesystem)
//   - neighborIPs: IPs of BGP neighbors (cluster nodes)
//   - advertiseNetworks: Networks to advertise via BGP
//
// Caller is responsible for cleaning up the returned directory.
func GenerateConfiguration(src TemplateSource, neighborIPs, advertiseNetworks []string) (directory string, err error) {
	templates, err := src.ParseTemplates("frr")
	if err != nil {
		return "", fmt.Errorf("failed to parse FRR templates: %w", err)
	}

	directory, err = os.MkdirTemp("", "frrconf-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(directory)
		}
	}()

	networksIPv4, networksIPv6 := SplitCIDRStringsByIPFamily(advertiseNetworks)
	neighborsIPv4, neighborsIPv6 := SplitIPStringsByIPFamily(neighborIPs)

	conf := Config{
		Routers: []RouterConfig{
			{
				NeighborsIPv4: neighborsIPv4,
				NetworksIPv4:  networksIPv4,
				NeighborsIPv6: neighborsIPv6,
				NetworksIPv6:  networksIPv6,
			},
		},
	}

	if err = ExecuteTemplateToFile(templates, directory, "frr.conf", conf); err != nil {
		return "", fmt.Errorf("failed to generate frr.conf: %w", err)
	}

	if err = ExecuteTemplateToFile(templates, directory, "daemons", nil); err != nil {
		return "", fmt.Errorf("failed to generate daemons: %w", err)
	}

	return directory, nil
}

// GenerateK8sConfiguration generates FRR-K8s (metallb/frr-k8s) configuration.
// It returns a temporary directory containing the FRRConfiguration YAML.
// The VRF is set to the networkName by default.
//
// Parameters:
//   - src: Template source (embedded or filesystem)
//   - networkName: Name for the FRRConfiguration resource and VRF
//   - labels: Labels for the FRRConfiguration resource
//   - neighborIPs: IPs of BGP neighbors (external FRR router)
//   - receiveNetworks: Networks to accept via BGP
//
// Caller is responsible for cleaning up the returned directory.
func GenerateK8sConfiguration(src TemplateSource, networkName string, labels map[string]string, neighborIPs, receiveNetworks []string) (directory string, err error) {
	return GenerateK8sConfigurationWithVRF(src, networkName, networkName, labels, neighborIPs, receiveNetworks)
}

// GenerateK8sConfigurationWithVRF generates FRR-K8s configuration with explicit VRF control.
// Use this when you need to specify a different VRF name or use no VRF (empty string for default network).
//
// Parameters:
//   - src: Template source (embedded or filesystem)
//   - networkName: Name for the FRRConfiguration resource
//   - vrf: VRF name (empty string for default/no VRF)
//   - labels: Labels for the FRRConfiguration resource
//   - neighborIPs: IPs of BGP neighbors (external FRR router)
//   - receiveNetworks: Networks to accept via BGP
//
// Caller is responsible for cleaning up the returned directory.
func GenerateK8sConfigurationWithVRF(src TemplateSource, networkName, vrf string, labels map[string]string, neighborIPs, receiveNetworks []string) (directory string, err error) {
	templates, err := src.ParseTemplates("frr-k8s")
	if err != nil {
		return "", fmt.Errorf("failed to parse FRR-K8s templates: %w", err)
	}

	directory, err = os.MkdirTemp("", "frrk8sconf-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(directory)
		}
	}()

	receivesIPv4, receivesIPv6 := SplitCIDRStringsByIPFamily(receiveNetworks)
	neighborsIPv4, neighborsIPv6 := SplitIPStringsByIPFamily(neighborIPs)

	conf := Config{
		Name:   networkName,
		Labels: labels,
		Routers: []RouterConfig{
			{
				VRF:           vrf,
				NeighborsIPv4: neighborsIPv4,
				NeighborsIPv6: neighborsIPv6,
				NetworksIPv4:  receivesIPv4,
				NetworksIPv6:  receivesIPv6,
			},
		},
	}

	if err = ExecuteTemplateToFile(templates, directory, "frrconf.yaml", conf); err != nil {
		return "", fmt.Errorf("failed to generate frrconf.yaml: %w", err)
	}

	return directory, nil
}

// WriteConfigToDir writes FRR configuration directly to a specified directory
// instead of creating a temp directory. This is useful for the bgp-setup tool
// which needs persistent configuration.
//
// Parameters:
//   - src: Template source (embedded or filesystem)
//   - outputDir: Directory to write configuration files to
//   - neighborIPs: IPs of BGP neighbors
//   - advertiseNetworks: Networks to advertise via BGP
func WriteConfigToDir(src TemplateSource, outputDir string, neighborIPs, advertiseNetworks []string) error {
	templates, err := src.ParseTemplates("frr")
	if err != nil {
		return fmt.Errorf("failed to parse FRR templates: %w", err)
	}

	networksIPv4, networksIPv6 := SplitCIDRStringsByIPFamily(advertiseNetworks)
	neighborsIPv4, neighborsIPv6 := SplitIPStringsByIPFamily(neighborIPs)

	conf := Config{
		Routers: []RouterConfig{
			{
				NeighborsIPv4: neighborsIPv4,
				NetworksIPv4:  networksIPv4,
				NeighborsIPv6: neighborsIPv6,
				NetworksIPv6:  networksIPv6,
			},
		},
	}

	if err := ExecuteTemplateToFile(templates, outputDir, "frr.conf", conf); err != nil {
		return fmt.Errorf("failed to generate frr.conf: %w", err)
	}

	if err := ExecuteTemplateToFile(templates, outputDir, "daemons", nil); err != nil {
		return fmt.Errorf("failed to generate daemons: %w", err)
	}

	return nil
}

// WriteK8sConfigToDir writes FRR-K8s configuration directly to a specified directory
// instead of creating a temp directory. This is useful for the bgp-setup tool
// which needs persistent configuration.
// VRF is set to networkName by default.
func WriteK8sConfigToDir(src TemplateSource, outputDir, networkName string, labels map[string]string, neighborIPs, receiveNetworks []string) error {
	return WriteK8sConfigToDirWithVRF(src, outputDir, networkName, networkName, labels, neighborIPs, receiveNetworks)
}

// WriteK8sConfigToDirWithVRF writes FRR-K8s configuration with explicit VRF control.
// Use this when you need to specify a different VRF name or use no VRF (empty string for default network).
func WriteK8sConfigToDirWithVRF(src TemplateSource, outputDir, networkName, vrf string, labels map[string]string, neighborIPs, receiveNetworks []string) error {
	templates, err := src.ParseTemplates("frr-k8s")
	if err != nil {
		return fmt.Errorf("failed to parse FRR-K8s templates: %w", err)
	}

	receivesIPv4, receivesIPv6 := SplitCIDRStringsByIPFamily(receiveNetworks)
	neighborsIPv4, neighborsIPv6 := SplitIPStringsByIPFamily(neighborIPs)

	conf := Config{
		Name:   networkName,
		Labels: labels,
		Routers: []RouterConfig{
			{
				VRF:           vrf,
				NeighborsIPv4: neighborsIPv4,
				NeighborsIPv6: neighborsIPv6,
				NetworksIPv4:  receivesIPv4,
				NetworksIPv6:  receivesIPv6,
			},
		},
	}

	if err := ExecuteTemplateToFile(templates, outputDir, "frrconf.yaml", conf); err != nil {
		return fmt.Errorf("failed to generate frrconf.yaml: %w", err)
	}

	return nil
}

// ExecuteTemplateToFile executes a named template and writes the output to a file.
func ExecuteTemplateToFile(templates *template.Template, directory, name string, data any) error {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		return fmt.Errorf("failed to execute template %q: %w", name, err)
	}

	outputPath := filepath.Join(directory, name)
	if err := os.WriteFile(outputPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write output file %q: %w", outputPath, err)
	}

	return nil
}

// SplitCIDRStringsByIPFamily splits CIDR strings into IPv4 and IPv6 slices.
func SplitCIDRStringsByIPFamily(cidrs []string) (ipv4, ipv6 []string) {
	for _, cidr := range cidrs {
		switch {
		case utilnet.IsIPv4CIDRString(cidr):
			ipv4 = append(ipv4, cidr)
		case utilnet.IsIPv6CIDRString(cidr):
			ipv6 = append(ipv6, cidr)
		}
	}
	return
}

// SplitIPStringsByIPFamily splits IP strings into IPv4 and IPv6 slices.
func SplitIPStringsByIPFamily(ips []string) (ipv4, ipv6 []string) {
	for _, ip := range ips {
		if ip == "" {
			continue
		}
		switch {
		case utilnet.IsIPv4String(ip):
			ipv4 = append(ipv4, ip)
		case utilnet.IsIPv6String(ip):
			ipv6 = append(ipv6, ip)
		}
	}
	return
}

// MatchCIDRStringsByIPFamily filters CIDRs to only include those matching the specified IP families.
func MatchCIDRStringsByIPFamily(cidrs []string, families ...utilnet.IPFamily) []string {
	familySet := make(map[utilnet.IPFamily]bool)
	for _, f := range families {
		familySet[f] = true
	}

	var matched []string
	for _, cidr := range cidrs {
		if utilnet.IsIPv4CIDRString(cidr) && familySet[utilnet.IPv4] {
			matched = append(matched, cidr)
		} else if utilnet.IsIPv6CIDRString(cidr) && familySet[utilnet.IPv6] {
			matched = append(matched, cidr)
		}
	}
	return matched
}

// NewEmbedFSSource creates a TemplateSource from an embedded filesystem.
// The baseDir should be the path within the embed.FS to the template root directory.
func NewEmbedFSSource(embedFS embed.FS, baseDir string) TemplateSource {
	return &EmbedFSSource{
		FS:      embedFS,
		BaseDir: baseDir,
	}
}

// NewFilesystemSource creates a TemplateSource from a real filesystem path.
func NewFilesystemSource(baseDir string) TemplateSource {
	return &FilesystemSource{
		BaseDir: baseDir,
	}
}

// CreateEmbedFSSource creates a TemplateSource from an fs.FS (for compatibility with io/fs).
// This is a helper for cases where you have a generic fs.FS rather than embed.FS.
func CreateEmbedFSSource(fsys fs.FS, baseDir string) TemplateSource {
	return &genericFSSource{
		fsys:    fsys,
		baseDir: baseDir,
	}
}

// genericFSSource wraps a generic fs.FS for template loading
type genericFSSource struct {
	fsys    fs.FS
	baseDir string
}

// ParseTemplates implements TemplateSource
func (g *genericFSSource) ParseTemplates(subdir string) (*template.Template, error) {
	pattern := filepath.Join(g.baseDir, subdir, "*.tmpl")
	return template.ParseFS(g.fsys, pattern)
}
