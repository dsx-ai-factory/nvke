// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package server

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/NVIDIA/nke/controller/internal/config"
)

// NewMetricsServer returns an HTTP server that serves only the Prometheus
// /metrics endpoint on cfg.MetricsListenAddress (default: 127.0.0.1:9090).
func NewMetricsServer(cfg config.Config) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	return &http.Server{
		Addr:              cfg.MetricsListenAddress,
		Handler:           mux,
		ReadHeaderTimeout: ReadHeaderTimeout,
	}
}
