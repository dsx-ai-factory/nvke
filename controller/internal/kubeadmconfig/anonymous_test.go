// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kube-apiserver refuses to start when --anonymous-auth is passed alongside an
// AuthenticationConfiguration carrying an anonymous section, and the flag on its
// own takes the static Pod health probes down with it. The restriction is
// expressed through the config file only; nothing may reintroduce the flag.
func TestClusterConfigurationNeverSetsAnonymousAuthFlag(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config func() *InitConfig
	}{
		{name: "without OIDC", config: validInitConfig},
		{
			name: "with OIDC",
			config: func() *InitConfig {
				cfg := validInitConfig()
				cfg.AuthenticationConfig = testAuthenticationConfig()
				return cfg
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := clusterConfiguration(tt.config()).APIServer.ExtraArgs

			assert.NotContains(t, args, "anonymous-auth",
				"--anonymous-auth is mutually exclusive with the anonymous section of AuthenticationConfiguration")
			assert.Equal(t, defaultOIDCAuthConfigPath, args[AuthConfigFlagName],
				"every cluster carries an AuthenticationConfiguration, OIDC or not")
		})
	}
}

// Anonymous access is closed everywhere except the endpoints kubeadm points the
// kube-apiserver static Pod probes at, plus the cluster-info discovery read.
func TestAnonymousProbeAuthConfigCoversTheProbeEndpoints(t *testing.T) {
	anonymous := anonymousProbeAuthConfig()
	require.NotNil(t, anonymous)
	assert.True(t, anonymous.Enabled)

	paths := make([]string, 0, len(anonymous.Conditions))
	for _, condition := range anonymous.Conditions {
		paths = append(paths, condition.Path)
	}
	assert.Equal(t, []string{"/livez", "/readyz", "/healthz", clusterInfoPath}, paths)
}

// Bootstrap token discovery reads kube-public/cluster-info before the joining
// node has a credential. Dropping it from the anonymous allow list fails every
// `kubeadm join` at preflight, which surfaces as control-plane nodes that never
// leave "Not yet provisioned" rather than as an auth error.
func TestAnonymousProbeAuthConfigAllowsClusterInfoDiscovery(t *testing.T) {
	conditions := anonymousProbeAuthConfig().Conditions
	paths := make([]string, 0, len(conditions))
	for _, condition := range conditions {
		paths = append(paths, condition.Path)
	}
	assert.Contains(t, paths, "/api/v1/namespaces/kube-public/configmaps/cluster-info")
}

// The anonymous restriction is folded in at render time, so a config that
// reaches the renderer without one -- the brownfield OIDC rollout builds its own
// -- still comes out restricted. Without this the two paths diverge silently.
func TestRenderAuthenticationConfigAddsAnonymousRestriction(t *testing.T) {
	rendered, err := renderAuthenticationConfig(testAuthenticationConfig())
	require.NoError(t, err)
	assert.Contains(t, string(rendered), "anonymous:")
	assert.Contains(t, string(rendered), "- path: /livez")
}

// A caller-supplied OIDC config with no providers is still a misconfiguration:
// the anonymous section must not make it render cleanly.
func TestRenderAuthenticationConfigStillRequiresAJWTProvider(t *testing.T) {
	_, err := renderAuthenticationConfig(&OIDCAuthenticationConfig{})
	require.ErrorContains(t, err, "requires at least one JWT provider")
}
