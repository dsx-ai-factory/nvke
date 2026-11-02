// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/controllertest"
)

// TestMetricsServerServesControllerRuntimeMetrics checks that /metrics serves
// controller-runtime's own registry beside the default one. Reconcile, work
// queue, client and leader-election metrics are recorded there, not in the
// default registry.
func TestMetricsServerServesControllerRuntimeMetrics(t *testing.T) {
	probe := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "nvke_controller_metrics_server_test_probe_total",
		Help: "Registered by the metrics server test in controller-runtime's registry.",
	})
	require.NoError(t, ctrlmetrics.Registry.Register(probe))
	t.Cleanup(func() { ctrlmetrics.Registry.Unregister(probe) })
	probe.Inc()

	recorder := scrapeMetrics(t.Context())

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "nvke_controller_metrics_server_test_probe_total 1")
	assert.Contains(t, recorder.Body.String(), "go_goroutines")
}

// TestMetricsServerKeepsServingWhenRegistriesCollide checks that a metric
// both registries define differently costs the scrape that metric, not the
// whole response.
func TestMetricsServerKeepsServingWhenRegistriesCollide(t *testing.T) {
	name := "nvke_controller_metrics_server_test_collision_total"
	inDefault := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: "Registered in the default registry."})
	inControllerRuntime := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: "Registered in controller-runtime's registry."})
	require.NoError(t, prometheus.DefaultRegisterer.Register(inDefault))
	t.Cleanup(func() { prometheus.DefaultRegisterer.Unregister(inDefault) })
	require.NoError(t, ctrlmetrics.Registry.Register(inControllerRuntime))
	t.Cleanup(func() { ctrlmetrics.Registry.Unregister(inControllerRuntime) })

	recorder := scrapeMetrics(t.Context())

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "go_goroutines")
}

// TestMetricsServerDropsPerClusterControllerSeries checks that series of the
// controllers NVKE runs for each tenant cluster, whose names end in the
// cluster's ID, are left out, so the scrape stays bounded however many
// clusters come and go. The management plane's own controllers are kept.
func TestMetricsServerDropsPerClusterControllerSeries(t *testing.T) {
	clusterID := "3f2c9a4e-8b1d-4c6e-9f0a-1b2c3d4e5f60"
	mixed := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nvke_controller_metrics_server_test_reconciles_total",
		Help: "Registered by the metrics server test in controller-runtime's registry.",
	}, []string{"controller", "name"})
	onlyTenants := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nvke_controller_metrics_server_test_depth",
		Help: "Registered by the metrics server test in controller-runtime's registry.",
	}, []string{"name"})
	for _, collector := range []prometheus.Collector{mixed, onlyTenants} {
		require.NoError(t, ctrlmetrics.Registry.Register(collector))
		t.Cleanup(func() { ctrlmetrics.Registry.Unregister(collector) })
	}
	mixed.WithLabelValues("tenant-csr-"+clusterID, "tenant-csr-"+clusterID).Inc()
	mixed.WithLabelValues("nico-spiffe", "nico-spiffe").Inc()
	onlyTenants.WithLabelValues("tenant-nodestatus-" + clusterID).Set(3)

	recorder := scrapeMetrics(t.Context())

	require.Equal(t, http.StatusOK, recorder.Code)
	body := recorder.Body.String()
	assert.NotContains(t, body, clusterID)
	assert.Contains(t, body, `nvke_controller_metrics_server_test_reconciles_total{controller="nico-spiffe",name="nico-spiffe"} 1`)
	assert.NotContains(t, body, "nvke_controller_metrics_server_test_depth", "a family with only per-cluster series is left out")
}

func TestMetricsServerServesReconcileMetrics(t *testing.T) {
	// controller-runtime keeps a controller's name for the life of the
	// process, so a second run with -count=2 needs a name of its own.
	name := fmt.Sprintf("metrics-server-test-%d", time.Now().UnixNano())
	reconciled := make(chan struct{}, 1)
	mgr, err := controllertest.NewManager(nil)
	require.NoError(t, err)
	require.NoError(t, builder.ControllerManagedBy(mgr).
		Named(name).
		For(&corev1.ConfigMap{}).
		Complete(reconcile.Func(func(context.Context, reconcile.Request) (reconcile.Result, error) {
			select {
			case reconciled <- struct{}{}:
			default:
			}
			return reconcile.Result{}, nil
		})))
	// Created before the manager starts, so the informer's initial list holds
	// it: the fake client's watch does not replay an object created between
	// that list and the watch.
	require.NoError(t, mgr.GetClient().Create(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "default"},
	}))
	controllertest.Start(t.Context(), t, mgr)
	select {
	case <-reconciled:
	case <-time.After(30 * time.Second):
		t.Fatal("the test controller never reconciled")
	}

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		recorder := scrapeMetrics(t.Context())
		assert.Equal(c, http.StatusOK, recorder.Code)
		assert.Contains(c, recorder.Body.String(), fmt.Sprintf(`controller_runtime_reconcile_total{controller=%q,result="success"} 1`, name))
		assert.Contains(c, recorder.Body.String(), fmt.Sprintf(`workqueue_adds_total{controller=%q,name=%q}`, name, name))
	}, 10*time.Second, 100*time.Millisecond)
}

func scrapeMetrics(ctx context.Context) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	NewMetricsServer(config.Config{}).Handler.ServeHTTP(recorder, httptest.NewRequestWithContext(ctx, http.MethodGet, metrics, http.NoBody))
	return recorder
}
