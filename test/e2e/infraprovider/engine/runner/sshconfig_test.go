// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearSSHEnv gives each test a known environment, whatever the developer shell
// has exported.
func clearSSHEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{EnvSSHHost, EnvSSHUser, EnvSSHPort, EnvSSHKey, EnvSSHKnownHosts, EnvSSHInsecureHostKey} {
		t.Setenv(key, "")
	}
}

func TestSSHConfigFromEnvAppliesDefaults(t *testing.T) {
	clearSSHEnv(t)
	t.Setenv(EnvSSHHost, "runtime.example.com")
	t.Setenv(EnvSSHUser, "tester")
	t.Setenv(EnvSSHKey, "/home/tester/.ssh/id_ed25519")

	cfg, err := SSHConfigFromEnv()
	if err != nil {
		t.Fatalf("SSHConfigFromEnv: %v", err)
	}
	if cfg.Port != DefaultSSHPort {
		t.Fatalf("default port not applied: got %q, want %q", cfg.Port, DefaultSSHPort)
	}
	if cfg.InsecureHostKey {
		t.Fatal("host key verification must be on unless explicitly disabled")
	}
}

func TestSSHConfigFromEnvReadsOverrides(t *testing.T) {
	clearSSHEnv(t)
	t.Setenv(EnvSSHHost, "10.0.0.5")
	t.Setenv(EnvSSHUser, "tester")
	t.Setenv(EnvSSHKey, "/keys/id_ed25519")
	t.Setenv(EnvSSHPort, "2222")
	t.Setenv(EnvSSHKnownHosts, "/keys/known_hosts")
	t.Setenv(EnvSSHInsecureHostKey, "1")

	cfg, err := SSHConfigFromEnv()
	if err != nil {
		t.Fatalf("SSHConfigFromEnv: %v", err)
	}
	if cfg.Port != "2222" {
		t.Fatalf("port override not read: %+v", cfg)
	}
	if cfg.KnownHostsPath != "/keys/known_hosts" || !cfg.InsecureHostKey {
		t.Fatalf("host key options not read: %+v", cfg)
	}
}

func TestSSHConfigFromEnvReportsEveryMissingRequirement(t *testing.T) {
	clearSSHEnv(t)

	_, err := SSHConfigFromEnv()
	if err == nil {
		t.Fatal("expected an error when no required variable is set")
	}
	for _, want := range []string{EnvSSHHost, EnvSSHUser, EnvSSHKey} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}

func TestSSHConfigFromEnvExpandsKeyPathHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	clearSSHEnv(t)
	t.Setenv(EnvSSHHost, "10.0.0.5")
	t.Setenv(EnvSSHUser, "tester")
	t.Setenv(EnvSSHKey, "~/.ssh/id_ed25519")

	cfg, err := SSHConfigFromEnv()
	if err != nil {
		t.Fatalf("SSHConfigFromEnv: %v", err)
	}
	if want := filepath.Join(home, ".ssh", "id_ed25519"); cfg.PrivateKeyPath != want {
		t.Fatalf("private key path: got %q, want %q", cfg.PrivateKeyPath, want)
	}
}

func TestSSHConfigValidateRejectsBadValues(t *testing.T) {
	base := SSHConfig{Host: "10.0.0.5", User: "tester", PrivateKeyPath: "/keys/id_ed25519"}
	for _, tc := range []struct {
		name    string
		mutate  func(*SSHConfig)
		wantErr string
	}{
		{"host with port", func(c *SSHConfig) { c.Host = "10.0.0.5:2222" }, EnvSSHPort},
		{"host with scheme", func(c *SSHConfig) { c.Host = "ssh://10.0.0.5" }, "no scheme"},
		{"bracketed host", func(c *SSHConfig) { c.Host = "[fd00::1]" }, "no brackets"},
		{"port out of range", func(c *SSHConfig) { c.Port = "70000" }, EnvSSHPort},
		{"non numeric port", func(c *SSHConfig) { c.Port = "ssh" }, EnvSSHPort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			cfg.applyDefaults()
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected %+v to be rejected", cfg)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestSSHConfigValidateAcceptsIPv6Host(t *testing.T) {
	cfg := SSHConfig{Host: "fd00::1", User: "tester", PrivateKeyPath: "/keys/id_ed25519"}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestNewSSHRunnerFromConfigRejectsInvalidConfig checks that the runner is not
// built, and no connection attempted, when the configuration is unusable.
func TestNewSSHRunnerFromConfigRejectsInvalidConfig(t *testing.T) {
	_, err := NewSSHRunnerFromConfig(SSHConfig{User: "tester", PrivateKeyPath: "/keys/id_ed25519"})
	if err == nil {
		t.Fatal("expected an error when the host is missing")
	}
	if !strings.Contains(err.Error(), EnvSSHHost) {
		t.Fatalf("error %q does not mention %s", err, EnvSSHHost)
	}
}

// TestNewSSHRunnerFromConfigPassesHostKeyPolicy checks that the config's
// host-key fields reach the runner: a known_hosts path that does not exist must
// be rejected rather than falling back to ~/.ssh/known_hosts.
func TestNewSSHRunnerFromConfigPassesHostKeyPolicy(t *testing.T) {
	_, keyPath := newTestKey(t)
	_, err := NewSSHRunnerFromConfig(SSHConfig{
		Host:           "127.0.0.1",
		User:           "tester",
		PrivateKeyPath: keyPath,
		KnownHostsPath: filepath.Join(t.TempDir(), "absent_known_hosts"),
	})
	if err == nil {
		t.Fatal("expected an error when the known hosts file is missing")
	}
	if !strings.Contains(err.Error(), "host key verification required") {
		t.Fatalf("unexpected error %v", err)
	}
}
