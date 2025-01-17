/*


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
// Code generated by applyconfiguration-gen. DO NOT EDIT.

package applyconfiguration

import (
	v1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	internal "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/applyconfiguration/internal"
	userdefinednetworkv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/applyconfiguration/userdefinednetwork/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	testing "k8s.io/client-go/testing"
)

// ForKind returns an apply configuration type for the given GroupVersionKind, or nil if no
// apply configuration type exists for the given GroupVersionKind.
func ForKind(kind schema.GroupVersionKind) interface{} {
	switch kind {
	// Group=k8s.ovn.org, Version=v1
	case v1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork"):
		return &userdefinednetworkv1.ClusterUserDefinedNetworkApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetworkSpec"):
		return &userdefinednetworkv1.ClusterUserDefinedNetworkSpecApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetworkStatus"):
		return &userdefinednetworkv1.ClusterUserDefinedNetworkStatusApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("Layer2Config"):
		return &userdefinednetworkv1.Layer2ConfigApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("Layer3Config"):
		return &userdefinednetworkv1.Layer3ConfigApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("Layer3Subnet"):
		return &userdefinednetworkv1.Layer3SubnetApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("NetworkSpec"):
		return &userdefinednetworkv1.NetworkSpecApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("TransportConfig"):
		return &userdefinednetworkv1.TransportConfigApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("UserDefinedNetwork"):
		return &userdefinednetworkv1.UserDefinedNetworkApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("UserDefinedNetworkSpec"):
		return &userdefinednetworkv1.UserDefinedNetworkSpecApplyConfiguration{}
	case v1.SchemeGroupVersion.WithKind("UserDefinedNetworkStatus"):
		return &userdefinednetworkv1.UserDefinedNetworkStatusApplyConfiguration{}

	}
	return nil
}

func NewTypeConverter(scheme *runtime.Scheme) *testing.TypeConverter {
	return &testing.TypeConverter{Scheme: scheme, TypeResolver: internal.Parser()}
}
