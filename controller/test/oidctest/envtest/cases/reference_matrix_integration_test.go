//go:build oidc_integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cases

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	testoidc "github.com/dsx-ai-factory/nvke/controller/api/testutil/oidc"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig"
	oidcenvtest "github.com/dsx-ai-factory/nvke/controller/test/oidctest/envtest"
	"github.com/dsx-ai-factory/nvke/controller/test/oidctest/probe"
)

const (
	testAudience     = "oidc-reference-api"
	managementDomain = "platform.example.test"
	managementPrefix = "spiffe://" + managementDomain + "/"
	reservedSubject  = "spiffe://exchange.example.test/nke-service/cwe"
	externalUsername = "external-operator"
	referenceTenant  = "reference"
)

func TestReferenceOIDCAuthenticationMatrix(t *testing.T) {
	external := startIssuer(t)
	shared := startIssuer(t)
	prefixes := append(controllerconfig.ReservedIdentityPrefixes(), managementPrefix)
	externalProvider := controllerconfig.OIDCProvider{
		Name: "external", IssuerURL: external.IssuerURL(), Audiences: []string{external.Audience()}, CABundle: string(external.CAPEM()),
		UsernameClaim: "username", GroupsClaim: "groups", RequiredClaims: map[string]string{"tenant": referenceTenant},
	}
	splitIssuers := make([]*testoidc.Server, 0, len(prefixes))
	splitProviders := make([]controllerconfig.OIDCProvider, 0, len(prefixes))
	for _, prefix := range prefixes {
		server := startIssuer(t)
		mappingPrefix, _ := splitPrefix(prefix)
		splitIssuers = append(splitIssuers, server)
		splitProviders = append(splitProviders, controllerconfig.OIDCProvider{
			Name: prefixName(prefix), IssuerURL: server.IssuerURL(), Audiences: []string{server.Audience()}, CABundle: string(server.CAPEM()),
			UsernameClaim: "username", UsernamePrefix: mappingPrefix, GroupsClaim: "groups", GroupsPrefix: mappingPrefix,
			RequiredClaims: map[string]string{"tenant": referenceTenant},
		})
	}
	policy := kubeadmconfig.OperatorJWTPolicy{
		ManagementJWTTrustDomain: managementDomain, ReservedSubjects: []string{reservedSubject},
	}

	runReferenceCase(t, "shared-disabled", func(t *testing.T) {
		runner := startRunner(t, controllerconfig.ManagementOIDCAuthConfig{}, nil, []controllerconfig.OIDCProvider{externalProvider}, policy)
		fixture := prepareFixture(t, runner)
		runReferenceCase(t, "valid-external-rbac", func(t *testing.T) {
			assertExternalRBAC(t, runner, fixture, signExternal(t, external, nil))
		})
		runReferenceCase(t, "shared-rejected", func(t *testing.T) {
			token := sign(t, shared, testoidc.TokenSpec{Claims: map[string]any{"sub": managementPrefix + "controller"}})
			assertRejected(t, runner, fixture, token)
		})
	})

	runReferenceCase(t, "shared-enabled", func(t *testing.T) {
		management := controllerconfig.ManagementOIDCAuthConfig{Enabled: true, IssuerURL: shared.IssuerURL()}
		providers := append([]controllerconfig.OIDCProvider{externalProvider}, splitProviders...)
		runner := startRunner(t, management, shared.CAPEM(), providers, policy)
		fixture := prepareFixture(t, runner)

		runReferenceCase(t, "valid-external-rbac", func(t *testing.T) {
			assertExternalRBAC(t, runner, fixture, signExternal(t, external, nil))
		})
		runReferenceCase(t, "valid-shared", func(t *testing.T) {
			username := managementPrefix + "controller"
			result := run(t, runner, fixture, sign(t, shared, testoidc.TokenSpec{Claims: map[string]any{"sub": username}}))
			require.Equal(t, http.StatusOK, result.HTTPStatus, runner.Diagnostics())
			require.Equal(t, username, result.Username)
			require.ElementsMatch(t, []string{"system:authenticated"}, result.Groups)
			require.Equal(t, http.StatusForbidden, result.Allowed.HTTPStatus)
		})

		for index, prefix := range prefixes {
			name := "protected-prefix-" + prefixName(prefix)
			runReferenceCase(t, name, func(t *testing.T) {
				assertExternalRBAC(t, runner, fixture, signExternal(t, external, nil))
				assertRejected(t, runner, fixture, signExternal(t, external, map[string]any{"username": prefix + "blocked"}))
				assertRejected(t, runner, fixture, signExternal(t, external, map[string]any{"groups": []string{"operators", prefix + "blocked"}}))
				assertSplitMapping(t, runner, fixture, splitIssuers[index], prefix)
			})
		}
		runReferenceCase(t, "exact-reserved-subject", func(t *testing.T) {
			assertExternalRBAC(t, runner, fixture, signExternal(t, external, nil))
			assertRejected(t, runner, fixture, signExternal(t, external, map[string]any{"username": reservedSubject}))
			assertRejected(t, runner, fixture, signExternal(t, external, map[string]any{"groups": []string{"operators", reservedSubject}}))
		})
		runReferenceCase(t, "missing-required-claim", func(t *testing.T) {
			assertExternalRBAC(t, runner, fixture, signExternal(t, external, nil))
			claims := externalClaims()
			delete(claims, "tenant")
			assertRejected(t, runner, fixture, sign(t, external, testoidc.TokenSpec{Claims: claims}))
		})
		runReferenceCase(t, "wrong-audience", func(t *testing.T) {
			assertExternalRBAC(t, runner, fixture, signExternal(t, external, nil))
			assertRejected(t, runner, fixture, sign(t, external, testoidc.TokenSpec{Audience: []string{"wrong-audience"}, Claims: externalClaims()}))
		})
	})
}

func runReferenceCase(t *testing.T, name string, run func(*testing.T)) {
	t.Helper()
	executed := false
	t.Run(name, func(child *testing.T) {
		executed = true
		defer func() {
			if child.Skipped() {
				t.Errorf("reference case %s was skipped", child.Name())
			}
		}()
		run(child)
	})
	require.True(t, executed, "reference case %s/%s was not selected", t.Name(), name)
}

func assertSplitMapping(t *testing.T, runner *oidcenvtest.Runner, fixture probe.AdminFixture, server *testoidc.Server, prefix string) {
	t.Helper()
	mappingPrefix, claimPrefix := splitPrefix(prefix)
	control := map[string]any{"username": "control", "groups": []string{"operators"}, "tenant": referenceTenant}
	result := run(t, runner, fixture, sign(t, server, testoidc.TokenSpec{Claims: control}))
	require.Equal(t, http.StatusOK, result.HTTPStatus, runner.Diagnostics())
	require.Equal(t, mappingPrefix+"control", result.Username)
	require.ElementsMatch(t, []string{mappingPrefix + "operators", "system:authenticated"}, result.Groups)

	badUsername := map[string]any{"username": claimPrefix + "blocked", "groups": []string{"operators"}, "tenant": referenceTenant}
	assertRejected(t, runner, fixture, sign(t, server, testoidc.TokenSpec{Claims: badUsername}))
	badGroup := map[string]any{"username": "control", "groups": []string{claimPrefix + "blocked"}, "tenant": referenceTenant}
	assertRejected(t, runner, fixture, sign(t, server, testoidc.TokenSpec{Claims: badGroup}))
}

func startIssuer(t *testing.T) *testoidc.Server {
	t.Helper()
	server, err := testoidc.Start(testoidc.Config{Audience: testAudience})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	return server
}

func startRunner(
	t *testing.T,
	management controllerconfig.ManagementOIDCAuthConfig,
	sharedCA []byte,
	providers []controllerconfig.OIDCProvider,
	policy kubeadmconfig.OperatorJWTPolicy,
) *oidcenvtest.Runner {
	t.Helper()
	kubernetesVersion := os.Getenv("ENVTEST_K8S_VERSION")
	require.NotEmpty(t, kubernetesVersion, "ENVTEST_K8S_VERSION is required; run make test-oidc-reference")
	authentication, err := kubeadmconfig.BuildCombinedAuthenticationConfig(testAudience, management, sharedCA, providers, policy)
	require.NoError(t, err)
	file, err := kubeadmconfig.GenerateAPIServerAuthenticationConfig(authentication, "v"+kubernetesVersion)
	require.NoError(t, err)
	require.Contains(t, string(file.Content), "apiVersion: apiserver.config.k8s.io/")
	runner, err := oidcenvtest.Start(t, oidcenvtest.Config{AuthenticationConfig: file.Content})
	require.NoError(t, err, "KUBEBUILDER_ASSETS must contain Kubernetes %s binaries", kubernetesVersion)
	return runner
}

func prepareFixture(t *testing.T, runner *oidcenvtest.Runner) probe.AdminFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, err := probe.PrepareAdminFixture(ctx, runner.AdminClient(), "oidc-reference", externalUsername)
	require.NoError(t, err)
	return fixture
}

func signExternal(t *testing.T, server *testoidc.Server, changes map[string]any) string {
	t.Helper()
	claims := externalClaims()
	for name, value := range changes {
		claims[name] = value
	}
	return sign(t, server, testoidc.TokenSpec{Claims: claims})
}

func externalClaims() map[string]any {
	return map[string]any{"username": externalUsername, "groups": []string{"operators", "auditors"}, "tenant": referenceTenant}
}

func sign(t *testing.T, server *testoidc.Server, spec testoidc.TokenSpec) string {
	t.Helper()
	token, err := server.Sign(spec)
	require.NoError(t, err)
	return token
}

func assertExternalRBAC(t *testing.T, runner *oidcenvtest.Runner, fixture probe.AdminFixture, token string) {
	t.Helper()
	result := run(t, runner, fixture, token)
	require.Equal(t, http.StatusOK, result.HTTPStatus, runner.Diagnostics())
	require.Equal(t, externalUsername, result.Username)
	require.ElementsMatch(t, []string{"operators", "auditors", "system:authenticated"}, result.Groups)
	require.NotNil(t, result.Allowed)
	require.Equal(t, probe.Outcome{HTTPStatus: http.StatusOK, Allowed: true}, *result.Allowed)
	require.NotNil(t, result.Denied)
	require.Equal(t, http.StatusForbidden, result.Denied.HTTPStatus)
	require.False(t, result.Denied.Allowed)
}

func assertRejected(t *testing.T, runner *oidcenvtest.Runner, fixture probe.AdminFixture, token string) {
	t.Helper()
	result := run(t, runner, fixture, token)
	require.Equal(t, http.StatusUnauthorized, result.HTTPStatus, runner.Diagnostics())
	require.Empty(t, result.Username)
	require.Nil(t, result.Allowed)
}

func run(t *testing.T, runner *oidcenvtest.Runner, fixture probe.AdminFixture, token string) probe.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := probe.Run(ctx, runner.ProbeRequest(token, fixture.Allowed, fixture.Denied))
	require.NoError(t, err, runner.Diagnostics())
	return result
}

func prefixName(prefix string) string {
	switch prefix {
	case managementPrefix:
		return "management-spiffe"
	default:
		return strings.TrimSuffix(prefix, ":")
	}
}

func splitPrefix(prefix string) (string, string) {
	index := len(prefix) / 2
	return prefix[:index], prefix[index:]
}
