// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var openVSwitchReconciliationCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "nvke_controller_open_vswitch_reconciliation_total",
		Help: "Total Open vSwitch reconciliation attempts by target cluster/node, implementation path, and result.",
	},
	[]string{tenantClusterLabel, nodeNameLabel, "path", resultLabel},
)

func init() {
	prometheus.MustRegister(openVSwitchReconciliationCounter)
}

// RecordOpenVSwitchReconciliation records which implementation path handled an
// Open vSwitch reconciliation and whether the attempt succeeded.
func RecordOpenVSwitchReconciliation(clusterName, nodeName, path string, err error) {
	result := "success"
	if err != nil {
		result = "failure"
	}
	openVSwitchReconciliationCounter.WithLabelValues(clusterName, nodeName, path, result).Inc()
}
