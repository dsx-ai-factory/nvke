// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	auth "github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/authz"
)

// authzReloadService backs only the reload endpoint (ReloadAuthzBundle): it
// is the one operation in this API whose own authorization is not just the
// ordinary Cedar deny/allow story, and it needs direct references to both
// authn and authzProvider to check that mode explicitly
// (design-authz-hot-reload.md §2). Nothing else on serverImpl reads either
// field.
type authzReloadService struct {
	authn         *auth.Authenticator
	authzProvider *authz.Provider
}

// NewAuthzReloadService creates a new authorization-reload service.
func NewAuthzReloadService(authn *auth.Authenticator, authzProvider *authz.Provider) *authzReloadService {
	return &authzReloadService{
		authn:         authn,
		authzProvider: authzProvider,
	}
}

// ReloadAuthzBundle forces this replica to check its mounted authorization
// bundle for a change right now, instead of waiting for the next poll tick.
//
// This route's own authorization is two checks, not one, and by the time this
// method runs, only the first — the ordinary Cedar permit/deny decision —
// has already happened, in the authorization middleware sitting outside the
// mux. This method is the second: an explicit enforcing-mode gate that is not
// subject to ModePermissive's ordinary "log and let through" behavior, which
// is the right default for every other route and the wrong one for a route
// whose whole job is forcing a side effect. See design-authz-hot-reload.md
// §2 for why a second Cedar rule cannot express this: Cedar's decision is
// about *who*, and this gap is about *what mode the deployment is running
// in*.
func (s *authzReloadService) ReloadAuthzBundle(
	ctx context.Context, _ api.ReloadAuthzBundleRequestObject,
) (api.ReloadAuthzBundleResponseObject, error) {
	if s.authn == nil || s.authn.Mode() != auth.ModeEnforcing ||
		s.authzProvider == nil || s.authzProvider.Mode() != authz.ModeEnforcing {
		return api.ReloadAuthzBundle403JSONResponse{
			ForbiddenJSONResponse: api.ForbiddenJSONResponse{
				Message: "reload requires authentication and authorization to both be enforcing",
			},
		}, nil
	}

	outcome, ok := s.authzProvider.TriggerReload(ctx, authz.ReloadSourceEndpoint)
	if !ok {
		return api.ReloadAuthzBundle500JSONResponse{
			Message: "authorization reload is not enabled on this deployment (AUTHZ_RELOAD_INTERVAL<=0)",
		}, nil
	}
	if outcome.Err != nil {
		return api.ReloadAuthzBundle500JSONResponse{
			Message: "reload failed; the bundle already live before this call is unaffected",
		}, nil
	}

	return api.ReloadAuthzBundle200JSONResponse{
		Changed:   outcome.Changed,
		Throttled: outcome.Throttled,
		Source:    authz.ReloadSourceEndpoint,
	}, nil
}
