// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dsx-ai-factory/nvke/auth/authz"
)

var (
	// authzDecisionsTotal has no subject label, because callers choose its
	// value. action is an operationId from the API spec, a closed set.
	authzDecisionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nvke_authz_decisions_total",
			Help: "Total API requests evaluated by authorization, by outcome and action. " +
				"In permissive mode, a refusing outcome counts a request that was let through.",
		},
		[]string{outcomeLabel, "action"},
	)

	// authzEnabled is set once at startup, so a site with no traffic still
	// reports its mode.
	authzEnabled = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nvke_authz_enabled",
			Help: "1 for the deployment's configured API authorization mode (off, permissive, or enforcing). " +
				"Set once at startup from config, independent of traffic -- see nvke_authz_decisions_total for activity.",
		},
		[]string{"mode"},
	)

	authzBundleReloadFailuresTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "nvke_authz_bundle_reload_failures_total",
			Help: "Total failed reloads of the API authorization bundle. The controller keeps enforcing the last bundle that loaded, " +
				"so a rising count means a bundle change is not in effect.",
		},
	)

	_ authz.Recorder = RecordAuthzDecision
)

func init() {
	prometheus.MustRegister(authzDecisionsTotal, authzEnabled, authzBundleReloadFailuresTotal)
}

// RecordAuthzDecision counts one authorization decision. Its signature is
// auth/authz.Recorder, so it is passed to authz.NewClient directly.
func RecordAuthzDecision(outcome authz.Outcome, action string) {
	authzDecisionsTotal.WithLabelValues(string(outcome), action).Inc()
}

// SetAuthzMode records the deployment's authorization mode once at startup:
// "off", "permissive", or "enforcing".
func SetAuthzMode(mode string) {
	authzEnabled.WithLabelValues(mode).Set(1)
}

// RecordAuthzBundleReloadFailure counts one failed reload of the authorization
// bundle.
func RecordAuthzBundleReloadFailure() {
	authzBundleReloadFailuresTotal.Inc()
}
