// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc/credentials"

	"github.com/dsx-ai-factory/nvke/agent/pkg/nkeauth"

	"github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	handshakeTimeout = 15 * time.Second
)

var _ credentials.TransportCredentials = (*tlsJWTTransportCredentials)(nil)

type tlsJWTTransportCredentials struct {
	base        credentials.TransportCredentials
	verifier    *authn.Verifier
	node        db.Node
	trustDomain spiffeid.TrustDomain
	logger      *slog.Logger
}

func newTLSJWTTransportCredentials(
	base credentials.TransportCredentials,
	verifier *authn.Verifier,
	subjectTrustDomain string,
	node db.Node,
	logger *slog.Logger,
) (*tlsJWTTransportCredentials, error) {
	if base == nil {
		return nil, errors.New("TLS/JWT authentication requires base transport credentials")
	}
	if verifier == nil {
		return nil, errors.New("TLS/JWT authentication requires a token verifier")
	}
	trustDomain, err := spiffeid.TrustDomainFromString(subjectTrustDomain)
	if err != nil {
		return nil, fmt.Errorf("parsing node-agent subject trust domain: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &tlsJWTTransportCredentials{
		base:        base,
		verifier:    verifier,
		node:        node,
		trustDomain: trustDomain,
		logger:      logger,
	}, nil
}

func (c *tlsJWTTransportCredentials) ClientHandshake(
	ctx context.Context,
	authority string,
	rawConn net.Conn,
) (net.Conn, credentials.AuthInfo, error) {
	deadline := time.Now().Add(handshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := rawConn.SetDeadline(deadline); err != nil {
		return nil, nil, fmt.Errorf("setting TLS/JWT handshake deadline: %w", err)
	}

	conn, authInfo, err := c.base.ClientHandshake(ctx, authority, rawConn)
	if err != nil {
		return nil, nil, fmt.Errorf("establishing encrypted node-agent connection: %w", err)
	}
	fail := func(err error) (net.Conn, credentials.AuthInfo, error) {
		_ = conn.Close()
		return nil, nil, err
	}

	token, err := nkeauth.ReadJWT(conn)
	if err != nil {
		return fail(fmt.Errorf("reading node-agent authentication record: %w", err))
	}
	principal, err := c.verifier.Verify(ctx, token)
	if err != nil {
		return fail(fmt.Errorf("verifying node-agent token: %w", err))
	}
	if err := verifyNodeAudience(principal.Claims); err != nil {
		return fail(err)
	}
	if err := verifyNodeSubject(principal.Subject, c.trustDomain, c.node.ID.String()); err != nil {
		return fail(err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fail(fmt.Errorf("clearing TLS/JWT handshake deadline: %w", err))
	}

	c.logger.Debug("node-agent TLS/JWT handshake authenticated", "node_id", c.node.ID, "node_name", c.node.Name)
	return conn, authInfo, nil
}

func (c *tlsJWTTransportCredentials) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("TLS/JWT node-agent credentials support client handshakes only")
}

func (c *tlsJWTTransportCredentials) Info() credentials.ProtocolInfo {
	info := c.base.Info()
	info.SecurityProtocol = "tls-jwt"
	return info
}

func (c *tlsJWTTransportCredentials) Clone() credentials.TransportCredentials {
	return c.clone()
}

func (c *tlsJWTTransportCredentials) clone() *tlsJWTTransportCredentials {
	clone := *c
	clone.base = c.base.Clone()
	return &clone
}

func (c *tlsJWTTransportCredentials) OverrideServerName(serverNameOverride string) error {
	// Required by credentials.TransportCredentials even though gRPC no longer
	// invokes it; delegate so callers using the interface still get its behavior.
	//nolint:staticcheck // Compatibility implementation for the gRPC interface.
	return c.base.OverrideServerName(serverNameOverride)
}

func verifyNodeAudience(claims jwt.MapClaims) error {
	audiences, err := claims.GetAudience()
	if err != nil {
		return fmt.Errorf("node-agent token audience is unreadable: %w", err)
	}
	if len(audiences) != 1 || !slices.Contains(authn.NodeAgentAudiences(), audiences[0]) {
		return fmt.Errorf("node-agent token audience must contain only %q or %q", authn.NodeAgentAudience, authn.NodeAgentAudienceLegacy)
	}
	return nil
}

func verifyNodeSubject(subject string, trustDomain spiffeid.TrustDomain, nodeID string) error {
	parsed, err := spiffeid.FromString(subject)
	if err != nil {
		return fmt.Errorf("node-agent token subject is not a SPIFFE ID: %w", err)
	}
	wantPath := "/bm/" + nodeID
	if parsed.TrustDomain() != trustDomain || parsed.Path() != wantPath || parsed.String() != subject {
		return fmt.Errorf("node-agent token subject does not identify expected node %s", nodeID)
	}
	return nil
}
