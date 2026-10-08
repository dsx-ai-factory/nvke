// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"time"
)

// validateCredentialMonitoring checks the settings of each enabled feature.
// Out-of-range settings for a disabled feature must not stop startup, except
// those the always-registered credential metrics collector reads.
// The chart's nke-controller-config template refuses the same values.
func (c Config) validateCredentialMonitoring() error {
	if c.CertificateObservationMaxAge <= 0 {
		return fmt.Errorf("credentialMonitoring.observationMaxAge must be positive")
	}
	if c.CertificateWarningDays <= 0 {
		return fmt.Errorf("credentialMonitoring.warningDays must be positive")
	}
	if c.CertRenewalControllerEnabled {
		if c.CertRenewalInterval < time.Hour || c.CertRenewalInterval > 24*time.Hour {
			return fmt.Errorf("certRenewal.interval must be between 1h and 24h")
		}
		if c.CertRenewalThresholdDays < 7 || c.CertRenewalThresholdDays > 90 {
			return fmt.Errorf("certRenewal.thresholdDays must be between 7 and 90")
		}
	}
	if !c.CredentialMonitoringEnabled {
		return nil
	}
	if c.CertificateInspectionInterval < 5*time.Minute || c.CertificateInspectionInterval > 12*time.Hour {
		return fmt.Errorf("credentialMonitoring.inspectionInterval must be between 5m and 12h")
	}
	if c.CertificateObservationMaxAge < 2*c.CertificateInspectionInterval || c.CertificateObservationMaxAge > 24*time.Hour {
		return fmt.Errorf("credentialMonitoring.observationMaxAge must be at least twice credentialMonitoring.inspectionInterval and at most 24h")
	}
	if c.CertificateWarningDays < 9 || c.CertificateWarningDays > 120 {
		return fmt.Errorf("credentialMonitoring.warningDays must be between 9 and 120")
	}
	if c.CertRenewalControllerEnabled &&
		time.Duration(c.CertificateWarningDays-c.CertRenewalThresholdDays)*24*time.Hour < 2*c.CertRenewalInterval {
		return fmt.Errorf("credentialMonitoring.warningDays must exceed certRenewal.thresholdDays by at least two certRenewal.intervals")
	}
	return nil
}
