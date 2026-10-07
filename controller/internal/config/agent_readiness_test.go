// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

func TestAgentReadinessIgnoresRetiredEnvironment(t *testing.T) {
	setMinimumEnv(t)
	for _, key := range []string{"WORKERS", "TIMEOUT", "INTERVAL", "JITTER"} {
		t.Setenv("NODE_AGENT_READINESS_"+key, "invalid-retired-value")
	}
	cfg, err := config.ParseConfigFromEnv()
	require.NoError(t, err)
	require.Zero(t, cfg.AgentReadiness)
}

func TestAgentReadinessValidation(t *testing.T) {
	tests := map[string]config.AgentReadinessConfig{
		"negative workers":  {Workers: -1},
		"negative timeout":  {Timeout: -time.Second},
		"negative interval": {Interval: -time.Second},
		"negative jitter":   {Jitter: -0.1},
		"jitter one":        {Jitter: 1},
		"jitter nan":        {Jitter: math.NaN()},
		"jitter infinity":   {Jitter: math.Inf(1)},
	}
	for name, settings := range tests {
		t.Run(name, func(t *testing.T) { require.Error(t, settings.Validate()) })
	}
	require.NoError(t, (config.AgentReadinessConfig{}).Validate())
	require.Equal(t, config.AgentReadinessConfig{Workers: 8, Timeout: 30 * time.Second, Interval: 2 * time.Minute}, (config.AgentReadinessConfig{}).WithDefaults())
}
