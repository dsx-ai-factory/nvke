// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package apiauthz authorizes API requests by role, with an auth/authz Client
// and the LocalBundle backend. For each request, it asks whether the caller
// may call the operation. The action is the operation's operationId, and the
// resource is the request path.
package apiauthz

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"
	"github.com/dsx-ai-factory/nvke/auth/authz/localbundle"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/metrics"
)

const (
	// reloadInterval is how often runReloads reloads the bundle.
	reloadInterval = 10 * time.Second

	// resourceType is the type of every resource Middleware asks about.
	resourceType = "API"
)

// Options are Setup's inputs.
type Options struct {
	// Config is apiAuthz from nvke-controller-config.
	Config controllerconfig.APIAuthzConfig

	// Issuers are the names of the issuers authentication trusts. The bundle
	// may name only these.
	Issuers []string
}

// Setup loads the authorization bundle, reloads it every reloadInterval until
// ctx is done, and returns the middleware that authorizes API requests. It
// returns (nil, nil) when apiAuthz is off.
func Setup(ctx context.Context, opts Options, logger *slog.Logger) (api.StrictMiddlewareFunc, error) {
	if !opts.Config.Enabled {
		logger.Info("API authorization is disabled")
		return nil, nil //nolint:nilnil // a nil middleware and a nil error is "off"
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
	client, err := authz.NewClient(lb, mode, authzLogger, metrics.RecordAuthzDecision)
	if err != nil {
		return nil, fmt.Errorf("build the authorization client: %w", err)
	}
	go runReloads(ctx, lb, authzLogger)

	logger.Info("API authorization is enabled",
		"mode", mode,
		"backend", lb.Name(),
		"bundle_dir", dir,
		"issuers", opts.Issuers,
	)
	return Middleware(client, authzLogger), nil
}

// Middleware returns the strict handler middleware that asks client whether
// the caller may call each operation. The generated handler calls it after
// request validation, so a malformed request gets 400 first. On a refusal it
// writes the response and returns no response object, so the generated
// handler writes nothing more.
func Middleware(client *authz.Client, logger *slog.Logger) api.StrictMiddlewareFunc {
	return func(next api.StrictHandlerFunc, operationID string) api.StrictHandlerFunc {
		// operationID is the Go name: the spec's operationId, capitalized.
		action := strings.ToLower(operationID[:1]) + operationID[1:]
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
			ctx, err := client.Check(ctx, authz.Target{
				Action:   action,
				Resource: authz.Resource{Type: resourceType, ID: r.URL.Path, Scope: authz.ScopeGlobal},
			})
			if err != nil {
				authz.WriteError(w, r, err, logger)
				return nil, nil
			}
			return next(ctx, w, r, request)
		}
	}
}

// actions returns every operationId in the API spec, sorted. It reads
// api.OpenapiYAML, because the spec api.GetSwagger returns has the
// operationIds renamed to Go names.
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

// NewLocalBundle loads the bundle in dir for the API's actions. issuers are
// the names of the issuers authentication trusts. The API has no tenants, so
// the load and every reload refuse authorization.yaml's tenancy keys.
func NewLocalBundle(dir string, issuers []string) (*localbundle.LocalBundle, error) {
	ids, err := actions()
	if err != nil {
		return nil, err
	}
	lb, err := localbundle.New(localbundle.Options{
		Dir:           dir,
		Issuers:       issuers,
		Actions:       ids,
		ResourceTypes: []string{resourceType},
		RoleOnly:      true,
	})
	if err != nil {
		return nil, fmt.Errorf("load the authorization bundle in %s: %w", dir, err)
	}
	return lb, nil
}

// runReloads reloads lb every reloadInterval until ctx is done.
func runReloads(ctx context.Context, lb *localbundle.LocalBundle, logger *slog.Logger) {
	ticker := time.NewTicker(reloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		reload(lb, logger)
	}
}

// reload reloads lb once. A failed reload keeps the live bundle and counts
// nvke_authz_bundle_reload_failures_total.
func reload(lb *localbundle.LocalBundle, logger *slog.Logger) {
	changed, err := lb.Reload()
	switch {
	case err != nil:
		metrics.RecordAuthzBundleReloadFailure()
		logger.Error("Failed to reload the authorization bundle; keeping the live one", "error", err)
	case changed:
		logger.Info("Reloaded the authorization bundle")
	}
}
