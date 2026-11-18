// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	nadinformerfactory "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Advertised network subnets controller", func() {
	const (
		raName       = "ra"
		eventually   = 5 * time.Second
		consistently = 500 * time.Millisecond
	)

	var (
		nbCleanup         *libovsdbtest.Context
		addressSetFactory addressset.AddressSetFactory
		nadClient         *nadfake.Clientset
		nadLister         nadlisters.NetworkAttachmentDefinitionLister
		networkManager    *networkmanager.FakeNetworkManager
		stopCh            chan struct{}
		resourceVersion   int
		subnetsController *AdvertisedNetworkSubnetsController
	)

	// newNetworkNAD returns a NAD of the given network, named after its
	// namespace, controlled by a ClusterUserDefinedNetwork and annotated with
	// the given RouteAdvertisements names.
	newNetworkNAD := func(namespace, network string, netInfo userDefinedNetInfo, raNames ...string) *nettypes.NetworkAttachmentDefinition {
		GinkgoHelper()
		netInfo.netName = network
		netInfo.nadName = namespacedName(namespace, namespace)
		nad, err := newNetworkAttachmentDefinition(namespace, namespace, *netInfo.netconf())
		Expect(err).NotTo(HaveOccurred(), "failed to build NAD %s", namespace)
		nad.OwnerReferences = []metav1.OwnerReference{makeCUDNOwnerRef(network)}
		if raNames != nil {
			annotation, err := json.Marshal(raNames)
			Expect(err).NotTo(HaveOccurred(), "failed to marshal RouteAdvertisements names %v", raNames)
			nad.Annotations[types.OvnRouteAdvertisementsKey] = string(annotation)
		}
		return nad
	}

	// newNAD returns the NAD of a network of its own.
	newNAD := func(name string, netInfo userDefinedNetInfo, raNames ...string) *nettypes.NetworkAttachmentDefinition {
		GinkgoHelper()
		return newNetworkNAD(name, name, netInfo, raNames...)
	}

	// defaultNetworkNAD returns a NAD of the default network, as the one
	// created once a RouteAdvertisements selects the default network.
	defaultNetworkNAD := func(namespace, name string) *nettypes.NetworkAttachmentDefinition {
		return &nettypes.NetworkAttachmentDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   namespace,
				Annotations: map[string]string{types.OvnRouteAdvertisementsKey: fmt.Sprintf("[%q]", raName)},
			},
			Spec: nettypes.NetworkAttachmentDefinitionSpec{
				Config: fmt.Sprintf("{\"cniVersion\": \"%s\", \"name\": \"ovn-kubernetes\", \"type\": \"%s\"}",
					config.CNISpecVersion, config.CNI.Plugin),
			},
		}
	}

	// setResourceVersion gives the NAD a new resource version, as the API
	// server does on every write and the fake client does not.
	setResourceVersion := func(nad *nettypes.NetworkAttachmentDefinition) {
		resourceVersion++
		nad.ResourceVersion = strconv.Itoa(resourceVersion)
	}

	// notifyNADEvent does what the network manager does on a NAD event once
	// the controller runs: it notifies the registered NAD reconcilers after
	// the NAD informer has seen the event.
	notifyNADEvent := func(nad *nettypes.NetworkAttachmentDefinition, deleted bool) {
		GinkgoHelper()
		if subnetsController == nil {
			return
		}
		Eventually(func() (string, error) {
			cached, err := nadLister.NetworkAttachmentDefinitions(nad.Namespace).Get(nad.Name)
			if apierrors.IsNotFound(err) {
				return "", nil
			}
			if err != nil {
				return "", fmt.Errorf("failed to get NAD %s/%s from the informer: %w", nad.Namespace, nad.Name, err)
			}
			return cached.ResourceVersion, nil
		}).WithTimeout(eventually).Should(Equal(map[bool]string{false: nad.ResourceVersion, true: ""}[deleted]),
			"the NAD informer should see the last event of NAD %s/%s", nad.Namespace, nad.Name)
		networkManager.TriggerHandlers(namespacedName(nad.Namespace, nad.Name), nil, deleted)
	}

	createNADs := func(nads ...*nettypes.NetworkAttachmentDefinition) {
		GinkgoHelper()
		for _, nad := range nads {
			setResourceVersion(nad)
			_, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(
				context.Background(), nad, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "failed to create NAD %s/%s", nad.Namespace, nad.Name)
			notifyNADEvent(nad, false)
		}
	}

	updateNAD := func(nad *nettypes.NetworkAttachmentDefinition) {
		GinkgoHelper()
		setResourceVersion(nad)
		_, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Update(
			context.Background(), nad, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed to update NAD %s/%s", nad.Namespace, nad.Name)
		notifyNADEvent(nad, false)
	}

	// startController starts the NAD informer and the controller under test.
	startController := func() {
		GinkgoHelper()
		nadFactory := nadinformerfactory.NewSharedInformerFactory(nadClient, 0)
		nadInformer := nadFactory.K8sCniCncfIo().V1().NetworkAttachmentDefinitions()
		nadLister = nadInformer.Lister()
		// instantiating the informer registers it with the factory
		nadInformer.Informer()
		subnetsController = NewAdvertisedNetworkSubnetsController(addressSetFactory, nadLister, networkManager)

		nadFactory.Start(stopCh)
		nadFactory.WaitForCacheSync(stopCh)
		Expect(subnetsController.Start()).To(Succeed(), "failed to start the advertised network subnets controller")
	}

	addressSetSubnets := func() []string {
		addrSet, err := addressSetFactory.GetAddressSet(GetAdvertisedNetworkSubnetsAddressSetDBIDs())
		if err != nil {
			return []string{err.Error()}
		}
		v4Addrs, v6Addrs := addrSet.GetAddresses()
		return sets.List(sets.New(v4Addrs...).Insert(v6Addrs...))
	}

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed(), "failed to prepare the test config")
		config.IPv4Mode = true
		config.IPv6Mode = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true

		nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
		Expect(err).NotTo(HaveOccurred(), "failed to create the NB test harness")
		nbCleanup = cleanup
		Expect(ConfigureAdvertisedNetworkIsolation(nbClient)).To(Succeed(), "failed to configure advertised network isolation")
		addressSetFactory = addressset.NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

		nadClient = nadfake.NewSimpleClientset()
		networkManager = &networkmanager.FakeNetworkManager{}
		stopCh = make(chan struct{})
		resourceVersion = 0
		subnetsController = nil
	})

	AfterEach(func() {
		if subnetsController != nil {
			subnetsController.Stop()
		}
		close(stopCh)
		nbCleanup.Cleanup()
	})

	// No network controller runs in these tests: as with dynamic UDN
	// allocation, the networks are not rendered in this zone.
	DescribeTable("fills the address set from the NADs of the cluster",
		func(nads func() []*nettypes.NetworkAttachmentDefinition, expected ...string) {
			createNADs(nads()...)
			startController()
			if len(expected) == 0 {
				Consistently(addressSetSubnets).WithTimeout(consistently).Should(BeEmpty(),
					"no subnet should be in the address set")
				return
			}
			Eventually(addressSetSubnets).WithTimeout(eventually).Should(ConsistOf(expected),
				"the address set should hold the advertised subnets")
		},
		Entry("advertised primary layer3 network",
			func() []*nettypes.NetworkAttachmentDefinition {
				return []*nettypes.NetworkAttachmentDefinition{
					newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", ""), raName),
				}
			}, "192.168.0.0/16"),
		Entry("advertised dual-stack primary layer2 network",
			func() []*nettypes.NetworkAttachmentDefinition {
				return []*nettypes.NetworkAttachmentDefinition{
					newNAD("blue", dummyPrimaryLayer2UserDefinedNetwork("192.168.0.0/16,fd00:10::/64"), raName),
				}
			}, "192.168.0.0/16", "fd00:10::/64"),
		Entry("several advertised primary networks",
			func() []*nettypes.NetworkAttachmentDefinition {
				return []*nettypes.NetworkAttachmentDefinition{
					newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", ""), raName),
					newNAD("red", dummyPrimaryLayer2UserDefinedNetwork("192.169.0.0/16"), "other-ra", raName),
				}
			}, "192.168.0.0/16", "192.169.0.0/16"),
		Entry("advertised default network",
			func() []*nettypes.NetworkAttachmentDefinition {
				defaultNAD := config.Default.ClusterDefaultNetworkNAD
				return []*nettypes.NetworkAttachmentDefinition{defaultNetworkNAD(defaultNAD.Namespace, defaultNAD.Name)}
			}, "10.128.0.0/14"),
		Entry("primary EVPN network without a RouteAdvertisements reference",
			func() []*nettypes.NetworkAttachmentDefinition {
				return []*nettypes.NetworkAttachmentDefinition{
					newNAD("blue", dummyPrimaryLayer3EVPNCUDN("192.168.0.0/16", "")),
				}
			}, "192.168.0.0/16"),
		Entry("NADs of the same network with different subnets",
			func() []*nettypes.NetworkAttachmentDefinition {
				return []*nettypes.NetworkAttachmentDefinition{
					newNetworkNAD("blue", "shared", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", ""), raName),
					newNetworkNAD("red", "shared", dummyPrimaryLayer3UserDefinedNetwork("192.169.0.0/16", "")),
				}
			}, "192.168.0.0/16", "192.169.0.0/16"),
		Entry("primary network with a malformed RouteAdvertisements annotation",
			func() []*nettypes.NetworkAttachmentDefinition {
				malformed := newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", ""))
				malformed.Annotations[types.OvnRouteAdvertisementsKey] = "not-json"
				return []*nettypes.NetworkAttachmentDefinition{malformed}
			}, "192.168.0.0/16"),
		Entry("primary network that is not advertised",
			func() []*nettypes.NetworkAttachmentDefinition {
				return []*nettypes.NetworkAttachmentDefinition{
					newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "")),
				}
			}),
		Entry("primary network with an empty list of RouteAdvertisements",
			func() []*nettypes.NetworkAttachmentDefinition {
				return []*nettypes.NetworkAttachmentDefinition{
					newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", ""), []string{}...),
				}
			}),
		Entry("primary EVPN network that is not controlled by a ClusterUserDefinedNetwork",
			func() []*nettypes.NetworkAttachmentDefinition {
				nad := newNAD("blue", dummyPrimaryLayer3EVPNCUDN("192.168.0.0/16", ""))
				nad.OwnerReferences = nil
				return []*nettypes.NetworkAttachmentDefinition{nad}
			}, "192.168.0.0/16"),
		Entry("primary no-overlay network that is not controlled by a ClusterUserDefinedNetwork",
			func() []*nettypes.NetworkAttachmentDefinition {
				netInfo := dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "")
				netInfo.transport = types.NetworkTransportNoOverlay
				nad := newNAD("blue", netInfo)
				nad.OwnerReferences = nil
				return []*nettypes.NetworkAttachmentDefinition{nad}
			}, "192.168.0.0/16"),
		Entry("primary overlay network that is not controlled by a ClusterUserDefinedNetwork",
			func() []*nettypes.NetworkAttachmentDefinition {
				nad := newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", ""), raName)
				nad.OwnerReferences = nil
				return []*nettypes.NetworkAttachmentDefinition{nad}
			}),
		Entry("secondary network",
			func() []*nettypes.NetworkAttachmentDefinition {
				return []*nettypes.NetworkAttachmentDefinition{
					newNAD("blue", dummySecondaryLayer3UserDefinedNetwork("192.168.0.0/16", ""), raName),
				}
			}),
		Entry("default network NAD other than the cluster default one",
			func() []*nettypes.NetworkAttachmentDefinition {
				return []*nettypes.NetworkAttachmentDefinition{defaultNetworkNAD("tenant", "default-network")}
			}),
	)

	It("adds the subnets of a network once it is advertised", func() {
		createNADs(newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "")))
		startController()
		Consistently(addressSetSubnets).WithTimeout(consistently).Should(BeEmpty(),
			"the subnet of a network that is not advertised should not be in the address set")

		By("annotating the NAD with a RouteAdvertisements")
		updateNAD(newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", ""), raName))
		Eventually(addressSetSubnets).WithTimeout(eventually).Should(ConsistOf("192.168.0.0/16"),
			"the subnet of the advertised network should be in the address set")
	})

	It("removes the subnets of a network once its NAD no longer references a RouteAdvertisements", func() {
		createNADs(newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", ""), raName))
		startController()
		Eventually(addressSetSubnets).WithTimeout(eventually).Should(ConsistOf("192.168.0.0/16"),
			"the subnet of the advertised network should be in the address set")

		By("removing the RouteAdvertisements reference from the NAD")
		updateNAD(newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", "")))
		Eventually(addressSetSubnets).WithTimeout(eventually).Should(BeEmpty(),
			"the subnet of a network that is no longer advertised should not be in the address set")
	})

	It("removes the subnets of a deleted network", func() {
		blue := newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", ""), raName)
		red := newNAD("red", dummyPrimaryLayer3UserDefinedNetwork("192.169.0.0/16", ""), raName)
		createNADs(blue, red)
		startController()
		Eventually(addressSetSubnets).WithTimeout(eventually).Should(ConsistOf("192.168.0.0/16", "192.169.0.0/16"),
			"the subnets of both advertised networks should be in the address set")

		By("deleting one of the networks")
		err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(red.Namespace).Delete(
			context.Background(), red.Name, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed to delete NAD %s/%s", red.Namespace, red.Name)
		notifyNADEvent(red, true)
		Eventually(addressSetSubnets).WithTimeout(eventually).Should(ConsistOf("192.168.0.0/16"),
			"only the subnet of the remaining network should be in the address set")
	})

	DescribeTable("keeps the subnets of a network whose NAD update cannot be parsed",
		func(config string) {
			nad := newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", ""), raName)
			createNADs(nad)
			startController()
			Eventually(addressSetSubnets).WithTimeout(eventually).Should(ConsistOf("192.168.0.0/16"),
				"the subnet of the advertised network should be in the address set")

			By("updating the NAD with a configuration that cannot be parsed")
			nad.Spec.Config = config
			updateNAD(nad)
			By("advertising another network, so that the address set is synced again")
			createNADs(newNAD("red", dummyPrimaryLayer3UserDefinedNetwork("192.169.0.0/16", ""), raName))
			Eventually(addressSetSubnets).WithTimeout(eventually).Should(ConsistOf("192.168.0.0/16", "192.169.0.0/16"),
				"the subnet of the network with the rejected NAD update should stay in the address set")
		},
		Entry("invalid topology",
			`{"cniVersion": "1.0.0", "type": "ovn-k8s-cni-overlay", "name": "blue", "netAttachDefName": "blue/blue", "topology": "unknown"}`),
		Entry("CNI plugin that is not OVN-Kubernetes",
			`{"cniVersion": "1.0.0", "type": "macvlan", "name": "blue"}`),
	)

	It("removes stale subnets on start", func() {
		addrSet, err := addressSetFactory.GetAddressSet(GetAdvertisedNetworkSubnetsAddressSetDBIDs())
		Expect(err).NotTo(HaveOccurred(), "failed to get the advertised network subnets address set")
		Expect(addrSet.AddAddresses([]string{"192.170.0.0/16", "fd00:20::/64"})).To(Succeed(),
			"failed to add stale subnets to the address set")

		createNADs(newNAD("blue", dummyPrimaryLayer3UserDefinedNetwork("192.168.0.0/16", ""), raName))
		startController()
		Eventually(addressSetSubnets).WithTimeout(eventually).Should(ConsistOf("192.168.0.0/16"),
			"only the subnet of the advertised network should be in the address set")
	})
})
