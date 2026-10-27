// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/spiffe/go-spiffe/v2/spiffegrpc/grpccredentials"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodecontract"
)

// Transport dials node agents using each node's pinned authentication profile.
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
	tlsJWTVerifier            *authn.Verifier
	logger                    *slog.Logger
	externalClientCertificate *tls.Certificate
	// externalClientCertificateNewURN carries the new-form controller URN. It is
	// optional: nil means every node is dialed with externalClientCertificate.
	externalClientCertificateNewURN *tls.Certificate
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

	var tlsJWTVerifier *authn.Verifier
	var tlsJWTLogger *slog.Logger
	if cfg.NodeAgent.TLSJWT.Enabled {
		tlsJWTLogger = logger.With("logger", "node-agent-auth")
		verifier, err := authn.SetupTokenVerifier(ctx, &authn.Config{
			Audiences: authn.NodeAgentAudiences(),
			Issuers: []authn.Issuer{{
				Name:       authn.NodeAgentIssuerName,
				Issuer:     cfg.NodeAgent.TLSJWT.Issuer,
				JWKS:       &authn.JWKSKeys{URI: cfg.NodeAgent.TLSJWT.JWKSURI},
				Algorithms: cfg.NodeAgent.TLSJWT.Algorithms,
			}},
		}, tlsJWTLogger)
		if err != nil {
			return nil, fmt.Errorf("setting up node-agent token verification: %w", err)
		}
		tlsJWTVerifier = verifier
	}

	var externalClientCertificate, newURNCertificate *tls.Certificate
	if cfg.NodeAgentExternalMTLSEnabled {
		certificate, err := tls.LoadX509KeyPair(
			config.NodeAgentExternalTLSCertPath,
			config.NodeAgentExternalTLSKeyPath,
		)
		if err != nil {
			return nil, fmt.Errorf("loading external node-agent client identity: %w", err)
		}
		externalClientCertificate = &certificate

		newURNCertificate, err = loadOptionalClientCertificate(
			config.NodeAgentExternalTLSNVKECertPath,
			config.NodeAgentExternalTLSNVKEKeyPath,
		)
		if err != nil && !errors.Is(err, errClientCertificateAbsent) {
			return nil, err
		}
	}

	return &Transport{
		cfg:                             cfg,
		trustDomain:                     trustDomain,
		tlsJWTVerifier:                  tlsJWTVerifier,
		logger:                          tlsJWTLogger,
		externalClientCertificate:       externalClientCertificate,
		externalClientCertificateNewURN: newURNCertificate,
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

	// The node's persisted profile selects its transport. Deployment settings
	// only make credentials available for profiles that require them.
	var baseTransportCredentials credentials.TransportCredentials
	baseSupportsTLSJWT := false
	switch {
	case node.NodeAgentExternalMTLSEnabled:
		if t.externalClientCertificate == nil {
			return nil, nil, fmt.Errorf("external mTLS client certificate is unavailable for node %s", node.ID)
		}
		baseTransportCredentials = credentials.NewTLS(t.externalTLSConfig(node))
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
	if node.NodeAgentTLSJWTEnabled {
		if t.tlsJWTVerifier == nil {
			cleanup()
			return nil, nil, fmt.Errorf("TLS/JWT verifier is unavailable for node %s", node.ID)
		}
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

	conn, err := grpc.NewClient(
		endpoint,
		grpc.WithTransportCredentials(transportCredentials),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to connect to node agent: %w", err)
	}
	closeFuncs = append(closeFuncs, conn.Close)

	return conn, cleanup, nil
}

// externalClientCertificateFor picks the controller identity whose URI SAN the
// node's agent accepts: the new-form URN only for an agent version that speaks
// it, and only when that identity is deployed.
func (t *Transport) externalClientCertificateFor(node db.Node) *tls.Certificate {
	if t.externalClientCertificateNewURN != nil && nodecontract.IsNewForm(node.AgentVersion) {
		return t.externalClientCertificateNewURN
	}
	return t.externalClientCertificate
}

// errClientCertificateAbsent reports that an optional client certificate file does not exist.
var errClientCertificateAbsent = errors.New("client certificate file does not exist")

// loadOptionalClientCertificate returns errClientCertificateAbsent when the certificate file does not exist.
func loadOptionalClientCertificate(certPath, keyPath string) (*tls.Certificate, error) {
	if _, err := os.Stat(certPath); errors.Is(err, os.ErrNotExist) {
		return nil, errClientCertificateAbsent
	}
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("loading new-URN external node-agent client identity: %w", err)
	}
	return &certificate, nil
}

func (t *Transport) externalTLSConfig(node db.Node) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		Certificates:       []tls.Certificate{*t.externalClientCertificateFor(node)},
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

	expectedID, err := spiffeid.FromString(node.GetSpiffeID(
		t.trustDomain,
		t.cfg.NodeAgent.TLSJWT.SubjectTenant,
		t.cfg.NodeAgent.TLSJWT.SubjectService,
	))
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
