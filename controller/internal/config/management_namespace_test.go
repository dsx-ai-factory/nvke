// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
)

func TestManagementNamespaceOrDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{
			// A config.yaml written before managementNamespace existed decodes
			// the key as empty. Such a site holds its Secrets in nke-system, and
			// a binary upgrade alone must not move it.
			name: "unset is the legacy namespace",
			cfg:  config.Config{},
			want: constants.LegacyManagementNamespace,
		},
		{
			name: "the chart default is taken as given",
			cfg:  config.Config{ManagementNamespace: "nvke-system"},
			want: "nvke-system",
		},
		{
			name: "a site may name its own",
			cfg:  config.Config{ManagementNamespace: "platform-system"},
			want: "platform-system",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.cfg.ManagementNamespaceOrDefault())
		})
	}
}
