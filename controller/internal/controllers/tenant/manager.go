// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenant

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/dsx-ai-factory/nvke/controller/internal/kube"
)

const (
	tenantManagerQPS          = float32(5)
	tenantManagerBurst        = 10
	tenantManagerProbeTimeout = 5 * time.Second
)

// Registrar installs one tenant-cluster reconciler into a manager. Registrars
// should create only their narrow watches; controller-runtime starts informers
// lazily for the object kinds actually requested by registered controllers.
type Registrar func(clusterID uuid.UUID, manager ctrl.Manager) error

// ManagerFactory builds controller-runtime managers with per-manager listeners
// disabled and conservative tenant API client limits.
type ManagerFactory struct {
	logger     *slog.Logger
	registrars []Registrar
	verify     func(context.Context, *rest.Config, time.Duration) error
}

func NewManagerFactory(logger *slog.Logger, registrars ...Registrar) *ManagerFactory {
	if logger == nil {
		logger = slog.Default()
	}
	return &ManagerFactory{
		logger:     logger,
		registrars: append([]Registrar(nil), registrars...),
		verify:     kube.VerifyRESTConfig,
	}
}

func (f *ManagerFactory) Start(ctx context.Context, target Target) (Runtime, error) {
	restConfig, err := kube.NewRESTConfigFromKubeconfig(target.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parsing tenant kubeconfig: %w", err)
	}
	restConfig.QPS = tenantManagerQPS
	restConfig.Burst = tenantManagerBurst
	restConfig.UserAgent = "nvke-tenant-controller-manager/" + target.ClusterID.String()

	if err := f.verify(ctx, restConfig, tenantManagerProbeTimeout); err != nil {
		return nil, fmt.Errorf("verifying tenant API access: %w", err)
	}

	scheme, err := kube.NewRuntimeScheme()
	if err != nil {
		return nil, err
	}
	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		return nil, fmt.Errorf("creating tenant controller manager: %w", err)
	}
	for _, register := range f.registrars {
		if err := register(target.ClusterID, manager); err != nil {
			return nil, fmt.Errorf("registering tenant controller for cluster %s: %w", target.ClusterID, err)
		}
	}

	runtimeCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- manager.Start(runtimeCtx)
		close(done)
	}()

	cacheSynced := make(chan bool, 1)
	go func() {
		cacheSynced <- manager.GetCache().WaitForCacheSync(runtimeCtx)
	}()

	select {
	case <-ctx.Done():
		cancel()
		<-done
		return nil, ctx.Err()
	case err := <-done:
		cancel()
		if err == nil {
			return nil, fmt.Errorf("tenant controller manager for cluster %s stopped during startup", target.ClusterID)
		}
		return nil, fmt.Errorf("starting tenant controller manager for cluster %s: %w", target.ClusterID, err)
	case synced := <-cacheSynced:
		if !synced {
			cancel()
			<-done
			return nil, fmt.Errorf("tenant controller manager cache for cluster %s did not sync", target.ClusterID)
		}
	}

	f.logger.Debug("Tenant controller manager cache synchronized", "clusterID", target.ClusterID, "clusterName", target.ClusterName)
	return &managerRuntime{cancel: cancel, done: done}, nil
}

type managerRuntime struct {
	// cancel stops the controller-runtime manager and every controller it owns.
	cancel context.CancelFunc
	// done receives manager.Start's result after all controllers have stopped.
	done <-chan error
	// once makes repeated stop requests safe during rotation and shutdown.
	once sync.Once
}

func (r *managerRuntime) Done() <-chan error {
	return r.done
}

func (r *managerRuntime) Stop() {
	r.once.Do(r.cancel)
}
