package factory

import (
	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
	"k8s.io/client-go/tools/cache"
)

// ObjectCacheInterface represents the exported methods for getting
// kubernetes resources from the informer cache
type ObjectCacheInterface interface {
	GetPod(namespace, name string) (*kapi.Pod, error)
	GetPods(namespace string) ([]*kapi.Pod, error)
	GetNodes() ([]*kapi.Node, error)
	GetNode(name string) (*kapi.Node, error)
	GetService(namespace, name string) (*kapi.Service, error)
	GetServiceEndpointSlices(namespace, svcName string) ([]*discovery.EndpointSlice, error)
	GetNamespace(name string) (*kapi.Namespace, error)
	GetNamespaces() ([]*kapi.Namespace, error)
}

// NodeWatchFactory is an interface that ensures node components only use informers available in a
// node context; under the hood, it's all the same watchFactory.
//
// If you add a new method here, make sure the underlying informer is started
// in factory.go NewNodeWatchFactory
type NodeWatchFactory interface {
	Shutdownable

	AddServiceHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) *Handler
	AddFilteredServiceHandler(namespace string, handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) *Handler
	RemoveServiceHandler(handler *Handler)

	AddEndpointSliceHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) *Handler

	AddPodHandler(handlerFuncs cache.ResourceEventHandler, processExisting func([]interface{})) *Handler
	RemovePodHandler(handler *Handler)

	NodeInformer() cache.SharedIndexInformer
	LocalPodInformer() cache.SharedIndexInformer

	GetService(namespace, name string) (*kapi.Service, error)
	GetServiceEndpointSlices(namespace, svcName string) ([]*discovery.EndpointSlice, error)
}

type Shutdownable interface {
	Shutdown()
}
