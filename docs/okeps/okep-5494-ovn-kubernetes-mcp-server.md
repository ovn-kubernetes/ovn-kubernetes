# OKEP-5494: Model Context Protocol for Troubleshooting OVN-Kubernetes

# Problem Statement

Diagnosing an issue in [OVN-Kubernetes](https://ovn-kubernetes.io/) network
plugin is complex because it has many layers (Kubernetes, OVN-Kubernetes, OVN,
OpenvSwitch, Kernel - especially Netfilter elements). Usually the person
troubleshooting an issue has to approach it in a layered fashion and has
to be fully aware of all the debugging tools each layer has to offer to
then be able to pin point where the problem is. That is time consuming
and requires years of expertise on each depth of the stack and across all
features. Sometimes it also involves working with the layered community
project teams like OVN and OVS since they hold more knowledge about their
domain than engineers working on the OVN-Kubernetes plugin. Each
troubleshooting session to understand where the packet is getting
blackholed or dropped takes a lot of time to solve (unless it's trivial).

# Goals

The goal of this enhancement is to try to improve "troubleshooting time",
"tool usage/typing time" and "erase the need to know how to use each of those
tools to a parameter level detail" by exposing these tools using
[Model Context Protocol](https://modelcontextprotocol.io/docs/learn/architecture#overview) (MCP)
and leveraging the backend Model's (Claude Sonnet4, Gemini2.5Pro, GPT-5, etc)
knowledge and context to troubleshoot issues on live clusters or locally on
containers with end user's databases loaded. This can go a long
way in speeding up bug triages.

* Phase1 targets only ovn-kubernetes community members as target audience
* Build an OVN-Kubernetes MCP Server(s) that exposes all the tools required
  to troubleshoot OVN-Kubernetes network plugin
    * This MCP Server(s) must also be aware of where to run these tools to
      troubleshoot the issue in question on a cluster (i.e Which node? Which
      pod? Which container?)
* Add support to load a MCP Server against not just a real-time cluster
  but also against a simulated environment constructed from information bundle
  gathered or other debugging information extracted from a live cluster.
  Examples are [must-gather](https://github.com/openshift/must-gather) and
  [sos-reports](https://github.com/sosreport/sos) and how tools like
  [omc](https://github.com/gmeghnag/omc) or [xsos](https://github.com/ryran/xsos)
  can be leveraged
* Ensure that the tools we expose have read only permissions. Whether this means
  we restrict the tools itself with read rights only OR expose only read actions
  via the tools is to be discussed in proposal section.
* Support troubleshooting all features for OVN-Kubernetes
    * We must have extensive failure scenario and chaos tests injected to see
      how good LLM is able to troubleshoot
    * We must form a list of all common ovn-kubernetes bugs we have hit across
      features (example all possible ways of running into staleSNATs)
    * We must add benchmarking tests for evaluating the quality of troubleshooting
      specific to CNI Networking.

# Future Goals

* Phase2 targets end-users and others using OVN-Kubernetes to troubleshoot
  issues
* Expand the MCP server to not just provide tools but also relevant context
  around OVN-Kubernetes implementation
* Creating a RAGing system with all internal docs and integrating that with
  the prompt client for better results (would require improving our docs
  first). This is especially important for the LLM to know how OVN-Kubernetes
  implements each traffic flow and feature and what constructs are created
  underneath into the layers. This might not be needed for older features
  like EgressIPs that has plenty of online resources but for newer features
  like RouteAdvertisements, the LLM may not know much without providing that
  extra context.
* Investigate if there is potential for converting it also into a remediation
  system (this would need write access)
* Work with the Layered project community teams (OVN, OpenvSwitch, Kernel)
  for each of those layers to also own an MCP server since they know their
  stack better. So instead of 1 OVN-Kubernetes MCP Server it would be a
  bunch of servers each owned by specific upstream community projects for
  better maintenance and ownership
* The same MCP Server could also potentially be used as a
  "OVN-Kubernetes network plugin - know it better" chatbot but that is not
  the initial goal here.

# Future-Stretch-Goals

* Phase3 includes this getting productized and shipped to end-users using
  OVN-Kubernetes in some far fetched future to run it on production to
  troubleshoot. But this would require:
    * Having a better overall architecture for the wholesome agentic AI
      troubleshooting solution on a cluster
    * Solving the compute problem of how and where to run the model
    * Having an air tight security vetting.

  Running this stack in production is out-of-scope for this OKEP; it is a
  future-stretch goal contingent on security and testing milestones.

# Non-Goals

* We will not be solving the problem around which LLM should be used. Most of
  the good ones (based on community member experience) were proprietary (claude sonnet4
  and gemini2.5pro) but testing all LLMs and coming up with which works the
  best is not in scope
    * By not solving this problem as part of this enhancement, we risk having
      to deal with "long-context windows causing hallucinations" but that's
      where RAGing mentioned in future goals could help.
* We will also not be developing our own model to teach and maintain it
    * By not solving this problem, we also risk relying on proprietary models
      knowing and learning what we want them to learn but having no control on
      how fast they learn it. Again RAGing could help here.
    * So the quality here will heavily depend on how good the brain LLM is
      which basically won't be in our control much.

# Introduction

An engineer troubleshooting OVN-Kubernetes usually uses the following set of
CLI tools in a layered fashion:

* Kubernetes and OVN-Kubernetes layer - this cluster state information is
  gathered as part of must-gather for offline troubleshooting
    * **kubectl** commands like list, get, describe, logs, exec, events to know
      everything about the Kubernetes API state of the feature and to know what
      the ovnkube pods were doing through the logs they generate during that
      window
    * **ovnkube-trace** which executes ovn-trace and ovs ofproto trace and
      detrace commands (this tool doesn't support all scenarios - it's not
      maintained well)
    * Future-tool - ovnkube CLI (need to ask NVIDIA what's the status here)
        * A place for K8s state-database syncer should/could potentially exist
* OVN Layer - OVN databases are gathered as part of must-gather for
  offline troubleshooting
    * **ovn-nbctl** commands that are executed on the ovnkube node pods to
      understand what OVN-Kubernetes created into northbound database via
      libovsdbclient transactions
    * **ovn-sbctl** commands that are executed on the ovnkube node pods to
      understand what northd created into southbound database
    * **ovn-trace and detrace** commands that are executed on the ovnkube node
      pods to understand simulated packet flow tracing based on flows in
      southbound database
    * **ovn-appctl -t ovn-controller ct-zone-list** to list all the conntrack
      zone to OVN construct mapping for better understanding how the conntrack
      commit of the packets happened
* OpenvSwitch Layer - openvswitch database is usually gathered as part
  of sos-report for offline troubleshooting
    * **ovs-ofctl dump-flows** to debug specially the breth0 openflows
      that OVN-Kubernetes creates on the gateway of each node
    * **ovs-appctl dpctl/dump-flows** to trace live packets run on a specific
      node's ovs container (KIND) or on the node (OpenShift)
    * **ovs-appctl ofproto/trace** and detrace to run an ovs trace of the
      packet based on the openflows
    * **ovs-appctl dpctl/dump-conntrack** to know all the conntrack zones used
      for a specific connection
    * **ovs-vsctl** commands to list interfaces and bridges
    * **retis** to see packet drops in ovs
* Netfilter/Kernel Layer - this information is usually gathered as part
  of sos-report for offline troubleshooting
    * **ip util commands** like **ip r** or **ip a** or **ip rule list** for
      debugging VRFs, BGP learnt routes or the routes OVN-Kubernetes creates
      on the node or custom routes that end user's add
    * **nft list ruleset** to understand what rules were created by
      OVN-Kubernetes specially in routingViaHost=true gateway mode
    * **iptables-save** to list all iptables (Given iptables is deprecated,
      I think we can skip this tool though for now 50% of ovn-kubernetes is
      still on IPT)
    * **conntrack** -L or -E on the host itself
    * **ip xfrm policy** and **ip xfrm state** when using IPSEC
* TCPDUMP - external open source tools, can't be used for offline
  troubleshooting
    * **tcpdump** is used for packet capture and analysis
    * [**pwru**](https://github.com/cilium/pwru) is used to know the kernel drop reason
    * **libreswan** `ipsec-stateus` and `ipsec-trafficstatus` commands
    * **frr** frr router config and routes learnt by BGP

Ideally speaking, there are metrics and events that via alerts also go to
the dashboard which is probably what most end-users use to troubleshoot.
So when we do the phase3 we would need to reconsider this stack of
troubleshooting entirely for including other aspects like OVN-Kubernetes
troubleshooting dashboard that the observability team created or the
various packet drop tools observability team already exposes. But for
the scope of this enhancement, for now, we will consider these above set
of tools as MVP.

As we can see, that's a lot of tools! So remembering the syntax for each of
these tools always and executing them one-by-one and gathering the information
at each layer, analysing them, and then moving to the next layer takes time
for a human. Always during a remote analysis of bug report the part that takes
the longest is the RCA by combing through all the data - same goes for
troubleshooting a cluster (which is slightly easier when we have access to the
cluster than analysing offline data). The fix is usually the easiest part (there are
exceptions).

```
                    OVN-Kubernetes Architecture & Troubleshooting Tools
                              (Per Node Components)

┌────────────────────────────────────────────────────────────────────────────┐
│                           ovnkube-node pod                                 │
│                                                                            │
│  ┌─────────────────┐    ┌─────────────────┐         kubectl exec/logs      │
│  │  ovnkube        │◄──►│      NBDB       │◄─────── ovn-nbctl show/list    │
│  │  controller     │    │  (northbound)   │                                │
│  └─────────────────┘    └─────────────────┘                                │
│           │                       │                                        │
│           │              ┌─────────────────┐                               │
│           │              │     northd      │                               │
│           │              └─────────────────┘                               │
│           │                       │                                        │
│           │              ┌─────────────────┐                               │
│           └─────────────►│      SBDB       │◄─────── ovn-sbctl show/list   │
│                          │  (southbound)   │         ovn-trace/detrace     │
│                          └─────────────────┘                               │
│                                   │                                        │
│                          ┌─────────────────┐                               │
│                          │ ovn-controller  │◄─────── ovn-appctl ct-zone    │
│                          └─────────────────┘                               │
│                                   │                                        │
└───────────────────────────────────┼────────────────────────────────────────┘
                                    │
                          ┌─────────────────┐
                          │       OVS       │◄─────── ovs-vsctl list
                          │   (database)    │         ovs-appctl dpctl/
                          └─────────────────┘         ovs-ofctl dump-flows
                                   │                  retis (packet drops)
                          ┌─────────────────┐
                          │   OVS bridge    │◄─────── ovs-appctl ofproto/trace
                          │   br-int/breth0 │
                          └─────────────────┘
                                   │
                          ┌─────────────────┐
                          │      NIC        │
                          │   (physical)    │
                          └─────────────────┘
                                   │
                    ┌──────────────┴──────────────┐
                    │         Host Network        │
                    │                             │
                    │  ip route/addr/rule ◄───────┼─────── ip commands
                    │  nft ruleset        ◄───────┼─────── nft list ruleset
                    │  iptables rules     ◄───────┼─────── iptables-save
                    │  conntrack zones    ◄───────┼─────── conntrack -L/-E
                    │                             │
                    │  Network interfaces ◄───────┼─────── tcpdump/pwru
                    └─────────────────────────────┘

    Problem: Engineers must know WHERE to run WHICH tool on WHICH component
             to troubleshoot issues across this distributed architecture
```

This enhancement aims to solve this pain point of reducing the time taken to
execute these tools and analyse these results using MCP Servers and LLMs.

# User Stories

**As an OVN-Kubernetes developer**, **I want to** troubleshoot my stack
without needing to know every tool's parameter fields by-heart or by spending
time looking it up each time I need to troubleshoot a feature **so that** I
can spend my time efficiently. I just want to tell in plain english what I
want and for the MCP server to execute those specific commands.

**As an OVN-Kubernetes engineer**, **I want to** troubleshoot my stack
without needing to analyse each flow output of these tools when I need to
troubleshoot a feature **so that** I can spend my time efficiently. I just
want to tell in plain english what I want and for the LLM to help me analyze
the output of the commands executed by the MCP Server. I understand that I
will need to verify the reasoning thoroughly before accepting the RCA from AI.

**As a new engineer joining the OVN-Kubernetes team**, **I want to** retrieve
specific information from different parts of the stack without having
knowledge of the topology or tooling of the stack.

# Proposed Solution

We build a Golang MCP Server (could be split into a set of MCP Servers in the
future) that exposes these tools in read only fashion and the LLM backend
that has the required context will analyse the results of the execution and
provide a response back to the prompter who has to verify it thoroughly. This
MCP Server code will be in a new repo in
[ovn-kubernetes org](https://github.com/ovn-kubernetes) called
**ovn-kubernetes-mcp**.

## Example Workflow for an end-user

1. An end-user can use any MCP Client to start a troubleshooting session via
   prompting. The client connects with all the available servers (in our case
   the OVN-Kubernetes MCP Server and maybe in the future all layered community
   MCP Severs) and gathers their available tools and presents this information
   to the LLM along with their schemas. Example: Using Cursor AI as your MCP
   Client
2. LLM will be able use its intelligence and analyze the end-user query and
   choose the appropriate tools. Example: Using Claude Sonnet4 as your LLM
   model
3. MCP Client then receives the LLM's tool call and routes it to the
   corresponding MCP Server which executes the tool and client then relays
   the response back to the LLM
4. LLM again uses its intelligence to analyze the responses
5. LLM provides a RCA back to the end user

The steps 2, 3 and 4 is repeated by the LLM and it intelligently does a step-by-step
layered troubleshooting exercise to find the root cause.

We may also include predefined and tested prompt steps in the documentation
of our repo for helping end-users to give the LLM good context around
OVN-Kubernetes and OVN and OVS. For example, provide the latest OVN man
pages so that it has the current knowledge of OVN DB schemas and tools usage.
Some standard preparation prompt can be maintained in the mcp-server repo
as reference.

```
OVN-Kubernetes Troubleshooting MCP Architecture
===============================================

Engineer Query: "Pod A can't reach Pod B on different nodes, consistent connection drops"

┌──────────────────────────────────────────────────────────────────────────────┐
│                    MCP SERVERS (Layer-Specific Tools)                        │
└──────────────────────────────────────────────────────────────────────────────┘

┌──────────────────────┐ ┌──────────────────────┐ ┌──────────────────────┐ ┌──────────────────────┐ ┌──────────────────────┐
│   Kubernetes/K8s     │ │     OVN Layer        │ │   OpenvSwitch        │ │   Netfilter/         │ │   TCPDUMP/Debug      │
│    MCP Server        │ │    MCP Server        │ │   Layer MCP          │ │   Kernel MCP         │ │    MCP Server        │
│                      │ │                      │ │   Server             │ │   Server             │ │                      │
│ Tools:               │ │ Tools:               │ │                      │ │                      │ │ Tools:               │
│ • kubectl_get        │ │ • ovn_nbctl_show     │ │ Tools:               │ │ Tools:               │ │ • tcpdump_capture    │
│ • kubectl_describe   │ │ • ovn_nbctl_list     │ │ • ovs_ofctl_flows    │ │ • ip_route_show      │ │ • tcpdump_analyze    │
│ • kubectl_logs       │ │ • ovn_sbctl_show     │ │ • ovs_appctl_dpctl   │ │ • ip_addr_show       │ │ • pwru_trace         │
│ • kubectl_exec       │ │ • ovn_sbctl_list     │ │ • ovs_ofproto_trace  │ │ • ip_rule_show       │ │                      │
│ • kubectl_events     │ │ • ovn_trace          │ │ • ovs_appctl_conntr  │ │ • nft_list_ruleset   │ │                      │
│ • ovnkube_trace      │ │ • ovn_detrace        │ │ • ovs_vsctl_show     │ │ • iptables_save      │ │                      │
│                      │ │ • ovn_controller_ct  │ │ • retis_capture      │ │ • conntrack_list     │ │                      │
│                      │ │                      │ │                      │ │ • conntrack_events   │ │                      │
└──────────────────────┘ └──────────────────────┘ └──────────────────────┘ └──────────────────────┘ └──────────────────────┘

                                │ All tools aggregated
                                ▼

┌──────────────────────────────────────────────────────────────────────────────┐
│                           MCP CLIENT                                         │
│                    (OVN-K8s Troubleshoot AI)                                 │
│                                                                              │
│ Unified Tool Interface:                                                      │
│ ┌──────────────────────────────────────────────────────────────────────────┐ │
│ │ Layer 1 (K8s):    kubectl_*, ovnkube_trace                               │ │
│ │ Layer 2 (OVN):    ovn_nbctl_*, ovn_sbctl_*, ovn_trace_*                  │ │
│ │ Layer 3 (OVS):    ovs_ofctl_*, ovs_appctl_*, ovs_vsctl_*, retis_*        │ │
│ │ Layer 4 (Kernel): ip_*, nft_*, iptables_*, conntrack_*                   │ │
│ │ Layer 5 (Debug):  tcpdump_*, pwru_*                                      │ │
│ └──────────────────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────────┘

                        │ Present unified interface
                        ▼

┌──────────────────────────────────────────────────────────────────────────────┐
│                        LLM (OVN-K8s Expert)                                  │
└──────────────────────────────────────────────────────────────────────────────┘
```

Doing multiple MCP Servers has clear advantages like each layer owning their
toolset, independent development and maintenance, granular RBAC, reusability,
one server can't affect the other. Given we would need to align with different
layered communities later on for a fully supported set of servers from their
side which might take several releases, for the phase1 here, we plan to build
our own monolithic server that could then be split into multiple servers later
on. A single unified server is simpler, faster to iterate on.

So our current implementation design looks like this:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              MCP Client                                     │
│                        "Debug pod connectivity issues"                      │
└─────────────────────────────────────────────────────────────────────────────┘
                                       │
                                 MCP Protocol
                                       │
                                       ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│                        OVN-Kubernetes MCP Server                             │
│                                                                              │
│  ┌─────────────────┐  ┌─────────────────┐  ┌──────────────────────────────┐  │
│  │   Kubernetes    │  │  Live Cluster   │  │      Offline Bundle          │  │
│  │     Layer       │  │   Execution     │  │       Execution              │  │
│  │                 │  │                 │  │                              │  │
│  │ • kubectl get   │  │ kubectl exec    │  │ Offline artifacts parser     │  │
│  │ • kubectl desc  │  │                 │  │ tools.                       │  │
│  │ • kubectl logs  │  │ Direct API      │  │ Example: xsos and omc        │  │
│  └─────────────────┘  └─────────────────┘  └──────────────────────────────┘  │
│                                                                              │
│  ┌──────────────────────────────────────────────────────────────────────┐    │
│  │                      Tool Categories                                 │    │
│  │                                                                      │    │
│  │  ┌─────────────┐  ┌─────────────┐  ┌──────────────┐  ┌─────────────┐ │    │
│  │  │ OVN Layer   │  │ OVS Layer   │  │Kernel Layer  │  │External     │ │    │
│  │  │             │  │             │  │              │  │Tools        │ │    │
│  │  │ ovn-nbctl   │  │ ovs-ofctl   │  │ ip route     │  │ tcpdump     │ │    │
│  │  │ ovn-sbctl   │  │ ovs-appctl  │  │ nft list     │  │ pwru        │ │    │
│  │  │ ovn-trace   │  │ ovs-vsctl   │  │ conntrack    │  │ retis       │ │    │
│  │  │ ovn-detrace │  │ ovs-dpctl   │  │ iptables-save│  │             │ │    │
│  │  │ ovn-appctl  │  │ ovs-ofproto │  │              │  │             │ │    │
│  │  └─────────────┘  └─────────────┘  └──────────────┘  └─────────────┘ │    │
│  └──────────────────────────────────────────────────────────────────────┘    │
│                                                                              │
│  ┌──────────────────────────────────────────────────────────────────────┐    │
│  │                      Security & RBAC                                 │    │
│  │                                                                      │    │
│  │ • ovn-troubleshooter ClusterRole (read-only)                         │    │
│  │ • Command parameter validation                                       │    │
│  │ • Node-specific targeting required                                   │    │
│  │ • Write operations blocked                                           │    │
│  └──────────────────────────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────────────────────────┘
                                       │
                           ┌───────────┼───────────┐
                           │           │           │
                           ▼           ▼           ▼
                      ┌────────┐  ┌────────┐  ┌────────┐
                      │ Node1  │  │ Node2  │  │ NodeN  │
                      │        │  │        │  │        │
                      │ovnkube-│  │ovnkube-│  │ovnkube-│
                      │node pod│  │node pod│  │node pod│
                      │        │  │        │  │        │
                      │ ovn-nb │  │ ovn-nb │  │ ovn-nb │
                      │ ovn-sb │  │ ovn-sb │  │ ovn-sb │
                      │ ovs    │  │ ovs    │  │ ovs    │
                      └────────┘  └────────┘  └────────┘

Data Flow Examples:
├─ Live: kubectl exec ovnkube-node-xyz -c nb-ovsdb -- ovn-nbctl show
├─ Live: kubectl debug node/worker-1 -- ovs-ofctl dump-flows br-int
├─ Live: kubectl debug node/worker-1 -- ip route show table all
├─ Offline: Parse must-gather/ovn-kubernetes/ovn-northbound.db
├─ Offline: Parse sos-report/node-1/openvswitch/ovs-ofctl_dump-flows
└─ Offline: Parse sos-report/node-1/networking/ip_route
```

Note: Some of the layers like Kubernetes and Offline Debugging have
existing servers like [kubernetes-mcp-server](https://github.com/containers/kubernetes-mcp-server)
and [mustgather-mcp-server](https://github.com/shivprakashmuley/mustgather-mcp-server)
can be re-used together with ovn-kubernetes-mcp server for holistic
end user experience. However kubernetes-mcp-server exposes `kubectl-exec`
which has security implications (although they also have a read-only
mode where only read commands are exposed).

Note2: Feeding container logs to LLM will make the context window
full pretty fast. We need to investigate a method to ensure we are
filtering out relevant logs to feed.

## Implementation Details of the OVN-Kubernetes MCP Server

See the Alternatives section for other ideas that were discarded.

### Chosen Approach: Direct CLI Tool Exposure (Idea1)

The initial implementation takes a pragmatic approach by directly exposing the
existing CLI tools (`ovn-nbctl`, `ovn-sbctl`, `ovs-vsctl`, etc.) as MCP tools.
While this approach may seem less elegant than creating higher-level wrapper
abstractions to connect to the database (see discarded alternatives), it
offers the fastest path to value for OVN-Kubernetes engineers who are already
familiar with these tools but want to leverage LLM assistance for command
construction and output analysis. The CLI-based approach is also optimal for
exposing the complete troubleshooting toolkit across all layers of the stack.
Most other discarded ideas only addressed OVSDB access and reusability
concerns while failing to provide the holistic set of tools needed for
comprehensive root cause analysis like running packet traces.

The MCP server acts as a secure execution bridge, translating natural language
troubleshooting requests into appropriate CLI commands.

**Advantages**:

* **Fastest Time to Value**: Leverages existing tools that engineers already
  know
* **Zero Deployment Overhead**: All required CLI binaries are already present
  in the pods and nodes within the cluster
* **Comprehensive Coverage**: Only approach that provides access to the
  complete troubleshooting toolkit across all stack layers
* **Security Controls**: Enables read-only access enforcement by exposing
  only the tools with read-only access (get/list)
* **No version compatibility issues** between the MCP server tools and the
  cluster's installed versions when running on live cluster environments

**Trade-offs**:

* **Limited Reusability**: Somewhat specific to OVN-Kubernetes deployment
  patterns and can't be reused in other layers like OVS

This approach was selected as the optimal balance between security,
functionality, and development effort.

## Security Model and RBAC Constraints

No matter how we approach this implementation, it is impossible to totally
secure the execution of networking troubleshooting tools at this stage. Most
networking tools require privileged access to function properly, creating
inherent security trade-offs. The goal is therefore to minimize blast radius
through layered controls rather than achieve perfect isolation.

**Kubernetes Layer Security:**

* `kubectl exec` and `kubectl debug node` operations require cluster-admin level
  privileges by design. Avoid exposing generic exec in the K8s layer.
  Prefer direct Kubernetes API reads (`get`, `list`, `logs`, `events`).
* Alternative approach using custom debug containers
  (`kubectl debug node --image=<custom-image>`) with volume mounts to database
  files reduces some attack surface but remains intrusive
* **Mitigation**: Expose only Kubernetes API read operations (`get`, `list`,
  `logs`, `events`); remove any generic exec tool in this layer.
* **Constraint**: MCP Server service account requires elevated privileges
  (including `pods/exec`) despite being conceptually a "troubleshooter" role

At first, for the kubernetes layer, we thought of leveraging the opensource
[kubernetes-mcp-server](https://github.com/containers/kubernetes-mcp-server).
So whatever security posture they use for now, can be adopted. They have a
`read-only` mode and a mode where write can be done via kubectl exec.
Later, after getting reviews, this enhancement has changed the approach
and pivotted towards opting into a more secure approach of adding a
tool that is a wrapper on top of `kubectl-exec` without directly exposing
kubectl-exec. So instead of relying on `kubernetes-mcp-server`, our
`ovn-kubernetes-mcp` will take the more secure approach of only using
`kubectl_exec` as an implementation detail but not directly expose it.
Downside of this is that we will need to also account for get or list
resources command being duplicated into `ovn-kubernetes-mcp`. So we
would need to implement the tools we need also on the kubernetes layer
ourselves.

**OVN/OVS Database Layer Security:**

* Unix socket-based database access prevents using SSL certificate-based
  authentication and authorization
* Database connections inherit the security context of the container executing
  the commands
* **Mitigation**: Command parameter validation to ensure only read-only
  database operations (`show`, `list`, `dump`) while blocking modification
  commands (`set`, `add`, `remove`)
* **Long-term Path**: Requires RBAC-enabled CLI execution even against local
  unix socket.

**Host/Kernel Layer Security:**

* Kernel-level networking tools (`ip`, `nft`, `conntrack`) inherently require
  root system access
* Current tooling lacks granular RBAC capabilities - tools are typically
  all-or-nothing from a privilege perspective
* **External Tools Note**: Tools like `tcpdump -i any` can be highly intrusive
  as they capture all network traffic on the host, requiring careful
  consideration of privacy and performance impact while being chosen for
  execution.
* **Short-term Mitigation**: Strict command allowlisting exposing only read
  operations (`ip route show`, `nft list ruleset`) while blocking modification
  commands (`ip route add`, `nft add rule`)
* **Long-term Path**: Requires RBAC-enabled wrapper tools from upstream
  layered community teams (example netfilter, kernel networking)

## Distributed Execution Context

MCP Server will only be supported on interconnect mode.

### **OVN-Kubernetes Architecture Challenge**

In OVN-Kubernetes interconnect architecture, each node maintains its own local
instances of critical databases and services:

* **Northbound Database**: Local OVN northbound database per node
* **Southbound Database**: Local OVN southbound database per node
* **OpenVSwitch Database**: Node-specific OVS database and flow tables
* **Host Networking**: Node-specific routing tables, conntrack zones, and
  kernel state

This distributed architecture means that troubleshooting commands must be executed
on the specific node where the relevant data resides.

### **Node-Targeted Command Execution**

**Node Selection Strategy**: All tools requiring node-specific data accept a
`node_name` parameter as a required argument. The MCP server uses this
parameter to:

1. **Pod Selection**: Locate the appropriate `ovnkube-node` pod running on the
   specified node using `pod.spec.nodeName` matching
2. **Container Targeting**: Route OVN database commands to the correct
   container within the ovnkube-node pod (e.g., `nb-ovsdb`, `sb-ovsdb`
   containers)
3. **Execution Context**: Execute host-level commands via
   `kubectl debug node/<node_name> --image <>` for direct host access

**LLM Responsibility**: The LLM must determine the appropriate target node(s)
based on the troubleshooting context:

* **Pod-specific issues**: Use the node where the problematic pod is scheduled
  (`kubectl get pod -o wide`)
* **Network flow analysis**: Target nodes along the packet path (source node,
  destination node, gateway nodes)
* **Cluster-wide analysis**: Potentially execute commands across multiple
  nodes for correlation

We need to account for testing the LLM Responsibility side which is not
something we can guarantee but something we are offloading to the LLM that we
won't solve.

## Deployment Strategy

### **Flexible Deployment Modes**

The MCP server is designed for flexible deployment without requiring elaborate
cluster infrastructure. Multiple deployment modes support different use cases
and security requirements:

**CLI Tool Mode (Simplest)**:

* Run the MCP server binary directly on a machine with `kubectl` access
  to the target cluster
* Server uses existing cluster credentials and executes commands via standard
  CLI tools
* Suitable for both live cluster troubleshooting
* Offline data based troubleshooting analysis would still need corresponding
  parser tools to be run locally where the debug artifact files are hosted
* No cluster deployment required - operates entirely through external API
  access

**Debug Container Mode**:

* Package the MCP server as a container image that the LLM can select for
  `kubectl debug node --image=<mcp-server-image>` operations
* This custom debug image contains the MCP server binary along with all
  necessary troubleshooting tools
* Reduces blast radius compared to using default debug images with full host
  access
* The LLM chooses this image when it needs to execute commands requiring
  direct host access

**Future Considerations**:

* More elaborate deployment patterns (DaemonSet, Deployment) can be considered
  when we think of use cases beyond ovn-kubernetes developers

## Testing Strategy

**Unit Testing - MCP Server Tools**:

* Straightforward validation of individual tool execution and parameter
  handling. Use [mcp-inspector](https://github.com/modelcontextprotocol/inspector).
* Mock cluster responses to test command routing and error handling
* Verify security controls and command allowlisting functionality

**Integration Testing - The Complex Challenge**:

* **Real Failure Scenario Reproduction**: Design test scenarios based on past
  bugs and commonly occurring incidents
* **Chaos Engineering Integration**: Implement controlled failure injection to
  create realistic troubleshooting scenarios
* **LLM Reasoning Validation**: The most critical and challenging aspect -
  verifying that the LLM can produce meaningful root cause analysis from tool
  outputs

### **Scenario-Based Test Design**

**Historical Incident Replay**:

* Collect must-gather and sos-report bundles from past bugs
* Use these as offline test datasets to validate LLM troubleshooting accuracy
* Build regression test suite ensuring consistent analysis quality over time

**Synthetic Failure Scenarios**:

Some examples include:
* Network policy and EgressIP misconfigurations
* Pod connectivity failures across nodes
* Gateway flow issues and routing problems
* OVN database inconsistencies specially around EgressIPs

**LLM Capability Assessment**:

* Measure accuracy of root cause identification - depends on how much
  OVN-Kubernetes feature specific context it has
* Evaluate quality of troubleshooting step recommendations
* Test correlation of multi-layer data analysis
* Validate handling of incomplete or missing data scenarios

### **Success Metrics**

* **Accuracy**: Percentage of correct root cause identifications in known
  failure scenarios
* **Completeness**: Coverage of troubleshooting steps recommended vs. manual
  expert analysis
* **Efficiency**: Time reduction compared to manual troubleshooting workflows
* **Safety**: Verification that only read-only operations are executed as
  intended

## Documentation Details

OKEP for the MCP Server will be on
[https://ovn-kubernetes.io/](https://ovn-kubernetes.io/) . All end-user
documentation will be on the new repo's docs folder.

* **Getting Started Guide**: MCP client setup and initial configuration
* **Troubleshooting Scenarios**: Common use cases and example natural language
  queries
* **Tool Reference**: Available tools and their capabilities across all stack
  layers
* **Security Model**: Warning around security considerations
* **Deployment Options**: CLI mode vs. debug container mode setup instructions
* **Offline Analysis**: Must-gather and sos-report analysis workflows

## Alternative Implementation Ideas

### Idea0: Using Existing kubernetes-mcp-server with Generic Bash Execution

**Approach**: Use the existing kubernetes-mcp-server's `pods_exec` tool to run
arbitrary bash commands like `ovn-nbctl show` or `ip route` directly through
`kubectl exec` or `kubectl debug` sessions, without building any specialized
tooling.

**Rationale**: This approach would provide immediate access to all CLI tools
without any development effort, leveraging the LLM's knowledge of command
syntax to construct appropriate bash commands.

**Why Discarded**:

* **Security Risk**: Allowing arbitrary bash command execution creates
  significant security vulnerabilities. There's no protection against
  destructive commands like `ovn-nbctl set` operations that could modify live
  database state.
* **Lack of Access Control**: No way to enforce read-only operations or
  validate command parameters before execution.
* **Separation of Concerns**: The LLM should focus on analysis and
  troubleshooting logic, not on understanding the security implications of
  direct system access.
* **Blast Radius**: Any compromise or LLM hallucination could potentially
  execute dangerous commands on production systems.

The fundamental principle that "each layer knows best about how/what tools to
allow with proper read access" makes a controlled wrapper approach essential
rather than direct bash execution.

### Idea1: Chosen Approach: Direct CLI Tool Exposure

See the proposed solution section.

### Idea2: libovsdb-client Golang Wrapper

**Approach**: Build a Golang MCP server using `NewOVSDBClient` to directly
query OVSDB instances with proper RBAC controls implemented through OVSDB's
native access control mechanisms.

**Advantages**:
* **High Reusability**: Could be shared across OVN, OVS, and OVN-Kubernetes
  projects
* **Native RBAC**: Leverages OVSDB's built-in role-based access controls
* **Structured Output**: Returns structured data rather than CLI text parsing

**Why Discarded**:
* **Deployment Model**: Would require running as a DaemonSet on each node or
  shipping binaries to ovnkube-node pods
* **Scope Limitation**: Only addresses database access, missing ovn and ovs
  flow trace simulation and host networking tools

### Idea3: ovsdb-client Binary Wrapper

**Approach**: Create a wrapper around the existing `ovsdb-client` binary
(owned by the OVS team) to provide structured database access.

**Advantages**:
* **High Reusability**: Could be shared across OVN, OVS, and OVN-Kubernetes
  projects
* **Native RBAC**: Leverages OVSDB's built-in role-based access controls

**Why Discarded**:
* **Ownership**: We could argue this wrapper better belongs in the openvswitch
  community then in OVN-Kubernetes org
* **Scope Limitation**: Only addresses database access, missing ovn and ovs
  flow trace simulation and host networking tools
* In future once we reach out to OVN and OVS communities to see what
  they plan to do, we could revisit it.

### Idea4: Direct Database Access Wrapper

**Approach**: Build wrapper for direct database read operations bypassing CLI
and client tools entirely.

**Advantages**:
* **High Reusability**: Could be shared across OVN, OVS, and OVN-Kubernetes
  projects

**Why Discarded**: Building from scratch when there's no real need to -
existing CLI tools already provide all necessary functionality with proven
reliability.

# Known Risks and Limitations

* AI! We can trust it only as much as we can throw it.
    * Quality of this troubleshooter depends on the LLM's intelligence
    * Quality of the MCP Server itself is however in our own hands and can be
      enhanced based on user experience
* Security! We know that we cannot fully eliminate the risk
* Performance/Scalability: MCPs are a relatively new concept. So aspects like
  how many tools could we expose per server and upto what point it scales etc
  are unknowns. We will need to try and test it in our PoCs as we develop this.
  * With sending bulky logs and debug details we also have danger of context
    window running out. We need to ensure we filter out relevant logs.
  * Token consumption and context window bloating.
  * Other potential problems we need to rule out during testing:
    * Poor tool selection: LLMs struggle to choose the right tool from too many options
    * Parameter hallucination: Agents invoke tools with incorrect or fabricated parameters
    * Misinterpretation: Responses from tools are more likely to be misunderstood
    * Attention spreading: The model's attention gets distributed thinly across many options
