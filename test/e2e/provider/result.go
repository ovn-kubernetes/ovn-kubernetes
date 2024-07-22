package provider

import "fmt"

// result holds the execution result of SSH command
type result struct {
	user   string
	ip     string
	cmd    string
	stdout string
	stderr string
	code   int
}

func (r result) String() string {
	return fmt.Sprintf("User: %q, IP: %q, Command: %q, Stdout: %q, Stderr: %q, Exit code: %d",
		r.user, r.ip, r.cmd, r.stdout, r.stderr, r.code)
}

func (r result) isError() bool {
	return r.code != 0 || r.stdout != ""
}

func (r result) getStdOut() string {
	return r.stdout
}
