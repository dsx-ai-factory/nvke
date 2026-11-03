// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals.
var tenantKubeconfigRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "nvke_controller_tenant_kubeconfig_rejected_total",
	Help: "Tenant kubeconfigs the controller refused to load, by fixed source: node_agent (a control-plane node's admin.conf) or secret (a published kubeconfig Secret). Counts every attempt.",
}, []string{"source"})

const (
	// TenantKubeconfigSourceNodeAgent is an admin.conf a control-plane node returned.
	TenantKubeconfigSourceNodeAgent = "node_agent"
	// TenantKubeconfigSourceSecret is a published tenant kubeconfig Secret.
	TenantKubeconfigSourceSecret = "secret"
)

func init() { //nolint:gochecknoinits // Register Prometheus collectors at package initialization.
	prometheus.MustRegister(tenantKubeconfigRejected)
	// Create both series at zero so increase() sees the first rejection
	// instead of treating a series' first sample as its baseline.
	for _, source := range []string{TenantKubeconfigSourceNodeAgent, TenantKubeconfigSourceSecret} {
		tenantKubeconfigRejected.WithLabelValues(source)
	}
}

// RecordTenantKubeconfigRejected records one refused tenant kubeconfig. The
// cluster and node go to the log, not to labels.
func RecordTenantKubeconfigRejected(source string) {
	switch source {
	case TenantKubeconfigSourceNodeAgent, TenantKubeconfigSourceSecret:
		tenantKubeconfigRejected.WithLabelValues(source).Inc()
	}
}
