// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var nodeProvisioningGiveups = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "cwe_node_provisioning_giveups_total",
		Help: "Total number of nodes recycled after provisioning exceeded the configured timeout",
	},
	[]string{siteIDLabel, clusterIDLabel, nodePoolIDLabel, "cluster_name", "node_pool_name"},
)

func init() {
	prometheus.MustRegister(nodeProvisioningGiveups)
}

func RecordNodeProvisioningGiveup(siteID, clusterID, nodePoolID, clusterName, nodePoolName string) {
	nodeProvisioningGiveups.WithLabelValues(
		siteID,
		clusterID,
		nodePoolID,
		metricLabelOrUnknown(clusterName),
		metricLabelOrUnknown(nodePoolName),
	).Inc()
}

func metricLabelOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
