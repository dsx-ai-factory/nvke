// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package temporal

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/uber-go/tally/v4"
	tprometheus "github.com/uber-go/tally/v4/prometheus"
	sdktally "go.temporal.io/sdk/contrib/tally"
	"go.temporal.io/sdk/converter"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/types/known/durationpb"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kube"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	rolloutcontroller "github.com/dsx-ai-factory/nvke/controller/internal/rollout/adapters/controller"
	rolloutkubevip "github.com/dsx-ai-factory/nvke/controller/internal/rollout/guards/kubevip"
	rolloutetcd "github.com/dsx-ai-factory/nvke/controller/internal/rollout/operations/etcd"
	rolloutoidc "github.com/dsx-ai-factory/nvke/controller/internal/rollout/operations/oidc"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout/preflights"
	rollouttemporal "github.com/dsx-ai-factory/nvke/controller/internal/rollout/temporal"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/activities"
	"github.com/dsx-ai-factory/nvke/controller/internal/temporal/workflows"
)

func RegisterNamespace(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	tlsConfig, err := newTLSConfig(cfg.TemporalTLS)
	if err != nil {
		return fmt.Errorf("unable to create Temporal namespace TLS config: %w", err)
	}

	namespaceClient, err := client.NewNamespaceClient(client.Options{
		HostPort: cfg.TemporalHostPort,
		ConnectionOptions: client.ConnectionOptions{
			GetSystemInfoTimeout: 10 * time.Second, // Found this helps when local dev over slow SSH tunnel
			TLS:                  tlsConfig,
		},
	})
	if err != nil {
		return fmt.Errorf("unable to create Temporal namespace client: %w", err)
	}
	defer namespaceClient.Close()

	err = namespaceClient.Register(ctx, &workflowservice.RegisterNamespaceRequest{
		Namespace:                        cfg.TemporalNamespace,
		WorkflowExecutionRetentionPeriod: durationpb.New(time.Hour * 24 * 30),
	})
	if err != nil {
		var namespaceErr *serviceerror.NamespaceAlreadyExists
		if errors.As(err, &namespaceErr) {
			logger.Info("Temporal namespace already exists", "namespace", cfg.TemporalNamespace)
			return nil
		}
		return fmt.Errorf("unable to register Temporal namespace: %w", err)
	}

	return nil
}

func NewClient(cfg config.Config, logger *slog.Logger) (client.Client, error) {
	tlsConfig, err := newTLSConfig(cfg.TemporalTLS)
	if err != nil {
		return nil, fmt.Errorf("unable to create controller Temporal TLS config: %w", err)
	}

	opts := client.Options{
		HostPort:  cfg.TemporalHostPort,
		Namespace: cfg.TemporalNamespace,
		ConnectionOptions: client.ConnectionOptions{
			GetSystemInfoTimeout: 10 * time.Second, // Found this helps when local dev over slow SSH tunnel
			TLS:                  tlsConfig,
		},
		Logger:         log.NewStructuredLogger(logger.With("temporalClient", "cwe")),
		MetricsHandler: sdktally.NewMetricsHandler(newPrometheusScope()), // Adding a metrics handler gets the built in temporal metrics for the controller temporal worker.
		DataConverter:  NewDataConverter(),
	}

	// Always installed: it carries the request's trace context into workflow
	// headers, which puts the trace ID on workflow and activity logs.
	tracingInterceptor, err := newTracingInterceptor()
	if err != nil {
		return nil, err
	}
	opts.Interceptors = []interceptor.ClientInterceptor{tracingInterceptor}

	return client.Dial(opts)
}

func newTLSConfig(cfg config.TemporalTLSConfig) (*tls.Config, error) {
	if cfg == (config.TemporalTLSConfig{}) {
		return nil, nil //nolint:nilnil // nil TLS config preserves plaintext Temporal clients.
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if cfg.ClientCert != "" || cfg.ClientKey != "" {
		if cfg.ClientCert == "" || cfg.ClientKey == "" {
			return nil, errors.New("both client certificate and key must be set")
		}
		cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate and key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	if cfg.CaCert != "" {
		caBytes, err := os.ReadFile(cfg.CaCert)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}
		certPool := x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("failed to append CA certificate to pool")
		}
		tlsConfig.RootCAs = certPool
	}

	if cfg.ServerName != "" {
		tlsConfig.ServerName = cfg.ServerName
	}

	return tlsConfig, nil
}

func NewWorker(
	cfg config.Config,
	_ *slog.Logger,
	temporalClient client.Client,
	dbInstance db.DB,
	k8sClient ctrlclient.Client,
	nodeAgentClient agent.Client,
	nodeAgentTransport *agent.Transport,
	trustDomain string,
	tenantClientProvider *kube.DBClusterTenantClientProvider,
) (worker.Worker, error) {
	controllerClientCABundle := ""
	if cfg.NodeAgentExternalMTLSEnabled {
		caPEM, err := os.ReadFile(config.NodeAgentExternalTLSCAPath)
		if err != nil {
			return nil, fmt.Errorf("read controller client CA bundle: %w", err)
		}
		controllerClientCABundle = string(caPEM)
	}

	w := worker.New(temporalClient, cfg.TemporalTaskQueue, WorkerOptions())

	w.RegisterActivity(activities.NewActivities(
		cfg,
		dbInstance,
		k8sClient,
		nodeAgentClient,
		trustDomain,
		tenantClientProvider,
		controllerClientCABundle,
	))

	w.RegisterWorkflow(workflows.ProvisionCluster)
	w.RegisterWorkflow(workflows.ProvisionNodePool)
	w.RegisterWorkflow(workflows.ProvisionNode)
	w.RegisterWorkflow(workflows.JoinNode)
	w.RegisterWorkflow(workflows.RebootNode)
	w.RegisterWorkflow(workflows.RebootNodeWaitForUptime)
	w.RegisterWorkflow(workflows.TeardownCluster)
	w.RegisterWorkflow(workflows.TeardownNodePool)
	w.RegisterWorkflow(workflows.TeardownNode)
	w.RegisterWorkflow(workflows.PatchNodePool)
	w.RegisterWorkflow(workflows.NodeReconfigureKubeletServer)
	w.RegisterWorkflow(workflows.ApplyK8sNodeCondition)
	w.RegisterWorkflow(workflows.EnableEtcdEncryption)
	w.RegisterWorkflow(workflows.EnableOIDC)
	w.RegisterWorkflow(workflows.UpdateSpireServer)
	w.RegisterWorkflow(workflows.RotateEtcdEncryptionKey)
	w.RegisterWorkflow(workflows.RetireLostEtcdRotationNode)
	w.RegisterWorkflow(workflows.UpgradeControlPlaneAddons)
	w.RegisterWorkflow(workflows.UpdateKubeadmConfigMap)
	w.RegisterWorkflow(workflows.RenewK8sCerts)
	w.RegisterWorkflow(workflows.RenewK8sCertsV2)

	// Rollout subsystem. One Registry backs the activity-side dispatch used by
	// custom workflows such as EnableEtcdEncryption.
	rolloutRegistry := rollout.NewRegistry()
	rolloutLoader := rolloutcontroller.NewLoader(dbInstance, cfg)
	rolloutResolver, err := rolloutcontroller.NewDatabaseNodeResolver(dbInstance)
	if err != nil {
		return nil, fmt.Errorf("construct rollout node resolver: %w", err)
	}
	rolloutDialer, err := rolloutcontroller.NewSPIFFEDialer(nodeAgentTransport, rolloutResolver)
	if err != nil {
		return nil, fmt.Errorf("construct rollout agent dialer: %w", err)
	}
	rolloutMethodChecker, err := rolloutcontroller.NewMethodSupportChecker(nodeAgentTransport, rolloutResolver)
	if err != nil {
		return nil, fmt.Errorf("construct rollout agent method-support checker: %w", err)
	}
	preflights.RegisterDefaults(rolloutRegistry, rolloutDialer)
	rolloutkubevip.RegisterDefaults(rolloutRegistry, rolloutDialer)
	rolloutetcd.RegisterDefaults(rolloutRegistry, rolloutDialer, k8sClient, cfg.PodNamespace)
	rolloutoidc.RegisterDefaults(rolloutRegistry, rolloutDialer, rolloutMethodChecker, k8sClient, cfg.NvkeOIDCAuth, trustDomain, cfg.RBACConfigMapSource())
	rollouttemporal.NewActivities(rolloutLoader, rolloutRegistry).Register(w)
	rollouttemporal.NewClusterLoaderActivity(rolloutLoader, cfg.NodeAgent.Port).Register(w)

	return w, nil
}

func newTracingInterceptor() (interceptor.Interceptor, error) {
	tracingInterceptor, err := opentelemetry.NewTracingInterceptor(opentelemetry.TracerOptions{})
	if err != nil {
		return nil, fmt.Errorf("create Temporal OpenTelemetry tracing interceptor: %w", err)
	}
	return tracingInterceptor, nil
}

// By default, temporal uses the tally library for go instrumentation.
// This sets up a tally scope to report through our prometheus registry and metrics handler.
// Adapted from: https://github.com/temporalio/samples-go/blob/main/metrics/worker/main.go
func newPrometheusScope() tally.Scope {
	// Create a Tally Prometheus reporter using the default Prometheus registry.
	//
	// DefaultTimerType defaults to summary, which only yields _count/_sum and no
	// _bucket series -- making per-activity/workflow latency percentiles
	// (p90/p99) impossible. Report timers as histograms with buckets spanning
	// 100ms to 60min so both fast DB activities (~10ms) and slow node operations
	// (up to 60min) land in a usable bucket. The tally default buckets cap at
	// 10s, which pushes p95/p99 to +Inf for most provisioning activities.
	//
	// This only ADDS `_bucket` series to the existing unprefixed `temporal_*`
	// names; it does not rename any metric. Existing consumers (e.g. the CWE
	// Fleet Health dashboard, whose latency P95 panels query
	// `temporal_*_latency_bucket` and are empty today) are fixed, not broken.
	reporter := tprometheus.NewReporter(tprometheus.Options{
		Registerer:       nil, // Use the default Prometheus registry
		DefaultTimerType: tprometheus.HistogramTimerType,
		DefaultHistogramBuckets: []float64{
			0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600, 1800, 3600,
		},
	})

	// Create a Tally scope to report through our prometheus registry and metrics handler.
	// Apply options to sanitize/separate the metric names for prometheus.
	scope, _ := tally.NewRootScope(tally.ScopeOptions{
		CachedReporter:  reporter,
		Separator:       tprometheus.DefaultSeparator,
		SanitizeOptions: &sdktally.PrometheusSanitizeOptions,
	}, time.Second)

	return scope
}

// NewDataConverter creates a data converter that matches Temporal's default converter.GetDefaultDataConverter()
// but with AllowUnknownFields enabled for protobuf messages to support
// forward/backward compatibility when adding new fields to proto definitions.
func NewDataConverter() converter.DataConverter {
	return converter.NewCompositeDataConverter(
		converter.NewNilPayloadConverter(),
		converter.NewByteSlicePayloadConverter(),
		converter.NewProtoJSONPayloadConverterWithOptions(converter.ProtoJSONPayloadConverterOptions{
			// By default, Temporal uses protojson.Unmarshal (not regular json.Unmarshal) when it detects protobuf message types,
			// and protojson by default has DiscardUnknown: false, which causes it to error on unknown fields.
			// This means that if another producer adds a new field to a proto, the controller
			// will fail to unmarshal the data unless the controller is updated first.
			// This is unexpected, and behaves differently than regular json.Unmarshal, and protobuf.
			//
			// Using the AllowUnknownFields option, we can tell Temporal to ignore unknown fields, for forward/backward compatibility.
			AllowUnknownFields: true,
		}),
		converter.NewJSONPayloadConverter(),
	)
}
