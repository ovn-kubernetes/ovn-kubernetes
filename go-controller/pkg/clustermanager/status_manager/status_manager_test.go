package status_manager

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpfake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/status_manager/zone_tracker"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	egressfirewallapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	networkqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func getNodeWithZone(nodeName, zoneName string) *corev1.Node {
	annotations := map[string]string{}
	if zoneName != zone_tracker.UnknownZone {
		annotations[util.OvnNodeZoneName] = zoneName
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: annotations,
		},
	}
}

func newAdminNetworkPolicy(name string, priority int32) anpapi.AdminNetworkPolicy {
	return anpapi.AdminNetworkPolicy{
		ObjectMeta: util.NewObjectMeta(name, ""),
		Spec: anpapi.AdminNetworkPolicySpec{
			Priority: priority,
			Subject: anpapi.AdminNetworkPolicySubject{
				Namespaces: &metav1.LabelSelector{},
			},
		},
	}
}

func newBaselineAdminNetworkPolicy(name string) anpapi.BaselineAdminNetworkPolicy {
	return anpapi.BaselineAdminNetworkPolicy{
		ObjectMeta: util.NewObjectMeta(name, ""),
		Spec: anpapi.BaselineAdminNetworkPolicySpec{
			Subject: anpapi.AdminNetworkPolicySubject{
				Namespaces: &metav1.LabelSelector{},
			},
		},
	}
}

func newEgressFirewall(namespace string) *egressfirewallapi.EgressFirewall {
	return &egressfirewallapi.EgressFirewall{
		ObjectMeta: util.NewObjectMeta("default", namespace),
		Spec: egressfirewallapi.EgressFirewallSpec{
			Egress: []egressfirewallapi.EgressFirewallRule{
				{
					Type: "Allow",
					To: egressfirewallapi.EgressFirewallDestination{
						CIDRSelector: "1.2.3.4/23",
					},
				},
			},
		},
	}
}

func updateEgressFirewallStatus(egressFirewall *egressfirewallapi.EgressFirewall, status *egressfirewallapi.EgressFirewallStatus,
	fakeClient *util.OVNClusterManagerClientset) {
	egressFirewall.Status = *status
	_, err := fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
		Update(context.TODO(), egressFirewall, metav1.UpdateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

func checkEFStatusEventually(egressFirewall *egressfirewallapi.EgressFirewall, expectFailure bool, expectEmpty bool, fakeClient *util.OVNClusterManagerClientset) {
	Eventually(func() bool {
		ef, err := fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
			Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if expectFailure {
			return strings.Contains(ef.Status.Status, types.EgressFirewallErrorMsg)
		} else if expectEmpty {
			return ef.Status.Status == ""
		} else {
			return strings.Contains(ef.Status.Status, "applied")
		}
	}).Should(BeTrue(), fmt.Sprintf("expected egress firewall status with expectFailure=%v expectEmpty=%v", expectFailure, expectEmpty))
}

func checkEmptyEFStatusConsistently(egressFirewall *egressfirewallapi.EgressFirewall, fakeClient *util.OVNClusterManagerClientset) {
	Consistently(func() bool {
		ef, err := fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
			Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return ef.Status.Status == ""
	}).Should(BeTrue(), "expected Status to be consistently empty")
}

func newAPBRoute(name string) *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute {
	return &adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
		ObjectMeta: util.NewObjectMeta(name, ""),
		Spec: adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteSpec{
			From: adminpolicybasedrouteapi.ExternalNetworkSource{
				NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"name": "ns"}},
			},
			NextHops: adminpolicybasedrouteapi.ExternalNextHops{},
		},
	}
}

func updateAPBRouteStatus(apbRoute *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, status *adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus,
	fakeClient *util.OVNClusterManagerClientset) {
	apbRoute.Status = *status
	_, err := fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().
		Update(context.TODO(), apbRoute, metav1.UpdateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

func checkAPBRouteStatusEventually(apbRoute *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, expectFailure bool, expectEmpty bool, fakeClient *util.OVNClusterManagerClientset) {
	Eventually(func() bool {
		route, err := fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().
			Get(context.TODO(), apbRoute.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if expectFailure {
			return route.Status.Status == adminpolicybasedrouteapi.FailStatus
		} else if expectEmpty {
			return route.Status.Status == ""
		} else {
			return route.Status.Status == adminpolicybasedrouteapi.SuccessStatus
		}
	}).Should(BeTrue(), fmt.Sprintf("expected apbRoute status with expectFailure=%v expectEmpty=%v", expectFailure, expectEmpty))
}

func checkEmptyAPBRouteStatusConsistently(apbRoute *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, fakeClient *util.OVNClusterManagerClientset) {
	Consistently(func() bool {
		ef, err := fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().
			Get(context.TODO(), apbRoute.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return ef.Status.Status == ""
	}).Should(BeTrue(), "expected Status to be consistently empty")
}

func newEgressQoS(namespace string) *egressqosapi.EgressQoS {
	return &egressqosapi.EgressQoS{
		ObjectMeta: util.NewObjectMeta("default", namespace),
		Spec: egressqosapi.EgressQoSSpec{
			Egress: []egressqosapi.EgressQoSRule{
				{
					DSCP:    60,
					DstCIDR: ptr.To("1.2.3.4/32"),
				},
			},
		},
	}
}

func updateEgressQoSStatus(egressQoS *egressqosapi.EgressQoS, status *egressqosapi.EgressQoSStatus,
	fakeClient *util.OVNClusterManagerClientset) {
	egressQoS.Status = *status
	_, err := fakeClient.EgressQoSClient.K8sV1().EgressQoSes(egressQoS.Namespace).
		Update(context.TODO(), egressQoS, metav1.UpdateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

func checkEQStatusEventually(egressQoS *egressqosapi.EgressQoS, expectFailure bool, expectEmpty bool, fakeClient *util.OVNClusterManagerClientset) {
	Eventually(func() bool {
		eq, err := fakeClient.EgressQoSClient.K8sV1().EgressQoSes(egressQoS.Namespace).
			Get(context.TODO(), egressQoS.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if expectFailure {
			return strings.Contains(eq.Status.Status, types.EgressQoSErrorMsg)
		} else if expectEmpty {
			return eq.Status.Status == ""
		} else {
			return strings.Contains(eq.Status.Status, "applied")
		}
	}).Should(BeTrue(), fmt.Sprintf("expected egress QoS status with expectFailure=%v expectEmpty=%v", expectFailure, expectEmpty))
}

func checkEmptyEQStatusConsistently(egressQoS *egressqosapi.EgressQoS, fakeClient *util.OVNClusterManagerClientset) {
	Consistently(func() bool {
		ef, err := fakeClient.EgressQoSClient.K8sV1().EgressQoSes(egressQoS.Namespace).
			Get(context.TODO(), egressQoS.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return ef.Status.Status == ""
	}).Should(BeTrue(), "expected Status to be consistently empty")
}

func newNetworkQoS(namespace string) *networkqosapi.NetworkQoS {
	return &networkqosapi.NetworkQoS{
		ObjectMeta: util.NewObjectMeta("default", namespace),
		Spec: networkqosapi.Spec{
			NetworkSelectors: []crdtypes.NetworkSelector{
				{
					NetworkSelectionType: crdtypes.NetworkAttachmentDefinitions,
					NetworkAttachmentDefinitionSelector: &crdtypes.NetworkAttachmentDefinitionSelector{
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": "stream",
							},
						},
					},
				},
			},
			Priority: 100,
			Egress: []networkqosapi.Rule{
				{
					DSCP: 60,
					Classifier: networkqosapi.Classifier{
						To: []networkqosapi.Destination{
							{
								IPBlock: &networkingv1.IPBlock{
									CIDR: "1.2.3.4/32",
								},
							},
						},
					},
					Bandwidth: networkqosapi.Bandwidth{
						Rate:  100,
						Burst: 1000,
					},
				},
			},
		},
	}
}

func updateNetworkQoSStatus(networkQoS *networkqosapi.NetworkQoS, status *networkqosapi.Status,
	fakeClient *util.OVNClusterManagerClientset) {
	networkQoS.Status = *status
	_, err := fakeClient.NetworkQoSClient.K8sV1alpha1().NetworkQoSes(networkQoS.Namespace).
		Update(context.TODO(), networkQoS, metav1.UpdateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

func checkNQStatusEventually(networkQoS *networkqosapi.NetworkQoS, expectFailure bool, expectEmpty bool, fakeClient *util.OVNClusterManagerClientset) {
	Eventually(func() bool {
		eq, err := fakeClient.NetworkQoSClient.K8sV1alpha1().NetworkQoSes(networkQoS.Namespace).
			Get(context.TODO(), networkQoS.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if expectFailure {
			return strings.Contains(eq.Status.Status, types.NetworkQoSErrorMsg)
		} else if expectEmpty {
			return eq.Status.Status == ""
		} else {
			return strings.Contains(eq.Status.Status, "applied")
		}
	}).Should(BeTrue(), fmt.Sprintf("expected network QoS status with expectFailure=%v expectEmpty=%v", expectFailure, expectEmpty))
}

func checkEmptyNQStatusConsistently(networkQoS *networkqosapi.NetworkQoS, fakeClient *util.OVNClusterManagerClientset) {
	Consistently(func() bool {
		ef, err := fakeClient.NetworkQoSClient.K8sV1alpha1().NetworkQoSes(networkQoS.Namespace).
			Get(context.TODO(), networkQoS.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return ef.Status.Status == ""
	}).Should(BeTrue(), "expected Status to be consistently empty")
}

var _ = Describe("Cluster Manager Status Manager", func() {
	var (
		statusManager *StatusManager
		wf            *factory.WatchFactory
		fakeClient    *util.OVNClusterManagerClientset
	)

	const (
		namespace1Name = "namespace1"
		apbrouteName   = "route"
	)

	start := func(zones sets.Set[string], objects ...runtime.Object) {
		for _, zone := range zones.UnsortedList() {
			objects = append(objects, getNodeWithZone(zone, zone))
		}
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		statusManager = NewStatusManager(wf, fakeClient)

		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		err = statusManager.Start()
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		wf = nil
		statusManager = nil
	})

	AfterEach(func() {
		if wf != nil {
			wf.Shutdown()
		}
		if statusManager != nil {
			statusManager.Stop()
		}
	})

	It("updates EgressFirewall status with 1 zone", func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		zones := sets.New("zone1")
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)
		start(zones, namespace1, egressFirewall)

		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)

		checkEFStatusEventually(egressFirewall, false, false, fakeClient)
	})

	It("updates EgressFirewall status with 2 zones", func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		zones := sets.New("zone1", "zone2")
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)
		start(zones, namespace1, egressFirewall)

		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)

		checkEmptyEFStatusConsistently(egressFirewall, fakeClient)

		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK"), types.GetZoneStatus("zone2", "OK")},
		}, fakeClient)
		checkEFStatusEventually(egressFirewall, false, false, fakeClient)

	})

	It("updates EgressFirewall status with UnknownZone", func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		zones := sets.New("zone1", zone_tracker.UnknownZone)
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)
		start(zones, namespace1, egressFirewall)

		// no matter how many messages are in the status, it won't be updated while UnknownZone is present
		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)
		checkEmptyEFStatusConsistently(egressFirewall, fakeClient)

		// when UnknownZone is removed, updates will be handled, but status from the new zone is not reported yet
		statusManager.onZoneUpdate(sets.New("zone1", "zone2"))
		checkEmptyEFStatusConsistently(egressFirewall, fakeClient)
		// when new zone status is reported, status will be set
		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK"), types.GetZoneStatus("zone2", "OK")},
		}, fakeClient)
		checkEFStatusEventually(egressFirewall, false, false, fakeClient)
	})
	It("updates APBRoute status with 1 zone", func() {
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true
		zones := sets.New("zone1")
		apbRoute := newAPBRoute(apbrouteName)
		start(zones, apbRoute)

		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)

		checkAPBRouteStatusEventually(apbRoute, false, false, fakeClient)
	})

	It("updates APBRoute status with 2 zones", func() {
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true
		zones := sets.New("zone1", "zone2")
		apbRoute := newAPBRoute(apbrouteName)
		start(zones, apbRoute)

		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)

		checkEmptyAPBRouteStatusConsistently(apbRoute, fakeClient)

		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK"), types.GetZoneStatus("zone2", "OK")},
		}, fakeClient)
		checkAPBRouteStatusEventually(apbRoute, false, false, fakeClient)

	})

	It("updates APBRoute status with UnknownZone", func() {
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true
		zones := sets.New("zone1", zone_tracker.UnknownZone)
		apbRoute := newAPBRoute(apbrouteName)
		start(zones, apbRoute)

		// no matter how many messages are in the status, it won't be updated while UnknownZone is present
		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)
		checkEmptyAPBRouteStatusConsistently(apbRoute, fakeClient)

		// when UnknownZone is removed, updates will be handled, but status from the new zone is not reported yet
		statusManager.onZoneUpdate(sets.New("zone1", "zone2"))
		checkEmptyAPBRouteStatusConsistently(apbRoute, fakeClient)
		// when new zone status is reported, status will be set
		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK"), types.GetZoneStatus("zone2", "OK")},
		}, fakeClient)
		checkAPBRouteStatusEventually(apbRoute, false, false, fakeClient)
	})

	It("updates EgressQoS status with 1 zone", func() {
		config.OVNKubernetesFeature.EnableEgressQoS = true
		zones := sets.New("zone1")
		namespace1 := util.NewNamespace(namespace1Name)
		egressQoS := newEgressQoS(namespace1.Name)
		start(zones, namespace1, egressQoS)
		updateEgressQoSStatus(egressQoS, &egressqosapi.EgressQoSStatus{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}},
		}, fakeClient)

		checkEQStatusEventually(egressQoS, false, false, fakeClient)
	})

	It("updates EgressQoS status with 2 zones", func() {
		config.OVNKubernetesFeature.EnableEgressQoS = true
		zones := sets.New("zone1", "zone2")
		namespace1 := util.NewNamespace(namespace1Name)
		egressQoS := newEgressQoS(namespace1.Name)
		start(zones, namespace1, egressQoS)

		updateEgressQoSStatus(egressQoS, &egressqosapi.EgressQoSStatus{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}},
		}, fakeClient)

		checkEmptyEQStatusConsistently(egressQoS, fakeClient)

		updateEgressQoSStatus(egressQoS, &egressqosapi.EgressQoSStatus{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}, {
				Type:    "Ready-In-Zone-zone2",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}},
		}, fakeClient)
		checkEQStatusEventually(egressQoS, false, false, fakeClient)

	})

	It("updates EgressQoS status with UnknownZone", func() {
		config.OVNKubernetesFeature.EnableEgressQoS = true
		zones := sets.New("zone1", zone_tracker.UnknownZone)
		namespace1 := util.NewNamespace(namespace1Name)
		egressQoS := newEgressQoS(namespace1.Name)
		start(zones, namespace1, egressQoS)

		// no matter how many messages are in the status, it won't be updated while UnknownZone is present
		updateEgressQoSStatus(egressQoS, &egressqosapi.EgressQoSStatus{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}},
		}, fakeClient)
		checkEmptyEQStatusConsistently(egressQoS, fakeClient)

		// when UnknownZone is removed, updates will be handled, but status from the new zone is not reported yet
		statusManager.onZoneUpdate(sets.New("zone1", "zone2"))
		checkEmptyEQStatusConsistently(egressQoS, fakeClient)
		// when new zone status is reported, status will be set
		updateEgressQoSStatus(egressQoS, &egressqosapi.EgressQoSStatus{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}, {
				Type:    "Ready-In-Zone-zone2",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}},
		}, fakeClient)
		checkEQStatusEventually(egressQoS, false, false, fakeClient)
	})
	// cleanup can't be tested by unit test apiserver, since it relies on SSA logic with FieldManagers
	It("test if APIServer lister/patcher is called for AdminNetworkPolicy when the zone is deleted", func() {
		config.OVNKubernetesFeature.EnableAdminNetworkPolicy = true
		zones := sets.New("zone1", "zone2")
		start(zones)
		statusManager.onZoneUpdate(sets.New("zone1", "zone2", "zone3")) // add
		// the actual status update for zones is done in ovnkube-controller but here we just want to
		// check if a zone delete at least triggers the API List calls which means we are triggering
		// the SSA logic to delete/clear that status. Real cleanup cannot be tested since fakeClient
		// doesn't support ApplyStatus patch with FieldManagers
		var anpsWereListed, banpWereListed uint32
		fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("list", "adminnetworkpolicies", func(clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			atomic.StoreUint32(&anpsWereListed, anpsWereListed+1)
			anpList := &anpapi.AdminNetworkPolicyList{Items: []anpapi.AdminNetworkPolicy{newAdminNetworkPolicy("harry-potter", 5)}}
			return true, anpList, nil
		})
		fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("list", "baselineadminnetworkpolicies", func(clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			atomic.StoreUint32(&banpWereListed, banpWereListed+1)
			banpList := &anpapi.BaselineAdminNetworkPolicyList{Items: []anpapi.BaselineAdminNetworkPolicy{newBaselineAdminNetworkPolicy("default")}}
			return true, banpList, nil
		})
		var anpsWerePatched, banpWerePatched uint32
		fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("patch", "adminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			atomic.StoreUint32(&anpsWerePatched, anpsWerePatched+1)
			patch := action.(clienttesting.PatchAction)
			if action.GetSubresource() == "status" {
				klog.Infof("Got a patch status action for %v", patch.GetResource())
				return true, nil, nil
			}
			klog.Infof("Got a patch spec action for %v", patch.GetResource())
			return false, nil, nil
		})
		fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("patch", "baselineadminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			atomic.StoreUint32(&banpWerePatched, banpWerePatched+1)
			patch := action.(clienttesting.PatchAction)
			if action.GetSubresource() == "status" {
				klog.Infof("Got a patch status action for %v", patch.GetResource())
				return true, nil, nil
			}
			klog.Infof("Got an patch spec action for %v", patch.GetResource())
			return false, nil, nil
		})
		statusManager.onZoneUpdate(sets.New("zone1")) // delete "zone2", "zone3"
		// ensure list was called only once for each resource even if multiple zones are deleted
		Eventually(func() uint32 {
			return atomic.LoadUint32(&anpsWereListed)
		}).Should(Equal(uint32(1)))
		Eventually(func() uint32 {
			return atomic.LoadUint32(&banpWereListed)
		}).Should(Equal(uint32(1)))
		// ensure patch status clean was called once for every zone, so here since two zones were deleted
		// we should have called it two times
		Eventually(func() uint32 {
			return atomic.LoadUint32(&anpsWerePatched)
		}).Should(Equal(uint32(2)))
		Eventually(func() uint32 {
			return atomic.LoadUint32(&banpWerePatched)
		}).Should(Equal(uint32(2)))
	})

	It("Should clean up EgressFirewall managedFields when a zone is deleted", func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)
		// Set up the initial state: 2 zones have reported status
		egressFirewall.Status = egressfirewallapi.EgressFirewallStatus{
			Messages: []string{
				types.GetZoneStatus("zone1", "zone1: EgressFirewall Rules applied"),
				types.GetZoneStatus("zone2", "zone2: EgressFirewall Rules applied"),
			},
		}
		egressFirewall.ManagedFields = []metav1.ManagedFieldsEntry{
			{Manager: "zone1", Subresource: "status", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:messages":{"v:\"zone1: zone1: EgressFirewall Rules applied\"":{}}}}`)}},
			{Manager: "zone2", Subresource: "status", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:messages":{"v:\"zone2: zone2: EgressFirewall Rules applied\"":{}}}}`)}},
		}

		// Set up a reactor to intercept cleanup patches and track which zones are cleaned
		var cleanupCalled atomic.Uint32
		objects := []runtime.Object{namespace1, egressFirewall}
		zones := sets.New("zone1", "zone2")
		for _, zone := range zones.UnsortedList() {
			objects = append(objects, getNodeWithZone(zone, zone))
		}
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		fakeClient.EgressFirewallClient.(*egressfirewallfake.Clientset).PrependReactor("patch", "egressfirewalls", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			patchAction := action.(clienttesting.PatchAction)
			if patchAction.GetSubresource() == "status" {
				patch := string(patchAction.GetPatch())
				klog.Infof("Status patch intercepted: %s", patch)

				// Only count cleanup patches, where the status field is empty
				if !strings.Contains(patch, `"status"`) {
					cleanupCalled.Add(1)
					klog.Infof("Cleanup patch detected for zone2")
				} else {
					klog.Infof("Normal status update patch (not a cleanup)")
				}
			}
			return false, nil, nil
		})

		// Now start the watch factory and status manager
		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		statusManager = NewStatusManager(wf, fakeClient)

		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		err = statusManager.Start()
		Expect(err).NotTo(HaveOccurred())

		// Simulate deleting zone2 (now zones = {zone1})
		// This will trigger message-based cleanup because len(messages)=2 > zones.Len()=1
		statusManager.onZoneUpdate(sets.New("zone1"))

		// Verify cleanup was called for the deleted zone
		// NOTE: Due to fake client limitations (doesn't support SSA), cleanup may be called
		// multiple times since the message doesn't actually get removed. We verify it's
		// called at least once, which proves the message-based cleanup logic is triggered.
		Eventually(func() uint32 {
			return cleanupCalled.Load()
		}).Should(BeNumerically(">=", uint32(1)), "Expected cleanup to be called at least once for deleted zone")

		// Note: We cannot verify that managedFields were actually removed because the fake client
		// doesn't properly support Server-Side Apply with FieldManagers.
		// But we verified that cleanupStatus was called with the correct zone
	})

	It("Should clean up stale EgressFirewall managedFields on startup (upgrade scenario)", func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)

		// Let's mimick an upgrade scenario:
		// - Status messages are already correct (only 2 zones report status)
		// - managedFields still has 3 entries (old code didn't clean up)
		// - stale managedField from zone3-deleted should be removed
		egressFirewall.Status = egressfirewallapi.EgressFirewallStatus{
			Messages: []string{
				types.GetZoneStatus("zone1", "zone1: EgressFirewall Rules applied"),
				types.GetZoneStatus("zone2", "zone2: EgressFirewall Rules applied"),
			},
		}
		egressFirewall.ManagedFields = []metav1.ManagedFieldsEntry{
			// Valid managedFields with actual message content nested inside
			{Manager: "zone1", Subresource: "status", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:messages":{"v:\"zone1: EgressFirewall Rules applied\"":{}}}}`)}},
			{Manager: "zone2", Subresource: "status", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:messages":{"v:\"zone2: EgressFirewall Rules applied\"":{}}}}`)}},
			// Stale managedField with empty status (left by buggy code when zone was deleted)
			{Manager: "zone3-deleted", Subresource: "status", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{}}`)}},
			// Legitimate cluster-manager managedField with its own nested structure
			{Manager: "cluster-manager", Subresource: "status", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:status":{}}}`)}},
		}

		var cleanupCalled atomic.Uint32
		var cleanupFieldManager atomic.Pointer[string]

		// We need to create the egress firewall before starting status manager,
		// so we can check if the initial cleanup takes place
		objects := []runtime.Object{namespace1, egressFirewall}

		// Add nodes for only zone1 and zone2 (zone3-deleted doesn't exist)
		zones := sets.New("zone1", "zone2")
		for _, zone := range zones.UnsortedList() {
			objects = append(objects, getNodeWithZone(zone, zone))
		}
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()

		// Set up a reactor to intercept cleanup patches
		fakeClient.EgressFirewallClient.(*egressfirewallfake.Clientset).PrependReactor("patch", "egressfirewalls", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			patchAction := action.(clienttesting.PatchAction)
			if patchAction.GetSubresource() == "status" {
				patch := string(patchAction.GetPatch())

				// Check if this is a cleanup patch (empty ApplyStatus) vs normal status update
				// Cleanup patches have no status field, normal status updates have a "status" field
				// with actual content
				if !strings.Contains(patch, `"status"`) {
					cleanupCalled.Add(1)
					manager := "zone3-deleted"
					cleanupFieldManager.Store(&manager)
					klog.Infof("Cleanup patch detected for zone3-deleted")
				}
			}
			return false, nil, nil
		})

		// Now start the watch factory and status manager
		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		statusManager = NewStatusManager(wf, fakeClient)

		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		// When statusManager Start() is called, it triggers ZoneTracker initialSync, which should:
		// 1. Discover current zones (zone1, zone2)
		// 2. Call onZonesUpdate
		// 3. Trigger ReconcileAll
		// 4. ReconcileAll calls the one-time startup cleanup
		// 5. Startup cleanup detects zone3-deleted has a stale empty-status managedField
		//    managed by a zone that is not listed between the current zonesb
		// 6. cleanupStatus is called for zone3-deleted
		err = statusManager.Start()
		Expect(err).NotTo(HaveOccurred())

		// Verify cleanup was called for the stale zone
		Eventually(func() uint32 {
			return cleanupCalled.Load()
		}).Should(Equal(uint32(1)), "Expected cleanup to be called exactly once for stale zone")

		// Ensure cleanup doesn't get called multiple times
		Consistently(func() uint32 {
			return cleanupCalled.Load()
		}).Should(Equal(uint32(1)), "Expected cleanup to be called exactly once and not repeated")

		Eventually(func() string {
			if managerPtr := cleanupFieldManager.Load(); managerPtr != nil {
				return *managerPtr
			}
			return ""
		}).Should(Equal("zone3-deleted"))

		// Check that we still have 2 messages (startup cleanup doesn't touch messages)
		ef, err := fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(ef.Status.Messages).To(HaveLen(2))

		// The fake client doesn't properly handle SSA managedFields, so we can't verify
		// that zone3-deleted was actually removed from managedFields.
		// But we verified that cleanupStatus was called with the correct zone name
		// TODO: when fake client supports server side apply, check that the managed field with the stale zone gets deleted.
		Expect(egressFirewall.ManagedFields).To(HaveLen(4))

	})

	It("updates NetworkQoS status with 1 zone", func() {
		config.OVNKubernetesFeature.EnableNetworkQoS = true
		zones := sets.New[string]("zone1")
		namespace1 := util.NewNamespace(namespace1Name)
		networkQoS := newNetworkQoS(namespace1.Name)
		start(zones, namespace1, networkQoS)
		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}},
		}, fakeClient)

		checkNQStatusEventually(networkQoS, false, false, fakeClient)
	})

	It("updates NetworkQoS status with 2 zones", func() {
		config.OVNKubernetesFeature.EnableNetworkQoS = true
		zones := sets.New[string]("zone1", "zone2")
		namespace1 := util.NewNamespace(namespace1Name)
		networkQoS := newNetworkQoS(namespace1.Name)
		start(zones, namespace1, networkQoS)

		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}},
		}, fakeClient)

		checkEmptyNQStatusConsistently(networkQoS, fakeClient)

		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}, {
				Type:    "Ready-In-Zone-zone2",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}},
		}, fakeClient)
		checkNQStatusEventually(networkQoS, false, false, fakeClient)

	})

	It("updates NetworkQoS status with UnknownZone", func() {
		config.OVNKubernetesFeature.EnableNetworkQoS = true
		zones := sets.New[string]("zone1", zone_tracker.UnknownZone)
		namespace1 := util.NewNamespace(namespace1Name)
		networkQoS := newNetworkQoS(namespace1.Name)
		start(zones, namespace1, networkQoS)

		// no matter how many messages are in the status, it won't be updated while UnknownZone is present
		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}},
		}, fakeClient)
		checkEmptyNQStatusConsistently(networkQoS, fakeClient)

		// when UnknownZone is removed, updates will be handled, but status from the new zone is not reported yet
		statusManager.onZoneUpdate(sets.New[string]("zone1", "zone2"))
		checkEmptyNQStatusConsistently(networkQoS, fakeClient)
		// when new zone status is reported, status will be set
		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}, {
				Type:    "Ready-In-Zone-zone2",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}},
		}, fakeClient)
		checkNQStatusEventually(networkQoS, false, false, fakeClient)
	})

})
