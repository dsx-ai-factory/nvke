// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

// How authorization reaches the handlers, tested through the real NewServer:
// lists are filtered to the caller's tenant, and a create records it.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"

	"github.com/dsx-ai-factory/nvke/controller/internal/apiauthz"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// authzBundleDir is apiauthz's test bundle, shared here: issuer idp reads the
// tenant from the tenant claim, callers in tenant ops are operators, and
// role=admin may do anything.
const authzBundleDir = "../apiauthz/testdata/bundle"

// authzMode is how a test server authorizes: off, or a Client in a mode.
type authzMode string

const (
	authzOff        authzMode = "off"
	authzEnforcing  authzMode = authzMode(authn.ModeEnforcing)
	authzPermissive authzMode = authzMode(authn.ModePermissive)
)

// newAuthzTestServer builds the production handler with authorization in
// mode. In place of authentication, it gives every request an admin principal from idp
// in tenant, or with no tenant claim if tenant is empty.
func newAuthzTestServer(t *testing.T, mockDB *testutil.MockDB, mode authzMode, tenant string) http.Handler {
	t.Helper()

	logger := slog.New(slog.DiscardHandler)
	var opts []server.Option
	if mode != authzOff {
		lb, err := apiauthz.NewLocalBundle(authzBundleDir, []string{"idp"})
		require.NoError(t, err)
		client, err := authz.NewClient(lb, authn.Mode(mode), logger, nil)
		require.NoError(t, err)
		opts = append(opts, server.WithAuthorization(apiauthz.NewMiddleware(client, mockDB, nil, logger).Strict))
	}
	srv, err := server.NewServer(config.Config{ProfilesDir: "./profiles"}, logger, mockDB, nil, nil, nil, opts...)
	require.NoError(t, err)

	claims := map[string]any{"role": "admin"}
	if tenant != "" {
		claims["tenant"] = tenant
	}
	principal := &authn.Principal{IssuerName: "idp", Subject: "user", Claims: claims}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Handler.ServeHTTP(w, r.WithContext(authn.ContextWithPrincipal(r.Context(), principal)))
	})
}

// listOperation is a list endpoint, the MockDB method it calls, and how to
// read the tenant filter from that method's filters argument.
type listOperation struct {
	path   string
	method string
	empty  any
	tenant func(filters any) *string
}

var listOperations = []listOperation{
	{"/api/clusters", "ListClusters", []db.Cluster{}, func(f any) *string { return f.(db.ClusterFilters).Tenant }},
	{"/api/nodePools", "ListNodePools", []db.NodePool{}, func(f any) *string { return f.(db.NodePoolFilters).Tenant }},
	{"/api/nodeSets", "ListNodeSets", []db.NodeSet{}, func(f any) *string { return f.(db.NodeSetFilters).Tenant }},
	{"/api/nodes", "ListNodes", []db.Node{}, func(f any) *string { return f.(db.NodeFilters).Tenant }},
}

// Each list is filtered to the caller's tenant. An operator, a permissive
// Client and disabled authorization list every tenant. A caller with no
// tenant gets 403 and reaches no query.
func TestListsAreFilteredToTheCallersTenant(t *testing.T) {
	t.Parallel()

	tenantA := "tenant-a"
	tests := []struct {
		name       string
		mode       authzMode
		tenant     string
		wantStatus int
		wantFilter *string
	}{
		{name: "same tenant", mode: authzEnforcing, tenant: tenantA, wantStatus: http.StatusOK, wantFilter: &tenantA},
		{name: "operator", mode: authzEnforcing, tenant: "ops", wantStatus: http.StatusOK},
		{name: "no tenant", mode: authzEnforcing, wantStatus: http.StatusForbidden},
		{name: "permissive, same tenant", mode: authzPermissive, tenant: tenantA, wantStatus: http.StatusOK},
		{name: "permissive, no tenant", mode: authzPermissive, wantStatus: http.StatusOK},
		{name: "authorization off", mode: authzOff, tenant: tenantA, wantStatus: http.StatusOK},
	}
	for _, op := range listOperations {
		for _, tt := range tests {
			t.Run(op.method+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				mockDB := &testutil.MockDB{}
				var filters any
				mockDB.On(op.method, mock.Anything, mock.Anything).
					Run(func(args mock.Arguments) { filters = args.Get(1) }).
					Return(op.empty, nil).Maybe()

				rr := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, op.path, nil)
				newAuthzTestServer(t, mockDB, tt.mode, tt.tenant).ServeHTTP(rr, req)

				require.Equal(t, tt.wantStatus, rr.Code, rr.Body.String())
				if tt.wantStatus != http.StatusOK {
					assert.Nil(t, filters, "a refused list reached the database")
					return
				}
				require.NotNil(t, filters)
				assert.Equal(t, tt.wantFilter, op.tenant(filters))
			})
		}
	}
}

// createCluster records the authorized caller's tenant as the owner. With
// authorization off, or a permissive refusal with no caller, the cluster has
// no owner.
func TestCreateClusterRecordsTheCallersTenant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mode       authzMode
		tenant     string
		wantStatus int
		wantOwner  string
	}{
		{name: "tenant", mode: authzEnforcing, tenant: "tenant-a", wantStatus: http.StatusCreated, wantOwner: "tenant-a"},
		{name: "operator", mode: authzEnforcing, tenant: "ops", wantStatus: http.StatusCreated, wantOwner: "ops"},
		{name: "no tenant", mode: authzEnforcing, wantStatus: http.StatusForbidden},
		{name: "permissive, tenant", mode: authzPermissive, tenant: "tenant-a", wantStatus: http.StatusCreated, wantOwner: "tenant-a"},
		{name: "permissive, no tenant", mode: authzPermissive, wantStatus: http.StatusCreated},
		{name: "authorization off", mode: authzOff, tenant: "tenant-a", wantStatus: http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockDB := &testutil.MockDB{MockTx: &testutil.MockTx{}}
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil).Maybe()
			var created *db.Cluster
			mockDB.MockTx.On("CreateCluster", mock.Anything, mock.Anything).
				Run(func(args mock.Arguments) { created = args.Get(1).(*db.Cluster) }).
				Return(db.Cluster{Name: "test-cluster", KubernetesVersion: "1.29.7"}, nil).Maybe()
			stubOpenAPICreateClusterTx(mockDB)

			body, err := json.Marshal(openAPIForgeCreateClusterRequest())
			require.NoError(t, err)
			rr := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/clusters", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			newAuthzTestServer(t, mockDB, tt.mode, tt.tenant).ServeHTTP(rr, req)

			require.Equal(t, tt.wantStatus, rr.Code, rr.Body.String())
			if tt.wantStatus != http.StatusCreated {
				assert.Nil(t, created, "a refused create reached the database")
				return
			}
			require.NotNil(t, created)
			assert.Equal(t, tt.wantOwner, created.Tenant)
		})
	}
}
