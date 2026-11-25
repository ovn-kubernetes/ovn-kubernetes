// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/wait"
)

// Run implements api.Runner interface to run commands over SSH.
type sshRunner struct {
	ip     string
	user   string
	port   string
	signer ssh.Signer
	mu     sync.Mutex
	client *ssh.Client
}

func NewSSHRunner(ip, user, port, privateKeyFilePath string) (api.Runner, error) {
	// Parse SSH private key
	signer, err := makePrivateKeySignerFromFile(privateKeyFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ssh key file: %w", err)
	}
	return &sshRunner{
		ip:     ip,
		port:   port,
		user:   user,
		signer: signer,
	}, nil
}

func (s *sshRunner) Run(command string, args ...string) (string, error) {
	// Build the command string (e.g., "podman network inspect kind")
	var cmdParts []string
	cmdParts = append(cmdParts, command)
	cmdParts = append(cmdParts, args...)

	// Execute via SSH on the remote node
	sshClient, err := s.getSSHClient()
	if err != nil {
		return "", err
	}
	result, err := runSSHCommand(sshClient, cmdParts)
	if err != nil {
		return "", fmt.Errorf("failed to run command for %s@%s, result: %v, err: %w",
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
	client, err := getSshClient(s.user, net.JoinHostPort(s.ip, s.port), s.signer)
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

func (r result) String() string {
	return fmt.Sprintf("User: %q, IP: %q, Command: %q, Stdout: %q, Stderr: %q",
		r.user, r.ip, r.cmd, r.stdout, r.stderr)
}

// runSSHCommand returns the stdout, stderr, and exit code from running cmd on
// host as specific user, along with any SSH-level error.
func runSSHCommand(sshClient *ssh.Client, cmdArgs []string) (result, error) {
	res := result{}
	user := sshClient.User()
	addr := sshClient.RemoteAddr().String()
	session, err := sshClient.NewSession()
	if err != nil {
		return res, fmt.Errorf("failed creating new ssh session with %s@%s: %w", user, addr, err)
	}
	defer session.Close()

	var quotedArgs []string
	for _, arg := range cmdArgs {
		quotedArgs = append(quotedArgs, shellQuote(arg))
	}
	cmd := strings.Join(quotedArgs, " ")

	// Run the command.
	var bout, berr bytes.Buffer
	session.Stdout, session.Stderr = &bout, &berr
	if err = session.Run(cmd); err != nil {
		err = fmt.Errorf("failed to run command `%s` on %s@%s: %w", cmd, user, addr, err)
	}
	res.cmd = cmd
	res.stdout = bout.String()
	res.stderr = berr.String()
	res.user = user
	res.ip = addr
	return res, err
}

func getSshClient(user, addr string, signer ssh.Signer) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// NOTE: InsecureIgnoreHostKey is used here because this is a test environment
		// where we're connecting to ephemeral VMs. In production, use proper host key verification.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		retryErr := wait.PollUntilContextTimeout(context.TODO(), 5*time.Second, 20*time.Second, true, func(ctx context.Context) (bool, error) {
			var dialErr error
			client, dialErr = ssh.Dial("tcp", addr, config)
			if dialErr != nil {
				ginkgo.GinkgoLogr.Info("error dialing, retrying", "user", user, "addr", addr, "error", dialErr)
				return false, nil
			}
			return true, nil
		})
		if retryErr != nil {
			return nil, fmt.Errorf("failed to initiate SSH connection to %s@%s: %v", user, addr, retryErr)
		}
	}
	return client, nil
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
