// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"context"
	"fmt"
	"strings"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// NADAdmission reserves the network names generated for UserDefinedNetwork
// and ClusterUserDefinedNetwork. ovnkube identifies a network by the name in
// the NAD config, so a hand-written NAD using such a name would join that
// network or collide with its OVN objects.
// A NAD may still use a UserDefinedNetwork name of its own namespace: that
// only affects networks of the namespace it is in.
// It rejects every caller. cluster-manager renders the NADs that carry these
// names, and the webhook configuration must not send its requests here.
type NADAdmission struct{}

// NewNADAdmissionWebhook creates the NetworkAttachmentDefinition validator.
func NewNADAdmissionWebhook() *NADAdmission {
	return &NADAdmission{}
}

var _ admission.Validator[*nadv1.NetworkAttachmentDefinition] = &NADAdmission{}

func (a NADAdmission) ValidateCreate(ctx context.Context, nad *nadv1.NetworkAttachmentDefinition) (admission.Warnings, error) {
	return nil, a.validate(ctx, nil, nad)
}

func (a NADAdmission) ValidateUpdate(ctx context.Context, oldNAD, newNAD *nadv1.NetworkAttachmentDefinition) (admission.Warnings, error) {
	return nil, a.validate(ctx, oldNAD, newNAD)
}

// ValidateDelete is outside the webhook rules.
func (a NADAdmission) ValidateDelete(_ context.Context, _ *nadv1.NetworkAttachmentDefinition) (admission.Warnings, error) {
	return nil, nil
}

func (a NADAdmission) validate(ctx context.Context, oldNAD, newNAD *nadv1.NetworkAttachmentDefinition) error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return err
	}
	// Leave metadata-only updates alone, so the garbage collector can remove
	// finalizers from a NAD rendered by cluster-manager.
	if oldNAD != nil && oldNAD.Spec.Config == newNAD.Spec.Config {
		return nil
	}
	// The old config is checked too: nobody else may rewrite or repurpose a
	// NAD that carries a reserved name.
	for _, nad := range []*nadv1.NetworkAttachmentDefinition{newNAD, oldNAD} {
		if nad == nil {
			continue
		}
		if err := checkReservedNetworkName(nad.Spec.Config, req.Namespace); err != nil {
			return err
		}
	}
	return nil
}

// checkReservedNetworkName returns an error if an OVN-Kubernetes CNI config
// uses a network name of the form generated for a ClusterUserDefinedNetwork,
// or for a UserDefinedNetwork of a namespace other than nadNamespace.
// A name for another namespace is reserved even if that namespace does not
// exist: it could be created later, with a UserDefinedNetwork of that name.
func checkReservedNetworkName(cniConfig, nadNamespace string) error {
	netconf, err := config.ParseNetConf([]byte(cniConfig))
	if err != nil {
		// Not an OVN-Kubernetes config, or one that ovnkube rejects as well.
		return nil
	}
	udnNamespace, udnName := util.ParseNetworkName(netconf.Name)
	switch {
	case udnName == "":
		// not a generated name
	case strings.HasPrefix(netconf.Name, types.CUDNPrefix):
		return fmt.Errorf("network name %q is reserved for ClusterUserDefinedNetwork %q: names starting with %q "+
			"cannot be used in a NetworkAttachmentDefinition",
			netconf.Name, udnName, types.CUDNPrefix)
	case udnNamespace != nadNamespace:
		return fmt.Errorf("network name %q is reserved for UserDefinedNetwork %q in namespace %q: "+
			"a NetworkAttachmentDefinition in namespace %q can use a name of the form \"<namespace>_<name>\" "+
			"only with its own namespace, or a name without '_'",
			netconf.Name, udnName, udnNamespace, nadNamespace)
	}
	return nil
}
