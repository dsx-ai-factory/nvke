// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

// testIssuer is the issuer every test identity comes from. testIssuers is
// what NewLocal needs to accept a mapping that names it.
const testIssuer = "kube-apiserver"

var testIssuers = []string{testIssuer}

// Test identities. testRoleMappings grants each of the first four one shipped
// role; the stranger holds none.
const (
	subjectReader       = "system:serviceaccount:nke-system:nke-cli"
	subjectOperator     = "spiffe://nvidia.com/ns/nke-system/sa/nke-controller"
	subjectNodeOperator = "spiffe://nvidia.com/ns/nke-system/sa/node-lifecycle"
	subjectAdmin        = "system:serviceaccount:nke-clients:nke-client"
	subjectStranger     = "spiffe://nvidia.com/ns/default/sa/nobody"
)

// testRoleMappings grants each shipped role to one test identity from
// testIssuer, matching its sub claim.
const testRoleMappings = `[
 {"issuer":"` + testIssuer + `","claim":{"name":"sub","value":"` + subjectReader + `"},
  "grants":[{"type":"NKE::Role","id":"reader"}]},
 {"issuer":"` + testIssuer + `","claim":{"name":"sub","value":"` + subjectOperator + `"},
  "grants":[{"type":"NKE::Role","id":"operator"}]},
 {"issuer":"` + testIssuer + `","claim":{"name":"sub","value":"` + subjectNodeOperator + `"},
  "grants":[{"type":"NKE::Role","id":"nodeOperator"}]},
 {"issuer":"` + testIssuer + `","claim":{"name":"sub","value":"` + subjectAdmin + `"},
  "grants":[{"type":"NKE::Role","id":"admin"}]}
]`

// subClaim is the verified claim set of a token whose sub is subject, which is
// what testRoleMappings matches.
func subClaim(subject string) map[string]any { return map[string]any{"sub": subject} }

const (
	someClusterID  = "11111111-1111-1111-1111-111111111111"
	someNodeID     = "22222222-2222-2222-2222-222222222222"
	someNodePoolID = "33333333-3333-3333-3333-333333333333"
)

// bundleCase is one row of the matrix. It is what deploy/authz's policies and
// roles allow each test identity under testRoleMappings, stated once and
// asserted against whichever engine is under test — which is what makes it the
// conformance test between them (AZ-4).
type bundleCase struct {
	name    string
	subject string
	method  string
	path    string
	want    int
}

func bundleMatrix() []bundleCase {
	return []bundleCase{
		// The reader role covers every GET through the read action group,
		// including endpoints no policy names.
		{"reader lists clusters", subjectReader, http.MethodGet, "/api/clusters", http.StatusOK},
		{"reader gets a cluster", subjectReader, http.MethodGet, "/api/clusters/" + someClusterID, http.StatusOK},
		{"reader reads a cluster's control plane", subjectReader, http.MethodGet, "/api/clusters/" + someClusterID + "/control-plane", http.StatusOK},
		{"reader lists releases", subjectReader, http.MethodGet, "/api/releases", http.StatusOK},

		// ...and no write.
		{"reader cannot update a cluster", subjectReader, http.MethodPatch, "/api/clusters/" + someClusterID, http.StatusForbidden},
		{"reader cannot create a cluster", subjectReader, http.MethodPost, "/api/clusters", http.StatusForbidden},

		// The operator role nests reader, so it reads as well as writes.
		{"operator updates a cluster", subjectOperator, http.MethodPatch, "/api/clusters/" + someClusterID, http.StatusOK},
		{"operator creates a node pool", subjectOperator, http.MethodPost, "/api/nodePools", http.StatusOK},
		{"operator reads, via the nested reader role", subjectOperator, http.MethodGet, "/api/clusters", http.StatusOK},

		// Node lifecycle is a DELETE or a PATCH and therefore in the write
		// group; a forbid is what holds it to one role.
		{"operator cannot delete a node", subjectOperator, http.MethodDelete, "/api/nodes/" + someNodeID, http.StatusForbidden},
		{"node operator deletes a node", subjectNodeOperator, http.MethodDelete, "/api/nodes/" + someNodeID, http.StatusOK},
		{"node operator cannot write otherwise", subjectNodeOperator, http.MethodPatch, "/api/clusters/" + someClusterID, http.StatusForbidden},

		// Deleting a pool tears down every node in it, and changing one rolls
		// them. Both used to be an ordinary operator write.
		{"operator cannot delete a node pool", subjectOperator, http.MethodDelete, "/api/nodePools/" + someNodePoolID, http.StatusForbidden},
		{"node operator deletes a node pool", subjectNodeOperator, http.MethodDelete, "/api/nodePools/" + someNodePoolID, http.StatusOK},
		{"operator cannot update a node pool", subjectOperator, http.MethodPatch, "/api/nodePools/" + someNodePoolID, http.StatusForbidden},
		{"node operator updates a node pool", subjectNodeOperator, http.MethodPatch, "/api/nodePools/" + someNodePoolID, http.StatusOK},

		// ...and creating one is still an ordinary write, because it is
		// additive; see writesLeftToTheGroup.
		{"operator creates a node pool, still", subjectOperator, http.MethodPost, "/api/nodePools", http.StatusOK},

		// Destructive, and named explicitly rather than left to the coarse
		// write grant — see operators-may-delete-clusters. nodeOperator does
		// not nest operator, so it still can't.
		{"operator deletes a cluster", subjectOperator, http.MethodDelete, "/api/clusters/" + someClusterID, http.StatusOK},
		{"node operator cannot delete a cluster", subjectNodeOperator, http.MethodDelete, "/api/clusters/" + someClusterID, http.StatusForbidden},

		// admin is parented to both operator and nodeOperator, so it must be
		// able to do an operator-only write AND a nodeOperator-only write in
		// the same run — the one thing neither role alone can do, and the
		// only behavior that actually distinguishes admin from "holds operator".
		{"admin updates a cluster (operator's write)", subjectAdmin, http.MethodPatch, "/api/clusters/" + someClusterID, http.StatusOK},
		{"admin deletes a node (nodeOperator's write)", subjectAdmin, http.MethodDelete, "/api/nodes/" + someNodeID, http.StatusOK},

		// A subject the bundle does not mention holds no role.
		{"a stranger reads nothing", subjectStranger, http.MethodGet, "/api/clusters", http.StatusForbidden},

		// Not a route, so never an operation, so refused before the engine.
		{"an unrouted path", subjectOperator, http.MethodGet, "/api/invented", http.StatusForbidden},
	}
}

// runBundleMatrix drives the real middleware over authorizer and asserts the
// matrix. Shared so both engines are held to the identical statements.
func runBundleMatrix(t *testing.T, authorizer Authorizer) {
	t.Helper()

	mux := http.NewServeMux()
	api.HandlerWithOptions(nil, api.StdHTTPServerOptions{BaseRouter: mux, BaseURL: api.BasePath})

	resolver, err := NewResolver(mux)
	require.NoError(t, err)

	middleware, err := NewMiddleware(authorizer, resolver, ModeEnforcing,
		slog.New(slog.DiscardHandler), nil)
	require.NoError(t, err)

	var reached bool
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	for _, tt := range bundleMatrix() {
		t.Run(tt.name, func(t *testing.T) {
			reached = false

			r := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, http.NoBody)
			r = r.WithContext(authn.ContextWithPrincipal(r.Context(), &authn.Principal{
				IssuerName: testIssuer,
				Issuer:     "https://kubernetes.default.svc.cluster.local",
				Subject:    tt.subject,
				Claims:     subClaim(tt.subject),
			}))

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)

			assert.Equal(t, tt.want, rec.Code)
			assert.Equal(t, tt.want == http.StatusOK, reached,
				"whether the handler ran must agree with the status")
		})
	}
}

// bundleDir locates deploy/authz from this package.
//
// A failure, not a skip. It is the first call in every test that asserts
// anything about the shipped bundle — the whole conformance matrix, the engine
// contract, the schema type-check, the vocabulary checks — and it used to
// t.Skipf, which meant a run that could not find the bundle printed nineteen
// SKIP lines and then PASS. Measured: run the test binary from a directory where
// ../../deploy/authz does not resolve and the package reports success while
// asserting none of it. A test suite that reports green when it has checked
// nothing is worse than one that is absent, because it is counted.
//
// It takes a testing.TB so the benchmarks share it rather than carrying a second
// copy of the path and a second reason to skip.
func bundleDir(tb testing.TB) string {
	tb.Helper()
	dir := filepath.Join("..", "..", "deploy", "authz")
	_, err := os.Stat(filepath.Join(dir, policiesFile))
	require.NoErrorf(tb, err, "the bundle sources must be readable at %s for this package to assert anything", dir)
	return dir
}

// shippedBundleWithMappings writes deploy/authz's policies, actions and roles
// with mappings as roleMappings.json, and returns the directory. It is how
// every test loads the shipped bundle: the shipped roleMappings.json is empty,
// and a bundle with no mapping does not load. Pass testRoleMappings for the
// test identities, and testIssuers to NewLocal.
func shippedBundleWithMappings(tb testing.TB, mappings string) string {
	tb.Helper()
	return writeBundleWithMappings(tb,
		shippedFile(tb, policiesFile), shippedFile(tb, actionsFile), shippedFile(tb, rolesFile), mappings)
}

// The engine against the bundle sources: no assembly step and no network, so
// what this asserts is what a deployment does with the files in deploy/authz.
func TestLocalMatchesTheBundle(t *testing.T) {
	local, err := NewLocal(shippedBundleWithMappings(t, testRoleMappings), testIssuers)
	require.NoError(t, err)
	require.NoError(t, local.Ready(context.Background()))

	runBundleMatrix(t, local)
}

// NewLocal is the whole of the readiness story for the in-process engine: a
// bundle that did not load is an ordinary error on CWE's own startup path,
// rather than a state to be interrogated out of another process afterwards.
func TestLocalRefusesABundleThatDidNotLoad(t *testing.T) {
	// Each case starts from the shipped bundle with valid mappings and breaks
	// one file, so the load fails at the part the case names.
	tests := []struct {
		name    string
		edit    func(files map[string]string)
		wantErr string
	}{
		{"no policy file", func(f map[string]string) {
			delete(f, policiesFile)
		}, "reading " + policiesFile},
		{"a policy that does not parse", func(f map[string]string) {
			f[policiesFile] = `permit(principal, action`
		}, "parsing " + policiesFile},
		{"no policies in the file", func(f map[string]string) {
			f[policiesFile] = `// only a comment`
		}, policiesFile + " declares no policies"},
		// No mapping can grant a role nobody declares, so this case carries an
		// empty mappings file and reaches the entity check.
		{"no entities", func(f map[string]string) {
			f[actionsFile], f[rolesFile], f[roleMappingsFile] = `[]`, `[]`, `[]`
		}, rolesFile + " declares no " + RoleType},
		{"an entity file that does not parse", func(f map[string]string) {
			f[actionsFile] = `{`
		}, "parsing " + actionsFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{
				policiesFile:     shippedFile(t, policiesFile),
				actionsFile:      shippedFile(t, actionsFile),
				rolesFile:        shippedFile(t, rolesFile),
				roleMappingsFile: testRoleMappings,
			}
			tt.edit(files)
			dir := t.TempDir()
			for name, content := range files {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
			}
			_, err := NewLocal(dir, testIssuers)
			require.Error(t, err, "a bundle that did not load must not produce a working authorizer")
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// A hostile identifier is refused, not evaluated — and refused identically
// whichever engine is deployed.
//
// cedar-go could treat a quote as data and deny it on the merits, and used to.
// That made the refusal an engine's opinion rather than a property of the
// request: the same identifier was a bad question to one engine and an ordinary
// denial to another. Query.Validate decides above the interface now, so what is
// refused does not depend on who would have evaluated it.
func TestAHostileIdentifierIsRefusedNotEvaluated(t *testing.T) {
	local, err := NewLocal(shippedBundleWithMappings(t, testRoleMappings), testIssuers)
	require.NoError(t, err)

	q := Query{
		Principal:    `evil" || true || "`,
		Issuer:       testIssuer,
		Action:       "getCluster",
		ResourceType: "NKE::Cluster",
		ResourceID:   someClusterID,
		Method:       http.MethodGet,
	}

	require.ErrorIs(t, q.Validate(), ErrInvalidQuery,
		"the middleware must refuse this before any engine sees it")

	decision, err := local.Authorize(context.Background(), q)
	require.ErrorIs(t, err, ErrInvalidQuery,
		"an engine called directly must refuse it too, not evaluate it")
	assert.False(t, decision.Allow)
}

func TestLocalRefusesAnIncompleteQuery(t *testing.T) {
	local, err := NewLocal(shippedBundleWithMappings(t, testRoleMappings), testIssuers)
	require.NoError(t, err)

	for _, q := range []Query{
		{Action: "getCluster", ResourceType: "NKE::Cluster", ResourceID: "x"},
		{Principal: subjectReader, ResourceType: "NKE::Cluster", ResourceID: "x"},
		{Principal: subjectReader, Action: "getCluster", ResourceID: "x"},
		{Principal: subjectReader, Action: "getCluster", ResourceType: "NKE::Cluster"},
	} {
		_, err := local.Authorize(context.Background(), q)
		require.Error(t, err)
	}
}

// Concurrency: the stored entities are shared, and the per-request resource is
// an overlay rather than a write into them. Run with -race.
func TestLocalIsSafeUnderConcurrency(t *testing.T) {
	local, err := NewLocal(shippedBundleWithMappings(t, testRoleMappings), testIssuers)
	require.NoError(t, err)

	done := make(chan struct{})
	for i := range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := range 50 {
				_, err := local.Authorize(context.Background(), Query{
					Principal:    subjectReader,
					Issuer:       testIssuer,
					Claims:       subClaim(subjectReader),
					Action:       "getCluster",
					ResourceType: "NKE::Cluster",
					ResourceID:   string(rune('a'+i)) + string(rune('a'+j%26)),
					Method:       http.MethodGet,
				})
				assert.NoError(t, err)
			}
		}()
	}
	for range 8 {
		<-done
	}
}
