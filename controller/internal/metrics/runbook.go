// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
	"github.com/prometheus/client_golang/prometheus"
)

var runbookExecutionCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "cwe_runbook_execution_total",
		Help: "Total number of runbook executions by runbook, target cluster/node, and result (success/failure).",
	},
	[]string{"runbook_name", tenantClusterLabel, "node_name", resultLabel},
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
