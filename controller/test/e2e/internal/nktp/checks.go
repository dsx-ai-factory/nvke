// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nktp

// Baseline is what NKTP delivers on any cluster: what nktp/hack/e2e-validate.sh
// checks on Kind, plus a span pushed through the node Collector, one Check per
// expectation so a failure names it.
func (v *Validator) Baseline() []Check {
	return []Check{
		// The installation itself.
		v.PodsReady(),
		v.CertificatesReady(),
		v.SidecarsReport(),
		v.NoExporterFailure(),

		// Kubernetes, through the kube-stack monitors, kube-state-metrics and
		// the cluster-events Collector.
		v.ScrapeJobUp("apiserver"),
		v.ScrapeJobUp("coredns"),
		v.ScrapeJobUp(chartRelease + "-operator"),
		v.ScrapeJobUp(chartNamespace + "/" + chartRelease + "-ksm-.+"),
		v.MetricArrives("kube_pod_info"),
		v.MetricArrives("kube_deployment_status_replicas_available"),
		v.EventsArrive(),
		v.NoEventWithoutService(),

		// The nodes, through the node Collector.
		v.ScrapeJobUp("kubelet"),
		v.ScrapeJobUp("kubelet-cadvisor"),
		v.ScrapeJobUp("node-exporter"),
		v.PodLogsArrive("kube-system"),
		v.PodLogsArrive(chartNamespace),
		v.JournalArrives(),
		v.CredentialArrivesRedacted(),
		v.CredentialNeverArrives(),

		// Internal mTLS: traffic flowing above shows the Collectors' own
		// certificates work; this shows a client without one cannot get in.
		v.GatewayRefusesAnonymous("watchdog"),
		v.GatewayRefusesAnonymous("gateway-logs"),
		v.GatewayRefusesAnonymous("gateway-metrics"),
		v.GatewayRefusesAnonymous("gateway-traces"),

		// Ingest: what a workload pushes to the node Collector arrives, with
		// the cluster's name added on the way.
		v.PushedTraceArrives(),

		// Autoscaling, through metrics-server.
		v.NodeMetricsServed(),
		v.HPAsReport(),

		// The cluster's name on every signal.
		v.NoMetricWithFallbackClusterName(),
		v.NoLogWithFallbackClusterName(),
	}
}
