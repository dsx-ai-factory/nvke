// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	teleportTokensCreated = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nke_controller_teleport_tokens_created_total",
			Help: "Total number of Teleport TPM join tokens created",
		},
		[]string{siteIDLabel},
	)

	teleportTokenErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nke_controller_teleport_token_errors_total",
			Help: "Total number of Teleport token creation errors",
		},
		[]string{siteIDLabel},
	)

	teleportRegistrationLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "nke_controller_teleport_registration_latency_seconds",
			Help:    "Time from token creation to Teleport registration",
			Buckets: []float64{60, 120, 300, 600, 900, 1200, 1800, 2400, 3600, 5400},
		},
		[]string{siteIDLabel},
	)

	teleportTokensSkipped = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nke_controller_teleport_tokens_skipped_total",
			Help: "Total number of token creations skipped (token recently created, still within refresh threshold)",
		},
		[]string{siteIDLabel},
	)

	teleportConfirmations = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nke_controller_teleport_confirmations_total",
			Help: "Total number of node registration confirmations (node appeared in Teleport)",
		},
		[]string{siteIDLabel},
	)

	teleportDeconfirmations = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nke_controller_teleport_deconfirmations_total",
			Help: "Total number of stale-node deconfirmations (anti-entropy corrections)",
		},
		[]string{siteIDLabel},
	)
)

func init() {
	prometheus.MustRegister(teleportTokensCreated)
	prometheus.MustRegister(teleportTokenErrors)
	prometheus.MustRegister(teleportRegistrationLatency)
	prometheus.MustRegister(teleportTokensSkipped)
	prometheus.MustRegister(teleportConfirmations)
	prometheus.MustRegister(teleportDeconfirmations)
}

func RecordTeleportTokensCreated(siteID string, count int) {
	teleportTokensCreated.WithLabelValues(siteID).Add(float64(count))
}

func RecordTeleportTokenErrors(siteID string, count int) {
	teleportTokenErrors.WithLabelValues(siteID).Add(float64(count))
}

func RecordTeleportRegistrationLatency(siteID string, seconds float64) {
	teleportRegistrationLatency.WithLabelValues(siteID).Observe(seconds)
}

func RecordTeleportTokensSkipped(siteID string, count int) {
	teleportTokensSkipped.WithLabelValues(siteID).Add(float64(count))
}

func RecordTeleportConfirmation(siteID string, count int) {
	teleportConfirmations.WithLabelValues(siteID).Add(float64(count))
}

func RecordTeleportDeconfirmation(siteID string, count int) {
	teleportDeconfirmations.WithLabelValues(siteID).Add(float64(count))
}
