// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

// execOutcome is the scripted answer of the test server to one exec request.
type execOutcome struct {
	stdout   string
	stderr   string
	exitCode uint32
}

func TestSSHRunnerReturnsCombinedOutput(t *testing.T) {
	server := startTestSSHServer(t, func(string) execOutcome {
		return execOutcome{stdout: "to stdout\n", stderr: "to stderr\n"}
	})
	sshRunner := newSecureTestRunner(t, server)

	out, err := sshRunner.Run("container", "logs", "server")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "to stdout") || !strings.Contains(out, "to stderr") {
		t.Fatalf("expected stdout and stderr in output, got %q", out)
	}
}

func TestSSHRunnerReturnsStderrOnlyOutput(t *testing.T) {
	server := startTestSSHServer(t, func(string) execOutcome {
		return execOutcome{stderr: "container logs on stderr\n"}
	})
	sshRunner := newSecureTestRunner(t, server)

	out, err := sshRunner.Run("container", "logs", "server")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "container logs on stderr") {
		t.Fatalf("expected stderr in output, got %q", out)
	}
}

func TestSSHRunnerReturnsOutputOnCommandFailure(t *testing.T) {
	server := startTestSSHServer(t, func(string) execOutcome {
		return execOutcome{stderr: "no such container\n", exitCode: 1}
	})
	sshRunner := newSecureTestRunner(t, server)

	out, err := sshRunner.Run("container", "inspect", "missing")
	if err == nil {
		t.Fatal("expected an error for a failing command")
	}
	if !strings.Contains(out, "no such container") {
		t.Fatalf("expected the failure output to be returned, got %q", out)
	}
}

func TestSSHRunnerQuotesArguments(t *testing.T) {
	commands := make(chan string, 1)
	server := startTestSSHServer(t, func(cmd string) execOutcome {
		commands <- cmd
		return execOutcome{}
	})
	sshRunner := newSecureTestRunner(t, server)

	if _, err := sshRunner.Run("container", "exec", "server", "sh -c", "echo it's"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := <-commands
	want := `'container' 'exec' 'server' 'sh -c' 'echo it'\''s'`
	if got != want {
		t.Fatalf("remote command line: got %q, want %q", got, want)
	}
}

func TestSSHRunnerRejectsUnknownHostKey(t *testing.T) {
	server := startTestSSHServer(t, func(string) execOutcome { return execOutcome{} })
	otherSigner, _ := newTestKey(t)
	knownHosts := writeKnownHosts(t, server.addr, otherSigner.PublicKey())

	sshRunner := newTestRunner(t, server, SSHOptions{KnownHostsPath: knownHosts})
	_, err := sshRunner.Run("true")
	if err == nil {
		t.Fatal("expected a host key mismatch to fail")
	}
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) {
		t.Fatalf("expected a knownhosts.KeyError, got %v", err)
	}
}

func TestSSHRunnerAcceptsInsecureHostKeyOptIn(t *testing.T) {
	server := startTestSSHServer(t, func(string) execOutcome {
		return execOutcome{stdout: "ok\n"}
	})

	sshRunner := newTestRunner(t, server, SSHOptions{InsecureHostKey: true})
	out, err := sshRunner.Run("true")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestNewSSHRunnerRequiresHostKeyPolicy(t *testing.T) {
	_, keyPath := newTestKey(t)
	missing := filepath.Join(t.TempDir(), "known_hosts")

	_, err := NewSSHRunnerWithOptions("127.0.0.1", "user", "22", keyPath, SSHOptions{KnownHostsPath: missing})
	if err == nil {
		t.Fatal("expected an error when no known_hosts file is available")
	}
	if !strings.Contains(err.Error(), "host key verification required") {
		t.Fatalf("unexpected error %v", err)
	}
}

// TestDialBoundsSilentPeerHandshake covers a peer that accepts the connection
// and then never speaks: the handshake, not just the TCP connect, must be
// bounded.
func TestDialBoundsSilentPeerHandshake(t *testing.T) {
	addr := startSilentListener(t)
	signer, _ := newTestKey(t)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	dialed := make(chan error, 1)
	go func() {
		client, err := dial(ctx, addr, testClientConfig(signer))
		if client != nil {
			client.Close()
		}
		dialed <- err
	}()
	// The outer timeout is what fails if the handshake is unbounded: dial would
	// otherwise block on a version exchange that never comes.
	select {
	case err := <-dialed:
		if err == nil {
			t.Fatal("expected a handshake error from a silent peer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dial did not return, so the handshake was not bounded by the context")
	}
}

// TestDialClearsHandshakeDeadline checks that the deadline protecting the
// handshake does not outlive it: the connection stays usable once the handshake
// deadline has passed.
func TestDialClearsHandshakeDeadline(t *testing.T) {
	server := startTestSSHServer(t, func(string) execOutcome {
		return execOutcome{stdout: "still connected\n"}
	})
	signer, err := loadTestSigner(server.keyPath)
	if err != nil {
		t.Fatalf("load signer: %v", err)
	}

	handshakeDeadline := 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), handshakeDeadline)
	defer cancel()
	client, err := dial(ctx, server.addr, testClientConfig(signer))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	time.Sleep(2 * handshakeDeadline)
	res, err := runSSHCommand(client, []string{"true"})
	if err != nil {
		t.Fatalf("command after the handshake deadline elapsed: %v", err)
	}
	if !strings.Contains(res.output, "still connected") {
		t.Fatalf("unexpected output %q", res.output)
	}
}

func TestSSHRunnerCloseIsIdempotent(t *testing.T) {
	server := startTestSSHServer(t, func(string) execOutcome { return execOutcome{} })
	sshRunner := newSecureTestRunner(t, server)

	if _, err := sshRunner.Run("true"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	closer, ok := sshRunner.(interface{ Close() error })
	if !ok {
		t.Fatal("ssh runner does not implement Close")
	}
	for i := 0; i < 2; i++ {
		if err := closer.Close(); err != nil {
			t.Fatalf("Close (call %d): %v", i+1, err)
		}
	}
}

// --- helpers ---

type testSSHServer struct {
	addr    string
	hostKey ssh.PublicKey
	keyPath string
}

// startTestSSHServer runs an in-process SSH server that answers every exec
// request with handler's outcome.
func startTestSSHServer(t *testing.T, handler func(cmd string) execOutcome) testSSHServer {
	t.Helper()
	hostSigner, _ := newTestKey(t)
	_, clientKeyPath := newTestKey(t)

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	config.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveTestSSHConn(conn, config, handler)
		}
	}()
	return testSSHServer{addr: listener.Addr().String(), hostKey: hostSigner.PublicKey(), keyPath: clientKeyPath}
}

func serveTestSSHConn(conn net.Conn, config *ssh.ServerConfig, handler func(cmd string) execOutcome) {
	defer conn.Close()
	serverConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer serverConn.Close()
	go ssh.DiscardRequests(reqs)
	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			// Terminal for this channel either way; the client sees the rejection.
			_ = newChannel.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			return
		}
		go serveTestSSHSession(channel, requests, handler)
	}
}

func serveTestSSHSession(channel ssh.Channel, requests <-chan *ssh.Request, handler func(cmd string) execOutcome) {
	defer channel.Close()
	// Returning closes the channel, so the client-side assertion sees any failure
	// here as a failed command.
	for req := range requests {
		var payload struct{ Command string }
		if req.Type != "exec" || ssh.Unmarshal(req.Payload, &payload) != nil {
			if err := req.Reply(false, nil); err != nil {
				return
			}
			continue
		}
		if err := req.Reply(true, nil); err != nil {
			return
		}
		outcome := handler(payload.Command)
		if _, err := io.WriteString(channel, outcome.stdout); err != nil {
			return
		}
		if _, err := io.WriteString(channel.Stderr(), outcome.stderr); err != nil {
			return
		}
		status := struct{ Status uint32 }{outcome.exitCode}
		if _, err := channel.SendRequest("exit-status", false, ssh.Marshal(status)); err != nil {
			return
		}
		return
	}
}

// newSecureTestRunner returns a runner that verifies the test server's host key.
func newSecureTestRunner(t *testing.T, server testSSHServer) api.Runner {
	t.Helper()
	return newTestRunner(t, server, SSHOptions{KnownHostsPath: writeKnownHosts(t, server.addr, server.hostKey)})
}

func newTestRunner(t *testing.T, server testSSHServer, opts SSHOptions) api.Runner {
	t.Helper()
	host, port, err := net.SplitHostPort(server.addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	sshRunner, err := NewSSHRunnerWithOptions(host, "tester", port, server.keyPath, opts)
	if err != nil {
		t.Fatalf("NewSSHRunnerWithOptions: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := sshRunner.(interface{ Close() error }); ok {
			closer.Close()
		}
	})
	return sshRunner
}

// newTestKey returns a signer and the path of a file holding its private key.
func newTestKey(t *testing.T) (ssh.Signer, string) {
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
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return signer, path
}

func writeKnownHosts(t *testing.T, addr string, key ssh.PublicKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(knownhosts.Line([]string{addr}, key)+"\n"), 0o600); err != nil {
		t.Fatalf("write known hosts: %v", err)
	}
	return path
}

// loadTestSigner reads back a key written by newTestKey.
func loadTestSigner(path string) (ssh.Signer, error) {
	return makePrivateKeySignerFromFile(path)
}

// testClientConfig is what getSshClient builds, for tests that call dial directly.
func testClientConfig(signer ssh.Signer) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         connectTimeout,
	}
}

// startSilentListener accepts connections and never speaks, so a client waits
// for a handshake that never starts.
func startSilentListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var accepted []net.Conn
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range accepted {
			conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, conn)
			mu.Unlock()
		}
	}()
	return listener.Addr().String()
}
