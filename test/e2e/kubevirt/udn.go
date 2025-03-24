package kubevirt

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func GenerateCUDN(namespace, name string, topology udnv1.NetworkTopology, role udnv1.NetworkRole, subnets udnv1.DualStackCIDRs) (*udnv1.ClusterUserDefinedNetwork, string) {
	cudn := &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace + "-" + name,
		},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{namespace},
			}}},
			Network: udnv1.NetworkSpec{
				Topology: topology,
			},
		},
	}
	var ipam *udnv1.IPAMConfig
	if len(subnets) > 0 {
		ipam = &udnv1.IPAMConfig{
			Mode:      udnv1.IPAMEnabled,
			Lifecycle: udnv1.IPAMLifecyclePersistent,
		}
	} else {
		ipam = &udnv1.IPAMConfig{
			Mode: udnv1.IPAMDisabled,
		}
	}
	networkName := util.GenerateCUDNNetworkName(cudn.Name)
	if topology == udnv1.NetworkTopologyLayer2 {
		cudn.Spec.Network.Layer2 = &udnv1.Layer2Config{
			Role:    role,
			Subnets: subnets,
			IPAM:    ipam,
		}
	} else if topology == udnv1.NetworkTopologyLocalnet {
		cudn.Spec.Network.Localnet = &udnv1.LocalnetConfig{
			Role:                role,
			Subnets:             subnets,
			IPAM:                ipam,
			PhysicalNetworkName: networkName,
		}
	}

	return cudn, networkName
}
