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
	rendered, err := renderAuthenticationConfig(testAuthenticationConfig(), supportedK8sVersion)
	require.NoError(t, err)
	assert.Contains(t, string(rendered), "anonymous:")
	assert.Contains(t, string(rendered), "- path: /livez")
}

// A caller-supplied OIDC config with no providers is still a misconfiguration:
// the anonymous section must not make it render cleanly.
func TestRenderAuthenticationConfigStillRequiresAJWTProvider(t *testing.T) {
	_, err := renderAuthenticationConfig(&OIDCAuthenticationConfig{}, supportedK8sVersion)
	require.ErrorContains(t, err, "requires at least one JWT provider")
}

// The AuthenticationConfiguration this package renders is only parseable from
// 1.32 on: earlier releases serve the type under a different API version and
// keep the anonymous conditions behind an alpha gate or lack them entirely.
// Staging it anyway leaves kube-apiserver unable to start, which is how the
// fleet's 1.29 and 1.31 profiles would have seen this change.
func TestSupportsAnonymousAuthConfig(t *testing.T) {
	for _, tt := range []struct {
		version string
		want    bool
	}{
		{version: "v1.29.7", want: false},
		{version: "v1.30.0", want: false},
		{version: "v1.31.12", want: false},
		{version: "v1.32.0", want: true},
		{version: "v1.34.2", want: true},
		{version: "", want: false},
		{version: "not-a-version", want: false},
	} {
		t.Run(tt.version, func(t *testing.T) {
			assert.Equal(t, tt.want, supportsAnonymousAuthConfig(tt.version))
		})
	}
}

// Below the floor a non-OIDC cluster keeps exactly the wiring it had before the
// anonymous restriction existed: no flag, no volume, no staged file.
func TestClusterConfigurationSkipsAuthConfigBelowFloor(t *testing.T) {
	cfg := validInitConfig()
	cfg.KubernetesVersion = unsupportedK8sVersion

	clusterConfig := clusterConfiguration(cfg)

	assert.NotContains(t, clusterConfig.APIServer.ExtraArgs, AuthConfigFlagName)
	for _, volume := range clusterConfig.APIServer.ExtraVolumes {
		assert.NotEqual(t, authenticationConfigVolume().Name, volume.Name)
	}

	artifacts, err := cfg.Render()
	require.NoError(t, err)
	for _, file := range artifacts.Files {
		assert.NotEqual(t, defaultOIDCAuthConfigPath, file.Path)
	}
}

// OIDC still needs the flag on those releases, so the gate must not take it
// away -- it only decides whether non-OIDC clusters get one too.
func TestClusterConfigurationKeepsAuthConfigForOIDCBelowFloor(t *testing.T) {
	cfg := validInitConfig()
	cfg.KubernetesVersion = unsupportedK8sVersion
	cfg.AuthenticationConfig = testAuthenticationConfig()

	args := clusterConfiguration(cfg).APIServer.ExtraArgs

	assert.Equal(t, defaultOIDCAuthConfigPath, args[AuthConfigFlagName])
}

// An OIDC cluster below the floor renders the document it rendered before this
// change: folding the anonymous section in would make kube-apiserver reject it.
func TestRenderAuthenticationConfigOmitsAnonymousBelowFloor(t *testing.T) {
	rendered, err := renderAuthenticationConfig(testAuthenticationConfig(), unsupportedK8sVersion)
	require.NoError(t, err)

	assert.NotContains(t, string(rendered), "anonymous:")
	assert.Contains(t, string(rendered), "jwt:")
}

// Control-plane joins stage the same file as init, so they need the same gate;
// otherwise an expanding control plane gets a document init never wrote.
func TestJoinRenderSkipsAuthConfigFileBelowFloor(t *testing.T) {
	cfg := validControlPlaneJoinConfig()
	cfg.KubernetesVersion = unsupportedK8sVersion

	artifacts, err := cfg.Render()
	require.NoError(t, err)

	for _, file := range artifacts.Files {
		assert.NotEqual(t, defaultOIDCAuthConfigPath, file.Path)
	}
}
