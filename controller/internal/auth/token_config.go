// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

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
	// order. Remove it with the other dual-accept windows.
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
