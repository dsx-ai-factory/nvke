// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"log/slog"
)

// Options is what a deployment configures. ConfigPath alone decides whether
// authentication runs at all; the other two only matter once it does.
type Options struct {
	// ConfigPath is the mounted YAML file holding the issuer registry. Empty
	// means authentication is disabled, which is the default.
	ConfigPath string

	// Permissive evaluates and logs decisions without acting on them. It is an
	// environment variable rather than a field in the configuration file because
	// it is the one setting an operator flips under pressure, so flipping it must
	// not risk taking the process down with a bad YAML edit.
	Permissive bool

	// Recorder counts decisions. Optional: nil records nothing.
	Recorder Recorder
}

// Setup assembles the three states described in this package's doc comment.
//
// It returns (nil, nil) for "disabled", which is what an empty ConfigPath means
// and what every deployment gets until an operator says otherwise. A nil
// Authenticator is not a permissive one: the caller is expected to leave the
// middleware out of the request chain entirely, so a disabled deployment serves
// requests exactly as it did before this package existed.
//
// A ConfigPath that is set but cannot be read, parsed or validated is an error,
// never "disabled". A typo'd mount path must fail loudly rather than quietly
// produce a server that authenticates nobody.
func Setup(ctx context.Context, opts Options, logger *slog.Logger) (*Authenticator, error) {
	if opts.ConfigPath == "" {
		logger.Info("API authentication is disabled", "reason", "no authentication config path is set")
		return nil, nil //nolint:nilnil // a nil Authenticator and a nil error is exactly "disabled"
	}

	cfg, err := Load(opts.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading authentication config %q: %w", opts.ConfigPath, err)
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
		"config_path", opts.ConfigPath,
		"audiences", cfg.Audiences,
		"issuers", issuers,
	)

	return authenticator, nil
}
