// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Options is what a deployment configures. Whether either of Endpoint and
// BundleDir is set decides whether authorization runs at all; the rest applies
// only when it does.
type Options struct {
	// BundleDir is the mounted directory holding the policy bundle sources,
	// evaluated in this process with cedar-go. Preferred over Endpoint.
	BundleDir string

	// Endpoint is an external policy engine's base URL, e.g.
	// "http://127.0.0.1:8180" for a cedar-agent sidecar. An alternative to
	// BundleDir, not an addition to it.
	Endpoint string

	// Permissive evaluates and logs every decision without acting on it, so no
	// request is refused.
	Permissive bool

	// Timeout bounds one decision made over Endpoint. Zero uses DefaultTimeout.
	// Unused with BundleDir, where a decision makes no network call.
	Timeout time.Duration

	// Recorder counts decisions. Optional: nil records nothing.
	Recorder Recorder
}

// engine builds the Authorizer this deployment asked for, or reports that
// authorization is disabled by returning a nil Authorizer.
//
// Both set is a configuration error rather than a precedence rule: an operator
// who set both has a belief about which one is running, and a silent winner
// makes that belief unfalsifiable.
func (o Options) engine() (Authorizer, error) {
	switch {
	case o.BundleDir != "" && o.Endpoint != "":
		return nil, errors.New("set either the bundle directory or the engine endpoint, not both")
	case o.BundleDir != "":
		return NewLocal(o.BundleDir)
	case o.Endpoint != "":
		return NewCedarAgent(o.Endpoint, o.Timeout)
	default:
		return nil, nil //nolint:nilnil // a nil Authorizer and a nil error is exactly "disabled"
	}
}

// Provider is a validated authorization configuration waiting for the mux.
//
// It exists because the two halves of setting this up happen at different
// times. Validating the endpoint and gating on the policy engine holding a
// bundle should happen early — before the database migrations, so a bad
// configuration costs a restart rather than a migrated schema — but the
// middleware cannot be built until internal/server has constructed the mux it
// resolves routes against. Setup does the first half; Middleware does the
// second.
type Provider struct {
	authorizer Authorizer
	mode       Mode
	logger     *slog.Logger
	record     Recorder
}

// Setup builds and validates a deployment's authorization engine, or reports
// that authorization is disabled.
//
// Neither BundleDir nor Endpoint set returns (nil, nil), meaning disabled: the
// caller leaves the middleware out of the chain. A nil Provider is not a
// permissive one.
//
// Either one set but broken — a bundle that does not parse, an engine that
// cannot be reached or holds no policy — is an error, never "disabled". In
// enforcing mode that stops the process: an empty policy store refuses every
// request, which from inside a running pod looks exactly like a policy nobody
// wrote, and is a much worse thing to discover at request time than at start
// time.
func Setup(ctx context.Context, opts Options, logger *slog.Logger) (*Provider, error) {
	engine, err := opts.engine()
	if err != nil {
		return nil, fmt.Errorf("building the policy engine: %w", err)
	}
	if engine == nil {
		logger.Info("API authorization is disabled",
			"reason", "neither a policy bundle nor an engine endpoint is set")
		return nil, nil //nolint:nilnil // a nil Provider and a nil error is exactly "disabled"
	}

	mode := ModeEnforcing
	if opts.Permissive {
		mode = ModePermissive
	}
	authzLogger := logger.With("logger", "authz")

	// Enforcing refuses to start without a loaded bundle; permissive logs and
	// carries on, since it refuses nobody anyway.
	if err := engine.Ready(ctx); err != nil {
		if mode == ModeEnforcing {
			return nil, fmt.Errorf("the policy engine is not ready: %w", err)
		}
		authzLogger.Warn("the policy engine is not ready; permissive mode continues anyway", "error", err)
	}

	// Which engine made a decision is the first thing anyone asks when a
	// decision surprises them, so it is a startup line rather than a guess.
	logger.Info("API authorization is enabled",
		"mode", string(mode),
		"engine", engine.Name(),
		"source", cmp.Or(opts.BundleDir, opts.Endpoint),
		"operations", len(operations),
	)

	return &Provider{
		authorizer: engine,
		mode:       mode,
		logger:     authzLogger,
		record:     opts.Recorder,
	}, nil
}

// Middleware builds the middleware over the server's mux. A nil Provider
// returns a nil Middleware, so a disabled deployment needs no branch of its own
// at the call site beyond leaving it out of the chain.
func (p *Provider) Middleware(mux *http.ServeMux) (*Middleware, error) {
	if p == nil {
		return nil, nil //nolint:nilnil // disabled, as above
	}
	if mux == nil {
		return nil, errors.New("authorization requires the server mux")
	}
	resolver, err := NewResolver(mux)
	if err != nil {
		return nil, fmt.Errorf("building the route resolver: %w", err)
	}
	middleware, err := NewMiddleware(p.authorizer, resolver, p.mode, p.logger, p.record)
	if err != nil {
		return nil, fmt.Errorf("building the authorization middleware: %w", err)
	}
	return middleware, nil
}
