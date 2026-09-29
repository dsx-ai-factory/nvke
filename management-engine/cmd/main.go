// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	nkev1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nke/management-engine/internal/controller"
	// +kubebuilder:scaffold:imports
)

// leaderElectionID is the Lease name shared by management-engine replicas.
const leaderElectionID = "management-engine.nke.nvidia.com"

var version = "dev"

var errFlagParse = errors.New("invalid flags")

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(kubescheme.AddToScheme(scheme))
	utilruntime.Must(nkev1.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	if err := run(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		if !errors.Is(err, errFlagParse) {
			setupLog.Error(err, "management engine stopped")
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("management-engine", flag.ContinueOnError)
	ctrl.RegisterFlags(flags)
	opts := defaultManagerOptions()
	var contextName string
	var showVersion bool
	var enableHTTP2 bool
	flags.StringVar(&contextName, "context", "", "Management kubeconfig context")
	flags.StringVar(&opts.Metrics.BindAddress, "metrics-bind-address", opts.Metrics.BindAddress, "Metrics address; :8443 for HTTPS, :8080 for HTTP, or 0 to disable")
	flags.BoolVar(&opts.Metrics.SecureServing, "metrics-secure", true, "Serve metrics over HTTPS with authentication and authorization")
	flags.StringVar(&opts.Metrics.CertDir, "metrics-cert-path", "", "Directory containing the metrics certificate")
	flags.StringVar(&opts.Metrics.CertName, "metrics-cert-name", "tls.crt", "Metrics certificate filename")
	flags.StringVar(&opts.Metrics.KeyName, "metrics-cert-key", "tls.key", "Metrics private key filename")
	flags.BoolVar(&enableHTTP2, "enable-http2", false, "Enable HTTP/2 for metrics")
	flags.StringVar(&opts.HealthProbeBindAddress, "health-probe-bind-address", opts.HealthProbeBindAddress, "Health/readiness HTTP address; 0 disables")
	flags.BoolVar(&opts.LeaderElection, "leader-elect", opts.LeaderElection, "Enable leader election; disable only for a sole development process")
	flags.StringVar(&opts.LeaderElectionNamespace, "leader-election-namespace", "", "Existing namespace in the target cluster for the election Lease")
	flags.BoolVar(&showVersion, "version", false, "Print version and exit")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flags)
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errFlagParse, err)
	}
	if showVersion {
		fmt.Println(version)
		return nil
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if enableHTTP2 {
		opts.Metrics.TLSOpts = nil
	}
	cfg, err := config.GetConfigWithContext(contextName)
	if err != nil {
		return fmt.Errorf("load management kubeconfig: %w", err)
	}
	ctx := ctrl.SetupSignalHandler()
	mgr, err := newManager(cfg, opts)
	if err != nil {
		return err
	}
	if err := (&controller.NodePoolReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register NodePool controller: %w", err)
	}
	if err := (&controller.NKEClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register NKECluster controller: %w", err)
	}
	// +kubebuilder:scaffold:builder
	setupLog.Info("starting management engine", "version", version,
		"leaderElection", opts.LeaderElection, "leaderElectionNamespace", opts.LeaderElectionNamespace)
	return mgr.Start(ctx)
}

// defaultManagerOptions enables leader election and health probes; metrics are opt-in.
// Callers must set LeaderElectionNamespace when election is enabled.
func defaultManagerOptions() ctrl.Options {
	return ctrl.Options{
		Metrics: metricsserver.Options{
			BindAddress:   "0",
			SecureServing: true,
			TLSOpts:       []func(*tls.Config){func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} }},
		},
		HealthProbeBindAddress: ":8081",
		LeaderElection:         true,
		LeaderElectionID:       leaderElectionID,
		// Avoid releasing leadership early while application work may still be stopping.
		LeaderElectionReleaseOnCancel: false,
		Cache: cache.Options{
			ReaderFailOnMissingInformer: true,
			DefaultTransform:            cache.TransformStripManagedFields(),
		},
	}
}

// newManager constructs a manager without controllers, watches, or API discovery prechecks.
// The management API must use HTTPS with certificate verification.
// Callers register controllers before Start, handle signals, and exit promptly when Start returns.
func newManager(config *rest.Config, opts ctrl.Options) (ctrl.Manager, error) {
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
	if opts.Metrics.SecureServing {
		opts.Metrics.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	opts.Scheme = scheme
	mgr, err := ctrl.NewManager(config, opts)
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
