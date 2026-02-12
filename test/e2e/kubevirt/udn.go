package kubevirt

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// GenerateCUDN creates a new ClusterUserDefinedNetwork (CUDN) object with the specified parameters.
// Parameters:
// - namespace: The namespace in which the CUDN will be created.
// - name: The name of the CUDN.
// - topology: The network topology for the CUDN.
// - role: The network role for the CUDN.
// - subnets: The dual-stack CIDRs for the CUDN.
// Returns:
// - A pointer to the created ClusterUserDefinedNetwork object.
// - A string representation of the CUDN's network name.
func GenerateCUDN(namespace, name string, topology udnv1.NetworkTopology, role udnv1.NetworkRole, subnets udnv1.DualStackCIDRs) (*udnv1.ClusterUserDefinedNetwork, string) {
	cudn := &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			// Generate a unique name for the CUDN by combining the namespace and name and add
			// a label with the same value for easy identification, for example at the RouteAdvertisement
			// CUDN selector
			Name: namespace + "-" + name,
			Labels: map[string]string{
				"name": namespace + "-" + name,
			},
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
	ipam := &udnv1.IPAMConfig{
		Mode: udnv1.IPAMDisabled,
	}

	if len(subnets) > 0 {
		ipam.Mode = udnv1.IPAMEnabled
		ipam.Lifecycle = udnv1.IPAMLifecyclePersistent
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
