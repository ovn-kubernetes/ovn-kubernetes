// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	gotesting "testing"
	"time"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("BaseUserDefinedNetworkController", func() {
	var (
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
	})

	const (
		deleteTestNetworkName  = "bluenet"
		deleteTestNADNamespace = "greenamespace"
		deleteTestNADName      = "rednad"
		deleteTestPodName      = "pod-a"
		deleteTestNodeName     = "node-a"
		deleteTestNADKey       = deleteTestNADNamespace + "/" + deleteTestNADName
	)

	setSecondaryPodNetworkWithIP := func(pod *corev1.Pod, podIP string) {
		pod.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: deleteTestNADKey}
		mac, err := net.ParseMAC("0a:58:64:80:00:03")
		Expect(err).NotTo(HaveOccurred())
		ip, ipNet, err := net.ParseCIDR(podIP)
		Expect(err).NotTo(HaveOccurred())
		ipNet.IP = ip
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, &util.PodAnnotation{
			MAC:  mac,
			IPs:  []*net.IPNet{ipNet},
			Role: types.NetworkRoleSecondary,
		}, deleteTestNADKey)
		Expect(err).NotTo(HaveOccurred())
	}
	setSecondaryPodNetwork := func(pod *corev1.Pod) {
		setSecondaryPodNetworkWithIP(pod, "100.128.0.3/16")
	}
	newDeleteTestNode := func() *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: deleteTestNodeName,
			Annotations: map[string]string{
				util.OvnNodeZoneName: types.OvnDefaultZone,
			},
		}}
	}

	It("allows the retry framework to process unscheduled UDN pods", func() {
		pod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, "", "")
		retryEligibilityChecks := map[string]func(interface{}) bool{
			"layer3":   (&Layer3UserDefinedNetworkControllerEventHandler{objType: factory.PodType}).IsResourceScheduled,
			"layer2":   (&layer2UserDefinedNetworkControllerEventHandler{objType: factory.PodType}).IsResourceScheduled,
			"localnet": (&LocalnetUserDefinedNetworkControllerEventHandler{objType: factory.PodType}).IsResourceScheduled,
		}
		for topology, isResourceScheduled := range retryEligibilityChecks {
			Expect(isResourceScheduled(pod)).To(BeTrue(), topology)
		}
	})

	It("uses a valid pod annotation directly when the applied cache is empty", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		pod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		setSecondaryPodNetwork(pod)

		lspName := util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, deleteTestNADKey)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:        lspName + "-UUID",
			Name:        lspName,
			Addresses:   []string{"0a:58:64:80:00:03 100.128.0.3"},
			ExternalIDs: map[string]string{"legacy": "true"},
		}
		// If this path queries discovery, this matching malformed row produces
		// an error because it has no NAD external ID. A valid annotation must be
		// sufficient to delete lsp without consulting this row.
		poisonDiscoveryLSP := &nbdb.LogicalSwitchPort{
			UUID: "poison-discovery-UUID",
			Name: "poison-discovery_" + util.GetLogicalPortName(pod.Namespace, pod.Name),
			ExternalIDs: map[string]string{
				"pod":                   "true",
				"namespace":             pod.Namespace,
				types.NetworkExternalID: deleteTestNetworkName,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}

		fakeOVN := NewFakeOVN(false)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp, poisonDiscoveryLSP}},
			pod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
		Expect(controller.bnc.getPortInfoForUserDefinedNetwork(pod)).To(BeNil())

		Expect(controller.bnc.removePodForUserDefinedNetwork(pod, nil)).To(Succeed())
		_, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
	})

	It("deletes an IPAM-less localnet static-IP port with malformed annotation and no cache", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.LocalnetTopology, "", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		deletedPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

		lspName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:      lspName + "-UUID",
			Name:      lspName,
			Addresses: []string{"0a:58:64:80:00:03 192.0.2.25"},
			Options:   map[string]string{"iface-id-ver": "some-other-uid"},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              deletedPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.LocalnetTopology,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLocalnetSwitch
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}

		fakeOVN := NewFakeOVN(false)
		// The deleted pod is intentionally absent from the informer. Stale state
		// is safe to remove regardless of the port's iface-id-ver.
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp}},
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]

		Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, nil)).To(Succeed())
		_, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
	})

	It("deletes a discovered Layer3 port after its switch IPAM state is gone", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		deletedPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{}

		lspName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:      lspName + "-UUID",
			Name:      lspName,
			Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
			Options:   map[string]string{"iface-id-ver": string(deletedPod.UID)},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              deletedPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.Layer3Topology,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}

		fakeOVN := NewFakeOVN(false)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp}},
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
		Expect(controller.bnc.lsManager.GetSwitchSubnets(switchName)).To(BeNil())

		Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, nil)).To(Succeed())
		_, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
	})

	DescribeTable("classifies same-name replacement ports without retrying on a missing iface-id-ver",
		func(ifaceIDVer string, currentWantsNetwork, expectDeleted bool) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
				types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
			currentPod.UID = "new-pod-uid"
			if currentWantsNetwork {
				setSecondaryPodNetwork(currentPod)
			}
			deletedPod := currentPod.DeepCopy()
			deletedPod.UID = "deleted-pod-uid"
			deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

			lspName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
			lsp := &nbdb.LogicalSwitchPort{
				UUID:      lspName + "-UUID",
				Name:      lspName,
				Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
				ExternalIDs: map[string]string{
					"pod":                    "true",
					"namespace":              deletedPod.Namespace,
					types.NetworkExternalID:  deleteTestNetworkName,
					types.NADExternalID:      deleteTestNADKey,
					types.TopologyExternalID: types.Layer2Topology,
				},
			}
			if ifaceIDVer != "" {
				lsp.Options = map[string]string{"iface-id-ver": ifaceIDVer}
			}
			switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
			logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}

			fakeOVN := NewFakeOVN(false)
			fakeOVN.startWithDBSetup(
				libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp}},
				currentPod,
				newDeleteTestNode(),
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)
			DeferCleanup(fakeOVN.shutdown)
			Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
			controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
			Expect(controller.bnc.lsManager.AddOrUpdateSwitch(switchName, ovntest.MustParseIPNets("100.128.0.0/16"), nil)).To(Succeed())

			Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
				deleteTestNADKey: {uuid: "stale-cache-UUID", logicalSwitch: switchName, appliedNetworkName: deleteTestNetworkName},
			})).To(Succeed())

			_, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
			if expectDeleted {
				Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
		},
		Entry("deletes a port owned by the deleted UID", "deleted-pod-uid", true, true),
		Entry("preserves a port owned by the current UID", "new-pod-uid", false, false),
		Entry("preserves a desired legacy port", "", true, false),
		Entry("deletes an undesired legacy port", "", false, true),
	)

	It("retries deletion of an ambiguous legacy port until replacement attachments are known", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		currentPod.UID = "new-pod-uid"
		currentPod.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: "["}
		deletedPod := currentPod.DeepCopy()
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{}

		lspName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:      lspName + "-UUID",
			Name:      lspName,
			Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              deletedPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.Layer2Topology,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}

		fakeOVN := NewFakeOVN(false)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp}},
			currentPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]

		err := controller.bnc.removePodForUserDefinedNetwork(deletedPod, nil)
		Expect(err).To(MatchError(ContainSubstring("cannot determine current network attachments for replacement pod")))
		persistedLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(err).NotTo(HaveOccurred())
		Expect(persistedLSP.Name).To(Equal(lsp.Name))
		Expect(persistedLSP.Addresses).To(Equal(lsp.Addresses))
		Expect(persistedLSP.ExternalIDs).To(Equal(lsp.ExternalIDs))

		updatedPod, err := fakeOVN.fakeClient.KubeClient.CoreV1().Pods(currentPod.Namespace).Get(
			context.TODO(), currentPod.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		delete(updatedPod.Annotations, nadapi.NetworkAttachmentAnnot)
		_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(updatedPod.Namespace).Update(
			context.TODO(), updatedPod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string {
			informerPod, err := controller.bnc.watchFactory.GetPod(currentPod.Namespace, currentPod.Name)
			if err != nil {
				return nadapi.NetworkAttachmentAnnot
			}
			return informerPod.Annotations[nadapi.NetworkAttachmentAnnot]
		}).Should(BeEmpty())

		Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, nil)).To(Succeed())
		_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
	})

	It("expires cache-only applied state when a same-name replacement has no LSP", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		currentPod.UID = "new-pod-uid"
		setSecondaryPodNetwork(currentPod)
		deletedPod := currentPod.DeepCopy()
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName}
		fakeOVN := NewFakeOVN(false)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch}},
			currentPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]

		stalePortInfo := controller.bnc.logicalPortCache.add(deletedPod, switchName, deleteTestNADKey,
			deleteTestNetworkName, "deleted-port-UUID", ovntest.MustParseMAC("0a:58:64:80:00:03"),
			ovntest.MustParseIPNets("100.128.0.3/16"))
		Expect(controller.bnc.shouldEnsurePodForUserDefinedNetwork(currentPod)).To(BeFalse())

		Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
			deleteTestNADKey: stalePortInfo,
		})).To(Succeed())

		cachedPortInfo, err := controller.bnc.logicalPortCache.get(currentPod, deleteTestNADKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(cachedPortInfo.uuid).To(Equal(stalePortInfo.uuid))
		Expect(cachedPortInfo.expires.IsZero()).To(BeFalse())
		Expect(controller.bnc.shouldEnsurePodForUserDefinedNetwork(currentPod)).To(BeTrue())
	})

	It("preserves a newer cache entry when a stale delete snapshot has no LSP", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		currentPod.UID = "new-pod-uid"
		setSecondaryPodNetwork(currentPod)
		deletedPod := currentPod.DeepCopy()
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName}
		fakeOVN := NewFakeOVN(false)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch}},
			currentPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]

		currentPortInfo := controller.bnc.logicalPortCache.add(currentPod, switchName, deleteTestNADKey,
			deleteTestNetworkName, "current-port-UUID", ovntest.MustParseMAC("0a:58:64:80:00:03"),
			ovntest.MustParseIPNets("100.128.0.3/16"))
		stalePortInfo := &lpInfo{
			uuid:               "deleted-port-UUID",
			logicalSwitch:      switchName,
			appliedNetworkName: deleteTestNetworkName,
			mac:                ovntest.MustParseMAC("0a:58:64:80:00:03"),
			ips:                ovntest.MustParseIPNets("100.128.0.3/16"),
		}

		Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
			deleteTestNADKey: stalePortInfo,
		})).To(Succeed())

		cachedPortInfo, err := controller.bnc.logicalPortCache.get(currentPod, deleteTestNADKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(cachedPortInfo.uuid).To(Equal(currentPortInfo.uuid))
		Expect(cachedPortInfo.expires.IsZero()).To(BeTrue())
		Expect(controller.bnc.shouldEnsurePodForUserDefinedNetwork(currentPod)).To(BeFalse())
	})

	DescribeTable("releases cache-only Layer3 IPAM state only when a replacement does not use the IP",
		func(replacementIP string, expectReleased bool) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
				types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, replacementIP)
			currentPod.UID = "new-pod-uid"
			setSecondaryPodNetworkWithIP(currentPod, replacementIP+"/16")
			deletedPod := currentPod.DeepCopy()
			deletedPod.UID = "deleted-pod-uid"
			deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

			fakeOVN := NewFakeOVN(false)
			fakeOVN.startWithDBSetup(
				libovsdbtest.TestSetup{},
				currentPod,
				newDeleteTestNode(),
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)
			DeferCleanup(fakeOVN.shutdown)
			Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
			controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
			switchName := controller.bnc.GetNetworkScopedSwitchName(deleteTestNodeName)
			Expect(controller.bnc.lsManager.AddOrUpdateSwitch(switchName,
				ovntest.MustParseIPNets("100.128.0.0/16"), nil)).To(Succeed())

			staleIPs := ovntest.MustParseIPNets("100.128.0.3/16")
			Expect(controller.bnc.lsManager.AllocateIPs(switchName, staleIPs)).To(Succeed())
			stalePortInfo := controller.bnc.logicalPortCache.add(deletedPod, switchName, deleteTestNADKey,
				deleteTestNetworkName, "deleted-port-UUID", ovntest.MustParseMAC("0a:58:64:80:00:03"), staleIPs)

			Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
				deleteTestNADKey: stalePortInfo,
			})).To(Succeed())

			err := controller.bnc.lsManager.AllocateIPs(switchName, staleIPs)
			if expectReleased {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(MatchError(ipallocator.ErrAllocated))
			}
		},
		Entry("when the replacement uses a different IP", "100.128.0.4", true),
		Entry("but preserves it when the replacement uses the same IP", "100.128.0.3", false),
	)

	DescribeTable("releases stale Layer3 IPAM state without deleting a replacement-owned LSP",
		func(replacementIP string, expectReleased bool) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
				types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, replacementIP)
			currentPod.UID = "new-pod-uid"
			setSecondaryPodNetworkWithIP(currentPod, replacementIP+"/16")
			deletedPod := currentPod.DeepCopy()
			deletedPod.UID = "deleted-pod-uid"
			deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

			lspName := util.GetUserDefinedNetworkLogicalPortName(currentPod.Namespace, currentPod.Name, deleteTestNADKey)
			currentLSP := &nbdb.LogicalSwitchPort{
				UUID:      lspName + "-current-UUID",
				Name:      lspName,
				Addresses: []string{"0a:58:64:80:00:04 " + replacementIP},
				Options:   map[string]string{"iface-id-ver": string(currentPod.UID)},
				ExternalIDs: map[string]string{
					"pod":                    "true",
					"namespace":              currentPod.Namespace,
					types.NetworkExternalID:  deleteTestNetworkName,
					types.NADExternalID:      deleteTestNADKey,
					types.TopologyExternalID: types.Layer3Topology,
				},
			}
			switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
			logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{currentLSP.UUID}}

			fakeOVN := NewFakeOVN(false)
			fakeOVN.startWithDBSetup(
				libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, currentLSP}},
				currentPod,
				newDeleteTestNode(),
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)
			DeferCleanup(fakeOVN.shutdown)
			Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
			controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
			Expect(controller.bnc.lsManager.AddOrUpdateSwitch(switchName,
				ovntest.MustParseIPNets("100.128.0.0/16"), nil)).To(Succeed())

			staleIPs := ovntest.MustParseIPNets("100.128.0.3/16")
			Expect(controller.bnc.lsManager.AllocateIPs(switchName, staleIPs)).To(Succeed())
			currentIPs := ovntest.MustParseIPNets(replacementIP + "/16")
			if replacementIP != "100.128.0.3" {
				Expect(controller.bnc.lsManager.AllocateIPs(switchName, currentIPs)).To(Succeed())
			}
			persistedCurrentLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
			Expect(err).NotTo(HaveOccurred())
			currentPortInfo := controller.bnc.logicalPortCache.add(currentPod, switchName, deleteTestNADKey,
				deleteTestNetworkName, persistedCurrentLSP.UUID, ovntest.MustParseMAC("0a:58:64:80:00:04"), currentIPs)
			stalePortInfo := &lpInfo{
				name:               lspName,
				uuid:               "deleted-port-UUID",
				logicalSwitch:      switchName,
				appliedNetworkName: deleteTestNetworkName,
				mac:                ovntest.MustParseMAC("0a:58:64:80:00:03"),
				ips:                staleIPs,
			}

			Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
				deleteTestNADKey: stalePortInfo,
			})).To(Succeed())

			actualLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
			Expect(err).NotTo(HaveOccurred())
			Expect(actualLSP.UUID).To(Equal(persistedCurrentLSP.UUID))
			cachedPortInfo, err := controller.bnc.logicalPortCache.get(currentPod, deleteTestNADKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(cachedPortInfo.uuid).To(Equal(currentPortInfo.uuid))
			Expect(cachedPortInfo.expires.IsZero()).To(BeTrue())

			err = controller.bnc.lsManager.AllocateIPs(switchName, staleIPs)
			if expectReleased {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(MatchError(ipallocator.ErrAllocated))
			}
		},
		Entry("when the replacement uses a different IP", "100.128.0.4", true),
		Entry("but preserves the allocation when the replacement uses the same IP", "100.128.0.3", false),
	)

	DescribeTable("discovers all durable pod ports when the applied cache is partial",
		func(malformedAnnotation, staleCachedUUID bool) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			alternateNADName := "blue-nad"
			alternateNADKey := deleteTestNADNamespace + "/" + alternateNADName
			primaryNAD := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
				types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			alternateNAD := ovntest.GenerateNAD(deleteTestNetworkName, alternateNADName, deleteTestNADNamespace,
				types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", primaryNAD)
			ovntest.AnnotateNADWithNetworkID("3", alternateNAD)

			deletedPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
			deletedPod.UID = "deleted-pod-uid"
			deletedPod.Annotations = map[string]string{}
			if malformedAnnotation {
				deletedPod.Annotations[types.OvnPodAnnotationName] = "not-json"
			}

			newLSP := func(nadKey, ip string) *nbdb.LogicalSwitchPort {
				portName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, nadKey)
				return &nbdb.LogicalSwitchPort{
					UUID:      portName + "-UUID",
					Name:      portName,
					Addresses: []string{"0a:58:64:80:00:03 " + ip},
					Options:   map[string]string{"iface-id-ver": string(deletedPod.UID)},
					ExternalIDs: map[string]string{
						"pod":                    "true",
						"namespace":              deletedPod.Namespace,
						types.NetworkExternalID:  deleteTestNetworkName,
						types.NADExternalID:      nadKey,
						types.TopologyExternalID: types.Layer2Topology,
					},
				}
			}
			primaryLSP := newLSP(deleteTestNADKey, "100.128.0.3")
			alternateLSP := newLSP(alternateNADKey, "100.128.0.4")
			switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
			logicalSwitch := &nbdb.LogicalSwitch{
				UUID: switchName + "-UUID", Name: switchName,
				Ports: []string{primaryLSP.UUID, alternateLSP.UUID},
			}

			fakeOVN := NewFakeOVN(false)
			fakeOVN.startWithDBSetup(
				libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, primaryLSP, alternateLSP}},
				newDeleteTestNode(),
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*primaryNAD, *alternateNAD}},
			)
			DeferCleanup(fakeOVN.shutdown)
			Expect(fakeOVN.NewUserDefinedNetworkController(primaryNAD)).To(Succeed())
			controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
			persistedPrimaryLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: primaryLSP.Name})
			Expect(err).NotTo(HaveOccurred())
			cachedUUID := persistedPrimaryLSP.UUID
			if staleCachedUUID {
				cachedUUID = "stale-cache-UUID"
			}
			partialPortInfo := controller.bnc.logicalPortCache.add(deletedPod, switchName, deleteTestNADKey,
				deleteTestNetworkName, cachedUUID, ovntest.MustParseMAC("0a:58:64:80:00:03"),
				ovntest.MustParseIPNets("100.128.0.3/16"))

			Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
				deleteTestNADKey: partialPortInfo,
			})).To(Succeed())
			for _, portName := range []string{primaryLSP.Name, alternateLSP.Name} {
				_, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: portName})
				Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue(), portName)
			}
		},
		Entry("when the OVN pod annotation is missing and the cached UUID is current", false, false),
		Entry("when the OVN pod annotation is malformed and the cached UUID is current", true, false),
		Entry("when the OVN pod annotation is missing and the cached UUID is stale", false, true),
		Entry("when the OVN pod annotation is malformed and the cached UUID is stale", true, true),
	)

	It("does not clear replacement cache or policy membership from a stale delete snapshot", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		currentPod.UID = "new-pod-uid"
		currentPod.Labels = map[string]string{"role": "selected"}
		setSecondaryPodNetwork(currentPod)
		deletedPod := currentPod.DeepCopy()
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

		lspName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:      lspName + "-current-UUID",
			Name:      lspName,
			Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
			Options:   map[string]string{"iface-id-ver": string(currentPod.UID)},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              deletedPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.Layer2Topology,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}
		policyPortGroup := &nbdb.PortGroup{UUID: "policy-a-UUID", Name: "policy-a", Ports: []string{lsp.UUID}}

		fakeOVN := NewFakeOVN(false)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp, policyPortGroup}},
			currentPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
		currentLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(err).NotTo(HaveOccurred())
		controller.bnc.logicalPortCache.add(currentPod, switchName, deleteTestNADKey, deleteTestNetworkName,
			currentLSP.UUID, ovntest.MustParseMAC("0a:58:64:80:00:03"), ovntest.MustParseIPNets("100.128.0.3/16"))

		policyObject := getPortNetworkPolicy("policy-a", currentPod.Namespace, "role", "selected", 80)
		np := NewNetworkPolicy(policyObject)
		np.portGroupName = policyPortGroup.Name
		np.localPodSelector, err = metav1.LabelSelectorAsSelector(&policyObject.Spec.PodSelector)
		Expect(err).NotTo(HaveOccurred())
		np.localPods.Store(lspName, currentLSP.UUID)
		np.localPodPorts.Store(localPolicyPodKey(currentPod), map[string]string{lspName: currentLSP.UUID})
		controller.bnc.networkPolicies.Store(np.getKey(), np)
		controller.bnc.addNetworkPolicyToNamespaceIndex(np)

		Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
			deleteTestNADKey: {uuid: "stale-cache-UUID", logicalSwitch: switchName, appliedNetworkName: deleteTestNetworkName},
		})).To(Succeed())

		_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(err).NotTo(HaveOccurred())
		actualPolicyPortGroup, err := libovsdbops.GetPortGroup(fakeOVN.nbClient, &nbdb.PortGroup{Name: policyPortGroup.Name})
		Expect(err).NotTo(HaveOccurred())
		Expect(actualPolicyPortGroup.Ports).To(ConsistOf(currentLSP.UUID))
		cachedPortInfo, err := controller.bnc.logicalPortCache.get(currentPod, deleteTestNADKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(cachedPortInfo.uuid).To(Equal(currentLSP.UUID))
		policyPorts, found := np.localPodPorts.Load(localPolicyPodKey(currentPod))
		Expect(found).To(BeTrue())
		Expect(policyPorts).To(Equal(map[string]string{lspName: currentLSP.UUID}))
		policyPortUUID, found := np.localPods.Load(lspName)
		Expect(found).To(BeTrue())
		Expect(policyPortUUID).To(Equal(currentLSP.UUID))
	})

	DescribeTable("clears stale policy membership for an ineligible same-name replacement",
		func(hostNetwork, unscheduled bool) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
				types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", nad)

			deletedPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
			deletedPod.UID = "deleted-pod-uid"
			deletedPod.Labels = map[string]string{"role": "selected"}
			setSecondaryPodNetwork(deletedPod)
			currentPod := deletedPod.DeepCopy()
			currentPod.UID = "new-pod-uid"
			currentPod.Spec.HostNetwork = hostNetwork
			if unscheduled {
				currentPod.Spec.NodeName = ""
			}

			portName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
			lsp := &nbdb.LogicalSwitchPort{
				UUID:        portName + "-UUID",
				Name:        portName,
				Addresses:   []string{"0a:58:64:80:00:03 100.128.0.3"},
				ExternalIDs: map[string]string{"legacy-unowned-test-port": "true"},
			}
			switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
			logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}

			fakeOVN := NewFakeOVN(false)
			fakeOVN.startWithDBSetup(
				libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp}},
				currentPod,
				newDeleteTestNode(),
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)
			DeferCleanup(fakeOVN.shutdown)
			Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
			controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
			persistedLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: portName})
			Expect(err).NotTo(HaveOccurred())

			policyObject := getPortNetworkPolicy("ineligible-replacement-policy", currentPod.Namespace, "role", "selected", 80)
			np := NewNetworkPolicy(policyObject)
			np.portGroupName = "ineligible-replacement-policy"
			np.localPodSelector, err = metav1.LabelSelectorAsSelector(&policyObject.Spec.PodSelector)
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.bnc.addPolicyToDefaultPortGroups(np, &libovsdbutil.ACLLoggingLevels{})).To(Succeed())
			Expect(libovsdbops.CreateOrUpdatePortGroups(fakeOVN.nbClient, &nbdb.PortGroup{Name: np.portGroupName})).To(Succeed())
			policyOps, err := libovsdbops.AddPortsToPortGroupOps(fakeOVN.nbClient, nil, np.portGroupName, persistedLSP.UUID)
			Expect(err).NotTo(HaveOccurred())
			portMembership := map[string]string{portName: persistedLSP.UUID}
			Expect(controller.bnc.denyPGAddPorts(np, portMembership, policyOps)).To(Succeed())
			np.setLocalPortsForPod(deletedPod, portMembership)
			controller.bnc.networkPolicies.Store(np.getKey(), np)
			controller.bnc.addNetworkPolicyToNamespaceIndex(np)

			Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, nil)).To(Succeed())

			Expect(np.getLocalPortsForPod(currentPod)).To(BeEmpty())
			_, found := np.localPods.Load(portName)
			Expect(found).To(BeFalse())
			for _, pgName := range []string{
				np.portGroupName,
				controller.bnc.defaultDenyPortGroupName(currentPod.Namespace, libovsdbutil.ACLIngress),
				controller.bnc.defaultDenyPortGroupName(currentPod.Namespace, libovsdbutil.ACLEgress),
			} {
				pg, err := libovsdbops.GetPortGroup(fakeOVN.nbClient, &nbdb.PortGroup{Name: pgName})
				Expect(err).NotTo(HaveOccurred())
				Expect(pg.Ports).To(BeEmpty(), pgName)
			}
			sharedPGs, found := controller.bnc.sharedNetpolPortGroups.Load(currentPod.Namespace)
			Expect(found).To(BeTrue())
			Expect(sharedPGs.ingressPortToPolicies).NotTo(HaveKey(portName))
			Expect(sharedPGs.egressPortToPolicies).NotTo(HaveKey(portName))
			Expect(sharedPGs.ingressPortToAssertedUUID).NotTo(HaveKey(portName))
			Expect(sharedPGs.egressPortToAssertedUUID).NotTo(HaveKey(portName))
		},
		Entry("when the replacement uses HostNetwork", true, false),
		Entry("when the replacement is unscheduled", false, true),
	)

	type dhcpTest struct {
		vmName                string
		ips                   []string
		dns                   []string
		expectedDHCPv4Options *nbdb.DHCPOptions
		expectedDHCPv6Options *nbdb.DHCPOptions
	}
	DescribeTable("with layer2 primary UDN when configuring DHCP", func(t dhcpTest) {
		layer2NAD := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		fakeOVN := NewFakeOVN(true)
		lsp := &nbdb.LogicalSwitchPort{
			Name: "vm-port",
			UUID: "vm-port-UUID",
		}
		logicalSwitch := &nbdb.LogicalSwitch{
			UUID:  "layer2-switch-UUID",
			Name:  "layer2-switch",
			Ports: []string{lsp.UUID},
		}

		initialDB := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				logicalSwitch,
				lsp,
			},
		}
		fakeOVN.startWithDBSetup(
			initialDB,
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
			&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "kube-system",
					Name:      "kube-dns",
				},
				Spec: corev1.ServiceSpec{
					ClusterIPs: t.dns,
				},
			},
		)
		defer fakeOVN.shutdown()

		Expect(fakeOVN.NewUserDefinedNetworkController(layer2NAD)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "dummy",
				Labels: map[string]string{
					kubevirtv1.AppLabel:                "virt-launcher",
					kubevirtv1.VirtualMachineNameLabel: t.vmName,
				},
				Annotations: map[string]string{
					kubevirtv1.DomainAnnotation: t.vmName,
				},
			},
		}
		ips, err := util.ParseIPNets(t.ips)
		Expect(err).ToNot(HaveOccurred())
		podAnnotation := &util.PodAnnotation{
			IPs: ips,
		}
		Expect(controller.bnc.ensureDHCP(pod, podAnnotation, lsp)).To(Succeed())
		expectedDB := []libovsdbtest.TestData{}

		By("asserting the OVN entities provisioned in the NBDB are the expected ones")
		expectedLSP := lsp.DeepCopy()
		if t.expectedDHCPv4Options != nil {
			t.expectedDHCPv4Options.UUID = "vm1-dhcpv4-UUID"
			expectedLSP.Dhcpv4Options = &t.expectedDHCPv4Options.UUID
			expectedDB = append(expectedDB, t.expectedDHCPv4Options)
		}
		if t.expectedDHCPv6Options != nil {
			t.expectedDHCPv6Options.UUID = "vm1-dhcpv6-UUID"
			expectedLSP.Dhcpv6Options = &t.expectedDHCPv6Options.UUID
			expectedDB = append(expectedDB, t.expectedDHCPv6Options)
		}
		// Refresh logical switch to have the propert ports uuid
		obtainedLogicalSwitches := []*nbdb.LogicalSwitch{}
		Expect(fakeOVN.nbClient.List(context.Background(), &obtainedLogicalSwitches)).To(Succeed())
		expectedDB = append(expectedDB,
			obtainedLogicalSwitches[0],
			expectedLSP,
		)
		Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDB))

	},
		Entry("for ipv4 singlestack", dhcpTest{
			vmName: "vm1",
			dns:    []string{"10.96.0.100"},
			ips:    []string{"192.168.100.4/24"},
			expectedDHCPv4Options: &nbdb.DHCPOptions{
				Cidr: "192.168.100.0/24",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/cidr":             "192.168.100.0/24",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:192.168.100.0/24",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
					"k8s.ovn.org/name":             "foo/vm1",
				},
				Options: map[string]string{
					"lease_time": "3500",
					"server_mac": "0a:58:a9:fe:01:01",
					"hostname":   "\"vm1\"",
					"mtu":        "1300",
					"dns_server": "10.96.0.100",
					"server_id":  "169.254.1.1",
				},
			},
		}),
		Entry("for ipv6 singlestack", dhcpTest{
			vmName: "vm1",
			dns:    []string{"2015:100:200::10"},
			ips:    []string{"2010:100:200::2/60"},
			expectedDHCPv6Options: &nbdb.DHCPOptions{
				Cidr: "2010:100:200::/60",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/name":             "foo/vm1",
					"k8s.ovn.org/cidr":             "2010.100.200../60",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:2010.100.200../60",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
				},
				Options: map[string]string{
					"server_id":  "0a:58:6d:6d:c1:50",
					"fqdn":       "\"vm1\"",
					"dns_server": "2015:100:200::10",
				},
			},
		}),
		Entry("for dualstack", dhcpTest{
			vmName: "vm1",
			dns:    []string{"10.96.0.100", "2015:100:200::10"},
			ips:    []string{"192.168.100.4/24", "2010:100:200::2/60"},
			expectedDHCPv4Options: &nbdb.DHCPOptions{
				Cidr: "192.168.100.0/24",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/cidr":             "192.168.100.0/24",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:192.168.100.0/24",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
					"k8s.ovn.org/name":             "foo/vm1",
				},
				Options: map[string]string{
					"lease_time": "3500",
					"server_mac": "0a:58:a9:fe:01:01",
					"hostname":   "\"vm1\"",
					"mtu":        "1300",
					"dns_server": "10.96.0.100",
					"server_id":  "169.254.1.1",
				},
			},
			expectedDHCPv6Options: &nbdb.DHCPOptions{
				Cidr: "2010:100:200::/60",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/name":             "foo/vm1",
					"k8s.ovn.org/cidr":             "2010.100.200../60",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:2010.100.200../60",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
				},
				Options: map[string]string{
					"server_id":  "0a:58:6d:6d:c1:50",
					"fqdn":       "\"vm1\"",
					"dns_server": "2015:100:200::10",
				},
			},
		}),
	)
	Context("enableSourceLSPFailedLiveMigration", func() {
		const (
			vmName        = "test-vm"
			nadKey        = "awips/mgmt"
			localNodeName = "node-local"
		)

		newVirtLauncherPod := func(name, nodeName string, phase corev1.PodPhase, annotations map[string]string) *corev1.Pod {
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[kubevirtv1.DomainAnnotation] = vmName
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: "awips",
					Labels: map[string]string{
						kubevirtv1.AppLabel:                "virt-launcher",
						kubevirtv1.VirtualMachineNameLabel: vmName,
					},
					CreationTimestamp: metav1.Time{Time: time.Now()},
					Annotations:       annotations,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "kubevirt.io/v1",
						Kind:       "VirtualMachineInstance",
						Name:       vmName,
					}},
				},
				Spec: corev1.PodSpec{
					NodeName: nodeName,
				},
				Status: corev1.PodStatus{
					Phase: phase,
				},
			}
			return pod
		}

		setupControllerWithDBSetup := func(dbSetup *libovsdbtest.TestSetup, pods ...*corev1.Pod) (*BaseUserDefinedNetworkController, *FakeOVN) {
			localnetNAD := ovntest.GenerateNAD("mgmt", "mgmt", "awips",
				types.LocalnetTopology, "", types.NetworkRoleSecondary)

			fakeOVN := NewFakeOVN(false)
			objs := []runtime.Object{}
			for _, p := range pods {
				objs = append(objs, p)
			}
			if dbSetup != nil {
				fakeOVN.startWithDBSetup(*dbSetup, objs...)
			} else {
				fakeOVN.start(objs...)
			}
			DeferCleanup(fakeOVN.shutdown)

			Expect(fakeOVN.NewUserDefinedNetworkController(localnetNAD)).To(Succeed())
			controller, ok := fakeOVN.userDefinedNetworkControllers["mgmt"]
			Expect(ok).To(BeTrue())

			// Set local zone to only include localNodeName
			controller.bnc.localZoneNodes = &sync.Map{}
			controller.bnc.localZoneNodes.Store(localNodeName, true)

			return controller.bnc, fakeOVN
		}

		setupController := func(pods ...*corev1.Pod) *BaseUserDefinedNetworkController {
			bnc, _ := setupControllerWithDBSetup(nil, pods...)
			return bnc
		}

		It("should skip source LSP re-enable when source pod is on a remote node", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			sourcePod := newVirtLauncherPod("virt-launcher-"+vmName+"-source", "node-remote", corev1.PodRunning, nil)
			// Target pod is local, failed (completed) — triggers LiveMigrationFailed detection
			targetPod := newVirtLauncherPod("virt-launcher-"+vmName+"-target", localNodeName, corev1.PodFailed, nil)
			// Make target created after source so DiscoverLiveMigrationStatus picks it as target
			targetPod.CreationTimestamp = metav1.Time{Time: sourcePod.CreationTimestamp.Add(time.Second)}

			bnc := setupController(sourcePod, targetPod)

			// Call with empty IPs (IPAM-less localnet) — this would fail without the locality guard
			err := bnc.enableSourceLSPFailedLiveMigration(targetPod, nadKey, "", nil)
			Expect(err).NotTo(HaveOccurred(), "should not error when source pod is on a remote node")
		})

		It("should skip source LSP re-enable when source pod LSP is not local", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			sourcePod := newVirtLauncherPod("virt-launcher-"+vmName+"-source", "node-remote", corev1.PodRunning, nil)
			targetPod := newVirtLauncherPod("virt-launcher-"+vmName+"-target", localNodeName, corev1.PodSucceeded, nil)
			targetPod.CreationTimestamp = metav1.Time{Time: sourcePod.CreationTimestamp.Add(time.Second)}

			bnc := setupController(sourcePod, targetPod)

			err := bnc.enableSourceLSPFailedLiveMigration(targetPod, nadKey, "", nil)
			Expect(err).NotTo(HaveOccurred(), "should not error when source pod LSP is not local")
		})

		It("should re-enable source LSP when source pod is local and migration failed", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			sourcePodName := "virt-launcher-" + vmName + "-source"
			sourcePod := newVirtLauncherPod(sourcePodName, localNodeName, corev1.PodRunning, nil)
			targetPod := newVirtLauncherPod("virt-launcher-"+vmName+"-target", localNodeName, corev1.PodFailed, nil)
			targetPod.CreationTimestamp = metav1.Time{Time: sourcePod.CreationTimestamp.Add(time.Second)}

			// Build LSP and switch names matching what the controller will compute:
			//   LSP name: GetUserDefinedNetworkLogicalPortName(namespace, podName, nadKey)
			//   Switch name: GetNetworkScopedSwitchName(OVNLocalnetSwitch)
			sourceLSPName := util.GetUserDefinedNetworkLogicalPortName(sourcePod.Namespace, sourcePodName, nadKey)
			sourceLSP := &nbdb.LogicalSwitchPort{
				UUID:    sourceLSPName + "-UUID",
				Name:    sourceLSPName,
				Enabled: ptr.To(false),
			}
			switchName := util.GetUserDefinedNetworkPrefix("mgmt") + types.OVNLocalnetSwitch
			logicalSwitch := &nbdb.LogicalSwitch{
				UUID:  switchName + "-UUID",
				Name:  switchName,
				Ports: []string{sourceLSP.UUID},
			}

			dbSetup := &libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					logicalSwitch,
					sourceLSP,
				},
			}

			bnc, fakeOVN := setupControllerWithDBSetup(dbSetup, sourcePod, targetPod)

			mac := "0a:58:0a:80:00:05"
			ips := []string{"10.128.0.5/24"}
			err := bnc.enableSourceLSPFailedLiveMigration(targetPod, nadKey, mac, ips)
			Expect(err).NotTo(HaveOccurred(), "should re-enable source LSP without error")

			// Verify the LSP was updated: Enabled=true and addresses set
			expectedLSP := &nbdb.LogicalSwitchPort{
				UUID:      sourceLSP.UUID,
				Name:      sourceLSPName,
				Enabled:   ptr.To(true),
				Addresses: []string{mac + " 10.128.0.5"},
			}
			expectedSwitch := logicalSwitch.DeepCopy()
			Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedSwitch, expectedLSP))
		})
	})

	It("should not fail to sync pods if namespace is gone", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		fakeOVN := NewFakeOVN(false)
		fakeOVN.start(
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"other": "3"}`,
					},
				},
			},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		// inject a real networkManager instead of a fake one, so getActiveNetworkForNamespace will get called
		nadController, err := networkmanager.NewForZone("dummyZone", nil, fakeOVN.watcher)
		Expect(err).NotTo(HaveOccurred())
		controller.bnc.networkManager = nadController.Interface()

		// simulate that we listed the pod, but namespace was deleted after
		podWithNoNamespace := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "doesnotexist",
				Name:      "dummy",
			},
		}

		var initialPodList []interface{}
		initialPodList = append(initialPodList, podWithNoNamespace)

		err = controller.bnc.syncPodsForUserDefinedNetwork(initialPodList)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should not fail to sync pods if namespace has primary UDN label but NAD not ready", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		fakeOVN := NewFakeOVN(false)
		// Create namespace with primary UDN label but no NAD
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-namespace",
				Labels: map[string]string{
					types.RequiredUDNNamespaceLabel: "",
				},
			},
		}
		fakeOVN.start(
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"other": "3"}`,
					},
				},
			},
			namespace,
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		// inject a real networkManager so GetActiveNetworkForNamespace will get called
		nadController, err := networkmanager.NewForZone("dummyZone", nil, fakeOVN.watcher)
		Expect(err).NotTo(HaveOccurred())
		controller.bnc.networkManager = nadController.Interface()

		// Pod in namespace with primary UDN label but no NAD causes InvalidPrimaryNetworkError
		podInLabeledNamespace := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "test-namespace",
				Name:      "test-pod",
			},
		}

		var initialPodList []interface{}
		initialPodList = append(initialPodList, podInLabeledNamespace)

		// Should skip pod without error when GetActiveNetworkForNamespace returns InvalidPrimaryNetworkError
		err = controller.bnc.syncPodsForUserDefinedNetwork(initialPodList)
		Expect(err).NotTo(HaveOccurred())
	})

})

func TestAdvertisedSharedGatewaySNATUsesLiveAllowedExtIPSets(t *gotesting.T) {
	for _, outboundSNAT := range []string{types.NoOverlaySNATDisabled, types.NoOverlaySNATEnabled} {
		t.Run(outboundSNAT, func(t *gotesting.T) {
			bsnc, asf, localPodSubnets := newAdvertisedSNATTestController(t, outboundSNAT, config.GatewayModeShared)
			seedAdvertisedSNATAddressSets(t, asf)

			expectAdvertisedSNATUsesLiveAllowedExtIPs(t, bsnc, asf, localPodSubnets)
		})
	}
}

func TestAdvertisedSharedGatewaySNATFailsWithoutAllowedExtIPsForFamily(t *gotesting.T) {
	bsnc, asf, localPodSubnets := newAdvertisedSNATTestController(t, types.NoOverlaySNATDisabled, config.GatewayModeShared)
	config.IPv6Mode = false
	seedAdvertisedSNATAddressSets(t, asf)

	g := NewWithT(t)
	_, err := bsnc.buildUDNEgressSNAT(localPodSubnets, "rtos-bluenet-worker1", true)
	g.Expect(err).To(MatchError(ContainSubstring(
		"failed to build allowed_ext_ips SNAT for advertised network bluenet, subnet ae70::/64: no address set UUID for IPv6",
	)))
}

func TestAdvertisedLocalGatewaySNATUsesDestinationMatch(t *gotesting.T) {
	bsnc, asf, localPodSubnets := newAdvertisedSNATTestController(t, types.NoOverlaySNATDisabled, config.GatewayModeLocal)
	seedAdvertisedSNATAddressSets(t, asf)

	expectAdvertisedSNATUsesDestinationMatch(t, bsnc, asf, localPodSubnets)
}

func newAdvertisedSNATTestController(
	t *gotesting.T,
	outboundSNAT string,
	gatewayMode config.GatewayMode,
) (*BaseUserDefinedNetworkController, *addressset.FakeAddressSetFactory, []*net.IPNet) {
	t.Helper()
	return newAdvertisedSNATTestControllerForTopology(
		t,
		types.Layer3Topology,
		"100.128.0.0/16/24,ae70::/60/64",
		outboundSNAT,
		gatewayMode,
		ovntest.MustParseIPNets("100.128.0.0/24", "ae70::/64"),
	)
}

func newAdvertisedSNATTestControllerForTopology(
	t *gotesting.T,
	topology string,
	cidrs string,
	outboundSNAT string,
	gatewayMode config.GatewayMode,
	localPodSubnets []*net.IPNet,
) (*BaseUserDefinedNetworkController, *addressset.FakeAddressSetFactory, []*net.IPNet) {
	t.Helper()
	RegisterTestingT(t)
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = gatewayMode
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
	config.Gateway.V6MasqueradeSubnet = "fd69::/112"

	const (
		networkName = "bluenet"
		nadName     = "rednad"
		namespace   = "greenamespace"
	)
	nad := ovntest.GenerateNADWithConfig(nadName, namespace, fmt.Sprintf(`
{
        "cniVersion": "1.1.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology": %q,
        "subnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q,
        "role": %q,
        "transport": %q,
        "outboundSNAT": %q
}
`,
		networkName,
		topology,
		cidrs,
		fmt.Sprintf("%s/%s", namespace, nadName),
		types.NetworkRolePrimary,
		types.NetworkTransportNoOverlay,
		outboundSNAT,
	))
	ovntest.AnnotateNADWithNetworkID("3", nad)
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		t.Fatalf("failed to parse NAD: %v", err)
	}

	controllerName := getNetworkControllerName(netInfo.GetNetworkName())
	asf := addressset.NewFakeAddressSetFactory(controllerName)
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker1",
			Annotations: map[string]string{
				util.OVNNodeHostCIDRs: `["192.168.126.11/24","fd00::11/64"]`,
			},
		},
	}
	clientSet := util.GetOVNClientset(&corev1.NodeList{Items: []corev1.Node{node}}).GetOVNKubeControllerClientset()
	watchFactory, err := factory.NewOVNKubeControllerWatchFactory(clientSet)
	if err != nil {
		t.Fatalf("failed to create watch factory: %v", err)
	}
	if err := watchFactory.Start(); err != nil {
		t.Fatalf("failed to start watch factory: %v", err)
	}
	t.Cleanup(watchFactory.Shutdown)

	nbClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{})
	if err != nil {
		t.Fatalf("failed to create libovsdb test harness: %v", err)
	}
	t.Cleanup(libovsdbCleanup.Cleanup)
	addressSetManager := addresssetmanager.NewAddressSetManager(
		watchFactory.PodCoreInformer(),
		watchFactory.NamespaceInformer(),
		watchFactory.NodeCoreInformer(),
		nbClient,
		networkmanager.Default().Interface().GetNetworkNameForNADKey,
	)
	return &BaseUserDefinedNetworkController{
			BaseNetworkController: BaseNetworkController{
				controllerName:      controllerName,
				ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
				addressSetFactory:   asf,
				addressSetManager:   addressSetManager,
			},
		},
		asf,
		localPodSubnets
}

func seedAdvertisedSNATAddressSets(t *gotesting.T, asf addressset.AddressSetFactory) {
	t.Helper()
	nodeIPsASIDs := getClusterNodeIPsAddrSetDbIDsForTest()
	if _, err := asf.NewAddressSet(nodeIPsASIDs, []string{"192.168.126.11", "fd00::11"}); err != nil {
		t.Fatalf("failed to create node IP address set: %v", err)
	}

	svcIPsASIDs := udnenabledsvc.GetAddressSetDBIDs()
	if _, err := asf.NewAddressSet(svcIPsASIDs, []string{"10.96.0.10", "fd02::10"}); err != nil {
		t.Fatalf("failed to create UDN-enabled service address set: %v", err)
	}
}

func expectAdvertisedSNATUsesLiveAllowedExtIPs(
	t *gotesting.T,
	bsnc *BaseUserDefinedNetworkController,
	asf addressset.AddressSetFactory,
	localPodSubnets []*net.IPNet,
) {
	t.Helper()
	g := NewWithT(t)

	snats, err := bsnc.buildUDNEgressSNAT(localPodSubnets, "rtos-bluenet-worker1", true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(snats).To(HaveLen(4))

	nodeIPsAS, err := asf.GetAddressSet(getClusterNodeIPsAddrSetDbIDsForTest())
	g.Expect(err).NotTo(HaveOccurred())
	nodeIPv4ASUUID, nodeIPv6ASUUID := nodeIPsAS.GetASUUID()
	svcIPsAS, err := asf.GetAddressSet(udnenabledsvc.GetAddressSetDBIDs())
	g.Expect(err).NotTo(HaveOccurred())
	svcIPv4ASUUID, svcIPv6ASUUID := svcIPsAS.GetASUUID()

	actualAllowedExtIPsByLogicalIP := map[string][]string{}
	for _, snat := range snats {
		g.Expect(snat.Match).To(Equal(""))
		g.Expect(snat.AllowedExtIPs).NotTo(BeNil())
		g.Expect(snat.ExemptedExtIPs).To(BeNil())
		actualAllowedExtIPsByLogicalIP[snat.LogicalIP] = append(
			actualAllowedExtIPsByLogicalIP[snat.LogicalIP],
			*snat.AllowedExtIPs,
		)
	}
	g.Expect(actualAllowedExtIPsByLogicalIP["100.128.0.0/24"]).To(ConsistOf(nodeIPv4ASUUID, svcIPv4ASUUID))
	g.Expect(actualAllowedExtIPsByLogicalIP["ae70::/64"]).To(ConsistOf(nodeIPv6ASUUID, svcIPv6ASUUID))
}

func expectAdvertisedSNATUsesDestinationMatch(
	t *gotesting.T,
	bsnc *BaseUserDefinedNetworkController,
	asf addressset.AddressSetFactory,
	localPodSubnets []*net.IPNet,
) {
	t.Helper()
	g := NewWithT(t)

	snats, err := bsnc.buildUDNEgressSNAT(localPodSubnets, "rtos-bluenet-worker1", true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(snats).To(HaveLen(2))

	nodeIPsAS, err := asf.GetAddressSet(getClusterNodeIPsAddrSetDbIDsForTest())
	g.Expect(err).NotTo(HaveOccurred())
	svcIPsAS, err := asf.GetAddressSet(udnenabledsvc.GetAddressSetDBIDs())
	g.Expect(err).NotTo(HaveOccurred())

	dstMac := util.IPAddrToHWAddr(bsnc.GetNodeManagementIP(localPodSubnets[0]).IP)
	dstMacMatch := getMasqueradeManagementIPSNATMatch(dstMac.String())
	v4Match := getClusterNodesDestinationBasedSNATMatch(utilnet.IPv4, nodeIPsAS, svcIPsAS)
	v6Match := getClusterNodesDestinationBasedSNATMatch(utilnet.IPv6, nodeIPsAS, svcIPsAS)

	g.Expect(snats[0].Match).To(Equal(fmt.Sprintf("%s && %s", dstMacMatch, v4Match)))
	g.Expect(snats[0].AllowedExtIPs).To(BeNil())
	g.Expect(snats[0].ExemptedExtIPs).To(BeNil())

	g.Expect(snats[1].Match).To(Equal(fmt.Sprintf("%s && %s", dstMacMatch, v6Match)))
	g.Expect(snats[1].AllowedExtIPs).To(BeNil())
	g.Expect(snats[1].ExemptedExtIPs).To(BeNil())
}
