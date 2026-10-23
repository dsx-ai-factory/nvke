// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	auth "github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/authz"
)

// The reload endpoint's own authorization is two checks (design-authz-hot-
// reload.md §2): the ordinary Cedar decision, already covered by
// internal/authz's TestReloadRouteIsCedarAuthorized, and — this file's
// concern — an explicit enforcing-mode gate inside ReloadAuthzBundle itself
// that is not subject to ModePermissive's ordinary "log and let through"
// behavior. By the time serverImpl.ReloadAuthzBundle runs, the Cedar decision
// has already happened outside the mux, so these tests only exercise the
// mode gate, and specifically assert that a refusal never even calls
// TriggerReload — a non-2xx response alone would not distinguish the mode
// gate refusing from Cedar having denied further upstream.

func noErrKeySource() auth.KeySource { return stubReloadKeySource{} }

type stubReloadKeySource struct{}

func (stubReloadKeySource) KeyFor(context.Context, string) (crypto.PublicKey, error) {
	return nil, errors.New("no keys in this test")
}

func testReloadAuthConfig() *auth.Config {
	return &auth.Config{
		Audiences: []string{"https://cwe.test.invalid"},
		Issuers: []auth.Issuer{{
			Name:       "test-kubernetes",
			Issuer:     "https://kubernetes.default.svc.cluster.local",
			JWKS:       &auth.JWKSKeys{URI: "https://kubernetes.default.svc/openid/v1/jwks"},
			Algorithms: []string{"RS256"},
			Admit:      &auth.Admit{ServiceAccounts: []auth.ServiceAccountRule{{Namespace: "nke-clients"}}},
		}},
	}
}

func testAuthenticator(t *testing.T, mode auth.Mode) *auth.Authenticator {
	t.Helper()
	verifier, err := auth.NewVerifier(testReloadAuthConfig(), map[string]auth.KeySource{"test-kubernetes": noErrKeySource()})
	require.NoError(t, err)
	authn, err := auth.NewAuthenticator(verifier, mode, testDiscardLogger(), nil)
	require.NoError(t, err)
	return authn
}

func testDiscardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// reloadCallCounter counts internal/authz.ReloadRecorder calls, so a test can
// assert "TriggerReload was never invoked" precisely, not merely infer it
// from the HTTP status.
type reloadCallCounter struct {
	calls int
}

func (c *reloadCallCounter) record(authz.ReloadOutcomeLabel, string) { c.calls++ }

// testAuthzBundle copies deploy/authz's policies, actions and roles into a
// directory beside a roleMappings.json granting operator to one identity from
// the test issuer. deploy/authz holds no roleMappings.json, and a bundle with
// no mapping does not load.
func testAuthzBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"policies.cedar", "actions.json", "roles.json"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "authz", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), raw, 0o600))
	}
	mappings := `[{"issuer":"test-kubernetes",` +
		`"claim":{"name":"sub","value":"system:serviceaccount:nke-clients:nke-client"},` +
		`"grants":[{"type":"NKE::Role","id":"operator"}]}]`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "roleMappings.json"), []byte(mappings), 0o600))
	return dir
}

func testAuthzProvider(t *testing.T, permissive bool, counter *reloadCallCounter) *authz.Provider {
	t.Helper()
	provider, err := authz.Setup(t.Context(), authz.Options{
		BundleDir:      testAuthzBundle(t),
		IssuerNames:    []string{"test-kubernetes"},
		Permissive:     permissive,
		ReloadInterval: time.Hour, // enabled, but no tick fires during the test
		ReloadRecorder: counter.record,
	}, testDiscardLogger())
	require.NoError(t, err)
	require.NotNil(t, provider)
	return provider
}

func TestReloadAuthzBundleRefusesWithNoAuthenticator(t *testing.T) {
	counter := &reloadCallCounter{}
	s := &serverImpl{authzReloadService: authzReloadService{authn: nil, authzProvider: testAuthzProvider(t, false, counter)}}

	resp, err := s.ReloadAuthzBundle(t.Context(), api.ReloadAuthzBundleRequestObject{})
	require.NoError(t, err)
	_, is403 := resp.(api.ReloadAuthzBundle403JSONResponse)
	assert.True(t, is403, "a nil authn must refuse outright, not fall through to a nil-pointer panic or an allow")
	assert.Zero(t, counter.calls, "the mode gate must refuse before TriggerReload is ever called")
}

func TestReloadAuthzBundleRefusesWhenAuthenticationIsPermissive(t *testing.T) {
	counter := &reloadCallCounter{}
	s := &serverImpl{authzReloadService: authzReloadService{
		authn:         testAuthenticator(t, auth.ModePermissive),
		authzProvider: testAuthzProvider(t, false, counter),
	}}

	resp, err := s.ReloadAuthzBundle(t.Context(), api.ReloadAuthzBundleRequestObject{})
	require.NoError(t, err)
	_, is403 := resp.(api.ReloadAuthzBundle403JSONResponse)
	assert.True(t, is403, "permissive authentication must refuse the reload route outright")
	assert.Zero(t, counter.calls)
}

func TestReloadAuthzBundleRefusesWhenAuthorizationIsPermissive(t *testing.T) {
	counter := &reloadCallCounter{}
	s := &serverImpl{authzReloadService: authzReloadService{
		authn:         testAuthenticator(t, auth.ModeEnforcing),
		authzProvider: testAuthzProvider(t, true, counter),
	}}

	resp, err := s.ReloadAuthzBundle(t.Context(), api.ReloadAuthzBundleRequestObject{})
	require.NoError(t, err)
	_, is403 := resp.(api.ReloadAuthzBundle403JSONResponse)
	assert.True(t, is403, "permissive authorization must refuse the reload route outright, "+
		"even though ModePermissive would ordinarily let a Cedar denial through")
	assert.Zero(t, counter.calls)
}

func TestReloadAuthzBundleSucceedsWhenBothAreEnforcing(t *testing.T) {
	counter := &reloadCallCounter{}
	s := &serverImpl{authzReloadService: authzReloadService{
		authn:         testAuthenticator(t, auth.ModeEnforcing),
		authzProvider: testAuthzProvider(t, false, counter),
	}}

	resp, err := s.ReloadAuthzBundle(t.Context(), api.ReloadAuthzBundleRequestObject{})
	require.NoError(t, err)
	ok, is200 := resp.(api.ReloadAuthzBundle200JSONResponse)
	require.True(t, is200, "both enforcing must reach TriggerReload and report its outcome")
	assert.Equal(t, "endpoint", ok.Source)
	assert.Equal(t, 1, counter.calls, "exactly one reload attempt must have been made")
}
