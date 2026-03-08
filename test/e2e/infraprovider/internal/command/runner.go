package command

// Runner abstracts command execution for various tools (docker, podman,
// ssh, kcli, etc.)
// Implementations execute commands with the provided arguments
// and return combined stdout/stderr.
type Runner interface {
	// Run executes a command with the given arguments
	// and returns its output, or an error on failure.
	Run(args ...string) (string, error)
}
