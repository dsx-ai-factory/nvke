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

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"
	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"

	"github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/automode"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/capi"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/certificates"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/certrenewal"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/controlplane"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/controlplanenodescheduler"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/nodeagentreadiness"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/nodepool"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/nodepoolnodescheduler"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/nodeprovisioningmonitor"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/nodeset"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/teleporttoken"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/tenant"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/tenant/csr"
	tenantnoderemediation "github.com/dsx-ai-factory/nvke/controller/internal/controllers/tenant/noderemediation"
	tenantnodestatus "github.com/dsx-ai-factory/nvke/controller/internal/controllers/tenant/nodestatus"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/tenantaccess"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/tpmpubhashes"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/db/migrations"
	"github.com/dsx-ai-factory/nvke/controller/internal/dbmetrics"
	"github.com/dsx-ai-factory/nvke/controller/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/internal/machinerelease"
	"github.com/dsx-ai-factory/nvke/controller/internal/metrics"
	"github.com/dsx-ai-factory/nvke/controller/internal/otelx"
	"github.com/dsx-ai-factory/nvke/controller/internal/runbook"
	"github.com/dsx-ai-factory/nvke/controller/internal/server"
	"github.com/dsx-ai-factory/nvke/controller/internal/spiffe"
	"github.com/dsx-ai-factory/nvke/controller/internal/teleport"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal"
	"github.com/dsx-ai-factory/nvke/controller/internal/version"
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
		fmt.Fprintf(os.Stdout, "controller %s\n", version.Version)
		os.Exit(0)
	}

	cfg, controllerCfg, err := controllerconfig.LoadStartupConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading configuration: %v\n", err)
		os.Exit(1)
	}

	logLevel := slog.LevelInfo
	if err := logLevel.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing configured log level: %v\n", err)
		os.Exit(1)
	}
	logger := slog.New(otelx.LogHandler{Handler: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:       logLevel,
		ReplaceAttr: otelx.NormalizeLogAttr,
	})})

	if cfg.RunbooksDir != "" {
		if err := runbook.ValidateProfileReferences(cfg.ProfilesDir, cfg.EffectiveRunbooksDir()); err != nil {
			logger.Error("Invalid config-profile runbook bundle", "error", err)
			os.Exit(1)
		}
	}

	if _, err := maxprocs.Set(maxprocs.Logger(func(message string, args ...interface{}) {
		logger.Info(fmt.Sprintf(message, args...))
	})); err != nil {
		logger.Error("Could not set GOMAXPROCS", "error", err)
	}

	logger.Info("Starting controller", "version", version.Version)

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

	logger.Info("Loaded nke-controller-config", "path", cfg.ControllerConfigPath, "oidcProviders", len(controllerCfg.OIDC.Providers))

	authenticator, err := authn.Setup(ctx, authn.Options{
		ConfigPath: cfg.AuthConfigPath,
		Permissive: cfg.PermissiveMode,
		Recorder:   metrics.RecordAuthRequest,
		Config:     controllerCfg.APIAuth.AuthConfig(),
	}, logger)
	if err != nil {
		logger.Error("Failed to set up API authentication", "error", err)
		os.Exit(1)
	}

	metrics.SetAuthMode(authMode(authenticator, cfg.PermissiveMode))

	// Get cloud-workflow-engine workload trust domain and bundle
	trustDomain, err := getSpiffeTrustDomain(ctx, cfg, logger)
	if err != nil {
		logger.Error("Failed to get SPIFFE trust domain and bundle", "error", err)
		os.Exit(1)
	}
	// One transport for the whole worker: the node-agent client and the rollout
	// subsystem must reach agents over the same authenticated connection.
	nodeAgentTransport, err := agent.NewTransport(ctx, cfg, trustDomain, logger)
	if err != nil {
		logger.Error("Failed to create node-agent transport", "error", err)
		os.Exit(1)
	}
	nodeAgentClient := agent.NewClientWithTransport(nodeAgentTransport)

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
	// Note: cfg.ProfilesDir feeds the config profile label backfill (20261016120000)
	if err := migrations.Migrate(ctx, bunDB, cfg.ProfilesDir); err != nil {
		logger.Error("Failed to migrate database", "error", err)
		os.Exit(1)
	}

	// Re-parent legacy node-pool-managed control planes onto the control-plane
	// controller. Fails closed on a half-migrated fleet.
	if err := db.MigrateControlPlaneToController(ctx, bunDB, dbLogger); err != nil {
		logger.Error("Failed to migrate control plane to controller", "error", err)
		os.Exit(1)
	}

	dbInstance := dbwrapper.New(db.NewDB(bunDB), dbLogger)

	dbMetricsLogger := logger.With("logger", "db-metrics")
	dbMetricsCollector := dbmetrics.NewCollector(dbMetricsLogger, dbInstance)
	credentialMetricsCollector := dbmetrics.NewCredentialCollector(dbMetricsLogger, dbInstance, cfg)
	if err := prometheus.Register(dbMetricsCollector); err != nil {
		logger.Error("Failed to register db metrics collector", "error", err)
		os.Exit(1)
	}

	if err := prometheus.Register(credentialMetricsCollector); err != nil {
		logger.Error("Failed to register credential metrics collector", "error", err)
		os.Exit(1)
	}
	teleportCollector := metrics.NewTeleportCollector(
		logger.With("logger", "teleport-metrics"), dbInstance,
	)
	if err := prometheus.Register(teleportCollector); err != nil {
		logger.Error("Failed to register Teleport metrics collector", "error", err)
		os.Exit(1)
	}

	managementK8sClient, err := kube.NewRuntimeClient(spirev1alpha1.AddToScheme, nicov1.AddToScheme, enginev1.AddToScheme)
	if err != nil {
		logger.Error("Failed to create management-cluster Kubernetes client", "error", err)
		os.Exit(1)
	}
	if err := machinerelease.CheckCRDs(ctx, managementK8sClient); err != nil {
		logger.Error("Failed the release CRD startup check", "error", err)
		os.Exit(1)
	}

	controllerRuntimeLogger := logger.With("logger", "controller-runtime")
	k8sControllerManager, err := kube.NewRuntimeManager(
		controllerRuntimeLogger,
		kube.RuntimeManagerOptions{
			ConfigMapCacheNamespace: cfg.SpireServerNamespace,
			LeaderElectionNamespace: cfg.PodNamespace,
			SecretCacheNamespace:    cfg.ManagementNamespaceOrDefault(),
		},
		spirev1alpha1.AddToScheme,
		nicov1.AddToScheme,
		enginev1.AddToScheme,
	)
	if err != nil {
		logger.Error("Failed to create Kubernetes controller manager", "error", err)
		os.Exit(1)
	}
	tenantClientProvider := kube.NewDBClusterTenantClientProvider(
		dbInstance,
		managementK8sClient,
		nodeAgentClient,
		cfg.TenantAccessEnabled,
		cfg.ManagementNamespaceOrDefault(),
	)

	if cfg.SpiffeEnabled {
		tpmPubhashesCmController := tpmpubhashes.NewController(
			cfg,
			logger.With("logger", "tpm-pubhashes-configmap-controller"),
			k8sControllerManager.GetClient(),
			trustDomain,
		)
		if err := tpmPubhashesCmController.SetupWithManager(k8sControllerManager); err != nil {
			logger.Error("Failed to set up TPM pubhashes ConfigMap controller", "error", err)
			os.Exit(1)
		}
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

	if !cfg.NodeHealthCheckDisabled {
		nodeAgentHealthController := nodeagentreadiness.NewAgentReadinessController(
			cfg,
			logger.With("logger", "node-agent-health-controller"),
			dbInstance,
			nodeAgentClient,
		)
		if err := nodeAgentHealthController.SetupWithManager(k8sControllerManager); err != nil {
			logger.Error("Failed to set up node agent health controller", "error", err)
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

	autoModeController := automode.NewController(
		cfg,
		logger.With("logger", "auto-mode-controller"),
		dbInstance,
	)
	if err := autoModeController.SetupWithManager(k8sControllerManager); err != nil {
		logger.Error("Failed to set up auto-mode controller", "error", err)
		os.Exit(1)
	}

	logger.Info("Kubernetes controller-runtime manager configuration",
		"enableNodeProvisioningMonitor", enableNodeProvisioningMonitor,
		"enableNicoControllers", cfg.NicoControllersEnabled,
		"enableSpiffe", cfg.SpiffeEnabled,
		"enableTenantAccess", cfg.TenantAccessEnabled)

	// CAPI Machine CRD is required for nvke
	machineCRDInstalled, err := capi.MachineCRDInstalled(k8sControllerManager.GetRESTMapper())
	if err != nil {
		logger.Error("Failed to look up the Machine CRD", "error", err)
		os.Exit(1)
	}
	if !machineCRDInstalled {
		logger.Error("Required CAPI Machine CRD is not installed; install Cluster API before starting the NVKE controller")
		os.Exit(1)
	}

	machineSpireEntryController := capi.NewMachineSpireEntryController(
		cfg,
		k8sControllerManager.GetClient(),
		dbInstance,
		logger.With("logger", "machine-spire-entry-controller"),
		trustDomain,
	)
	if err := machineSpireEntryController.SetupWithManager(k8sControllerManager); err != nil {
		logger.Error("Failed to set up machine SPIRE entry controller", "error", err)
		os.Exit(1)
	}

	// Writes the SPIRE agent ID path MachineSpireEntryController consumes, so it
	// is only useful alongside that controller. Gated on CAPLL's CRD rather than
	// supportedInfraProviders: dropping launchLayer from that list stops new
	// clusters, while existing clusters' new and replacement machines still need
	// their agent path to join.
	launchLayerCRDInstalled, err := capi.LaunchLayerMachineCRDInstalled(k8sControllerManager.GetRESTMapper())
	if err != nil {
		logger.Error("Failed to look up the LaunchLayerMachine CRD", "error", err)
		os.Exit(1)
	}
	if launchLayerCRDInstalled {
		launchLayerSpiffeController := capi.NewLaunchLayerSpiffeController(
			dbInstance,
			k8sControllerManager.GetClient(),
			logger.With("logger", "launchlayer-spiffe-controller"),
		)
		if err := launchLayerSpiffeController.SetupWithManager(k8sControllerManager); err != nil {
			logger.Error("Failed to set up LaunchLayer SPIFFE controller", "error", err)
			os.Exit(1)
		}
	} else {
		logger.Info("LaunchLayerMachine CRD not installed; skipping the LaunchLayer SPIFFE controller")
	}

	if cfg.NicoControllersEnabled {
		nicoCRDInstalled, err := capi.NicoMachineCRDInstalled(k8sControllerManager.GetRESTMapper())
		if err != nil {
			logger.Error("Failed to look up the NicoMachine CRD", "error", err)
			os.Exit(1)
		}
		if !nicoCRDInstalled {
			logger.Error("Required NicoMachine CRD is not installed while NICo controllers are enabled; install CAPNICo first, or set controllers.nico.enabled=false")
			os.Exit(1)
		}

		nicoSpiffeController := capi.NewNicoSpiffeController(
			cfg,
			dbInstance,
			k8sControllerManager.GetClient(),
			logger.With("logger", "nico-spiffe-controller"),
		)
		if err := nicoSpiffeController.SetupWithManager(k8sControllerManager); err != nil {
			logger.Error("Failed to set up nico SPIFFE controller", "error", err)
			os.Exit(1)
		}

		nicoProviderIDController := capi.NewNicoProviderIDController(
			k8sControllerManager.GetClient(),
			tenantClientProvider,
			logger.With("logger", "nico-provider-id-controller"),
		)
		if err := nicoProviderIDController.SetupWithManager(k8sControllerManager); err != nil {
			logger.Error("Failed to set up nico provider ID controller", "error", err)
			os.Exit(1)
		}
	} else {
		logger.Info("NICo controllers disabled by configuration")
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

	if cfg.Teleport.Enabled && teleportClient != nil {
		teleportTokenController := teleporttoken.NewController(
			cfg,
			dbInstance,
			k8sControllerManager.GetClient(),
			logger.With("logger", "teleport-token-controller"),
			teleportClient,
		)
		if err := teleportTokenController.SetupWithManager(k8sControllerManager); err != nil {
			logger.Error("Failed to set up Teleport token controller", "error", err)
			os.Exit(1)
		}
	}

	temporalLogger := logger.With("logger", "temporal")
	// Retry before giving up. Temporal restarting for a few seconds used to be
	// enough to exit here, and once the process is in CrashLoopBackOff the
	// restart delay grows to five minutes -- so a dependency that was away for
	// seconds kept the controller down for minutes. Retry for a bounded window instead.
	if err := registerTemporalNamespaceWithRetry(ctx, cfg, temporalLogger, logger); err != nil {
		logger.Error("Failed to register Temporal namespace", "error", err)
		os.Exit(1)
	}

	temporalClient, err := temporal.NewClient(cfg, temporalLogger)
	if err != nil {
		logger.Error("Failed to create Temporal client", "error", err)
		os.Exit(1)
	}
	defer temporalClient.Close()

	tenantRegistrars := []tenant.Registrar{
		csr.NewRegistrar(dbInstance, logger.With("logger", "tenant-csr-controller")),
	}
	if cfg.CredentialMonitoringEnabled {
		tenantRegistrars = append(tenantRegistrars, csr.NewObserverRegistrar(dbInstance, logger.With("logger", "tenant-csr-observer")))
	}
	if !cfg.NodeHealthCheckDisabled {
		tenantRegistrars = append(tenantRegistrars,
			tenantnodestatus.NewRegistrar(dbInstance, logger.With("logger", "tenant-node-health-controller")),
			tenantnoderemediation.NewRegistrar(
				cfg,
				dbInstance,
				temporalClient,
				logger.With("logger", "tenant-node-remediation-controller"),
			),
		)
	}
	tenantManagerSet, err := tenant.NewManagerSet(
		ctx,
		logger.With("logger", "tenant-controller-manager-set"),
		tenant.NewManagerFactory(
			logger.With("logger", "tenant-controller-runtime"),
			tenantRegistrars...,
		),
		tenant.DefaultManagerSetOptions(),
	)
	if err != nil {
		logger.Error("Failed to create tenant controller manager set", "error", err)
		os.Exit(1)
	}
	defer tenantManagerSet.Shutdown()

	tenantAccessController := tenantaccess.NewController(
		cfg,
		logger.With("logger", "tenant-access"),
		dbInstance,
		managementK8sClient,
		nodeAgentClient,
		tenantManagerSet,
	)
	if err := tenantAccessController.SetupWithManager(k8sControllerManager); err != nil {
		logger.Error("Failed to set up tenant access controller", "error", err)
		os.Exit(1)
	}

	tenantSecretReconciler := tenant.NewSecretReconciler(
		dbInstance,
		k8sControllerManager.GetClient(),
		tenantManagerSet,
		cfg.ManagedByLabelValue,
		cfg.ManagementNamespaceOrDefault(),
	)
	if err := tenantSecretReconciler.SetupWithManager(k8sControllerManager); err != nil {
		logger.Error("Failed to set up tenant manager Secret reconciler", "error", err)
		os.Exit(1)
	}

	serverLogger := logger.With("logger", "server")

	logger.Info(
		"controller-runtime manager configuration",
		"enableNodeProvisioningMonitor", enableNodeProvisioningMonitor,
	)

	controlPlaneController := controlplane.NewController(
		cfg,
		logger.With("logger", "control-plane-controller"),
		dbInstance,
		temporalClient,
		k8sControllerManager.GetClient(),
		managementK8sClient,
		nodeAgentClient,
	)
	if err := controlPlaneController.SetupWithManager(k8sControllerManager); err != nil {
		logger.Error("Failed to set up control plane controller", "error", err)
		os.Exit(1)
	}

	nodePoolNodeScheduler := nodepoolnodescheduler.NewNodePoolNodeScheduler(
		cfg,
		logger.With("logger", "node-pool-node-scheduler"),
		dbInstance,
		temporalClient,
	)
	if err := nodePoolNodeScheduler.SetupWithManager(k8sControllerManager); err != nil {
		logger.Error("Failed to set up node pool node scheduler", "error", err)
		os.Exit(1)
	}

	controlPlaneNodeScheduler := controlplanenodescheduler.NewControlPlaneNodeScheduler(
		cfg,
		logger.With("logger", "control-plane-node-scheduler"),
		dbInstance,
		temporalClient,
	)
	if err := controlPlaneNodeScheduler.SetupWithManager(k8sControllerManager); err != nil {
		logger.Error("Failed to set up control plane node scheduler", "error", err)
		os.Exit(1)
	}

	if cfg.CredentialMonitoringEnabled {
		if err := certificates.NewController(cfg, dbInstance, nodeAgentClient).SetupWithManager(k8sControllerManager); err != nil {
			logger.Error("Failed to setup certificate inspection controller", "error", err)
			os.Exit(1)
		}
	}
	if cfg.CertRenewalControllerEnabled {
		certRenewalController := certrenewal.NewController(
			cfg,
			logger.With("logger", "cert-renewal-controller"),
			dbInstance,
			temporalClient,
		)
		if err := certRenewalController.SetupWithManager(k8sControllerManager); err != nil {
			logger.Error("Failed to set up cert renewal controller", "error", err)
			os.Exit(1)
		}
	} else {
		logger.Info("Cert renewal controller disabled")
	}

	httpServer, err := server.NewServer(
		cfg,
		serverLogger,
		dbInstance,
		temporalClient,
		authenticator,
		managementK8sClient,
		server.WithFleetOIDC(&controllerCfg.OIDC),
		server.WithAllowLegacyCalicoClusterCreation(controllerCfg.AllowLegacyCalicoClusterCreation),
		server.WithNodeAgentClient(nodeAgentClient),
	)
	if err != nil {
		logger.Error("Failed to construct HTTP server", "error", err)
		os.Exit(1)
	}

	metricsServer := server.NewMetricsServer(cfg)

	temporalWorker, err := temporal.NewWorker(
		cfg,
		temporalLogger,
		temporalClient,
		dbInstance,
		managementK8sClient,
		nodeAgentClient,
		nodeAgentTransport,
		trustDomain,
		tenantClientProvider,
	)
	if err != nil {
		logger.Error("Failed to create Temporal worker", "error", err)
		os.Exit(1)
	}

	outboxLogger := logger.With("logger", "outbox-processor")
	outboxProcessor := temporal.NewOutboxProcessor(cfg, outboxLogger, dbInstance, temporalClient)

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
	}()

	// Wait for all goroutines to complete
	if err := g.Wait(); err != nil {
		logger.Error("Service error occurred", "error", err)
		os.Exit(1)
	}
}

// authMode is the nvke_auth_enabled mode label. Setup builds no
// authenticator exactly when authentication is off.
func authMode(authenticator *authn.Authenticator, permissive bool) string {
	switch {
	case authenticator == nil:
		return "off"
	case permissive:
		return "permissive"
	default:
		return "enforcing"
	}
}

// registerTemporalNamespaceWithRetry retries namespace registration for a
// bounded window so a brief Temporal outage does not take the worker down for
// far longer than the outage itself lasted.
func registerTemporalNamespaceWithRetry(ctx context.Context, cfg config.Config, temporalLogger, logger *slog.Logger) error {
	const (
		attempts = 12
		delay    = 5 * time.Second
	)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		err := temporal.RegisterNamespace(ctx, cfg, temporalLogger)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == attempts {
			break
		}
		logger.Warn("Temporal namespace registration failed, retrying",
			"attempt", attempt, "of", attempts, "retryIn", delay, "error", lastErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return lastErr
}

func getSpiffeTrustDomain(ctx context.Context, cfg config.Config, logger *slog.Logger) (string, error) {
	if !cfg.SpiffeEnabled {
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
