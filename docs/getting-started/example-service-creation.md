# Deploying Services on OVN-Kubernetes Cluster

This guide demonstrates how to create and manage various types of Kubernetes services on an OVN-Kubernetes cluster, including ClusterIP, NodePort, LoadBalancer services, and advanced service configurations.

## Prerequisites

- A running OVN-Kubernetes cluster
- `kubectl` configured to access your cluster
- Basic understanding of Kubernetes services
- Applications deployed (see [Pod Creation Guide](./example-pod-creation.md))

## Service Types Overview

OVN-Kubernetes supports all standard Kubernetes service types:

- **ClusterIP**: Internal cluster communication (default)
- **NodePort**: Exposes service on each node's IP at a static port
- **LoadBalancer**: Exposes service externally using cloud provider's load balancer
- **ExternalName**: Maps service to external DNS name

## ClusterIP Services

ClusterIP services provide internal cluster connectivity and are the foundation for service-to-service communication.

### Basic ClusterIP Service

```yaml
# clusterip-service.yaml
apiVersion: v1
kind: Service
metadata:
  name: web-service
  namespace: default
  labels:
    app: web-service
spec:
  type: ClusterIP
  selector:
    app: web-app
  ports:
  - name: http
    port: 80
    targetPort: 8080
    protocol: TCP
  - name: https
    port: 443
    targetPort: 8443
    protocol: TCP
```

Deploy and test:

```bash
# Apply the service
kubectl apply -f clusterip-service.yaml

# Check service details
kubectl get service web-service
kubectl describe service web-service

# Test service from within cluster
kubectl run test-client --image=busybox:1.35 -it --rm -- sh
# Inside the container:
wget -qO- http://web-service
wget -qO- http://web-service.default.svc.cluster.local
```

### Headless Service

Headless services allow direct access to individual pods without load balancing:

```yaml
# headless-service.yaml
apiVersion: v1
kind: Service
metadata:
  name: database-headless
  namespace: default
spec:
  type: ClusterIP
  clusterIP: None  # This makes it headless
  selector:
    app: database
  ports:
  - name: postgres
    port: 5432
    targetPort: 5432
    protocol: TCP
```

Test headless service:

```bash
# Apply the service
kubectl apply -f headless-service.yaml

# Query DNS to see individual pod IPs
kubectl run dns-test --image=busybox:1.35 -it --rm -- sh
# Inside the container:
nslookup database-headless
nslookup database-headless.default.svc.cluster.local
```

## NodePort Services

NodePort services expose applications on each cluster node's IP address at a static port.

### Basic NodePort Service

```yaml
# nodeport-service.yaml
apiVersion: v1
kind: Service
metadata:
  name: web-nodeport
  namespace: default
spec:
  type: NodePort
  selector:
    app: web-app
  ports:
  - name: http
    port: 80
    targetPort: 8080
    nodePort: 30080  # Optional: specify port (30000-32767)
    protocol: TCP
  - name: https
    port: 443
    targetPort: 8443
    nodePort: 30443
    protocol: TCP
```

Deploy and test:

```bash
# Apply the service
kubectl apply -f nodeport-service.yaml

# Get service details
kubectl get service web-nodeport

# Get node IPs
kubectl get nodes -o wide

# Test from external client (replace with actual node IP)
curl http://NODE_IP:30080
curl -k https://NODE_IP:30443

# Test from within cluster
kubectl run test-client --image=busybox:1.35 -it --rm -- sh
# Inside the container:
wget -qO- http://web-nodeport
```

### NodePort with Session Affinity

```yaml
# nodeport-session-affinity.yaml
apiVersion: v1
kind: Service
metadata:
  name: web-nodeport-sticky
  namespace: default
spec:
  type: NodePort
  selector:
    app: web-app
  sessionAffinity: ClientIP
  sessionAffinityConfig:
    clientIP:
      timeoutSeconds: 3600  # 1 hour
  ports:
  - name: http
    port: 80
    targetPort: 8080
    nodePort: 30081
    protocol: TCP
```

## LoadBalancer Services

LoadBalancer services integrate with cloud provider load balancers to provide external access.

### Basic LoadBalancer Service

```yaml
# loadbalancer-service.yaml
apiVersion: v1
kind: Service
metadata:
  name: web-loadbalancer
  namespace: default
  annotations:
    # Cloud provider specific annotations
    service.beta.kubernetes.io/aws-load-balancer-type: "nlb"
    service.beta.kubernetes.io/azure-load-balancer-resource-group: "my-rg"
spec:
  type: LoadBalancer
  selector:
    app: web-app
  ports:
  - name: http
    port: 80
    targetPort: 8080
    protocol: TCP
  - name: https
    port: 443
    targetPort: 8443
    protocol: TCP
  loadBalancerSourceRanges:
  - "10.0.0.0/8"
  - "192.168.0.0/16"
```

Deploy and test:

```bash
# Apply the service
kubectl apply -f loadbalancer-service.yaml

# Wait for external IP assignment
kubectl get service web-loadbalancer --watch

# Test external access (replace with actual external IP)
curl http://EXTERNAL_IP
curl -k https://EXTERNAL_IP
```

### LoadBalancer with ExternalTrafficPolicy

```yaml
# loadbalancer-local-traffic.yaml
apiVersion: v1
kind: Service
metadata:
  name: web-loadbalancer-local
  namespace: default
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local  # Preserves source IP
  selector:
    app: web-app
  ports:
  - name: http
    port: 80
    targetPort: 8080
    protocol: TCP
  healthCheckNodePort: 32000  # Required with Local policy
```

## Advanced Service Configurations

### Multi-Port Service

```yaml
# multi-port-service.yaml
apiVersion: v1
kind: Service
metadata:
  name: multi-port-app
  namespace: default
spec:
  type: ClusterIP
  selector:
    app: multi-port-app
  ports:
  - name: web
    port: 80
    targetPort: http
    protocol: TCP
  - name: api
    port: 8080
    targetPort: api
    protocol: TCP
  - name: metrics
    port: 9090
    targetPort: metrics
    protocol: TCP
  - name: grpc
    port: 9000
    targetPort: grpc
    protocol: TCP
```

### Service with Custom Endpoints

For services that point to external resources:

```yaml
# external-service.yaml
apiVersion: v1
kind: Service
metadata:
  name: external-database
  namespace: default
spec:
  type: ClusterIP
  ports:
  - name: postgres
    port: 5432
    targetPort: 5432
    protocol: TCP
---
apiVersion: v1
kind: Endpoints
metadata:
  name: external-database
  namespace: default
subsets:
- addresses:
  - ip: 192.168.1.100  # External database IP
  - ip: 192.168.1.101  # Backup database IP
  ports:
  - name: postgres
    port: 5432
    protocol: TCP
```

### ExternalName Service

```yaml
# externalname-service.yaml
apiVersion: v1
kind: Service
metadata:
  name: external-api
  namespace: default
spec:
  type: ExternalName
  externalName: api.example.com
  ports:
  - name: https
    port: 443
    protocol: TCP
```

## Service Discovery and DNS

### DNS Resolution Testing

```yaml
# dns-test-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: dns-test
  namespace: default
spec:
  containers:
  - name: test
    image: busybox:1.35
    command:
      - sleep
      - "3600"
```

Test DNS resolution:

```bash
# Deploy test pod
kubectl apply -f dns-test-pod.yaml

# Test various DNS formats
kubectl exec dns-test -- nslookup web-service
kubectl exec dns-test -- nslookup web-service.default
kubectl exec dns-test -- nslookup web-service.default.svc
kubectl exec dns-test -- nslookup web-service.default.svc.cluster.local

# Test cross-namespace resolution
kubectl exec dns-test -- nslookup kube-dns.kube-system.svc.cluster.local

# Test reverse DNS
kubectl exec dns-test -- nslookup $(kubectl get service web-service -o jsonpath='{.spec.clusterIP}')
```

### Service Mesh Integration

Example with Istio service mesh:

```yaml
# istio-service.yaml
apiVersion: v1
kind: Service
metadata:
  name: web-service-mesh
  namespace: default
  annotations:
    service.istio.io/canonical-name: web-app
    service.istio.io/canonical-revision: v1
spec:
  type: ClusterIP
  selector:
    app: web-app
    version: v1
  ports:
  - name: http
    port: 80
    targetPort: 8080
    protocol: TCP
```

## Service Monitoring and Observability

### Service with Prometheus Annotations

```yaml
# monitored-service.yaml
apiVersion: v1
kind: Service
metadata:
  name: monitored-app
  namespace: default
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "9090"
    prometheus.io/path: "/metrics"
spec:
  type: ClusterIP
  selector:
    app: monitored-app
  ports:
  - name: http
    port: 80
    targetPort: 8080
    protocol: TCP
  - name: metrics
    port: 9090
    targetPort: 9090
    protocol: TCP
```

### Service Monitor for Prometheus Operator

```yaml
# service-monitor.yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: web-app-metrics
  namespace: default
spec:
  selector:
    matchLabels:
      app: web-app
  endpoints:
  - port: metrics
    interval: 30s
    path: /metrics
```

## Load Balancing and Traffic Management

### Service with External Traffic Policy

```yaml
# external-traffic-policy.yaml
apiVersion: v1
kind: Service
metadata:
  name: web-external-traffic
  namespace: default
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local  # Cluster (default) or Local
  selector:
    app: web-app
  ports:
  - name: http
    port: 80
    targetPort: 8080
    protocol: TCP
```

### Service with Internal Traffic Policy

```yaml
# internal-traffic-policy.yaml
apiVersion: v1
kind: Service
metadata:
  name: web-internal-traffic
  namespace: default
spec:
  type: ClusterIP
  internalTrafficPolicy: Local  # Cluster (default) or Local
  selector:
    app: web-app
  ports:
  - name: http
    port: 80
    targetPort: 8080
    protocol: TCP
```

## Security and Network Policies

### Service with Network Policy

```yaml
# service-network-policy.yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: web-service-policy
  namespace: default
spec:
  podSelector:
    matchLabels:
      app: web-app
  policyTypes:
  - Ingress
  - Egress
  ingress:
  - from:
    - podSelector:
        matchLabels:
          app: frontend
    - namespaceSelector:
        matchLabels:
          name: api-namespace
    ports:
    - protocol: TCP
      port: 8080
  egress:
  - to:
    - podSelector:
        matchLabels:
          app: database
    ports:
    - protocol: TCP
      port: 5432
```

## Testing and Troubleshooting Services

### Service Connectivity Testing

```bash
# Create a comprehensive test script
cat > test-services.sh << 'EOF'
#!/bin/bash

echo "=== Service Connectivity Tests ==="

# Test ClusterIP service
echo "Testing ClusterIP service..."
kubectl run test-clusterip --image=busybox:1.35 --rm -it -- \
  wget -qO- http://web-service.default.svc.cluster.local

# Test NodePort service
echo "Testing NodePort service..."
NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
curl -s http://$NODE_IP:30080 || echo "NodePort test failed"

# Test DNS resolution
echo "Testing DNS resolution..."
kubectl run test-dns --image=busybox:1.35 --rm -it -- \
  nslookup web-service.default.svc.cluster.local

# Test service endpoints
echo "Testing service endpoints..."
kubectl get endpoints web-service

echo "=== Tests Complete ==="
EOF

chmod +x test-services.sh
./test-services.sh
```

### Service Debug Commands

```bash
# Check service configuration
kubectl get service web-service -o yaml

# Check service endpoints
kubectl get endpoints web-service
kubectl describe endpoints web-service

# Check service events
kubectl get events --field-selector involvedObject.name=web-service

# Check pod labels and selectors
kubectl get pods --show-labels
kubectl get pods -l app=web-app

# Test service connectivity with netshoot
kubectl run netshoot --image=nicolaka/netshoot -it --rm -- bash
# Inside container:
nslookup web-service
curl -v http://web-service
dig web-service.default.svc.cluster.local
```

### Performance Testing

```bash
# Load test a service
kubectl run load-test --image=busybox:1.35 -it --rm -- sh -c '
  for i in $(seq 1 100); do
    wget -qO- http://web-service.default.svc.cluster.local
    sleep 0.1
  done
'

# Concurrent connection test
kubectl run concurrent-test --image=alpine/curl -it --rm -- sh -c '
  for i in $(seq 1 10); do
    curl -s http://web-service.default.svc.cluster.local &
  done
  wait
'
```

## Best Practices

### Service Design

- Use meaningful service names that reflect their purpose
- Implement health checks for all service endpoints
- Use appropriate service types for different use cases
- Configure proper resource limits and requests

### Security

- Limit service exposure using Network Policies
- Use TLS for sensitive communications
- Implement proper authentication and authorization
- Regular security scanning of service endpoints

### Performance

- Use session affinity when stateful sessions are required
- Configure appropriate timeouts and retries
- Monitor service latency and throughput
- Use load balancing policies suitable for your workload

### Monitoring

- Add Prometheus metrics endpoints
- Configure proper logging for service components
- Set up alerts for service availability
- Monitor service discovery and DNS resolution

## Common Issues and Solutions

### Service Not Accessible

```bash
# Check if pods are running and have correct labels
kubectl get pods -l app=web-app
kubectl describe pods -l app=web-app

# Verify service selector matches pod labels
kubectl get service web-service -o yaml | grep -A5 selector
kubectl get pods -l app=web-app --show-labels

# Check endpoints
kubectl get endpoints web-service
```

### DNS Resolution Issues

```bash
# Test DNS from pod
kubectl exec test-pod -- nslookup kubernetes.default.svc.cluster.local

# Check CoreDNS pods
kubectl get pods -n kube-system -l k8s-app=kube-dns

# Check CoreDNS configuration
kubectl get configmap -n kube-system coredns -o yaml
```

### Load Balancer Not Getting External IP

```bash
# Check service events
kubectl describe service web-loadbalancer

# Check cloud provider integration
kubectl get nodes -o yaml | grep -A5 providerID

# Check service controller logs
kubectl logs -n kube-system -l app=cloud-controller-manager
```

## Cleanup

Remove all test services and resources:

```bash
# Delete services
kubectl delete service web-service
kubectl delete service web-nodeport
kubectl delete service web-loadbalancer
kubectl delete service database-headless
kubectl delete service external-database
kubectl delete service external-api

# Delete endpoints
kubectl delete endpoints external-database

# Delete test pods
kubectl delete pod dns-test netshoot --ignore-not-found=true

# Remove test scripts
rm -f test-services.sh
```

## Next Steps

- Explore [Network Security Controls](../features/network-security-controls/)
- Learn about [Ingress Controllers](../features/ingress/)
- Configure [Service Mesh Integration](../features/service-mesh/)
- Set up [Monitoring and Observability](../observability/)
- Implement [Multi-Network Services](../features/multiple-networks/)

For more advanced service configurations and OVN-Kubernetes specific features, see the [Features Documentation](../features/).
