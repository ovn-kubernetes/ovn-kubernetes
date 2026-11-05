// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	observabilityconfigv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The sampling sync is how an ObservabilityConfig change reaches ACLs that already exist. It
// resolves each ACL's feature and namespace from the ACL's own external IDs, so it covers every
// sampled ACL regardless of which controller created it, and it writes nothing but the
// SampleNew/SampleEst columns.
var _ = Describe("Observability sampling sync", func() {
	var (
		nbClient        libovsdbclient.Client
		libovsdbCleanup *libovsdbtest.Context
		manager         *Manager
	)

	AfterEach(func() {
		if manager != nil {
			manager.stopRetryTimer()
			manager = nil
		}
		if libovsdbCleanup != nil {
			libovsdbCleanup.Cleanup()
			libovsdbCleanup = nil
		}
	})

	// netpolACL builds a network policy ACL for the given namespace, keyed the way
	// BaseNetworkController keys them (namespace+name in ObjectNameKey).
	// The Sample metadata is derived from the ACL's identity and match, so each ACL needs its
	// own, the way real ACLs do; otherwise they would all share one Sample row.
	netpolACL := func(uuid, namespace, name string) *nbdb.ACL {
		objectName := libovsdbops.BuildNamespaceNameKey(namespace, name)
		return &nbdb.ACL{
			UUID:  uuid,
			Match: "ip4.src == $" + objectName,
			ExternalIDs: map[string]string{
				libovsdbops.OwnerTypeKey.String():       libovsdbops.NetworkPolicyOwnerType,
				libovsdbops.OwnerControllerKey.String(): types.DefaultNetworkControllerName,
				libovsdbops.ObjectNameKey.String():      objectName,
				libovsdbops.PrimaryIDKey.String():       objectName,
			},
		}
	}

	// sampledACL builds an ACL of an arbitrary owner type, owned by the controller the manager
	// governs. Each needs its own identity and match for the same reason as netpolACL.
	sampledACL := func(uuid, ownerType, objectName, match, ownerController string) *nbdb.ACL {
		return &nbdb.ACL{
			UUID:  uuid,
			Match: match,
			ExternalIDs: map[string]string{
				libovsdbops.OwnerTypeKey.String():       ownerType,
				libovsdbops.OwnerControllerKey.String(): ownerController,
				libovsdbops.ObjectNameKey.String():      objectName,
				libovsdbops.PrimaryIDKey.String():       objectName,
			},
		}
	}

	// portGroup attaches the ACLs to a port group, so the test DB is well formed.
	portGroup := func(acls ...*nbdb.ACL) *nbdb.PortGroup {
		pg := &nbdb.PortGroup{UUID: "pg-uuid"}
		for _, acl := range acls {
			pg.ACLs = append(pg.ACLs, acl.UUID)
		}
		return pg
	}

	// config builds an ObservabilityConfig enabling the given features at 100%, optionally
	// scoped to a set of namespaces.
	config := func(name string, collectorID int64, namespaces []string, features ...observabilityconfigv1alpha1.ObservabilityFeature) *observabilityconfigv1alpha1.ObservabilityConfig {
		featureConfigs := make([]observabilityconfigv1alpha1.FeatureConfig, 0, len(features))
		for _, f := range features {
			featureConfigs = append(featureConfigs, observabilityconfigv1alpha1.FeatureConfig{Feature: f, Probability: 100})
		}
		cfg := &observabilityconfigv1alpha1.ObservabilityConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: observabilityconfigv1alpha1.ObservabilitySpec{
				CollectorID: collectorID,
				Features:    featureConfigs,
			},
		}
		if len(namespaces) > 0 {
			cfg.Spec.Filter = &observabilityconfigv1alpha1.Filter{Namespaces: namespaces}
		}
		return cfg
	}

	// retryState snapshots the retry bookkeeping. Assertions must never run while holding
	// retryLock: a Ginkgo failure panics out of the It, and the lock would never be released,
	// deadlocking the AfterEach that stops the manager.
	retryState := func() (retries int, armed bool) {
		manager.retryLock.Lock()
		defer manager.retryLock.Unlock()
		return manager.retries, manager.retryTimer != nil
	}

	// resetRetries clears the bookkeeping left by the apply that start() performs, so a spec can
	// assert on the retries it schedules itself.
	resetRetries := func() {
		manager.retryLock.Lock()
		defer manager.retryLock.Unlock()
		manager.retries = 0
	}

	start := func(data []libovsdbtest.TestData, configs ...*observabilityconfigv1alpha1.ObservabilityConfig) {
		var err error
		nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{NBData: data})
		Expect(err).NotTo(HaveOccurred(), "failed to start the libovsdb test harness")
		manager = NewManager(nbClient, types.DefaultNetworkControllerName)
		Expect(manager.Init()).To(Succeed(), "failed to initialize the observability manager")
		Expect(manager.applyConfigs(configs)).To(Succeed(), "failed to apply the observability configs")
	}

	// collectorsOf resolves the Sample the ACL with the given ObjectNameKey points at and
	// returns its collectors, or nil when the ACL is not sampled. ACLs are looked up by
	// external ID because the test harness rewrites the named UUIDs.
	collectorsOf := func(objectName string) []string {
		acls, err := libovsdbops.FindACLsWithPredicate(nbClient, func(acl *nbdb.ACL) bool {
			return acl.ExternalIDs[libovsdbops.ObjectNameKey.String()] == objectName
		})
		Expect(err).NotTo(HaveOccurred(), "failed to list ACLs")
		Expect(acls).To(HaveLen(1), "expected exactly one ACL named %q", objectName)
		if acls[0].SampleNew == nil {
			return nil
		}
		samples, err := libovsdbops.ListSamples(nbClient)
		Expect(err).NotTo(HaveOccurred(), "failed to list Samples")
		for _, sample := range samples {
			if sample.UUID == *acls[0].SampleNew {
				return sample.Collectors
			}
		}
		Fail("ACL references a Sample that does not exist")
		return nil
	}

	It("applies a namespace-scoped config only to that namespace's existing ACLs", func() {
		fooACL := netpolACL("foo-acl", "foo", "policy")
		barACL := netpolACL("bar-acl", "bar", "policy")
		pg := portGroup(fooACL, barACL)

		start([]libovsdbtest.TestData{pg, fooACL, barACL},
			config("foo-only", 7, []string{"foo"}, observabilityconfigv1alpha1.NetworkPolicy))

		collectors, err := libovsdbops.ListSampleCollectors(nbClient)
		Expect(err).NotTo(HaveOccurred(), "failed to list collectors")
		Expect(collectors).To(HaveLen(1), "one config at one probability means one collector")

		Expect(collectorsOf("foo:policy")).To(ConsistOf(collectors[0].UUID))
		Expect(collectorsOf("bar:policy")).To(BeEmpty(), "a namespace-scoped config must not sample other namespaces")
	})

	It("merges cluster-wide and namespace-scoped collectors on the same ACL", func() {
		fooACL := netpolACL("foo-acl", "foo", "policy")
		barACL := netpolACL("bar-acl", "bar", "policy")
		pg := portGroup(fooACL, barACL)

		start([]libovsdbtest.TestData{pg, fooACL, barACL},
			config("cluster", 7, nil, observabilityconfigv1alpha1.NetworkPolicy),
			config("foo-only", 8, []string{"foo"}, observabilityconfigv1alpha1.NetworkPolicy))

		collectors, err := libovsdbops.ListSampleCollectors(nbClient)
		Expect(err).NotTo(HaveOccurred(), "failed to list collectors")
		Expect(collectors).To(HaveLen(2), "two configs with distinct collector IDs means two collectors")

		Expect(collectorsOf("foo:policy")).To(HaveLen(2), "foo gets both the cluster-wide and its own collector")
		Expect(collectorsOf("bar:policy")).To(HaveLen(1), "bar gets only the cluster-wide collector")
	})

	It("covers ACLs that have no Kubernetes object of their own", func() {
		// Node management-port allow ACLs and UDN isolation ACLs are not derived from a
		// NetworkPolicy/EgressFirewall object, so nothing could re-enqueue them. The sync finds
		// them through their owner type like any other sampled ACL.
		nodeACL := sampledACL("node-acl", libovsdbops.NetpolNodeOwnerType, "node1",
			"ip4.src == 10.0.0.1", types.DefaultNetworkControllerName)
		udnACL := sampledACL("udn-acl", libovsdbops.UDNIsolationOwnerType, "SecondaryPods",
			"ip4.src == 10.1.0.0/16", types.DefaultNetworkControllerName)
		pg := portGroup(nodeACL, udnACL)

		start([]libovsdbtest.TestData{pg, nodeACL, udnACL},
			config("cluster", 7, nil,
				observabilityconfigv1alpha1.NetworkPolicy, observabilityconfigv1alpha1.UDNIsolation))

		Expect(collectorsOf("node1")).NotTo(BeEmpty(), "node management-port ACLs must be sampled")
		Expect(collectorsOf("SecondaryPods")).NotTo(BeEmpty(), "UDN isolation ACLs must be sampled")
	})

	It("leaves ACLs of unsampled owner types alone", func() {
		otherACL := sampledACL("other-acl", "some-other-feature", "other",
			"ip4.src == 10.2.0.0/16", types.DefaultNetworkControllerName)
		pg := portGroup(otherACL)

		start([]libovsdbtest.TestData{pg, otherACL},
			config("cluster", 7, nil, observabilityconfigv1alpha1.NetworkPolicy))

		Expect(collectorsOf("other")).To(BeEmpty(), "an owner type with no sampled feature must not be touched")
	})

	It("leaves ACLs owned by a secondary network controller alone", func() {
		// Secondary (UDN) network controllers are not given an observability manager, so they
		// create their ACLs with no Sample. The sync must not attach one, otherwise the owning
		// controller would strip it again on its next reconcile and the two would fight.
		udnNetpolACL := sampledACL("udn-netpol-acl", libovsdbops.NetworkPolicyOwnerType, "foo:udn-policy",
			"ip4.src == 10.200.0.0/16", "bluenet-network-controller")
		defaultACL := netpolACL("foo-acl", "foo", "policy")
		pg := portGroup(udnNetpolACL, defaultACL)

		start([]libovsdbtest.TestData{pg, udnNetpolACL, defaultACL},
			config("cluster", 7, nil, observabilityconfigv1alpha1.NetworkPolicy))

		Expect(collectorsOf("foo:policy")).NotTo(BeEmpty(), "default network ACLs are sampled")
		Expect(collectorsOf("foo:udn-policy")).To(BeEmpty(), "secondary network ACLs must be left alone")
	})

	It("reports a pass as unchanged once the ACLs already match", func() {
		// This is the convergence signal the retry timer keys off: a second pass over an
		// already-correct DB must not report a change, otherwise the retry would never stop.
		acl := netpolACL("foo-acl", "foo", "policy")
		start([]libovsdbtest.TestData{portGroup(acl), acl},
			config("cluster", 7, nil, observabilityconfigv1alpha1.NetworkPolicy))

		manager.collectorsLock.Lock()
		changed, err := manager.syncSamplingLocked()
		manager.collectorsLock.Unlock()
		Expect(err).NotTo(HaveOccurred(), "the second sync pass must not fail")
		Expect(changed).To(BeFalse(), "a pass over an already-correct DB must report no change")
	})

	It("picks up an ACL whose write was in flight while the config changed", func() {
		// Regression test for the race the verification pass exists for. A controller resolves
		// the sampling config while assembling its ops and transacts afterwards. If the config
		// is enabled in between, the sync cannot see that ACL (it is not in the DB yet) and so
		// finds nothing to do, and the ACL then lands carrying the sampling config as it was
		// before. Nothing else would repair it: informers run with no resync period and the
		// retry framework only revisits objects that failed.
		start(nil)
		manager.retryInterval = 50 * time.Millisecond

		// The controller builds its ops while sampling is off, so they carry no Sample. The ACL
		// is attached to a port group in the same transaction, as a real one always is: ACL is a
		// non-root table, so an unreferenced row would simply be garbage collected.
		acl := netpolACL("foo-acl", "foo", "policy")
		ops, err := libovsdbops.CreateOrUpdateACLsOps(nbClient, nil,
			manager.SamplingConfigForContext("foo", libovsdbops.NetworkPolicySample), acl)
		Expect(err).NotTo(HaveOccurred(), "failed to build the ACL ops")
		ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(nbClient, ops, portGroup(acl))
		Expect(err).NotTo(HaveOccurred(), "failed to build the port group ops")

		// Sampling is enabled before those ops are transacted. There is no sampled ACL in the
		// DB at this point, so this sync has nothing to update.
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{
			config("cluster", 7, nil, observabilityconfigv1alpha1.NetworkPolicy),
		})).To(Succeed(), "failed to apply the observability config")

		// Only now does the in-flight write land, unsampled.
		_, err = libovsdbops.TransactAndCheck(nbClient, ops)
		Expect(err).NotTo(HaveOccurred(), "failed to transact the delayed ACL ops")
		Expect(collectorsOf("foo:policy")).To(BeEmpty(), "the delayed write must land with no Sample")

		Eventually(func() []string {
			return collectorsOf("foo:policy")
		}, 10*time.Second).ShouldNot(BeEmpty(), "the verification pass must sample the late ACL")
	})

	It("keeps the in-use collectors when a retry pass refreshes the snapshot", func() {
		// A retry pass refreshes the collector snapshot before cleaning up, so that a retrieval
		// that failed earlier gets another chance. Refreshing marks every collector unused, so
		// the pass has to re-claim the ones the published resolution still refers to; it has no
		// configs to walk, unlike applyConfigs.
		//
		// The collector here is deliberately referenced by no ACL, which is the ordinary state
		// right after sampling is enabled and before any policy exists. A collector that some
		// Sample already points at would survive a buggy pass anyway, since OVSDB refuses to
		// delete a referenced row; an unreferenced one is deleted for real, and the resolution
		// is then left pointing at a collector that no longer exists.
		start(nil, config("cluster", 7, nil, observabilityconfigv1alpha1.NetworkPolicy))

		before, err := libovsdbops.ListSampleCollectors(nbClient)
		Expect(err).NotTo(HaveOccurred(), "failed to list collectors")
		Expect(before).To(HaveLen(1), "the config must have created a collector")

		manager.retrySync()

		after, err := libovsdbops.ListSampleCollectors(nbClient)
		Expect(err).NotTo(HaveOccurred(), "failed to list collectors after the retry pass")
		Expect(after).To(HaveLen(1), "a collector still referenced by the resolution must survive")
		Expect(after[0].UUID).To(Equal(before[0].UUID), "the collector must not be recreated")

		// And it must still be usable: an ACL created afterwards gets that same collector.
		acl := netpolACL("foo-acl", "foo", "policy")
		ops, err := libovsdbops.CreateOrUpdateACLsOps(nbClient, nil,
			manager.SamplingConfigForContext("foo", libovsdbops.NetworkPolicySample), acl)
		Expect(err).NotTo(HaveOccurred(), "failed to build the ACL ops")
		ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(nbClient, ops, portGroup(acl))
		Expect(err).NotTo(HaveOccurred(), "failed to build the port group ops")
		_, err = libovsdbops.TransactAndCheck(nbClient, ops)
		Expect(err).NotTo(HaveOccurred(), "the resolution must not point at a deleted collector")
		Expect(collectorsOf("foo:policy")).To(ConsistOf(before[0].UUID))
	})

	It("discards a retry result that a newer config apply has superseded", func() {
		// A retry pass releases collectorsLock before reporting its outcome, so a config apply
		// can land in between. The older result describes a resolution that no longer exists;
		// acting on it would stop the verification pass the apply just armed, and an ACL written
		// against the newer config would stay unsampled for good.
		start(nil, config("cluster", 7, nil, observabilityconfigv1alpha1.NetworkPolicy))
		manager.retryInterval = time.Hour // keep the armed timer pending for the assertions

		// A pass starts and captures the generation it is running for.
		staleGen := manager.currentGeneration()

		// A new config lands before that pass reports, arming its own verification pass.
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{
			config("cluster", 7, nil, observabilityconfigv1alpha1.NetworkPolicy),
			config("second", 8, nil, observabilityconfigv1alpha1.EgressFirewall),
		})).To(Succeed(), "failed to apply the second observability config")
		_, armed := retryState()
		Expect(armed).To(BeTrue(), "the config apply must arm a verification pass")

		// The older pass now reports a converged result. It must not be acted on.
		manager.scheduleRetry(staleGen, false, nil)

		_, armed = retryState()
		Expect(armed).To(BeTrue(), "a superseded result must not cancel the newer verification pass")
	})

	It("re-arms the retry until a pass is a no-op", func() {
		// A retry pass asks for another one only while it keeps changing something, so the loop
		// stops as soon as a pass is a no-op.
		start(nil)
		manager.retryInterval = 10 * time.Millisecond
		resetRetries()

		manager.scheduleRetry(manager.currentGeneration(), true, nil)
		retries, armed := retryState()
		Expect(retries).To(Equal(1), "a pass that changed something must arm a retry")
		Expect(armed).To(BeTrue(), "a retry timer must be armed")

		// The armed pass finds nothing to change (no ACLs at all here), so the loop stops.
		Eventually(func() int {
			retries, _ := retryState()
			return retries
		}, 10*time.Second).Should(BeZero(), "the retry loop must stop once a pass is a no-op")
		_, armed = retryState()
		Expect(armed).To(BeFalse(), "no retry must remain armed after convergence")
	})

	It("gives up re-arming after the retry bound", func() {
		start(nil)
		manager.retryInterval = time.Hour // never actually fires during the test
		resetRetries()

		gen := manager.currentGeneration()
		for i := 1; i <= maxSyncRetries; i++ {
			manager.scheduleRetry(gen, true, nil)
			retries, _ := retryState()
			Expect(retries).To(Equal(i), "every non-converged pass must count towards the bound")
		}
		manager.scheduleRetry(gen, true, nil)
		retries, armed := retryState()
		Expect(retries).To(BeZero(), "the counter resets once the bound is exceeded")
		Expect(armed).To(BeFalse(), "no further retry is armed")
	})
})

var _ = Describe("aclSamplingUpToDate", func() {
	sampleUUID := "sample-uuid"
	acl := func(sampleRef *string) *nbdb.ACL {
		return &nbdb.ACL{SampleNew: sampleRef, SampleEst: sampleRef}
	}
	samples := func(collectors ...string) map[string]*nbdb.Sample {
		return map[string]*nbdb.Sample{sampleUUID: {
			UUID:       sampleUUID,
			Metadata:   int(libovsdbops.GetACLSampleID(acl(nil))),
			Collectors: collectors,
		}}
	}

	It("treats an unsampled ACL with no desired collectors as up to date", func() {
		Expect(aclSamplingUpToDate(acl(nil), nil, nil)).To(BeTrue())
	})

	It("detects an ACL that should no longer be sampled", func() {
		Expect(aclSamplingUpToDate(acl(&sampleUUID), nil, samples("c1"))).To(BeFalse())
	})

	It("detects an ACL that is not yet sampled but should be", func() {
		Expect(aclSamplingUpToDate(acl(nil), []string{"c1"}, nil)).To(BeFalse())
	})

	It("ignores collector ordering, since Sample.Collectors is a set", func() {
		Expect(aclSamplingUpToDate(acl(&sampleUUID), []string{"c2", "c1"}, samples("c1", "c2"))).To(BeTrue())
	})

	It("detects a changed collector set", func() {
		Expect(aclSamplingUpToDate(acl(&sampleUUID), []string{"c1", "c3"}, samples("c1", "c2"))).To(BeFalse())
	})

	It("detects a dangling Sample reference", func() {
		Expect(aclSamplingUpToDate(acl(&sampleUUID), []string{"c1"}, nil)).To(BeFalse())
	})

	It("detects a Sample whose metadata no longer matches the ACL", func() {
		stale := samples("c1")
		stale[sampleUUID].Metadata = 1234
		Expect(aclSamplingUpToDate(acl(&sampleUUID), []string{"c1"}, stale)).To(BeFalse())
	})

	It("detects SampleNew and SampleEst pointing at different Samples", func() {
		other := "other-uuid"
		mismatched := acl(&sampleUUID)
		mismatched.SampleEst = &other
		Expect(aclSamplingUpToDate(mismatched, []string{"c1"}, samples("c1"))).To(BeFalse())
	})
})
