// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
)

func TestEgressIPHealthServerCheck(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		service       string
		expectedState grpc_health_v1.HealthCheckResponse_ServingStatus
	}{
		{
			name:          "egress ip service is serving",
			service:       serviceEgressIPNode,
			expectedState: grpc_health_v1.HealthCheckResponse_SERVING,
		},
		{
			name:          "unknown service is not serving",
			service:       "unknown-service",
			expectedState: grpc_health_v1.HealthCheckResponse_NOT_SERVING,
		},
		{
			name:          "empty service is not serving",
			service:       "",
			expectedState: grpc_health_v1.HealthCheckResponse_NOT_SERVING,
		},
	}

	server := healthServer{}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			response, err := server.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{Service: tc.service})
			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if response.GetStatus() != tc.expectedState {
				t.Fatalf("expected status %s, got %s", tc.expectedState, response.GetStatus())
			}
		})
	}
}

func TestEgressIPHealthClientProbeReportsServingResponseHealthy(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed listening for health server: %v", err)
	}
	server := grpc.NewServer()
	grpc_health_v1.RegisterHealthServer(server, &healthServer{})

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		if err := server.Serve(listener); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("failed serving health server: %v", err)
		}
	}()
	t.Cleanup(func() {
		server.Stop()
		<-serverDone
	})

	dialCtx := context.Background()
	// Ignore SA1019, production client still uses DialContext.
	//nolint:staticcheck
	conn, err := grpc.DialContext(
		dialCtx,
		listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed dialing health server: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("failed closing client connection: %v", err)
		}
	})

	client := &egressIPHealthClient{
		nodeName:    "node1",
		nodeAddr:    listener.Addr().String(),
		conn:        conn,
		probeFailed: true,
	}

	if !client.Probe(context.Background()) {
		t.Fatal("expected probe to report healthy for serving response")
	}
	if client.probeFailed {
		t.Fatal("expected successful probe to clear previous probe failure")
	}
}
