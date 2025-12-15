/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BGPSessionStateSpec defines the desired state of BGPSessionState.
type BGPSessionStateSpec struct {
}

// BGPSessionStateStatus defines the observed state of BGPSessionState.
type BGPSessionStateStatus struct {
	BGPStatus string `json:"bgpStatus,omitempty"`
	BFDStatus string `json:"bfdStatus,omitempty"`
	Node      string `json:"node,omitempty"`
	Peer      string `json:"peer,omitempty"`
	VRF       string `json:"vrf,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// BGPSessionState exposes the status of a BGP Session from the FRR instance running on the node.
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.node`
// +kubebuilder:printcolumn:name="Peer",type=string,JSONPath=`.status.peer`
// +kubebuilder:printcolumn:name="VRF",type=string,JSONPath=`.status.vrf`
// +kubebuilder:printcolumn:name="BGP",type=string,JSONPath=`.status.bgpStatus`
// +kubebuilder:printcolumn:name="BFD",type=string,JSONPath=`.status.bfdStatus`
type BGPSessionState struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BGPSessionStateSpec   `json:"spec,omitempty"`
	Status BGPSessionStateStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// BGPSessionStateList contains a list of BGPSessionState.
type BGPSessionStateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BGPSessionState `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BGPSessionState{}, &BGPSessionStateList{})
}
