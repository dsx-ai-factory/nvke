// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
)

// NewMetricsServer returns an HTTP server that serves only the Prometheus
// /metrics endpoint on cfg.MetricsListenAddress (default: 127.0.0.1:9090).
func NewMetricsServer(cfg config.Config) *http.Server {
	mux := http.NewServeMux()
	mux.Handle(metrics, promhttp.Handler())
	return &http.Server{
		Addr:              cfg.MetricsListenAddress,
		Handler:           mux,
		ReadHeaderTimeout: ReadHeaderTimeout,
	}
}
