package testing

import (
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/utils/ptr"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// USED ONLY FOR TESTING

// makeReadyEndpointList returns a list of only one endpoint that carries the input addresses.
func MakeReadyEndpointList(node string, addresses ...string) []discovery.Endpoint {
	return []discovery.Endpoint{
		MakeReadyEndpoint(node, addresses...),
	}
}

func MakeReadyEndpoint(node string, addresses ...string) discovery.Endpoint {
	return discovery.Endpoint{
		Conditions: discovery.EndpointConditions{
			Ready:       ptr.To(true),
			Serving:     ptr.To(true),
			Terminating: ptr.To(false),
		},
		Addresses: addresses,
		NodeName:  &node,
	}
}

func MakeTerminatingServingEndpoint(node string, addresses ...string) discovery.Endpoint {
	return discovery.Endpoint{
		Conditions: discovery.EndpointConditions{
			Ready:       ptr.To(false),
			Serving:     ptr.To(true),
			Terminating: ptr.To(true),
		},
		Addresses: addresses,
		NodeName:  &node,
	}
}

func MakeTerminatingNonServingEndpoint(node string, addresses ...string) discovery.Endpoint {
	return discovery.Endpoint{
		Conditions: discovery.EndpointConditions{
			Ready:       ptr.To(false),
			Serving:     ptr.To(false),
			Terminating: ptr.To(true),
		},
		Addresses: addresses,
		NodeName:  &node,
	}
}

func MakeUnassignedEndpoint(addresses ...string) discovery.Endpoint {
	return discovery.Endpoint{
		Conditions: discovery.EndpointConditions{
			Ready:       ptr.To(true),
			Serving:     ptr.To(true),
			Terminating: ptr.To(false),
		},
		Addresses: addresses,
		NodeName:  nil,
	}
}

func MirrorEndpointSlice(defaultEndpointSlice *discovery.EndpointSlice, network string, keepEndpoints bool) *discovery.EndpointSlice {
	mirror := defaultEndpointSlice.DeepCopy()
	mirror.Name = defaultEndpointSlice.Name + "-mirrored"
	mirror.Labels[discovery.LabelManagedBy] = types.EndpointSliceMirrorControllerName
	mirror.Labels[types.LabelUserDefinedServiceName] = defaultEndpointSlice.Labels[discovery.LabelServiceName]
	if mirror.Annotations == nil {
		mirror.Annotations = make(map[string]string)
	}
	mirror.Annotations[types.SourceEndpointSliceAnnotation] = defaultEndpointSlice.Name
	mirror.Annotations[types.UserDefinedNetworkEndpointSliceAnnotation] = network

	if !keepEndpoints {
		mirror.Endpoints = nil
	}

	return mirror
}
