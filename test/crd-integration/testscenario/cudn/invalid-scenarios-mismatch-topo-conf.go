// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cudn

import (
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
)

var MismatchTopologyConfig = []testscenario.ValidateCRScenario{
	{
		Description: "topology is localnet but topology config is layer2",
		ExpectedErr: `spec.localnet is required when topology is Localnet and forbidden otherwise`,
		Object:      testscenario.WithField(newCUDN("topology-localnet-mismatch-topology-config-layer2-fail", udnv1.NetworkSpec{Topology: "Localnet"}), nil, "spec.network.layer2"),
	},
	{
		Description: "topology is localnet but topology config is layer3",
		ExpectedErr: `spec.localnet is required when topology is Localnet and forbidden otherwise`,
		Object:      testscenario.WithField(newCUDN("topology-localnet-mismatch-topology-config-layer3-fail", udnv1.NetworkSpec{Topology: "Localnet"}), nil, "spec.network.layer3"),
	},
	{
		Description: "topology is layer2 but topology config is localnet",
		ExpectedErr: `spec.layer2 is required when topology is Layer2 and forbidden otherwise`,
		Object:      testscenario.WithField(newCUDN("topology-layer2-mismatch-topology-config-localnet-fail", udnv1.NetworkSpec{Topology: "Layer2"}), nil, "spec.network.localnet"),
	},
	{
		Description: "topology is layer2 but topology config is layer3",
		ExpectedErr: `spec.layer2 is required when topology is Layer2 and forbidden otherwise`,
		Object:      testscenario.WithField(newCUDN("topology-layer2-mismatch-topology-config-layer3-fail", udnv1.NetworkSpec{Topology: "Layer2"}), nil, "spec.network.layer3"),
	},
	{
		Description: "topology is layer3 but topology config is localnet",
		ExpectedErr: `spec.layer3 is required when topology is Layer3 and forbidden otherwise`,
		Object:      testscenario.WithField(newCUDN("topology-layer3-mismatch-topology-config-localnet-fail", udnv1.NetworkSpec{Topology: "Layer3"}), nil, "spec.network.localnet"),
	},
	{
		Description: "topology is layer3 but topology config is layer2",
		ExpectedErr: `spec.layer3 is required when topology is Layer3 and forbidden otherwise`,
		Object:      testscenario.WithField(newCUDN("topology-layer3-mismatch-topology-config-layer2-fail", udnv1.NetworkSpec{Topology: "Layer3"}), nil, "spec.network.layer2"),
	},
}
