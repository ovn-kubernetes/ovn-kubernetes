# Performance tooling
```
contrib/perf
├── generate_perf_report.py         # Generates high-level report of OVNK Container CPU/Memory utilization during a workload
├── metrics.yml                     # Metrics we capture with kube-burner
├── performance-meta.yml            # Additional Metadata we append to our OpenSearch documents
└── workloads                       # Workload definition folder
    └── kubelet-density-cni.yml     # kubelet-density-cni workload for kube-burner
```

## Workloads
### kubelet-density-cni
This simple workload launches a webserver, service and a curl client within a namespace.

For our use-case we make some modifications to the base config shipped with kube-burner.

```
  - name: kubelet-density-cni
    jobIterations: 100
    qps: 10
    burst: 10
    namespacedIterations: false 
    namespace: kubelet-density-cni
    waitWhenFinished: true
    podWait: false 
    preLoadImages: true
    preLoadPeriod: 2m
    churnConfig:
      percent: 10
      cycles: 10
      mode: objects
```

We enable churn, which will delete and recreate the objects we created in the namespace. We also lower the QPS to 10/10 since 
we are testing on a kind cluster.
