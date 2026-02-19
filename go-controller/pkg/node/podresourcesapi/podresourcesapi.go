package podresourcesapi

import (
	"fmt"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
)

const KubeletSocketPath = "/var/lib/kubelet/pod-resources/kubelet.sock"

var _ podresourcesapi.PodResourcesListerClient = (*PodResClient)(nil)

type PodResClient struct {
	podresourcesapi.PodResourcesListerClient
	conn *grpc.ClientConn
}

// New initializes a new podresources client with the given socket path.
func New() (*PodResClient, error) {
	socketPath := fmt.Sprintf("unix://%s", filepath.Clean(KubeletSocketPath))

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
