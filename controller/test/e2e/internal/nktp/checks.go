// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nktp

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/kube"
)

const (
	otlpGRPCPort        = "4317"
	fallbackClusterName = "unknown"

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

// Baseline is what NKTP delivers on any cluster: what nktp/hack/e2e-validate.sh
// checks on Kind, each scrape job on its own.
func Baseline() []Check {
	return slices.Concat(
		[]Check{
			{name: "every Pod in " + chartNamespace + " is Ready", probe: podsReady},
			{name: "every certificate in " + chartNamespace + " is Ready", probe: certificatesReady},
		},
		ScrapeJobsUp("kubelet", "kubelet-cadvisor", chartRelease+"-operator", "node-exporter",
			chartNamespace+"/"+chartRelease+"-ksm-.+", "apiserver", "coredns"),
		MetricsArrive("kube_pod_info", "kube_deployment_status_replicas_available"),
		[]Check{{name: "every Collector's sidecar reports its own telemetry", probe: sidecarsReport}},
		PodLogsArrive("kube-system", chartNamespace),
		[]Check{
			logSome("node journal logs reach Loki", func(t *target) string {
				return logLines(t.where(eq(serviceLabel, "journald")), "", recentLogs)
			}),
			logSome("Kubernetes events reach Loki", func(t *target) string {
				return logLines(t.where(like(serviceLabel, ".+"), serviceLabel+`!="unknown_service"`), `| k8s_event_reason=~".+"`, recentLogs)
			}),
			logSome("a logged credential arrives redacted", func(t *target) string {
				return logLines(t.where(eq(namespaceLabel, probeNamespace)), "|= "+strconv.Quote(t.redaction.pod)+` |= "****"`, recentLogs)
			}),
		},
		each([]string{"watchdog", "gateway-logs", "gateway-metrics", "gateway-traces"}, refusesAnonymous),
		[]Check{
			{name: "metrics-server serves node metrics", probe: nodeMetricsServed},
			{name: "every HPA in " + chartNamespace + " reports metrics", probe: hpasReport},

			promNone("no NKTP exporter fails to send", func(*target) string {
				return "sum by (service_name) (increase(" + selector(like("__name__", "otelcol_exporter_send_failed.*"), like(serviceLabel, chartRelease+"-.+")) + "[5m])) > 0"
			}),
			logNone("no Kubernetes event lacks a service name", func(*target) string {
				return logLines(selector(eq(serviceLabel, "unknown_service")), `| k8s_event_reason=~".+"`, recentLogs)
			}),
			logNone("the logged credential never reaches the backend", func(t *target) string {
				return logLines(selector(eq(namespaceLabel, probeNamespace)), "|= "+strconv.Quote(t.redaction.secret), recentLogs)
			}),
			promNone("no metric carries the fallback cluster name", func(*target) string {
				return "count(" + selector(eq(clusterLabel, fallbackClusterName)) + ")"
			}),
			logNone("no log carries the fallback cluster name", func(*target) string {
				return logLines(selector(eq(clusterLabel, fallbackClusterName)), "", recentLogs)
			}),
		},
	)
}

// ScrapeJobsUp is one check per job pattern: it has targets and every one is
// up. Per job, so a job with no targets cannot hide behind one that is up.
func ScrapeJobsUp(jobs ...string) []Check {
	return each(jobs, func(job string) Check {
		return Check{name: "scrape job " + job + " has targets, all up", probe: func(ctx context.Context, t *target) error {
			expr := "up" + t.where(like("job", job))
			series, err := t.grafana.promQL(ctx, expr)
			if err != nil {
				return err
			}
			if len(series) == 0 {
				return fmt.Errorf("no target: %s returned nothing", expr)
			}
			var down model.Vector
			for _, s := range series {
				if s.Value != 1 {
					down = append(down, s)
				}
			}
			if len(down) > 0 {
				return fmt.Errorf("%d of %d targets down: %s", len(down), len(series), describe(down))
			}
			return nil
		}}
	})
}

// MetricsArrive is one check per metric name pattern: a matching series
// exists, so the scrape returned data and not only an up.
func MetricsArrive(families ...string) []Check {
	return each(families, func(family string) Check {
		return promSome("metrics matching "+family+" arrive", func(t *target) string {
			return "count(" + t.where(like("__name__", family)) + ")"
		})
	})
}

// PodLogsArrive is one check per namespace: its Pods' logs reach Loki.
func PodLogsArrive(namespaces ...string) []Check {
	return each(namespaces, func(namespace string) Check {
		return logSome("pod logs from "+namespace+" reach Loki", func(t *target) string {
			return logLines(t.where(eq(namespaceLabel, namespace)), "", startupLogs)
		})
	})
}

// AuditLogArrives expects the API server's audit log in Loki.
func AuditLogArrives() Check {
	return logSome("the API server's audit log reaches Loki", func(t *target) string {
		return logLines(t.where(eq(serviceLabel, "kube-apiserver")), `| nvidia_telemetry_class="k8s.api.audit"`, startupLogs)
	})
}

// NoTargetDown fails on any scrape target that is down. Only an installation
// that turns off every monitor whose target it cannot reach can ask for it.
func NoTargetDown() Check {
	return promNone("no scrape target is down", func(t *target) string { return "up" + t.where() + " == 0" })
}

// TraceArrives expects Tempo to have the trace, by ID, so only a trace this
// run caused can satisfy it.
func TraceArrives(id, service string) Check {
	return Check{name: "trace " + id + " from " + service + " reaches Tempo", probe: func(ctx context.Context, t *target) error {
		query := fmt.Sprintf("{trace:id = %s && resource.service.name = %s && resource.k8s.cluster.name = %s}",
			strconv.Quote(id), strconv.Quote(service), strconv.Quote(t.clusterName))
		n, err := t.grafana.traceQL(ctx, query, traceWindow)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%s found nothing", query)
		}
		return nil
	}}
}

// sidecarsReport checks the sidecars' metrics: they keep only error and slow
// traces, so metrics are the signal that always flows.
func sidecarsReport(ctx context.Context, t *target) error {
	collectors, err := collectorNames(ctx, t)
	if err != nil {
		return err
	}
	if len(collectors) == 0 {
		return fmt.Errorf("no OpenTelemetryCollectors in %s", chartNamespace)
	}
	series, err := t.grafana.promQL(ctx, "count by (service_name) ("+t.where(like("__name__", "otelcol_process_uptime.*"), like(serviceLabel, ".+-sidecar"))+")")
	if err != nil {
		return err
	}
	reporting := make(map[string]bool, len(series))
	for _, s := range series {
		reporting[string(s.Metric[serviceLabel])] = true
	}
	var silent []string
	for _, collector := range collectors {
		if !reporting[collector+"-sidecar"] {
			silent = append(silent, collector+"-sidecar")
		}
	}
	if len(silent) > 0 {
		return fmt.Errorf("%d of %d sidecars report no uptime: %s", len(silent), len(collectors), list(silent))
	}
	return nil
}

// refusesAnonymous passes only when the gateway refuses a certificate-less
// client with a TLS alert. That leaves client-go logging "error forwarding
// port 4317 ... broken pipe": the gateway hanging up afterwards.
func refusesAnonymous(gateway string) Check {
	service := chartRelease + "-" + gateway + "-collector"
	return Check{name: service + " refuses a client without a certificate", probe: func(ctx context.Context, t *target) error {
		roots, err := internalCA(ctx, t.clientset, chartNamespace, chartRelease+"-internal-ca")
		if err != nil {
			return err
		}
		forwarder, err := kube.NewServiceForwarder(t.restConfig, chartNamespace, service, otlpGRPCPort)
		if err != nil {
			return err
		}
		defer forwarder.Close()
		conn, err := forwarder.Dial(ctx)
		if err != nil {
			return err
		}
		return refusesAnonymousClient(ctx, conn, service+"."+chartNamespace+".svc", roots)
	}}
}

// promSome holds when the query returns a series and promNone, a hold check,
// when it returns none; logSome and logNone are the same for LogQL.
func promSome(name string, expr func(*target) string) Check {
	return vectorCheck(name, false, (*grafana).promQL, expr)
}

func promNone(name string, expr func(*target) string) Check {
	return vectorCheck(name, true, (*grafana).promQL, expr)
}

func logSome(name string, expr func(*target) string) Check {
	return vectorCheck(name, false, (*grafana).logQL, expr)
}

func logNone(name string, expr func(*target) string) Check {
	return vectorCheck(name, true, (*grafana).logQL, expr)
}

func vectorCheck(name string, none bool, query func(*grafana, context.Context, string) (model.Vector, error), expr func(*target) string) Check {
	return Check{name: name, hold: none, probe: func(ctx context.Context, t *target) error {
		e := expr(t)
		series, err := query(t.grafana, ctx, e)
		if err != nil {
			return err
		}
		switch {
		case none && len(series) > 0:
			return fmt.Errorf("%s returned %d series, want none: %s", e, len(series), describe(series))
		case !none && len(series) == 0:
			return fmt.Errorf("%s returned nothing", e)
		}
		return nil
	}}
}

// where is a selector that carries the cluster's name: a signal without it is
// as lost to a dashboard filtering on it as one that never arrived.
func (t *target) where(matchers ...string) string {
	return selector(append([]string{eq(clusterLabel, t.clusterName)}, matchers...)...)
}

func selector(matchers ...string) string {
	return "{" + strings.Join(matchers, ", ") + "}"
}

func eq(label, value string) string {
	return label + "=" + strconv.Quote(value)
}

func like(label, pattern string) string {
	return label + "=~" + strconv.Quote(pattern)
}

func logLines(selector, pipeline, window string) string {
	return fmt.Sprintf("sum(count_over_time(%s [%s]))", strings.TrimSpace(selector+" "+pipeline), window)
}

func each[T any](items []T, check func(T) Check) []Check {
	checks := make([]Check, len(items))
	for i, item := range items {
		checks[i] = check(item)
	}
	return checks
}

func describe(series model.Vector) string {
	if len(series) <= describeLimit {
		return series.String()
	}
	return fmt.Sprintf("%s\nand %d more", series[:describeLimit].String(), len(series)-describeLimit)
}
