# Performance tooling
```
contrib/perf
├── generate_perf_report.py         # Generates high-level report of OVNK Container CPU/Memory utilization during a workload
├── metrics.yml                     # Metrics we capture with kube-burner
├── performance-meta.yml            # Additional Metadata we append to our OpenSearch documents
└── workloads                       # Workload definition folder
    ├── kubelet-density-cni.yml             # kubelet-density-cni workload
    ├── udn-density-l2-noPods.yml           # UDN L2 density without pods
    ├── cudn-density-l2-noPods.yml          # CUDN L2 density without pods
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

### udn-density-l2-noPods / cudn-density-l2-noPods
These workloads create UDN (UserDefinedNetwork) or CUDN (ClusterUserDefinedNetwork) L2 network objects without any pods.

### udn-density-l2-pods
This workload creates UDN L2 networks along with client pods.

### cudn-density-l2-pods-netpol / udn-density-l2-pods-netpol / udn-density-l3-pods-netpol
These workloads create UDN or CUDN networks (L2 or L3) along with server deployments, client deployments, services, and network policies (deny-all + allow-from-clients).