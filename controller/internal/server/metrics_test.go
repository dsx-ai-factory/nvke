// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
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
	"sigs.k8s.io/controller-runtime/pkg/manager"
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

func TestMetricsServerServesReconcileMetrics(t *testing.T) {
	reconciled := make(chan struct{}, 1)
	mgr := controllertest.StartManager(t.Context(), t, nil, func(mgr manager.Manager) error {
		return builder.ControllerManagedBy(mgr).
			Named("metrics-server-test").
			For(&corev1.ConfigMap{}).
			Complete(reconcile.Func(func(context.Context, reconcile.Request) (reconcile.Result, error) {
				select {
				case reconciled <- struct{}{}:
				default:
				}
				return reconcile.Result{}, nil
			}))
	})
	require.NoError(t, mgr.GetClient().Create(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "default"},
	}))
	select {
	case <-reconciled:
	case <-time.After(30 * time.Second):
		t.Fatal("the test controller never reconciled")
	}

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		recorder := scrapeMetrics(t.Context())
		assert.Equal(c, http.StatusOK, recorder.Code)
		assert.Contains(c, recorder.Body.String(), `controller_runtime_reconcile_total{controller="metrics-server-test",result="success"} 1`)
		assert.Contains(c, recorder.Body.String(), `workqueue_adds_total{controller="metrics-server-test",name="metrics-server-test"}`)
	}, 10*time.Second, 100*time.Millisecond)
}

func scrapeMetrics(ctx context.Context) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	NewMetricsServer(config.Config{}).Handler.ServeHTTP(recorder, httptest.NewRequestWithContext(ctx, http.MethodGet, metrics, http.NoBody))
	return recorder
}
