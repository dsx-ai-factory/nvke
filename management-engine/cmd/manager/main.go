// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/dsx-ai-factory/nke/management-engine/pkg/controllers/registry"
	"github.com/dsx-ai-factory/nke/management-engine/pkg/manager"
)

var version = "dev"

var errFlagParse = errors.New("invalid flags")

func main() {
	if err := run(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		if !errors.Is(err, errFlagParse) {
			slog.Error("management engine stopped", "error", err)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	// Avoid colliding with controller-runtime's global kubeconfig flag.
	flags := flag.NewFlagSet("management-engine", flag.ContinueOnError)
	opts := manager.DefaultOptions()
	var config manager.ConfigOptions
	var level slog.Level
	var showVersion bool
	flags.StringVar(&config.Kubeconfig, "kubeconfig", "", "Path to the management cluster kubeconfig")
	flags.StringVar(&config.Context, "context", "", "Management kubeconfig context")
	flags.StringVar(&opts.MetricsBindAddress, "metrics-bind-address", opts.MetricsBindAddress, "Metrics HTTP address; 0 disables")
	flags.StringVar(&opts.HealthProbeBindAddress, "health-probe-bind-address", opts.HealthProbeBindAddress, "Health/readiness HTTP address; 0 disables")
	flags.BoolVar(&opts.LeaderElection, "leader-elect", opts.LeaderElection, "Enable leader election; disable only for a sole development process")
	flags.StringVar(&opts.LeaderElectionNamespace, "leader-election-namespace", "", "Existing namespace in the target cluster for the election Lease")
	flags.TextVar(&level, "log-level", slog.LevelInfo, "Log level: DEBUG, INFO, WARN or ERROR")
	flags.BoolVar(&showVersion, "version", false, "Print version and exit")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errFlagParse, err)
	}
	if showVersion {
		fmt.Println(version)
		return nil
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	opts.Logger = logr.FromSlogHandler(logger.Handler())
	ctrl.SetLogger(opts.Logger)
	cfg, err := manager.LoadConfig(config)
	if err != nil {
		return err
	}
	ctx := ctrl.SetupSignalHandler()
	mgr, err := manager.New(cfg, opts)
	if err != nil {
		return err
	}
	if err := registry.Register(mgr); err != nil {
		return err
	}
	logger.Info("starting management engine", "version", version,
		"leaderElection", opts.LeaderElection, "leaderElectionNamespace", opts.LeaderElectionNamespace)
	return mgr.Start(ctx)
}
