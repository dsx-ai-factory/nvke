// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllerconfig

import (
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

// AgentReadinessConfig configures bounded node-agent probes and periodic scheduling.
type AgentReadinessConfig struct {
	Workers  int             `json:"workers"`
	Timeout  metav1.Duration `json:"timeout"`
	Interval metav1.Duration `json:"interval"`
	Jitter   float64         `json:"jitter"`
}

func (c AgentReadinessConfig) runtimeConfig() config.AgentReadinessConfig {
	return config.AgentReadinessConfig{
		Workers: c.Workers, Timeout: c.Timeout.Duration, Interval: c.Interval.Duration, Jitter: c.Jitter,
	}
}

func (c AgentReadinessConfig) validate() error {
	if c.Workers <= 0 || c.Timeout.Duration <= 0 || c.Interval.Duration <= 0 {
		return errors.New("workers, timeout and interval must be greater than 0")
	}
	return c.runtimeConfig().Validate()
}
