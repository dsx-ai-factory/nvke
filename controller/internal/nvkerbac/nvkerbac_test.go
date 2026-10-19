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
		_, err := nvkerbac.ReadManifest(ctx, fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build(), constants.LegacyManagementNamespace)
		require.Error(t, err)
		assert.True(t, apierrors.IsNotFound(err))
	})

	t.Run("returns the trimmed manifest", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.ConfigMapName},
			Data:       map[string]string{nvkerbac.ConfigMapKey: "\nkind: ClusterRole\n\n"},
		}
		got, err := nvkerbac.ReadManifest(ctx, fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(cm).Build(), constants.LegacyManagementNamespace)
		require.NoError(t, err)
		assert.Equal(t, "kind: ClusterRole", string(got))
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
