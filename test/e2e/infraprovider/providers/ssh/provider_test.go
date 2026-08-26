// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

// testConfig is a valid configuration that never connects: New builds the runner
// but the SSH session is only established on the first command.
func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Host:            "10.0.0.5",
		User:            "tester",
		PrivateKeyPath:  writeTestKey(t),
		InsecureHostKey: true,
	}
}

func writeTestKey(t *testing.T) string {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err = os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Host = ""

	if _, err := New(cfg); err == nil {
		t.Fatal("expected New to reject a configuration without a host")
	}
}

func TestNewRequiresHostKeyPolicy(t *testing.T) {
	cfg := testConfig(t)
	cfg.InsecureHostKey = false
	cfg.KnownHostsPath = filepath.Join(t.TempDir(), "known_hosts")

	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected New to fail without a usable known_hosts file")
	}
	if !strings.Contains(err.Error(), "host key verification required") {
		t.Fatalf("unexpected error %v", err)
	}
}

// closableRunner records Close calls and reports a configurable error.
type closableRunner struct {
	api.Runner
	closes int
	err    error
}

func (r *closableRunner) Close() error {
	r.closes++
	return r.err
}

// TestProviderClosesItsRunner covers the ownership rule: the provider creates
// the SSH runner, so Provider.Close closes it and surfaces its error.
func TestProviderClosesItsRunner(t *testing.T) {
	runner := &closableRunner{}
	provider := &Provider{runner: runner}

	if err := provider.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if runner.closes != 1 {
		t.Fatalf("runner Close calls: got %d, want 1", runner.closes)
	}

	runner.err = errors.New("connection reset")
	if err := provider.Close(); !errors.Is(err, runner.err) {
		t.Fatalf("Close error: got %v, want the runner's error", err)
	}
	if runner.closes != 2 {
		t.Fatalf("runner Close calls: got %d, want 2", runner.closes)
	}
}

// TestProviderCloseWithoutClosableRunner covers a runner that holds nothing to
// release: closing the provider is then a no-op rather than an error.
func TestProviderCloseWithoutClosableRunner(t *testing.T) {
	provider := &Provider{runner: nopRunner{}}
	if err := provider.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type nopRunner struct{ api.Runner }

func TestProviderReportsSetupUnderlayUnsupported(t *testing.T) {
	provider, err := New(testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { provider.Close() })

	if provider.Name() != "ssh" {
		t.Fatalf("provider name: got %q, want \"ssh\"", provider.Name())
	}
	if got := provider.UnsupportedCapabilities(); len(got) != 1 || got[0] != api.CapabilitySetupUnderlay {
		t.Fatalf("unsupported capabilities: got %v", got)
	}
	err = (&providerContext{}).SetupUnderlay(nil, api.Underlay{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("SetupUnderlay error: got %v, want one wrapping ErrUnsupported", err)
	}
}

// TestSupportsSetupUnderlay checks the gate the specs use, not just the list the
// provider returns: this provider is skipped, a provider that reports nothing
// unsupported is not.
func TestSupportsSetupUnderlay(t *testing.T) {
	provider, err := New(testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { provider.Close() })

	t.Cleanup(func() { infraprovider.Set(nil) })
	infraprovider.Set(provider)
	if infraprovider.SupportsSetupUnderlay() {
		t.Fatal("expected the ssh provider not to support SetupUnderlay")
	}

	infraprovider.Set(fullyCapableProvider{})
	if !infraprovider.SupportsSetupUnderlay() {
		t.Fatal("expected a provider without UnsupportedCapabilities to be treated as capable")
	}
}

// fullyCapableProvider does not list unsupported capabilities. The embedded
// interface is never called.
type fullyCapableProvider struct{ api.Provider }

func TestPreloadImagesDelegatesToConfiguredPreloader(t *testing.T) {
	cfg := testConfig(t)
	var preloaded []string
	cfg.ImagePreloader = func(images []string) error {
		preloaded = append(preloaded, images...)
		return nil
	}
	provider, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { provider.Close() })

	provider.PreloadImages([]string{"image:one", "image:two"})
	if len(preloaded) != 2 {
		t.Fatalf("preloaded images: got %v", preloaded)
	}
}

// TestPreloadImagesWithoutPreloaderIsANoOp covers the remote host case, where the
// images have to be in place before the run.
func TestPreloadImagesWithoutPreloaderIsANoOp(t *testing.T) {
	provider, err := New(testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { provider.Close() })

	provider.PreloadImages([]string{"image:one"})
}
