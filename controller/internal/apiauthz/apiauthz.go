// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package apiauthz authorizes API requests with an auth/authz Client and the
// LocalBundle backend. Middleware maps each request to an auth/authz Target
// after authentication and request validation, and asks the Client. Handlers
// read the authorized caller with auth/authz CallerFromContext and ListTenant.
package apiauthz

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"
	"github.com/dsx-ai-factory/nvke/auth/authz/localbundle"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/metrics"
)

const (
	// reloadInterval is how often runReloads reloads the bundle.
	reloadInterval = 10 * time.Second
)

// Options are Setup's inputs.
type Options struct {
	// Config is apiAuthz from nvke-controller-config.
	Config controllerconfig.APIAuthzConfig

	// Issuers are the names of the issuers authentication trusts. The bundle
	// may name only issuers that authentication trusts.
	Issuers []string

	// Tenants looks up the tenant that owns each object.
	Tenants db.TenantQueries
}

// Setup loads the authorization bundle, reloads it every reloadInterval until
// ctx is done, and returns the middleware that authorizes API requests. It
// returns (nil, nil) when apiAuthz is off.
func Setup(ctx context.Context, opts Options, logger *slog.Logger) (*Middleware, error) {
	if !opts.Config.Enabled {
		logger.Info("API authorization is disabled")
		return nil, nil //nolint:nilnil // a nil Middleware and a nil error is "off"
	}

	dir := strings.TrimSpace(opts.Config.Authorizer.LocalBundle.Dir)
	lb, err := NewLocalBundle(dir, opts.Issuers)
	if err != nil {
		return nil, err
	}
	mode := authn.ModeEnforcing
	if opts.Config.Permissive {
		mode = authn.ModePermissive
	}
	authzLogger := logger.With("logger", "authz")
	record := metrics.AuthzRecorder(mode)
	client, err := authz.NewClient(lb, mode, authzLogger, record)
	if err != nil {
		return nil, fmt.Errorf("build the authorization client: %w", err)
	}
	go runReloads(ctx, lb, reloadInterval, authzLogger)

	logger.Info("API authorization is enabled",
		"mode", mode,
		"backend", lb.Name(),
		"bundle_dir", dir,
		"issuers", opts.Issuers,
	)
	return NewMiddleware(client, opts.Tenants, record, authzLogger), nil
}

// actions returns every operationId in the API spec, sorted. These are the
// actions Middleware asks about. It reads api.OpenapiYAML, because the spec
// api.GetSwagger returns has the operationIds renamed to Go names.
func actions() ([]string, error) {
	spec, err := openapi3.NewLoader().LoadFromData([]byte(api.OpenapiYAML))
	if err != nil {
		return nil, fmt.Errorf("load the OpenAPI spec: %w", err)
	}
	var ids []string
	for _, item := range spec.Paths.Map() {
		for _, op := range item.Operations() {
			ids = append(ids, op.OperationID)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// NewLocalBundle loads the bundle in dir for the API's actions and resource
// types. issuers are the names of the issuers authentication trusts.
func NewLocalBundle(dir string, issuers []string) (*localbundle.LocalBundle, error) {
	ids, err := actions()
	if err != nil {
		return nil, err
	}
	lb, err := localbundle.New(localbundle.Options{
		Dir:           dir,
		Issuers:       issuers,
		Actions:       ids,
		ResourceTypes: resourceTypes,
	})
	if err != nil {
		return nil, fmt.Errorf("load the authorization bundle in %s: %w", dir, err)
	}
	return lb, nil
}

// runReloads reloads lb every interval until ctx is done. A failed reload
// keeps the live bundle.
func runReloads(ctx context.Context, lb *localbundle.LocalBundle, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		changed, err := lb.Reload()
		switch {
		case err != nil:
			logger.Error("Failed to reload the authorization bundle; keeping the live one", "error", err)
		case changed:
			logger.Info("Reloaded the authorization bundle")
		}
	}
}
