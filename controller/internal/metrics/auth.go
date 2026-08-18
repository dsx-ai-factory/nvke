// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/NVIDIA/nke/controller/internal/auth"
)

const (
	// outcomeLabel is authenticated | would_reject | rejected.
	outcomeLabel = "outcome"

	// reasonLabel is an internal/auth.FailureReason, empty on success.
	reasonLabel = "reason"

	// issuerLabel is the configured issuer name or "unknown". The name, never
	// the "iss" claim: the name is a closed set fixed at startup, while a URL
	// read off a token would let any caller mint metric series at will.
	issuerLabel = "issuer"
)

var (
	// No path label: it would multiply the series by the operation count for a
	// signal about callers rather than routes, and the middleware already logs
	// the path.
	authRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nke_auth_requests_total",
			Help: "Total API requests evaluated by authentication, by outcome, failure reason and issuer. " +
				"outcome=would_reject is a permissive-mode deployment counting what enforcement would have refused.",
		},
		[]string{outcomeLabel, reasonLabel, issuerLabel},
	)

	_ auth.Recorder = RecordAuthRequest
)

func init() {
	prometheus.MustRegister(authRequestsTotal)
}

// RecordAuthRequest counts one authentication decision. Its signature is
// internal/auth.Recorder, so main passes it to auth.NewAuthenticator directly.
func RecordAuthRequest(outcome auth.Outcome, reason auth.FailureReason, issuer string) {
	authRequestsTotal.WithLabelValues(string(outcome), string(reason), issuer).Inc()
}
