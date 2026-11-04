// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/auth/authz"
)

const (
	// modeLabel is off | permissive | enforcing on nvke_authz_enabled, and
	// permissive | enforcing on nvke_authz_decisions_total.
	modeLabel = "mode"
)

var (
	// authzDecisionsTotal has no tenant or subject label, because callers
	// choose those values. action is an operationId from the API spec, a
	// closed set.
	authzDecisionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nvke_authz_decisions_total",
			Help: "Total API requests evaluated by authorization, by outcome, action and mode. " +
				"In mode=permissive, a refusing outcome counts a request that was let through.",
		},
		[]string{"outcome", "action", modeLabel},
	)

	// authzEnabled is set once at startup, so a site with no traffic still
	// reports its mode.
	authzEnabled = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nvke_authz_enabled",
			Help: "1 for the deployment's configured API authorization mode (off, permissive, or enforcing). " +
				"Set once at startup from config, independent of traffic -- see nvke_authz_decisions_total for activity.",
		},
		[]string{modeLabel},
	)
)

func init() {
	prometheus.MustRegister(authzDecisionsTotal, authzEnabled)
}

// SetAuthzMode records the deployment's authorization mode once at startup:
// "off", "permissive", or "enforcing".
func SetAuthzMode(mode string) {
	authzEnabled.WithLabelValues(mode).Set(1)
}

// AuthzRecorder returns the authz.Recorder for a Client in mode. It counts
// each decision in nvke_authz_decisions_total.
func AuthzRecorder(mode authn.Mode) authz.Recorder {
	return func(outcome authz.Outcome, action string) {
		authzDecisionsTotal.WithLabelValues(string(outcome), action, string(mode)).Inc()
	}
}
