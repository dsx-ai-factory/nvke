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
	"github.com/dsx-ai-factory/nke/controller/internal/kubeadmconfig"
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

func TestRollback_AnonymousConfigurationMatchesBootstrap(t *testing.T) {
	for _, tt := range []struct {
		version string
		restore bool
	}{
		{version: "v1.33.9"},
		{version: "v1.34.0", restore: true},
		{version: "v1.35.0", restore: true},
	} {
		t.Run(tt.version, func(t *testing.T) {
			op := NewEnableOIDCOperation(WithOIDCConfig(&config.NkeOIDCAuth{Enabled: true, IssuerURL: "https://shared.example.com"}))
			payload, err := op.BuildApplyToNodePayload(context.Background(), fakeCluster{name: "tenant", k8s: tt.version})
			require.NoError(t, err)
			artifact, err := parsePayload(payload)
			require.NoError(t, err)

			initConfig := &kubeadmconfig.InitConfig{
				NodeIP: "10.0.0.10", APIServerEndpoint: "tenant.example:6443", NodeName: "cp-0",
				KubernetesVersion: tt.version, PodSubnet: "10.244.0.0/16", ServiceSubnet: "10.96.0.0/12",
			}
			initArtifacts, err := initConfig.Render()
			require.NoError(t, err)
			joinConfig := &kubeadmconfig.JoinConfig{
				ControlPlane: true, NodeIP: "10.0.0.11", APIServerEndpoint: "tenant.example:6443", NodeName: "cp-1",
				KubernetesVersion: tt.version, CertificateKey: "REDACTED", Token: "example-token", DiscoveryTokenCACertHash: "sha256:REDACTED",
			}
			joinArtifacts, err := joinConfig.Render()
			require.NoError(t, err)
			authFiles := func(files []kubeadmconfig.File) []fileArtifact {
				var out []fileArtifact
				for _, file := range files {
					if file.Path == artifact.AuthConfigPath {
						out = append(out, fileArtifact{Path: file.Path, Content: file.Content, Mode: file.Mode, Owner: file.Owner})
					}
				}
				return out
			}
			bootstrapFiles := authFiles(initArtifacts.Files)
			require.Equal(t, bootstrapFiles, authFiles(joinArtifacts.Files))
			c := &fakeClient{}
			op.nodeAgentClientDialer = dialerFor(c)
			require.NoError(t, op.Rollback(context.Background(), cpNode(), payload))
			require.Len(t, c.calls, 1)
			req := c.calls[0]
			require.Len(t, req.GetFlagOps(), 1)
			if !tt.restore {
				require.Nil(t, artifact.Previous)
				require.Empty(t, bootstrapFiles)
				require.Empty(t, req.GetFiles())
				require.Equal(t, kubeadmconfig.AuthConfigFlagName, req.GetFlagOps()[0].GetRemove().GetName())
				require.Equal(t, []string{artifact.AuthConfigPath}, req.GetRemoveFiles())
				return
			}
			require.NotNil(t, artifact.Previous, "rollback must retain the bootstrap anonymous restriction")
			require.Len(t, bootstrapFiles, 1)
			require.Equal(t, bootstrapFiles, artifact.Previous.Files)
			require.Equal(t, filesToProto(bootstrapFiles), req.GetFiles())
			require.Empty(t, req.GetRemoveFiles())
			require.Equal(t, kubeadmconfig.AuthConfigFlagName, req.GetFlagOps()[0].GetSet().GetName())
			require.Equal(t, artifact.AuthConfigPath, req.GetFlagOps()[0].GetSet().GetValue())
			enable, err := buildEnablePatchRequest(artifact)
			require.NoError(t, err)
			require.Equal(t, enable.GetStrategicMergePatches(), req.GetStrategicMergePatches(), "rollback must retain the mount and volume")
		})
	}
}
