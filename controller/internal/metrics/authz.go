// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/NVIDIA/nke/controller/internal/authz"
)

const (
	// authzOutcomeLabel is allowed | would_reject | rejected.
	authzOutcomeLabel = "outcome"

	// authzReasonLabel is an internal/authz.DenyReason, empty on success.
	authzReasonLabel = "reason"

	// actionLabel is the operationId. Unlike a path it is a closed set, fixed at
	// build time by the generated operation table, so no caller can mint a
	// series by inventing a URL. It is empty when the request never resolved to
	// an operation.
	actionLabel = "action"
)

var (
	authzRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nke_authz_requests_total",
			Help: "Total API requests evaluated by authorization, by outcome, denial reason and action. " +
				"outcome=would_reject is a permissive-mode deployment counting what enforcement would have refused.",
		},
		[]string{authzOutcomeLabel, authzReasonLabel, actionLabel},
	)

	_ authz.Recorder = RecordAuthzRequest
)

func init() {
	prometheus.MustRegister(authzRequestsTotal)
}

// RecordAuthzRequest counts one authorization decision. Its signature is
// internal/authz.Recorder, so main passes it to authz.Setup directly.
func RecordAuthzRequest(outcome authz.Outcome, reason authz.DenyReason, action string) {
	authzRequestsTotal.WithLabelValues(string(outcome), string(reason), action).Inc()
}
