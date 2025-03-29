package provider

import (
	"bytes"
	"context"
	"fmt"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/wait"
	"os"
	"time"
)

const sshKeyPathEnvKey = "SSH_KEY_PATH" //TODO: check if theres ocp env var already

// runSSHCommandViaBastion returns the stdout, stderr, and exit code from running cmd on
// host as specific user, along with any SSH-level error.
func runSSHCommand(cmd, addr string, signer ssh.Signer) (result, error) {
	result := result{}
	config := &ssh.ClientConfig{
		User:            machineUserName,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		err = wait.PollUntilContextTimeout(context.TODO(), 5*time.Second, 20*time.Second, true, func(ctx context.Context) (bool, error) {
			fmt.Printf("error dialing %s@%s: %v, retrying\n", machineUserName, addr, err)
			if client, err = ssh.Dial("tcp", addr, config); err != nil {
				return false, nil
			}
			return true, nil
		})
	}
	if err != nil {
		return result, fmt.Errorf("failed to initiate SSH connection to %s@%s: %v", machineUserName, addr, err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return result, fmt.Errorf("failed creating new session to %s@%s: %w", machineUserName, addr, err)
	}
	defer session.Close()

	// Run the command.
	code := 0
	var bout, berr bytes.Buffer
	session.Stdout, session.Stderr = &bout, &berr
	if err = session.Run(cmd); err != nil {
		// Check whether the command failed to run or didn't complete.
		if exiterr, ok := err.(*ssh.ExitError); ok {
			// If we got an ExitError and the exit code is nonzero, we'll
			// consider the SSH itself successful (just that the command run
			// errored on the host).
			if code = exiterr.ExitStatus(); code != 0 {
				err = nil
			}
		} else {
			// Some other kind of error happened (e.g. an IOError); consider the
			// SSH unsuccessful.
			err = fmt.Errorf("failed running `%s` on %s@%s: %w", cmd, machineUserName, addr, err)
		}
	}
	result.cmd = cmd
	result.code = code
	result.stdout = bout.String()
	result.stderr = berr.String()
	result.user = machineUserName
	result.ip = addr
	return result, err
}

func getSigner() (ssh.Signer, error) {
	if path := os.Getenv(sshKeyPathEnvKey); len(path) > 0 {
		return makePrivateKeySignerFromFile(path)
	}
	return nil, fmt.Errorf("environment key %q is not set or doesn't have a valid value", sshKeyPathEnvKey)
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
	return signer, err
}
