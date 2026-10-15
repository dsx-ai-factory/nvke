// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nktp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

const (
	nameLabel      = "__name__"
	clusterLabel   = "k8s_cluster_name"
	namespaceLabel = "k8s_namespace_name"
	serviceLabel   = "service_name"

	// startupLogs covers namespaces whose Pods log little after they start;
	// a run checks soon after installing them.
	recentLogs  = "15m"
	startupLogs = "1h"
	traceWindow = time.Hour

	describeLimit = 5
)

// ScrapeJobUp holds when the job has targets and every one is up: min()
// catches one target down among many, which a plain up == 1 would hide.
func (v *Validator) ScrapeJobUp(job string) Check {
	return v.some("scrape job "+job+" has targets, all up", datasourcePrometheus, "min(up"+v.where(like("job", job))+") == 1")
}

// MetricArrives holds when a series matching the name pattern exists, so the
// scrape returned data and not only an up.
func (v *Validator) MetricArrives(family string) Check {
	return v.some("metrics matching "+family+" arrive", datasourcePrometheus, "count("+v.where(like(nameLabel, family))+")")
}

// SidecarsReport holds when every Collector's sidecar reports its uptime. The
// sidecars keep only error and slow traces, so metrics are the signal that
// always flows.
func (v *Validator) SidecarsReport() Check {
	return Check{Name: "every Collector's sidecar reports its own telemetry", Probe: func(ctx context.Context) error {
		collectors, err := v.list(ctx, gvr("opentelemetry.io", "v1beta1", "opentelemetrycollectors"), chartNamespace)
		if err != nil {
			return err
		}
		uptime := v.where(like(nameLabel, "otelcol_process_uptime.*"), like(serviceLabel, ".+-sidecar"))
		return v.expect(ctx, false, datasourcePrometheus, fmt.Sprintf("count(count by (%s) (%s)) == %d", serviceLabel, uptime, len(collectors)))
	}}
}

// NoExporterFailure fails when an NKTP exporter failed to send in the last
// five minutes, so failures from a rollout age out. It is the check that
// catches a backend the gateways cannot reach.
func (v *Validator) NoExporterFailure() Check {
	failed := selector(like(nameLabel, "otelcol_exporter_send_failed.*"), like(serviceLabel, chartRelease+"-.+"))
	return v.none("no NKTP exporter fails to send", datasourcePrometheus, "sum by ("+serviceLabel+") (increase("+failed+"[5m])) > 0")
}

// NoTargetDown fails on any scrape target that is down. Only an installation
// that turns off every monitor whose target it cannot reach can ask for it.
func (v *Validator) NoTargetDown() Check {
	return v.none("no scrape target is down", datasourcePrometheus, "up"+v.where()+" == 0")
}

// NoMetricWithFallbackClusterName fails on a metric the Collectors could not
// label with the cluster's name.
func (v *Validator) NoMetricWithFallbackClusterName() Check {
	return v.none("no metric carries the fallback cluster name", datasourcePrometheus, "count("+selector(eq(clusterLabel, fallbackClusterName))+")")
}

// PodLogsArrive holds when the namespace's Pods' logs reach Loki, through the
// node Collector's filelog receiver.
func (v *Validator) PodLogsArrive(namespace string) Check {
	return v.some("pod logs from "+namespace+" reach Loki", datasourceLoki, logLines(v.where(eq(namespaceLabel, namespace)), "", startupLogs))
}

// JournalArrives holds when the nodes' systemd journals reach Loki, through the
// node Collector's journald receiver.
func (v *Validator) JournalArrives() Check {
	return v.some("node journal logs reach Loki", datasourceLoki, logLines(v.where(eq(serviceLabel, "journald")), "", recentLogs))
}

// EventsArrive holds when Kubernetes events reach Loki, named after the
// controller that reported them.
func (v *Validator) EventsArrive() Check {
	named := v.where(like(serviceLabel, ".+"), serviceLabel+`!="unknown_service"`)
	return v.some("Kubernetes events reach Loki", datasourceLoki, logLines(named, `| k8s_event_reason=~".+"`, recentLogs))
}

// NoEventWithoutService fails on an event that names no reporting controller.
func (v *Validator) NoEventWithoutService() Check {
	return v.none("no Kubernetes event lacks a service name", datasourceLoki,
		logLines(selector(eq(serviceLabel, "unknown_service")), `| k8s_event_reason=~".+"`, recentLogs))
}

// AuditLogArrives holds when the API server's audit log reaches Loki.
func (v *Validator) AuditLogArrives() Check {
	return v.some("the API server's audit log reaches Loki", datasourceLoki,
		logLines(v.where(eq(serviceLabel, "kube-apiserver")), `| nvidia_telemetry_class="k8s.api.audit"`, startupLogs))
}

// CredentialArrivesRedacted holds when the redaction probe's line arrives with
// its marker and the credential masked.
func (v *Validator) CredentialArrivesRedacted() Check {
	return v.some("a logged credential arrives redacted", datasourceLoki,
		logLines(v.where(eq(namespaceLabel, probeNamespace)), "|= "+strconv.Quote(v.probe.pod)+` |= "****"`, recentLogs))
}

// CredentialNeverArrives fails when the probe's credential reaches the backend.
func (v *Validator) CredentialNeverArrives() Check {
	return v.none("the logged credential never reaches the backend", datasourceLoki,
		logLines(selector(eq(namespaceLabel, probeNamespace)), "|= "+strconv.Quote(v.probe.secret), recentLogs))
}

// NoLogWithFallbackClusterName fails on a log the Collectors could not label
// with the cluster's name.
func (v *Validator) NoLogWithFallbackClusterName() Check {
	return v.none("no log carries the fallback cluster name", datasourceLoki, logLines(selector(eq(clusterLabel, fallbackClusterName)), "", recentLogs))
}

// TraceArrives expects Tempo to have the trace, by ID, so only a trace this
// run caused can satisfy it.
func (v *Validator) TraceArrives(id, service string) Check {
	return Check{Name: "trace " + id + " from " + service + " reaches Tempo", Probe: func(ctx context.Context) error {
		query := fmt.Sprintf("{trace:id = %q && resource.service.name = %q && resource.k8s.cluster.name = %q}", id, service, v.cluster)
		n, err := v.grafana.traces(ctx, query, traceWindow)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%s found nothing", query)
		}
		return nil
	}}
}

// some holds when the query returns a series. none is a hold check: it holds
// when the query returns no series.
func (v *Validator) some(name, datasource, expr string) Check {
	return Check{Name: name, Probe: func(ctx context.Context) error { return v.expect(ctx, false, datasource, expr) }}
}

func (v *Validator) none(name, datasource, expr string) Check {
	return Check{Name: name, Hold: true, Probe: func(ctx context.Context) error { return v.expect(ctx, true, datasource, expr) }}
}

func (v *Validator) expect(ctx context.Context, none bool, datasource, expr string) error {
	series, err := v.grafana.vector(ctx, datasource, expr)
	if err != nil {
		return err
	}
	switch {
	case none && len(series) > 0:
		return fmt.Errorf("%s returned %d series, want none: %s", expr, len(series), describe(series))
	case !none && len(series) == 0:
		return fmt.Errorf("%s returned nothing", expr)
	}
	return nil
}

// where is a selector that carries the cluster's name: a signal without it is
// as lost to a dashboard filtering on it as one that never arrived.
func (v *Validator) where(matchers ...string) string {
	return selector(append([]string{eq(clusterLabel, v.cluster)}, matchers...)...)
}

func selector(matchers ...string) string { return "{" + strings.Join(matchers, ", ") + "}" }

func eq(label, value string) string { return label + "=" + strconv.Quote(value) }

func like(label, pattern string) string { return label + "=~" + strconv.Quote(pattern) }

func logLines(sel, pipeline, window string) string {
	return fmt.Sprintf("sum(count_over_time(%s [%s]))", strings.TrimSpace(sel+" "+pipeline), window)
}

func describe(series model.Vector) string {
	if len(series) <= describeLimit {
		return series.String()
	}
	return fmt.Sprintf("%s\nand %d more", series[:describeLimit].String(), len(series)-describeLimit)
}
