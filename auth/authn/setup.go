// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"fmt"
	"log/slog"
)

// Options is what a deployment configures. Config decides whether
// authentication runs at all; the other two apply only when it does.
type Options struct {
	// Permissive evaluates and logs every decision without acting on it, so no
	// request is rejected.
	Permissive bool

	// Recorder counts decisions. Optional: nil records nothing.
	Recorder Recorder

	// Config is the issuer registry read from apiAuth in
	// nvke-controller-config. Nil means authentication is disabled.
	Config *Config
}

// Setup builds the Authenticator for a deployment, or reports that
// authentication is disabled.
//
// With no Config it returns (nil, nil), meaning disabled: the caller leaves the
// middleware out of the chain. A nil Authenticator is not a permissive one.
//
// A Config that fails ValidateStartup is an error, never "disabled".
func Setup(ctx context.Context, opts Options, logger *slog.Logger) (*Authenticator, error) {
	cfg := opts.Config
	if cfg == nil {
		logger.Info("API authentication is disabled", "reason", "apiAuth is not enabled in nvke-controller-config")
		return nil, nil //nolint:nilnil // a nil Authenticator and a nil error is exactly "disabled"
	}
	if err := cfg.ValidateStartup(); err != nil {
		return nil, fmt.Errorf("invalid apiAuth in nvke-controller-config: %w", err)
	}

	mode := ModeEnforcing
	if opts.Permissive {
		mode = ModePermissive
	}

	authLogger := logger.With("logger", "auth")

	// Enforcing refuses to start without every issuer's keys; permissive logs and
	// carries on, since it rejects nobody anyway.
	sources, err := NewKeySources(ctx, cfg, mode, authLogger)
	if err != nil {
		return nil, fmt.Errorf("building issuer key sources: %w", err)
	}

	verifier, err := NewVerifier(cfg, sources)
	if err != nil {
		return nil, fmt.Errorf("building token verifier: %w", err)
	}

	authenticator, err := NewAuthenticator(verifier, mode, authLogger, opts.Recorder)
	if err != nil {
		return nil, fmt.Errorf("building the authentication middleware: %w", err)
	}

	issuers := make([]string, 0, len(cfg.Issuers))
	for _, issuer := range cfg.Issuers {
		issuers = append(issuers, issuer.Name)
	}
	logger.Info("API authentication is enabled",
		"mode", string(mode),
		"audiences", cfg.Audiences,
		"issuers", issuers,
	)

	return authenticator, nil
}
