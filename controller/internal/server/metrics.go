// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"regexp"
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
	handler := promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
	mux.Handle(metrics, promhttp.InstrumentMetricHandler(prometheus.DefaultRegisterer, handler))
	return &http.Server{
		Addr:              cfg.MetricsListenAddress,
		Handler:           mux,
		ReadHeaderTimeout: ReadHeaderTimeout,
	}
}

// perClusterControllerName matches the names of the controllers NVKE runs for
// each tenant cluster, which end in the cluster's ID (tenant-csr-<cluster ID>
// and the like, under internal/controllers/tenant).
var perClusterControllerName = regexp.MustCompile(`-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// gatherControllerRuntime gathers controller-runtime's registry without the Go
// runtime and process collectors it registers by default (collectors.NewGoCollector
// and collectors.NewProcessCollector), which the default registry already serves.
//
// It also leaves out the series of per-cluster controllers. Their names carry
// a cluster ID, so served as they are the series would grow with every tenant
// cluster and outlive deleted ones; a family left with no series is dropped.
func gatherControllerRuntime() ([]*dto.MetricFamily, error) {
	families, err := ctrlmetrics.Registry.Gather()
	kept := families[:0]
	for _, family := range families {
		if name := family.GetName(); strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			continue
		}
		family.Metric = withoutPerClusterControllers(family.GetMetric())
		if len(family.GetMetric()) > 0 {
			kept = append(kept, family)
		}
	}
	return kept, err
}

// withoutPerClusterControllers drops the series whose controller or name label
// (the work queue's) is a per-cluster controller's name.
func withoutPerClusterControllers(series []*dto.Metric) []*dto.Metric {
	kept := series[:0]
	for _, metric := range series {
		if !namesPerClusterController(metric) {
			kept = append(kept, metric)
		}
	}
	return kept
}

func namesPerClusterController(metric *dto.Metric) bool {
	for _, label := range metric.GetLabel() {
		if name := label.GetName(); (name == "controller" || name == "name") && perClusterControllerName.MatchString(label.GetValue()) {
			return true
		}
	}
	return false
}
