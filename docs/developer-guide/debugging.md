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

### Debugging C Binaries with GDB (e.g. FRR)

Some coredumps come from C binaries such as FRR's `bgpd` or `zebra`, not from Go
binaries. These require GDB instead of Delve.

The key challenge is matching the exact container image that produced the coredump,
since GDB needs the same binary and shared libraries to resolve symbols.

1. **Identify the image that produced the coredump.** Check the CI job logs for the
   `docker run` command that started the crashed process. For example, the external
   FRR container may use `quay.io/frrouting/frr:9.1.0` (deployed via
   `contrib/kind-common.sh`).

2. **Run the same image with the coredumps mounted:**

   ```bash
   docker run --platform linux/amd64 -it \
     -v /path/to/coredumps:/coredumps \
     quay.io/frrouting/frr:9.1.0 sh
   ```

   Using `--platform linux/amd64` is important if the coredump was generated on
   x86_64 and you are on a different architecture (e.g. Apple Silicon).

3. **Install GDB and debug symbols inside the container:**

   ```bash
   apk add gdb frr-dbg musl-dbg
   ```

   The exact package names depend on the base distro. Alpine uses `-dbg` suffix.

4. **Run GDB:**

   ```bash
   gdb /usr/lib/frr/bgpd /coredumps/core.38907.bgpd.ovn-control-plane.11
   ```

5. **Explore the crash:**

   ```
   (gdb) bt                        # Show backtrace
   (gdb) thread apply all bt       # Backtraces for all threads
   (gdb) frame <n>                 # Select stack frame
   (gdb) info locals               # Show local variables
   (gdb) info args                 # Show function arguments
   (gdb) print *some_ptr           # Dereference and print a pointer
   (gdb) info sharedlibrary        # Check if all shared libraries are resolved
   ```

6. **Troubleshooting missing symbols.** If the backtrace shows `??` for most frames:
   - Run `info sharedlibrary` in GDB. Lines marked `(*)` are missing debug info.
   - Verify you are using the exact same image tag that produced the coredump.
     Floating tags (like `latest` or even `9.1.0`) may have been rebuilt with updated
     packages. If the shared library versions don't match (GDB will print warnings
     about missing `.so` files), you need the exact image digest from CI.
   - Install additional `-dbg` packages for libraries that appear in the backtrace.

### Local Development

To enable coredump collection in a local KIND cluster:

```bash
ENABLE_COREDUMPS=true ./contrib/kind.sh
```

To manually export logs with coredump binaries:

```bash
./contrib/export-kind-logs.sh /path/to/output
```
