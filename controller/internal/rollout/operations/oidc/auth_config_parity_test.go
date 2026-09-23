// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
)

// sharedAuthConfigGolden is the fixture the greenfield bootstrap path already
// asserts, from kubeadmconfig/init_test.go and join_test.go.
//
// Read from kubeadmconfig rather than copied here on purpose. The doc comment on
// BuildOIDCAuthenticationConfig says greenfield and brownfield render
// byte-identical output, and one shared file is what enforces it. A copy would
// let the two drift while both tests stayed green.
const sharedAuthConfigGolden = "../../../kubeadmconfig/testdata/auth_config_simple.yaml"

// TestBuildApplyToNodePayload_MatchesGreenfieldGolden closes the gap the doc
// comment on BuildOIDCAuthenticationConfig claims is closed.
//
// Greenfield renders through kubeadmconfig and is asserted against this golden.
// Brownfield renders through BuildApplyToNodePayload and asserted nothing, so
// either call site could drift and no test would notice. Both now compare to the
// same bytes.
//
// Comparing the two functions directly would prove nothing: they call the same
// BuildOIDCAuthenticationConfig, so equal inputs give equal output by
// construction. The risk is in the call sites, which is where this sits.
func TestBuildApplyToNodePayload_MatchesGreenfieldGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Clean(sharedAuthConfigGolden))
	require.NoError(t, err, "reading shared golden %q", sharedAuthConfigGolden)

	op := NewEnableOIDCOperation(
		WithClient(oidcCABundleClient(t)),
		WithOIDCConfig(&config.NkeOIDCAuth{
			Enabled:      true,
			IssuerURL:    "https://nke.site-controller.internal",
			DiscoveryURL: "https://10.0.0.10:30443/.well-known/openid-configuration",
			CABundleSecret: config.OIDCCABundleSecretRef{
				Namespace: "nke-system",
				Name:      "oidc-ca",
				Key:       "ca.crt",
			},
		}),
	)

	// The cluster name becomes the audience, so it has to match the golden, and
	// the version has to be one that carries the anonymous restriction -- the
	// greenfield golden is rendered with it, so parity is only meaningful above
	// the floor. Below it both paths drop the section, which
	// TestRenderAuthenticationConfigOmitsAnonymousBelowFloor covers.
	cluster := fakeCluster{name: "tenant-cluster-42", k8s: "1.34.2", cps: []rollout.Node{cpNode()}}
	payload, err := op.BuildApplyToNodePayload(context.Background(), cluster)
	require.NoError(t, err)

	parsed, err := parsePayload(payload)
	require.NoError(t, err)
	require.Len(t, parsed.Files, 1)

	assert.Equal(t, string(want), string(parsed.Files[0].Content),
		"brownfield auth config must match the golden greenfield asserts")
}

func oidcCABundleClient(t *testing.T) client.Client {
	t.Helper()

	return fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nke-system", Name: "oidc-ca"},
		Data: map[string][]byte{
			"ca.crt": []byte("-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n"),
		},
	}).Build()
}
