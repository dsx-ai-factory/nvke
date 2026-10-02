// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package manager constructs the management engine's single-cluster runtime.
package manager

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	nkev1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
)

// LeaderElectionID is the Lease name shared by management-engine replicas.
const LeaderElectionID = "management-engine.nke.nvidia.com"

// Options configures the manager runtime.
type Options struct {
	MetricsBindAddress      string
	HealthProbeBindAddress  string
	LeaderElection          bool
	LeaderElectionNamespace string
	Logger                  logr.Logger
}

// DefaultOptions enables leader election, metrics, and health probes.
// Callers must set LeaderElectionNamespace when election is enabled.
func DefaultOptions() Options {
	return Options{
		MetricsBindAddress: "127.0.0.1:9090", HealthProbeBindAddress: ":8081",
		LeaderElection: true,
	}
}

// New constructs a manager without controllers, watches, or API discovery prechecks.
// The management API must use HTTPS with certificate verification.
// Custom transports and wrappers are trusted to preserve TLS verification and the selected API origin.
// Callers register controllers before Start, handle signals, and exit promptly when Start returns.
func New(config *rest.Config, opts Options) (ctrl.Manager, error) {
	if config == nil {
		return nil, fmt.Errorf("management API configuration is required")
	}
	if !rest.IsConfigTransportTLS(*config) {
		return nil, fmt.Errorf("management API must use HTTPS")
	}
	if config.Insecure {
		return nil, fmt.Errorf("management API TLS certificate verification must be enabled")
	}
	if opts.LeaderElection && opts.LeaderElectionNamespace == "" {
		return nil, fmt.Errorf("leader-election namespace is required when election is enabled")
	}
	config = rest.CopyConfig(config)
	endpoint, _, err := rest.DefaultServerUrlFor(config)
	if err != nil {
		return nil, fmt.Errorf("resolve management API endpoint: %w", err)
	}
	// All clients, including discovery and leader election, inherit this guard.
	config.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &originTransport{RoundTripper: rt, origin: apiOrigin(endpoint)}
	})

	scheme := runtime.NewScheme()
	builder := runtime.NewSchemeBuilder(kubescheme.AddToScheme, nkev1.AddToScheme, clusterv1.AddToScheme)
	if err := builder.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register management API scheme: %w", err)
	}
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme, Logger: opts.Logger,
		Cache: cache.Options{
			ReaderFailOnMissingInformer: true,
			DefaultTransform:            cache.TransformStripManagedFields(),
		},
		LeaderElection: opts.LeaderElection, LeaderElectionID: LeaderElectionID,
		LeaderElectionNamespace: opts.LeaderElectionNamespace,
		// Avoid releasing leadership early while application work may still be stopping.
		LeaderElectionReleaseOnCancel: false,
		Metrics:                       metricsserver.Options{BindAddress: opts.MetricsBindAddress},
		HealthProbeBindAddress:        opts.HealthProbeBindAddress,
	})
	if err != nil {
		return nil, fmt.Errorf("construct management controller manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return nil, fmt.Errorf("register health check: %w", err)
	}
	state := &readiness{cache: mgr.GetCache()}
	if err := mgr.Add(state); err != nil {
		return nil, fmt.Errorf("register readiness lifecycle: %w", err)
	}
	if err := mgr.AddReadyzCheck("startup", state.check); err != nil {
		return nil, fmt.Errorf("register readiness check: %w", err)
	}
	return mgr, nil
}

type readiness struct {
	cache   cache.Cache
	started atomic.Bool
}

func (r *readiness) NeedLeaderElection() bool { return false }

func (r *readiness) Start(ctx context.Context) error {
	r.started.Store(true)
	<-ctx.Done()
	r.started.Store(false)
	return nil
}

func (r *readiness) check(req *http.Request) error {
	if !r.started.Load() {
		return fmt.Errorf("manager has not started or is shutting down")
	}
	ctx, cancel := context.WithTimeout(req.Context(), time.Second)
	defer cancel()
	if !r.cache.WaitForCacheSync(ctx) {
		return fmt.Errorf("registered resource caches have not synchronized")
	}
	return nil
}
