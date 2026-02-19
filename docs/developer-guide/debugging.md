# Debugging

This document covers debugging techniques for ovn-kubernetes.

## Coredump Analysis

When ovn-kubernetes processes crash, coredumps and their corresponding binaries are
automatically collected in CI. This allows post-mortem debugging to investigate
what went wrong.

### How It Works

1. **Coredump collection is enabled** via `ENABLE_COREDUMPS=true` in KIND clusters.
   This sets up:
   - `kernel.core_pattern` to pipe coredumps to `/tmp/kind/logs/coredumps/`
   - `GOTRACEBACK=crash` environment variable for Go binaries (required for Go to
     generate coredumps on crashes)

2. **When a process crashes**, the kernel writes a coredump file with the pattern:
   ```
   core.<PID>.<executable>.<hostname>.<signal>
   ```

3. **Binary collection** happens during log export. The `export-kind-logs.sh` script
   searches all containers for the crashed binary and copies it alongside the coredump.

4. **Artifacts are uploaded** to GitHub Actions and can be downloaded from the job's
   artifacts section.

### Downloading Artifacts

After a CI job completes, download the `kind-logs-*` artifact from the GitHub Actions
job page. Extract it to find:

```
/tmp/kind/logs/coredumps/
├── core.29132.ovnkube.ovn-worker.6    # Coredump file
└── binaries/
    └── ovnkube                         # Matching binary
```

### Debugging with Delve

Use the [Delve](https://github.com/go-delve/delve) debugger for post-mortem analysis.

1. **Create a path substitution file** (`dlv.init`) to map build paths to your local
   source checkout:

   ```
   config substitute-path /workspace/ovn-kubernetes/go-controller /path/to/your/ovn-kubernetes/go-controller
   config substitute-path /usr/local/go /path/to/your/go/installation
   ```

   The build paths can be found by running `dlv core` without the init file and
   using the `list` command - it will show the paths it's looking for.

2. **Start the debugger**:

   ```bash
   dlv core ./binaries/ovnkube ./core.29132.ovnkube.ovn-worker.6 --init dlv.init
   ```

3. **Explore the crash**:

   ```
   (dlv) goroutines           # List all goroutines
   (dlv) goroutine <id>       # Switch to a specific goroutine
   (dlv) bt                   # Show backtrace
   (dlv) frame <n>            # Select stack frame
   (dlv) list                 # Show source code at current location
   (dlv) locals               # Show local variables
   (dlv) print <var>          # Print variable value
   ```

### Local Development

To enable coredump collection in a local KIND cluster:

```bash
ENABLE_COREDUMPS=true ./contrib/kind.sh
```

To manually export logs with coredump binaries:

```bash
./contrib/export-kind-logs.sh /path/to/output
```
