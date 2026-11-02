// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkerbac_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/nvkerbac"
)

func TestReadManifest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("absent ConfigMap is NotFound", func(t *testing.T) {
		t.Parallel()
		_, err := nvkerbac.ReadManifest(ctx, fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build(), nvkerbac.Source{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName})
		require.Error(t, err)
		assert.True(t, apierrors.IsNotFound(err))
	})

	t.Run("returns the trimmed manifest", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName},
			Data:       map[string]string{nvkerbac.ConfigMapKey: "\nkind: ClusterRole\n\n"},
		}
		got, err := nvkerbac.ReadManifest(ctx, fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(cm).Build(), nvkerbac.Source{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName})
		require.NoError(t, err)
		assert.Equal(t, "kind: ClusterRole", string(got))
	})

	t.Run("reads the configured name, not the legacy one", func(t *testing.T) {
		t.Parallel()
		// Both ConfigMaps exist, as they would on a site mid-rename. Only the
		// one the Source names may be read: reading the other would apply RBAC
		// the operator has stopped maintaining.
		pinned := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "nvke-system", Name: "nvke-rbac"},
			Data:       map[string]string{nvkerbac.ConfigMapKey: "kind: ClusterRole\nmetadata:\n  name: pinned\n"},
		}
		legacy := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "nvke-system", Name: nvkerbac.LegacyConfigMapName},
			Data:       map[string]string{nvkerbac.ConfigMapKey: "kind: ClusterRole\nmetadata:\n  name: legacy\n"},
		}
		c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(pinned, legacy).Build()
		got, err := nvkerbac.ReadManifest(ctx, c, nvkerbac.Source{Namespace: "nvke-system", Name: "nvke-rbac"})
		require.NoError(t, err)
		assert.Contains(t, string(got), "name: pinned")
		assert.NotContains(t, string(got), "name: legacy")
	})

	t.Run("a name in the wrong namespace is NotFound", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "nvke-system", Name: "nvke-rbac"},
			Data:       map[string]string{nvkerbac.ConfigMapKey: "kind: ClusterRole\n"},
		}
		c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(cm).Build()
		_, err := nvkerbac.ReadManifest(ctx, c, nvkerbac.Source{Namespace: constants.LegacyManagementNamespace, Name: "nvke-rbac"})
		require.Error(t, err)
		assert.True(t, apierrors.IsNotFound(err))
	})
}

func TestUserSubjects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		manifest string
		want     []string
		wantErr  bool
	}{
		{
			name: "users from both binding kinds, each once, in order",
			manifest: `kind: ClusterRoleBinding
subjects:
  - {kind: User, name: spiffe://a.example/one}
  - {kind: Group, name: nke:operators}
  - {kind: ServiceAccount, name: sa, namespace: nke-system}
---
kind: RoleBinding
subjects:
  - {kind: User, name: spiffe://b.example/two}
  - {kind: User, name: spiffe://a.example/one}
`,
			want: []string{"spiffe://a.example/one", "spiffe://b.example/two"},
		},
		{
			name: "subjects outside bindings are ignored",
			manifest: `kind: ClusterRole
rules: []
---
kind: ConfigMap
subjects:
  - {kind: User, name: ignored}
`,
		},
		{
			name: "operator users from both binding kinds are not reserved",
			manifest: `kind: ClusterRoleBinding
metadata:
  annotations:
    nvke.nvidia.com/identity-source: operator
subjects:
  - {kind: User, name: corp:alice}
---
kind: RoleBinding
metadata:
  annotations:
    nvke.nvidia.com/identity-source: operator
subjects:
  - {kind: User, name: corp:bob}
`,
		},
		{
			name: "management binding still reserves a user also in an operator binding",
			manifest: `kind: RoleBinding
metadata:
  annotations:
    nvke.nvidia.com/identity-source: operator
subjects:
  - {kind: User, name: exchanged-service}
---
kind: ClusterRoleBinding
subjects:
  - {kind: User, name: exchanged-service}
---
kind: RoleBinding
metadata:
  annotations:
    nvke.nvidia.com/identity-source: management
subjects:
  - {kind: User, name: explicit-management-service}
`,
			want: []string{"exchanged-service", "explicit-management-service"},
		},
		{
			name: "invalid identity source fails closed",
			manifest: `kind: ClusterRoleBinding
metadata:
  name: invalid
  annotations:
    nvke.nvidia.com/identity-source: opertor
subjects:
  - {kind: User, name: corp:alice}
`,
			wantErr: true,
		},
		{
			name:     "malformed YAML is an error",
			manifest: "kind: [unclosed",
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := nvkerbac.UserSubjects([]byte(tt.manifest))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
