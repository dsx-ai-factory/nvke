// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

// NewMetricsServer returns an HTTP server that serves only the Prometheus
// /metrics endpoint on cfg.MetricsListenAddress (default: 127.0.0.1:9090).
// It serves controller-runtime's registry beside the default one, because the
// manager's own metrics server is disabled.
func NewMetricsServer(cfg config.Config) *http.Server {
	mux := http.NewServeMux()
	gatherers := prometheus.Gatherers{prometheus.DefaultGatherer, prometheus.GathererFunc(gatherControllerRuntime)}
	mux.Handle(metrics, promhttp.InstrumentMetricHandler(prometheus.DefaultRegisterer, promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{})))
	return &http.Server{
		Addr:              cfg.MetricsListenAddress,
		Handler:           mux,
		ReadHeaderTimeout: ReadHeaderTimeout,
	}
}

// gatherControllerRuntime gathers controller-runtime's registry without its Go
// runtime and process collectors, which the default registry already serves.
func gatherControllerRuntime() ([]*dto.MetricFamily, error) {
	families, err := ctrlmetrics.Registry.Gather()
	kept := families[:0]
	for _, family := range families {
		if name := family.GetName(); !strings.HasPrefix(name, "go_") && !strings.HasPrefix(name, "process_") {
			kept = append(kept, family)
		}
	}
	return kept, err
}
