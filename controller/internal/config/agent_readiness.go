// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"math"
	"time"
)

// AgentReadinessConfig bounds concurrent reachability checks and periodic scheduling.
// Zero Workers, Timeout and Interval use defaults for programmatically constructed configs.
// Zero Jitter deliberately disables jitter.
type AgentReadinessConfig struct {
	Workers  int           `controllerconfig:"agentReadiness.workers"`
	Timeout  time.Duration `controllerconfig:"agentReadiness.timeout"`
	Interval time.Duration `controllerconfig:"agentReadiness.interval"`
	// Jitter varies Interval symmetrically by this fraction, without changing its mean.
	Jitter float64 `controllerconfig:"agentReadiness.jitter"`
}

// WithDefaults returns settings with bounded defaults for zero-valued durations and workers.
func (c AgentReadinessConfig) WithDefaults() AgentReadinessConfig {
	if c.Workers == 0 {
		c.Workers = 8
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.Interval == 0 {
		c.Interval = 2 * time.Minute
	}
	return c
}

// Validate rejects settings that would disable bounds or produce invalid schedules.
func (c AgentReadinessConfig) Validate() error {
	if c.Workers < 0 || c.Timeout < 0 || c.Interval < 0 {
		return fmt.Errorf("agentReadiness workers, timeout and interval must not be negative (zero uses defaults)")
	}
	if math.IsNaN(c.Jitter) || math.IsInf(c.Jitter, 0) || c.Jitter < 0 || c.Jitter >= 1 {
		return fmt.Errorf("agentReadiness.jitter must be finite and in [0, 1)")
	}
	return nil
}
