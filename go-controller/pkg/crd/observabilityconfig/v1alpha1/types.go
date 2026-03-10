package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ObservabilityConfig described OVN Observability configuration.
//
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=observabilityconfig,scope=cluster
// +kubebuilder:singular=observabilityconfig
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type ObservabilityConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:Required
	// +required
	Spec ObservabilitySpec `json:"spec"`
	// +optional
	Status ObservabilityStatus `json:"status,omitempty"`
}
type ObservabilitySpec struct {
	// CollectorID is the OVN Sample_Collector set_id: unique across the cluster, range 1 to 4,294,967,295 (MaxUint32).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +required
	CollectorID int64 `json:"collectorID"`
	// Features is a list of Observability features that can generate samples and their probabilities for a given collector.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +required
	Features []FeatureConfig `json:"features"`

	// Filter allows to apply ObservabilityConfig in a granular manner.
	// +optional
	Filter *Filter `json:"filter,omitempty"`
}

type FeatureConfig struct {
	// Probability is the probability of the feature being sampled in percent.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +required
	Probability int32 `json:"probability"`
	// Feature is the Observability feature that should be sampled.
	// +kubebuilder:validation:Required
	// +required
	Feature ObservabilityFeature `json:"feature"`
}

// Filter allows to apply ObservabilityConfig in a granular manner.
// Currently, it supports node and namespace based filtering.
// If both node and namespace filters are specifies, they are logically ANDed.
// +kubebuilder:validation:MinProperties=1
type Filter struct {
	// nodeSelector applies ObservabilityConfig only to nodes that match the selector.
	// +optional
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
	// namespaces is a list of namespaces to which the ObservabilityConfig should be applied.
	// It only applies to the namespaced features, currently that includes NetworkPolicy and EgressFirewall.
	// +kubebuilder:MinItems=1
	// +optional
	Namespaces *[]string `json:"namespaces,omitempty"`
}

// ObservabilityStatus contains the observed status of the ObservabilityConfig.
type ObservabilityStatus struct {
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:validation:Enum=NetworkPolicy;AdminNetworkPolicy;EgressFirewall;UDNIsolation;MulticastIsolation
type ObservabilityFeature string

const (
	NetworkPolicy      ObservabilityFeature = "NetworkPolicy"
	AdminNetworkPolicy ObservabilityFeature = "AdminNetworkPolicy"
	EgressFirewall     ObservabilityFeature = "EgressFirewall"
	UDNIsolation       ObservabilityFeature = "UDNIsolation"
	MulticastIsolation ObservabilityFeature = "MulticastIsolation"
)

// ObservabilityConfigList contains a list of ObservabilityConfig.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ObservabilityConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObservabilityConfig `json:"items"`
}
