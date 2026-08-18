// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/NVIDIA/nke/controller/internal/config"
)

func TestTeleportConfig_NodePoolTeleportDefault(t *testing.T) {
	tests := []struct {
		name          string
		cpDefault     bool
		workerDefault bool
		controlPlane  bool
		want          bool
	}{
		{
			name:          "control plane pool uses CP default (false)",
			cpDefault:     false,
			workerDefault: true,
			controlPlane:  true,
			want:          false,
		},
		{
			name:          "control plane pool uses CP default (true)",
			cpDefault:     true,
			workerDefault: false,
			controlPlane:  true,
			want:          true,
		},
		{
			name:          "worker pool uses worker default (false)",
			cpDefault:     true,
			workerDefault: false,
			controlPlane:  false,
			want:          false,
		},
		{
			name:          "worker pool uses worker default (true)",
			cpDefault:     false,
			workerDefault: true,
			controlPlane:  false,
			want:          true,
		},
		{
			name:          "both defaults false, control plane",
			cpDefault:     false,
			workerDefault: false,
			controlPlane:  true,
			want:          false,
		},
		{
			name:          "both defaults true, worker",
			cpDefault:     true,
			workerDefault: true,
			controlPlane:  false,
			want:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.TeleportConfig{
				DefaultCPPoolEnabled:     tt.cpDefault,
				DefaultWorkerPoolEnabled: tt.workerDefault,
			}
			got := cfg.NodePoolTeleportDefault(tt.controlPlane)
			assert.Equal(t, tt.want, got)
		})
	}
}
