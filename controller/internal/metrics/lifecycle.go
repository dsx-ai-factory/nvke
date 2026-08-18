// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	tenantClusterLabel = "tenant_cluster"
	clusterIDLabel     = "cluster_id"
	nodePoolIDLabel    = "node_pool_id"
	nodeTypeLabel      = "node_type"
	resultLabel        = "result"
	siteIDLabel        = "site_id"
)

var (
	provisionDurationHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "cwe_node_provision_duration_seconds",
			Help: "End-to-end node provisioning duration by result (success/failure). " +
				"Query by result label to compare happy/unhappy paths, or omit it to see combined.",
			Buckets: []float64{30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600, 5400, 7200},
		},
		[]string{tenantClusterLabel, nodeTypeLabel, resultLabel},
	)

	provisionPhaseDurationHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cwe_node_provision_phase_duration_seconds",
			Help:    "Duration of individual phases within node provisioning",
			Buckets: []float64{30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600},
		},
		[]string{tenantClusterLabel, nodeTypeLabel, "phase"},
	)

	provisionResultCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cwe_node_provision_total",
			Help: "Total number of node provision workflow completions",
		},
		[]string{tenantClusterLabel, nodeTypeLabel, resultLabel},
	)

	teardownDurationHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "cwe_node_teardown_duration_seconds",
			Help: "End-to-end node teardown duration by result (success/failure). " +
				"Query by result label to compare happy/unhappy paths, or omit it to see combined.",
			Buckets: []float64{30, 60, 120, 300, 600, 900, 1200, 1800, 2700},
		},
		[]string{tenantClusterLabel, nodeTypeLabel, resultLabel},
	)

	teardownPhaseDurationHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cwe_node_teardown_phase_duration_seconds",
			Help:    "Duration of individual phases within node teardown",
			Buckets: []float64{15, 30, 60, 120, 300, 600, 900, 1200},
		},
		[]string{tenantClusterLabel, nodeTypeLabel, "phase"},
	)

	teardownResultCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cwe_node_teardown_total",
			Help: "Total number of node teardown workflow completions",
		},
		[]string{tenantClusterLabel, nodeTypeLabel, resultLabel},
	)
)

func init() {
	prometheus.MustRegister(provisionDurationHistogram)
	prometheus.MustRegister(provisionPhaseDurationHistogram)
	prometheus.MustRegister(provisionResultCounter)
	prometheus.MustRegister(teardownDurationHistogram)
	prometheus.MustRegister(teardownPhaseDurationHistogram)
	prometheus.MustRegister(teardownResultCounter)
}

func RecordProvisionCompleted(clusterName, nodeType string, duration time.Duration) {
	provisionDurationHistogram.WithLabelValues(clusterName, nodeType, "success").Observe(duration.Seconds())
	provisionResultCounter.WithLabelValues(clusterName, nodeType, "success").Inc()
}

func RecordProvisionFailed(clusterName, nodeType string, duration time.Duration) {
	provisionDurationHistogram.WithLabelValues(clusterName, nodeType, "failure").Observe(duration.Seconds())
	provisionResultCounter.WithLabelValues(clusterName, nodeType, "failure").Inc()
}

func RecordProvisionPhase(clusterName, nodeType, phase string, duration time.Duration) {
	provisionPhaseDurationHistogram.WithLabelValues(clusterName, nodeType, phase).Observe(duration.Seconds())
}

func RecordTeardownCompleted(clusterName, nodeType string, duration time.Duration) {
	teardownDurationHistogram.WithLabelValues(clusterName, nodeType, "success").Observe(duration.Seconds())
	teardownResultCounter.WithLabelValues(clusterName, nodeType, "success").Inc()
}

func RecordTeardownFailed(clusterName, nodeType string, duration time.Duration) {
	teardownDurationHistogram.WithLabelValues(clusterName, nodeType, "failure").Observe(duration.Seconds())
	teardownResultCounter.WithLabelValues(clusterName, nodeType, "failure").Inc()
}

func RecordTeardownPhase(clusterName, nodeType, phase string, duration time.Duration) {
	teardownPhaseDurationHistogram.WithLabelValues(clusterName, nodeType, phase).Observe(duration.Seconds())
}
