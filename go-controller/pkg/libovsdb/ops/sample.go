// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"hash/fnv"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

func CreateOrUpdateSampleCollector(nbClient libovsdbclient.Client, collector *nbdb.SampleCollector) error {
	opModel := operationModel{
		Model:          collector,
		OnModelUpdates: onModelUpdatesAllNonDefault(),
		ErrNotFound:    false,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModel)
	return err
}

func UpdateSampleCollectorExternalIDs(nbClient libovsdbclient.Client, collector *nbdb.SampleCollector) error {
	opModel := operationModel{
		Model:          collector,
		OnModelUpdates: []interface{}{&collector.ExternalIDs},
		ErrNotFound:    true,
		BulkOp:         false,
	}

	m := newModelClient(nbClient)
	_, err := m.CreateOrUpdate(opModel)
	return err
}

func DeleteSampleCollector(nbClient libovsdbclient.Client, collector *nbdb.SampleCollector) error {
	opModel := operationModel{
		Model:       collector,
		ErrNotFound: false,
		BulkOp:      false,
	}
	m := newModelClient(nbClient)
	return m.Delete(opModel)
}

func DeleteSampleCollectorWithPredicate(nbClient libovsdbclient.Client, p func(collector *nbdb.SampleCollector) bool) error {
	opModel := operationModel{
		Model:          &nbdb.SampleCollector{},
		ModelPredicate: p,
		ErrNotFound:    false,
		BulkOp:         true,
	}
	m := newModelClient(nbClient)
	return m.Delete(opModel)
}

func FindSampleCollectorWithPredicate(nbClient libovsdbclient.Client, p func(*nbdb.SampleCollector) bool) ([]*nbdb.SampleCollector, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	collectors := []*nbdb.SampleCollector{}
	err := nbClient.WhereCache(p).List(ctx, &collectors)
	return collectors, err
}

// ListSamples returns every Sample row. The table holds one row per sampled ACL, so this is
// used to compare the Sample references currently in the DB against the desired ones.
func ListSamples(nbClient libovsdbclient.Client) ([]*nbdb.Sample, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	samples := []*nbdb.Sample{}
	err := nbClient.List(ctx, &samples)
	return samples, err
}

func ListSampleCollectors(nbClient libovsdbclient.Client) ([]*nbdb.SampleCollector, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	collectors := []*nbdb.SampleCollector{}
	err := nbClient.List(ctx, &collectors)
	return collectors, err
}

func CreateOrUpdateSamplingAppsOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, samplingApps ...*nbdb.SamplingApp) ([]ovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(samplingApps))
	for i := range samplingApps {
		// can't use i in the predicate, for loop replaces it in-memory
		samplingApp := samplingApps[i]
		opModel := operationModel{
			Model:          samplingApp,
			OnModelUpdates: onModelUpdatesAllNonDefault(),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}

func DeleteSamplingAppsWithPredicate(nbClient libovsdbclient.Client, p func(collector *nbdb.SamplingApp) bool) error {
	opModel := operationModel{
		Model:          &nbdb.SamplingApp{},
		ModelPredicate: p,
		ErrNotFound:    false,
		BulkOp:         true,
	}
	m := newModelClient(nbClient)
	return m.Delete(opModel)
}

func FindSample(nbClient libovsdbclient.Client, sampleMetadata int) (*nbdb.Sample, error) {
	sample := &nbdb.Sample{
		Metadata: sampleMetadata,
	}
	return GetSample(nbClient, sample)
}

func GetSample(nbClient libovsdbclient.Client, sample *nbdb.Sample) (*nbdb.Sample, error) {
	found := []*nbdb.Sample{}
	opModel := operationModel{
		Model:          sample,
		ExistingResult: &found,
		ErrNotFound:    true,
		BulkOp:         false,
	}
	modelClient := newModelClient(nbClient)
	err := modelClient.Lookup(opModel)
	if err != nil {
		return nil, err
	}
	return found[0], err
}

type SampleFeature = string

const (
	EgressFirewallSample     SampleFeature = "EgressFirewall"
	NetworkPolicySample      SampleFeature = "NetworkPolicy"
	AdminNetworkPolicySample SampleFeature = "AdminNetworkPolicy"
	MulticastSample          SampleFeature = "Multicast"
	UDNIsolationSample       SampleFeature = "UDNIsolation"
)

// SamplingConfig is used to configure sampling for different db objects.
type SamplingConfig struct {
	featureCollectors map[SampleFeature][]string
}

func NewSamplingConfig(featureCollectors map[SampleFeature][]string) *SamplingConfig {
	return &SamplingConfig{
		featureCollectors: featureCollectors,
	}
}

func addSample(c *SamplingConfig, opModels []operationModel, model model.Model) []operationModel {
	switch t := model.(type) {
	case *nbdb.ACL:
		return createOrUpdateSampleForACL(opModels, c, t)
	}
	return opModels
}

// createOrUpdateSampleForACL should be called before acl operationModel is appended to opModels.
func createOrUpdateSampleForACL(opModels []operationModel, c *SamplingConfig, acl *nbdb.ACL) []operationModel {
	var collectors []string
	if c != nil {
		collectors = c.featureCollectors[GetACLSampleFeature(acl)]
	}
	return sampleForACLOps(opModels, acl, collectors)
}

// sampleForACLOps points acl at a Sample referencing collectors, adding the op that creates or
// updates that Sample. An empty collectors slice detaches sampling from the ACL instead. The
// caller is responsible for appending the ACL's own operationModel afterwards, so that the
// Sample UUID set by DoAfter is picked up.
func sampleForACLOps(opModels []operationModel, acl *nbdb.ACL, collectors []string) []operationModel {
	if len(collectors) == 0 {
		acl.SampleEst = nil
		acl.SampleNew = nil
		return opModels
	}
	aclID := GetACLSampleID(acl)
	sample := &nbdb.Sample{
		Collectors: collectors,
		// 32 bits
		Metadata: int(aclID),
	}
	opModel := operationModel{
		Model: sample,
		DoAfter: func() {
			acl.SampleEst = &sample.UUID
			acl.SampleNew = &sample.UUID
		},
		OnModelUpdates: []interface{}{&sample.Collectors},
		ErrNotFound:    false,
		BulkOp:         false,
	}
	opModels = append(opModels, opModel)
	return opModels
}

// ACLSampling pairs an existing ACL with the collectors its Sample should reference.
// An empty Collectors slice means sampling must be detached from the ACL.
type ACLSampling struct {
	ACL        *nbdb.ACL
	Collectors []string
}

// UpdateACLsSamplingOps returns ops that update only the Sample references (SampleNew/SampleEst)
// of ACLs that already exist, leaving every other ACL column untouched. It is how the
// observability manager re-points ACLs at new collectors when the ObservabilityConfig changes,
// without going through the controller that owns the ACL and without re-applying the rest of
// its state.
//
// The ACLs must come from the DB (they are looked up by UUID); an ACL that has been deleted
// since it was listed makes this return an error, so the caller can retry on a fresh list.
func UpdateACLsSamplingOps(nbClient libovsdbclient.Client, ops []ovsdb.Operation, samplings ...ACLSampling) ([]ovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(samplings)*2)
	for _, sampling := range samplings {
		acl := sampling.ACL
		opModels = sampleForACLOps(opModels, acl, sampling.Collectors)
		opModels = append(opModels, operationModel{
			Model:          acl,
			OnModelUpdates: []interface{}{&acl.SampleNew, &acl.SampleEst},
			ErrNotFound:    true,
			BulkOp:         false,
		})
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}

func GetACLSampleID(acl *nbdb.ACL) uint32 {
	// primaryID is unique for each ACL, but established connections will keep sampleID that is set on
	// connection creation. Here is the situation we want to avoid:
	// 1. ACL1 is created with sampleID=1 (e.g. based on ANP namespace+name+...+rule index with action Allow)
	// 2. connection A is established with sampleID=1, sample is decoded to say "Allowed by ANP namespace+name"
	// 3. ACL1 is updated with sampleID=1 (e.g. now same rule in ANP says Deny, but PrimaryIDKey is the same)
	// 4. connection A still generates samples with sampleID=1, but now it is "Denied by ANP namespace+name"
	// In reality, connection A is still allowed, as existing connections are not affected by ANP updates.
	// To avoid this, we encode Match and Action to the sampleID, to ensure a new sampleID is assigned on Match or action change.
	// In that case stale sampleIDs will just report messages like "sampling for this connection was updated or deleted".
	primaryID := acl.ExternalIDs[PrimaryIDKey.String()] + acl.Match + acl.Action
	h := fnv.New32a()
	h.Write([]byte(primaryID))
	return h.Sum32()
}

// GetACLSampleNamespace returns the namespace an ACL's sampling resolves against, or "" when the
// ACL is cluster-scoped (its collectors come only from configs with no Filter.Namespaces).
// It mirrors how the owning controller picks the namespace it passes to GetSamplingConfig.
func GetACLSampleNamespace(acl *nbdb.ACL) string {
	objectName := acl.ExternalIDs[ObjectNameKey.String()]
	switch acl.ExternalIDs[OwnerTypeKey.String()] {
	case NetworkPolicyOwnerType:
		// policy ACLs are keyed by namespace+name
		namespace, _, err := ParseNamespaceNameKey(objectName)
		if err != nil {
			return ""
		}
		return namespace
	case NetpolNamespaceOwnerType, EgressFirewallOwnerType, MulticastNamespaceOwnerType:
		// these are keyed by namespace alone
		return objectName
	}
	// NetpolNodeOwnerType is keyed by node name, the remaining owner types are cluster-scoped.
	return ""
}

// GetACLSampleFeature returns the observability feature whose sampling configuration governs
// this ACL, or "" if the ACL is not sampled at all. The owner type recorded in the ACL's
// external IDs is the single source of truth for that mapping, which lets the observability
// manager find every sampled ACL of a feature without involving the controller that owns it.
func GetACLSampleFeature(acl *nbdb.ACL) SampleFeature {
	switch acl.ExternalIDs[OwnerTypeKey.String()] {
	case AdminNetworkPolicyOwnerType, BaselineAdminNetworkPolicyOwnerType:
		return AdminNetworkPolicySample
	case MulticastNamespaceOwnerType, MulticastClusterOwnerType:
		return MulticastSample
	case NetpolNodeOwnerType, NetworkPolicyOwnerType, NetpolNamespaceOwnerType:
		return NetworkPolicySample
	case EgressFirewallOwnerType:
		return EgressFirewallSample
	case UDNIsolationOwnerType:
		return UDNIsolationSample
	}
	return ""
}
