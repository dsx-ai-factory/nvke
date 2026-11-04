// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package apiauthz_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/apiauthz"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// bundleDir holds a bundle whose issuer idp reads the tenant from the tenant
// claim, makes callers in tenant ops operators, and lets role=admin do
// anything.
const bundleDir = "testdata/bundle"

const (
	tenantA  = "tenant-a"
	tenantB  = "tenant-b"
	operator = "ops"
)

// The tenant lookups, by method name.
const (
	clusterLookup  = "GetClusterTenant"
	nodePoolLookup = "GetNodePoolTenant"
	nodeSetLookup  = "GetNodeSetTenant"
	nodeLookup     = "GetNodeTenant"
)

// The objects a call acts on.
type object string

const (
	ownedByA object = "owned by tenant-a"
	unowned  object = "unowned"
	missing  object = "missing"
)

// objectID returns the ID of obj under lookup. Each lookup has its own IDs,
// so an ID resolves only under its own lookup.
func objectID(lookup string, obj object) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(lookup+"/"+string(obj)))
}

// lookupErr fails every lookup.
var lookupErr = uuid.MustParse("00000000-0000-4000-8000-00000000000d")

// lookupCall is one call to a stubTenants lookup.
type lookupCall struct {
	method string
	id     uuid.UUID
}

// stubTenants knows, under each lookup, one object owned by tenant-a and one
// unowned object. It records every call.
type stubTenants struct {
	mu    sync.Mutex
	calls []lookupCall
}

func (s *stubTenants) lookup(method string, id uuid.UUID) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, lookupCall{method: method, id: id})
	s.mu.Unlock()
	switch id {
	case objectID(method, ownedByA):
		return tenantA, nil
	case objectID(method, unowned):
		return "", nil
	case lookupErr:
		return "", errors.New("database is down")
	default:
		return "", db.ErrNotFound
	}
}

func (s *stubTenants) recorded() []lookupCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]lookupCall(nil), s.calls...)
}

func (s *stubTenants) GetClusterTenant(_ context.Context, id uuid.UUID) (string, error) {
	return s.lookup(clusterLookup, id)
}

func (s *stubTenants) GetNodePoolTenant(_ context.Context, id uuid.UUID) (string, error) {
	return s.lookup(nodePoolLookup, id)
}

func (s *stubTenants) GetNodeSetTenant(_ context.Context, id uuid.UUID) (string, error) {
	return s.lookup(nodeSetLookup, id)
}

func (s *stubTenants) GetNodeTenant(_ context.Context, id uuid.UUID) (string, error) {
	return s.lookup(nodeLookup, id)
}

// decision is one decision a Client or Middleware counts.
type decision struct {
	outcome authz.Outcome
	action  string
}

// decisions records every decision counted.
type decisions struct {
	mu   sync.Mutex
	seen []decision
}

func (d *decisions) record(outcome authz.Outcome, action string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, decision{outcome: outcome, action: action})
}

func (d *decisions) all() []decision {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]decision(nil), d.seen...)
}

// newMiddleware loads the test bundle into a Client in mode, and returns a
// Middleware that asks it and looks up owners in tenants.
func newMiddleware(t *testing.T, mode authn.Mode, tenants db.TenantQueries, record authz.Recorder) *apiauthz.Middleware {
	t.Helper()

	lb, err := apiauthz.NewLocalBundle(bundleDir, []string{"idp"})
	require.NoError(t, err)
	logger := slog.New(slog.DiscardHandler)
	client, err := authz.NewClient(lb, mode, logger, record)
	require.NoError(t, err)
	return apiauthz.NewMiddleware(client, tenants, record, logger)
}

// caller returns a context carrying an admin principal from idp in tenant, or
// with no tenant claim if tenant is empty.
func caller(tenant string) context.Context {
	claims := map[string]any{"role": "admin"}
	if tenant != "" {
		claims["tenant"] = tenant
	}
	return authn.ContextWithPrincipal(context.Background(), &authn.Principal{
		IssuerName: "idp",
		Subject:    "user-in-" + tenant,
		Claims:     claims,
	})
}

// serve runs request for the operation with Go name operationID through m,
// and returns the response status.
func serve(t *testing.T, m *apiauthz.Middleware, ctx context.Context, operationID string, request any) int {
	t.Helper()

	called := false
	next := func(context.Context, http.ResponseWriter, *http.Request, any) (any, error) {
		called = true
		return "passed", nil
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/test", nil)
	response, err := m.Strict(next, operationID)(ctx, w, r, request)
	require.NoError(t, err)
	if called {
		require.Equal(t, "passed", response)
		require.Equal(t, http.StatusOK, w.Code, "the middleware wrote a response and then called next")
		return http.StatusOK
	}
	require.Nil(t, response)
	return w.Code
}

// operation is one operation in the spec, with its strict server method.
type operation struct {
	id     string
	path   string
	method reflect.Method
}

// operations returns every operation in the spec.
func operations(t *testing.T) []operation {
	t.Helper()

	spec, err := openapi3.NewLoader().LoadFromData([]byte(api.OpenapiYAML))
	require.NoError(t, err)
	server := reflect.TypeFor[api.StrictServerInterface]()
	var ops []operation
	for path, item := range spec.Paths.Map() {
		for _, op := range item.Operations() {
			method, ok := server.MethodByName(strings.ToUpper(op.OperationID[:1]) + op.OperationID[1:])
			require.True(t, ok, "the strict server has no method for %s", op.OperationID)
			ops = append(ops, operation{id: op.OperationID, path: path, method: method})
		}
	}
	require.NotEmpty(t, ops)
	return ops
}

// request returns op's request, acting on the object id. createNodePool acts
// on the cluster its body names.
func (op operation) request(id uuid.UUID) any {
	if op.id == "createNodePool" {
		return api.CreateNodePoolRequestObject{Body: &api.CreateNodePoolRequest{ClusterID: id}}
	}
	request := reflect.New(op.method.Type.In(1)).Elem()
	for i := range request.NumField() {
		if f := request.Field(i); f.Type() == reflect.TypeFor[uuid.UUID]() {
			f.Set(reflect.ValueOf(id))
		}
	}
	return request.Interface()
}

// check says how an operation is checked: its scope, and the lookup that
// finds its object's owner, if it acts on one object.
type check struct {
	scope  authz.Scope
	lookup string
}

// checks says how each operation is checked. TestEveryOperationIsChecked fails
// for an operation missing here.
var checks = map[string]check{
	"listClusters":                        {scope: authz.ScopeList},
	"createCluster":                       {scope: authz.ScopeCreate},
	"getCluster":                          {authz.ScopeObject, clusterLookup},
	"updateCluster":                       {authz.ScopeObject, clusterLookup},
	"deleteCluster":                       {authz.ScopeObject, clusterLookup},
	"getClusterControlPlane":              {authz.ScopeObject, clusterLookup},
	"getClusterControlPlaneConfigHistory": {authz.ScopeObject, clusterLookup},
	"getClusterSpireServer":               {authz.ScopeObject, clusterLookup},
	"updateClusterSpireServer":            {authz.ScopeObject, clusterLookup},
	"listNodePools":                       {scope: authz.ScopeList},
	"createNodePool":                      {authz.ScopeObject, clusterLookup},
	"getNodePool":                         {authz.ScopeObject, nodePoolLookup},
	"updateNodePool":                      {authz.ScopeObject, nodePoolLookup},
	"deleteNodePool":                      {authz.ScopeObject, nodePoolLookup},
	"listNodeSets":                        {scope: authz.ScopeList},
	"getNodeSet":                          {authz.ScopeObject, nodeSetLookup},
	"listNodes":                           {scope: authz.ScopeList},
	"getNode":                             {authz.ScopeObject, nodeLookup},
	"deleteNode":                          {authz.ScopeObject, nodeLookup},
	"listReleases":                        {scope: authz.ScopeGlobal},
	"listConfigProfiles":                  {scope: authz.ScopeGlobal},
}

// Each operation, for each kind of caller: the enforcing status, the lookup
// it makes, and the action it asks about. A permissive Client lets every
// request through.
func TestEveryOperationIsChecked(t *testing.T) {
	t.Parallel()

	type call struct {
		name   string
		tenant string
		object object
	}
	calls := []call{
		{name: "same tenant", tenant: tenantA, object: ownedByA},
		{name: "other tenant", tenant: tenantB, object: ownedByA},
		{name: "operator", tenant: operator, object: ownedByA},
		{name: "no tenant", object: ownedByA},
		{name: "tenant on an unowned object", tenant: tenantA, object: unowned},
		{name: "tenant on a missing object", tenant: tenantA, object: missing},
	}
	// want is the enforcing status of c on an operation of kind scope.
	want := func(scope authz.Scope, c call) int {
		switch {
		case scope == authz.ScopeGlobal:
			return http.StatusOK
		case c.tenant == "":
			return http.StatusForbidden
		case scope != authz.ScopeObject:
			return http.StatusOK
		case c.object == missing:
			return http.StatusNotFound
		case c.tenant == operator:
			return http.StatusOK
		case c.object == unowned || c.tenant != tenantA:
			return http.StatusNotFound
		default:
			return http.StatusOK
		}
	}

	for _, op := range operations(t) {
		check, ok := checks[op.id]
		require.True(t, ok, "checks doesn't say how %s is checked", op.id)
		for _, c := range calls {
			t.Run(op.id+"/"+c.name, func(t *testing.T) {
				t.Parallel()

				id := objectID(check.lookup, c.object)
				request := op.request(id)
				tenants := &stubTenants{}
				recorded := &decisions{}
				enforcing := newMiddleware(t, authn.ModeEnforcing, tenants, recorded.record)
				require.Equal(t, want(check.scope, c), serve(t, enforcing, caller(c.tenant), op.method.Name, request))

				var wantLookups []lookupCall
				if check.lookup != "" {
					wantLookups = []lookupCall{{method: check.lookup, id: id}}
				}
				assert.Equal(t, wantLookups, tenants.recorded(), "%s looked up the wrong owner", op.id)
				if c.tenant == operator && !authn.IsAnonymousRequestPath("/api"+op.path, "/api"+op.path) {
					assert.Equal(t, []decision{{authz.OutcomeAllow, op.id}}, recorded.all(), "%s asked about the wrong action", op.id)
				}

				permissive := newMiddleware(t, authn.ModePermissive, &stubTenants{}, nil)
				require.Equal(t, http.StatusOK, serve(t, permissive, caller(c.tenant), op.method.Name, request), "permissive mode refused a request")
			})
		}
	}
}

// A failed tenant lookup is neither an allow nor a refusal: it gets 500,
// counts as an error, and never reaches the handler.
func TestFailedLookupAnswers500(t *testing.T) {
	t.Parallel()

	for _, mode := range []authn.Mode{authn.ModeEnforcing, authn.ModePermissive} {
		recorded := &decisions{}
		m := newMiddleware(t, mode, &stubTenants{}, recorded.record)
		status := serve(t, m, caller(tenantA), "GetCluster", api.GetClusterRequestObject{ClusterID: lookupErr})
		assert.Equal(t, http.StatusInternalServerError, status, "mode %s", mode)
		assert.Equal(t, []decision{{authz.OutcomeError, "getCluster"}}, recorded.all(), "mode %s", mode)
	}
}
