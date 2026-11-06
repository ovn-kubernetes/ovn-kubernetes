# BGP Performance Lanes

The `performance-test.yml` workflow runs a ladder of kube-burner lanes that
measure the control-plane cost of BGP route advertisements. They run on every
pull request and on the nightly schedule. This page describes what each lane
measures, what it needs, how to run one locally, and what it leaves behind.

## The ladder

Each RouteAdvertisements makes the cluster manager generate one
FRRConfiguration per node per matching source FRRConfiguration, so the
generated object count grows as RAs x nodes. Adjacent lanes differ in exactly
one variable, so the useful numbers are ratios between neighbours rather than
absolutes.

| Lane | Nodes | RAs | Generated objects | Isolates |
|---|---|---|---|---|
| `bgp-ra-density` | 6 | 20 | 120 | baseline |
| `bgp-ra-density-wide` | 12 | 20 | 240 | node count, against density |
| `bgp-ra-density-deep` | 12 | 40 | 480 | RA count, against wide |
| `bgp-ra-churn` | 12 | 40 | 480 | churn: a tenth of the RAs deleted and recreated five times, against deep |
| `bgp-ra-density-threads` | 12 | 40 | 480 | reconcile worker count: four workers instead of one, against deep |

Node count is `num-workers` plus `KIND_NUM_INFRA` (2) plus the control plane.
Each lane is its own file under `contrib/perf/workloads/`, because the
artifact name comes from the matrix value while the indexed documents key on
the job name inside the file. Sharing a file would make two lanes
indistinguishable once their metrics reach the datastore.

The workloads create Layer3 ClusterUserDefinedNetworks, each advertised by its
own RouteAdvertisements with an empty `nodeSelector`, `PodNetwork` and no
`targetVRF`, selecting the `receive-all` FRRConfiguration the kind harness
installs. No pods are created: the pod path is covered by the `udn-density`
lanes, and leaving it out keeps these lanes focused on the controller.

`bgp-ra-density-threads` is `bgp-ra-density-deep` with four RouteAdvertisements
reconcile workers instead of one. The workload files are identical apart from
the job names; the difference is the matrix entry setting `ra-threadiness`,
which `OVN_RA_THREADINESS` turns into
`--cluster-manager-route-advertisements-threadiness`. Every other lane leaves
it unset and keeps the binary default of 1. Diffing the two lanes isolates the
worker count, which tests whether the queue wait the ladder measures is
serialisation behind a single worker.

## Prerequisites

- A kind cluster created with route advertisements (`kind.sh -rae`). That
  installs frr-k8s, the external FRR container and the `receive-all`
  FRRConfiguration. Without it every RouteAdvertisements is Accepted but
  generates nothing.
- `--metrics-enable-scale` on ovnkube (`OVN_METRICS_SCALE_ENABLE=true` in the
  workflow, `kind.sh -sm` locally). The workqueue, OVSDB transaction,
  RouteAdvertisements and route import metric families are not registered
  without it.
- Metrics bound to `0.0.0.0` (`METRICS_IP` in the workflow, `kind.sh -mip
  0.0.0.0` locally). Prometheus scrapes the pod IP, and the default loopback
  bind makes every `ovnkube_*`, `ovn_*` and `ovs_*` query return nothing.
- Prometheus installed by `contrib/install-prometheus-infra.sh`, which loads
  `contrib/prometheus-ovnk-scrape.yaml` as an additional scrape config. The
  ovnkube pods are host-network with no Service and declare no container
  port, so neither a ServiceMonitor nor a PodMonitor can discover them.
- kube-burner v2.5.0, the version the workflows pin.
- `jq`, and `docker` access to the FRR container for the peer summary.

## Running a lane locally

`contrib/perf/run-local-lane.sh` runs a lane the way the CI lane runs it, in
independent stages, so the cluster is paid for once while the workload is
re-run in seconds:

```bash
cd contrib/perf
./run-local-lane.sh install    # pinned kube-burner into ~/.local/bin
./run-local-lane.sh cluster    # kind.sh -rae -sm -mip 0.0.0.0 ...
./run-local-lane.sh prom       # Prometheus plus a port-forward on :9090
./run-local-lane.sh workload   # kube-burner init with metric-endpoint-local.yml
./run-local-lane.sh check      # documents indexed per query
```

`WORKLOAD` selects the lane (default `bgp-ra-density`) and `NUM_WORKERS` the
kind worker count (default 3, two fewer than CI, which adds `KIND_NUM_INFRA`).
Node count changes the generated object count, so keep it in mind when
comparing numbers against a CI run.

The `check` stage exists because a query whose metric name is wrong does not
fail: Prometheus indexes zero documents for it. The stage prints the document
count per query in `metrics-ovnk.yml`, so a rename shows up as `EMPTY` rather
than as a missing file nobody looks for.

## Artifacts

Each lane uploads three artifacts.

| Artifact | Contents |
|---|---|
| `<lane>-performance-test-data-<run-id>` | Everything kube-burner wrote. `perf/metrics/<metricName>.json` holds one file per query in `metrics.yml` and `metrics-ovnk.yml`. `perf/bgp-state-<lane>.json` is the hook snapshot described below. |
| `<lane>-performance-report-<run-id>` | `performance_report.md`, with CPU and memory from cAdvisor, generated by `generate_perf_report.py`. |
| `<lane>-pprof-<run-id>` | Profiles of `ovnkube-control-plane`, collected every 30s. |

Prometheus figures are indexed by the queries in `contrib/perf/metrics-ovnk.yml`.
An empty query file is either a genuinely idle subsystem or a metric name that
no longer resolves. Prometheus does not distinguish them, so check the name
against the registered metric before reading an empty file as zero.

## Generating a report

`contrib/perf/analyze-bgp-lanes.py` turns the artifacts of one run into a
markdown report: convergence per lane, shape and outcome, workqueue figures,
reconcile work by operation, the scaling fit across the single-worker lanes,
and one row per adjacent pair isolating a variable. A quantile sitting on the
last finite histogram bucket is reported as saturated rather than as a value,
and snapshots that had not converged are excluded from the derived figures.

```bash
for lane in bgp-ra-density bgp-ra-density-wide bgp-ra-density-deep bgp-ra-churn bgp-ra-density-threads; do
  gh run download <run-id> -n "${lane}-performance-test-data-<run-id>" -D "${lane}"
done
contrib/perf/analyze-bgp-lanes.py . --run-id <run-id> -o report.md
```

### The hook snapshot

`contrib/perf/collect-bgp-state.sh` runs as the `beforeCleanup` hook of the
RouteAdvertisements job and records what cannot be a Prometheus series:
generated FRRConfiguration count and bytes, `FRRNodeState` sizes against the
1.5 MB etcd object limit, `BGPSessionState` counts by status, peer and VRF,
the neighbours each source FRRConfiguration asks for, and the peer's prefix
totals per address family from `show bgp summary`.

kube-burner runs `beforeCleanup` straight after object creation, before the
fabric has converged, so the hook first waits. It polls until the generated
object count and the total `runningConfig` size hold still for two polls and
the per-node `runningConfig` sizes agree to within `BGP_STATE_SETTLE_SPREAD`
permille of the mean (default 20). Every node in these lanes carries every
advertised network, so at convergence the sizes must agree. While frr-k8s is
still reloading nodes in waves they do not, even though the total can hold
still between waves.

The snapshot records `settle.converged`, `settle.waitedSeconds` and
`settle.nodeSpreadPermille`. A reading with `converged: false` was taken
mid-reload and understates the true sizes. A workload whose nodes
legitimately differ, such as one with a populated `nodeSelector`, will not
converge by this test and needs the threshold raised.

The hook never fails the run. A collector that cannot run reports `null` for
its field. It is POSIX sh, because kube-burner does not execute the file but
pipes it to `/bin/sh -s -`, which is dash on the CI runners.

| Variable | Default | Meaning |
|---|---|---|
| `BGP_STATE_DIR` | `.` | Output directory for `bgp-state-<label>.json`. |
| `BGP_STATE_SETTLE_TIMEOUT` | `300` | Seconds to wait for convergence before snapshotting anyway. |
| `BGP_STATE_SETTLE_INTERVAL` | `5` | Seconds between polls. |
| `BGP_STATE_SETTLE_SPREAD` | `20` | Largest per-node spread, in permille of the mean, counted as converged. |
| `BGP_STATE_REQUEST_TIMEOUT` | `30s` | Timeout for each kubectl request, so a stalled apiserver cannot hold the hook past the settle timeout. |
| `FRR_CONTAINER_NAME` | `frr` | The external FRR container queried for the peer summary. |
