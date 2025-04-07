package types

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NetworkSelectors selects multiple sets of networks.
// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:MaxItems=5
// +patchMergeKey=networkSelectionType
// +patchStrategy=merge
// +listType=map
// +listMapKey=networkSelectionType
type NetworkSelectors []NetworkSelector

// NetworkSelector selects a set of networks.
// +kubebuilder:validation:XValidation:rule="!has(self.networkSelectionType) ? true : has(self.clusterUserDefinedNetworkSelector) ? self.networkSelectionType == 'ClusterUserDefinedNetworks' : self.networkSelectionType != 'ClusterUserDefinedNetworks'",message="Inconsistent selector: both networkSelectionType ClusterUserDefinedNetworks and clusterUserDefinedNetworkSelector have to be set or neither"
// +kubebuilder:validation:XValidation:rule="!has(self.networkSelectionType) ? true : has(self.primaryUserDefinedNetworkSelector) ? self.networkSelectionType == 'PrimaryUserDefinedNetworks' : self.networkSelectionType != 'PrimaryUserDefinedNetworks'",message="Inconsistent selector: both networkSelectionType PrimaryUserDefinedNetworks and primaryUserDefinedNetworkSelector have to be set or neither"
// +kubebuilder:validation:XValidation:rule="!has(self.networkSelectionType) ? true : has(self.secondaryUserDefinedNetworkSelector) ? self.networkSelectionType == 'SecondaryUserDefinedNetworks' : self.networkSelectionType != 'SecondaryUserDefinedNetworks'",message="Inconsistent selector: both networkSelectionType SecondaryUserDefinedNetworks and secondaryUserDefinedNetworkSelector have to be set or neither"
// +kubebuilder:validation:XValidation:rule="!has(self.networkSelectionType) ? true : has(self.networkAttachmentDefinitionSelector) ? self.networkSelectionType == 'NetworkAttachmentDefinitions' : self.networkSelectionType != 'NetworkAttachmentDefinitions'",message="Inconsistent selector: both networkSelectionType NetworkAttachmentDefinitions and networkAttachmentDefinitionSelector have to be set or neither"
type NetworkSelector struct {
	// networkSelectionType determines the type of networks selected.
	// +unionDiscriminator
	// +kubebuilder:validation:Required
	NetworkSelectionType NetworkSelectionType `json:"networkSelectionType"`

	// clusterUserDefinedNetworkSelector selects ClusterUserDefinedNetworks when
	// NetworkSelectionType is 'ClusterUserDefinedNetworks'.
	// +kubebuilder:validation:Optional
	ClusterUserDefinedNetworkSelector *ClusterUserDefinedNetworkSelector `json:"clusterUserDefinedNetworkSelector,omitempty"`

	// primaryUserDefinedNetworkSelector selects primary UserDefinedNetworks when
	// NetworkSelectionType is 'PrimaryUserDefinedNetworks'.
	// +kubebuilder:validation:Optional
	PrimaryUserDefinedNetworkSelector *PrimaryUserDefinedNetworkSelector `json:"primaryUserDefinedNetworkSelector,omitempty"`

	// secondaryUserDefinedNetworkSelector selects secondary UserDefinedNetworks
	// when NetworkSelectionType is 'SecondaryUserDefinedNetworks'.
	// +kubebuilder:validation:Optional
	SecondaryUserDefinedNetworkSelector *SecondaryUserDefinedNetworkSelector `json:"secondaryUserDefinedNetworkSelector,omitempty"`

	// networkAttachmentDefinitionSelector selects networks defined in the
	// selected NetworkAttachmentDefinitions when NetworkSelectionType is
	// 'SecondaryUserDefinedNetworks'.
	// +kubebuilder:validation:Optional
	NetworkAttachmentDefinitionSelector *NetworkAttachmentDefinitionSelector `json:"networkAttachmentDefinitionSelector,omitempty"`
}

// NetworkSelectionType determines the type of networks selected.
// +kubebuilder:validation:Enum=DefaultNetwork;ClusterUserDefinedNetworks;PrimaryUserDefinedNetworks;SecondaryUserDefinedNetworks;NetworkAttachmentDefinitions
type NetworkSelectionType string

const (
	// DefaultNetwork determines that the default pod network is selected.
	DefaultNetwork NetworkSelectionType = "DefaultNetwork"

	// ClusterUserDefinedNetworks determines that ClusterUserDefinedNetworks are selected.
	ClusterUserDefinedNetworks NetworkSelectionType = "ClusterUserDefinedNetworks"

	// PrimaryUserDefinedNetworks determines that primary UserDefinedNetworks are selected.
	PrimaryUserDefinedNetworks NetworkSelectionType = "PrimaryUserDefinedNetworks"

	// SecondaryUserDefinedNetworks determines that secondary UserDefinedNetworks are selected.
	SecondaryUserDefinedNetworks NetworkSelectionType = "SecondaryUserDefinedNetworks"

	// NetworkAttachmentDefinitions determines that networks defined in NetworkAttachmentDefinitions are selected.
	NetworkAttachmentDefinitions NetworkSelectionType = "NetworkAttachmentDefinitions"
)

// ClusterUserDefinedNetworkSelector selects ClusterUserDefinedNetworks.
type ClusterUserDefinedNetworkSelector struct {
	// networkSelector selects ClusterUserDefinedNetworks by label. A null
	// selector will mot match anything, while an empty ({}) selector will match
	// all.
	// +kubebuilder:validation:Required
	NetworkSelector metav1.LabelSelector `json:"networkSelector"`
}

// PrimaryUserDefinedNetworkSelector selects primary UserDefinedNetworks.
type PrimaryUserDefinedNetworkSelector struct {
	// namespaceSelector select the primary UserDefinedNetworks that are servind
	// the selected namespaces. This field follows standard label selector
	// semantics.
	// +kubebuilder:validation:Required
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`
}

// SecondaryUserDefinedNetworkSelector selects secondary UserDefinedNetworks.
type SecondaryUserDefinedNetworkSelector struct {
	// namespaceSelector selects namespaces where the secondary
	// UserDefinedNetworks are defined. This field follows standard label
	// selector semantics.
	// +kubebuilder:validation:Required
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// networkSelector selects secondary UserDefinedNetworks within the selected
	// namespaces by label. This field follows standard label selector
	// semantics.
	// +kubebuilder:validation:Required
	NetworkSelector metav1.LabelSelector `json:"networkSelector"`
}

// NetworkAttachmentDefinitionSelector selects networks defined in the selected NetworkAttachmentDefinitions.
type NetworkAttachmentDefinitionSelector struct {
	// namespaceSelector selects namespaces where the
	// NetworkAttachmentDefinitions are defined. This field follows standard
	// label selector semantics.
	// +kubebuilder:validation:Required
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// networkSelector selects NetworkAttachmentDefinitions within the selected
	// namespaces by label. This field follows standard label selector
	// semantics.
	// +kubebuilder:validation:Required
	NetworkSelector metav1.LabelSelector `json:"networkSelector"`
}
