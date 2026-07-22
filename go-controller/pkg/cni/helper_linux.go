// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package cni

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/invoke"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/safchain/ethtool"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

const (
	// vmDHCPTimeout bounds each attempt of the one-shot VM DORA; nclient4
	// doubles it per retry, so with vmDHCPRetries attempts one exchange
	// phase waits at most 10s+20s = 30s, and a full DORA (DISCOVER +
	// REQUEST) at most 60s — half the 2-minute CNI request deadline
	// (kubeletDefaultCRIOperationTimeout), leaving room for the VFIO
	// rebind and interface teardown when no DHCP server answers. Both
	// values are pinned here so a library default change cannot silently
	// push the worst case past the request deadline.
	vmDHCPTimeout = 10 * time.Second
	vmDHCPRetries = 2
)

// dhcpLeaseMarkerDir holds the per-sandbox DHCP lease markers on DPU-host
// nodes, which have no host OVS to record them in. It lives in tmpfs, so the
// markers share the lifetime of the DHCP daemon's in-memory leases (both are
// cleared on reboot). It is a var so unit tests can point it at a tempdir.
var dhcpLeaseMarkerDir = "/var/run/ovn-kubernetes/dhcp-lease"

func addRoute(ipn *net.IPNet, gw net.IP, dev netlink.Link, mtu, table int) error {
	return util.GetNetLinkOps().RouteAdd(&netlink.Route{
		LinkIndex: dev.Attrs().Index,
		Scope:     netlink.SCOPE_UNIVERSE,
		Dst:       ipn,
		Gw:        gw,
		MTU:       mtu,
		Table:     table,
	})
}

func replaceRouteECMP(ipn *net.IPNet, gw net.IP, devs []netlink.Link, mtu int) error {
	ecmpRoute := &netlink.Route{
		Dst: ipn,
		MTU: mtu,
	}
	ecmpRoute.MultiPath = make([]*netlink.NexthopInfo, len(devs))
	for i, dev := range devs {
		ecmpRoute.MultiPath[i] = &netlink.NexthopInfo{
			LinkIndex: dev.Attrs().Index,
			Gw:        gw,
			Hops:      0, // Weight (0 means weight of 1)
		}
	}
	return util.GetNetLinkOps().RouteReplace(ecmpRoute)
}

// This is a good value that allows fast streams of small packets to be aggregated,
// without introducing noticeable latency in slower traffic.
const udpPacketAggregationTimeout = 50 * time.Microsecond

const (
	mlx5CoreDriver = "mlx5_core"
	vfioPCIDriver  = "vfio-pci"

	// pciVendorMellanox is the PCI vendor ID of NVIDIA/Mellanox NICs, as read
	// from the sysfs vendor attribute.
	pciVendorMellanox = "0x15b3"
)

// vfKernelDriverByVendor maps PCI vendor IDs to the kernel VF netdev driver
// used for the temporary VFIO→kernel driver handoff. Only vendors validated
// with this flow are listed — the handoff refuses unknown vendors before
// touching the driver binding (OKEP-6224: an unknown/unsupported NIC vendor
// must fail the CNI request).
var vfKernelDriverByVendor = map[string]string{
	pciVendorMellanox: mlx5CoreDriver, // NVIDIA/Mellanox ConnectX
}

// sysfs PCI roots; vars so unit tests can point them at a fake sysfs tree.
var (
	sysBusPCIDevices = "/sys/bus/pci/devices"
	sysBusPCIDrivers = "/sys/bus/pci/drivers"
)

var udpPacketAggregationTimeoutBytes = []byte(fmt.Sprintf("%d\n", udpPacketAggregationTimeout.Nanoseconds()))

// sets up the host side of a veth for UDP packet aggregation
func setupVethUDPAggregationHost(ifname string) error {
	e, err := ethtool.NewEthtool()
	if err != nil {
		return fmt.Errorf("failed to initialize ethtool: %v", err)
	}
	defer e.Close()

	err = e.Change(ifname, map[string]bool{
		"rx-gro":                true,
		"rx-udp-gro-forwarding": true,
	})
	if err != nil {
		return fmt.Errorf("could not enable interface features: %v", err)
	}
	channels, err := e.GetChannels(ifname)
	if err == nil {
		channels.RxCount = uint32(runtime.NumCPU())
		_, err = e.SetChannels(ifname, channels)
	}
	if err != nil {
		return fmt.Errorf("could not update channels: %v", err)
	}

	timeoutFile := fmt.Sprintf("/sys/class/net/%s/gro_flush_timeout", ifname)
	err = os.WriteFile(timeoutFile, udpPacketAggregationTimeoutBytes, 0644)
	if err != nil {
		return fmt.Errorf("could not set flush timeout: %v", err)
	}

	return nil
}

// sets up the container side of a veth for UDP packet aggregation
func setupVethUDPAggregationContainer(ifname string) error {
	e, err := ethtool.NewEthtool()
	if err != nil {
		return fmt.Errorf("failed to initialize ethtool: %v", err)
	}
	defer e.Close()

	channels, err := e.GetChannels(ifname)
	if err == nil {
		channels.TxCount = uint32(runtime.NumCPU())
		_, err = e.SetChannels(ifname, channels)
	}
	if err != nil {
		return fmt.Errorf("could not update channels: %v", err)
	}

	return nil
}

func renameLink(curName, newName string) error {
	link, err := util.GetNetLinkOps().LinkByName(curName)
	if err != nil {
		return err
	}

	if err := util.GetNetLinkOps().LinkSetDown(link); err != nil {
		return err
	}
	if err := util.GetNetLinkOps().LinkSetName(link, newName); err != nil {
		return err
	}
	if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
		return err
	}

	return nil
}

func setSysctl(sysctl string, newVal int) error {
	return os.WriteFile(sysctl, []byte(strconv.Itoa(newVal)), 0o640)
}

// safely move the netdev to the pod namespace, making sure to avoid name conflicts
func safeMoveIfToNetns(ifname string, netns ns.NetNS, containerID string) (newNetdeviceName string, err error) {
	newNetdeviceName = ifname
	err = moveIfToNetns(ifname, netns)

	if err != nil {
		if strings.Contains(err.Error(), "file exists") {
			// netdev with the same name exists in the pod
			newNetdeviceName = generateIfName(containerID)
			err = renameLink(ifname, newNetdeviceName)
			if err != nil {
				return ifname, err
			}
			err = moveIfToNetns(newNetdeviceName, netns)
			if err != nil {
				return ifname, err
			}
		} else {
			return ifname, err
		}
	}
	return newNetdeviceName, nil
}

func moveIfToNetns(ifname string, netns ns.NetNS) error {
	dev, err := util.GetNetLinkOps().LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("failed to lookup device %v: %q", ifname, err)
	}

	// move netdevice to ns
	if err = util.GetNetLinkOps().LinkSetNsFd(dev, int(netns.Fd())); err != nil {
		return fmt.Errorf("failed to move device %+v to netns: %q", ifname, err)
	}

	return nil
}

func setupNetwork(link netlink.Link, ifInfo *PodInterfaceInfo) error {
	// make sure link is up
	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to set up interface %s: %v", link.Attrs().Name, err)
		}
	}

	if ifInfo.SkipIPConfig {
		klog.Infof("Skipping network configuration for pod: %s", ifInfo.PodUID)
		return nil
	}

	// set the IP address
	for _, ip := range ifInfo.IPs {
		addr := &netlink.Addr{IPNet: ip}
		if err := util.GetNetLinkOps().AddrAdd(link, addr); err != nil {
			return fmt.Errorf("failed to add IP addr %s to %s: %v", ip, link.Attrs().Name, err)
		}
	}
	for _, gw := range ifInfo.Gateways {
		if err := addRoute(nil, gw, link, ifInfo.RoutableMTU, 0); err != nil {
			return fmt.Errorf("failed to add gateway route to link '%s': %v", link.Attrs().Name, err)
		}
	}

	// all pod links of the same secondary UDN up to the current one
	var links []netlink.Link
	var iptableNum int

	if len(ifInfo.PodIfNamesOfSameNAD) != 0 {
		// this is a pod with multiple interfaces of the same non-primary UDN, we need to configure the following sysctl configuration
		// stop ARP flux and stop RP filter drop from happening
		//   sysctl -w net.ipv4.conf.<linkName>.arp_ignore=1 # only reply to ARP requests if its sent to the IP on this interface
		//   sysctl -w net.ipv4.conf.<linkName>.arp_announce=2 # when sending ARP use the IP that belongs to this interface
		//   sysctl -w net.ipv4.conf.<linkName>.rp_filter=2 # allow reverse path filter check to pass if there is any route to the source
		linkName := link.Attrs().Name
		if err := setSysctl(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/arp_ignore", linkName), 1); err != nil {
			return fmt.Errorf("failed to set arp_ignore for %s: %v", linkName, err)
		}
		if err := setSysctl(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/arp_announce", linkName), 2); err != nil {
			return fmt.Errorf("failed to set arp_announce for %s: %v", linkName, err)
		}
		if err := setSysctl(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", linkName), 2); err != nil {
			return fmt.Errorf("failed to set rp_filter for %s: %v", linkName, err)
		}

		if len(ifInfo.Routes) != 0 {
			// needs ip table for each Pod interface of the same secondary UDN, table number start from 100
			iptableNum = 100 + link.Attrs().Index

			// ECMP routes are needed
			// get all the Pod interface names of the same nadName
			// up to the current link name. This is used for configuring ECMP routes via multiple pod interfaces
			links = make([]netlink.Link, 0, len(ifInfo.PodIfNamesOfSameNAD))
			for _, ifName := range ifInfo.PodIfNamesOfSameNAD {
				if ifName == link.Attrs().Name {
					// loop all pod interfaces of the same secondary UDN until the current pod interface
					links = append(links, link)
					break
				}
				ifLink, err := util.GetNetLinkOps().LinkByName(ifName)
				if err != nil {
					return fmt.Errorf("failed to lookup pod interface %s when setup route for link %s: %v", ifName, link.Attrs().Name, err)
				}
				links = append(links, ifLink)
			}

			// ip rules are needed to force traffic from an ip to egress the correct interface via a route table for that interface:
			//   ip rule add from <IP_on_this_interface> table <iptableNum>
			for _, ip := range ifInfo.IPs {
				rule := netlink.NewRule()
				rule.Src = util.GetIPNetFullMaskFromIP(ip.IP)
				rule.Table = iptableNum
				if err := util.GetNetLinkOps().RuleAdd(rule); err != nil {
					return fmt.Errorf("failed to add IP rule for %s to table %d: %v", ip.IP, iptableNum, err)
				}

				// add scope link route to the specific table
				route := &netlink.Route{
					LinkIndex: link.Attrs().Index,
					Scope:     netlink.SCOPE_LINK,
					Dst:       util.IPsToNetworkIPs(ip)[0],
					Table:     iptableNum,
				}
				if err := util.GetNetLinkOps().RouteAdd(route); err != nil {
					return fmt.Errorf("failed to add link scope route to %v via %v to table %v: %v", ip, linkName, iptableNum, err)
				}
			}
		}
	}

	for _, route := range ifInfo.Routes {
		if len(ifInfo.PodIfNamesOfSameNAD) == 0 {
			// if there is no other interface of the same NAD, just add route directly
			if err := addRoute(route.Dest, route.NextHop, link, ifInfo.RoutableMTU, 0); err != nil {
				return fmt.Errorf("failed to add pod route %v via %v: %v", route.Dest, route.NextHop, err)
			}
		} else {
			if len(links) == 1 {
				// if this is the first pod interface of this NAD, just add route directly
				if err := addRoute(route.Dest, route.NextHop, link, ifInfo.RoutableMTU, 0); err != nil {
					return fmt.Errorf("failed to add pod route %v via %v: %v", route.Dest, route.NextHop, err)
				}
			} else {
				// otherwise replace it with ECMP routes
				if err := replaceRouteECMP(route.Dest, route.NextHop, links, ifInfo.RoutableMTU); err != nil {
					return fmt.Errorf("failed to replace pod route %v via %v through links %v: %v", route.Dest, route.NextHop, links, err)
				}
			}

			// add ECMP route to specific IP route table
			if err := addRoute(route.Dest, route.NextHop, link, ifInfo.RoutableMTU, iptableNum); err != nil {
				return fmt.Errorf("failed to add pod route %v nexthop %v via %v table %v: %v", route.Dest, route.NextHop, link.Attrs().Name, iptableNum, err)
			}
		}
	}

	return nil
}

func setupInterface(netns ns.NetNS, containerID, ifName string, ifInfo *PodInterfaceInfo) (*current.Interface, *current.Interface, error) {
	hostIface := &current.Interface{}
	contIface := &current.Interface{}
	ifnameSuffix := ""

	var oldHostVethName string
	err := netns.Do(func(hostNS ns.NetNS) error {
		// create the veth pair in the container and move host end into host netns
		// set host interface name now for default network as it is already known; otherwise for secondary network,
		// host interface will be renamed later.
		if ifInfo.NetName == types.DefaultNetworkName {
			hostIface.Name = containerID[:15]
		} else {
			hostIface.Name = ""
		}
		contIface.Mac = ifInfo.MAC.String()
		hostVeth, containerVeth, err := ip.SetupVethWithName(ifName, hostIface.Name, ifInfo.MTU, contIface.Mac, hostNS)
		if err != nil {
			return err
		}
		hostIface.Mac = hostVeth.HardwareAddr.String()
		contIface.Name = containerVeth.Name

		link, err := util.GetNetLinkOps().LinkByName(contIface.Name)
		if err != nil {
			return fmt.Errorf("failed to lookup %s: %v", contIface.Name, err)
		}

		err = setupNetwork(link, ifInfo)
		if err != nil {
			return err
		}
		contIface.Sandbox = netns.Path()

		if ifInfo.EnableUDPAggregation {
			err = setupVethUDPAggregationContainer(contIface.Name)
			if err != nil {
				return fmt.Errorf("could not enable UDP packet aggregation in container: %v", err)
			}
		}

		oldHostVethName = hostVeth.Name

		// to generate the unique host interface name, postfix it with the podInterface index for non-default network
		if ifInfo.NetName != types.DefaultNetworkName {
			ifnameSuffix = fmt.Sprintf("_%d", containerVeth.Index)
		}

		// If we have the ipv6 gateway LLA then this is a primary layer2 UDN
		if len(ifInfo.GatewayIPv6LLA) > 0 {
			if err = setupIngressFilter(ifName, ifInfo.GatewayIPv6LLA.String()); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// rename the host end of veth pair for the secondary network
	if ifInfo.NetName != types.DefaultNetworkName {
		hostIface.Name = containerID[:(15-len(ifnameSuffix))] + ifnameSuffix
		if err := renameLink(oldHostVethName, hostIface.Name); err != nil {
			return nil, nil, fmt.Errorf("failed to rename %s to %s: %v", oldHostVethName, hostIface.Name, err)
		}
	}

	if ifInfo.EnableUDPAggregation {
		err = setupVethUDPAggregationHost(hostIface.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("could not enable UDP packet aggregation on host veth interface %q: %v", hostIface.Name, err)
		}
	}

	return hostIface, contIface, nil
}

// generate a unique interface name for the temporary netdev that will be moved to pod namespace
func generateIfName(containerID string) string {
	randomId := util.GenerateId(5) // random ID with 5 chars
	// ifname max length is 15
	return containerID[:(15-len(randomId))] + randomId
}

// Setup sriov interface in the pod
func setupSriovInterface(netns ns.NetNS, containerID, ifName string, ifInfo *PodInterfaceInfo, deviceID string, isVFIO bool) (*current.Interface, *current.Interface, error) {
	hostIface := &current.Interface{}
	contIface := &current.Interface{}
	netdevice := ifInfo.NetdevName

	// 0. init contIface for VFIO
	if isVFIO {
		if util.IsAuxDeviceName(deviceID) {
			return nil, nil, fmt.Errorf("VFIO not supported for device %s", deviceID)
		}
		// if the SR-IOV device is bound to VFIO, then there is nothing
		// to do as it will be passed to the KVM VM directly
		contIface.Name = ifName
		contIface.Mac = ifInfo.MAC.String()
		contIface.Sandbox = netns.Path()
	} else {
		// 1. Move netdevice to Container namespace
		if len(netdevice) != 0 {
			newNetdevName, err := safeMoveIfToNetns(netdevice, netns, containerID)
			if err != nil {
				return nil, nil, err
			}
			err = netns.Do(func(_ ns.NetNS) error {
				contIface.Name = ifName
				err = renameLink(newNetdevName, contIface.Name)
				if err != nil {
					return err
				}
				link, err := util.GetNetLinkOps().LinkByName(contIface.Name)
				if err != nil {
					return err
				}
				err = util.GetNetLinkOps().LinkSetHardwareAddr(link, ifInfo.MAC)
				if err != nil {
					return err
				}
				err = util.GetNetLinkOps().LinkSetMTU(link, ifInfo.MTU)
				if err != nil {
					return err
				}
				err = util.GetNetLinkOps().LinkSetUp(link)
				if err != nil {
					return err
				}

				err = setupNetwork(link, ifInfo)
				if err != nil {
					return err
				}

				contIface.Mac = ifInfo.MAC.String()
				contIface.Sandbox = netns.Path()

				return nil
			})
			if err != nil {
				return nil, nil, err
			}
		}
	}

	if !ifInfo.IsDPUHostMode {
		// 2. get device representor name
		hostRepName, err := util.GetFunctionRepresentorName(deviceID)
		if err != nil {
			return nil, nil, err
		}

		// 3. it's not possible to set mac address within container netns for VFIO case,
		// hence set it through VF representor (PCI device IDs only).
		if util.IsPCIDeviceName(deviceID) {
			if err := util.SetVFHardwreAddress(deviceID, ifInfo.MAC); err != nil {
				return nil, nil, err
			}
		}

		hostIface.Name = hostRepName
		link, err := util.GetNetLinkOps().LinkByName(hostIface.Name)
		if err != nil {
			return nil, nil, err
		}

		hostIface.Mac = link.Attrs().HardwareAddr.String()
		// Do not bring the representor up or set MTU here. In full mode the representor must be
		// added to br-int first, then brought up (and set MTU) in ConfigureOVS after the port
		// is added to br-int. This avoids a race where an old pod's CmdDel could bring down the
		// same VF representor and remove it from br-int after we brought the link up but before
		// we added it to br-int.
	}

	return hostIface, contIface, nil
}

func getPCIDeviceDriver(deviceID string) (string, error) {
	driverPath := filepath.Join(sysBusPCIDevices, deviceID, "driver")
	driverTarget, err := os.Readlink(driverPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read driver for PCI device %s: %w", deviceID, err)
	}
	return filepath.Base(driverTarget), nil
}

// getPCIDeviceVendor returns the device's PCI vendor ID as sysfs reports it
// (lowercase hex with a 0x prefix, e.g. "0x15b3").
func getPCIDeviceVendor(deviceID string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(sysBusPCIDevices, deviceID, "vendor"))
	if err != nil {
		return "", fmt.Errorf("failed to read PCI vendor of device %s: %w", deviceID, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// vfKernelDriverForDevice returns the kernel netdev driver matching the VF's
// hardware, detected from its PCI vendor ID. An unsupported vendor is an
// explicit error so the VFIO DHCP handoff fails cleanly before any driver
// unbind happens, instead of force-probing a driver that cannot serve the
// device (driver_override bypasses the kernel's device-ID matching).
func vfKernelDriverForDevice(deviceID string) (string, error) {
	vendor, err := getPCIDeviceVendor(deviceID)
	if err != nil {
		return "", err
	}
	driver, ok := vfKernelDriverByVendor[vendor]
	if !ok {
		return "", fmt.Errorf("cannot run DHCP on VFIO device %s: unsupported NIC vendor %s, "+
			"no kernel driver known for the DHCP driver handoff (supported: %s NVIDIA/Mellanox → %s)",
			deviceID, vendor, pciVendorMellanox, mlx5CoreDriver)
	}
	return driver, nil
}

func writePCIDeviceDriverOverride(deviceID, driver string) error {
	overridePath := filepath.Join(sysBusPCIDevices, deviceID, "driver_override")
	overrideValue := []byte(driver)
	if driver == "" {
		overrideValue = []byte("\n")
	}
	if err := os.WriteFile(overridePath, overrideValue, 0o644); err != nil {
		return fmt.Errorf("failed to write driver override for PCI device %s: %w", deviceID, err)
	}
	return nil
}

// readPCIDeviceDriverOverride returns the device's current driver_override
// value, "" when unset (sysfs reports "(null)") or when the device's sysfs
// entry does not exist.
func readPCIDeviceDriverOverride(deviceID string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(sysBusPCIDevices, deviceID, "driver_override"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read driver override of PCI device %s: %w", deviceID, err)
	}
	v := strings.TrimSpace(string(raw))
	if v == "(null)" {
		return "", nil
	}
	return v, nil
}

// healInterruptedVFIOHandoff returns the device to vfio-pci when a previous
// CNI ADD died mid driver-handoff. runLocalnetVFIODHCP keeps driver_override
// pinned to vfio-pci for the whole handoff as the pin lives in the kernel, so
// it survives an ovnkube-node crash which makes the interrupted state
// detectable: the override says vfio-pci while the device is bound elsewhere.
// Without this repair the retried ADD would read the crash-corrupted binding
// at its IsVFIO check and silently configure the VM's VF as a kernel netdev;
// the VM then fails on the missing /dev/vfio device and no retry converges.
func healInterruptedVFIOHandoff(deviceID string) error {
	currentDriver, err := getPCIDeviceDriver(deviceID)
	if err != nil {
		return err
	}
	if currentDriver == vfioPCIDriver {
		// already bound where it belongs — nothing to repair
		return nil
	}
	override, err := readPCIDeviceDriverOverride(deviceID)
	if err != nil {
		return err
	}
	if override != vfioPCIDriver {
		// no vfio pin: a regular kernel-bound VF (or no sysfs entry at all),
		// not an interrupted handoff
		return nil
	}
	klog.Warningf("Device %s is bound to %q but pinned to %s — repairing an interrupted VFIO DHCP handoff",
		deviceID, currentDriver, vfioPCIDriver)
	if err := dhcpOps.BindPCIDeviceDriver(deviceID, vfioPCIDriver); err != nil {
		return fmt.Errorf("failed to restore device %s to %s after an interrupted handoff: %w",
			deviceID, vfioPCIDriver, err)
	}
	// bindPCIDeviceDriver ends with the override cleared; restore the pin
	return writePCIDeviceDriverOverride(deviceID, vfioPCIDriver)
}

// bindPCIDeviceDriver rebinds a PCI device to the given driver via sysfs.
// Note: it always ends with driver_override cleared; callers that rely on a
// persistent pin (the VFIO DHCP handoff and its crash repair) re-write the
// pin after each call.
func bindPCIDeviceDriver(deviceID, driver string) error {
	currentDriver, err := getPCIDeviceDriver(deviceID)
	if err != nil {
		return err
	}
	if currentDriver == driver {
		return nil
	}

	if err := writePCIDeviceDriverOverride(deviceID, driver); err != nil {
		return err
	}

	if currentDriver != "" {
		unbindPath := filepath.Join(sysBusPCIDevices, deviceID, "driver", "unbind")
		if err := os.WriteFile(unbindPath, []byte(deviceID), 0o644); err != nil {
			_ = writePCIDeviceDriverOverride(deviceID, "")
			return fmt.Errorf("failed to unbind PCI device %s from driver %s: %w", deviceID, currentDriver, err)
		}
	}

	bindPath := filepath.Join(sysBusPCIDrivers, driver, "bind")
	if err := os.WriteFile(bindPath, []byte(deviceID), 0o644); err != nil {
		bindErr := fmt.Errorf("failed to bind PCI device %s to driver %s: %w", deviceID, driver, err)
		// The device was already unbound above; try to give it back to its
		// original driver rather than leaving it bound to nothing. The
		// override must point at the original driver for the bind to be
		// honored (e.g. vfio-pci only claims devices via driver_override).
		// Recovery failures ride along in the returned error: only the
		// caller can act on a device left bound to nothing.
		if currentDriver != "" {
			if rerr := writePCIDeviceDriverOverride(deviceID, currentDriver); rerr != nil {
				bindErr = errors.Join(bindErr, fmt.Errorf("failed to restore driver override to %s: %w", currentDriver, rerr))
			} else if rerr := os.WriteFile(filepath.Join(sysBusPCIDrivers, currentDriver, "bind"),
				[]byte(deviceID), 0o644); rerr != nil {
				bindErr = errors.Join(bindErr, fmt.Errorf("failed to restore PCI device to driver %s: %w", currentDriver, rerr))
			}
		}
		if oerr := writePCIDeviceDriverOverride(deviceID, ""); oerr != nil {
			bindErr = errors.Join(bindErr, oerr)
		}
		return bindErr
	}

	if err := writePCIDeviceDriverOverride(deviceID, ""); err != nil {
		return err
	}

	return nil
}

func (pr *PodRequest) shouldRunLocalnetVFIODriverHandoff() bool {
	return pr != nil &&
		pr.CNIConf != nil &&
		pr.CNIConf.DeviceID != "" &&
		pr.IsVFIO &&
		pr.netName != types.DefaultNetworkName &&
		pr.nadName != types.DefaultNetworkName &&
		pr.CNIConf.Topology == types.LocalnetTopology
}

func getPfEncapIP(ovsClient client.Client, deviceID string) (string, error) {
	var mapping string
	if ovsClient != nil {
		ovs, err := ovsops.GetOpenvSwitch(ovsClient)
		if err != nil {
			return "", fmt.Errorf("failed to get Open_vSwitch row: %v", err)
		}
		mapping = ovs.ExternalIDs["ovn-pf-encap-ip-mapping"]
	} else {
		stdout, err := ovsGet("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping")
		if err != nil {
			return "", fmt.Errorf("failed to get ovn-pf-encap-ip-mapping, error: %v", err)
		}
		mapping = stdout
	}
	return parsePfEncapIPMapping(mapping, deviceID)
}

func parsePfEncapIPMapping(mapping, deviceID string) (string, error) {
	if mapping == "" {
		return "", nil
	}
	encapIpMapping := map[string]string{}
	for _, entry := range strings.Split(mapping, ",") {
		tokens := strings.Split(entry, ":")
		if len(tokens) != 2 {
			return "", fmt.Errorf("bad ovn-pf-encap-ip-mapping config: %s", mapping)
		}
		encapIpMapping[tokens[0]] = tokens[1]
	}
	uplinkRepName, err := util.GetUplinkRepresentorName(deviceID)
	if err != nil {
		// FIXME(leih): unlikely to happen, treat this as a valid case and ignore for now.
		klog.Errorf("Failed to get uplink representor for VF PCI address %s: %v",
			deviceID, err)
		return "", nil
	}
	return encapIpMapping[uplinkRepName], nil
}

// getBrIntDatapathType returns the datapath type of br-int. Uses libovsdb when
// an OVS client is available, otherwise falls back to ovs-vsctl.
func getBrIntDatapathType(ovsClient client.Client) (string, error) {
	if ovsClient != nil {
		br, err := ovsops.GetBridge(ovsClient, "br-int")
		if err != nil {
			return "", fmt.Errorf("failed to get bridge br-int: %v", err)
		}
		return br.DatapathType, nil
	}
	return getDatapathType("br-int")
}

// deleteStalePodPorts removes any existing OVS ports on br-int whose
// Interface external_ids[iface-id] == ifaceID, except hostIfaceName itself
// (which may be a legitimate re-add by a restarted ovnkube-node).
func deleteStalePodPorts(ovsClient client.Client, ifaceID, hostIfaceName string) error {
	if ovsClient != nil {
		stale, err := ovsops.FindInterfacesWithPredicate(ovsClient, func(i *vswitchd.Interface) bool {
			return i.ExternalIDs["iface-id"] == ifaceID
		})
		if err != nil {
			return fmt.Errorf("failed to list interfaces with iface-id %s: %v", ifaceID, err)
		}
		for _, s := range stale {
			if s.Name == hostIfaceName {
				continue
			}
			if err := ovsops.DeletePortWithInterfaces(ovsClient, "br-int", s.Name); err != nil {
				klog.Warningf("Failed to delete stale OVS port %q with iface-id %q from br-int: %v",
					s.Name, ifaceID, err)
			}
		}
		return nil
	}
	names, _ := ovsFind("Interface", "name", "external-ids:iface-id="+ifaceID)
	for _, name := range names {
		if name == hostIfaceName {
			continue
		}
		if out, err := ovsExec("--with-iface", "del-port", "br-int", name); err != nil {
			klog.Warningf("Failed to delete stale OVS port %q with iface-id %q from br-int: %v\n %q",
				name, ifaceID, err, out)
		}
	}
	return nil
}

// getExistingIfaceMeta returns the iface-id and effective NAD key recorded in
// the named Interface's external_ids, or (_, _, false) if the interface does
// not exist. An absent NADExternalID is mapped to types.DefaultNetworkName so
// callers can compare directly against ifInfo.NADKey.
func getExistingIfaceMeta(ovsClient client.Client, name string) (string, string, bool, error) {
	if ovsClient != nil {
		existing, err := ovsops.GetOVSInterface(ovsClient, name)
		if err != nil {
			if errors.Is(err, client.ErrNotFound) {
				return "", "", false, nil
			}
			return "", "", false, err
		}
		nadKey := existing.ExternalIDs[types.NADExternalID]
		if nadKey == "" {
			nadKey = types.DefaultNetworkName
		}
		return existing.ExternalIDs["iface-id"], nadKey, true, nil
	}
	extIds, err := ovsFind("Interface", "external_ids", "name="+name)
	if err != nil || len(extIds) != 1 {
		return "", "", false, nil
	}
	nadKey := util.GetExternalIDValByKey(extIds[0], types.NADExternalID)
	if nadKey == "" {
		nadKey = types.DefaultNetworkName
	}
	return util.GetExternalIDValByKey(extIds[0], "iface-id"), nadKey, true, nil
}

// addOrUpdatePodPort creates or updates the OVS port and its single backing
// Interface on br-int for the given pod iface. extIDs becomes the Interface
// external_ids without removing keys owned by other components. On the default
// network, stripDefaultNetIDs=true explicitly removes NetworkExternalID and
// NADExternalID left by a previous owner. If useDPDK is true the Interface
// type is set to dpdk with mtu_request=mtu.
func addOrUpdatePodPort(ovsClient client.Client, hostIfaceName string,
	extIDs map[string]string, useDPDK bool, mtu int, stripDefaultNetIDs bool) error {
	if ovsClient != nil {
		iface := &vswitchd.Interface{ExternalIDs: extIDs}
		if useDPDK {
			m := mtu
			iface.Type = "dpdk"
			iface.MTURequest = &m
		}
		port := &vswitchd.Port{OtherConfig: map[string]string{"transient": "true"}}
		ops, err := ovsops.CreateOrUpdatePodPortOps(ovsClient, nil, "br-int", hostIfaceName, port, iface)
		if err != nil {
			return fmt.Errorf("failed to build operations for plugging pod interface: %v", err)
		}
		if stripDefaultNetIDs {
			ops, err = ovsops.RemoveOVSInterfaceExternalIDsOps(ovsClient, ops, hostIfaceName, types.NetworkExternalID, types.NADExternalID)
			if err != nil {
				return fmt.Errorf("failed to clear default-network external IDs from interface %s: %v", hostIfaceName, err)
			}
		}
		if _, err := ovsops.TransactAndCheck(ovsClient, ops); err != nil {
			return fmt.Errorf("failure in plugging pod interface: %v", err)
		}
		return nil
	}
	args := []string{
		"--may-exist", "add-port", "br-int", hostIfaceName, "other_config:transient=true",
		"--", "set", "interface", hostIfaceName,
	}
	// Walk extIDs in the canonical ovs-vsctl arg order that this CNI has
	// historically used, so the shell call is stable across runs and matches
	// any test expectations.
	for _, k := range []string{
		"attached_mac",
		"iface-id",
		"iface-id-ver",
		"sandbox",
		"pod-if-name",
		"encap-ip",
		"ip_addresses",
		"vf-netdev-name",
		"vf-is-vfio",
		types.NetworkExternalID,
		types.NADExternalID,
	} {
		if v, ok := extIDs[k]; ok {
			args = append(args, fmt.Sprintf("external_ids:%s=%s", k, v))
		}
	}
	if useDPDK {
		args = append(args, "type=dpdk", fmt.Sprintf("mtu_request=%v", mtu))
	}
	if stripDefaultNetIDs {
		args = append(args, "--", "--if-exists", "remove", "interface", hostIfaceName, "external_ids", types.NetworkExternalID)
		args = append(args, "--", "--if-exists", "remove", "interface", hostIfaceName, "external_ids", types.NADExternalID)
	}
	if out, err := ovsExec(args...); err != nil {
		return fmt.Errorf("failure in plugging pod interface: %v\n  %q", err, out)
	}
	return nil
}

// ConfigureOVS performs OVS configurations in order to set up Pod networking.
// When ovsClient is non-nil (ovnkube-node privileged-mode path) it uses
// libovsdb; when nil (unprivileged CNI shim path) it falls back to ovs-vsctl
// shell-outs.
func ConfigureOVS(ctx context.Context, ovsClient client.Client, namespace, podName, podIfName, hostIfaceName string,
	ifInfo *PodInterfaceInfo, sandboxID, deviceID string, isVFIO bool, getter PodInfoGetter) error {

	ifaceID := util.GetIfaceId(namespace, podName)
	if ifInfo.NetName != types.DefaultNetworkName {
		ifaceID = util.GetUDNIfaceId(namespace, podName, ifInfo.NADKey)
	}
	initialPodUID := ifInfo.PodUID
	ipStrs := make([]string, len(ifInfo.IPs))
	for i, ip := range ifInfo.IPs {
		ipStrs[i] = ip.String()
	}

	dpType, err := getBrIntDatapathType(ovsClient)
	if err != nil {
		return err
	}

	klog.Infof("ConfigureOVS: namespace: %s, podName: %s, hostIfaceName: %s, network: %s, NAD %s, SandboxID: %q, PCI device ID: %s, UID: %q, MAC: %s, IPs: %v",
		namespace, podName, hostIfaceName, ifInfo.NetName, ifInfo.NADKey, sandboxID, deviceID, initialPodUID, ifInfo.MAC, ipStrs)

	// Find and remove any existing OVS port with this iface-id. Pods can
	// have multiple sandboxes if some are waiting for garbage collection,
	// but only the latest one should have the iface-id set.
	if err := deleteStalePodPorts(ovsClient, ifaceID, hostIfaceName); err != nil {
		return err
	}

	// if the specified port was created for other Pod/NAD, return error
	if existingIfaceID, existingNADKey, exists, err := getExistingIfaceMeta(ovsClient, hostIfaceName); err != nil {
		return err
	} else if exists {
		if existingIfaceID != ifaceID {
			return fmt.Errorf("OVS port %s was added for iface-id (%s), now readding it for (%s)", hostIfaceName, existingIfaceID, ifaceID)
		}
		if existingNADKey != ifInfo.NADKey {
			return fmt.Errorf("OVS port %s was added for NAD (%s), expect (%s)", hostIfaceName, existingNADKey, ifInfo.NADKey)
		}
	}

	// Add the new sandbox's OVS port, tag the port as transient so stale
	// pod ports are scrubbed on hard reboot
	extIDs := map[string]string{
		"attached_mac": ifInfo.MAC.String(),
		"iface-id":     ifaceID,
		"iface-id-ver": initialPodUID,
		"sandbox":      sandboxID,
	}

	// pod interface name, used to identify CNI request with the same NAD
	if podIfName != "" {
		extIDs["pod-if-name"] = podIfName
	}

	// In case of multi-vtep, host has multipe NICs and each NIC has a VTEP interface, the mapping
	// of VTEP IP to NIC is stored in Open_vSwitch table's `external_ids:ovn-pf-encap-ip-mapping`,
	// the value's format is:
	//   enp1s0f0:<vtep-ip1>,enp193s0f0:<vtep-ip2>,enp197s0f0:<vtep-ip3>
	// Here configure the OVS Interface's encap-ip according to the mapping.
	if deviceID != "" {
		encapIP, err := getPfEncapIP(ovsClient, deviceID)
		if err != nil {
			return err
		}
		if len(encapIP) > 0 {
			extIDs["encap-ip"] = encapIP
		}
	}

	// IPAM is optional for secondary flatL2 networks; thus, the ifaces may not
	// have IP addresses.
	if len(ifInfo.IPs) > 0 {
		extIDs["ip_addresses"] = strings.Join(ipStrs, ",")
	}

	useDPDK := false
	if dpType == types.DatapathUserspace {
		if _, err := util.GetSriovnetOps().GetRepresentorPortFlavour(hostIfaceName); err != nil {
			// The error is not important: the given port is not a switchdev one and won't
			// be used with DPDK. It can happen for legitimate reason. Keep a trace of the
			// event and continue configuring OVS.
			klog.Infof("Port %s cannot be used with DPDK, will use netlink interface in OVS",
				hostIfaceName)
		} else {
			useDPDK = true
		}
	}

	if len(ifInfo.NetdevName) != 0 {
		// NOTE: For SF representor same external_id is used due to https://github.com/ovn-kubernetes/ovn-kubernetes/pull/3054
		// Review this line when upgrade mechanism will be implemented
		extIDs["vf-netdev-name"] = ifInfo.NetdevName
	}
	if isVFIO {
		// VFIO case
		extIDs["vf-is-vfio"] = "true"
	}

	stripDefaultNetIDs := false
	if ifInfo.NetName != types.DefaultNetworkName {
		extIDs[types.NetworkExternalID] = ifInfo.NetName
		extIDs[types.NADExternalID] = ifInfo.NADKey
	} else {
		// On the default network NetworkExternalID/NADExternalID must not
		// be set; for the shell path this requires explicit removes.
		stripDefaultNetIDs = true
	}

	if err := addOrUpdatePodPort(ovsClient, hostIfaceName, extIDs, useDPDK, ifInfo.MTU, stripDefaultNetIDs); err != nil {
		return err
	}

	if err := clearPodBandwidth(ovsClient, sandboxID); err != nil {
		return err
	}

	var link netlink.Link
	if deviceID != "" || (ifInfo.Ingress > 0 || ifInfo.Egress > 0) {
		if link, err = util.GetNetLinkOps().LinkByName(hostIfaceName); err != nil {
			return fmt.Errorf("failed to find interface %s: %v", hostIfaceName, err)
		}
	}

	if deviceID != "" {
		// 4. set MTU on the representor
		if err = util.GetNetLinkOps().LinkSetMTU(link, ifInfo.MTU); err != nil {
			return fmt.Errorf("failed to set MTU on %s: %v", hostIfaceName, err)
		}
		// 5. if the interface is not up, set it to up
		err = util.GetNetLinkOps().LinkSetUp(link)
		if err != nil {
			return fmt.Errorf("failed to set link UP on %s: %v", hostIfaceName, err)
		}
	}

	if ifInfo.Ingress > 0 || ifInfo.Egress > 0 {
		err = netlink.LinkSetTxQLen(link, 1000)
		if err != nil {
			return fmt.Errorf("failed to set host veth txqlen: %v", err)
		}

		if err := setPodBandwidth(sandboxID, hostIfaceName, ifInfo.Ingress, ifInfo.Egress); err != nil {
			return err
		}
	}

	if err := waitForPodInterface(ctx, ovsClient, ifInfo, hostIfaceName, ifaceID, getter,
		namespace, podName, initialPodUID); err != nil {
		// Ensure the error shows up in node logs, rather than just
		// being reported back to the runtime.
		klog.Warningf("[%s/%s %s] pod uid %s: %v", namespace, podName, sandboxID, initialPodUID, err)
		return err
	}
	return nil
}

type PodRequestInterfaceOps interface {
	ConfigureInterface(pr *PodRequest, ovsClient client.Client, getter PodInfoGetter, ifInfo *PodInterfaceInfo) ([]*current.Interface, error)
	UnconfigureInterface(pr *PodRequest, ifInfo *PodInterfaceInfo) error
}

type defaultPodRequestInterfaceOps struct{}

var podRequestInterfaceOps PodRequestInterfaceOps = &defaultPodRequestInterfaceOps{}

// ConfigureInterface sets up the container interface
func (*defaultPodRequestInterfaceOps) ConfigureInterface(pr *PodRequest, ovsClient client.Client, getter PodInfoGetter, ifInfo *PodInterfaceInfo) ([]*current.Interface, error) {
	netns, err := ns.GetNS(pr.Netns)
	if err != nil {
		return nil, fmt.Errorf("failed to open netns %q: %v", pr.Netns, err)
	}
	defer netns.Close()

	var hostIface, contIface *current.Interface

	klog.V(5).Infof("CNI Conf %v", pr.CNIConf)
	if pr.CNIConf.DeviceID != "" {
		// SR-IOV Case
		hostIface, contIface, err = setupSriovInterface(netns, pr.SandboxID, pr.IfName, ifInfo, pr.CNIConf.DeviceID, pr.IsVFIO)
	} else {
		if ifInfo.IsDPUHostMode {
			return nil, fmt.Errorf("unexpected configuration, pod request on dpu host. " +
				"device ID must be provided")
		}

		// General case
		hostIface, contIface, err = setupInterface(netns, pr.SandboxID, pr.IfName, ifInfo)
	}
	if err != nil {
		return nil, err
	}

	if !ifInfo.IsDPUHostMode {
		err = ConfigureOVS(pr.ctx, ovsClient, pr.PodNamespace, pr.PodName, pr.IfName, hostIface.Name, ifInfo, pr.SandboxID, pr.CNIConf.DeviceID, pr.IsVFIO, getter)
		if err != nil {
			pr.deletePort(hostIface.Name, pr.PodNamespace, pr.PodName)
			return nil, err
		}
	}

	// Only configure IPv6 specific stuff and wait for addresses to become usable
	// if there are any IPv6 addresses to assign. v4 doesn't have the concept
	// of tentative addresses so it doesn't need any of this.
	haveV6 := false
	for _, ip := range ifInfo.IPs {
		if ip.IP.To4() == nil {
			haveV6 = true
			break
		}
	}
	if haveV6 && !pr.IsVFIO {
		err = netns.Do(func(_ ns.NetNS) error {
			// deny IPv6 neighbor solicitations
			dadSysctlIface := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/dad_transmits", contIface.Name)
			if _, err := os.Stat(dadSysctlIface); !os.IsNotExist(err) {
				err = setSysctl(dadSysctlIface, 0)
				if err != nil {
					klog.Warningf("Failed to disable IPv6 DAD: %q", err)
				}
			}
			// generate address based on EUI64
			genSysctlIface := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/addr_gen_mode", contIface.Name)
			if _, err := os.Stat(genSysctlIface); !os.IsNotExist(err) {
				err = setSysctl(genSysctlIface, 0)
				if err != nil {
					klog.Warningf("Failed to set IPv6 address generation mode to EUI64: %q", err)
				}
			}

			return ip.SettleAddresses(contIface.Name, 10*time.Second)
		})
		if err != nil {
			klog.Warningf("Failed to settle addresses: %q", err)
		}
	}

	return []*current.Interface{hostIface, contIface}, nil
}

func (*defaultPodRequestInterfaceOps) UnconfigureInterface(pr *PodRequest, ifInfo *PodInterfaceInfo) error {
	podDesc := fmt.Sprintf("for pod %s/%s NAD %s", pr.PodNamespace, pr.PodName, pr.nadName)
	klog.V(5).Infof("Tear down interface (%+v) %s", *pr, podDesc)
	if ifInfo.IsDPUHostMode {
		if pr.CNIConf.DeviceID == "" {
			klog.Warningf("Unexpected configuration %s, pod request on DPU host. device ID must be provided", podDesc)
			return nil
		}
		// nothing else to do in DPUHostMode for VFIO device
		if pr.IsVFIO {
			return nil
		}
		// in the case of VF, we need to rename the container interface to VF name and move it to host
	}

	ifnameSuffix := ""
	isSecondary := pr.netName != types.DefaultNetworkName
	// nothing needs to be done for the VFIO case in the container namespace
	if !pr.IsVFIO {
		netns, err := ns.GetNS(pr.Netns)
		if err != nil {
			return fmt.Errorf("failed to get container namespace %s: %v", podDesc, err)
		}
		defer netns.Close()

		hostNS, err := ns.GetCurrentNS()
		if err != nil {
			return fmt.Errorf("failed to get host namespace %s: %v", podDesc, err)
		}
		defer hostNS.Close()

		// 1. For SRIOV case, we'd need to move device from container namespace back to the host namespace
		// 2. If it is secondary network and not dpu-host mode, then get the container interface index
		//    so that we know the host-side interface name.
		err = netns.Do(func(_ ns.NetNS) error {
			// container side interface deletion
			link, err := util.GetNetLinkOps().LinkByName(pr.IfName)
			if err != nil {
				return fmt.Errorf("failed to get container interface %s %s: %v", pr.IfName, podDesc, err)
			}
			if pr.CNIConf.DeviceID != "" {
				// SR-IOV Case
				err = util.GetNetLinkOps().LinkSetDown(link)
				if err != nil {
					return fmt.Errorf("failed to bring down container interface %s %s: %v", pr.IfName, podDesc, err)
				}
				// rename netdevice to make sure it is unique in the host namespace:
				// if original name of netdevice is empty, sandbox id and a '0' letter prefix is used to make up the unique name.
				oldName := ifInfo.NetdevName
				if oldName == "" {
					id := fmt.Sprintf("_0%d", link.Attrs().Index)
					oldName = pr.SandboxID[:(15-len(id))] + id
				}
				err = util.GetNetLinkOps().LinkSetName(link, oldName)
				if err != nil {
					return fmt.Errorf("failed to rename container interface %s to %s %s: %v",
						pr.IfName, oldName, podDesc, err)
				}
				// move netdevice to host netns
				err = util.GetNetLinkOps().LinkSetNsFd(link, int(hostNS.Fd()))
				if err != nil {
					return fmt.Errorf("failed to move container interface %s back to host namespace %s: %v",
						pr.IfName, podDesc, err)
				}
			}
			if isSecondary {
				ifnameSuffix = fmt.Sprintf("_%d", link.Attrs().Index)
			}
			return nil
		})
		if err != nil {
			klog.Errorf("Error in UnconfigureInterface: %v", err)
		}
	}

	if !ifInfo.IsDPUHostMode {
		var err error
		// host side interface deletion
		var hostIfName string
		if !util.IsNetworkSegmentationSupportEnabled() || isSecondary {
			// this is a secondary network (not primary) or segmentation is not enabled
			hostIfName = pr.SandboxID[:(15-len(ifnameSuffix))] + ifnameSuffix
		}
		if pr.CNIConf.DeviceID != "" {
			hostIfName, err = util.GetFunctionRepresentorName(pr.CNIConf.DeviceID)
			if err != nil {
				klog.Errorf("Failed to get the representor name for DeviceID %s for pod %s: %v",
					pr.CNIConf.DeviceID, podDesc, err)
			}
		}
		portList, err := ovsFind("interface", "name", "external-ids:sandbox="+pr.SandboxID)
		if err != nil {
			return fmt.Errorf("failed to list interfaces in OVS during delete for sandbox: %s, err: %w",
				pr.SandboxID, err)
		}
		// hostIfName is not empty if using device ID, a secondary network, or segmentation not enabled
		// delete the port in traditional fashion
		if hostIfName != "" {
			pr.deletePort(hostIfName, pr.PodNamespace, pr.PodName)
		} else {
			// this is a primary interface deletion and segmentation is enabled, delete all ports
			// delete happens in reverse order for attached networks, so this is the final deletion
			// In other words we dont have to worry about accidentally deleting a secondary network interface at
			// this point.
			if len(portList) > 1 {
				klog.V(5).Infof("Removing multiple interfaces for primary network segmentation (%+v) %s: %s",
					*pr, podDesc, strings.Join(portList, ","))
			}
			pr.deletePorts(portList, pr.PodNamespace, pr.PodName)
		}
		err = clearPodBandwidthForPorts(portList, pr.SandboxID)
		if err != nil {
			klog.Errorf("Failed to clearPodBandwidth sandbox %v %s: %v", pr.SandboxID, podDesc, err)
		}
		pr.deletePodConntrack()
	}
	return nil
}

func (pr *PodRequest) deletePodConntrack() {
	if pr.CNIConf.PrevResult == nil {
		return
	}
	result, err := current.NewResultFromResult(pr.CNIConf.PrevResult)
	if err != nil {
		klog.Warningf("Could not convert result to current version: %v", err)
		return
	}

	for _, ip := range result.IPs {
		// Skip known non-sandbox interfaces
		if ip.Interface != nil {
			intIdx := *ip.Interface
			if intIdx >= 0 &&
				intIdx < len(result.Interfaces) && result.Interfaces[intIdx].Sandbox == "" {
				continue
			}
		}
		_, err = util.DeleteConntrack(ip.Address.IP.String(), 0, "", netlink.ConntrackReplyAnyIP, nil)
		if err != nil {
			klog.Errorf("Failed to delete Conntrack Entry for %s: %v", ip.Address.IP.String(), err)
			continue
		}
	}
}

func (pr *PodRequest) deletePort(ifaceName, podNamespace, podName string) {
	podDesc := fmt.Sprintf("%s/%s", podNamespace, podName)

	var isVFDevice bool
	link, err := util.GetNetLinkOps().LinkByName(ifaceName)
	if err != nil {
		klog.Warningf("Failed to find host-side link %s for pod %q: %v", ifaceName, podDesc, err)
	} else if pr.CNIConf.DeviceID != "" {
		isVFDevice = true
	}

	if isVFDevice {
		// SR-IOV case: bring down the representor before removing from OVS.
		// The device plugin can re-allocate a VF to a new pod before this
		// CmdDel completes, causing the new pod's CmdAdd shim to run
		// concurrently. By doing LinkSetDown before del-port, we eliminate
		// the window where a racing CmdAdd could have its LinkSetUp or
		// ConfigureOVS undone.
		if err = util.GetNetLinkOps().LinkSetDown(link); err != nil {
			klog.Warningf("Failed to bring down pod %q interface %s: %v", podDesc, ifaceName, err)
		}
	}

	out, err := ovsExec("del-port", "br-int", ifaceName)
	if err != nil && !strings.Contains(err.Error(), "no port named") {
		// DEL should be idempotent; don't return an error just log it
		klog.Warningf("Failed to delete pod %q OVS port %s: %v\n  %q", podDesc, ifaceName, err, string(out))
	}

	// skip deleting representor ports
	if link != nil && !isVFDevice {
		if err = util.LinkDelete(ifaceName); err != nil {
			klog.Warningf("Failed to delete pod %q interface %s: %v", podDesc, ifaceName, err)
		}
	}
}

func (pr *PodRequest) deletePorts(ifaces []string, podNamespace, podName string) {
	for _, iface := range ifaces {
		pr.deletePort(iface, podNamespace, podName)
	}
}

// setupIngressFilter sets up an ingress filter using nftables to block
// unwanted ICMPv6 Router Advertisement (RA) packets on a specific device.
// It creates a new nftables table, chain, and rule to drop RA packets
// that do not match the specified gateway link-local address (gwLLA) and
// have a Router Advertisement (RA) lifetime not equal to 0.
//
// The nftables rule created by this function looks like:
// `icmpv6 type nd-router-advert ip6 saddr != <gwLLA> @th,48,16 != 0 drop`
//
// Parameters:
//   - ifaceName: The name of the network interface where the ingress filter
//     should be applied.
//   - gwLLA: The gateway link-local address to match against in the RA packets.
//
// Returns:
// - error: An error if the nftables setup fails, otherwise nil.
func setupIngressFilter(ifaceName, gwLLA string) error {
	const (
		tableName   = "ingress_filter" // Name of the nftables table to be created
		rasLifetime = "@th,48,16"      // Offset for RA lifetime field in ICMPv6 packets
	)

	// Initialize a new nftables table for the ingress filter
	nft, err := knftables.New(knftables.NetDevFamily, tableName)
	if err != nil {
		return fmt.Errorf("failed to initialize table: %w", err)
	}

	// Delegate the setup of the filter with the specified table and lifetime matcher
	return setupIngressFilterWithTableAndLifetimeMatcher(nft, ifaceName, gwLLA, rasLifetime)
}

func setupIngressFilterWithTableAndLifetimeMatcher(nft knftables.Interface, ifaceName, gwLLA, rasLifetime string) error {
	const (
		chainName = "input"
	)

	tx := nft.NewTransaction()

	tx.Add(&knftables.Table{})

	tx.Add(&knftables.Chain{
		Name:     chainName,
		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.IngressHook),
		Priority: knftables.PtrTo(knftables.FilterPriority),
		Device:   knftables.PtrTo(ifaceName),
	})

	tx.Add(&knftables.Rule{
		Chain: chainName,
		Rule: knftables.Concat(
			"icmpv6", "type", "nd-router-advert", "ip6", "saddr", "!=", gwLLA, rasLifetime, "!=", "0", "drop",
		),
	})

	// Execute the transaction
	if err := nft.Run(context.Background(), tx); err != nil {
		return fmt.Errorf("could not update netdev nftables: %v", err)
	}

	return nil

}

// DHCPOps abstracts the DHCP IPAM operations so unit tests can stub them
// without exec'ing the dhcp plugin binary, opening raw DHCP sockets, or
// entering a network namespace.
type DHCPOps interface {
	ExecAdd(pr *PodRequest) (*current.Result, error)
	ExecDel(pr *PodRequest) error
	DoOneShot(pr *PodRequest) (*current.Result, error)
	ApplyResult(netnsPath, ifName string, dhcpResult *current.Result) error
	BindPCIDeviceDriver(deviceID, driver string) error
}

type defaultDHCPOps struct{}

var dhcpOps DHCPOps = &defaultDHCPOps{}

func (*defaultDHCPOps) ExecAdd(pr *PodRequest) (*current.Result, error)   { return pr.execDHCPAdd() }
func (*defaultDHCPOps) ExecDel(pr *PodRequest) error                      { return pr.execDHCPDel() }
func (*defaultDHCPOps) DoOneShot(pr *PodRequest) (*current.Result, error) { return pr.doOneShotDHCP() }
func (*defaultDHCPOps) ApplyResult(netnsPath, ifName string, dhcpResult *current.Result) error {
	return applyDHCPResult(netnsPath, ifName, dhcpResult)
}
func (*defaultDHCPOps) BindPCIDeviceDriver(deviceID, driver string) error {
	return bindPCIDeviceDriver(deviceID, driver)
}

// dhcpPluginExec overrides the exec environment handed to the CNI invoke API
// for the delegated dhcp plugin; nil selects the library default. Unit tests
// inject a fake to avoid exec'ing the plugin binary.
var dhcpPluginExec invoke.Exec

// findDHCPPlugin locates the dhcp plugin binary in the CNI path, honoring the
// injected exec in tests.
func findDHCPPlugin(cniPath string) (string, error) {
	if dhcpPluginExec != nil {
		return dhcpPluginExec.FindInPath(types.IPAMTypeDHCP, filepath.SplitList(cniPath))
	}
	return invoke.FindInPath(types.IPAMTypeDHCP, filepath.SplitList(cniPath))
}

// execDHCPAdd delegates to the DHCP IPAM plugin to obtain IP configuration
// for the pod interface, applies the result (IPs, routes, gateway) inside
// the container netns, and returns the DHCP result for the caller to merge
// into the CNI result and report via the pod-networks annotation.
//
// This runs on the server side (ovnkube-node) in privileged mode.
// Unlike the shim, the server does not inherit kubelet's CNI_* env vars,
// so we use invoke.Args to explicitly provide them.
func (pr *PodRequest) execDHCPAdd() (*current.Result, error) {
	dhcpConfBytes, err := pr.buildDHCPConf()
	if err != nil {
		return nil, err
	}

	cniPath := getCNIPath()
	pluginPath, err := findDHCPPlugin(cniPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find dhcp plugin in CNI_PATH (%s): %v", cniPath, err)
	}

	// Build explicit CNI args since the server doesn't have kubelet's env
	cniArgs := &invoke.Args{
		Command:     "ADD",
		ContainerID: pr.SandboxID,
		NetNS:       pr.Netns,
		IfName:      pr.IfName,
		Path:        cniPath,
	}

	ipamResult, err := invoke.ExecPluginWithResult(pr.ctx, pluginPath, dhcpConfBytes, cniArgs, dhcpPluginExec)
	if err != nil {
		return nil, fmt.Errorf("DHCP plugin ADD failed: %v", err)
	}

	dhcpResult, err := current.GetResult(ipamResult)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DHCP result: %v", err)
	}

	if len(dhcpResult.IPs) == 0 {
		return nil, fmt.Errorf("DHCP returned no IP addresses")
	}

	pr.filterDHCPDefaultRoutes(dhcpResult)

	klog.Infof("DHCP IPAM for pod %s/%s on %s: IPs=%v",
		pr.PodNamespace, pr.PodName, pr.IfName, dhcpResult.IPs)

	// Apply DHCP-obtained IPs and routes to the interface inside the container netns.
	if err := dhcpOps.ApplyResult(pr.Netns, pr.IfName, dhcpResult); err != nil {
		// On failure, release the DHCP lease
		delArgs := &invoke.Args{
			Command:     "DEL",
			ContainerID: pr.SandboxID,
			NetNS:       pr.Netns,
			IfName:      pr.IfName,
			Path:        cniPath,
		}
		// Best-effort rollback: the ADD is failing regardless, but an
		// unreleased lease outlives it (the daemon keeps renewing until the
		// retried ADD's DEL or the TTL), so leave a trace when the release
		// also fails.
		if delErr := invoke.ExecPluginWithoutResult(pr.ctx, pluginPath, dhcpConfBytes, delArgs, dhcpPluginExec); delErr != nil {
			klog.Warningf("DHCP: failed to release lease for pod %s/%s iface %s while rolling back a failed ADD: %v",
				pr.PodNamespace, pr.PodName, pr.IfName, delErr)
		}
		return nil, fmt.Errorf("failed to apply DHCP result to %s: %v", pr.IfName, err)
	}

	return dhcpResult, nil
}

// updatePodNetworksAnnotationWithDHCPResult patches the pod's
// k8s.ovn.org/pod-networks entry for this NAD with the DHCP-learned
// ip_addresses/gateway_ips so ovnkube-controller programs the logical switch
// port with them and IP-based features (MultiNetworkPolicy, NetworkQoS) see
// the address. The IPs of a DHCP entry are owned by the external DHCP server;
// on a repeat CNI ADD (sandbox recreation) with a new lease they are
// overwritten with the newly learned ones.
func (pr *PodRequest) updatePodNetworksAnnotationWithDHCPResult(clientset *ClientSet, dhcpResult *current.Result) error {
	pod, err := clientset.getPod(pr.PodNamespace, pr.PodName)
	if err != nil {
		return fmt.Errorf("failed to get pod %s/%s: %w", pr.PodNamespace, pr.PodName, err)
	}

	updateFn := func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
		// The lease belongs to this request's sandbox: if that sandbox's
		// netns is gone, kubelet has torn it down. Since kubelet
		// completes the old sandbox's DEL before creating a new one, a newer
		// sandbox may already have patched its fresh lease, which this
		// abandoned request (nothing cancels an in-flight ADD when kubelet
		// times it out and moves on) must not overwrite with its stale one.
		// The check sits INSIDE updateFn so every retry attempt re-runs it:
		// a write conflict with the newer sandbox's patch forces a retry and
		// deterministically lands here after the netns is gone.
		if _, err := os.Stat(pr.Netns); err != nil {
			return nil, nil, fmt.Errorf("sandbox %s of pod %s/%s was superseded (netns %q is gone), "+
				"refusing to patch a stale DHCP lease: %w",
				pr.SandboxID, pr.PodNamespace, pr.PodName, pr.Netns, err)
		}
		// guard against the pod having been deleted and recreated since this
		// CNI ADD started; never report this sandbox's lease on a new instance
		if pr.PodUID != "" && string(pod.UID) != pr.PodUID {
			return nil, nil, fmt.Errorf("pod %s/%s UID %q does not match CNI request UID %q",
				pr.PodNamespace, pr.PodName, pod.UID, pr.PodUID)
		}
		podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, pr.nadKey)
		if err != nil {
			return nil, nil, fmt.Errorf("no pod-networks entry for NAD key %s: %w", pr.nadKey, err)
		}
		podAnnotation.IPs = nil
		podAnnotation.Gateways = nil
		for _, ipc := range dhcpResult.IPs {
			addr := ipc.Address
			podAnnotation.IPs = append(podAnnotation.IPs, &addr)
			if ipc.Gateway != nil {
				podAnnotation.Gateways = append(podAnnotation.Gateways, ipc.Gateway)
			}
		}
		annotations, err := util.MarshalPodAnnotation(pod.Annotations, podAnnotation, pr.nadKey)
		if err != nil {
			if util.IsAnnotationAlreadySetError(err) {
				// repeat CNI ADD with the same lease, nothing to update
				return pod, nil, nil
			}
			return nil, nil, err
		}
		pod.Annotations = annotations
		return pod, nil, nil
	}
	return util.UpdatePodWithRetryOrRollback(clientset.podLister, &kube.Kube{KClient: clientset.kclient}, pod, updateFn)
}

// execDHCPDel releases the DHCP lease by delegating DEL to the DHCP plugin.
func (pr *PodRequest) execDHCPDel() error {
	dhcpConfBytes, err := pr.buildDHCPConf()
	if err != nil {
		return err
	}

	cniPath := getCNIPath()
	pluginPath, err := findDHCPPlugin(cniPath)
	if err != nil {
		return fmt.Errorf("failed to find dhcp plugin in CNI_PATH (%s): %v", cniPath, err)
	}

	cniArgs := &invoke.Args{
		Command:     "DEL",
		ContainerID: pr.SandboxID,
		NetNS:       pr.Netns,
		IfName:      pr.IfName,
		Path:        cniPath,
	}

	return invoke.ExecPluginWithoutResult(pr.ctx, pluginPath, dhcpConfBytes, cniArgs, dhcpPluginExec)
}

func (pr *PodRequest) dhcpLeaseMarkerPath() string {
	return filepath.Join(dhcpLeaseMarkerDir, pr.SandboxID+"_"+pr.IfName)
}

// recordDHCPLeaseMarker records that the DHCP CNI daemon holds (or is about to
// be asked for) a lease for this sandbox, so cmdDel keys the release off
// node-local state and needs no apiserver access. Full mode uses the host OVS
// interface external_ids; DPU-host nodes have no host OVS, so a host-local
// file (keyed by sandbox+ifname) is used instead.
func (pr *PodRequest) recordDHCPLeaseMarker(result *current.Result) error {
	if config.IsModeDPUHost() {
		if err := os.MkdirAll(dhcpLeaseMarkerDir, 0o700); err != nil {
			return fmt.Errorf("failed to create DHCP lease marker dir %q: %w", dhcpLeaseMarkerDir, err)
		}
		return os.WriteFile(pr.dhcpLeaseMarkerPath(), nil, 0o600)
	}
	return markDHCPDaemonLeaseOVS(result)
}

// dhcpLeaseMarkerExists reports whether cmdAdd recorded a daemon-managed DHCP
// lease for this sandbox. Consulting node-local state (OVS in full mode, a
// file on DPU-host) instead of the pod object keeps the decision correct when
// the pod was already deleted from the apiserver (e.g. force delete): KubeVirt
// VM pods never carry the marker, so their DEL never sends a bogus release.
// (false, nil) means there is provably nothing to release: repeat DEL, or a
// VM pod that never recorded one. A lookup failure is returned as an error,
// not folded into false: mistaking "couldn't check" for "no lease" would skip
// the RELEASE while the teardown below deletes the marker with the port,
// leaking the daemon lease with no retry path.
func (pr *PodRequest) dhcpLeaseMarkerExists() (bool, error) {
	if config.IsModeDPUHost() {
		if _, err := os.Stat(pr.dhcpLeaseMarkerPath()); err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, fmt.Errorf("failed to check the DHCP lease marker of pod %s/%s (sandbox %s): %w",
				pr.PodNamespace, pr.PodName, pr.SandboxID, err)
		}
		return true, nil
	}
	return pr.dhcpDaemonLeaseRecordedOVS()
}

// removeDHCPLeaseMarker clears the marker after the lease is released. In full
// mode the OVS marker dies with the port on UnconfigureInterface, so this is a
// no-op there; on DPU-host it unlinks the file.
func (pr *PodRequest) removeDHCPLeaseMarker() {
	if config.IsModeDPUHost() {
		if err := os.Remove(pr.dhcpLeaseMarkerPath()); err != nil && !os.IsNotExist(err) {
			klog.Warningf("Failed to remove DHCP lease marker for pod %s/%s: %v",
				pr.PodNamespace, pr.PodName, err)
		}
	}
}

// markDHCPDaemonLeaseOVS records the marker on the pod's host-side OVS
// interface, using the same bounded retry as the ConfigureOVS port-add
// transaction. The marker is removed together with the port on teardown.
func markDHCPDaemonLeaseOVS(result *current.Result) error {
	hostIfaceName := ""
	for _, iface := range result.Interfaces {
		if iface.Sandbox == "" && iface.Name != "" {
			hostIfaceName = iface.Name
			break
		}
	}
	if hostIfaceName == "" {
		return fmt.Errorf("no host-side interface in the CNI result to record the DHCP lease marker on")
	}
	backoff := wait.Backoff{Duration: 100 * time.Millisecond, Steps: 3}
	return retry.OnError(backoff, func(error) bool { return true }, func() error {
		return ovsSet("interface", hostIfaceName, "external_ids:dhcp-lease=true")
	})
}

// dhcpDaemonLeaseRecordedOVS reads the marker back from the host OVS interface.
// Zero matches is the legitimate "port already gone" repeat-DEL case; an
// ovs-vsctl failure is a lookup error, not evidence of absence.
func (pr *PodRequest) dhcpDaemonLeaseRecordedOVS() (bool, error) {
	ovsIfNames, err := ovsFind("Interface", "name",
		"external-ids:sandbox="+pr.SandboxID,
		fmt.Sprintf("external_ids:pod-if-name=%s", pr.IfName))
	if err != nil {
		return false, fmt.Errorf("failed to look up the OVS interface of pod %s/%s (sandbox %s, iface %s) to check the DHCP lease marker: %w",
			pr.PodNamespace, pr.PodName, pr.SandboxID, pr.IfName, err)
	}
	if len(ovsIfNames) != 1 {
		// 0: the port (and with it the marker) is already gone — repeat DEL,
		// nothing to release. >1 cannot legitimately happen since
		// sandbox+pod-if-name is unique per attachment.
		return false, nil
	}
	out, err := ovsGet("interface", ovsIfNames[0], "external_ids", "dhcp-lease")
	if err != nil {
		return false, fmt.Errorf("failed to read the DHCP lease marker from OVS interface %s of pod %s/%s: %w",
			ovsIfNames[0], pr.PodNamespace, pr.PodName, err)
	}
	return out == "true", nil
}

// buildDHCPConf constructs the JSON network config that the DHCP IPAM plugin expects.
//
// The cniVersion is inherited from the UDN-generated NAD (config.CNISpecVersion,
// currently "1.1.0") and forwarded to the delegated /opt/cni/bin/dhcp plugin,
// which validates it against the CNI spec versions its vendored cni library
// supports. CNI spec 1.1.0 is accepted by plugins vendoring cni >= v1.2.0,
// first shipped in containernetworking/plugins v1.6.0 (2024-10-15; v1.5.x
// still vendored cni v1.1.2, which tops out at spec 1.0.0 and rejects this
// config with an unsupported-version error). Both the dhcp binary and the
// long-running dhcp daemon on the node must therefore come from
// containernetworking/plugins v1.6.0 or newer.
func (pr *PodRequest) buildDHCPConf() ([]byte, error) {
	conf := map[string]any{
		"cniVersion": pr.CNIConf.CNIVersion,
		"name":       pr.CNIConf.Name,
		"type":       "ovn-k8s-cni-overlay",
		"ipam": map[string]any{
			"type": types.IPAMTypeDHCP,
		},
	}
	return json.Marshal(conf)
}

// getCNIPath returns the CNI plugin binary search path.
// It checks CNI_PATH env var first (set by kubelet or the container runtime),
// then falls back to the standard /opt/cni/bin directory.
// The ovnkube-node DaemonSet must mount the host's CNI bin directory
// for DHCP IPAM plugin delegation to work.
func getCNIPath() string {
	if p := os.Getenv("CNI_PATH"); p != "" {
		return p
	}
	return "/opt/cni/bin"
}

// filterDHCPDefaultRoutes drops default routes from a pod's DHCP result.
// DHCP IPAM is restricted to Secondary networks (CRD CEL rule), and a
// secondary attachment must not compete with the pod's primary network for
// the default route — matching how every other OVN-Kubernetes secondary
// attachment behaves. Off-subnet reachability via the localnet belongs in
// explicit option-121 subnet routes, which are kept; a deliberate
// default-route override remains available via the Multus gateway request,
// which Multus applies itself. KubeVirt VM guests are unaffected: their
// in-guest DHCP client applies the server's routes, default route included.
func (pr *PodRequest) filterDHCPDefaultRoutes(dhcpResult *current.Result) {
	routes := dhcpResult.Routes[:0]
	for _, route := range dhcpResult.Routes {
		if ones, _ := route.Dst.Mask.Size(); ones == 0 {
			klog.Infof("DHCP: dropping default route via %s for pod %s/%s iface %s: "+
				"the pod's primary network owns the default route",
				route.GW, pr.PodNamespace, pr.PodName, pr.IfName)
			continue
		}
		routes = append(routes, route)
	}
	dhcpResult.Routes = routes
}

// applyDHCPResult applies the IP addresses and routes of a DHCP IPAM result
// to the named interface inside the container network namespace, playing the
// role of upstream ipam.ConfigureIface for the delegated dhcp plugin's result.
func applyDHCPResult(netnsPath, ifName string, dhcpResult *current.Result) error {
	netns, err := ns.GetNS(netnsPath)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", netnsPath, err)
	}
	defer netns.Close()

	return netns.Do(func(_ ns.NetNS) error {
		return applyDHCPResultInNS(ifName, dhcpResult)
	})
}

// applyDHCPResultInNS does the actual interface configuration and must run
// inside the container network namespace. The result's routes are installed
// exactly as handed in, honoring scope (on-link option-121 routes carry
// SCOPE_LINK), priority and table like upstream ipam.ConfigureIface. The
// IPConfig gateway is reporting-only metadata and contributes no route: the
// dhcp daemon already encodes the server's routing intent — RFC 3442
// precedence included — in the routes list, so a gateway-derived default
// route here would either duplicate it or install a route the server said
// to ignore. A nil-GW fallback like upstream's is deliberately absent:
// daemon results always carry explicit routers.
func applyDHCPResultInNS(ifName string, dhcpResult *current.Result) error {
	link, err := util.GetNetLinkOps().LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to find interface %s: %v", ifName, err)
	}

	// Add IP addresses from DHCP; EEXIST is tolerated so a repeated ADD
	// stays idempotent
	for _, ipConfig := range dhcpResult.IPs {
		addr := &netlink.Addr{IPNet: &ipConfig.Address}
		if err := util.GetNetLinkOps().AddrAdd(link, addr); err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to add IP %s to %s: %v",
				ipConfig.Address.String(), ifName, err)
		}
		klog.Infof("DHCP: applied IP %s to %s", ipConfig.Address.String(), ifName)
	}

	// Route failures are fatal, like upstream ipam.ConfigureIface and
	// setupNetwork in this file: a pod missing its DHCP-advertised routes
	// must not report a successful ADD. Only EEXIST is tolerated, for the
	// same idempotency reason as above.
	for _, route := range dhcpResult.Routes {
		nlRoute := netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       &route.Dst,
			Gw:        route.GW,
			Priority:  route.Priority,
		}
		if route.Table != nil {
			nlRoute.Table = *route.Table
		}
		if route.Scope != nil {
			nlRoute.Scope = netlink.Scope(*route.Scope)
		}
		if err := util.GetNetLinkOps().RouteAdd(&nlRoute); err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to add route dst=%s gw=%s scope=%d on %s: %v",
				route.Dst.String(), route.GW, nlRoute.Scope, ifName, err)
		}
		klog.Infof("DHCP: applied route dst=%s gw=%s on %s", route.Dst.String(), route.GW, ifName)
	}

	return nil
}

const (
	netDevPollTimeout  = 5 * time.Second
	netDevPollInterval = 200 * time.Millisecond
)

// getNetdevName resolves the host netdev name for a PCI/aux device, polling
// until it appears. After the VFIO→kernel driver handoff the kernel creates the
// netdev asynchronously, so a single lookup can race with the driver bind; poll
// until the netdev shows up instead of failing on the transient lookup error.
func getNetdevName(ctx context.Context, deviceID string, deviceInfo nadapi.DeviceInfo) (string, error) {
	var netdevName string
	retries := 0
	err := wait.PollUntilContextTimeout(ctx, netDevPollInterval, netDevPollTimeout, true,
		func(_ context.Context) (bool, error) {
			var localErr error
			netdevName, localErr = util.GetNetdevNameFromDeviceId(deviceID, deviceInfo)
			retries++
			// The netdev may not exist yet right after a driver rebind; keep
			// polling rather than aborting on the transient "no netdevice" error.
			return localErr == nil && netdevName != "", nil
		})
	if err != nil {
		return "", fmt.Errorf("failed to find netdev for device %s after %d retries: %w", deviceID, retries, err)
	}
	return netdevName, nil
}

// runLocalnetVFIODHCP acquires a DHCP lease for a VFIO VF on a localnet network.
// It temporarily unbinds the VF from vfio-pci and rebinds it to the kernel
// driver matching its hardware (detected from the PCI vendor ID) to get a
// netdev, performs a DHCP DORA exchange to discover the IP, then rebinds to
// vfio-pci. VFs of unsupported NIC vendors are rejected before any driver
// state is touched.
//
// This must run AFTER ConfigureOVS so the representor→OVS→localnet path is
// ready for DHCP packets to reach the physical network.
//
// Unlike the pod (DHCPManaged) case, this does NOT use the CNI DHCP daemon.
// It uses the insomniacslk/dhcp library directly to acquire the lease.
// No lease maintenance is performed and the VM's own DHCP client takes over.
func (pr *PodRequest) runLocalnetVFIODHCP() (dhcpResult *current.Result, retErr error) {
	deviceID := pr.CNIConf.DeviceID

	currentDriver, err := getPCIDeviceDriver(deviceID)
	if err != nil {
		return nil, err
	}
	if currentDriver != vfioPCIDriver {
		return nil, fmt.Errorf("pci device %s is not bound to %s, found %q",
			deviceID, vfioPCIDriver, currentDriver)
	}

	// Detect the kernel driver matching this VF's hardware BEFORE any driver
	// state is touched: an unsupported vendor must fail cleanly here, not
	// halfway through an unbind/bind cycle with a raw sysfs error.
	kernelDriver, err := vfKernelDriverForDevice(deviceID)
	if err != nil {
		return nil, err
	}

	klog.Infof("VFIO DHCP: acquiring lease for pod %s/%s device %s via driver %s",
		pr.PodNamespace, pr.PodName, deviceID, kernelDriver)

	// Always restore vfio-pci on the way out, no matter where we fail below.
	// This must be registered before the driver swap is attempted: the swap
	// unbinds from vfio-pci before binding the kernel driver, so a failure
	// halfway leaves the VF bound to nothing. Rebinding is a no-op when the
	// device is still (or already back) on vfio-pci.
	defer func() {
		if err := dhcpOps.BindPCIDeviceDriver(deviceID, vfioPCIDriver); err != nil {
			if retErr != nil {
				retErr = fmt.Errorf("%w; rebind to %s failed: %v",
					retErr, vfioPCIDriver, err)
			} else {
				retErr = fmt.Errorf("rebind device %s to %s failed: %w",
					deviceID, vfioPCIDriver, err)
			}
			dhcpResult = nil
			// the failed bind's internal rollback clears driver_override;
			// re-pin so the retried ADD's repair check still fires
			if perr := writePCIDeviceDriverOverride(deviceID, vfioPCIDriver); perr != nil {
				klog.Warningf("Failed to re-pin device %s to %s after a failed rebind: %v",
					deviceID, vfioPCIDriver, perr)
			}
		} else {
			klog.Infof("VFIO DHCP: device %s rebound to %s",
				deviceID, vfioPCIDriver)
			// leave the pin in place: it matches how SR-IOV tooling keeps
			// vfio devices bound across reprobes, and keeps the ADD-time
			// repair (healInterruptedVFIOHandoff) armed for the next crash
			if err := writePCIDeviceDriverOverride(deviceID, vfioPCIDriver); err != nil {
				klog.Warningf("Failed to restore the %s pin on device %s: %v",
					vfioPCIDriver, deviceID, err)
			}
		}
	}()

	// Unbind from vfio-pci, bind to the hardware-matching kernel driver to
	// get a netdev
	if err := dhcpOps.BindPCIDeviceDriver(deviceID, kernelDriver); err != nil {
		return nil, fmt.Errorf("failed to bind device %s to %s: %w",
			deviceID, kernelDriver, err)
	}
	// The device is now on the kernel driver but belongs to vfio-pci. Record
	// that intent in driver_override immediately: the pin lives in the
	// kernel, survives an ovnkube-node crash, and never affects the current
	// binding. It is what healInterruptedVFIOHandoff keys the repair off
	// when a retried ADD finds the device mid-handoff. Failing here is safe as
	// the deferred rebind above restores vfio-pci on the way out.
	if err := writePCIDeviceDriverOverride(deviceID, vfioPCIDriver); err != nil {
		return nil, fmt.Errorf("failed to pin device %s to %s during the DHCP handoff: %w",
			deviceID, vfioPCIDriver, err)
	}

	// Discover the temporary netdev
	netdevName, err := getNetdevName(pr.ctx, deviceID, pr.deviceInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to find netdev for device %s: %w",
			deviceID, err)
	}

	link, err := util.GetNetLinkOps().LinkByName(netdevName)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup netdev %s: %w", netdevName, err)
	}
	if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("failed to bring up %s: %w", netdevName, err)
	}

	klog.Infof("VFIO DHCP: device %s netdev %s MAC %s is up, starting DHCP",
		deviceID, netdevName, link.Attrs().HardwareAddr)

	// One-shot DHCP directly using insomniacslk/dhcp. No daemon involved.
	// The temporary netdev carries the MAC that ConfigureInterface programmed
	// on the VF (the annotation MAC the VM will use), so the lease is keyed to
	// the identity the guest's DHCP client will present after boot.
	// Release immediately (probe semantics): the vfio-pci rebind below
	// destroys the transmit path, so a lease kept across a failed rebind
	// could never be released. Requires a static MAC→IP reservation for the
	// guest to re-acquire the same IP at boot.
	dhcpResult, err = acquireDHCPLease(pr.ctx, netdevName, link.Attrs().HardwareAddr, true)
	if err != nil {
		return nil, fmt.Errorf("DHCP lease acquisition failed on %s: %w", netdevName, err)
	}

	klog.Infof("VFIO DHCP: pod %s/%s device %s got IP=%s gw=%s",
		pr.PodNamespace, pr.PodName, deviceID,
		dhcpResult.IPs[0].Address.String(), dhcpResult.IPs[0].Gateway)

	// The result is only reported (CNI result + pod-networks annotation).
	// No AddrAdd is needed for VFIO, as it has no netdev in the container netns and the VF is
	// rebound to vfio-pci; the VM's own DHCP client manages the lease after boot.
	return dhcpResult, nil
}

// doOneShotDHCP performs a single DHCP DORA to discover the externally-assigned
// IP for a KubeVirt VM workload and returns it for reporting (CNI result,
// pod-networks annotation and NBDB programming). The IP is never applied to the
// interface:
//   - VFIO passthrough: the VF is rebound to vfio-pci afterwards, so any address
//     configured on the temporary netdev would be discarded.
//   - non-VFIO (l2bridge/managedTap): applying the IP would start KubeVirt's
//     in-pod dnsmasq, which conflicts with the external DHCP server and prevents
//     the guest from renewing its lease.
//
// In both cases the guest runs its own DHCP client after boot and owns the lease
// lifecycle; no lease maintenance is done
func (pr *PodRequest) doOneShotDHCP() (*current.Result, error) {
	if pr.shouldRunLocalnetVFIODriverHandoff() {
		// VFIO passthrough: a temporary driver handoff exposes a host netdev for
		// the DORA exchange, after which the VF is rebound to vfio-pci.
		return pr.runLocalnetVFIODHCP()
	}
	// non-VFIO binding: the netdev already exists in the pod netns.
	return pr.acquireOneShotDHCPInPodNetns()
}

// acquireOneShotDHCPInPodNetns performs a one-shot DHCP DORA on the pod-netns
// interface for a non-VFIO KubeVirt VM (l2bridge/managedTap binding) to discover
// the IP assigned by the external DHCP server. The discovered IP is returned
// for reporting only and is deliberately NOT applied to the interface, since
// configuring it would trigger KubeVirt's in-pod dnsmasq.
func (pr *PodRequest) acquireOneShotDHCPInPodNetns() (*current.Result, error) {
	netns, err := ns.GetNS(pr.Netns)
	if err != nil {
		return nil, fmt.Errorf("failed to open netns %q: %w", pr.Netns, err)
	}
	defer netns.Close()

	var dhcpResult *current.Result
	if err := netns.Do(func(_ ns.NetNS) error {
		link, err := util.GetNetLinkOps().LinkByName(pr.IfName)
		if err != nil {
			return fmt.Errorf("failed to find interface %s: %w", pr.IfName, err)
		}
		if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to bring up %s: %w", pr.IfName, err)
		}
		// KubeVirt's bridge binding hands the pod interface MAC to the guest,
		// so keying the lease on it matches the guest's later DHCP identity.
		// Keep the lease: the netdev persists in the pod netns for the VM's
		// lifetime and the guest's DHCP client, presenting the same MAC-based
		// client-id, renews this very lease at boot — the lease is handed
		// over, not leaked. Releasing here would only open a window for the
		// pool to reassign the IP before the guest boots.
		dhcpResult, err = acquireDHCPLease(pr.ctx, pr.IfName, link.Attrs().HardwareAddr, false)
		if err != nil {
			return fmt.Errorf("DHCP lease acquisition failed on %s: %w", pr.IfName, err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	klog.Infof("VM DHCP: pod %s/%s iface %s discovered IP=%s gw=%s (reported only, not applied)",
		pr.PodNamespace, pr.PodName, pr.IfName,
		dhcpResult.IPs[0].Address.String(), dhcpResult.IPs[0].Gateway)

	return dhcpResult, nil
}

// mergeDHCPResultIntoCNIResult copies the DHCP-discovered IPs and routes into the
// CNI result so they are surfaced in the pod-networks annotation and programmed
// in the OVN NBDB. It does not modify the interface itself.
func mergeDHCPResultIntoCNIResult(dhcpResult, result *current.Result) {
	// Reference the container-side interface (the one with a sandbox path).
	// When none exists — e.g. VFIO, where the VF has no netdev in the pod
	// netns — leave ips[].interface unset: the CNI spec makes it optional,
	// and a fabricated index would point at a host-side interface or, with
	// an empty interfaces list, be out of range.
	var contIfIdx *int
	for i, iface := range result.Interfaces {
		if iface.Sandbox != "" {
			contIfIdx = current.Int(i)
			break
		}
	}
	for _, ipConfig := range dhcpResult.IPs {
		ipConfig.Interface = contIfIdx
		result.IPs = append(result.IPs, ipConfig)
	}
	result.Routes = append(result.Routes, dhcpResult.Routes...)
}

// dhcpClientID returns the RFC 2132 Client-ID (option 61) for the given
// hardware address: a 1-byte hardware type (0x01 = Ethernet) followed by the
// address itself.
func dhcpClientID(hwAddr net.HardwareAddr) []byte {
	return append([]byte{0x01}, hwAddr...)
}

// acquireDHCPLease performs a DHCP DORA (Discover, Offer, Request, Ack) exchange
// on the given interface using the insomniacslk/dhcp library and returns the
// result. No lease maintenance is performed — the caller is responsible for
// any subsequent renewals or releases.
//
// DHCP IPAM mode is IPv4-only: this client speaks DHCPv4 (as does the
// delegated dhcp IPAM plugin on the pod path); IPv6 addressing (DHCPv6 or
// SLAAC) is neither acquired nor reported.
//
// The DHCP Client-ID option (option 61) is explicitly set to the RFC 2132
// htype-prefixed hardware address (0x01 + MAC). This matches the Client-ID
// sent by common guest DHCP clients keyed on the MAC (Windows, NetworkManager
// with ipv4.dhcp-client-id=mac, dhclient with a configured client-id).
// This ensures the external DHCP server keys the lease identically whether the
// request comes from this one-shot DORA during CNI ADD or from the VM's own
// DHCP client after boot, so the guest renews this very lease instead of
// being allocated a different IP (ISC dhcpd in particular records a second
// lease when the Client-ID presence differs between requests).
func acquireDHCPLease(ctx context.Context, ifName string, hwAddr net.HardwareAddr, releaseLease bool) (*current.Result, error) {
	// Create a raw DHCP client on the interface
	client, err := nclient4.New(ifName,
		nclient4.WithTimeout(vmDHCPTimeout),
		nclient4.WithRetry(vmDHCPRetries),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP client on %s: %w", ifName, err)
	}
	defer client.Close()

	// Perform full DORA: DISCOVER → OFFER → REQUEST → ACK
	lease, err := client.Request(ctx,
		dhcpv4.WithOption(dhcpv4.OptClientIdentifier(dhcpClientID(hwAddr))),
	)
	if err != nil {
		return nil, fmt.Errorf("DHCP request failed on %s: %w", ifName, err)
	}

	if lease.ACK == nil {
		return nil, fmt.Errorf("DHCP lease has no ACK")
	}

	if releaseLease {
		// Probe semantics: the DORA above only learns the server's assignment
		// for this MAC; the lease is returned immediately, while the netdev
		// still exists, so no later failure (in particular a failed vfio-pci
		// rebind, which destroys the transmit path) can strand it until TTL.
		// The guest re-acquires at boot — a static MAC→IP reservation on the
		// server is required for it to receive the IP already published in
		// the pod annotation. Best-effort by protocol (RFC 2131 §4.4.4,
		// releases are unacknowledged): on failure the lease simply expires
		// at TTL, exactly as if no release was sent.
		if err := releaseDHCPLease(ifName, lease, hwAddr); err != nil {
			klog.Warningf("DHCP: failed to release one-shot lease on %s: %v", ifName, err)
		}
	}

	return dhcpACKToCNIResult(ifName, lease.ACK)
}

// releaseDHCPLease sends a DHCPRELEASE for a lease acquired by
// acquireDHCPLease. RFC 2131 §4.4.4 requires the release be unicast to the
// server sourced from the leased address; since the one-shot path never
// configures the lease on the netdev, the address is added transiently (with
// the lease's real mask, so the kernel's prefix route provides reachability
// and ARP) for the kernel-socket send, mirroring containernetworking/plugins
// PR #1271. The vendored NewReleaseFromACK does not carry the Client
// Identifier, so it is passed explicitly — the server matched this lease on
// option 61 and would not apply a release without it.
//
// The DHCP server is assumed to be on the leased subnet. With a relayed
// (off-subnet) server the unicast send has no route and this falls back to
// the legacy raw-socket release (source 0.0.0.0, L2 broadcast), which such
// servers may ignore — the lease then expires at TTL. See the DHCP IPAM
// documentation.
func releaseDHCPLease(ifName string, lease *nclient4.Lease, hwAddr net.HardwareAddr) error {
	clientID := dhcpv4.WithOption(dhcpv4.OptClientIdentifier(dhcpClientID(hwAddr)))
	leasedIP := lease.ACK.YourIPAddr
	mask := lease.ACK.SubnetMask()

	if link, err := util.GetNetLinkOps().LinkByName(ifName); err == nil && mask != nil {
		addr := &netlink.Addr{IPNet: &net.IPNet{IP: leasedIP, Mask: mask}}
		if err := util.GetNetLinkOps().AddrAdd(link, addr); err == nil {
			defer func() { _ = util.GetNetLinkOps().AddrDel(link, addr) }()
			c, err := nclient4.New(ifName,
				nclient4.WithUnicast(&net.UDPAddr{IP: leasedIP, Port: nclient4.ClientPort}))
			if err == nil {
				defer c.Close()
				if err := c.Release(lease, clientID); err == nil {
					return nil
				}
			}
		}
	}

	// Legacy raw-socket fallback (source 0.0.0.0, L2 broadcast) — lenient
	// servers accept it; strict or relayed ones may not.
	c, err := nclient4.New(ifName)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Release(lease, clientID)
}

// dhcpACKToCNIResult converts a DHCPACK into a CNI result, mirroring the
// delegated dhcp IPAM plugin (containernetworking/plugins/plugins/ipam/dhcp)
// so pods (plugin delegation) and VMs (this one-shot DORA) report identical
// results for the same lease:
//   - a missing or malformed subnet mask (option 1) fails the request, like
//     upstream lease.IPNet ("DHCP option Subnet Mask not found in DHCPACK"),
//     instead of being defaulted to a prefix no real client would compute;
//   - the first Router (option 3) is reported as the IPConfig gateway,
//     unconditionally, like upstream daemon Allocate;
//   - per RFC 3442 and upstream lease.Routes, when Classless Static Routes
//     (option 121) are present they are the entire route set (on-link routes
//     carry SCOPE_LINK) and the Router option contributes no default route;
//     otherwise the router is added as an explicit default route. The
//     deprecated classful Static Routes option (33) is not parsed — the
//     vendored dhcpv4 library has no parser for it.
func dhcpACKToCNIResult(ifName string, ack *dhcpv4.DHCPv4) (*current.Result, error) {
	ip := ack.YourIPAddr
	if ip == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("DHCP ACK on %s has no IP address", ifName)
	}

	mask := ack.SubnetMask()
	if mask == nil {
		return nil, fmt.Errorf("DHCP option Subnet Mask not found in DHCPACK on %s", ifName)
	}

	// Extract the gateway (option 3) with the library parser, which
	// validates the option layout; servers may return several routers —
	// use the first, as standard clients do.
	var gateway net.IP
	if routers := ack.Router(); len(routers) > 0 {
		gateway = routers[0]
	}

	result := &current.Result{
		IPs: []*current.IPConfig{{
			Address: net.IPNet{IP: ip, Mask: mask},
			Gateway: gateway,
		}},
	}

	// Extract classless static routes (option 121) with the library parser
	// (RFC 3442): malformed option data — e.g. a prefix length > 32 — is
	// rejected wholesale rather than half-parsed into bogus routes that
	// could masquerade as a default route.
	result.Routes = classlessRoutesToCNIRoutes(ack.ClasslessStaticRoute())
	if len(result.Routes) == 0 && gateway != nil {
		result.Routes = append(result.Routes, &cnitypes.Route{
			Dst: net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			GW:  gateway,
		})
	}

	return result, nil
}

// classlessRoutesToCNIRoutes converts RFC 3442 routes parsed by the dhcpv4
// library into CNI result routes. An unspecified router means an on-link
// route; it is marked SCOPE_LINK, as the delegated dhcp IPAM plugin does.
func classlessRoutesToCNIRoutes(routes []*dhcpv4.Route) []*cnitypes.Route {
	var cniRoutes []*cnitypes.Route
	for _, r := range routes {
		route := &cnitypes.Route{Dst: *r.Dest, GW: r.Router}
		if r.Router.IsUnspecified() {
			scope := int(netlink.SCOPE_LINK)
			route.Scope = &scope
		}
		cniRoutes = append(cniRoutes, route)
	}
	return cniRoutes
}
