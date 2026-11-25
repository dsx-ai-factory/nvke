// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseContainerNodeSettings(t *testing.T) {
	tests := map[string]struct {
		gpus     string
		wantGPUs string
	}{
		"defaults":             {},
		"GPUs are trimmed":     {gpus: " all ", wantGPUs: "all"},
		"whitespace-only GPUs": {gpus: "  "},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("NICO_CONTAINERS_NODE_GPUS", tt.gpus)
			cfg, err := Parse(slog.Default())
			require.NoError(t, err)
			require.Equal(t, tt.wantGPUs, cfg.ContainerNodeGPUs)
		})
	}
}
