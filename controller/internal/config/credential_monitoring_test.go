// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCredentialMonitoringConfigurationBounds(t *testing.T) {
	base := Config{CertRenewalControllerEnabled: true, CertRenewalInterval: 12 * time.Hour, CertRenewalThresholdDays: 30, CredentialMonitoringEnabled: true, CertificateInspectionInterval: time.Hour, CertificateObservationMaxAge: 3 * time.Hour, CertificateWarningDays: 45}
	require.NoError(t, base.validateCredentialMonitoring())
	for _, test := range []struct {
		name    string
		mutate  func(*Config)
		message string
	}{
		{"zero renewal interval", func(c *Config) { c.CertRenewalInterval = 0 }, "certRenewal.interval"},
		{"negative renewal interval", func(c *Config) { c.CertRenewalInterval = -time.Hour }, "certRenewal.interval"},
		{"too frequent checks", func(c *Config) { c.CertRenewalInterval = time.Minute }, "certRenewal.interval"},
		{"too slow checks", func(c *Config) { c.CertRenewalInterval = 25 * time.Hour }, "certRenewal.interval"},
		{"threshold too small", func(c *Config) { c.CertRenewalThresholdDays = 6 }, "certRenewal.thresholdDays"},
		{"threshold too large", func(c *Config) { c.CertRenewalThresholdDays = 91 }, "certRenewal.thresholdDays"},
		{"inspection too frequent", func(c *Config) { c.CertificateInspectionInterval = time.Minute }, "credentialMonitoring.inspectionInterval"},
		{"freshness too short", func(c *Config) { c.CertificateObservationMaxAge = time.Hour }, "credentialMonitoring.observationMaxAge"},
		{"freshness too long", func(c *Config) { c.CertificateObservationMaxAge = 25 * time.Hour }, "credentialMonitoring.observationMaxAge"},
		{"warning below threshold", func(c *Config) { c.CertificateWarningDays = 29 }, "must exceed"},
		{"warning outside bound", func(c *Config) { c.CertificateWarningDays = 121 }, "credentialMonitoring.warningDays"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.mutate(&cfg)
			require.ErrorContains(t, cfg.validateCredentialMonitoring(), test.message)
		})
	}
	for _, interval := range []time.Duration{time.Hour, 24 * time.Hour} {
		cfg := base
		cfg.CertRenewalInterval = interval
		require.NoError(t, cfg.validateCredentialMonitoring())
	}
	// Monitoring settings are still checked when only renewal is disabled.
	cfg := base
	cfg.CertRenewalControllerEnabled = false
	cfg.CertificateInspectionInterval = 0
	require.Error(t, cfg.validateCredentialMonitoring())
}

func TestCredentialMonitoringConfigurationIgnoresDisabledFeatures(t *testing.T) {
	base := Config{CertRenewalControllerEnabled: true, CertRenewalInterval: 12 * time.Hour, CertRenewalThresholdDays: 30, CredentialMonitoringEnabled: true, CertificateInspectionInterval: time.Hour, CertificateObservationMaxAge: 3 * time.Hour, CertificateWarningDays: 45}
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"renewal disabled with out-of-range interval and threshold", func(c *Config) {
			c.CertRenewalControllerEnabled = false
			c.CertRenewalInterval = 30 * time.Minute
			c.CertRenewalThresholdDays = 120
		}},
		{"renewal disabled leaves the warning window unrelated to its threshold", func(c *Config) {
			c.CertRenewalControllerEnabled = false
			c.CertRenewalThresholdDays = 60
		}},
		{"monitoring disabled with zero inspection and out-of-range collector inputs", func(c *Config) {
			c.CredentialMonitoringEnabled = false
			c.CertificateInspectionInterval = 0
			c.CertificateObservationMaxAge = 48 * time.Hour
			c.CertificateWarningDays = 200
		}},
		{"both disabled with only the collector inputs set", func(c *Config) {
			*c = Config{CertificateObservationMaxAge: time.Minute, CertificateWarningDays: 1}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.mutate(&cfg)
			require.NoError(t, cfg.validateCredentialMonitoring())
		})
	}
	// Enabling a feature brings its bounds back, and the warning relationship needs both.
	cfg := base
	cfg.CertRenewalThresholdDays = 60
	require.ErrorContains(t, cfg.validateCredentialMonitoring(), "must exceed")
	cfg = base
	cfg.CredentialMonitoringEnabled = false
	cfg.CertRenewalInterval = 30 * time.Minute
	require.ErrorContains(t, cfg.validateCredentialMonitoring(), "certRenewal.interval")
}

// TestCredentialMonitoringConfigurationChecksCollectorInputs: the credential
// metrics collector is always registered and reads these settings, so they must
// be usable even when monitoring and renewal are both disabled.
func TestCredentialMonitoringConfigurationChecksCollectorInputs(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*Config)
		message string
	}{
		{"zero observation age", func(c *Config) { c.CertificateObservationMaxAge = 0 }, "credentialMonitoring.observationMaxAge must be positive"},
		{"negative observation age", func(c *Config) { c.CertificateObservationMaxAge = -time.Hour }, "credentialMonitoring.observationMaxAge must be positive"},
		{"zero warning days", func(c *Config) { c.CertificateWarningDays = 0 }, "credentialMonitoring.warningDays must be positive"},
		{"negative warning days", func(c *Config) { c.CertificateWarningDays = -1 }, "credentialMonitoring.warningDays must be positive"},
	} {
		for _, monitoring := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/monitoring=%t", test.name, monitoring), func(t *testing.T) {
				cfg := Config{CredentialMonitoringEnabled: monitoring, CertificateInspectionInterval: time.Hour, CertificateObservationMaxAge: 3 * time.Hour, CertificateWarningDays: 45}
				test.mutate(&cfg)
				require.ErrorContains(t, cfg.validateCredentialMonitoring(), test.message)
			})
		}
	}
}
