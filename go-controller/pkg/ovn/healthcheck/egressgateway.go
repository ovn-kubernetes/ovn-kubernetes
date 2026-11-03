// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"context"
	"net"
	"strconv"

	"golang.org/x/net/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
)

const (
	serviceEgressGatewayNode = "Service_Egress_Gateway"
)

// EgressGatewayHealthClient interface offers the functions needed for connecting to
// the egress gateway health check service.
type EgressGatewayHealthClient interface {
	IsConnected() bool
	Connect(dialCtx context.Context, mgmtIPs []net.IP, healthCheckPort int) bool
	Disconnect()
	Probe(dialCtx context.Context) bool
}

type egressGatewayHealthClient struct {
	nodeName string
	nodeAddr string
	conn     *grpc.ClientConn
	// the probeFailed state is used to mitigate situations when
	// connection just went down. With that, we do not declare node
	// unreachable unless connection could not be re-established.
	probeFailed bool
}

// NewEgressGatewayHealthClient allocates an Egress Gateway health client.
func NewEgressGatewayHealthClient(nodeName string) EgressGatewayHealthClient {
	return &egressGatewayHealthClient{nodeName: nodeName}
}

// IsConnected returns whether client session is established or not.
func (ehc *egressGatewayHealthClient) IsConnected() bool {
	return ehc.conn != nil
}

// Connect attempts to establish gRPC session with the egress gateway health check service.
func (ehc *egressGatewayHealthClient) Connect(dialCtx context.Context, mgmtIPs []net.IP, healthCheckPort int) bool {
	var conn *grpc.ClientConn
	var nodeAddr string
	var err error

	// Ignore SA1019, PR#4286 should refactor this.
	// nolint:staticcheck
	opts := []grpc.DialOption{
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, s string) (net.Conn, error) {
			return proxy.Dial(ctx, "tcp", s)
		}),
	}
	cfg := &config.EgressGatewayHealthCheckTLS
	if cfg.CACert == "" || cfg.CertCommonName == "" {
		klog.Warning("Health checking using insecure connection")
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		// Set up the credentials for the connection.
		creds, err := credentials.NewClientTLSFromFile(cfg.CACert, cfg.CertCommonName)
		if err != nil {
			klog.Errorf("Health checking TLS key failed: %v", err)
			return false
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	}
	for _, nodeMgmtIP := range mgmtIPs {
		nodeAddr = net.JoinHostPort(nodeMgmtIP.String(), strconv.Itoa(healthCheckPort))
		// Ignore SA1019, PR#4286 should refactor this.
		//nolint:staticcheck
		conn, err = grpc.DialContext(dialCtx, nodeAddr, opts...)
		if err == nil && conn != nil {
			break
		}
		klog.Warningf("Could not connect to %s (%s): %v", ehc.nodeName, nodeAddr, err)
	}
	if conn == nil {
		return false
	}

	klog.Infof("Connected to %s (%s)", ehc.nodeName, nodeAddr)
	ehc.nodeAddr = nodeAddr
	ehc.conn = conn
	return true
}

// Disconnect stops gRPC session with the egress gateway health check service.
func (ehc *egressGatewayHealthClient) Disconnect() {
	if ehc.conn != nil {
		klog.Infof("Closing connection with %s (%s)", ehc.nodeName, ehc.nodeAddr)
		ehc.conn.Close()
		ehc.conn = nil
	}
}

// Probe checks the health of egress gateway service using a connected gRPC session.
func (ehc *egressGatewayHealthClient) Probe(dialCtx context.Context) bool {
	if ehc.conn == nil {
		// should never happen
		klog.Warningf("Unexpected probing before connecting %s", ehc.nodeName)
		return false
	}

	response, err := grpc_health_v1.NewHealthClient(ehc.conn).Check(dialCtx, &grpc_health_v1.HealthCheckRequest{Service: serviceEgressGatewayNode})
	if err != nil {
		// TODO: Add configurable failure thresholds and retry intervals to tune failover,
		// e.g. retry every second after a failed probe while tolerating transient failures.
		// check failed. What we will return here will depend on ehc.probeFailed. If this is the first failure,
		// let's tolerate it to account for cases where session went down and we just need it re-established.
		// Otherwise, declare it failed.
		klog.V(5).Infof("Probe failed %s (%s): %s", ehc.nodeName, ehc.nodeAddr, err)
		ehc.Disconnect()
		prevProbeFailed := ehc.probeFailed
		ehc.probeFailed = true
		return !prevProbeFailed
	}

	ehc.probeFailed = false
	klog.V(5).Infof("Got response from %s (%s): %v", ehc.nodeName, ehc.nodeAddr, response.GetStatus())
	return response.GetStatus() == grpc_health_v1.HealthCheckResponse_SERVING
}
