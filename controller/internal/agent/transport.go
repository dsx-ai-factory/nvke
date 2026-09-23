// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"

	"github.com/spiffe/go-spiffe/v2/spiffegrpc/grpccredentials"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dsx-ai-factory/nke/controller/internal/auth"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// Transport dials node agents using the deployment's configured
// authentication.
//
// Every caller that reaches an agent goes through it. That is a requirement
// rather than a convenience: an agent running with TLS/JWT writes an
// authentication record onto the connection before HTTP/2 starts, and a dial
// path that does not read that record sees the bytes as a corrupt HTTP/2
// preface. A second implementation of this logic is a second way for a node to
// become unreachable when the deployment turns TLS/JWT or external mTLS on.
type Transport struct {
	cfg                       config.Config
	trustDomain               string
	tlsJWTVerifier            *auth.Verifier
	logger                    *slog.Logger
	externalClientCertificate *tls.Certificate
}

// NewTransport builds the node-agent transport for this deployment. When
// TLS/JWT is enabled it sets up token verification up front, so a bad issuer or
// JWKS URI fails at startup rather than on the first node dial.
func NewTransport(
	ctx context.Context,
	cfg config.Config,
	trustDomain string,
	logger *slog.Logger,
) (*Transport, error) {
	if logger == nil {
		logger = slog.Default()
	}

	var tlsJWTVerifier *auth.Verifier
	var tlsJWTLogger *slog.Logger
	if cfg.NodeAgent.TLSJWT.Enabled {
		tlsJWTLogger = logger.With("logger", "node-agent-auth")
		verifier, err := auth.SetupTokenVerifier(ctx, &auth.Config{
			Audiences: []string{auth.NodeAgentAudience},
			Issuers: []auth.Issuer{{
				Name:       auth.NodeAgentIssuerName,
				Issuer:     cfg.NodeAgent.TLSJWT.Issuer,
				JWKS:       &auth.JWKSKeys{URI: cfg.NodeAgent.TLSJWT.JWKSURI},
				Algorithms: cfg.NodeAgent.TLSJWT.Algorithms,
			}},
		}, tlsJWTLogger)
		if err != nil {
			return nil, fmt.Errorf("setting up node-agent token verification: %w", err)
		}
		tlsJWTVerifier = verifier
	}

	var externalClientCertificate *tls.Certificate
	if cfg.NodeAgentExternalMTLSEnabled {
		certificate, err := tls.LoadX509KeyPair(
			config.NodeAgentExternalTLSCertPath,
			config.NodeAgentExternalTLSKeyPath,
		)
		if err != nil {
			return nil, fmt.Errorf("loading external node-agent client identity: %w", err)
		}
		externalClientCertificate = &certificate
	}

	return &Transport{
		cfg:                       cfg,
		trustDomain:               trustDomain,
		tlsJWTVerifier:            tlsJWTVerifier,
		logger:                    tlsJWTLogger,
		externalClientCertificate: externalClientCertificate,
	}, nil
}

// Dial opens a connection to the node's agent and returns it with the cleanup
// the caller must invoke.
func (t *Transport) Dial(ctx context.Context, node db.Node) (*grpc.ClientConn, func(), error) {
	endpoint := net.JoinHostPort(node.IP, t.cfg.NodeAgent.Port)

	var closeFuncs []func() error
	cleanup := func() {
		for _, f := range closeFuncs {
			_ = f()
		}
	}

	// Select the connection's base TLS independently from the optional pre-gRPC
	// JWT exchange. External mTLS is deployment-wide and takes precedence over
	// the node's legacy SPIFFE bit.
	var baseTransportCredentials credentials.TransportCredentials
	baseSupportsTLSJWT := false
	switch {
	case t.externalClientCertificate != nil:
		baseTransportCredentials = credentials.NewTLS(t.externalTLSConfig())
		baseSupportsTLSJWT = true
	case !node.SpiffeEnabled:
		baseTransportCredentials = insecure.NewCredentials()
	default:
		spiffeCredentials, spiffeCloseFuncs, err := t.spiffeTransportCredentials(ctx, node)
		if err != nil {
			return nil, nil, err
		}
		closeFuncs = append(closeFuncs, spiffeCloseFuncs...)
		baseTransportCredentials = spiffeCredentials
		baseSupportsTLSJWT = true
	}

	transportCredentials := baseTransportCredentials
	if t.tlsJWTVerifier != nil {
		if !baseSupportsTLSJWT {
			cleanup()
			return nil, nil, fmt.Errorf("node-agent base transport does not support TLS/JWT authentication")
		}
		var err error
		transportCredentials, err = newTLSJWTTransportCredentials(
			baseTransportCredentials,
			t.tlsJWTVerifier,
			t.cfg.NodeAgent.TLSJWT.SubjectTrustDomain,
			node,
			t.logger,
		)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("setting up node-agent TLS/JWT transport: %w", err)
		}
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(transportCredentials))
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to connect to node agent: %w", err)
	}
	closeFuncs = append(closeFuncs, conn.Close)

	return conn, cleanup, nil
}

func (t *Transport) externalTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		Certificates:       []tls.Certificate{*t.externalClientCertificate},
		InsecureSkipVerify: true, //nolint:gosec // The agent presents an intentionally unverified self-signed server certificate.
	}
}

func (t *Transport) spiffeTransportCredentials(
	ctx context.Context,
	node db.Node,
) (credentials.TransportCredentials, []func() error, error) {
	if t.trustDomain == "" {
		return nil, nil, fmt.Errorf("trust domain is required for SPIFFE authentication")
	}

	expectedID, err := spiffeid.FromString(node.GetSpiffeID(t.trustDomain))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse node SPIFFE ID: %w", err)
	}

	workloadClient, err := workloadapi.New(ctx, workloadapi.WithAddr(t.cfg.SpiffeEndpointSocket))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create SPIFFE workload API client: %w", err)
	}
	closeFuncs := make([]func() error, 0, 2)
	closeFuncs = append(closeFuncs, workloadClient.Close)

	x509Source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClient(workloadClient))
	if err != nil {
		_ = workloadClient.Close()
		return nil, nil, fmt.Errorf("failed to create SPIFFE X509 source: %w", err)
	}
	closeFuncs = append(closeFuncs, x509Source.Close)

	return grpccredentials.MTLSClientCredentials(
		x509Source,
		x509Source,
		tlsconfig.AuthorizeID(expectedID),
	), closeFuncs, nil
}
