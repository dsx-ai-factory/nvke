// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals.
var credentialAuditWrites = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "nvke_controller_credential_audit_writes_total",
	Help: "Credential audit ledger write observations by fixed result: stored, invalid_event, sink_failed, rate_limited. Includes duplicate writes.",
}, []string{"result"})

const (
	credentialAuditResultStored       = "stored"
	credentialAuditResultInvalidEvent = "invalid_event"
	credentialAuditResultSinkFailed   = "sink_failed"
	credentialAuditResultRateLimited  = "rate_limited"
)

func init() { //nolint:gochecknoinits // Register Prometheus collectors at package initialization.
	prometheus.MustRegister(credentialAuditWrites)
	// Create every series at zero so increase() sees the first failure burst
	// instead of treating a series' first sample as its baseline.
	for _, result := range []string{credentialAuditResultStored, credentialAuditResultInvalidEvent, credentialAuditResultSinkFailed, credentialAuditResultRateLimited} {
		credentialAuditWrites.WithLabelValues(result)
	}
}

// RecordCredentialAuditWrite records a bounded ledger result, never event IDs or
// execution, cluster, node, error text or secret values as metric labels.
func RecordCredentialAuditWrite(result string) {
	switch result {
	case credentialAuditResultStored, credentialAuditResultInvalidEvent, credentialAuditResultSinkFailed, credentialAuditResultRateLimited:
		credentialAuditWrites.WithLabelValues(result).Inc()
	}
}
