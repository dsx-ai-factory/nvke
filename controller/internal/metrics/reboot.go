// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	rebootDurationHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cwe_node_reboot_duration_seconds",
			Help:    "Time taken for node reboots to complete",
			Buckets: []float64{600, 900, 1200, 1800, 2700, 3600, 5400, 7200},
		},
		[]string{"reboot_reason"},
	)

	rebootAttemptsHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cwe_node_reboot_attempts",
			Help:    "Number of attempts required for successful reboots",
			Buckets: []float64{1, 2, 3, 4, 5, 10},
		},
		[]string{"reboot_reason"},
	)

	rebootCurrentAttemptGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cwe_node_reboot_current_attempt",
			Help: "Current reboot attempt number for a node",
		},
		[]string{"node_name"},
	)
)

func init() {
	prometheus.MustRegister(rebootDurationHistogram)
	prometheus.MustRegister(rebootAttemptsHistogram)
	prometheus.MustRegister(rebootCurrentAttemptGauge)
}

func RecordRebootCompleted(rebootReason string, duration time.Duration, attempt int) {
	rebootDurationHistogram.WithLabelValues(
		rebootReason,
	).Observe(duration.Seconds())

	rebootAttemptsHistogram.WithLabelValues(
		rebootReason,
	).Observe(float64(attempt))
}

func SetCurrentAttempt(nodeName string, attempt int) {
	rebootCurrentAttemptGauge.WithLabelValues(nodeName).Set(float64(attempt))
}

func ClearCurrentAttempt(nodeName string) {
	rebootCurrentAttemptGauge.WithLabelValues(nodeName).Set(0)
}
