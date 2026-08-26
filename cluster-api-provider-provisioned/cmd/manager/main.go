// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command manager runs the provisioned Cluster API infrastructure provider.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/NVIDIA/nke/cluster-api-provider-provisioned/api/v1alpha1"
	"github.com/NVIDIA/nke/cluster-api-provider-provisioned/controllers"
)

var version = "dev"

func main() {
	var metricsAddress string
	var probeAddress string
	var leaderElection bool
	var showVersion bool

	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8080", "Address for the metrics endpoint.")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":8081", "Address for health probes.")
	flag.BoolVar(&leaderElection, "leader-elect", false, "Enable leader election.")
	flag.BoolVar(&showVersion, "version", false, "Print the version and exit.")
	flag.Parse()

	if showVersion {
		if _, err := fmt.Fprintf(os.Stdout, "cluster-api-provider-provisioned %s\n", version); err != nil {
			os.Exit(1)
		}
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctrllog.SetLogger(logr.FromSlogHandler(logger.Handler()))

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	utilruntime.Must(infrav1.AddToScheme(scheme))

	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddress},
		HealthProbeBindAddress: probeAddress,
		LeaderElection:         leaderElection,
		LeaderElectionID:       "provisioned.infrastructure.cluster.x-k8s.io",
	})
	exitOnError(logger, err, "create manager")

	exitOnError(logger, controllers.NewClusterReconciler(
		manager.GetClient(),
		logger.With("controller", "provisioned-cluster"),
	).SetupWithManager(manager), "register cluster controller")

	exitOnError(logger, controllers.NewMachineReconciler(
		manager.GetClient(),
		logger.With("controller", "provisioned-machine"),
	).SetupWithManager(manager), "register machine controller")

	exitOnError(logger, manager.AddHealthzCheck("healthz", healthz.Ping), "register health check")
	exitOnError(logger, manager.AddReadyzCheck("readyz", healthz.Ping), "register readiness check")

	logger.Info("Starting provisioned controller manager", "version", version)
	exitOnError(logger, manager.Start(ctrl.SetupSignalHandler()), "run manager")
}

func exitOnError(logger *slog.Logger, err error, action string) {
	if err == nil {
		return
	}
	logger.Error("Provisioned controller manager failed", "action", action, "error", err)
	os.Exit(1)
}
