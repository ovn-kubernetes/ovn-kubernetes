// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	apbroutecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	logicalswitchmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestAllocatePodIPsAfterSubnetChange(t *testing.T) {
	for _, networkName := range []string{"default", "blue"} {
		for _, tc := range []struct {
			name     string
			phase    corev1.PodPhase
			ips      []string
			reserved []string
			wantErr  bool
		}{
			{name: "evicted pod with obsolete IPv4", phase: corev1.PodFailed, ips: []string{"172.18.1.3/24"}},
			{name: "succeeded pod with obsolete IPv6", phase: corev1.PodSucceeded, ips: []string{"fd00:1::3/64"}},
			{name: "terminal pod retains current reservations", phase: corev1.PodFailed, ips: []string{"172.18.2.3/24", "fd00:2::3/64"}, reserved: []string{"172.18.2.3/24", "fd00:2::3/64"}},
			{name: "terminal pod retains IPv6 when IPv4 changed", phase: corev1.PodFailed, ips: []string{"172.18.1.3/24", "fd00:2::3/64"}, reserved: []string{"fd00:2::3/64"}},
			{name: "terminal pod retains IPv4 when IPv6 changed", phase: corev1.PodSucceeded, ips: []string{"172.18.2.3/24", "fd00:1::3/64"}, reserved: []string{"172.18.2.3/24"}},
			{name: "running pod mismatch remains an error", phase: corev1.PodRunning, ips: []string{"172.18.1.3/24"}, wantErr: true},
			{name: "pending pod mismatch remains an error", phase: corev1.PodPending, ips: []string{"172.18.1.3/24"}, wantErr: true},
		} {
			t.Run(fmt.Sprintf("%s/%s", networkName, tc.name), func(t *testing.T) {
				g := gomega.NewWithT(t)
				g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "initialize test configuration")
				t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "restore test configuration") })
				config.IPv4Mode, config.IPv6Mode = true, true
				var netInfo util.NetInfo = &util.DefaultNetInfo{}
				nadKey := "default"
				if networkName != "default" {
					nad := ovntest.GenerateNAD(networkName, networkName, "ns1", types.Layer3Topology, "172.18.0.0/16/24,fd00::/16/64", types.NetworkRolePrimary)
					var err error
					netInfo, err = util.ParseNADInfo(nad)
					g.Expect(err).NotTo(gomega.HaveOccurred(), "parse network %s", networkName)
					nadKey = "ns1/" + networkName
				}
				switchName := netInfo.GetNetworkScopedSwitchName("node1")
				client := util.GetOVNClientset().GetOVNKubeControllerClientset()
				wf, err := factory.NewOVNKubeControllerWatchFactory(client, "node1")
				g.Expect(err).NotTo(gomega.HaveOccurred(), "create the watch factory")
				t.Cleanup(wf.Shutdown)
				g.Expect(wf.NodeCoreInformer().Informer().GetStore().Add(&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{Name: "node1"},
				})).To(gomega.Succeed(), "add node1 to the informer cache")
				lsm := logicalswitchmanager.NewLogicalSwitchManager()
				g.Expect(lsm.AddOrUpdateSwitch(switchName, ovntest.MustParseIPNets("172.18.2.0/24", "fd00:2::/64"), nil, nil)).To(gomega.Succeed(), "initialize switch %s with the current subnets", switchName)
				bnc := &BaseNetworkController{
					CommonNetworkControllerInfo: CommonNetworkControllerInfo{watchFactory: wf},
					ReconcilableNetInfo:         util.NewReconcilableNetInfo(netInfo),
					lsManager:                   lsm,
				}
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1", UID: "pod1"},
					Spec:       corev1.PodSpec{NodeName: "node1"},
					Status:     corev1.PodStatus{Phase: tc.phase},
				}
				annotation := &util.PodAnnotation{IPs: ovntest.MustParseIPNets(tc.ips...)}
				port, err := bnc.allocatePodIPsOnSwitch(pod, annotation.IPs, nadKey, switchName)
				if tc.wantErr {
					g.Expect(err).To(gomega.HaveOccurred(), "live pod inconsistencies must not be hidden")
					return
				}
				g.Expect(err).NotTo(gomega.HaveOccurred(), "obsolete terminal-pod addresses must not prevent startup")
				g.Expect(port).To(gomega.Equal(bnc.GetLogicalPortName(pod, nadKey)), "retain the port for normal cleanup")
				g.Expect(annotation.IPs).To(gomega.Equal(ovntest.MustParseIPNets(tc.ips...)), "do not rewrite pod annotations")
				for _, ip := range tc.reserved {
					ips := ovntest.MustParseIPNets(ip)
					g.Expect(lsm.OwnsIPs(switchName, bnc.podIPOwner(pod, nadKey), ips)).To(gomega.BeTrue(), "current addresses must retain the completed pod's owner")
					g.Expect(lsm.AllocateIPs(switchName, "other-pod", ips)).To(gomega.MatchError(ipallocator.ErrAllocatedByOther), "current addresses must stay reserved until cleanup")
				}
			})
		}
	}
}

func TestDeleteCompletedPodAfterSubnetChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		ips  []string
	}{
		{name: "IPv4 changed", ips: []string{"172.18.1.3/24", "fd00:2::3/64"}},
		{name: "IPv6 changed", ips: []string{"172.18.2.3/24", "fd00:1::3/64"}},
		{name: "both changed", ips: []string{"172.18.1.3/24", "fd00:1::3/64"}},
		{name: "prefix length changed", ips: []string{"172.18.2.3/16", "fd00:2::3/48"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "initialize test configuration")
			t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed(), "restore test configuration") })
			config.IPv4Mode, config.IPv6Mode = true, true
			config.Gateway.DisableSNATMultipleGWs = true
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns1", UID: "pod1"},
				Spec:       corev1.PodSpec{NodeName: "node1"},
				Status:     corev1.PodStatus{Phase: corev1.PodFailed},
			}
			ips := ovntest.MustParseIPNets(tc.ips...)
			annotation := &util.PodAnnotation{IPs: ips, MAC: util.IPAddrToHWAddr(ips[0].IP)}
			var err error
			pod.Annotations, err = util.MarshalPodAnnotation(nil, annotation, types.DefaultNetworkName)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "annotate the completed pod")
			subnets := ovntest.MustParseIPNets("172.18.2.0/24", "fd00:2::/64")
			lsp := &nbdb.LogicalSwitchPort{UUID: "pod-UUID", Name: "ns1_pod1", Addresses: []string{annotation.MAC.String() + " " + util.JoinIPNetIPs(ips, " ")}}
			ls := &nbdb.LogicalSwitch{UUID: "switch-UUID", Name: "node1", Ports: []string{lsp.UUID}}
			router := &nbdb.LogicalRouter{UUID: "router-UUID", Name: types.GWRouterPrefix + "node1"}
			data := []libovsdbtest.TestData{ls, lsp, router}
			expectedRouter := &nbdb.LogicalRouter{UUID: router.UUID, Name: router.Name}
			expected := []libovsdbtest.TestData{&nbdb.LogicalSwitch{UUID: ls.UUID, Name: ls.Name}, expectedRouter}
			var retainedIPs []*net.IPNet
			for i, ip := range ips {
				nat := &nbdb.NAT{UUID: fmt.Sprintf("nat-%d-UUID", i), Type: nbdb.NATTypeSNAT, LogicalIP: ip.IP.String(), ExternalIP: "192.0.2.1"}
				if ip.IP.To4() == nil {
					nat.ExternalIP = "2001:db8::1"
				}
				data = append(data, nat)
				router.Nat = append(router.Nat, nat.UUID)
				if util.IsIPContainedInAnyCIDR(ip.IP, subnets...) {
					retainedIPs = append(retainedIPs, ip)
				} else {
					// Cleanup must not remove IP-keyed rules outside the pod's current allocation.
					expected = append(expected, nat)
					expectedRouter.Nat = append(expectedRouter.Nat, nat.UUID)
				}
			}
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: data}, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "initialize the pod and SNAT database")
			t.Cleanup(cleanup.Cleanup)
			wf, err := factory.NewOVNKubeControllerWatchFactory(util.GetOVNClientset().GetOVNKubeControllerClientset(), "node1")
			g.Expect(err).NotTo(gomega.HaveOccurred(), "create the watch factory")
			t.Cleanup(wf.Shutdown)
			g.Expect(wf.NodeCoreInformer().Informer().GetStore().Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}})).To(gomega.Succeed(), "cache the local node")
			oc := &DefaultNetworkController{
				BaseNetworkController: BaseNetworkController{
					CommonNetworkControllerInfo: CommonNetworkControllerInfo{watchFactory: wf, nodeName: "node1", nbClient: nbClient},
					ReconcilableNetInfo:         &util.DefaultNetInfo{},
					lsManager:                   logicalswitchmanager.NewLogicalSwitchManager(),
				},
				externalGatewayRouteInfo: apbroutecontroller.NewExternalGatewayRouteInfoCache(),
			}
			g.Expect(oc.lsManager.AddOrUpdateSwitch("node1", subnets, nil, nil)).To(gomega.Succeed(), "initialize the current subnets")
			_, err = oc.allocatePodIPsOnSwitch(pod, ips, types.DefaultNetworkName, "node1")
			g.Expect(err).NotTo(gomega.HaveOccurred(), "restore the completed pod's remaining reservations")
			g.Expect(oc.deleteLogicalPort(pod, nil)).To(gomega.Succeed(), "clean up the completed pod after restart")
			g.Eventually(nbClient, 2*time.Second, 20*time.Millisecond).Should(libovsdbtest.HaveData(expected...), "delete the port and owned SNAT rules, preserving unowned rules")
			for _, ip := range retainedIPs {
				g.Expect(oc.lsManager.AllocateIPs("node1", "next-owner", []*net.IPNet{ip})).To(gomega.Succeed(), "cleanup must release the retained address %s", ip)
			}
			if len(retainedIPs) != 0 {
				nats, natErr := buildPodSNAT(ovntest.MustParseIPNets("192.0.2.2/24", "2001:db8::2/64"), retainedIPs, "", "")
				g.Expect(natErr).NotTo(gomega.HaveOccurred(), "build SNAT rules for the next owner")
				g.Expect(libovsdbops.CreateOrUpdateNATs(nbClient, &nbdb.LogicalRouter{Name: router.Name}, nats...)).To(gomega.Succeed(), "install the next owner's SNAT rules")
				for _, nat := range nats {
					expected = append(expected, nat)
					expectedRouter.Nat = append(expectedRouter.Nat, nat.UUID)
				}
				// Cleanup must see the new rules in its cache before the retry.
				g.Eventually(nbClient, 2*time.Second, 20*time.Millisecond).Should(libovsdbtest.HaveData(expected...), "observe the next owner's SNAT rules")
			}
			g.Expect(oc.deleteLogicalPort(pod, nil)).To(gomega.Succeed(), "retry cleanup after IP reuse")
			g.Consistently(nbClient, time.Second, 20*time.Millisecond).Should(libovsdbtest.HaveData(expected...), "retry must preserve unowned SNAT rules")
			if len(retainedIPs) != 0 {
				g.Expect(oc.lsManager.OwnsIPs("node1", "next-owner", retainedIPs)).To(gomega.BeTrue(), "retry must preserve the next owner's addresses")
			}
			actualAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, types.DefaultNetworkName)
			g.Expect(err).NotTo(gomega.HaveOccurred(), "read the pod annotation after cleanup")
			g.Expect(actualAnnotation.IPs).To(gomega.Equal(ips), "cleanup must not change the pod annotation")
		})
	}
}
