package mocks

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
)

var _ podresourcesapi.PodResourcesListerClient = (*PodResourcesAPIClient)(nil)

// PodResourcesAPIClient is a simple mock implementation of PodResourcesListerClient
type PodResourcesAPIClient struct {
	allocatableCPUs []int64
	usedCPUs        [][]int64 // outer slice = containers, inner slice = CPUs per container
}

// Get no-op
func (m *PodResourcesAPIClient) Get(_ context.Context, _ *podresourcesapi.GetPodResourcesRequest, _ ...grpc.CallOption) (*podresourcesapi.GetPodResourcesResponse, error) {
	return &podresourcesapi.GetPodResourcesResponse{}, nil
}

// NewPodResourcesAPIClient creates a new mock client with the specified allocatable and used CPUs
func NewPodResourcesAPIClient(allocatableCPUs []int64, usedCPUs [][]int64) *PodResourcesAPIClient {
	return &PodResourcesAPIClient{
		allocatableCPUs: allocatableCPUs,
		usedCPUs:        usedCPUs,
	}
}

// List returns mock pod resources data
func (m *PodResourcesAPIClient) List(_ context.Context, _ *podresourcesapi.ListPodResourcesRequest, _ ...grpc.CallOption) (*podresourcesapi.ListPodResourcesResponse, error) {
	var podResources []*podresourcesapi.PodResources

	if len(m.usedCPUs) > 0 {
		// Create containers for each set of CPU allocations
		var containers []*podresourcesapi.ContainerResources
		for i, containerCPUs := range m.usedCPUs {
			if len(containerCPUs) > 0 {
				containers = append(containers, &podresourcesapi.ContainerResources{
					Name:   fmt.Sprintf("container-%d", i),
					CpuIds: containerCPUs,
				})
			}
		}

		if len(containers) > 0 {
			// Create a mock pod using the specified containers and CPUs
			podResources = append(podResources, &podresourcesapi.PodResources{
				Name:       "test-pod",
				Namespace:  "default",
				Containers: containers,
			})
		}
	}

	return &podresourcesapi.ListPodResourcesResponse{
		PodResources: podResources,
	}, nil
}

// GetAllocatableResources returns mock allocatable resources data
func (m *PodResourcesAPIClient) GetAllocatableResources(_ context.Context, _ *podresourcesapi.AllocatableResourcesRequest, _ ...grpc.CallOption) (*podresourcesapi.AllocatableResourcesResponse, error) {
	return &podresourcesapi.AllocatableResourcesResponse{
		CpuIds: m.allocatableCPUs,
	}, nil
}
