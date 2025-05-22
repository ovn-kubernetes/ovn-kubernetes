package podresourcesapi

import (
	"context"
	"fmt"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
)

type PodResClient struct {
	podresourcesapi.PodResourcesListerClient
	conn *grpc.ClientConn
}

// New initializes a new podresources client with the given socket path.
func New(socket string) (*PodResClient, error) {
	socketPath := fmt.Sprintf("unix://%s", filepath.Clean(socket))

	conn, err := grpc.NewClient(socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to podresources socket: %w", err)
	}

	client := podresourcesapi.NewPodResourcesListerClient(conn)
	return &PodResClient{conn: conn, PodResourcesListerClient: client}, nil
}

// Close closes the gRPC connection
func (c *PodResClient) Close() error {
	return c.conn.Close()
}

var _ podresourcesapi.PodResourcesListerClient = (*MockPodResourcesListerClient)(nil)

// MockPodResourcesListerClient implements podresourcesapi.PodResourcesListerClient
type MockPodResourcesListerClient struct {
	allocatableCPUs []int64
	usedCPUs        [][]int64
}

func NewMock(allocatableCPUs []int64, usedCPUs [][]int64) *MockPodResourcesListerClient {
	return &MockPodResourcesListerClient{
		allocatableCPUs: allocatableCPUs,
		usedCPUs:        usedCPUs,
	}
}

func (m *MockPodResourcesListerClient) GetAllocatableResources(_ context.Context, _ *podresourcesapi.AllocatableResourcesRequest, _ ...grpc.CallOption) (*podresourcesapi.AllocatableResourcesResponse, error) {
	return &podresourcesapi.AllocatableResourcesResponse{
		CpuIds: m.allocatableCPUs,
	}, nil
}

func (m *MockPodResourcesListerClient) List(_ context.Context, _ *podresourcesapi.ListPodResourcesRequest, _ ...grpc.CallOption) (*podresourcesapi.ListPodResourcesResponse, error) {
	pods := []*podresourcesapi.PodResources{}
	for _, used := range m.usedCPUs {
		pods = append(pods, &podresourcesapi.PodResources{
			Containers: []*podresourcesapi.ContainerResources{
				{CpuIds: used},
			},
		})
	}
	return &podresourcesapi.ListPodResourcesResponse{
		PodResources: pods,
	}, nil
}

// Get Noop
func (m *MockPodResourcesListerClient) Get(_ context.Context, _ *podresourcesapi.GetPodResourcesRequest, _ ...grpc.CallOption) (*podresourcesapi.GetPodResourcesResponse, error) {
	return nil, nil
}
