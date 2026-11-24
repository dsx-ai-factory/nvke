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
	// NodeAgentAudienceLegacy is the audience bootstrapped agents request, so
	// the controller accepts it beside NodeAgentAudience.
	//
	// It stays: no release removes it. The agent picks its audience with
	// --controller-audience, which defaults to this constant and falls back to
	// it when empty (agent/cmd/agent/main.go). nodeAgentBootstrapFiles never
	// passes the flag (controller/internal/temporal/activities), so agents it
	// bootstraps request the legacy audience whatever version they report.
	// Moving them off it (#2039) was dropped. Removed without that move, those
	// nodes fail closed in controller/internal/agent/tlsjwt.go, which is
	// enforcing-only.
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
