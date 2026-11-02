// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/nvkerbac"
)

const clusterAuthTrustDomain = "management.example.test"

func clusterAuthClient(objects ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objects...).Build()
}

func clusterAuthShared() config.NvkeOIDCAuth {
	return config.NvkeOIDCAuth{
		Enabled:   true,
		IssuerURL: "https://management.example.test",
		CABundleSecret: config.OIDCCABundleSecretRef{
			Namespace: "nke-system",
			Name:      "oidc-ca",
			Key:       "ca.crt",
		},
	}
}

func clusterAuthCASecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nke-system", Name: "oidc-ca"},
		Data:       map[string][]byte{"ca.crt": []byte("management CA")},
	}
}

func clusterAuthOperator() controllerconfig.OIDCProvider {
	return controllerconfig.OIDCProvider{
		Name:           "corp",
		IssuerURL:      "https://login.example.test",
		Audiences:      []string{"nke"},
		UsernameClaim:  "email",
		UsernamePrefix: "corp:",
		CABundle:       "corp CA",
	}
}

func nvkeRBAC(users ...string) *corev1.ConfigMap {
	manifest := "kind: ClusterRoleBinding\nsubjects:\n"
	for _, user := range users {
		manifest += "  - {kind: User, name: " + user + "}\n"
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName},
		Data:       map[string]string{nvkerbac.ConfigMapKey: manifest},
	}
}

func TestSupportsAuthenticationConfiguration(t *testing.T) {
	t.Parallel()
	for version, want := range map[string]bool{
		"1.34.0":  true,
		"v1.34.1": true,
		"1.35.0":  true,
		"1.33.9":  false,
		"1.30.0":  false,
		"":        false,
		"latest":  false,
	} {
		assert.Equal(t, want, SupportsAuthenticationConfiguration(version), version)
	}
}

// Otherwise a node joining an existing cluster would disagree with its peers.
func TestBuildClusterAuthenticationConfigSharedOnlyIsUnchanged(t *testing.T) {
	t.Parallel()

	want, err := BuildOIDCAuthenticationConfig("tenant-1", clusterAuthShared(), []byte("management CA"))
	require.NoError(t, err)

	got, err := BuildClusterAuthenticationConfig(context.Background(), clusterAuthClient(clusterAuthCASecret()), ClusterAuthentication{
		ClusterName:   "tenant-1",
		Shared:        clusterAuthShared(),
		SharedEnabled: true,
		TrustDomain:   clusterAuthTrustDomain,
	})
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestBuildClusterAuthenticationConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("shared and operator providers, with nke-rbac subjects reserved", func(t *testing.T) {
		t.Parallel()
		got, err := BuildClusterAuthenticationConfig(ctx, clusterAuthClient(clusterAuthCASecret(), nvkeRBAC("spiffe://exchange.example/capi")), ClusterAuthentication{
			ClusterName:   "tenant-1",
			Shared:        clusterAuthShared(),
			SharedEnabled: true,
			Providers:     []controllerconfig.OIDCProvider{clusterAuthOperator()},
			TrustDomain:   clusterAuthTrustDomain,

			RBACConfigMap: nvkerbac.Source{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName},
		})
		require.NoError(t, err)
		require.Len(t, got.JWT, 2)
		assert.Equal(t, "https://management.example.test", got.JWT[0].Issuer.URL)
		assert.Equal(t, "management CA", got.JWT[0].Issuer.CertificateAuthority)
		assert.Empty(t, got.JWT[0].UserValidationRules, "the shared provider gets no operator rules")
		assert.Equal(t, "https://login.example.test", got.JWT[1].Issuer.URL)
		expressions := make([]string, 0, len(got.JWT[1].UserValidationRules))
		for _, rule := range got.JWT[1].UserValidationRules {
			expressions = append(expressions, rule.Expression)
		}
		assert.Contains(t, expressions, `!user.username.startsWith("spiffe://`+clusterAuthTrustDomain+`/")`)
		assert.Contains(t, expressions, `user.username != "spiffe://exchange.example/capi"`)
	})

	t.Run("operator providers without the shared provider read no CA bundle", func(t *testing.T) {
		t.Parallel()
		got, err := BuildClusterAuthenticationConfig(ctx, clusterAuthClient(), ClusterAuthentication{
			ClusterName: "tenant-1",
			Shared:      clusterAuthShared(),
			Providers:   []controllerconfig.OIDCProvider{clusterAuthOperator()},
		})
		require.NoError(t, err)
		require.Len(t, got.JWT, 1)
		assert.Equal(t, "https://login.example.test", got.JWT[0].Issuer.URL)
	})

	t.Run("neither provider is an error", func(t *testing.T) {
		t.Parallel()
		_, err := BuildClusterAuthenticationConfig(ctx, clusterAuthClient(), ClusterAuthentication{ClusterName: "tenant-1"})
		require.Error(t, err)
	})
}

func TestAdditionalRBACIdentitySources(t *testing.T) {
	t.Parallel()
	cm := nvkeRBAC("spiffe://exchange.example/capi", "corp:exchanged-service")
	cm.Data[nvkerbac.ConfigMapKey] += `
---
kind: ClusterRoleBinding
metadata:
  name: tenant-users
  annotations:
    nvke.nvidia.com/identity-source: operator
subjects:
  - {kind: User, name: corp:alice}
  - {kind: User, name: spiffe://management.example.test/service}
  - {kind: User, name: system:admin}
`
	got, err := BuildClusterAuthenticationConfig(context.Background(), clusterAuthClient(clusterAuthCASecret(), cm), ClusterAuthentication{
		ClusterName:   "tenant-1",
		Shared:        clusterAuthShared(),
		SharedEnabled: true,
		Providers:     []controllerconfig.OIDCProvider{clusterAuthOperator()},
		TrustDomain:   clusterAuthTrustDomain,
		RBACConfigMap: nvkerbac.Source{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName},
	})
	require.NoError(t, err)
	require.Len(t, got.JWT, 2)
	assert.Empty(t, got.JWT[0].UserValidationRules)
	operator := got.JWT[1]
	assertUserValidation(t, operator.UserValidationRules, mapOperatorIdentity(operator, "alice", nil), true)
	assertUserValidation(t, operator.UserValidationRules, mapOperatorIdentity(operator, "exchanged-service", nil), false)
	for _, username := range []string{"spiffe://exchange.example/capi", "spiffe://management.example.test/service", "system:admin"} {
		assertUserValidation(t, operator.UserValidationRules, mappedOperatorIdentity{username: username}, false)
	}
}

func TestLoadOperatorJWTPolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	readErr := errors.New("apiserver unavailable")
	failing := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return readErr
		},
	}).Build()

	// A site that pinned a name other than the default. The ConfigMap carries a
	// name the legacy fixture never uses, so a caller that read the legacy one
	// would reserve nothing and the case would fail rather than pass quietly.
	pinnedSrc := nvkerbac.Source{Namespace: constants.LegacyManagementNamespace, Name: "pinned-rbac"}
	pinnedRBAC := nvkeRBAC("spiffe://exchange.example/pinned")
	pinnedRBAC.Name = pinnedSrc.Name

	tests := []struct {
		name        string
		reader      client.Reader
		trustDomain string
		src         nvkerbac.Source
		want        OperatorJWTPolicy
		wantErr     string
	}{
		{
			name:        "the configured name is the one read",
			reader:      clusterAuthClient(pinnedRBAC),
			trustDomain: clusterAuthTrustDomain,
			src:         pinnedSrc,
			want: OperatorJWTPolicy{
				ManagementJWTTrustDomain: clusterAuthTrustDomain,
				ReservedSubjects:         []string{"spiffe://exchange.example/pinned"},
			},
		},
		{
			// The manifest is there under its own name; a caller still on the
			// default reserves nothing and leaves those identities claimable.
			name:        "a manifest under another name is not read",
			reader:      clusterAuthClient(pinnedRBAC),
			trustDomain: clusterAuthTrustDomain,
			want:        OperatorJWTPolicy{ManagementJWTTrustDomain: clusterAuthTrustDomain},
		},
		{
			name:        "no nke-rbac reserves the trust domain only",
			reader:      clusterAuthClient(),
			trustDomain: clusterAuthTrustDomain,
			want:        OperatorJWTPolicy{ManagementJWTTrustDomain: clusterAuthTrustDomain},
		},
		{
			name:   "no nke-rbac and no SPIFFE reserves nothing extra",
			reader: clusterAuthClient(),
		},
		{
			name:        "nke-rbac users are reserved",
			reader:      clusterAuthClient(nvkeRBAC("spiffe://exchange.example/capi", "spiffe://exchange.example/cwe")),
			trustDomain: clusterAuthTrustDomain,
			want: OperatorJWTPolicy{
				ManagementJWTTrustDomain: clusterAuthTrustDomain,
				ReservedSubjects:         []string{"spiffe://exchange.example/capi", "spiffe://exchange.example/cwe"},
			},
		},
		{
			name:    "nke-rbac without a trust domain is an error",
			reader:  clusterAuthClient(nvkeRBAC("spiffe://exchange.example/capi")),
			wantErr: "trust domain is unknown",
		},
		{
			name:        "a failed read is an error",
			reader:      failing,
			trustDomain: clusterAuthTrustDomain,
			wantErr:     readErr.Error(),
		},
		{
			name:        "no client is an error",
			trustDomain: clusterAuthTrustDomain,
			wantErr:     "needs a management-cluster client",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := tt.src
			if src == (nvkerbac.Source{}) {
				src = nvkerbac.Source{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName}
			}
			got, err := LoadOperatorJWTPolicy(ctx, tt.reader, tt.trustDomain, src)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
