package informer

import (
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
)

type ServiceEventHandler interface {
	AddService(*corev1.Service) error
	DeleteService(*corev1.Service) error
	UpdateService(old, new *corev1.Service) error
	SyncServices([]interface{}) error
}

type EndpointSliceEventHandler interface {
	AddEndpointSlice(*discovery.EndpointSlice) error
	DeleteEndpointSlice(*discovery.EndpointSlice) error
	UpdateEndpointSlice(old, new *discovery.EndpointSlice) error
}

type ServiceAndEndpointsEventHandler interface {
	ServiceEventHandler
	EndpointSliceEventHandler
}
