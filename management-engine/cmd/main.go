// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	internalv1 "github.com/dsx-ai-factory/nke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nke/management-engine/controllers"
	// +kubebuilder:scaffold:imports
)

var version = "dev"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(internalv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var probeAddr string
	var leaderElect bool
	var watchNamespace string
	var leaderElectionNamespace string
	var contextName string
	var showVersion bool
	var secureMetrics bool
	var enableHTTP2 bool
	var metricsCertPath, metricsCertName, metricsCertKey string

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metric endpoint binds to; 0 disables it.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for the controller manager.")
	flag.StringVar(
		&watchNamespace,
		"namespace",
		"",
		"Namespace that the controller watches. If unspecified, the controller watches all namespaces.",
	)
	flag.StringVar(
		&leaderElectionNamespace,
		"leader-election-namespace",
		"",
		"Existing namespace in the target cluster for the election Lease",
	)
	flag.StringVar(&contextName, "context", "", "Management kubeconfig context")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.BoolVar(&secureMetrics, "metrics-secure", true, "Serve metrics over HTTPS with authentication and authorization")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "", "Directory containing the metrics certificate")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "Metrics certificate filename")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "Metrics private key filename")
	flag.BoolVar(&enableHTTP2, "enable-http2", false, "Enable HTTP/2 for metrics")

	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return
	}

	var watchNamespaces map[string]cache.Config
	if watchNamespace != "" {
		watchNamespaces = map[string]cache.Config{watchNamespace: {}}
	}
	metricsOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		CertDir:       metricsCertPath,
		CertName:      metricsCertName,
		KeyName:       metricsCertKey,
	}
	if secureMetrics {
		metricsOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if !enableHTTP2 {
		metricsOptions.TLSOpts = []func(*tls.Config){func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} }}
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	cfg, err := config.GetConfigWithContext(contextName)
	if err != nil {
		setupLog.Error(err, "load management kubeconfig")
		os.Exit(1)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsOptions,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionNamespace: leaderElectionNamespace,
		LeaderElectionID:        "management-engine.nke.nvidia.com",
		Cache:                   cache.Options{DefaultNamespaces: watchNamespaces},
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	ctx := ctrl.SetupSignalHandler()

	if err := (&controllers.NKEClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NKECluster")
		os.Exit(1)
	}
	if err := (&controllers.NodePoolReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NodePool")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "version", version)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
