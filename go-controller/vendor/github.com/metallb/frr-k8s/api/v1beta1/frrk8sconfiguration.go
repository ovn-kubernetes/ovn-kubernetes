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

// FRRK8sConfigurationSpec defines the desired state of FRRK8sConfiguration.
type FRRK8sConfigurationSpec struct {
	// LogLevel sets the logging verbosity for the FRR-K8s components at runtime.
	// When configured, this value overrides the defaults established by the --log-level CLI flag.
	// Valid values are: all, debug, info, warn, error, none.
	// +kubebuilder:validation:Enum=all;debug;info;warn;error;none
	// +optional
	LogLevel string `json:"logLevel,omitempty"`
}

// FRRK8sConfigurationStatus defines the observed state of FRRK8sConfiguration.
type FRRK8sConfigurationStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//nolint
//+genclient

// FRRK8sConfiguration holds the FRR Operator configuration with global
// settings for the K8s and FRR.
type FRRK8sConfiguration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FRRK8sConfigurationSpec   `json:"spec,omitempty"`
	Status FRRK8sConfigurationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// FRRK8sConfigurationList contains a list of FRRK8sConfiguration.
type FRRK8sConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FRRK8sConfiguration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FRRK8sConfiguration{}, &FRRK8sConfigurationList{})
}
