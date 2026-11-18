// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseContainerNodeShmSize(t *testing.T) {
	for name, tc := range map[string]struct{ env, want string }{
		"unset":           {"", "16g"},
		"whitespace only": {"  ", "16g"},
		"override":        {"32g", "32g"},
		"padded override": {" 8g ", "8g"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("NICO_CONTAINERS_ENABLED", "true")
			t.Setenv("NICO_CONTAINERS_NODE_SHM_SIZE", tc.env)
			cfg, err := Parse(slog.Default())
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.ContainerNodeShmSize)
		})
	}
}
