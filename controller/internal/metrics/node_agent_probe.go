// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:gochecknoglobals // Prometheus metrics are conventionally package-level globals.
package metrics

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	metricOutcomeSuccess = "success"
)

var (
	nodeAgentActiveProbes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "nvke_controller_node_agent_probes_active",
		Help: "Number of node-agent reachability RPCs currently in flight.",
	})
	nodeAgentProbeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nvke_controller_node_agent_probe_duration_seconds",
		Help:    "Node-agent reachability RPC duration including transport setup, partitioned by bounded outcome.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 120},
	}, []string{"outcome"})
)

func init() {
	prometheus.MustRegister(nodeAgentActiveProbes, nodeAgentProbeDuration)
}

// NodeAgentProbeStarted records an in-flight reachability RPC.
func NodeAgentProbeStarted() {
	nodeAgentActiveProbes.Inc()
}

// NodeAgentProbeFinished records an RPC outcome without per-node metric labels.
func NodeAgentProbeFinished(duration time.Duration, err error) {
	nodeAgentActiveProbes.Dec()
	outcome := metricOutcomeSuccess
	switch {
	case errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded:
		outcome = "timeout"
	case errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled:
		outcome = "canceled"
	case err != nil:
		outcome = "error"
	}
	nodeAgentProbeDuration.WithLabelValues(outcome).Observe(duration.Seconds())
}
