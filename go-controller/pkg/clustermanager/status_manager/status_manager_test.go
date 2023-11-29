package status_manager

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/status_manager/zone_tracker"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
)

func getNodeWithZone(nodeName, zoneName string) *v1.Node {
	annotations := map[string]string{}
	if zoneName != zone_tracker.UnknownZone {
		annotations[util.OvnNodeZoneName] = zoneName
	}
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: annotations,
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
		zones := sets.New[string]("zone1")
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
		zones := sets.New[string]("zone1", "zone2")
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
		zones := sets.New[string]("zone1", zone_tracker.UnknownZone)
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)
		start(zones, namespace1, egressFirewall)

		// no matter how many messages are in the status, it won't be updated while UnknownZone is present
		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)
		checkEmptyEFStatusConsistently(egressFirewall, fakeClient)

		// when UnknownZone is removed, updates will be handled, but status from the new zone is not reported yet
		statusManager.onZoneUpdate(sets.New[string]("zone1", "zone2"))
		checkEmptyEFStatusConsistently(egressFirewall, fakeClient)
		// when new zone status is reported, status will be set
		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK"), types.GetZoneStatus("zone2", "OK")},
		}, fakeClient)
		checkEFStatusEventually(egressFirewall, false, false, fakeClient)
	})
	It("updates APBRoute status with 1 zone", func() {
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true
		zones := sets.New[string]("zone1")
		apbRoute := newAPBRoute(apbrouteName)
		start(zones, apbRoute)

		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)

		checkAPBRouteStatusEventually(apbRoute, false, false, fakeClient)
	})

	It("updates APBRoute status with 2 zones", func() {
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true
		zones := sets.New[string]("zone1", "zone2")
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
		zones := sets.New[string]("zone1", zone_tracker.UnknownZone)
		apbRoute := newAPBRoute(apbrouteName)
		start(zones, apbRoute)

		// no matter how many messages are in the status, it won't be updated while UnknownZone is present
		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)
		checkEmptyAPBRouteStatusConsistently(apbRoute, fakeClient)

		// when UnknownZone is removed, updates will be handled, but status from the new zone is not reported yet
		statusManager.onZoneUpdate(sets.New[string]("zone1", "zone2"))
		checkEmptyAPBRouteStatusConsistently(apbRoute, fakeClient)
		// when new zone status is reported, status will be set
		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK"), types.GetZoneStatus("zone2", "OK")},
		}, fakeClient)
		checkAPBRouteStatusEventually(apbRoute, false, false, fakeClient)
	})
	// cleanup can't be tested by unit test apiserver, since it relies on SSA logic with FieldManagers
})
