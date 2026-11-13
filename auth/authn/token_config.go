// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"fmt"
	"log/slog"
)

const (
	// NodeAgentIssuerName and NodeAgentAudience are protocol constants rather
	// than deployment choices in the first TLS/JWT implementation.
	NodeAgentIssuerName = "dsx-identity"
	NodeAgentAudience   = "nvke-controller"
	// NodeAgentAudienceLegacy is the audience agents request until they move to
	// NodeAgentAudience, so the controller accepts both whatever the release
	// order.
	//
	// The audience moves by the agent's own default, not by the controller
	// selecting it per node (#2039). The controller cannot select it: AGENT_ARGS
	// is rendered before the node has booted an agent, so it has no signal for
	// what the image-installed binary accepts, and agent v0.2.1 takes
	// --grpc-auth-mode=tls-jwt but predates --controller-audience, which arrived
	// in v0.3.0. Handed a flag it does not know, that agent exits during
	// flag.Parse and never comes up far enough to be upgraded over RPC.
	//
	// So an agent requests this audience when it predates the flipped default,
	// or when its frozen AGENT_ARGS names it: the env file is written once at
	// bootstrap, and agent/scripts/migrate-agent-paths.sh preserves its contents
	// across the rename rather than re-rendering them.
	//
	// Gate: every node reports an agent_version at or past the release that
	// carries the flipped default. That is an ordinary sweep over the observed
	// version — the flip writes nothing per node, so the frozen-AGENT_ARGS
	// problem in #2052 does not gate this window. Removed before the gate, every
	// node still on this audience fails closed in
	// controller/internal/agent/tlsjwt.go, which is enforcing-only.
	NodeAgentAudienceLegacy = "nke-controller"
)

// NodeAgentAudiences is the "aud" allow-list for node-agent tokens: both
// spellings while the agent still requests the legacy one.
func NodeAgentAudiences() []string {
	return []string{NodeAgentAudience, NodeAgentAudienceLegacy}
}

// SetupTokenVerifier builds the enforcing verifier used by the transport. A
// configured transport can never run permissively.
func SetupTokenVerifier(
	ctx context.Context,
	cfg *Config,
	logger *slog.Logger,
) (*Verifier, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid node-agent token verification configuration: %w", err)
	}

	sources, err := NewKeySources(ctx, cfg, ModeEnforcing, logger)
	if err != nil {
		return nil, fmt.Errorf("building token verification key sources: %w", err)
	}
	verifier, err := newVerifier(cfg, sources, false)
	if err != nil {
		return nil, fmt.Errorf("building transport token verifier: %w", err)
	}
	return verifier, nil
}
