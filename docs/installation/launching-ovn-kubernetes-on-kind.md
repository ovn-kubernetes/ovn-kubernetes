# OVN-Kubernetes KIND Setup

KIND (Kubernetes in Docker) provides a fast and simple way to install and test Kubernetes with the OVN-Kubernetes CNI.
This setup is ideal for developers who need to quickly reproduce issues or validate fixes in a local environment that
can be spun up in minutes.

## Prerequisites

### System Requirements
- 20 GB of free space in root file system
- Docker or Podman installed

### Required packages
- [kubectl]( https://kubernetes.io/docs/tasks/tools/install-kubectl/ )
- python
- pip
- golang
- jq
- openssl
- openvswitch
- [KIND]( https://kind.sigs.k8s.io/docs/user/quick-start/ )
   - Install it using the official [installation guide](https://kind.sigs.k8s.io/docs/user/quick-start/#installation).
   - NOTE: The OVN-Kubernetes [ovn-kubernetes/contrib/kind.sh](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/contrib/kind.sh)
  and [ovn-kubernetes/contrib/kind.yaml.j2](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/contrib/kind.yaml.j2)
  files provision port 11337. If firewalld is enabled, this port will need to be unblocked:

      ```
      sudo firewall-cmd --permanent --add-port=11337/tcp
      sudo firewall-cmd --reload
      ```

First download and build the OVN-Kubernetes repo:

```
$ git clone https://github.com/ovn-kubernetes/ovn-kubernetes.git
$ cd ovn-kubernetes
```
The `kind.sh` script builds OVN-Kubernetes into a container image. To verify
local changes before building in KIND, run the following:

```
$ pushd go-controller && make && popd
```
---
## KIND OVN-Kubernetes deployments:
- [Run KIND deployment with Docker](#run-the-kind-deployment-with-docker)
- [Run KIND deployment with Podman](#run-the-kind-deployment-with-podman)
- [Usage Notes](#usage-notes-)
- [Troubleshooting](#troubleshooting)

## KIND OVN-Kubernetes advanced deployments:
- [Run KIND deployment with IPv6](#run-kind-deployment-with-ipv6)
- [Run KIND deployment with IP Dual-stack](#run-kind-deployment-with-dual-stack)
- [Troubleshooting for IPv6](#troubleshooting-for-ipv6)

## KIND OVN-Kubernetes custom deployments:
- [Use specific Kind container image and tag](#use-specific-kind-container-image-and-tag)
- [Use local Kind registry to deploy non ovn-k containers](#use-local-kind-registry-to-deploy-non-ovn-k-containers)
- [Load ovn-kubernetes changes without restarting kind](#load-ovn-kubernetes-changes-without-restarting-kind)
- [Known issues](#known-issues)

---

## Run the KIND deployment with Docker
Build the image for fedora and launch the KIND Deployment

```
$ pushd dist/images && make fedora-image && popd
$ pushd contrib && ./kind.sh && popd
```
---

## Run the KIND deployment with Podman
To verify local changes, the steps are mostly the same as with docker, except the `fedora` make target.
Set container runtime as 'podman':
```
$ OCI_BIN=podman
```
```
$ pushd dist/images && make fedora-image && popd
```

Since KIND needs root access, follow these steps to run it and set up your kubeconfig for non-root use:
```
$ pushd contrib && ./kind.sh -ep podman && popd
$ sudo cp /root/ovn.conf ~/.kube/kind-config
$ sudo chown $(id -u):$(id -g) ~/.kube/kind-config
$ export KUBECONFIG=~/.kube/kind-config
```

This will launch a KIND deployment. By default, the cluster is named `ovn`.

```
$ kubectl get nodes
NAME                STATUS   ROLES           AGE   VERSION
ovn-control-plane   Ready    control-plane   50m   v1.31.1
ovn-worker          Ready    <none>          50m   v1.31.1
ovn-worker2         Ready    <none>          50m   v1.31.1

$ kubectl get pods --all-namespaces
NAMESPACE            NAME                                        READY   STATUS    RESTARTS   AGE
kube-system          coredns-7c65d6cfc9-55c8d                    1/1     Running   0          50m
kube-system          coredns-7c65d6cfc9-psnlc                    1/1     Running   0          50m
kube-system          etcd-ovn-control-plane                      1/1     Running   0          50m
kube-system          kube-apiserver-ovn-control-plane            1/1     Running   0          50m
kube-system          kube-controller-manager-ovn-control-plane   1/1     Running   0          50m
kube-system          kube-scheduler-ovn-control-plane            1/1     Running   0          50m
local-path-storage   local-path-provisioner-57c5987fd4-wsbg6     1/1     Running   0          50m
ovn-kubernetes       ovnkube-db-6895c8ff84-l7d9n                 2/2     Running   0          49m
ovn-kubernetes       ovnkube-identity-7dcdcc46d4-vn8lc           1/1     Running   0          49m
ovn-kubernetes       ovnkube-master-5b4bcb4bdf-zbdd9             2/2     Running   0          49m
ovn-kubernetes       ovnkube-node-qrxj4                          3/3     Running   0          49m
ovn-kubernetes       ovnkube-node-sdvmf                          3/3     Running   0          49m
ovn-kubernetes       ovnkube-node-wt46w                          3/3     Running   0          49m
ovn-kubernetes       ovs-node-7r6k9                              1/1     Running   0          49m
ovn-kubernetes       ovs-node-qntlb                              1/1     Running   0          49m
ovn-kubernetes       ovs-node-vvcjq                              1/1     Running   0          49m
```
---

## Usage Notes

- You can create your own KIND J2 configuration file if the default one is not sufficient
- You can also specify these values as environment variables. Command line parameters will override the environment variables.
- To tear down the KIND cluster when finished simply run

```
$  pushd contrib && ./kind.sh --delete && popd
```

The `kind.sh` script defaults the cluster to HA disabled. There are numerous
configuration options when deploying. Use `./kind.sh -h` to see the latest options.

```
# ./kind.sh --help
usage: kind.sh [[[-cf |--config-file <file>] [-kt|--keep-taint] [-ha|--ha-enabled]
                 [-ho |--hybrid-enabled] [-ii|--install-ingress] [-n4|--no-ipv4]
                 [-i6 |--ipv6] [-wk|--num-workers <num>] [-ds|--disable-snat-multiple-gws]
                 [-dp |--disable-pkt-mtu-check]
                 [-df |--disable-forwarding]
                 [-ecp | --encap-port |
                 [-pl]|--install-cni-plugins ]
                 [-nf |--netflow-targets <targets>] [sf|--sflow-targets <targets>]
                 [-if |--ipfix-targets <targets>]  [-ifs|--ipfix-sampling <num>]
                 [-ifm|--ipfix-cache-max-flows <num>] [-ifa|--ipfix-cache-active-timeout <num>]
                 [-sw |--allow-system-writes] [-gm|--gateway-mode <mode>]
                 [-nl |--node-loglevel <num>] [-ml|--master-loglevel <num>]
                 [-dbl|--dbchecker-loglevel <num>] [-ndl|--ovn-loglevel-northd <loglevel>]
                 [-nbl|--ovn-loglevel-nb <loglevel>] [-sbl|--ovn-loglevel-sb <loglevel>]
                 [-cl |--ovn-loglevel-controller <loglevel>] [-me|--multicast-enabled]
                 [-lcl|--libovsdb-client-logfile <logfile>]
                 [-ep |--experimental-provider <name>] |
                 [-eb |--egress-gw-separate-bridge] |
                 [-lr |--local-kind-registry |
                 [-dd |--dns-domain |
                 [-ric | --run-in-container |
                 [-cn | --cluster-name |
                 [-ehp|--egress-ip-healthcheck-port <num>]
                 [-is | --ipsec]
                 [-cm | --compact-mode]
                 [-ic | --enable-interconnect]
                 [-rae | --enable-route-advertisements]
                 [--isolated]
                 [-dns | --enable-dnsnameresolver]
                 [-obs | --observability]
                 [-h]]

-cf  | --config-file                  Name of the KIND J2 configuration file.
                                      DEFAULT: ./kind.yaml.j2
-kt  | --keep-taint                   Do not remove taint components.
                                      DEFAULT: Remove taint components.
-ha  | --ha-enabled                   Enable high availability. DEFAULT: HA Disabled.
-scm | --separate-cluster-manager     Separate cluster manager from ovnkube-master and run as a separate container within ovnkube-master deployment.
-me  | --multicast-enabled            Enable multicast. DEFAULT: Disabled.
-ho  | --hybrid-enabled               Enable hybrid overlay. DEFAULT: Disabled.
-ds  | --disable-snat-multiple-gws    Disable SNAT for multiple gws. DEFAULT: Disabled.
-dp  | --disable-pkt-mtu-check        Disable checking packet size greater than MTU. Default: Disabled
-df  | --disable-forwarding           Disable forwarding on OVNK managed interfaces. Default: Disabled
-ecp | --encap-port                   UDP port used for geneve overlay. DEFAULT: 6081
-pl  | --install-cni-plugins ]        Installs additional CNI network plugins. DEFAULT: Disabled
-nf  | --netflow-targets              Comma delimited list of ip:port or :port (using node IP) netflow collectors. DEFAULT: Disabled.
-sf  | --sflow-targets                Comma delimited list of ip:port or :port (using node IP) sflow collectors. DEFAULT: Disabled.
-if  | --ipfix-targets                Comma delimited list of ip:port or :port (using node IP) ipfix collectors. DEFAULT: Disabled.
-ifs | --ipfix-sampling               Fraction of packets that are sampled and sent to each target collector: 1 packet out of every <num>. DEFAULT: 400 (1 out of 400 packets).
-ifm | --ipfix-cache-max-flows        Maximum number of IPFIX flow records that can be cached at a time. If 0, caching is disabled. DEFAULT: Disabled.
-ifa | --ipfix-cache-active-timeout   Maximum period in seconds for which an IPFIX flow record is cached and aggregated before being sent. If 0, caching is disabled. DEFAULT: 60.
-el  | --ovn-empty-lb-events          Enable empty-lb-events generation for LB without backends. DEFAULT: Disabled
-ii  | --install-ingress              Flag to install Ingress Components.
                                      DEFAULT: Don't install ingress components.
-mlb | --install-metallb              Install metallb to test service type LoadBalancer deployments
-n4  | --no-ipv4                      Disable IPv4. DEFAULT: IPv4 Enabled.
-i6  | --ipv6                         Enable IPv6. DEFAULT: IPv6 Disabled.
-wk  | --num-workers                  Number of worker nodes. DEFAULT: HA - 2 worker
                                      nodes and no HA - 0 worker nodes.
-sw  | --allow-system-writes          Allow script to update system. Intended to allow
                                      github CI to be updated with IPv6 settings.
                                      DEFAULT: Don't allow.
-gm  | --gateway-mode                 Enable 'shared' or 'local' gateway mode.
                                      DEFAULT: shared.
-ov  | --ovn-image            	      Use the specified docker image instead of building locally. DEFAULT: local build.
-ovr  | --ovn-repo                    Specify the repository to build OVN from
-ovg  | --ovn-gitref                  Specify the branch, tag or commit id to build OVN from, it can be a pattern like 'branch-*' it will order results and use the first one
-ml  | --master-loglevel              Log level for ovnkube (master), DEFAULT: 5.
-nl  | --node-loglevel                Log level for ovnkube (node), DEFAULT: 5
-dbl | --dbchecker-loglevel           Log level for ovn-dbchecker (ovnkube-db), DEFAULT: 5.
-ndl | --ovn-loglevel-northd          Log config for ovn northd, DEFAULT: '-vconsole:info -vfile:info'.
-nbl | --ovn-loglevel-nb              Log config for northbound DB DEFAULT: '-vconsole:info -vfile:info'.
-sbl | --ovn-loglevel-sb              Log config for southboudn DB DEFAULT: '-vconsole:info -vfile:info'.
-cl  | --ovn-loglevel-controller      Log config for ovn-controller DEFAULT: '-vconsole:info'.
-lcl | --libovsdb-client-logfile      Separate logs for libovsdb client into provided file. DEFAULT: do not separate.
-ep  | --experimental-provider        Use an experimental OCI provider such as podman, instead of docker. DEFAULT: Disabled.
-eb  | --egress-gw-separate-bridge    The external gateway traffic uses a separate bridge.
-lr  | --local-kind-registry          Configure kind to use a local docker registry rather than manually loading images
-dd  | --dns-domain                   Configure a custom dnsDomain for k8s services, Defaults to 'cluster.local'
-cn  | --cluster-name                 Configure the kind cluster's name
-ric | --run-in-container             Configure the script to be run from a docker container, allowing it to still communicate with the kind controlplane
-ehp | --egress-ip-healthcheck-port   TCP port used for gRPC session by egress IP node check. DEFAULT: 9107 (Use 0 for legacy dial to port 9).
-is  | --ipsec                        Enable IPsec encryption (spawns ovn-ipsec pods)
-sm  | --scale-metrics                Enable scale metrics
-cm  | --compact-mode                 Enable compact mode, ovnkube master and node run in the same process.
-ic  | --enable-interconnect          Enable interconnect with each node as a zone (only valid if OVN_HA is false)
--disable-ovnkube-identity            Disable per-node cert and ovnkube-identity webhook
-npz | --nodes-per-zone               If interconnect is enabled, number of nodes per zone (Default 1). If this value > 1, then (total k8s nodes (workers + 1) / num of nodes per zone) should be zero.
-mtu                                  Define the overlay mtu
--isolated                            Deploy with an isolated environment (no default gateway)
--delete                              Delete current cluster
--deploy                              Deploy ovn kubernetes without restarting kind
--add-nodes                           Adds nodes to an existing cluster. The number of nodes to be added is specified by --num-workers. Also use -ic if the cluster is using interconnect.
-dns | --enable-dnsnameresolver       Enable DNSNameResolver for resolving the DNS names used in the DNS rules of EgressFirewall.
-obs | --observability                Enable OVN Observability feature.
-rae | --enable-route-advertisements  Enable route advertisements
```

As seen above, if you do not specify any options the script will assume the default values.

---
## Troubleshooting

- Issue with /dev/dma_heap: if you get the error `kind "Error: open /dev/dma_heap: permission denied"`,
there's a [known issue](https://bugzilla.redhat.com/show_bug.cgi?id=1966158) about it (directory mislabelled with selinux).

Workaround:
```
$ sudo setenforce 0
$ sudo chcon system\_u:object\_r:device\_t:s0 /dev/dma\_heap/
$ sudo setenforce 1
```

- If you see errors related to go, you may not have go `$PATH` configured as root. Make sure it is configured, or define it while running `kind.sh`:

```
$ sudo PATH=$PATH:/usr/local/go/bin ./kind.sh -ep podman
```

In certain operating systems such as CentOS 8.x, pip2 and pip3 binaries are installed instead of pip.
In such situations, create a soft link for pip that points to pip2.<br>
For OVN kubernetes KIND deployment, use the `kind.sh` script.

---
## Running OVN-Kubernetes with IPv6 or Dual-stack In KIND

This section describes the configuration needed for IPv6 and dual-stack environments.

## Run KIND deployment with IPv6
## IPv6 pre-requisites
### Disable firewalld

Currently, to run OVN-Kubernetes with IPv6 only in a KIND deployment, firewalld
needs to be disabled. To disable:
```
sudo systemctl stop firewalld
```

### OVN-Kubernetes With IPv6

To run OVN-Kubernetes with IPv6 in a KIND deployment, run:
```
$ go get github.com/ovn-kubernetes/ovn-kubernetes
$ pushd $GOPATH/src/github.com/ovn-kubernetes/ovn-kubernetes

$ pushd go-controller && make && popd
$ pushd dist/images && make fedora-image && popd
$ pushd contrib && KIND_IPV4_SUPPORT=false KIND_IPV6_SUPPORT=true ./kind.sh && popd
$ popd
```

Once `kind.sh` completes, setup kube config file:
```
$ sudo cp /root/ovn.conf ~/.kube/kind-config
$ sudo chown $(id -u):$(id -g) ~/.kube/kind-config
$ export KUBECONFIG=~/.kube/kind-config
```

Once testing is complete, to tear down the KIND deployment:
```
$ kind delete cluster --name ovn
```
---
## Troubleshooting For IPv6

For KIND clusters using KIND v0.7.0 or older (CI currently is using v0.8.1), to
use IPv6, IPv6 needs to be enable in Docker on the host:

```
$ sudo bash -c 'cat > /etc/docker/daemon.json <<EOF
{
  "ipv6": true,
}
EOF' && sudo systemctl reload docker
```

On a CentOS host running Docker version 19.03.6, the above configuration worked.
After the host was rebooted, Docker failed to start. To fix, change
`daemon.json` as follows:

```
$ sudo bash -c 'cat > /etc/docker/daemon.json <<EOF
{
  "ipv6": true,
  "fixed-cidr-v6": "2001:db8:1::/64"
}
EOF' && sudo systemctl reload docker
```

[IPv6](https://github.com/docker/docker.github.io/blob/c0eb65aabe4de94d56bbc20249179f626df5e8c3/engine/userguide/networking/default_network/ipv6.md)
from Docker repo provided the fix. Newer documentation does not include this
change, so change may be dependent on Docker version.

- To verify IPv6 is enabled in Docker, run:

```
$ docker run --rm busybox ip a
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host
       valid_lft forever preferred_lft forever
341: eth0@if342: <BROADCAST,MULTICAST,UP,LOWER_UP,M-DOWN> mtu 1500 qdisc noqueue
    link/ether 02:42:ac:11:00:02 brd ff:ff:ff:ff:ff:ff
    inet 172.17.0.2/16 brd 172.17.255.255 scope global eth0
       valid_lft forever preferred_lft forever
    inet6 2001:db8:1::242:ac11:2/64 scope global flags 02
       valid_lft forever preferred_lft forever
    inet6 fe80::42:acff:fe11:2/64 scope link tentative
       valid_lft forever preferred_lft forever
```

For the eth0 vEth-pair, there should be the two IPv6 entries (global and link
addresses).

If firewalld is enabled during a IPv6 deployment, additional nodes fail to join the cluster:
```
:
Creating cluster "ovn" ...
 ✓ Ensuring node image (kindest/node:v1.18.2) 🖼
 ✓ Preparing nodes 📦 📦 📦
 ✓ Writing configuration 📜
 ✓ Starting control-plane 🕹️
 ✓ Installing StorageClass 💾
 ✗ Joining worker nodes 🚜
ERROR: failed to create cluster: failed to join node with kubeadm: command "docker exec --privileged ovn-worker kubeadm join --config /kind/kubeadm.conf --ignore-preflight-errors=all --v=6" failed with error: exit status 1
```

And logs show:
```
I0430 16:40:44.590181     579 token.go:215] [discovery] Failed to request cluster-info, will try again: Get https://[2001:db8:1::242:ac11:3]:6443/api/v1/namespaces/kube-public/configmaps/cluster-info?timeout=10s: dial tcp [2001:db8:1::242:ac11:3]:6443: connect: permission denied
Get https://[2001:db8:1::242:ac11:3]:6443/api/v1/namespaces/kube-public/configmaps/cluster-info?timeout=10s: dial tcp [2001:db8:1::242:ac11:3]:6443: connect: permission denied
```

This issue was reported upstream in KIND
[1257](https://github.com/kubernetes-sigs/kind/issues/1257#issuecomment-575984987)
and blamed on firewalld.

---
## Run KIND deployment with Dual-stack

Currently, IP dual-stack is not fully supported in:
* Kubernetes
* KIND
* OVN-Kubernetes

### Kubernetes And Docker With IP Dual-stack

#### Update kubectl
Kubernetes has some IP dual-stack support but the feature is not complete. <br>
Additional changes are constantly being added. This setup is using the latest
Kubernetes release to test against. <br>
Kubernetes is being installed below using OVN-Kubernetes KIND script, however to test, an equivalent version of `kubectl`
needs to be installed.

First determine what version of `kubectl` is currently being used and save it:

```
$ which kubectl
/usr/bin/kubectl

$ kubectl version --client
Client Version: version.Info{Major:"1", Minor:"28", GitVersion:"v1.17.3",
GitCommit:"06ad960bfd03b39c8310aaf92d1e7c12ce618213", GitTreeState:"clean",
BuildDate:"2020-02-11T18:14:22Z", GoVersion:"go1.13.6", Compiler:"gc", Platform:"linux/amd64"}

$ sudo mv /usr/bin/kubectl /usr/bin/kubectl-v1.17.3
```

Download and install latest version of `kubectl`:
```
$ K8S_VERSION=`curl -L -s https://dl.k8s.io/release/stable.txt`
$ curl -LO "https://dl.k8s.io/release/$K8S_VERSION/bin/linux/arm64/kubectl"

$ chmod +x kubectl && sudo mv kubectl /usr/bin/kubectl-$K8S_VERSION
$ sudo ln -sf /usr/bin/kubectl-$K8S_VERSION /usr/bin/kubectl

$ kubectl version --client
Client Version: v1.32.2
Kustomize Version: v5.5.0
```

### Docker Changes For Dual-stack

For dual-stack, IPv6 needs to be enable in Docker on the host same as
for IPv6 only. See above: [Docker Changes For IPv6](#docker-changes-for-ipv6)

## KIND With IP Dual-stack

IP dual-stack is not currently supported in KIND. There is a PR
([692](https://github.com/kubernetes-sigs/kind/pull/692))
with IP dual-stack changes. Currently using this to test with.

Optionally, save previous version of KIND (if it exists):

```
$ [ -f "$GOPATH/bin/kind" ] && cp "$GOPATH/bin/kind" "$GOPATH/bin/kind.$($GOPATH/bin/kind version -q)"
```

#### Build KIND With Dual-stack Locally

To build locally (if additional needed):
```
$ go get github.com/kubernetes-sigs/kind; cd $GOPATH/src/github.com/kubernetes-sigs/kind
$ git pull --no-edit --strategy=ours origin pull/692/head
$ make clean && make install INSTALL_DIR=$GOPATH/bin
```

### OVN-Kubernetes With IP Dual-stack

For status of IP dual-stack in OVN-Kubernetes, see
[1142](https://github.com/ovn-org/ovn-kubernetes/issues/1142).

To run OVN-Kubernetes with IP dual-stack in a KIND deployment, run:

```
$ go get github.com/ovn-kubernetes/ovn-kubernetes
$ pushd $GOPATH/src/github.com/ovn-kubernetes/ovn-kubernetes

$ pushd go-controller && make && popd
$ pushd dist/images && make fedora-image && popd
$ pushd contrib && KIND_IPV4_SUPPORT=true KIND_IPV6_SUPPORT=true K8S_VERSION=$K8S_VERSION ./kind.sh && popd
$ popd
```

Once `kind.sh` completes, setup kube config file:
```
$ sudo cp /root/ovn.conf ~/.kube/kind-config
$ sudo chown $(id -u):$(id -g) ~/.kube/kind-config
$ export KUBECONFIG=~/.kube/kind-config
```

Once testing is complete, to tear down the KIND deployment:
```
$ kind delete cluster --name ovn
```
---
## Use specific Kind container image and tag

:warning: Use with caution, as kind expects this image to have all it needs.

In order to use an image/tag other than the default hardcoded in kind.sh, specify
one (or both of) the following variables:

```
$ pushd contrib
$ KIND_IMAGE=example.com/kindest/node K8S_VERSION=v1.31.0 ./kind.sh
$ popd
```
---
## Use local Kind registry to deploy non ovn-k containers

A local registry can be made available to the cluster if started with:
```
$ pushd contrib && ./kind.sh --local-kind-registry && popd
```
This is useful if you want to make your own local images available to the 
cluster.<br> These images can be pushed, fetched or used
in manifests using the prefix `localhost:5000`.
---
## Load ovn-kubernetes changes without restarting kind

Sometimes it is useful to update ovn-kubernetes without redeploying the whole 
cluster all over again.<br> For example, when testing the update itself.
This can be achieved with the "--deploy" flag:

```
# Default options will use kind mechanism to push images directly to the
$ ./kind.sh --deploy
```

Using a local registry is an alternative to deploy ovn-kubernetes updates
While also being useful to deploy other local images
```
$ ./kind.sh --deploy --local-kind-registry
```
---
### Current Status

This is subject to change because code is being updated constantly. But this is
a more cautionary note that this feature is not completely working at the
moment.

The nodes do not go to ready because the OVN-Kubernetes hasn't set up the network completely:

```
$ kubectl get nodes
NAME                STATUS     ROLES    AGE   VERSION
ovn-control-plane   NotReady   master   94s   v1.18.0
ovn-worker          NotReady   <none>   61s   v1.18.0
ovn-worker2         NotReady   <none>   62s   v1.18.0

$ kubectl get pods -o wide --all-namespaces
NAMESPACE          NAME                                      READY STATUS   RESTARTS AGE    IP          NODE
kube-system        coredns-66bff467f8-hh4c9                  0/1   Pending  0        2m45s  <none>      <none>
kube-system        coredns-66bff467f8-vwbcj                  0/1   Pending  0        2m45s  <none>      <none>
kube-system        etcd-ovn-control-plane                    1/1   Running  0        2m56s  172.17.0.2  ovn-control-plane
kube-system        kube-apiserver-ovn-control-plane          1/1   Running  0        2m56s  172.17.0.2  ovn-control-plane
kube-system        kube-controller-manager-ovn-control-plane 1/1   Running  0        2m56s  172.17.0.2  ovn-control-plane
kube-system        kube-scheduler-ovn-control-plane          1/1   Running  0        2m56s  172.17.0.2  ovn-control-plane
local-path-storage local-path-provisioner-774f7f8fdb-msmd2   0/1   Pending  0        2m45s  <none>      <none>
ovn-kubernetes     ovnkube-db-cf4cc89b7-8d4xq                2/2   Running  0        107s   172.17.0.2  ovn-control-plane
ovn-kubernetes     ovnkube-master-87fb56d6d-7qmnb            2/2   Running  0        107s   172.17.0.2  ovn-control-plane
ovn-kubernetes     ovnkube-node-278l9                        2/3   Running  0        107s   172.17.0.3  ovn-worker2
ovn-kubernetes     ovnkube-node-bm7v6                        2/3   Running  0        107s   172.17.0.2  ovn-control-plane
ovn-kubernetes     ovnkube-node-p4k4t                        2/3   Running  0        107s   172.17.0.4  ovn-worker
```

## Known issues

Some environments (Fedora32,31 on desktop), have problems when the cluster
is deleted directly with kind `kind delete cluster --name ovn`, it restarts the host.
The root cause is unknown, this also can not be reproduced in Ubuntu 20.04 or
with Fedora32 Cloud, but it does not happen if we clean first the ovn-kubernetes resources.

You can use the following command to delete the cluster:
```
contrib/kind.sh --delete
```
