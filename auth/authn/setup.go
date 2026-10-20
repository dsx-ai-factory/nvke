// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"fmt"
	"log/slog"
)

// Options is what a deployment configures. Config or ConfigPath decides whether
// authentication runs at all; the other two apply only when it does.
type Options struct {
	// ConfigPath is the mounted YAML file holding the issuer registry, for a
	// ConfigMap the chart does not render. Empty means no file.
	ConfigPath string

	// Permissive evaluates and logs every decision without acting on it, so no
	// request is rejected.
	Permissive bool

	// Recorder counts decisions. Optional: nil records nothing.
	Recorder Recorder

	// Config is the issuer registry read from apiAuth in
	// nvke-controller-config. Nil means none. At most one of Config and
	// ConfigPath may be set.
	Config *Config
}

// Where Setup read the issuer registry from, as its startup log names it.
const (
	sourceControllerConfig = "nvke-controller-config"
	sourceConfigPath       = "AUTH_CONFIG_PATH"
)

// Setup builds the Authenticator for a deployment, or reports that
// authentication is disabled.
//
// With neither Config nor ConfigPath it returns (nil, nil), meaning disabled:
// the caller leaves the middleware out of the chain. A nil Authenticator is not
// a permissive one.
//
// A ConfigPath that is set but cannot be read, parsed or validated is an error,
// never "disabled", and so is a Config that fails ValidateStartup.
func Setup(ctx context.Context, opts Options, logger *slog.Logger) (*Authenticator, error) {
	cfg, source, err := opts.load()
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		logger.Info("API authentication is disabled", "reason", "apiAuth is not enabled in nvke-controller-config and no authentication config path is set")
		return nil, nil //nolint:nilnil // a nil Authenticator and a nil error is exactly "disabled"
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
		"source", source,
		"config_path", opts.ConfigPath,
		"audiences", cfg.Audiences,
		"issuers", issuers,
	)

	return authenticator, nil
}

// load returns the issuer registry and the name of its source, or a nil
// registry when authentication is disabled.
func (o Options) load() (*Config, string, error) {
	switch {
	case o.Config != nil && o.ConfigPath != "":
		return nil, "", fmt.Errorf("API authentication is configured twice, by apiAuth in nvke-controller-config and by the file at %q; remove one", o.ConfigPath)
	case o.Config != nil:
		if err := o.Config.ValidateStartup(); err != nil {
			return nil, "", fmt.Errorf("invalid apiAuth in nvke-controller-config: %w", err)
		}
		return o.Config, sourceControllerConfig, nil
	case o.ConfigPath != "":
		cfg, err := Load(o.ConfigPath)
		if err != nil {
			return nil, "", fmt.Errorf("loading authentication config %q: %w", o.ConfigPath, err)
		}
		return cfg, sourceConfigPath, nil
	default:
		return nil, "", nil
	}
}
