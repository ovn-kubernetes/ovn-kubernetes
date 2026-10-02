// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

// Environment variables consumed by SSHConfigFromEnv.
const (
	// EnvSSHHost is the SSH host (IP or resolvable name) of the machine running
	// the container runtime. Required.
	EnvSSHHost = "OVN_TEST_SSH_HOST"
	// EnvSSHUser is the SSH user. Required.
	EnvSSHUser = "OVN_TEST_SSH_USER"
	// EnvSSHPort is the SSH port. Optional, defaults to DefaultSSHPort.
	EnvSSHPort = "OVN_TEST_SSH_PORT"
	// EnvSSHKey is the path to the SSH private key file used for public-key
	// auth. Required.
	EnvSSHKey = "OVN_TEST_SSH_KEY"
	// EnvSSHKnownHosts is the path to an SSH known_hosts file used for host-key
	// verification. Optional; when unset the runner falls back to
	// ~/.ssh/known_hosts if present.
	EnvSSHKnownHosts = "OVN_TEST_SSH_KNOWN_HOSTS"
	// EnvSSHInsecureHostKey opts out of host-key verification (test-only).
	EnvSSHInsecureHostKey = "OVN_TEST_SSH_INSECURE_HOST_KEY"
)

// DefaultSSHPort is used when EnvSSHPort is unset.
const DefaultSSHPort = "22"

// SSHConfig describes how to reach a remote host over SSH. The e2e suite fills
// it from environment variables (SSHConfigFromEnv); other callers can populate
// the same struct from their own sources.
type SSHConfig struct {
	// Host is the SSH host (IP or name) of the machine to run commands on.
	Host string
	// User is the SSH user.
	User string
	// Port is the SSH port (string, to match the underlying runner API).
	Port string
	// PrivateKeyPath is the filesystem path to the SSH private key.
	PrivateKeyPath string
	// KnownHostsPath is the SSH known_hosts file for host-key verification.
	KnownHostsPath string
	// InsecureHostKey disables SSH host-key verification when true.
	InsecureHostKey bool
}

// SSHConfigFromEnv builds an SSHConfig from environment variables and validates
// it. Required: OVN_TEST_SSH_HOST, OVN_TEST_SSH_USER, OVN_TEST_SSH_KEY.
func SSHConfigFromEnv() (SSHConfig, error) {
	cfg := SSHConfig{
		Host:            strings.TrimSpace(os.Getenv(EnvSSHHost)),
		User:            strings.TrimSpace(os.Getenv(EnvSSHUser)),
		Port:            strings.TrimSpace(os.Getenv(EnvSSHPort)),
		PrivateKeyPath:  strings.TrimSpace(os.Getenv(EnvSSHKey)),
		KnownHostsPath:  strings.TrimSpace(os.Getenv(EnvSSHKnownHosts)),
		InsecureHostKey: strings.TrimSpace(os.Getenv(EnvSSHInsecureHostKey)) == "1",
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return SSHConfig{}, err
	}
	return cfg, nil
}

// NewSSHRunnerFromConfig returns a runner that executes commands on cfg's host.
func NewSSHRunnerFromConfig(cfg SSHConfig) (api.Runner, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	r, err := NewSSHRunnerWithOptions(cfg.Host, cfg.User, cfg.Port, cfg.PrivateKeyPath, SSHOptions{
		KnownHostsPath:  cfg.KnownHostsPath,
		InsecureHostKey: cfg.InsecureHostKey,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create ssh runner for %s@%s: %w", cfg.User, cfg.Host, err)
	}
	return r, nil
}

// applyDefaults normalizes fields (so direct SSHConfig consumers get the same
// treatment as SSHConfigFromEnv) and fills optional fields that were left empty.
func (c *SSHConfig) applyDefaults() {
	c.Host = strings.TrimSpace(c.Host)
	c.User = strings.TrimSpace(c.User)
	c.Port = strings.TrimSpace(c.Port)
	c.PrivateKeyPath = expandHome(strings.TrimSpace(c.PrivateKeyPath))
	if c.Port == "" {
		c.Port = DefaultSSHPort
	}
}

// expandHome expands a leading "~/" (or a bare "~") in path to the invoking
// user's home directory, so common forms like OVN_TEST_SSH_KEY=~/.ssh/id_ed25519
// work (the SSH runner passes the value straight to os.ReadFile, which does not
// expand "~"). Other forms (e.g. "~otheruser") are intentionally left untouched.
// If the home directory cannot be resolved, path is returned unchanged so a later
// read surfaces a clear error rather than a silently-wrong path.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[len("~/"):])
}

// Validate checks that required fields are present and optional fields are sane.
// It aggregates all problems into a single error so callers see everything at
// once. Note: applyDefaults must have run (SSHConfigFromEnv and
// NewSSHRunnerFromConfig both ensure this) so that Port is populated.
func (c *SSHConfig) Validate() error {
	var problems []string
	if c.Host == "" {
		problems = append(problems, fmt.Sprintf("%s (SSH host) is required", EnvSSHHost))
	} else if err := validateSSHHost(c.Host); err != nil {
		problems = append(problems, err.Error())
	}
	if c.User == "" {
		problems = append(problems, fmt.Sprintf("%s (SSH user) is required", EnvSSHUser))
	}
	if c.PrivateKeyPath == "" {
		problems = append(problems, fmt.Sprintf("%s (SSH private key path) is required", EnvSSHKey))
	}
	if c.Port != "" {
		if p, err := strconv.Atoi(c.Port); err != nil || p < 1 || p > 65535 {
			problems = append(problems, fmt.Sprintf("%s must be a valid TCP port (1-65535), got %q", EnvSSHPort, c.Port))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid ssh configuration: %s", strings.Join(problems, "; "))
}

// validateSSHHost rejects values that are not a bare hostname or IP. The SSH port
// belongs in OVN_TEST_SSH_PORT / SSHConfig.Port, not embedded in the host string.
func validateSSHHost(host string) error {
	if strings.ContainsFunc(host, unicode.IsSpace) {
		return fmt.Errorf("%s must be a bare host or IP (no whitespace), got %q", EnvSSHHost, host)
	}
	if strings.Contains(host, "://") {
		return fmt.Errorf("%s must be a bare host or IP (no scheme), got %q", EnvSSHHost, host)
	}
	if strings.HasPrefix(host, "[") {
		return fmt.Errorf("%s must be a bare host or IP (no brackets), got %q", EnvSSHHost, host)
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	if strings.Contains(host, ":") {
		if hostHasNumericPortSuffix(host) {
			return fmt.Errorf("%s must not include a port (use %s), got %q", EnvSSHHost, EnvSSHPort, host)
		}
		return fmt.Errorf("%s must be a bare host or IP, got %q", EnvSSHHost, host)
	}
	return nil
}

func hostHasNumericPortSuffix(host string) bool {
	if strings.Count(host, ":") != 1 {
		return false
	}
	_, portPart, found := strings.Cut(host, ":")
	if !found || portPart == "" {
		return true
	}
	p, err := strconv.Atoi(portPart)
	return err == nil && p >= 1 && p <= 65535
}
