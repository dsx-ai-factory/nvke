// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// Options is what a deployment configures. BundleDir decides whether
// authorization runs at all; the rest applies only when it does.
type Options struct {
	// BundleDir is the mounted directory holding the policy bundle. Empty means
	// authorization is disabled, which is the default.
	BundleDir string

	// Permissive evaluates and logs every decision without acting on it, so no
	// request is refused.
	Permissive bool

	// Recorder counts decisions. Optional: nil records nothing.
	Recorder Recorder

	// IssuerNames is the configured authentication issuers
	// (auth.Authenticator.IssuerNames). Every roleMappings.json entry must name
	// one of them. Nil or empty means no issuer, so any mapping entry fails
	// the load.
	IssuerNames []string

	// GroupOnlyWriteRecorder records how many write operations the loaded
	// bundle covers only through NKE::Action::"write". Optional: nil records
	// nothing.
	GroupOnlyWriteRecorder GroupOnlyWriteRecorder

	// newEngine builds the Authorizer for BundleDir. Nil means a Local loaded
	// from BundleDir and checked against IssuerNames, which is every
	// deployment.
	//
	// It is unexported, so only this package can set it, and only a test does.
	// Setup's readiness gate is the reason it exists: no engine here can
	// construct successfully and then fail Ready, so without a seam that gate
	// is code no test can reach — and a gate nothing exercises is one that can
	// be deleted without anything noticing, which is how it would come to be
	// missing for the engine it is actually there for.
	newEngine func(bundleDir string) (Authorizer, error)
}

// engine builds the Authorizer this deployment asked for, or reports that
// authorization is disabled by returning a nil Authorizer. issuers is
// IssuerNames as a set, built once by Setup.
func (o Options) engine(issuers issuerSet, logger *slog.Logger) (Authorizer, error) {
	if o.BundleDir == "" {
		return nil, nil //nolint:nilnil // a nil Authorizer and a nil error is exactly "disabled"
	}
	if o.newEngine != nil {
		return o.newEngine(o.BundleDir)
	}
	local, err := newLocal(o.BundleDir, issuers, logger)
	if err != nil {
		return nil, err
	}
	return local, nil
}

// Provider is a validated authorization configuration waiting for the mux.
//
// It exists because the two halves of setting this up happen at different
// times. Building the policy engine and gating on it holding a bundle should
// happen early — before the database migrations, so a bad configuration costs
// a restart rather than a migrated schema — but the middleware cannot be built
// until internal/server has constructed the mux it resolves routes against.
// Setup does the first half; Middleware does the second.
type Provider struct {
	authorizer Authorizer
	mode       Mode
	logger     *slog.Logger
	record     Recorder
}

// Setup builds and validates a deployment's authorization engine, or reports
// that authorization is disabled.
//
// An empty BundleDir returns (nil, nil), meaning disabled: the caller leaves the
// middleware out of the chain. A nil Provider is not a permissive one.
//
// A BundleDir that is set but whose bundle does not load is an error, never
// "disabled" — and it is fatal in both modes, because NewLocal fails before the
// mode is consulted. A bundle that did not load refuses every request, which
// from inside a running pod looks exactly like a policy nobody wrote.
//
// A nil logger means slog.Default().
func Setup(ctx context.Context, opts Options, logger *slog.Logger) (*Provider, error) {
	if logger == nil {
		logger = slog.Default()
	}
	issuers := newIssuerSet(opts.IssuerNames)
	authzLogger := logger.With("logger", "authz")
	engine, err := opts.engine(issuers, authzLogger)
	if err != nil {
		return nil, fmt.Errorf("building the policy engine: %w", err)
	}
	if engine == nil {
		logger.Info("API authorization is disabled", "reason", "no policy bundle is configured")
		return nil, nil //nolint:nilnil // a nil Provider and a nil error is exactly "disabled"
	}

	mode := ModeEnforcing
	if opts.Permissive {
		mode = ModePermissive
	}

	// A belt-and-braces check on an invariant NewLocal already enforces. It
	// costs nothing and it is the one place that would notice if a future engine
	// constructed successfully with nothing loaded.
	if err := engine.Ready(ctx); err != nil {
		return nil, fmt.Errorf("the policy engine is not ready: %w", err)
	}

	// Which engine made a decision is the first thing anyone asks when a
	// decision surprises them, so it is a startup line rather than a guess.
	logger.Info("API authorization is enabled",
		"mode", string(mode),
		"engine", engine.Name(),
		"source", opts.BundleDir,
		"operations", len(operations),
	)
	// A bundle that grants the write group grants every write, including one
	// an upgrade adds without any change to the bundle. Nothing refuses that,
	// so each such operation is logged and counted; a change in the count is
	// what shows the widening.
	if local, ok := engine.(*Local); ok {
		reportGroupOnlyWrites(authzLogger, opts.GroupOnlyWriteRecorder, local)
	}

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
