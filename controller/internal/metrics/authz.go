// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dsx-ai-factory/nvke/controller/internal/authz"
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

	// authzEnabled is a config-state gauge, not a traffic counter, mirroring
	// authEnabled: it is set once at startup from cfg.AuthzBundlePath/
	// AuthzPermissive, so a site with zero callers still reports its real mode.
	// nke_authz_requests_total cannot answer "is this on" on its own -- its
	// series only exist once a request has been evaluated, so a quiet site is
	// indistinguishable from a disabled one.
	authzEnabled = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nke_authz_enabled",
			Help: "1 for the deployment's configured API authorization mode (off, permissive, or enforcing). " +
				"Set once at startup from config, independent of traffic -- see nke_authz_requests_total for activity.",
		},
		[]string{"mode"},
	)

	_ authz.Recorder = RecordAuthzRequest
)

func init() {
	prometheus.MustRegister(authzRequestsTotal, authzEnabled)
}

// RecordAuthzRequest counts one authorization decision. Its signature is
// internal/authz.Recorder, so main passes it to authz.Setup directly.
func RecordAuthzRequest(outcome authz.Outcome, reason authz.DenyReason, action string) {
	authzRequestsTotal.WithLabelValues(string(outcome), string(reason), action).Inc()
}

// SetAuthzMode records the deployment's authorization mode once at startup:
// "off", "permissive", or "enforcing". Call exactly once; it is not meant to
// track changes at runtime, since the config is only ever read once at
// startup (see internal/authz.Setup).
func SetAuthzMode(mode string) {
	authzEnabled.WithLabelValues(mode).Set(1)
}
