# Test Hooks

This directory contains test hook YAML files that are executed after the CNI components are deployed.

## Configuration

Test hooks are configured in the NetworkOperator CR using the `testHooks` field:

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkOperator
metadata:
  name: my-operator
spec:
  # ... other configuration ...
  
  testHooks:
    enabled: true                    # Enable test hook execution
    timeout: 600                     # Global timeout in seconds (default: 600)
    tests:                           # Specify which test files to run
    - "connectivity-test.yaml"       # Run only this test
    # If 'tests' is empty, all test hooks will be executed
```

### Configuration Options

- **`enabled`** (bool): Whether test hooks should be executed. Default: `false`
- **`timeout`** (int32): Global timeout for test execution in seconds. Default: `600` (10 minutes)  
- **`tests`** ([]string): List of test hook file names to execute. If empty, all `.yaml` files in this directory will be executed.

## How it works

1. **Test Execution**: After all NetworkOperator components are deployed and validated, the operator checks if test hooks are enabled.

2. **File Selection**: The operator loads all `.yaml` files from this directory and filters them based on the `tests` configuration.

3. **Template Support**: Test hook YAML files support Go template syntax with access to the NetworkOperator object.

4. **Available Template Variables**:
   - `{{.NetworkOperator.Name}}` - Name of the NetworkOperator CR
   - `{{.NetworkOperator.Namespace}}` - Namespace of the NetworkOperator CR
   - `{{.NetworkOperator.Spec.Version}}` - Version being deployed

5. **Multi-Document YAML**: Each test hook file can contain multiple Kubernetes resources separated by `---`.

6. **Automatic Cleanup**: All test resources are automatically cleaned up after the test completes.

## Example Test Hook

```yaml
# Simple connectivity test
apiVersion: batch/v1
kind: Job
metadata:
  name: "{{.NetworkOperator.Name}}-connectivity-test"
  namespace: "{{.NetworkOperator.Namespace}}"
spec:
  template:
    spec:
      containers:
      - name: connectivity-test
        image: busybox:latest
        command: ["/bin/sh", "-c"]
        args:
        - |
          echo "Testing connectivity for {{.NetworkOperator.Name}}"
          nslookup kubernetes.default.svc.cluster.local
          echo "Test completed"
      restartPolicy: Never
```

## Test Hook Files

- `connectivity-test.yaml` - Basic network connectivity validation

## Usage Examples

### Run All Test Hooks
```yaml
testHooks:
  enabled: true
  # tests field omitted - runs all test hooks
```

### Run Specific Test Hooks
```yaml
testHooks:
  enabled: true
  tests:
  - "connectivity-test.yaml"
  - "performance-test.yaml"
```

### Disable Test Hooks
```yaml
testHooks:
  enabled: false
# OR simply omit the testHooks field entirely
```

## Adding New Tests

1. Create a new `.yaml` file in this directory
2. Define Kubernetes resources (Jobs, ServiceAccounts, etc.) 
3. Use Go template syntax to inject NetworkOperator data
4. Add the file name to the `tests` list in your NetworkOperator CR (or leave empty to run all)

## Test Execution Flow

1. Components deployed → 2. Check testHooks.enabled → 3. Filter test files → 4. Execute tests → 5. Clean up resources → 6. Update status