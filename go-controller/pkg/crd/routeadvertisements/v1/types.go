package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:openapi-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=routeadvertisements,scope=Cluster,shortName=ra,singular=routeadvertisements
// +kubebuilder::singular=routeadvertisements
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
// RouteAdvertisements is the Schema for the routeadvertisements API
type RouteAdvertisements struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RouteAdvertisementsSpec   `json:"spec,omitempty"`
	Status RouteAdvertisementsStatus `json:"status,omitempty"`
}

// RouteAdvertisementsSpec defines the desired state of RouteAdvertisements
// +kubebuilder:validation:XValidation:rule="(!has(self.nodeSelector.matchLabels) && !has(self.nodeSelector.matchExpressions)) || !('PodNetwork' in self.advertisements)",message="If 'PodNetwork' is selected for advertisement, a 'nodeSelector' can't be specified as it needs to be advertised on all nodes"
// +kubebuilder:validation:XValidation:rule="!self.networkSelectors.exists(i, i.networkSelectionType != 'DefaultNetwork' && i.networkSelectionType != 'ClusterUserDefinedNetworks')",message="Only DefaultNetwork or ClusterUserDefinedNetworks can be selected"
type RouteAdvertisementsSpec struct {
	// targetVRF determines which VRF the routes should be advertised in.
	// +kubebuilder:validation:Optional
	TargetVRF string `json:"targetVRF,omitempty"`

	// networkSelectors determines which network routes should be advertised.
	// Only ClusterUserDefinedNetworks and the default network can be selected.
	// +kubebuilder:validation:Required
	NetworkSelectors types.NetworkSelectors `json:"networkSelectors"`

	// nodeSelector limits the advertisements to selected nodes. This field
	// follows standard label selector semantics.
	// +kubebuilder:validation:Required
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// frrConfigurationSelector determines which FRRConfigurations will the
	// OVN-Kubernetes driven FRRConfigurations be based on. This field follows
	// standard label selector semantics.
	// +kubebuilder:validation:Required
	FRRConfigurationSelector metav1.LabelSelector `json:"frrConfigurationSelector"`

	// advertisements determines what is advertised.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:XValidation:rule="self.all(x, self.exists_one(y, x == y))"
	Advertisements []AdvertisementType `json:"advertisements,omitempty"`
}

// AdvertisementType determines the type of advertisement.
// +kubebuilder:validation:Enum=PodNetwork;EgressIP
type AdvertisementType string

const (
	// PodNetwork determines that the pod network is advertised.
	PodNetwork AdvertisementType = "PodNetwork"

	// EgressIP determines that egress IPs are being advertised.
	EgressIP AdvertisementType = "EgressIP"
)

// RouteAdvertisementsStatus defines the observed state of RouteAdvertisements.
// It should always be reconstructable from the state of the cluster and/or
// outside world.
type RouteAdvertisementsStatus struct {
	// status is a concise indication of whether the RouteAdvertisements
	// resource is applied with success.
	// +kubebuilder:validation:Optional
	Status string `json:"status,omitempty"`

	// conditions is an array of condition objects indicating details about
	// status of RouteAdvertisements object.
	// +kubebuilder:validation:Optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// RouteAdvertisementsList contains a list of RouteAdvertisements
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type RouteAdvertisementsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RouteAdvertisements `json:"items"`
}
