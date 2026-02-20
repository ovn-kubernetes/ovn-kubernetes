package kubevirt

import (
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/ndp"
)

const (
	// ARPProxyIPv4 is a randomly chosen IPv4 link-local address that kubevirt
	// pods will have as default gateway
	ARPProxyIPv4 = "169.254.1.1"

	// ARPProxyIPv6 is a randomly chosen IPv6 link-local address that kubevirt
	// pods will have as default gateway
	ARPProxyIPv6 = "fe80::1"

	// ARPProxyMAC is a generated mac from ARPProxyIPv4, it's generated with
	// the mechanism at `util.IPAddrToHWAddr`
	ARPProxyMAC = "0a:58:a9:fe:01:01"
)

// ComposeARPProxyLSPOption returns the "arp_proxy" field needed at router type
// LSP to implement stable default gw for pod ip migration, it consists of
// generated MAC address, a link local ipv4 and ipv6( it's the same
// for all the logical switches) and the cluster subnet to allow the migrated
// vm to ping pods for the same subnet.
// This is how it works step by step:
// For default gw:
//   - VM is configured with arp proxy IPv4/IPv6 as default gw
//   - when a VM access an address that do not belong to its subnet it will
//     send an ARP asking for the default gw IP
//   - This will reach the OVN flows from arp_proxy and answer back with the
//     mac address here
//   - The vm will send the packet with that mac address so it will en being
//     route by ovn.
//
// For vm accessing pods at the same subnet after live migration
//   - Since the dst address is at the same subnet it will
//     not use default gw and will send an ARP for dst IP
//   - The logical switch do not have any LSP with that address since
//     vm has being live migrated
//   - ovn will fallback to arp_proxy flows to resolve ARP (these flows have
//     less priority that LSPs ones so they don't collide with them)
//   - The ovn flow for the cluster wide CIDR will be hit and ovn will answer
//     back with arp_proxy mac
//   - VM will send the message to that mac and it will end being route by
//     ovn
func ComposeARPProxyLSPOption() string {
	arpProxy := []string{ARPProxyMAC, ARPProxyIPv4, ARPProxyIPv6}
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		arpProxy = append(arpProxy, clusterSubnet.CIDR.String())
	}
	return strings.Join(arpProxy, " ")
}

func notifyProxy(ipsToNotify []*net.IPNet) error {
	arpProxyHardwareAddr, err := net.ParseMAC(ARPProxyMAC)
	if err != nil {
		return err
	}
	for _, ipToNotify := range ipsToNotify {
		if ipToNotify.IP.To4() != nil {
			garp, err := util.NewGARP(ipToNotify.IP, &arpProxyHardwareAddr)
			if err != nil {
				klog.Errorf("kubevirt: failed creating GARP for IP %s: %v", ipToNotify.IP.String(), err)
				continue // ipv6
			}
			if err := util.BroadcastGARP(types.K8sMgmtIntfName, garp); err != nil {
				return fmt.Errorf("failed sending GARP: %w", err)
			}
		} else {
			na, err := ndp.NewNeighborAdvertisement(ipToNotify.IP, &arpProxyHardwareAddr)
			if err != nil {
				klog.Errorf("kubevirt: failed creating NA for IP %s: %v", ipToNotify.IP.String(), err)
				continue // ipv4
			}
			if err := ndp.SendUnsolicitedNeighborAdvertisement(types.K8sMgmtIntfName, na); err != nil {
				return fmt.Errorf("failed sending Unsolicited NA: %w", err)
			}
		}
	}
	return nil
}

// notifyDirectNeighbors sends GARPs/NAs with the real MAC addresses of
// same-subnet pods and the gateway. This is used when a VM migrates back to
// the node owning its subnet so the VM's neighbor cache is updated from
// ARP proxy MAC to the actual MACs, allowing direct L2 communication on the
// same switch.
func notifyDirectNeighbors(watchFactory *factory.WatchFactory, pod *corev1.Pod, podAnnotation *util.PodAnnotation, nadName string) error {
	nodeOwningSubnet, err := findNodeOwningSubnet(watchFactory, podAnnotation, nadName)
	if err != nil {
		return err
	}
	if nodeOwningSubnet == nil {
		return nil
	}

	pods, err := watchFactory.GetAllPods()
	if err != nil {
		return err
	}
	vmName := ExtractVMNameFromPod(pod)
	if vmName == nil {
		return fmt.Errorf("missing virtual machine name label at pod %s/%s", pod.Namespace, pod.Name)
	}
	for _, podToNotify := range pods {
		if podToNotify.Spec.NodeName != nodeOwningSubnet.Name {
			continue
		}
		peerAnnotation, err := util.UnmarshalPodAnnotation(podToNotify.Annotations, nadName)
		if err != nil {
			klog.Errorf("kubevirt: failed unmarshaling pod annotation for pod %s/%s: %v", podToNotify.Namespace, podToNotify.Name, err)
			continue
		}
		if util.PodCompleted(podToNotify) {
			continue
		}
		obtainedVMName := ExtractVMNameFromPod(podToNotify)
		if obtainedVMName != nil && *obtainedVMName == *vmName {
			continue
		}
		mac := peerAnnotation.MAC
		for _, ip := range peerAnnotation.IPs {
			if ip.IP.To4() != nil {
				garp, err := util.NewGARP(ip.IP, &mac)
				if err != nil {
					klog.Errorf("kubevirt: failed creating GARP for IP %s with MAC %s: %v", ip.IP, mac, err)
					continue
				}
				if err := util.BroadcastGARP(types.K8sMgmtIntfName, garp); err != nil {
					return fmt.Errorf("failed sending GARP: %w", err)
				}
			} else {
				na, err := ndp.NewNeighborAdvertisement(ip.IP, &mac)
				if err != nil {
					klog.Errorf("kubevirt: failed creating NA for IP %s with MAC %s: %v", ip.IP, mac, err)
					continue
				}
				if err := ndp.SendUnsolicitedNeighborAdvertisement(types.K8sMgmtIntfName, na); err != nil {
					return fmt.Errorf("failed sending Unsolicited NA: %w", err)
				}
			}
		}
	}

	// Send GARPs for gateway IPs with the LRP MAC so the VM's
	// gateway neighbor entry is updated from arp_proxy MAC to
	// the real LRP MAC on the subnet-owning switch.
	for _, gw := range podAnnotation.Gateways {
		gwMAC := util.IPAddrToHWAddr(gw)
		if gw.To4() != nil {
			garp, err := util.NewGARP(gw, &gwMAC)
			if err != nil {
				klog.Errorf("kubevirt: failed creating GARP for gateway IP %s: %v", gw, err)
				continue
			}
			if err := util.BroadcastGARP(types.K8sMgmtIntfName, garp); err != nil {
				return fmt.Errorf("failed sending gateway GARP: %w", err)
			}
		} else {
			na, err := ndp.NewNeighborAdvertisement(gw, &gwMAC)
			if err != nil {
				klog.Errorf("kubevirt: failed creating NA for gateway IP %s: %v", gw, err)
				continue
			}
			if err := ndp.SendUnsolicitedNeighborAdvertisement(types.K8sMgmtIntfName, na); err != nil {
				return fmt.Errorf("failed sending gateway Unsolicited NA: %w", err)
			}
		}
	}
	return nil
}

func deleteStaleLogicalSwitchPorts(watchFactory *factory.WatchFactory, nbClient libovsdbclient.Client, pod *corev1.Pod) error {
	vmPods, err := findVMRelatedPods(watchFactory, pod)
	if err != nil {
		return err
	}
	for _, vmPod := range vmPods {
		if util.PodCompleted(vmPod) {
			continue
		}
		// Only delete LSP for pods that are stale (not the current active VM pod)
		isStale, err := IsMigratedSourcePodStale(watchFactory, vmPod)
		if err != nil {
			klog.Errorf("kubevirt: failed checking if pod %s/%s is stale: %v", vmPod.Namespace, vmPod.Name, err)
			continue
		}
		if !isStale {
			continue
		}
		lspName := util.GetLogicalPortName(vmPod.Namespace, vmPod.Name)
		lsp := &nbdb.LogicalSwitchPort{
			Name: lspName,
		}
		lsp, err = libovsdbops.GetLogicalSwitchPort(nbClient, lsp)
		if err != nil {
			klog.Errorf("kubevirt: failed retrieving LSP %q: %v", lspName, err)
			continue
		}
		logicalSwitch := &nbdb.LogicalSwitch{
			Name: vmPod.Spec.NodeName,
		}
		if err := libovsdbops.DeleteLogicalSwitchPorts(nbClient, logicalSwitch, lsp); err != nil {
			return err
		}
	}
	return nil
}
