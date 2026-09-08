// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controllertest provides an in-process controller-runtime manager
// whose fake client and informer cache share one Kubernetes object tracker.
package controllertest

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// NewManager creates a real controller-runtime manager backed by an in-memory
// Kubernetes client and informer cache. If scheme is nil, the client-go scheme
// is used.
func NewManager(scheme *runtime.Scheme) (manager.Manager, error) {
	if scheme == nil {
		scheme = kscheme.Scheme
	}

	kubeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		Build()
	mapper := meta.NewDefaultRESTMapper(nil)

	return ctrl.NewManager(&rest.Config{
		Host: "https://example.invalid",
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
	}, ctrl.Options{
		Scheme: scheme,
		Logger: logr.FromSlogHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
		MapperProvider: func(_ *rest.Config, _ *http.Client) (meta.RESTMapper, error) {
			return mapper, nil
		},
		NewCache: func(_ *rest.Config, opts cache.Options) (cache.Cache, error) {
			return newFakeCache(kubeClient, scheme, opts.ByObject), nil
		},
		NewClient: func(_ *rest.Config, _ client.Options) (client.Client, error) {
			return kubeClient, nil
		},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		PprofBindAddress:       "0",
	})
}

// StartManager creates a manager, registers controllers through register, and
// starts the manager for the lifetime of the test.
func StartManager(
	ctx context.Context,
	tb testing.TB,
	scheme *runtime.Scheme,
	register func(manager.Manager) error,
) manager.Manager {
	tb.Helper()

	mgr, err := NewManager(scheme)
	if err != nil {
		tb.Fatalf("Creating controller-runtime manager: %v", err)
	}
	if err := register(mgr); err != nil {
		tb.Fatalf("Registering controllers: %v", err)
	}
	Start(ctx, tb, mgr)
	return mgr
}

// Start runs mgr in the background and stops it during test cleanup.
func Start(ctx context.Context, tb testing.TB, mgr manager.Manager) {
	tb.Helper()

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(runCtx) }()
	tb.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				tb.Errorf("Manager Start should return nil on graceful shutdown, got: %v", err)
			}
		case <-time.After(10 * time.Second):
			tb.Error("Manager did not stop within the deadline")
		}
	})
}
