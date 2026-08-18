// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"go.temporal.io/sdk/worker"
	"go.uber.org/automaxprocs/maxprocs"
	"golang.org/x/sync/errgroup"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/controllers"
	"github.com/NVIDIA/nke/controller/internal/controllers/capi"
	"github.com/NVIDIA/nke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/NVIDIA/nke/controller/internal/controllers/nodepool"
	"github.com/NVIDIA/nke/controller/internal/controllers/nodeprovisioningmonitor"
	"github.com/NVIDIA/nke/controller/internal/controllers/nodeset"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/db/migrations"
	"github.com/NVIDIA/nke/controller/internal/dbmetrics"
	"github.com/NVIDIA/nke/controller/internal/kube"
	"github.com/NVIDIA/nke/controller/internal/metrics"
	"github.com/NVIDIA/nke/controller/internal/otelx"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	"github.com/NVIDIA/nke/controller/internal/server"
	"github.com/NVIDIA/nke/controller/internal/spiffe"
	"github.com/NVIDIA/nke/controller/internal/teleport"
	"github.com/NVIDIA/nke/controller/internal/temporal"
	"github.com/NVIDIA/nke/controller/internal/version"
)

const (
	ServerGracefulShutdownTimeout = 30 * time.Second
)

//nolint:gocyclo,maintidx // This isn't real complexity, it's just a lot of "exit if error"
func main() {
	// Before ParseConfigFromEnv, so --version answers without a configured env.
	versionFlag := flag.Bool("version", false, "Print the version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Fprintf(os.Stdout, "cloud-workflow-engine %s\n", version.Version)
		os.Exit(0)
	}

	cfg, err := config.ParseConfigFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading configuration: %v\n", err)
		os.Exit(1)
	}

	logLevel := slog.LevelInfo
	if err := logLevel.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing configured log level: %v\n", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))

	if _, err := maxprocs.Set(maxprocs.Logger(func(message string, args ...interface{}) {
		logger.Info(fmt.Sprintf(message, args...))
	})); err != nil {
		logger.Error("Could not set GOMAXPROCS", "error", err)
	}

	logger.Info("Starting cloud-workflow-engine", "version", version.Version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	otelShutdown, err := otelx.Init(ctx, cfg.Tracing, logger.With("logger", "otel"))
	if err != nil {
		logger.Error("Failed to initialize OpenTelemetry tracing", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), ServerGracefulShutdownTimeout)
		defer cancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			logger.Error("Error during OpenTelemetry shutdown", "error", err)
		}
	}()

	// Get cloud-workflow-engine workload trust domain and bundle
	trustDomain, err := getSpiffeTrustDomain(ctx, cfg, logger)
	if err != nil {
		logger.Error("Failed to get SPIFFE trust domain and bundle", "error", err)
		os.Exit(1)
	}

	// Connect to the database, exit on fail.
	dbLogger := logger.With("logger", "db")
	bunDB, err := db.NewBunDB(cfg, dbLogger)
	if err != nil {
		logger.Error("Could not connect to database", "error", err)
		os.Exit(1)
	}

	defer func() {
		if err = bunDB.Close(); err != nil {
			logger.Error("Could not close database", "error", err)
		}
	}()

	// Run any new DB migrations
	if err := migrations.Migrate(ctx, bunDB); err != nil {
		logger.Error("Failed to migrate database", "error", err)
		os.Exit(1)
	}

	dbInstance := dbwrapper.New(db.NewDB(bunDB), dbLogger)

	dbMetricsLogger := logger.With("logger", "db-metrics")
	dbMetricsCollector := dbmetrics.NewCollector(dbMetricsLogger, dbInstance)
	if err := prometheus.Register(dbMetricsCollector); err != nil {
		logger.Error("Failed to register db metrics collector", "error", err)
		os.Exit(1)
	}

	teleportCollector := metrics.NewTeleportCollector(
		logger.With("logger", "teleport-metrics"), dbInstance,
	)
	if err := prometheus.Register(teleportCollector); err != nil {
		logger.Error("Failed to register Teleport metrics collector", "error", err)
		os.Exit(1)
	}

	forgeProviderLogger := logger.With("logger", "forge-provider")
	forgeProvider := forge.NewForgeProvider(cfg, dbInstance, forgeProviderLogger)

	managementK8sClient, err := kube.NewRuntimeClient(spirev1alpha1.AddToScheme, nicov1.AddToScheme)
	if err != nil {
		logger.Error("Failed to create management-cluster Kubernetes client", "error", err)
		os.Exit(1)
	}

	controllerRuntimeLogger := logger.With("logger", "controller-runtime")
	k8sControllerManager, err := kube.NewRuntimeManager(controllerRuntimeLogger, spirev1alpha1.AddToScheme, nicov1.AddToScheme)
	if err != nil {
		logger.Error("Failed to create Kubernetes controller manager", "error", err)
		os.Exit(1)
	}

	nodeSetController := nodeset.NewController(
		cfg,
		logger.With("logger", "node-set-controller"),
		dbInstance,
	)
	if err := nodeSetController.SetupWithManager(k8sControllerManager); err != nil {
		logger.Error("Failed to set up node set controller", "error", err)
		os.Exit(1)
	}

	enableNodeProvisioningMonitor := cfg.NodeProvisioningTimeout > 0
	if enableNodeProvisioningMonitor {
		nodeProvisioningMonitorController := nodeprovisioningmonitor.NewController(
			cfg,
			logger.With("logger", "node-provisioning-monitor"),
			dbInstance,
		)
		if err := nodeProvisioningMonitorController.SetupWithManager(k8sControllerManager); err != nil {
			logger.Error("Failed to set up node provisioning monitor controller", "error", err)
			os.Exit(1)
		}
	}

	nodePoolController := nodepool.NewController(
		cfg,
		logger.With("logger", "node-pool-controller"),
		dbInstance,
	)
	if err := nodePoolController.SetupWithManager(k8sControllerManager); err != nil {
		logger.Error("Failed to set up node pool controller", "error", err)
		os.Exit(1)
	}

	logger.Info("controller-runtime manager configuration", "enableCapi", cfg.EnableCapi, "enableForgeCapi", cfg.EnableForgeCapi, "enableNodeProvisioningMonitor", enableNodeProvisioningMonitor)
	if cfg.EnableCapi {
		machineSpireEntryController := capi.NewMachineSpireEntryController(
			cfg,
			k8sControllerManager.GetClient(),
			logger.With("logger", "machine-spire-entry-controller"),
			trustDomain,
		)
		if err := machineSpireEntryController.SetupWithManager(k8sControllerManager); err != nil {
			logger.Error("Failed to set up machine SPIRE entry controller", "error", err)
			os.Exit(1)
		}

		if cfg.EnableForgeCapi {
			nicoTpmSpiffeController := capi.NewNicoTpmSpiffeController(
				cfg,
				k8sControllerManager.GetClient(),
				logger.With("logger", "nico-tpm-spiffe-controller"),
			)
			if err := nicoTpmSpiffeController.SetupWithManager(k8sControllerManager); err != nil {
				logger.Error("Failed to set up nico TPM SPIFFE controller", "error", err)
				os.Exit(1)
			}

			nicoProviderIDController := capi.NewNicoProviderIDController(
				k8sControllerManager.GetClient(),
				kube.NewCAPIKubeconfigTenantClientProvider(k8sControllerManager.GetClient()),
				logger.With("logger", "nico-provider-id-controller"),
			)
			if err := nicoProviderIDController.SetupWithManager(k8sControllerManager); err != nil {
				logger.Error("Failed to set up nico provider ID controller", "error", err)
				os.Exit(1)
			}
		}
	}

	var teleportClient teleport.Client
	if cfg.Teleport.Enabled {
		teleportLogger := logger.With("logger", "teleport")
		var err error
		teleportClient, err = teleport.NewClient(ctx, cfg.Teleport, teleport.WithLogger(teleportLogger))
		if err != nil {
			logger.Warn("Teleport client not available, continuing without Teleport", "error", err)
		} else {
			defer teleportClient.Close()
			logger.Info("Teleport client initialized", "proxy", cfg.Teleport.ProxyAddress)
		}
	} else {
		logger.Info("Teleport integration disabled")
	}

	serverLogger := logger.With("logger", "server")
	temporalLogger := logger.With("logger", "temporal")
	if err := temporal.RegisterNamespace(ctx, cfg, temporalLogger); err != nil {
		logger.Error("Failed to register Temporal namespace", "error", err)
		os.Exit(1)
	}

	temporalClient, err := temporal.NewClient(cfg, temporalLogger)
	if err != nil {
		logger.Error("Failed to create Temporal client", "error", err)
		os.Exit(1)
	}
	defer temporalClient.Close()

	httpServer, err := server.NewServer(cfg, serverLogger, dbInstance, forgeProvider, temporalClient)
	if err != nil {
		logger.Error("Failed to construct HTTP server", "error", err)
		os.Exit(1)
	}

	metricsServer := server.NewMetricsServer(cfg)

	nodeAgentClient := agent.NewClient(cfg, trustDomain)
	tenantClientProvider := kube.NewDBClusterTenantClientProvider(
		dbInstance,
		managementK8sClient,
		nodeAgentClient,
		cfg.TenantAccessEnabled,
	)

	temporalWorker, err := temporal.NewWorker(
		cfg,
		temporalLogger,
		temporalClient,
		dbInstance,
		managementK8sClient,
		nodeAgentClient,
		forgeProvider,
		trustDomain,
		tenantClientProvider,
	)
	if err != nil {
		logger.Error("Failed to create Temporal worker", "error", err)
		os.Exit(1)
	}

	outboxLogger := logger.With("logger", "outbox-processor")
	outboxProcessor := temporal.NewOutboxProcessor(cfg, outboxLogger, dbInstance, temporalClient)

	// Create ControllerManager (creates all controllers internally)
	controllerManagerLogger := logger.With("logger", "controller-manager")
	controllerManager, err := controllers.NewControllerManager(
		cfg,
		controllerManagerLogger,
		dbInstance,
		managementK8sClient,
		temporalClient,
		forgeProvider,
		nodeAgentClient,
		tenantClientProvider,
		trustDomain,
		teleportClient,
	)
	if err != nil {
		logger.Error("Failed to create controller-manager", "error", err)
		os.Exit(1)
	}

	// Use errgroup to manage all services
	var g errgroup.Group

	// Start the HTTP server in a goroutine
	g.Go(func() error {
		logger.Info("Starting HTTP server")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server encountered an error", "error", err)
			return err
		}
		return nil
	})

	// Start the metrics server in a goroutine
	g.Go(func() error {
		logger.Info("Starting metrics server", "addr", cfg.MetricsListenAddress)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Metrics server encountered an error", "error", err)
			stop()
			return err
		}
		return nil
	})

	// Start the Temporal worker in a goroutine
	g.Go(func() error {
		logger.Info("Starting Temporal worker")
		err := temporalWorker.Run(worker.InterruptCh())
		if err != nil {
			logger.Error("Temporal worker stopped unexpectedly", "error", err)
			// Cancel root context so the shutdown goroutine runs HTTP Shutdown and stops
			// other services; otherwise errgroup.Wait would block forever on ListenAndServe.
			stop()
		}
		return err
	})

	// Start the workflow outbox processor
	g.Go(func() error {
		logger.Info("Starting workflow outbox processor")
		outboxProcessor.Start(ctx)
		return nil
	})

	// Start the controller-manager
	if !cfg.ControllerManagerDisabled {
		g.Go(func() error {
			logger.Info("Starting controller-manager")
			controllerManager.Start(ctx)
			return nil
		})
	}

	g.Go(func() error {
		logger.Info("Starting Kubernetes controller-runtime manager")
		err := k8sControllerManager.Start(ctx)
		if err != nil {
			logger.Error("Kubernetes controller-runtime manager stopped unexpectedly", "error", err)
			// Cancel root context so the shutdown goroutine runs HTTP Shutdown and stops
			// other services; otherwise errgroup.Wait would block forever on ListenAndServe.
			stop()
		}
		return err
	})

	// Start the forge provider cache engine
	g.Go(func() error {
		if cfg.FWEAPIKey == "" {
			logger.Warn("Forge API key is not set, skipping forge provider cache engine")
			return nil
		}
		logger.Info("Starting forge provider cache engine")
		forgeProvider.GetCacheEngine().Start(ctx)
		return nil
	})

	// Wait for context cancellation or an error from any service
	go func() {
		<-ctx.Done()
		logger.Info("Shutdown signal received, starting graceful shutdown")

		// Create a deadline to wait for shutdown
		shutdownCtx, cancel := context.WithTimeout(context.Background(), ServerGracefulShutdownTimeout)
		defer cancel()

		// Shutdown the HTTP server
		//nolint:contextcheck // this is a shutdown context
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("Error during HTTP server shutdown", "error", err)
		} else {
			logger.Info("HTTP server shut down gracefully")
		}

		// Shutdown the metrics server
		//nolint:contextcheck // this is a shutdown context
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("Error during metrics server shutdown", "error", err)
		} else {
			logger.Info("Metrics server shut down gracefully")
		}

		// Stop the Temporal worker
		temporalWorker.Stop()
		logger.Info("Temporal worker shut down gracefully")

		// Stop the workflow outbox processor
		outboxProcessor.Stop()
		logger.Info("Workflow outbox processor shut down gracefully")

		// Stop the forge provider cache engine
		forgeProvider.GetCacheEngine().Stop()
		logger.Info("Forge provider cache engine shut down gracefully")
	}()

	// Wait for all goroutines to complete
	if err := g.Wait(); err != nil {
		logger.Error("Service error occurred", "error", err)
		os.Exit(1)
	}
}

func getSpiffeTrustDomain(ctx context.Context, cfg config.Config, logger *slog.Logger) (string, error) {
	if !cfg.SpiffeEnabled {
		logger.Info("SPIFFE is disabled, using empty trust domain")
		return "", nil
	}

	// Give the SPIRE agent a chance to get a valid svid on initial pod deployment, before restarting.
	// Before this timeout, we would often observe the initial pod deployment fail with "no identity issued" errors, for 1-2 times before succeeding.
	startupCtx, cancel := context.WithTimeout(ctx, cfg.SpireAgentStartupTimeout)
	defer cancel()

	var lastErr error
	for {
		trustDomain, _, err := spiffe.GetTrustDomainAndBundle(startupCtx, cfg)
		if err == nil {
			logger.Info("SPIFFE trust domain", "trustDomain", trustDomain)
			return trustDomain, nil
		}

		lastErr = err
		logger.Warn("SPIFFE identity not ready yet; retrying", "error", err)

		select {
		case <-startupCtx.Done():
			return "", lastErr
		case <-time.After(cfg.SpireAgentStartupRetryInterval):
		}
	}
}
