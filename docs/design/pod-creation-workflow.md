# Pod Creation Workflow

This document describes how a pod gets its network configured in
OVN-Kubernetes, from the moment it is scheduled to a node until it has
a working network interface with connectivity.

## Overview

Pod networking is a **two-phase, loosely coupled** process:

1. **Control plane (ovnkube-controller)** — watches the Pod API, allocates
   IPs, writes the pod annotation, and creates OVN NorthBound DB objects.
2. **Data plane (CNI on the node)** — kubelet invokes the CNI plugin, which
   waits for the annotation, plumbs the veth/OVS interface, and returns IPs
   to the container runtime. The CNI plugin binary is a lightweight shim that
   forwards requests to the CNI server running inside the ovnkube-node
   container via a Unix socket.

The controller must write the annotation **before** CNI ADD can succeed.
The CNI server polls for the annotation every 200 ms for up to 2 minutes.
See the [End-to-End Timeline](#end-to-end-timeline) below for the full sequence.

## Phase 1: Control Plane — Pod Reconciliation

### Event Handling

When a pod is scheduled, the Kubernetes informer fires an Add event.
The retry framework in `go-controller/pkg/retry/` ensures that transient
failures are retried with exponential backoff.

The call chain:

```go
func (oc *DefaultNetworkController) ensurePod(oldPod, pod *corev1.Pod, addPort bool) error

func (oc *DefaultNetworkController) addLogicalPort(pod *corev1.Pod) (err error)

func (bnc *BaseNetworkController) addLogicalPortToNetwork(pod *corev1.Pod, nadKey string,
    network *nadapi.NetworkSelectionElement, enable *bool) (ops []ovsdb.Operation,
    lsp *nbdb.LogicalSwitchPort, podAnnotation *util.PodAnnotation, newlyCreatedPort bool, err error)
```

Pods are only processed after nodes and services are synced. Host-network
pods are skipped (they do not need an OVN logical port).

### IP Allocation (IPAM)

```go
func (bnc *BaseNetworkController) allocatePodAnnotation(pod *corev1.Pod,
    existingLSP *nbdb.LogicalSwitchPort, podDesc, nadKey string,
    network *nadapi.NetworkSelectionElement, networkRole string) (*util.PodAnnotation, bool, error)
```

The allocation logic:

1. If the pod already has an annotation (e.g. after a restart), the
   existing IPs are reserved in IPAM to prevent duplicates.
2. If an existing LogicalSwitchPort is found in OVN NB, its addresses
   are recovered.
3. Otherwise, `assignPodAddresses()` allocates the next available IP(s)
   from the node's subnet via the LogicalSwitchManager:

```go
assignPodAddresses(switchName)
  → lsManager.AllocateNextIPs(switchName)
    → subnet.Allocator.AllocateNextIPs()
      → ip.Range.AllocateNext()
```

The MAC address is derived deterministically from the first allocated IP
using `util.IPAddrToHWAddr()`.

The allocated addresses are written to the pod as the
`k8s.ovn.org/pod-networks` annotation:

```go
type PodAnnotation struct {
    IPs            []*net.IPNet
    MAC            net.HardwareAddr
    Gateways       []net.IP
    GatewayIPv6LLA net.IP
    Routes         []PodRoute
    TunnelID       int
    Role           string
}
```

Example of a pod on the default network:

```yaml
annotations:
  k8s.ovn.org/pod-networks: |
    {
      "default": {
        "ip_addresses": ["10.244.0.3/24"],
        "mac_address": "0a:58:0a:f4:00:03",
        "gateway_ips": ["10.244.0.1"],
        "routes": [
          {"dest": "10.244.0.0/16", "nextHop": "10.244.0.1"},
          {"dest": "10.96.0.0/16", "nextHop": "10.244.0.1"},
          {"dest": "169.254.0.5/32", "nextHop": "10.244.0.1"},
          {"dest": "100.64.0.0/16", "nextHop": "10.244.0.1"}
        ],
        "ip_address": "10.244.0.3/24",
        "gateway_ip": "10.244.0.1",
        "role": "primary"
      }
    }
```

### OVN NorthBound DB Objects

`addLogicalPortToNetwork()` creates/updates the following in a single
OVSDB transaction:

* **LogicalSwitchPort** — Named `<namespace>_<pod-name>`, attached to the
  node's logical switch. Addresses set to `"<mac> <ip1> <ip2>"`. Port
  security enabled.
* **Namespace port group** — The LSP UUID is added to the namespace's port
  group (used by network policies).
* **External IDs** — `namespace`, `pod=true`, pod UID in `iface-id-ver`,
  and `requested-chassis` (node chassis UUID) for binding.

```go
lsp := &nbdb.LogicalSwitchPort{
    Name:         "namespace_podname",
    Addresses:    []string{"0a:58:0a:f4:00:05 10.244.0.5"},
    PortSecurity: []string{"0a:58:0a:f4:00:05 10.244.0.5"},
    ExternalIDs: map[string]string{
        "namespace": namespace,
        "pod":       "true",
    },
    Options: map[string]string{
        "iface-id-ver":      string(pod.UID),
        "requested-chassis": chassisID, // from k8s.ovn.org/node-chassis-id
    },
}
```

Additional OVN objects may be created depending on features:

* **SNAT rules** — if multi-gateway with no external gateway annotation
* **Gateway routes** — if namespace has external/pod gateway annotations
* **DHCP options** — for live-migratable pods

> [!NOTE]
> NetworkPolicy ACLs are **not** created during pod add. They are managed
> separately by the network policy controller when a pod matches a policy
> selector — the pod's LSP is added to the policy's port group, which
> already has ACLs attached.

## Phase 2: Data Plane — CNI ADD

### CNI Shim

The CNI binary is invoked by kubelet. It acts as a thin shim that forwards
the request over a Unix domain socket to the CNI server running inside
ovnkube-node.

```go
func (pr *Plugin) CmdAdd(args *skel.CmdArgs) error
```

### CNI Server

The CNI server listens on a root-only Unix socket and dispatches ADD/DEL
requests.

```go
func (pr *PodRequest) cmdAdd(kubeAuth *KubeAPIAuth, clientset *ClientSet,
    ovsClient client.Client) (*Response, error)
```

`cmdAdd()` performs the following steps:

1. Get pod from the Kubernetes API.
2. Wait for `k8s.ovn.org/pod-networks` annotation to appear (`isOvnReady`
   check). This is the synchronization point with the control plane.
3. Parse the annotation into IP/MAC/gateway configuration.
4. Call `ConfigureInterface()`.

### Interface Plumbing

```go
func (*defaultPodRequestInterfaceOps) ConfigureInterface(pr *PodRequest,
    getter PodInfoGetter, ifInfo *PodInterfaceInfo) ([]*current.Interface, error)

func ConfigureOVS(ctx context.Context, namespace, podName, podIfName, hostIfaceName string,
    ifInfo *PodInterfaceInfo, sandboxID, deviceID string, isVFIO bool, getter PodInfoGetter) error
```

The interface plumbing steps:

1. Create a veth pair — one end placed in the pod's network namespace
   (`eth0`), the other kept on the host.
2. Configure the pod namespace — assign IP addresses, set up default
   route via the gateway, configure any additional routes.
3. Add to OVS — `ConfigureOVS()` adds the host-side veth to `br-int`
   with external IDs:

```bash
ovs-vsctl add-port br-int abcd1234ef \
  -- set interface abcd1234ef \
       external_ids:iface-id="default_my-pod" \
       external_ids:iface-id-ver="pod-uid-here" \
       external_ids:sandbox="container-id" \
       external_ids:ip_addresses="10.244.0.5/24" \
       external_ids:attached_mac="0a:58:0a:f4:00:05"
```

4. Wait for OVN binding — poll OVS until `ovn-installed=true` is set
   on the interface by ovn-controller, confirming that OpenFlow rules are
   programmed and the port is active.
5. Return CNI result — IPs, MAC, gateway, and routes are returned to
   kubelet through the CNI shim.

Example of an OVS interface after binding (`ovn-installed=true`):

```bash
$ ovs-vsctl list interface 12939f8d3d906f0
name            : "12939f8d3d906f0"
type            : ""
ofport          : 5
mtu             : 1400
link_state      : up
status          : {driver_name=veth, driver_version="1.0"}
external_ids    : {
    attached_mac="0a:58:0a:f4:00:03",
    iface-id="default_workflow-test",
    iface-id-ver="69611cd1-cfcd-414f-afea-5669dc947fb1",
    ip_addresses="10.244.0.3/24",
    ovn-installed="true",
    ovn-installed-ts="1784626958272",
    pod-if-name="eth0",
    sandbox="12939f8d3d906f0675c2d7a8733a4c0fe8ec128ca5506f329ffb91987e544369"
}
```

Corresponding Southbound DB port binding:

```bash
$ ovn-sbctl list port_binding default_workflow-test
_uuid               : <uuid>
chassis             : 789baf7d-f678-42aa-a0e4-b85427878d64
logical_port        : "default_workflow-test"
mac                 : ["0a:58:0a:f4:00:03 10.244.0.3"]
external_ids        : {namespace=default, pod="true"}
options             : {iface-id-ver="69611cd1-cfcd-414f-afea-5669dc947fb1",
                       requested-chassis="636c9bc8-453a-4c6b-9404-201d034700dc"}
port_security       : ["0a:58:0a:f4:00:03 10.244.0.3"]
requested_chassis   : 789baf7d-f678-42aa-a0e4-b85427878d64
tunnel_key          : 4
type                : ""
up                  : true
```

Note `up: true`, and that `logical_port` / `iface-id` and `iface-id-ver` match
across OVS and SBDB.

The Northbound DB LSP reflects the same `up: true` from SBDB:

```bash
$ ovn-nbctl list logical_switch_port default_workflow-test
_uuid               : <uuid>
addresses           : ["0a:58:0a:f4:00:03 10.244.0.3"]
enabled             : []
external_ids        : {namespace=default, pod="true"}
name                : "default_workflow-test"
options             : {iface-id-ver="69611cd1-cfcd-414f-afea-5669dc947fb1",
                       requested-chassis="636c9bc8-453a-4c6b-9404-201d034700dc"}
port_security       : ["0a:58:0a:f4:00:03 10.244.0.3"]
up                  : true
```

## End-to-End Timeline

```mermaid
flowchart TB
    subgraph ControlPlane["Phase 1: Control Plane (ovnkube-controller)"]
        direction TB
        A["Pod Add event (informer)"]
        B["ensurePod()"]
        C["ensureLocalZonePod()"]
        D["addLogicalPort()"]
        E["addLogicalPortToNetwork()"]
        F["allocatePodAnnotation() — IPAM"]
        G["assignPodAddresses() → AllocateNextIPs()"]
        H["IPAddrToHWAddr() — derive MAC from IP"]
        I["updatePodAnnotationWithRetry()"]
        J["Patch pod: k8s.ovn.org/pod-networks (IPs, MAC, gateways, routes)"]
        K["Create LogicalSwitchPort in OVN NBDB"]
        L["Set Addresses + PortSecurity on LSP"]
        M["Add LSP UUID to namespace port group"]
        N["TransactAndCheckAndSetUUIDs()"]
        A --> B --> C --> D --> E --> F --> G --> H --> I --> J --> K --> L --> M --> N
    end

    subgraph OVNStack["OVN Pipeline (asynchronous)"]
        direction TB
        O["NBDB change notification"]
        P["ovn-northd compiles logical flows"]
        Q["Logical flows written to SouthBound DB"]
        R["SBDB change notification"]
        S["ovn-controller translates to OpenFlow"]
        T["OpenFlow rules installed on br-int"]
        U["Set external_ids:ovn-installed=true on OVS port"]
        O --> P --> Q --> R --> S --> T --> U
    end

    subgraph DataPlane["Phase 2: Data Plane (CNI on node)"]
        direction TB
        V["kubelet calls CNI ADD"]
        W["ovn-k8s-cni-overlay (shim binary)"]
        X["HTTP POST to CNI server via Unix socket"]
        Y["GetPodWithAnnotations() — poll every 200ms"]
        Z["Wait up to 2 minutes for annotation"]
        AA["k8s.ovn.org/pod-networks annotation found"]
        AB["setupInterface() — enter pod netns"]
        AC["Create veth pair (eth0 ↔ host-veth)"]
        AD["Assign IPs, MAC, routes, gateway in pod netns"]
        AE["ConfigureOVS() — ovs-vsctl add-port br-int"]
        AF["Set external_ids: iface-id, iface-id-ver, sandbox"]
        AG["Poll OVS for ovn-installed=true"]
        AH["ovn-controller confirms port binding"]
        AI["Build CNI Result: IPs, MAC, gateway, routes"]
        AJ["Return result to kubelet"]
        AK["Pod containers start"]
        V --> W --> X --> Y --> Z --> AA --> AB --> AC --> AD --> AE --> AF --> AG --> AH --> AI --> AJ --> AK
    end

    J -.->|"unblocks"| AA
    N --> O
    U -.->|"sets ovn-installed"| AH
```

### How the phases connect

The critical handoff between the control plane and the data plane is the
**`k8s.ovn.org/pod-networks` pod annotation**:

* The **controller writes** it after IPAM allocation (during pod reconciliation), and CNI waits on this annotation before interface plumbing.
* The **CNI server blocks** on it before continuing with interface setup.

The OVN pipeline (**ovn-northd → ovn-controller**) runs asynchronously between
these two phases:

* **ovn-northd** compiles LogicalSwitchPort changes into logical flows in SBDB.
* **ovn-controller** translates those flows into OpenFlow rules on `br-int` and
  sets `external_ids:ovn-installed=true` on the OVS interface.
* The CNI server's **`ovn-installed` wait** cannot succeed until that step completes.

### What happens if things go wrong

* If the **controller is slow** (backlog, IPAM exhausted, node switch not ready),
  T1 is delayed and the CNI server times out at T5 after ~2 minutes waiting
  for the annotation.
* If **ovn-controller is slow** (SBDB unreachable, flow computation failure),
  T4 is delayed and the CNI server times out at T6 waiting for
  `ovn-installed=true`.
* In both cases, kubelet receives a CNI error and retries pod creation.

## Failure Modes

* **CNI ADD timeout** — if the controller has not written the annotation
  within ~2 minutes (e.g. IPAM exhausted, node switch not ready, controller
  backlog), CNI returns an error and kubelet retries.
* **ovn-installed timeout** — if ovn-controller cannot bind the port (e.g.
  SBDB unreachable, flow computation failure), the CNI server times out.
* **IPAM exhaustion** — `AllocateNextIPs` fails when the node subnet is
  full. The retry framework re-queues the pod.

## Key Source Files

* `go-controller/pkg/ovn/ovn.go`, `pods.go`, `base_network_controller_pods.go` — Pod handler and reconcile logic
* `go-controller/pkg/ovn/default_network_controller.go` — Event handlers and watch setup
* `go-controller/pkg/retry/obj_retry.go` — Retry framework
* `go-controller/pkg/ovn/logical_switch_manager/`, `go-controller/pkg/allocator/ip/` — IPAM
* `go-controller/pkg/util/pod_annotation.go` — Pod annotation marshalling
* `go-controller/pkg/cni/cnishim.go` — CNI shim binary
* `go-controller/pkg/cni/cniserver.go`, `cni.go` — CNI server
* `go-controller/pkg/cni/helper_linux.go`, `ovs.go` — Interface plumbing
* `go-controller/pkg/ovn/base_network_controller_user_defined.go` — User-Defined Networks
