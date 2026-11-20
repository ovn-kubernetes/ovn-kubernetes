// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package informer

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
)

type ServiceEventHandler interface {
	AddService(context.Context, *corev1.Service) error
	DeleteService(context.Context, *corev1.Service) error
	UpdateService(ctx context.Context, old, new *corev1.Service) error
	SyncServices(context.Context, []interface{}) error
}

type EndpointSliceEventHandler interface {
	AddEndpointSlice(context.Context, *discovery.EndpointSlice) error
	DeleteEndpointSlice(context.Context, *discovery.EndpointSlice) error
	UpdateEndpointSlice(ctx context.Context, old, new *discovery.EndpointSlice) error
}

type ServiceAndEndpointsEventHandler interface {
	ServiceEventHandler
	EndpointSliceEventHandler
}
