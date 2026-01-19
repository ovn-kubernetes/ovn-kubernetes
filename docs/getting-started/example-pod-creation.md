# Deploying Workloads on OVN-Kubernetes Cluster

This guide demonstrates how to deploy various types of workloads on an OVN-Kubernetes cluster, including pods, deployments, and multi-tier applications.

## Prerequisites

- A running OVN-Kubernetes cluster
- `kubectl` configured to access your cluster
- Basic understanding of Kubernetes concepts

## Verify Cluster Setup

Before deploying workloads, verify that your OVN-Kubernetes cluster is working correctly:

```bash
# Check cluster nodes
kubectl get nodes

# Verify OVN-Kubernetes pods are running
kubectl get pods -n ovn-kubernetes

# Check CNI configuration
kubectl get pods -n kube-system | grep ovn

# Verify cluster networking
kubectl cluster-info
```

## Basic Pod Deployment

### Simple Pod

Create a basic pod to test network connectivity:

```yaml
# simple-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
  namespace: default
spec:
  containers:
  - name: test-container
    image: busybox:1.35
    command:
      - sleep
      - "3600"
    resources:
      requests:
        memory: "64Mi"
        cpu: "250m"
      limits:
        memory: "128Mi"
        cpu: "500m"
```

Deploy and test the pod:

```bash
# Create the pod
kubectl apply -f simple-pod.yaml

# Check pod status
kubectl get pod test-pod

# Get pod IP and node information
kubectl get pod test-pod -o wide

# Test network connectivity from within the pod
kubectl exec test-pod -- ping -c 3 8.8.8.8

# Check DNS resolution
kubectl exec test-pod -- nslookup kubernetes.default.svc.cluster.local
```

### Pod with Multiple Containers

```yaml
# multi-container-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: multi-container-pod
  namespace: default
spec:
  containers:
  - name: web-server
    image: nginx:1.21
    ports:
    - containerPort: 80
    volumeMounts:
    - name: shared-data
      mountPath: /usr/share/nginx/html
  - name: content-generator
    image: busybox:1.35
    command:
      - /bin/sh
      - -c
      - |
        while true; do
          echo "Hello from OVN-Kubernetes! $(date)" > /shared/index.html
          sleep 10
        done
    volumeMounts:
    - name: shared-data
      mountPath: /shared
  volumes:
  - name: shared-data
    emptyDir: {}
```

Deploy and test:

```bash
# Create the pod
kubectl apply -f multi-container-pod.yaml

# Check both containers are running
kubectl get pod multi-container-pod

# Test web server
kubectl port-forward multi-container-pod 8080:80

# In another terminal, test the web server
curl http://localhost:8080
```

## Deployment Examples

### Web Application Deployment

```yaml
# web-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web-app
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app: web-app
  template:
    metadata:
      labels:
        app: web-app
    spec:
      containers:
      - name: web
        image: nginx:1.21
        ports:
        - containerPort: 80
        resources:
          requests:
            memory: "64Mi"
            cpu: "250m"
          limits:
            memory: "128Mi"
            cpu: "500m"
        livenessProbe:
          httpGet:
            path: /
            port: 80
          initialDelaySeconds: 30
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /
            port: 80
          initialDelaySeconds: 5
          periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: web-app-service
  namespace: default
spec:
  selector:
    app: web-app
  ports:
  - protocol: TCP
    port: 80
    targetPort: 80
  type: ClusterIP
```

Deploy and test:

```bash
# Deploy the application
kubectl apply -f web-deployment.yaml

# Check deployment status
kubectl get deployment web-app
kubectl get pods -l app=web-app

# Check service
kubectl get service web-app-service

# Test service connectivity
kubectl run test-client --image=busybox:1.35 -it --rm -- sh
# Inside the container:
wget -qO- http://web-app-service
```

### Database Application

```yaml
# database-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres-db
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: postgres-db
  template:
    metadata:
      labels:
        app: postgres-db
    spec:
      containers:
      - name: postgres
        image: postgres:13
        ports:
        - containerPort: 5432
        env:
        - name: POSTGRES_DB
          value: "testdb"
        - name: POSTGRES_USER
          value: "testuser"
        - name: POSTGRES_PASSWORD
          value: "testpass"
        volumeMounts:
        - name: postgres-storage
          mountPath: /var/lib/postgresql/data
        resources:
          requests:
            memory: "256Mi"
            cpu: "500m"
          limits:
            memory: "512Mi"
            cpu: "1000m"
      volumes:
      - name: postgres-storage
        emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: postgres-service
  namespace: default
spec:
  selector:
    app: postgres-db
  ports:
  - protocol: TCP
    port: 5432
    targetPort: 5432
  type: ClusterIP
```

Deploy and test:

```bash
# Deploy PostgreSQL
kubectl apply -f database-deployment.yaml

# Check deployment
kubectl get deployment postgres-db
kubectl get pod -l app=postgres-db

# Test database connection
kubectl run postgres-client --image=postgres:13 -it --rm -- bash
# Inside the container:
PGPASSWORD=testpass psql -h postgres-service -U testuser -d testdb -c "SELECT version();"
```

## Multi-Tier Application

Deploy a complete multi-tier application with frontend, backend, and database:

### Backend API

```yaml
# backend-api.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: backend-api
  namespace: default
spec:
  replicas: 2
  selector:
    matchLabels:
      app: backend-api
      tier: backend
  template:
    metadata:
      labels:
        app: backend-api
        tier: backend
    spec:
      containers:
      - name: api
        image: python:3.9-slim
        ports:
        - containerPort: 8000
        command:
          - python
          - -c
          - |
            import http.server
            import socketserver
            import json
            from urllib.parse import urlparse
            
            class APIHandler(http.server.SimpleHTTPRequestHandler):
                def do_GET(self):
                    if self.path == '/api/health':
                        self.send_response(200)
                        self.send_header('Content-type', 'application/json')
                        self.end_headers()
                        response = {"status": "healthy", "service": "backend-api"}
                        self.wfile.write(json.dumps(response).encode())
                    else:
                        self.send_response(404)
                        self.end_headers()
            
            with socketserver.TCPServer(("", 8000), APIHandler) as httpd:
                print("Backend API server running on port 8000")
                httpd.serve_forever()
        resources:
          requests:
            memory: "128Mi"
            cpu: "250m"
          limits:
            memory: "256Mi"
            cpu: "500m"
---
apiVersion: v1
kind: Service
metadata:
  name: backend-api-service
  namespace: default
spec:
  selector:
    app: backend-api
    tier: backend
  ports:
  - protocol: TCP
    port: 8000
    targetPort: 8000
  type: ClusterIP
```

### Frontend Application

```yaml
# frontend-app.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: frontend-app
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app: frontend-app
      tier: frontend
  template:
    metadata:
      labels:
        app: frontend-app
        tier: frontend
    spec:
      containers:
      - name: frontend
        image: nginx:1.21
        ports:
        - containerPort: 80
        volumeMounts:
        - name: frontend-config
          mountPath: /etc/nginx/conf.d
        - name: frontend-content
          mountPath: /usr/share/nginx/html
        resources:
          requests:
            memory: "64Mi"
            cpu: "250m"
          limits:
            memory: "128Mi"
            cpu: "500m"
      volumes:
      - name: frontend-config
        configMap:
          name: frontend-nginx-config
      - name: frontend-content
        configMap:
          name: frontend-content
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: frontend-nginx-config
  namespace: default
data:
  default.conf: |
    upstream backend {
        server backend-api-service:8000;
    }
    
    server {
        listen 80;
        server_name localhost;
        
        location / {
            root /usr/share/nginx/html;
            index index.html;
        }
        
        location /api/ {
            proxy_pass http://backend/;
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
        }
    }
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: frontend-content
  namespace: default
data:
  index.html: |
    <!DOCTYPE html>
    <html>
    <head>
        <title>OVN-Kubernetes Demo</title>
        <style>
            body { font-family: Arial, sans-serif; margin: 40px; }
            .container { max-width: 800px; margin: 0 auto; }
            .status { padding: 20px; margin: 20px 0; border-radius: 5px; }
            .healthy { background-color: #d4edda; color: #155724; }
            .error { background-color: #f8d7da; color: #721c24; }
        </style>
    </head>
    <body>
        <div class="container">
            <h1>OVN-Kubernetes Multi-Tier Application</h1>
            <p>This application demonstrates pod-to-pod communication in OVN-Kubernetes.</p>
            
            <h2>Backend API Status</h2>
            <div id="api-status" class="status">Checking...</div>
            
            <button onclick="checkBackend()">Refresh Status</button>
            
            <script>
                async function checkBackend() {
                    const statusDiv = document.getElementById('api-status');
                    try {
                        const response = await fetch('/api/health');
                        const data = await response.json();
                        statusDiv.className = 'status healthy';
                        statusDiv.innerHTML = `✓ Backend is healthy: ${data.service}`;
                    } catch (error) {
                        statusDiv.className = 'status error';
                        statusDiv.innerHTML = `✗ Backend is unreachable: ${error.message}`;
                    }
                }
                
                // Check backend status on page load
                checkBackend();
            </script>
        </div>
    </body>
    </html>
---
apiVersion: v1
kind: Service
metadata:
  name: frontend-service
  namespace: default
spec:
  selector:
    app: frontend-app
    tier: frontend
  ports:
  - protocol: TCP
    port: 80
    targetPort: 80
  type: LoadBalancer  # or NodePort depending on your setup
```

Deploy the complete application:

```bash
# Deploy backend
kubectl apply -f backend-api.yaml

# Deploy frontend
kubectl apply -f frontend-app.yaml

# Check all deployments
kubectl get deployments
kubectl get services
kubectl get pods

# Test the application
kubectl port-forward service/frontend-service 8080:80

# In another terminal
curl http://localhost:8080
curl http://localhost:8080/api/health
```

## Testing Network Connectivity

### Pod-to-Pod Communication

```bash
# Create test pods in different nodes
kubectl run pod1 --image=busybox:1.35 --command -- sleep 3600
kubectl run pod2 --image=busybox:1.35 --command -- sleep 3600

# Get pod IPs
POD1_IP=$(kubectl get pod pod1 -o jsonpath='{.status.podIP}')
POD2_IP=$(kubectl get pod pod2 -o jsonpath='{.status.podIP}')

# Test connectivity
kubectl exec pod1 -- ping -c 3 $POD2_IP
kubectl exec pod2 -- ping -c 3 $POD1_IP
```

### Service Discovery

```bash
# Test service discovery
kubectl exec pod1 -- nslookup web-app-service
kubectl exec pod1 -- nslookup backend-api-service
kubectl exec pod1 -- nslookup frontend-service

# Test service connectivity
kubectl exec pod1 -- wget -qO- http://web-app-service
kubectl exec pod1 -- wget -qO- http://backend-api-service:8000/api/health
```

### External Connectivity

```bash
# Test external connectivity
kubectl exec pod1 -- ping -c 3 8.8.8.8
kubectl exec pod1 -- nslookup google.com
kubectl exec pod1 -- wget -qO- http://httpbin.org/ip
```

## Monitoring and Troubleshooting

### Check Pod Network Configuration

```bash
# Get detailed pod information
kubectl describe pod test-pod

# Check pod logs
kubectl logs test-pod

# Check events
kubectl get events --sort-by=.metadata.creationTimestamp
```

### Network Troubleshooting

```bash
# Use ovnkube-trace for network debugging
ovnkube-trace --src-namespace=default --src-pod=pod1 \
              --dst-namespace=default --dst-pod=pod2

# Check OVN-Kubernetes logs
kubectl logs -n ovn-kubernetes -l app=ovnkube-master
kubectl logs -n ovn-kubernetes -l app=ovnkube-node
```

### Performance Testing

```bash
# Network performance test between pods
kubectl run iperf-server --image=networkstatic/iperf3 -- iperf3 -s
kubectl run iperf-client --image=networkstatic/iperf3 -it --rm -- \
  iperf3 -c $(kubectl get pod iperf-server -o jsonpath='{.status.podIP}')
```

## Best Practices

### Resource Management

- Always set resource requests and limits
- Use appropriate CPU and memory values
- Monitor resource usage with `kubectl top`

### Health Checks

- Implement liveness and readiness probes
- Use appropriate initial delays and periods
- Test probe endpoints independently

### Security

- Use non-root containers when possible
- Implement Pod Security Standards
- Use network policies for micro-segmentation

### Networking

- Use services for pod-to-pod communication
- Prefer ClusterIP for internal services
- Use meaningful service names for discovery

## Cleanup

Remove all test resources:

```bash
# Delete deployments and services
kubectl delete -f web-deployment.yaml
kubectl delete -f database-deployment.yaml
kubectl delete -f backend-api.yaml
kubectl delete -f frontend-app.yaml

# Delete test pods
kubectl delete pod test-pod multi-container-pod pod1 pod2

# Delete other resources
kubectl delete pod iperf-server --ignore-not-found=true
```

## Next Steps

- Learn about [Service Creation](./example-service-creation.md)
- Explore [Network Security Controls](../features/network-security-controls/)
- Set up [Monitoring and Observability](../observability/)
- Configure [Multi-Network support](../features/multiple-networks/)

For more advanced networking features, see the [Features Documentation](../features/).
