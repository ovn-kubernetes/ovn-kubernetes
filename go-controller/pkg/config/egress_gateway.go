// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import "github.com/urfave/cli/v2"

// EgressGatewayHealthCheckTLSConfig holds TLS settings for the EgressGateway
// gRPC health-check channel.
type EgressGatewayHealthCheckTLSConfig struct {
	PrivKey        string `gcfg:"client-privkey"`
	Cert           string `gcfg:"client-cert"`
	CACert         string `gcfg:"client-cacert"`
	CertCommonName string `gcfg:"cert-common-name"`
}

// EgressGatewayHealthCheckTLSFlags capture the independent gateway TLS options.
var EgressGatewayHealthCheckTLSFlags = []cli.Flag{
	&cli.StringFlag{
		Name:        "egressgateway-client-privkey",
		Usage:       "Private key used by the EgressGateway gRPC health-check channel",
		Destination: &cliConfig.EgressGatewayHealthCheckTLS.PrivKey,
	},
	&cli.StringFlag{
		Name:        "egressgateway-client-cert",
		Usage:       "Server certificate used by the EgressGateway gRPC health-check channel",
		Destination: &cliConfig.EgressGatewayHealthCheckTLS.Cert,
	},
	&cli.StringFlag{
		Name:        "egressgateway-client-cacert",
		Usage:       "CA certificate used by the EgressGateway gRPC health-check channel",
		Destination: &cliConfig.EgressGatewayHealthCheckTLS.CACert,
	},
	&cli.StringFlag{
		Name:        "egressgateway-cert-common-name",
		Usage:       "Expected TLS server name for the EgressGateway gRPC health-check channel",
		Destination: &cliConfig.EgressGatewayHealthCheckTLS.CertCommonName,
	},
}

// buildEgressGatewayHealthCheckTLSConfig follows EgressIP's file-then-CLI precedence.
func buildEgressGatewayHealthCheckTLSConfig(fileTLS, cliTLS *EgressGatewayHealthCheckTLSConfig) (*EgressGatewayHealthCheckTLSConfig, error) {
	tlsConfig := savedEgressGatewayHealthCheckTLS
	if err := overrideFields(&tlsConfig, fileTLS, &savedEgressGatewayHealthCheckTLS); err != nil {
		return nil, err
	}
	if err := overrideFields(&tlsConfig, cliTLS, &savedEgressGatewayHealthCheckTLS); err != nil {
		return nil, err
	}
	return &tlsConfig, nil
}
