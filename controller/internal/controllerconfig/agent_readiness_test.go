// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerconfig_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

func TestAgentReadinessFileConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, old, replacement string
		wantErr                bool
	}{
		{name: "defaults"},
		{name: "zero jitter disables scheduling variation", old: "jitter: 0.1", replacement: "jitter: 0"},
		{name: "zero workers", old: "workers: 8", replacement: "workers: 0", wantErr: true},
		{name: "negative workers", old: "workers: 8", replacement: "workers: -1", wantErr: true},
		{name: "omitted workers", old: "  workers: 8\n", wantErr: true},
		{name: "zero timeout", old: "timeout: 30s", replacement: "timeout: 0s", wantErr: true},
		{name: "negative interval", old: "interval: 2m", replacement: "interval: -1s", wantErr: true},
		{name: "negative jitter", old: "jitter: 0.1", replacement: "jitter: -0.1", wantErr: true},
		{name: "jitter one", old: "jitter: 0.1", replacement: "jitter: 1", wantErr: true},
		{name: "malformed duration", old: "timeout: 30s", replacement: "timeout: invalid", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			document := minimalConfig
			if tc.old != "" {
				document = strings.Replace(document, tc.old, tc.replacement, 1)
			}
			file, err := controllerconfig.Load(writeConfig(t, document))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			var runtime config.Config
			file.ApplyTo(&runtime)
			want := config.AgentReadinessConfig{Workers: 8, Timeout: 30 * time.Second, Interval: 2 * time.Minute, Jitter: 0.1}
			if tc.old == "jitter: 0.1" {
				want.Jitter = 0
			}
			require.Equal(t, want, runtime.AgentReadiness)
		})
	}
}
