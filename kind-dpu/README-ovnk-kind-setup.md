# Manual OVN-Kubernetes Kind Cluster Setup

Copy-paste commands to set up a basic kind cluster with OVN-Kubernetes networking.

## Prerequisites

```bash
pip install jinjanator[yaml]
```

## Step 1: Delete any existing cluster

```bash
kind delete cluster --name=ovn
```

## Step 2: Generate kind config

```bash
cd contrib

ovn_ip_family="" \
ovn_ha=false \
net_cidr="10.244.0.0/16" \
svc_cidr="10.96.0.0/16" \
use_local_registy=false \
dns_domain="cluster.local" \
ovn_num_master=1 \
ovn_num_worker=2 \
cluster_log_level=4 \
kind_local_registry_port=5000 \
kind_local_registry_name="kind-registry" \
jinjanate kind.yaml.j2 -o kind-ovn.yaml

cd ..
```

## Step 3: Create kind cluster

```bash
kind create cluster \
  --name ovn \
  --kubeconfig $HOME/ovn.conf \
  --image kindest/node:v1.34.0 \
  --config contrib/kind-ovn.yaml \
  --retain

export KUBECONFIG=$HOME/ovn.conf
```

## Step 4: Enable IPv6 on nodes

```bash
for node in $(kind get nodes --name ovn); do
  podman exec "$node" sysctl --ignore net.ipv6.conf.all.disable_ipv6=0
  podman exec "$node" sysctl --ignore net.ipv6.conf.all.forwarding=1
done
```

## Step 5: Build OVN image

```bash
make -C dist/images \
  IMAGE="localhost/ovn-daemonset-fedora:dev" \
  OVN_REPO="" \
  OVN_GITREF="" \
  OCI_BIN="podman" \
  fedora-image
```

## Step 6: Load image into kind

```bash
kind load docker-image localhost/ovn-daemonset-fedora:dev --name ovn
```

## Step 7: Get API URL

```bash
DNS_NAME_URL=$(kind get kubeconfig --internal --name ovn | grep server | awk '{ print $2 }')
CP_NODE=${DNS_NAME_URL#*//}
CP_NODE=${CP_NODE%:*}
NODE_IP=$(podman inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' "$CP_NODE")
API_URL=${DNS_NAME_URL/$CP_NODE/$NODE_IP}
echo "API URL: $API_URL"
```

## Step 8: Generate daemonset manifests

```bash
cd dist/images

./daemonset.sh \
  --output-directory="../yaml" \
  --image="localhost/ovn-daemonset-fedora:dev" \
  --ovnkube-image="localhost/ovn-daemonset-fedora:dev" \
  --net-cidr="10.244.0.0/16" \
  --svc-cidr="10.96.0.0/16" \
  --gateway-mode="shared" \
  --dummy-gateway-bridge="false" \
  --gateway-options="" \
  --enable-ipsec="false" \
  --hybrid-enabled="false" \
  --disable-snat-multiple-gws="false" \
  --disable-forwarding="false" \
  --ovn-encap-port="" \
  --disable-pkt-mtu-check="false" \
  --ovn-empty-lb-events="false" \
  --multicast-enabled="false" \
  --k8s-apiserver="$API_URL" \
  --ovn-master-count="1" \
  --ovn-unprivileged-mode=no \
  --master-loglevel="5" \
  --node-loglevel="5" \
  --dbchecker-loglevel="5" \
  --ovn-loglevel-northd="-vconsole:info -vfile:info" \
  --ovn-loglevel-nb="-vconsole:info -vfile:info" \
  --ovn-loglevel-sb="-vconsole:info -vfile:info" \
  --ovn-loglevel-controller="-vconsole:info" \
  --ovnkube-libovsdb-client-logfile="" \
  --enable-coredumps="false" \
  --ovnkube-config-duration-enable=true \
  --admin-network-policy-enable=true \
  --egress-ip-enable=true \
  --egress-ip-healthcheck-port="9107" \
  --egress-firewall-enable=true \
  --egress-qos-enable=true \
  --egress-service-enable=true \
  --v4-join-subnet="100.64.0.0/16" \
  --v6-join-subnet="fd98::/64" \
  --v4-masquerade-subnet="169.254.0.0/17" \
  --v6-masquerade-subnet="fd69::/112" \
  --v4-transit-subnet="100.88.0.0/16" \
  --v6-transit-subnet="fd97::/64" \
  --ex-gw-network-interface="" \
  --multi-network-enable="false" \
  --network-segmentation-enable="false" \
  --network-connect-enable="false" \
  --preconfigured-udn-addresses-enable="false" \
  --route-advertisements-enable="false" \
  --advertise-default-network="false" \
  --advertised-udn-isolation-mode="strict" \
  --ovnkube-metrics-scale-enable="false" \
  --compact-mode="false" \
  --enable-interconnect="true" \
  --enable-multi-external-gateway=true \
  --enable-ovnkube-identity="true" \
  --enable-persistent-ips=true \
  --network-qos-enable="false" \
  --mtu="1400" \
  --enable-dnsnameresolver="false" \
  --enable-observ="false"

cd ../..
```

## Step 9: Apply CRDs

```bash
cd dist/yaml

kubectl apply -f k8s.ovn.org_egressfirewalls.yaml
kubectl apply -f k8s.ovn.org_egressips.yaml
kubectl apply -f k8s.ovn.org_egressqoses.yaml
kubectl apply -f k8s.ovn.org_egressservices.yaml
kubectl apply -f k8s.ovn.org_adminpolicybasedexternalroutes.yaml
kubectl apply -f k8s.ovn.org_networkqoses.yaml
kubectl apply -f k8s.ovn.org_userdefinednetworks.yaml
kubectl apply -f k8s.ovn.org_clusteruserdefinednetworks.yaml
kubectl apply -f k8s.ovn.org_routeadvertisements.yaml

kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_adminnetworkpolicies.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_baselineadminnetworkpolicies.yaml
```

## Step 10: Apply setup and RBAC

```bash
kubectl apply -f ovn-setup.yaml
kubectl apply -f rbac-ovnkube-identity.yaml
kubectl apply -f rbac-ovnkube-cluster-manager.yaml
kubectl apply -f rbac-ovnkube-master.yaml
kubectl apply -f rbac-ovnkube-node.yaml
kubectl apply -f rbac-ovnkube-db.yaml
```

## Step 11: Label and untaint control plane

```bash
kubectl label node ovn-control-plane k8s.ovn.org/ovnkube-db=true node-role.kubernetes.io/control-plane="" --overwrite
kubectl taint node ovn-control-plane node-role.kubernetes.io/master:NoSchedule- || true
kubectl taint node ovn-control-plane node-role.kubernetes.io/control-plane:NoSchedule- || true
```

## Step 12: Apply OVS node daemonset

```bash
kubectl apply -f ovs-node.yaml
```

## Step 13: Label nodes with zone names (for interconnect)

```bash
for node in $(kind get nodes --name ovn); do
  kubectl label node "$node" k8s.ovn.org/zone-name=${node} --overwrite
done
```

## Step 14: Apply OVN-Kubernetes components

```bash
kubectl apply -f ovnkube-identity.yaml
kubectl apply -f ovnkube-control-plane.yaml
kubectl apply -f ovnkube-single-node-zone.yaml
```

## Step 15: Patch for faster rolling updates

```bash
kubectl patch ds -n ovn-kubernetes ovnkube-node --type='json' \
  -p='[{"op": "add", "path": "/spec/updateStrategy/rollingUpdate", "value": {"maxUnavailable": "100%"}}]'
```

## Step 16: Wait for rollout

```bash
kubectl rollout status daemonset -n ovn-kubernetes ovs-node --timeout=300s
kubectl rollout status daemonset -n ovn-kubernetes ovnkube-node --timeout=300s
kubectl wait pods -n ovn-kubernetes -l name=ovnkube-control-plane --for condition=Ready --timeout=300s
kubectl wait -n kube-system --for=condition=ready pods --all --timeout=300s
```

## Step 17: Verify

```bash
kubectl get pods -n ovn-kubernetes -o wide
kubectl get nodes
```

## Cleanup

```bash
kind delete cluster --name=ovn
```
