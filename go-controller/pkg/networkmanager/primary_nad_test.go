// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkmanager

import (
	"context"
	"testing"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnlisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type testUDNOwners struct {
	udnlisters.UserDefinedNetworkLister
	indexer cache.Indexer
}

type testCUDNOwners struct {
	udnlisters.ClusterUserDefinedNetworkLister
	indexer cache.Indexer
}

// addPrimaryNADOwner supplies the parent objects for existing network lifecycle
// tests. Ownership rejection tests below build their NADs and parents explicitly.
func addPrimaryNADOwner(t *testing.T, c *nadController, nad *nettypes.NetworkAttachmentDefinition) {
	t.Helper()
	if nad == nil {
		return
	}
	g := gomega.NewWithT(t)
	switch owner := ovntest.PrimaryNADOwner(nad).(type) {
	case *udnv1.ClusterUserDefinedNetwork:
		if c.cudnLister == nil {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			c.cudnLister = &testCUDNOwners{udnlisters.NewClusterUserDefinedNetworkLister(indexer), indexer}
		}
		g.Expect(c.cudnLister.(*testCUDNOwners).indexer.Add(owner)).To(gomega.Succeed())
	case *udnv1.UserDefinedNetwork:
		if c.udnLister == nil {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			c.udnLister = &testUDNOwners{udnlisters.NewUserDefinedNetworkLister(indexer), indexer}
		}
		g.Expect(c.udnLister.(*testUDNOwners).indexer.Add(owner)).To(gomega.Succeed())
	}
}

func TestPrimaryNADOwnership(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     string
		modify   func(*nettypes.NetworkAttachmentDefinition, *udnv1.UserDefinedNetwork)
		missing  bool
		accepted bool
	}{
		{name: "raw primary NAD", modify: func(n *nettypes.NetworkAttachmentDefinition, _ *udnv1.UserDefinedNetwork) { n.OwnerReferences = nil }},
		{name: "UDN", accepted: true},
		{name: "CUDN", kind: "ClusterUserDefinedNetwork", accepted: true},
		{name: "missing UDN", missing: true},
		{name: "missing CUDN", kind: "ClusterUserDefinedNetwork", missing: true},
		{name: "recreated CUDN", kind: "ClusterUserDefinedNetwork", modify: func(_ *nettypes.NetworkAttachmentDefinition, u *udnv1.UserDefinedNetwork) { u.UID = "replacement" }},
		{name: "secondary CUDN", kind: "ClusterUserDefinedNetwork", modify: func(_ *nettypes.NetworkAttachmentDefinition, u *udnv1.UserDefinedNetwork) {
			u.Spec.Layer2.Role = udnv1.NetworkRoleSecondary
		}},
		{name: "recreated owner", modify: func(_ *nettypes.NetworkAttachmentDefinition, u *udnv1.UserDefinedNetwork) { u.UID = "replacement" }},
		{name: "wrong API group", modify: func(n *nettypes.NetworkAttachmentDefinition, _ *udnv1.UserDefinedNetwork) {
			n.OwnerReferences[0].APIVersion = "example.org/v1"
		}},
		{name: "wrong kind", modify: func(n *nettypes.NetworkAttachmentDefinition, _ *udnv1.UserDefinedNetwork) {
			n.OwnerReferences[0].Kind = "Deployment"
		}},
		{name: "non-controller owner", modify: func(n *nettypes.NetworkAttachmentDefinition, _ *udnv1.UserDefinedNetwork) {
			n.OwnerReferences[0].Controller = ptr.To(false)
		}},
		{name: "secondary owner", modify: func(_ *nettypes.NetworkAttachmentDefinition, u *udnv1.UserDefinedNetwork) {
			u.Spec.Layer2.Role = udnv1.NetworkRoleSecondary
		}},
		{name: "UDN in another namespace", modify: func(_ *nettypes.NetworkAttachmentDefinition, u *udnv1.UserDefinedNetwork) { u.Namespace = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			t.Cleanup(func() { _ = config.PrepareTestConfig() })
			c, client := newClusterNADTestController(t)
			udns := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			cudns := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			c.udnLister = udnlisters.NewUserDefinedNetworkLister(udns)
			c.cudnLister = udnlisters.NewClusterUserDefinedNetworkLister(cudns)
			recorder := record.NewFakeRecorder(10)
			c.recorder = recorder
			u := &udnv1.UserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: "ns", UID: "owner-uid"},
				Spec:       udnv1.UserDefinedNetworkSpec{Topology: udnv1.NetworkTopologyLayer2, Layer2: &udnv1.Layer2Config{Role: udnv1.NetworkRolePrimary}},
			}
			kind := tc.kind
			if kind == "" {
				kind = "UserDefinedNetwork"
			}
			nad, err := buildNAD("net", "ns", primaryLayer2NADConfig("ns/net"))
			g.Expect(err).NotTo(gomega.HaveOccurred())
			nad.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(u, udnv1.SchemeGroupVersion.WithKind(kind))}
			if tc.modify != nil {
				tc.modify(nad, u)
			}
			if !tc.missing {
				g.Expect(udns.Add(u)).To(gomega.Succeed())
				g.Expect(cudns.Add(&udnv1.ClusterUserDefinedNetwork{ObjectMeta: metav1.ObjectMeta{Name: u.Name, UID: u.UID}, Spec: udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{Topology: u.Spec.Topology, Layer2: u.Spec.Layer2}}})).To(gomega.Succeed())
			}
			_, err = client.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("ns").Create(context.Background(), nad, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(c.syncNAD(context.Background(), "ns/net", nad)).To(gomega.Succeed())
			if tc.accepted {
				g.Expect(c.primaryNADs).To(gomega.HaveKeyWithValue("ns", "ns/net"))
				g.Expect(c.GetNetwork("networkAPrimary")).NotTo(gomega.BeNil())
				g.Expect(recorder.Events).NotTo(gomega.Receive())
			} else {
				g.Expect(c.primaryNADs).To(gomega.BeEmpty())
				g.Expect(c.GetNetwork("networkAPrimary")).To(gomega.BeNil())
				g.Expect(c.networkIDAllocator.GetID("networkAPrimary")).To(gomega.Equal(types.InvalidID))
				updated, err := client.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("ns").Get(context.Background(), "net", metav1.GetOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(updated.Annotations).To(gomega.BeEmpty())
				g.Expect(recorder.Events).To(gomega.Receive(gomega.ContainSubstring("InvalidConfig")))
			}
		})
	}
}

func TestPrimaryNADOwnerEvents(t *testing.T) {
	for _, kind := range []string{"UserDefinedNetwork", "ClusterUserDefinedNetwork"} {
		t.Run(kind, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			t.Cleanup(func() { _ = config.PrepareTestConfig() })
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			ctx := context.Background()
			owner := &udnv1.UserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: "ns", UID: "owner-uid"},
				Spec:       udnv1.UserDefinedNetworkSpec{Topology: udnv1.NetworkTopologyLayer2, Layer2: &udnv1.Layer2Config{Role: udnv1.NetworkRolePrimary}},
			}
			nad, err := buildNAD("net", "ns", primaryLayer2NADConfig("ns/net"))
			g.Expect(err).NotTo(gomega.HaveOccurred())
			nad.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, udnv1.SchemeGroupVersion.WithKind(kind))}
			// A preexisting allocation must not bypass ownership checks at startup.
			nad.Annotations = map[string]string{types.OvnNetworkIDAnnotation: "17"}
			client := util.GetOVNClientset(nad).GetClusterManagerClientset()
			wf, err := factory.NewClusterManagerWatchFactory(client)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(wf.Shutdown)
			manager, err := NewForCluster(&testControllerManager{controllers: map[string]NetworkController{}}, wf, client, nil, id.NewTunnelKeyAllocator("TunnelKeys"))
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(manager.Stop)
			g.Expect(wf.Start()).To(gomega.Succeed())
			g.Expect(manager.Start()).To(gomega.Succeed())
			c := manager.(*nadController)
			g.Expect(c.GetNetwork("networkAPrimary")).To(gomega.BeNil())
			g.Expect(c.networkIDAllocator.GetID("networkAPrimary")).To(gomega.Equal(types.InvalidID))

			// The parent may reach its informer after the NAD. Its add event must retry the NAD.
			if kind == "UserDefinedNetwork" {
				_, err = client.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks("ns").Create(ctx, owner, metav1.CreateOptions{})
			} else {
				cudn := &udnv1.ClusterUserDefinedNetwork{
					ObjectMeta: metav1.ObjectMeta{Name: owner.Name, UID: owner.UID},
					Spec:       udnv1.ClusterUserDefinedNetworkSpec{Network: udnv1.NetworkSpec{Topology: owner.Spec.Topology, Layer2: owner.Spec.Layer2}},
				}
				_, err = client.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Create(ctx, cudn, metav1.CreateOptions{})
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Eventually(func() util.NetInfo { return c.GetNetwork("networkAPrimary") }, 5*time.Second, 10*time.Millisecond).ShouldNot(gomega.BeNil())

			// Losing just the controller reference must trigger reconciliation and cleanup.
			current, err := client.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("ns").Get(ctx, "net", metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			current.OwnerReferences = nil
			current.ResourceVersion = "2"
			current.ManagedFields = nil
			_, err = client.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("ns").Update(ctx, current, metav1.UpdateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Eventually(func() util.NetInfo { return c.GetNetwork("networkAPrimary") }, 5*time.Second, 10*time.Millisecond).Should(gomega.BeNil())

			current.OwnerReferences = nad.OwnerReferences
			current.ResourceVersion = "3"
			_, err = client.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("ns").Update(ctx, current, metav1.UpdateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Eventually(func() util.NetInfo { return c.GetNetwork("networkAPrimary") }, 5*time.Second, 10*time.Millisecond).ShouldNot(gomega.BeNil())

			if kind == "UserDefinedNetwork" {
				err = client.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks("ns").Delete(ctx, "net", metav1.DeleteOptions{})
			} else {
				err = client.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(ctx, "net", metav1.DeleteOptions{})
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Eventually(func() util.NetInfo { return c.GetNetwork("networkAPrimary") }, 5*time.Second, 10*time.Millisecond).Should(gomega.BeNil())
		})
	}
}
