// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/nvkerbac"
	adapter "github.com/dsx-ai-factory/nvke/controller/internal/rollout/adapters/controller"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout/operations/oidc"
)

func TestBootstrapAuthenticationMatchesRollout(t *testing.T) {
	provider := controllerconfig.OIDCProvider{
		Name: "workforce", IssuerURL: "https://workforce.example.com", Audiences: []string{"tenant"},
		UsernameClaim: "sub", UsernamePrefix: "workforce:", GroupsClaim: "groups", GroupsPrefix: "workforce:",
		RequiredClaims: map[string]string{"organization": "example"}, CABundle: "example public CA bundle",
	}
	a := &Activities{
		cfg:         config.Config{NvkeOIDCAuth: config.NvkeOIDCAuth{Enabled: true, IssuerURL: "https://shared.example.com"}},
		trustDomain: "jwt.example.com",
		k8sClient: fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName},
			Data:       map[string]string{nvkerbac.ConfigMapKey: "kind: ClusterRoleBinding\nsubjects:\n  - {kind: User, name: spiffe://jwt.example.com/capi}\n"},
		}).Build(),
	}
	pinned := db.ClusterOIDCProvidersFromConfig([]controllerconfig.OIDCProvider{provider})
	tests := []struct {
		name    string
		cluster db.Cluster
		needs   bool
		issuers []string
	}{
		{name: "nothing to render", cluster: db.Cluster{Name: "tenant"}},
		{name: "shared only", cluster: db.Cluster{Name: "tenant", OIDCAuthEnabled: true}, needs: true, issuers: []string{"https://shared.example.com"}},
		{name: "operator only before enablement", cluster: db.Cluster{Name: "tenant", OIDCProviders: pinned}, needs: true, issuers: []string{provider.IssuerURL}},
		{
			name: "combined after enablement", cluster: db.Cluster{Name: "tenant", OIDCAuthEnabled: true, OIDCProviders: pinned},
			needs: true, issuers: []string{"https://shared.example.com", provider.IssuerURL},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authn := a.clusterAuthentication(tt.cluster)
			require.Equal(t, tt.needs, authn.Needed())
			if !tt.needs {
				return
			}
			tt.cluster.KubernetesVersion = "v1.34.2"
			cfg, err := kubeadmconfig.BuildClusterAuthenticationConfig(context.Background(), a.k8sClient, authn)
			require.NoError(t, err)
			got := make([]string, 0, len(cfg.JWT))
			for _, jwt := range cfg.JWT {
				got = append(got, jwt.Issuer.URL)
			}
			require.Equal(t, tt.issuers, got)

			file, err := kubeadmconfig.GenerateAPIServerAuthenticationConfig(cfg, tt.cluster.KubernetesVersion)
			require.NoError(t, err)
			op := oidc.NewEnableOIDCOperation(
				oidc.WithOIDCConfig(&a.cfg.NvkeOIDCAuth),
				oidc.WithClient(a.k8sClient),
				oidc.WithTrustDomain(a.trustDomain),
				oidc.WithRBACConfigMap(a.cfg.RBACConfigMapSource()),
			)
			payload, err := op.BuildApplyToNodePayload(context.Background(), adapter.NewCluster(tt.cluster, nil, nil, "50051"))
			require.NoError(t, err)
			var artifact struct {
				Files    []*nvkeproto.File `json:"files"`
				Previous *struct {
					Files []*nvkeproto.File `json:"files"`
				} `json:"previous"`
			}
			require.NoError(t, json.Unmarshal(payload, &artifact))
			files := artifact.Files
			if !tt.cluster.OIDCAuthEnabled {
				require.NotNil(t, artifact.Previous, "operator-only bootstrap must have a rollback configuration")
				files = artifact.Previous.Files
			}
			require.Equal(t, toNodeAgentFiles([]kubeadmconfig.File{file}), files, "bootstrap and rollout must render identical authentication files")
		})
	}
}
