// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var controlPlaneFailureDomainRotations = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "nvke_controller_control_plane_failure_domain_rotations_total",
		Help: "Total number of pending control-plane nodes moved to another failure domain because theirs had no capacity",
	},
	[]string{clusterIDLabel, clusterNameLabel},
)

func init() {
	prometheus.MustRegister(controlPlaneFailureDomainRotations)
}

// RecordControlPlaneFailureDomainRotation counts one control-plane node moved to another failure domain.
func RecordControlPlaneFailureDomainRotation(clusterID, clusterName string) {
	controlPlaneFailureDomainRotations.WithLabelValues(clusterID, metricLabelOrUnknown(clusterName)).Inc()
}
