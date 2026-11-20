# Performance tooling
```
contrib/perf
├── generate_perf_report.py         # Generates high-level report of OVNK Container CPU/Memory utilization during a workload
├── metrics.yml                     # Metrics we capture with kube-burner
├── performance-meta.yml            # Additional Metadata we append to our OpenSearch documents
├── download-artifacts.py           # Download artifacts from GitHub Actions workflow runs
├── get-pr-info.py                  # Extract PR information from workflow runs
├── get-baseline-run.py             # Find the latest successful baseline workflow run
├── compare-reports.py              # Compare performance reports and generate comparison tables
├── post-pr-comment.py              # Post performance reports as comments to GitHub PRs
├── requirements.txt                # Python dependencies for CI scripts
└── workloads                       # Workload definition folder
    ├── kubelet-density-cni.yml             # kubelet-density-cni workload
    ├── udn-density-l2-pods.yml             # UDN L2 density with just one pod
    ├── cudn-density-l2-pods-netpol.yml     # CUDN L2 density with pods, services and network policies
    ├── udn-density-l2-pods-netpol.yml      # UDN L2 density with pods, services and network policies
    ├── udn-density-l3-pods-netpol.yml      # UDN L3 density with pods, services and network policies
    └── templates/udn-density/              # Shared object templates for UDN/CUDN workloads
```

## Workloads
### kubelet-density-cni
This simple workload launches a webserver, service and a curl client within a namespace.

For our use-case we make some modifications to the base config shipped with kube-burner.

```
  - name: kubelet-density-cni
    jobIterations: {{.ITERATIONS}}
    qps: 20
    burst: 20
    namespacedIterations: false
    namespace: kubelet-density-cni
    waitWhenFinished: true
    podWait: false
    preLoadImages: true
    preLoadPeriod: 5m
    churnConfig:
      percent: 10
      cycles: 10
      mode: objects
```

We enable churn, which will delete and recreate the objects we created in the namespace. We also cap the QPS at 20/20 since
we are testing on a KinD cluster.

`jobIterations` is templated: kube-burner renders the workload as a Go template, so `{{.ITERATIONS}}` resolves from the
`ITERATIONS` environment variable, which the `performance-test` workflow sets per matrix entry.

### udn-density-l2-pods
This workload creates UDN L2 networks along with client pods.

### cudn-density-l2-pods-netpol / udn-density-l2-pods-netpol / udn-density-l3-pods-netpol
These workloads create UDN or CUDN networks (L2 or L3) along with server deployments, client deployments, services, and network policies (deny-all + allow-from-clients).

## Test Reporting Locally.

Generate a GH Token :

`$ export GITHUB_TOKEN=$(gh auth token)`

List some of the recent PRs that have had the performance workload :

`$ gh run list --workflow performance-test.yml --limit 5`

Pick from the list and run the end to end script.

`$ bash test-full-workflow.sh 25505803912`
