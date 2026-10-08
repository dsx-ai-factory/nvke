// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

// Validate rejects the compiled-in dev-default database credentials on any
// database that is not loopback. On loopback they are what a local run is meant
// to use.
func TestValidateWeakDBCredentials(t *testing.T) {
	t.Parallel()

	base := config.Config{
		CertRenewalInterval:           12 * time.Hour,
		CertRenewalThresholdDays:      30,
		CertificateInspectionInterval: time.Hour,
		CertificateObservationMaxAge:  3 * time.Hour,
		CertificateWarningDays:        45,
		MetricsListenAddress:          "127.0.0.1:9090",
		SpireServerAPIAddress:         "spire-server.spire-server.svc.cluster.local:8081",
		SpireJoinTokenTTL:             time.Hour,
		NodeAgent:                     config.NodeAgentConfig{SpireAttestationMethod: config.SpireAttestationMethodTPM},
	}

	tests := []struct {
		name                  string
		dbConnectionString    string
		cnpgSuperuserPassword string
		wantErr               bool
	}{
		{
			name:               "dev default against an in-cluster database",
			dbConnectionString: "postgres://postgres:postgres@postgres-postgresql.postgres.svc.cluster.local:5432/cwe?sslmode=disable",
			wantErr:            true,
		},
		{
			name:               "dev default against a remote address",
			dbConnectionString: "postgres://postgres:postgres@10.0.0.5:5432/cwe?sslmode=disable",
			wantErr:            true,
		},
		{
			name:                  "in-cluster, but a password overrides the default",
			dbConnectionString:    "postgres://postgres:postgres@postgres-postgresql.postgres.svc.cluster.local:5432/cwe?sslmode=disable",
			cnpgSuperuserPassword: "from-vault",
		},
		{
			name:               "credentials that are not the dev default",
			dbConnectionString: "postgres://cwe:strong@postgres-postgresql.postgres.svc.cluster.local:5432/cwe?sslmode=disable",
		},
		{
			name:               "dev default on localhost is a developer's own container",
			dbConnectionString: "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable",
		},
		{
			name:               "dev default on the loopback address",
			dbConnectionString: "postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable",
		},
		{
			name:               "dev default on IPv6 loopback",
			dbConnectionString: "postgres://postgres:postgres@[::1]:5432/postgres?sslmode=disable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := base
			cfg.DBConnectionString = tt.dbConnectionString
			cfg.CnpgSuperuserPassword = tt.cnpgSuperuserPassword

			err := cfg.Validate()
			if tt.wantErr {
				require.ErrorContains(t, err, "CNPG_SUPERUSER_PASSWORD")
				return
			}
			require.NoError(t, err)
		})
	}
}
