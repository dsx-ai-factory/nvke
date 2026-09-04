// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var runbookExecutionCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "cwe_runbook_execution_total",
		Help: "Total number of runbook executions by runbook, target cluster/node, and result (success/failure).",
	},
	[]string{"runbook_name", tenantClusterLabel, nodeNameLabel, resultLabel},
)

func init() {
	prometheus.MustRegister(runbookExecutionCounter)
}

func RecordRunbookExecution(runbookName, clusterName, nodeName string, err error) {
	result := "success"
	if err != nil {
		result = "failure"
	}
	runbookExecutionCounter.WithLabelValues(runbookName, clusterName, nodeName, result).Inc()
}
