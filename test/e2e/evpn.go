package e2e

// =============================================================================
// EVPN E2E Test Infrastructure
// =============================================================================
//
// This file contains utilities and tests for EVPN (Ethernet VPN) functionality
// in OVN-Kubernetes. EVPN enables Layer 2 and Layer 3 connectivity between
// pods in a Kubernetes cluster and external networks using BGP as the control
// plane and VXLAN as the data plane.
//
// =============================================================================
// Test Infrastructure Overview
// =============================================================================
//
// EVPN Test Infrastructure:
//
// +---------------------------------------+                      +------------------+
// | KIND Cluster                          |    KIND Primary      | External FRR     |
// |                                       |    Network           | (reused from BGP)|
// | +-------------+ +-------------+       |    (BGP/EVPN)        |                  |
// | | Node 1      | | Node 2      |       |    172.18.0.0/16     | eth0 (primary)   |
// | | - FRR-K8s   | | - FRR-K8s   |       | <----------------->  | br0 (EVPN)       |
// | | - OVN VTEP  | | - OVN VTEP  |       |                      | vxlan0 (VTEP)    |
// | +-------------+ +-------------+       |                      +------------------+
// |                                       |                             |
// | +-------------+ +----------------+    |         +------------------+--------------------+
// | | Node 3      | | Pod (on CUDN)  |    |         |   CUDN-network   | agnhost-ipvrf-net  |
// | | - FRR-K8s   | | 10.100.0.0/16  |    |         |   10.100.0.0/16  | 172.27.102.0/24    |
// | | - OVN VTEP  | +----------------+    |         +--------|---------+---------|---------+
// | +-------------+                       |                  |                   |
// +---------------------------------------+                  v                   v
//                                                   +------------------+--------------------+
//                                                   | agnhost-macvrf   | agnhost-ipvrf      |
//                                                   | 10.100.0.250     | 172.27.102.2       |
//                                                   | VNI: 10100       | VNI: 20102         |
//                                                   +------------------+--------------------+
//
// =============================================================================
// KIND Setup Prerequisites
// =============================================================================
//
// EVPN tests depend on the RouteAdvertisements/BGP infrastructure being set up
// during KIND cluster installation. This is enabled via:
//
//   $ ./contrib/kind.sh -rae
//
// This creates:
//   1. FRR container ("frr") connected to KIND's primary network
//   2. FRR-K8s operator deployed in the cluster
//   3. BGP peering between cluster nodes and external FRR
//
// EVPN tests REUSE the existing FRR container rather than creating a new one.
// We extend its configuration to support EVPN by adding:
//   - br0: Linux bridge with vlan_filtering for EVPN
//   - vxlan0: VXLAN device (VTEP) with vnifilter for SVD mode
//   - VRF and SVI configuration for IP-VRF tests
//   - Access ports for MAC-VRF tests
//   - BGP EVPN address-family configuration
//
// This approach:
//   - Avoids duplicate infrastructure setup
//   - Ensures consistent BGP AS numbers and peering
//   - Leverages existing FRR-K8s CRDs for cluster-side config
//
// =============================================================================
// Component Details
// =============================================================================
//
// KIND Cluster (172.18.0.0/16):
//   - 3 nodes running OVN-Kubernetes with EVPN support
//   - FRR-K8s daemonset on each node for BGP/EVPN peering
//   - OVN VTEP (br0 + vxlan0) configured on each node via VTEP CR
//
// External FRR Container:
//   - Reuses the existing FRR container from BGP/RouteAdvertisements tests
//   - Acts as the EVPN peer for the cluster nodes
//   - Components:
//     * eth0: Connected to KIND primary network (172.18.0.0/16) for BGP peering
//     * br0: Linux bridge for EVPN with vlan_filtering enabled
//     * vxlan0: VXLAN device (VTEP) with vnifilter for SVD (Single VXLAN Device) mode
//
// External Agnhost Containers (test endpoints):
//   - agnhost-macvrf: For L2 MAC-VRF connectivity tests
//     * Connected to br0 via evpn10100 access port (PVID 100)
//     * IP: 10.100.0.250/16 (same subnet as CUDN)
//     * Runs agnhost netexec for HTTP connectivity verification
//   - agnhost-ipvrf: For L3 IP-VRF connectivity tests
//     * Connected to vrf202 (routed via br0.202 SVI)
//     * IP: 172.27.102.2/24 (external routed network)
//     * Runs agnhost netexec for HTTP connectivity verification
//
// =============================================================================
// Test Type 1: Layer 3 CUDN with IP-VRF
// =============================================================================
//
// This test validates north-south L3 connectivity between pods on a Layer 3
// CUDN and an external server via EVPN Type-5 (IP Prefix) routes.
//
// Data Path (Pod -> External agnhost-ipvrf and vice versa):
//   Pod (10.100.x.x) -> OVN logical network -> ovn-k8s-mpX (management port)
//   -> Linux VRF (mpX-udn-vrf) -> br0.202 (SVI) -> br0 -> vxlan0 (VNI 20102)
//   -> VXLAN tunnel -> External FRR vxlan0 -> br0 -> br0.202 (SVI)
//   -> vrf202 -> agnhost-ipvrf (172.27.102.2)
//
// External FRR Configuration:
//   - vrf202: Linux VRF bound to VNI 20102
//   - br0.202: 802.1Q sub-interface (SVI) for routing, master of vrf202
//   - Bridge VLAN/VNI mapping: VID 202 <-> VNI 20102
//   - BGP: Type-5 routes advertise 172.27.102.0/24 with RT 64512:20102
//
// Cluster Node Configuration:
//   - br0.202: SVI sub-interface bound to OVN-K's UDN VRF (mpX-udn-vrf)
//   - Bridge VLAN/VNI mapping: VID 202 <-> VNI 20102
//   - FRR-K8s: Receives Type-5 routes into VRF, advertises pod subnet
//
// VNI/VID: 20102/202
// Network: agnhost-ipvrf-net (172.27.102.0/24)
//
// =============================================================================
// Test Type 2: Layer 2 CUDN with MAC-VRF
// =============================================================================
//
// This test validates north-south L2 connectivity between pods on a Layer 2
// CUDN and an external server via EVPN Type-2 (MAC/IP) and Type-3 (IMET) routes.
//
// Data Path (Pod -> External agnhost-macvrf and vice versa):
//   Pod (10.100.0.x) -> OVN logical switch -> OVS internal port (evpn10100)
//   -> br0 (tagged VID 100) -> vxlan0 (VNI 10100)
//   -> VXLAN tunnel -> External FRR vxlan0 -> br0 (VID 100)
//   -> evpn10100 (access port) -> agnhost-macvrf (10.100.0.250)
//
// External FRR Configuration:
//   - br0: Linux bridge with VLAN filtering, no VRF (pure L2)
//   - evpn10100: Access port on br0 with PVID 100 (untagged)
//   - Bridge VLAN/VNI mapping: VID 100 <-> VNI 10100
//   - BGP: Type-2/Type-3 routes for MAC learning and BUM traffic
//
// Cluster Node Configuration:
//   - evpn10100: OVS internal port connected to OVN logical switch
//   - OVS port enslaved to br0 with VID 100 (access mode)
//   - Bridge VLAN/VNI mapping: VID 100 <-> VNI 10100
//   - FRR-K8s: EVPN Type-2/Type-3 for MAC advertisement
//
// VNI/VID: 10100/100
// Network: CUDN-network (10.100.0.0/16) - same subnet as the CUDN
//
// =============================================================================
// Test Type 3: Layer 2 CUDN with both MAC-VRF and IP-VRF
// =============================================================================
//
// This test combines both L2 and L3 EVPN connectivity on a single Layer 2 CUDN.
// Pods can reach:
//   - agnhost-macvrf via L2 (MAC-VRF, same subnet bridging)
//   - agnhost-ipvrf via L3 (IP-VRF, routed via SVI, reply can be assymmetric)
//
// This is the most complex scenario, requiring both:
//   - OVS port to br0 for L2 MAC-VRF traffic (VNI 10100)
//   - SVI (br0.202) bound to OVN-K VRF for L3 IP-VRF traffic (VNI 20102)
//
// External FRR Configuration:
//   - MAC-VRF: evpn10100 access port, VID 100 <-> VNI 10100
//   - IP-VRF: vrf202 + br0.202 SVI, VID 202 <-> VNI 20102
//   - BGP: Both Type-2/Type-3 (MAC-VRF) and Type-5 (IP-VRF) routes
//
// Cluster Node Configuration:
//   - MAC-VRF: OVS internal port evpn10100 -> br0 (VID 100)
//   - IP-VRF: br0.202 SVI -> mpX-udn-vrf (VID 202)
//   - FRR-K8s: Full EVPN config for both MAC-VRF and IP-VRF
//
// =============================================================================
// SVD (Single VXLAN Device) Mode
// =============================================================================
//
// All tests use SVD mode where a single vxlan0 device handles multiple VNIs
// via the vnifilter option. This is more efficient than creating separate
// VXLAN devices per VNI.
//
// vxlan0 configuration:
//   - external: Allows FDB programming for remote VTEPs
//   - vnifilter: Enables per-VLAN VNI mapping via bridge
//   - nolearning: Disables MAC learning (controlled by BGP EVPN)
//
// Bridge VLAN/VNI mapping commands:
//   bridge vlan add dev br0 vid <VID> self
//   bridge vlan add dev vxlan0 vid <VID>
//   bridge vni add dev vxlan0 vni <VNI>
//   bridge vlan add dev vxlan0 vid <VID> tunnel_info id <VNI>
//
// =============================================================================
// See OKEP: https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/okeps/okep-5088-evpn.md for more details.
