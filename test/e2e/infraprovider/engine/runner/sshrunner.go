// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

// Bounds for establishing a connection.
const (
	// connectTimeout bounds the TCP connect of a single dial attempt.
	connectTimeout = 10 * time.Second
	// handshakeTimeout bounds the SSH handshake of a single dial attempt.
	handshakeTimeout = 15 * time.Second
	// connectDeadline bounds all dial attempts, including retries.
	connectDeadline = 20 * time.Second
	// connectRetryInterval is the delay between dial attempts.
	connectRetryInterval = 2 * time.Second
)

// SSHOptions configures host-key verification for NewSSHRunner.
type SSHOptions struct {
	KnownHostsPath  string
	InsecureHostKey bool
}

func (o SSHOptions) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if o.InsecureHostKey {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	path := o.KnownHostsPath
	if path == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, ".ssh", "known_hosts")
		}
	}
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			return knownhosts.New(path)
		}
	}
	return nil, fmt.Errorf("SSH host key verification required: set OVN_TEST_SSH_KNOWN_HOSTS or OVN_TEST_SSH_INSECURE_HOST_KEY=1")
}

// Run implements api.Runner interface to run commands over SSH.
type sshRunner struct {
	ip              string
	user            string
	port            string
	signer          ssh.Signer
	hostKeyCallback ssh.HostKeyCallback
	mu              sync.Mutex
	client          *ssh.Client
}

// NewSSHRunner connects over SSH with insecure host-key verification. It
// preserves the original four-argument API for existing callers. Prefer
// NewSSHRunnerWithOptions when host-key verification is required.
func NewSSHRunner(ip, user, port, privateKeyFilePath string) (api.Runner, error) {
	return NewSSHRunnerWithOptions(ip, user, port, privateKeyFilePath, SSHOptions{InsecureHostKey: true})
}

// NewSSHRunnerWithOptions connects over SSH using the supplied host-key policy.
func NewSSHRunnerWithOptions(ip, user, port, privateKeyFilePath string, opts SSHOptions) (api.Runner, error) {
	signer, err := makePrivateKeySignerFromFile(privateKeyFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ssh key file: %w", err)
	}
	hostKeyCallback, err := opts.hostKeyCallback()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve ssh host key policy for %s@%s: %w", user, ip, err)
	}
	return &sshRunner{
		ip:              ip,
		port:            port,
		user:            user,
		signer:          signer,
		hostKeyCallback: hostKeyCallback,
	}, nil
}

// Run executes command on the remote host. A successful command returns its
// stdout alone, because callers parse that value -- container inspection
// unmarshals JSON straight from it -- and a warning printed to stderr by an
// otherwise successful command would corrupt it. A failing command returns
// stdout and stderr together, where the explanation is as likely to be on one
// as the other and nothing is parsing the result.
func (s *sshRunner) Run(command string, args ...string) (string, error) {
	cmdParts := append([]string{command}, args...)

	sshClient, err := s.getSSHClient()
	if err != nil {
		return "", err
	}
	result, err := runSSHCommand(sshClient, cmdParts)
	if err != nil {
		return result.output(), fmt.Errorf("failed to run command for %s@%s, result: %v, err: %w",
			s.user, s.ip, result, err)
	}
	return result.stdout, nil
}

// Close drops the cached SSH connection. The runner reuses one client for every
// command, so without this the connection outlives the suite that created it.
// Clearing the field leaves the runner usable: a later Run dials again.
func (s *sshRunner) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil
	}
	err := s.client.Close()
	s.client = nil
	return err
}

// getSSHClient returns the cached SSH client to the remote IP, creating it if needed.
// If the existing connection is broken, it will be recreated.
func (s *sshRunner) getSSHClient() (*ssh.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// If we already have a client, verify it's still alive
	if s.client != nil {
		// Quick check: try to create a session
		session, err := s.client.NewSession()
		if err == nil {
			defer func() {
				err = session.Close()
				if err != nil {
					ginkgo.GinkgoLogr.Info("error closing ssh session", "error", err)
				}
			}()
			return s.client, nil
		}
		// Connection is dead, close it and create a new one
		s.client.Close()
		s.client = nil
	}

	// Create new connection
	client, err := getSshClient(s.user, net.JoinHostPort(s.ip, s.port), s.signer, s.hostKeyCallback)
	if err != nil {
		return nil, fmt.Errorf("error getting ssh proxy client: %w", err)
	}

	s.client = client
	return s.client, nil
}

// result holds the execution result of SSH command
type result struct {
	user   string
	ip     string
	cmd    string
	stdout string
	stderr string
}

// output is everything the command printed, for the failure path where the
// useful message may be on either stream.
func (r result) output() string {
	return r.stdout + r.stderr
}

func (r result) String() string {
	return fmt.Sprintf("User: %q, IP: %q, Command: %q, Stdout: %q, Stderr: %q", r.user, r.ip, r.cmd, r.stdout, r.stderr)
}

// runSSHCommand runs cmdArgs as a single quoted command line, capturing stdout
// and stderr separately, along with any command or SSH-level error.
func runSSHCommand(sshClient *ssh.Client, cmdArgs []string) (result, error) {
	res := result{user: sshClient.User(), ip: sshClient.RemoteAddr().String()}
	session, err := sshClient.NewSession()
	if err != nil {
		return res, fmt.Errorf("failed creating new ssh session with %s@%s: %w", res.user, res.ip, err)
	}
	defer session.Close()

	quotedArgs := make([]string, 0, len(cmdArgs))
	for _, arg := range cmdArgs {
		quotedArgs = append(quotedArgs, shellQuote(arg))
	}
	res.cmd = strings.Join(quotedArgs, " ")

	// Capture the streams separately so the caller can hand back stdout alone
	// when the command succeeds. Both buffers hold whatever was produced before
	// a failing command exited.
	var stdout, stderr bytes.Buffer
	session.Stdout, session.Stderr = &stdout, &stderr
	err = session.Run(res.cmd)
	res.stdout, res.stderr = stdout.String(), stderr.String()
	if err != nil {
		err = fmt.Errorf("failed to run command `%s` on %s@%s: %w", res.cmd, res.user, res.ip, err)
	}
	return res, err
}

// getSshClient dials addr, retrying transient failures until connectDeadline.
func getSshClient(user, addr string, signer ssh.Signer, hostKeyCallback ssh.HostKeyCallback) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         connectTimeout,
	}

	var client *ssh.Client
	var lastErr error
	retryErr := wait.PollUntilContextTimeout(context.Background(), connectRetryInterval, connectDeadline, true, func(ctx context.Context) (bool, error) {
		c, dialErr := dial(ctx, addr, config)
		if dialErr == nil {
			client = c
			return true, nil
		}
		lastErr = dialErr
		if isPermanentSSHDialError(dialErr) {
			return false, dialErr
		}
		ginkgo.GinkgoLogr.Info("error dialing, retrying", "user", user, "addr", addr, "error", dialErr)
		return false, nil
	})
	if retryErr != nil {
		if lastErr == nil {
			lastErr = retryErr
		}
		return nil, fmt.Errorf("failed to initiate SSH connection to %s@%s: %w", user, addr, lastErr)
	}
	return client, nil
}

// dial connects to addr and completes the SSH handshake, bounded by ctx and
// handshakeTimeout. ssh.ClientConfig.Timeout bounds the TCP connect only, so a
// peer that accepts the connection and then stays silent would otherwise block
// the handshake indefinitely.
func dial(ctx context.Context, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
	dialer := net.Dialer{Timeout: config.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}
	deadline := time.Now().Add(handshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err = conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to set the handshake deadline for %s: %w", addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh handshake with %s failed: %w", addr, err)
	}
	// Clear the handshake deadline: it would otherwise abort long running
	// commands on this connection.
	if err = conn.SetDeadline(time.Time{}); err != nil {
		sshConn.Close()
		return nil, fmt.Errorf("failed to clear the handshake deadline for %s: %w", addr, err)
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

func isPermanentSSHDialError(err error) bool {
	if err == nil {
		return false
	}
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) && keyErr != nil {
		return true
	}
	if errors.Is(err, ssh.ErrNoAuth) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unable to authenticate") {
		return true
	}
	if strings.Contains(msg, "host key") || strings.Contains(msg, "knownhosts") || strings.Contains(msg, "host key rejected") {
		return true
	}
	return false
}

func makePrivateKeySignerFromFile(key string) (ssh.Signer, error) {
	buffer, err := os.ReadFile(key)
	if err != nil {
		return nil, fmt.Errorf("error reading SSH key %s: %w", key, err)
	}
	signer, err := ssh.ParsePrivateKey(buffer)
	if err != nil {
		return nil, fmt.Errorf("error parsing SSH key: %w", err)
	}
	return signer, nil
}

// shellQuote wraps a string in single quotes and escapes existing single quotes
// so that it survives a shell evaluation as a single literal argument.
func shellQuote(s string) string {
	if len(s) == 0 {
		return "''"
	}
	// Replace ' with '\'' and wrap in ''
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
