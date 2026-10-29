// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/nvkerbac"
)

func TestRBACConfigMapSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  config.Config
		want nvkerbac.Source
	}{
		{
			// A config.yaml written before rbacConfigMapName existed decodes the
			// key as empty. Such a site's ConfigMap is nke-rbac in nke-system, and
			// a binary upgrade alone must not look elsewhere.
			name: "unset is the legacy ConfigMap in the legacy namespace",
			cfg:  config.Config{},
			want: nvkerbac.Source{Namespace: constants.LegacyManagementNamespace, Name: nvkerbac.LegacyConfigMapName},
		},
		{
			name: "the chart defaults are taken as given",
			cfg:  config.Config{ManagementNamespace: "nvke-system", RBACConfigMapName: "nvke-rbac"},
			want: nvkerbac.Source{Namespace: "nvke-system", Name: "nvke-rbac"},
		},
		{
			// The two are pinned independently, so a site that moved only its
			// namespace still reads the name it already has.
			name: "a pinned namespace keeps the legacy name",
			cfg:  config.Config{ManagementNamespace: "platform-system"},
			want: nvkerbac.Source{Namespace: "platform-system", Name: nvkerbac.LegacyConfigMapName},
		},
		{
			name: "a site may name its own",
			cfg:  config.Config{ManagementNamespace: "platform-system", RBACConfigMapName: "platform-rbac"},
			want: nvkerbac.Source{Namespace: "platform-system", Name: "platform-rbac"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.cfg.RBACConfigMapSource())
		})
	}
}
