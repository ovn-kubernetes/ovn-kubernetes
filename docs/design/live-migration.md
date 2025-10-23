# KubeVirt VM Live Migration

## Introduction

The Live Migration feature allows [KubeVirt](https://kubevirt.io) virtual machines to be [live migrated](https://kubevirt.io/user-guide/operations/live_migration/) across nodes while:
- Keeping established TCP connections alive
- Preserving the VM IP and MAC addresses
- Maintaining seamless network connectivity during migration

This provides transparent live-migration of KubeVirt VMs using the OVN-Kubernetes cluster default network.

## Design Goals

The design enables live migration for KubeVirt VMs with bridge binding on the pod's default network by:
- Allowing VM IP addresses to be independent of the node subnet they run on
- Providing consistent gateway IP and MAC addresses regardless of which node hosts the VM
- Delivering IP configuration to VMs via DHCP instead of pod network interface configuration

## High-Level Architecture

### DHCP-Based IP Delivery

OVN-Kubernetes allocates IP addresses using its standard IPAM but delivers them to the VM guest via OVN DHCP options rather than configuring the virt-launcher pod's network interface. This allows KubeVirt's bridge binding to present the same IP to the VM regardless of which node it runs on.

### Point-to-Point Routing

When a VM runs on a node that doesn't "own" its IP address subnet (after live migration), OVN-Kubernetes creates:
- **Static routes** for ingress traffic to reach the VM via its current node
- **Routing policies** for egress traffic from the VM via the appropriate gateway

This enables the VM to retain its IP address even when migrated to a different node's subnet.

### Proxy ARP for Consistent Gateway

To maintain a consistent gateway experience across migrations, OVN-Kubernetes uses:
- A link-local gateway IP (169.254.1.1 for IPv4) with a consistent MAC address
- ARP proxy configuration on logical switch ports connecting nodes to the cluster router
- This ensures VMs always see the same gateway IP and MAC regardless of their location

## Key Implementation Components

1. **Logical Switch Ports**: VM pods get DHCP options configured to advertise IP, gateway, and DNS
2. **Node Switch Ports**: Configured with ARP proxy to respond for the link-local gateway
3. **Static Routes & Policies**: Created dynamically when VMs migrate to non-owning nodes
4. **IPAM Reservation**: IP addresses are reserved even when the owning node's subnet is deallocated

## Limitations

- Only KubeVirt VMs with bridge binding on pod networking are supported
- Single-stack IPv6 is not currently supported
- Dual-stack requires manual IPv6 gateway route configuration in the VM
- Not available for secondary networks

## Reference

For detailed usage instructions, configuration examples, and step-by-step workflows, see the [Live Migration feature guide](../features/live-migration.md).
