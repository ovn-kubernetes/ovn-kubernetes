# OVN-Kubernetes Logging

Effective logging is crucial for monitoring the health of your OVN-Kubernetes cluster, diagnosing issues, and understanding network behavior. OVN-Kubernetes components produce logs that can be configured for verbosity and output location.

## Key Components and Their Logs

OVN-Kubernetes consists of several components, each generating its own logs:

1.  **`ovnkube-master`**:
    *   Runs on master nodes (or as a deployment).
    *   Logs events related to watching Kubernetes API objects (Pods, Services, NetworkPolicies, Endpoints, Namespaces, Nodes).
    *   Logs interactions with the OVN Northbound Database (NBDB) where it writes the logical network configuration.
    *   Log file typically: `/var/log/ovn-kubernetes/ovnkube-master.log` (inside the container/pod).
    *   Controlled by `ovnkube` process flags like `--loglevel`, `--logfile`, etc.

2.  **`ovnkube-node`**:
    *   Runs as a DaemonSet on each Kubernetes node.
    *   Logs events related to CNI plugin operations (pod setup/teardown).
    *   Logs interactions with the local `ovn-controller`.
    *   Logs information about gateway setup, IP address management, and local OVS configurations.
    *   Log file typically: `/var/log/ovn-kubernetes/ovnkube-node.log` (inside the container/pod).
    *   Controlled by `ovnkube` process flags.

3.  **`ovn-controller`**:
    *   Runs on each node, managed by `ovnkube-node` (often in the same pod or as a separate process/container started by `ovnkube-node`'s init scripts).
    *   Connects to the OVN Southbound Database (SBDB).
    *   Translates logical flow information from SBDB into OpenFlow rules for the local OVS instance.
    *   Logs activities related to OVS programming and SBDB synchronization.
    *   Log file typically: `/var/log/ovn/ovn-controller.log` (or as configured for OVN).
    *   Logging is controlled via `ovn-appctl` or OVN configuration options.

4.  **OVN Databases (`ovn-northd`, `ovsdb-server` for NBDB and SBDB)**:
    *   `ovn-northd` translates logical configurations from NBDB to SBDB.
    *   `ovsdb-server` processes host the Northbound and Southbound databases.
    *   Log database transactions, connections, and internal states.
    *   Log files typically: `/var/log/ovn/ovn-northd.log`, `/var/log/openvswitch/ovsdb-server-nb.log`, `/var/log/openvswitch/ovsdb-server-sb.log` (or as configured).
    *   Logging is controlled via OVN configuration options.

5.  **`ovn-k8s-cni-overlay` (CNI Plugin)**:
    *   The binary executed by Kubelet during pod lifecycle events (ADD, DEL, CHECK).
    *   Logs operations related to setting up pod network interfaces, IPAM, etc.
    *   Log output is often configured in the CNI configuration file (e.g., `/etc/cni/net.d/10-ovn-kubernetes.conf`) via `logFile` and `logLevel` parameters.
    *   Typical log file: `/var/log/ovn-kubernetes/ovn-k8s-cni-overlay.log` on the host node, or as specified in CNI config.

## Configuring Log Levels

Log verbosity for OVN-Kubernetes components (especially `ovnkube-master` and `ovnkube-node`) is typically controlled by the `--loglevel` command-line flag or an equivalent configuration file setting.

Common log levels (may vary slightly by component):
*   `0`: Panic
*   `1`: Fatal
*   `2`: Error
*   `3`: Warning
*   `4`: Info (Default, recommended for production)
*   `5`: Debug (Very verbose, useful for troubleshooting)
*   Higher levels might be available for trace-level debugging in some components.

For OVN native daemons (`ovn-controller`, `ovn-northd`, `ovsdb-server`), logging is often controlled using `ovs-appctl` or by setting options in their respective configuration files or startup scripts. For example:

```bash
# For ovn-controller
ovs-appctl -t ovn-controller vlog/set PATTERN:MODULE:LEVEL
# Example: Set ovn-controller to debug level
ovs-appctl -t ovn-controller vlog/set dbg

# For ovn-northd
ovs-appctl -t ovn-northd vlog/set dbg
```

## Accessing Logs

### Using `kubectl logs`

For components running as pods within Kubernetes (e.g., `ovnkube-master`, `ovnkube-node`):

```bash
# Check logs for ovnkube-master pods
kubectl logs -n ovn-kubernetes -l name=ovnkube-master

# Check logs for a specific ovnkube-node pod
kubectl logs -n ovn-kubernetes <ovnkube-node-pod-name> -c ovnkube-node # specify container if needed
kubectl logs -n ovn-kubernetes <ovnkube-node-pod-name> -c ovn-controller # if ovn-controller is a container in the same pod

# Follow logs in real-time
kubectl logs -f -n ovn-kubernetes -l name=ovnkube-master
```

### Accessing Logs on Nodes

For logs written to files directly on the Kubernetes nodes (e.g., CNI plugin logs if not captured by the container runtime, or logs from OVN components if run as system services outside of Kubernetes pods in some deployment models):

*   SSH into the relevant node.
*   Navigate to the log directories (e.g., `/var/log/ovn-kubernetes/`, `/var/log/ovn/`, `/var/log/openvswitch/`).
*   Use standard Linux tools like `cat`, `tail`, `grep`, `less`.

Example:
```bash
ssh node1
sudo tail -f /var/log/ovn-kubernetes/ovn-k8s-cni-overlay.log
sudo tail -f /var/log/ovn/ovn-controller.log
```

## Log Rotation

For components writing to log files, ensure log rotation is configured to prevent disk space exhaustion.
*   **Kubernetes Pod Logs**: Kubernetes and the container runtime typically handle log rotation for pod stdout/stderr.
*   **File Logs on Nodes**: If components write directly to files on nodes, use tools like `logrotate` to manage file sizes, backups, and retention. DaemonSet configurations for `ovnkube-node` might include log rotation settings like:
    *   `OVNKUBE_LOGFILE_MAXSIZE`
    *   `OVNKUBE_LOGFILE_MAXBACKUPS`
    *   `OVNKUBE_LOGFILE_MAXAGE`

## Key Information to Look For

When troubleshooting, search logs for:

*   **Error messages**: Keywords like `ERROR`, `Failed`, `Cannot`, `Timeout`.
*   **Warnings**: Keywords like `WARN`, `Issue`.
*   **Resource IDs**: Pod names, namespace names, IP addresses, MAC addresses, OVN logical port/switch/router names.
*   **Timestamps**: Correlate events across different components.
*   **Specific operations**: CNI ADD/DEL, network policy creation/deletion, service updates.
*   **OVN DB interactions**: Connections to NBDB/SBDB, transaction IDs.
*   **Flow programming**: Messages from `ovn-controller` related to OpenFlow rule installation.

## Enabling Debug Logging Temporarily

During active troubleshooting, you might need to increase log verbosity.

*   **For `ovnkube` pods**:
    *   Edit the DaemonSet (`ovnkube-node`) or Deployment (`ovnkube-master`).
    *   Modify the `--loglevel` argument to `5`.
    *   Kubernetes will roll out the changes. Remember to revert after troubleshooting.

*   **For OVN daemons (`ovn-controller`, `ovn-northd`)**:
    *   Use `ovs-appctl vlog/set module:level` (e.g., `ovs-appctl -t ovn-controller vlog/set dbg`).
    *   This change is typically temporary and might revert on process restart unless persisted in configuration files.

Example: Increase `ovn-controller` log level on a specific node `worker1`:
```bash
# Find the ovnkube-node pod running on worker1
OVNKUBE_NODE_POD=$(kubectl get pods -n ovn-kubernetes --field-selector spec.nodeName=worker1 -l name=ovnkube-node -o jsonpath='{.items[0].metadata.name}')

# Exec into the ovnkube-node pod and run ovs-appctl for ovn-controller
kubectl exec -n ovn-kubernetes $OVNKUBE_NODE_POD -c ovn-controller -- ovs-appctl -t /var/run/ovn/ovn-controller.<pid>.ctl vlog/set dbg
# Note: The exact path to ovn-controller's ctl socket might vary. You may need to find the PID first.
# A simpler approach if ovn-controller is in its own container with its own ovs-appctl:
kubectl exec -n ovn-kubernetes $OVNKUBE_NODE_POD -c ovn-controller -- ovs-appctl -t ovn-controller vlog/set dbg
```

## Centralized Logging

For production environments, consider integrating OVN-Kubernetes logs into a centralized logging solution (e.g., ELK stack - Elasticsearch, Logstash, Kibana; or Grafana Loki). This allows for easier searching, correlation, and alerting across all components and nodes.
*   Configure your log collection agent (e.g., Fluentd, Filebeat) to collect logs from standard Kubernetes pod log paths and relevant file paths on nodes.

By effectively managing and analyzing logs, you can gain valuable insights into your OVN-Kubernetes deployment, ensuring stability and performance.
