// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

// setMinimumEnv sets only what ParseConfigFromEnv and Config.Validate require,
// so a test exercises its own fields and nothing else.
func setMinimumEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CNPG_SUPERUSER_PASSWORD", "not-the-dev-default")
	t.Setenv("CONTROLLER_CONFIG_PATH", "/etc/nvke/controller-config/config.yaml")
}

// Validate ignores PermissiveMode, set or unset: nvke-controller-config owns
// the rule that it needs apiAuth.enabled.
func TestValidateIgnoresAuthFields(t *testing.T) {
	base := config.Config{
		CertRenewalInterval:           12 * time.Hour,
		CertRenewalThresholdDays:      30,
		CertificateInspectionInterval: time.Hour,
		CertificateObservationMaxAge:  3 * time.Hour,
		CertificateWarningDays:        45,
		MetricsListenAddress:          "127.0.0.1:9090",
		DBConnectionString:            "postgres://cwe:strong@localhost:5432/cwe?sslmode=disable",
		SpireServerAPIAddress:         "spire-server.spire-server.svc.cluster.local:8081",
		SpireJoinTokenTTL:             time.Hour,
		NodeAgent:                     config.NodeAgentConfig{SpireAttestationMethod: config.SpireAttestationMethodTPM},
	}

	for _, permissive := range []bool{false, true} {
		configured := base
		configured.PermissiveMode = permissive
		require.NoError(t, configured.Validate(), "permissive=%t must not change validation", permissive)
	}
}
