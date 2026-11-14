// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"net"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestIndexedPodStartupRestoresSecondaryAllocations(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.IPv4Mode, config.IPv6Mode = true, true
	netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{Name: "blue"}, Topology: types.LocalnetTopology,
		Subnets: "192.168.200.0/24,fd12:1234::/64",
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	mutable := util.NewMutableNetInfo(netInfo)
	mutable.AddNADs("tenant/blue", "infra/alias")
	nm := &networkmanager.FakeNetworkManager{NADNetworks: map[string]util.NetInfo{
		"tenant/blue": mutable, "infra/alias": mutable,
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "existing", UID: "existing-uid",
			Annotations: map[string]string{"k8s.v1.cni.cncf.io/networks": `[{"name":"blue"},{"name":"alias","namespace":"infra"},{"name":"blue","interface":"net3"}]`}},
		Spec: corev1.PodSpec{NodeName: "node1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	allocations := map[string][]*net.IPNet{
		"tenant/blue":   ovntest.MustParseIPNets("192.168.200.10/24", "fd12:1234::10/64"),
		"infra/alias":   ovntest.MustParseIPNets("192.168.200.11/24", "fd12:1234::11/64"),
		"tenant/blue/1": ovntest.MustParseIPNets("192.168.200.12/24", "fd12:1234::12/64"),
	}
	for nadKey, ips := range allocations {
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, &util.PodAnnotation{
			IPs: ips, MAC: ovntest.MustParseMAC("0a:58:c0:a8:c8:0a"), Role: types.NetworkRoleSecondary,
		}, nadKey)
		g.Expect(err).NotTo(gomega.HaveOccurred())
	}
	unrelated := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "unrelated", UID: "unrelated-uid",
			Annotations: map[string]string{"k8s.v1.cni.cncf.io/networks": "green"}},
		Spec: corev1.PodSpec{NodeName: "node1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	clients := util.GetOVNClientset(pod, unrelated, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}).GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(clients)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(wf.Shutdown)
	g.Expect(wf.Start()).To(gomega.Succeed())
	ncc := newNetworkClusterController(mutable, clients, wf, record.NewFakeRecorder(100), nm, nil, nil)
	t.Cleanup(ncc.Stop)
	g.Expect(ncc.Start(context.Background())).To(gomega.Succeed())
	for nadKey, ips := range allocations {
		g.Expect(ncc.subnetAllocator.AllocateIPs("blue", "another-pod", ips)).To(
			gomega.MatchError(ip.ErrAllocatedByOther), "must restore every attachment on %s", nadKey)
	}
	current, err := clients.KubeClient.CoreV1().Pods("tenant").Get(context.Background(), "unrelated", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(current.Annotations).To(gomega.Equal(unrelated.Annotations))
}
