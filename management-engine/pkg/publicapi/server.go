// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package publicapi

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/dsx-ai-factory/nvke/auth/authn"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

const (
	// LivenessPath identifies the process liveness endpoint.
	LivenessPath = "/healthz"
	// ReadinessPath identifies the datastore readiness endpoint.
	ReadinessPath = "/readyz"
)

// MountHealth installs liveness and readiness probes using the supplied datastore check.
func MountHealth(mux *http.ServeMux, checkReadiness func(context.Context) error) {
	mux.HandleFunc("GET "+LivenessPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET "+ReadinessPath, func(w http.ResponseWriter, r *http.Request) {
		if err := checkReadiness(r.Context()); err != nil {
			http.Error(w, "datastore unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// Run parses common flags, mounts the command's public surface, and serves it until shutdown.
func Run(version string, registrations []resource.Registration, cfg resource.ClientConfig) error {
	address := flag.String("listen", ":8080", "HTTP listen address")
	contextName := flag.String("context", "", "Datastore kubeconfig context")
	authConfig := flag.String("auth-config", "", "Authentication configuration file. Empty disables authentication.")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	var authenticator *authn.Authenticator
	if *authConfig != "" {
		var err error
		authenticator, err = authn.Setup(context.Background(), authn.Options{ConfigPath: *authConfig}, slog.Default())
		if err != nil {
			return err
		}
		if authenticator == nil {
			return fmt.Errorf("authentication config %q did not enable authentication", *authConfig)
		}
	}

	datastore, err := config.GetConfigWithContext(*contextName)
	if err != nil {
		return fmt.Errorf("load datastore configuration: %w", err)
	}
	datastore.Timeout = 5 * time.Second
	disco, err := discovery.NewDiscoveryClientForConfig(datastore)
	if err != nil {
		return fmt.Errorf("configure readiness discovery: %w", err)
	}
	cfg.Datastore = datastore
	mux := http.NewServeMux()
	for _, registration := range registrations {
		if err := registration.Mount(mux, cfg); err != nil {
			return fmt.Errorf("mount %s surface: %w", cfg.Surface, err)
		}
	}
	MountHealth(mux, func(ctx context.Context) error {
		_, err := disco.ServerResourcesForGroupVersionWithContext(ctx, "service.nvke.nvidia.com/v1")
		return err
	})
	handler := resource.WithLimits(mux, resource.ServerOptions{})
	if authenticator != nil {
		handler = authenticator.Wrap(handler)
	}
	server := &http.Server{
		Addr: *address, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: time.Minute,
	}
	ctx := signals.SetupSignalHandler()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	slog.Info("serving REST API", "surface", cfg.Surface, "address", *address)
	select {
	case err := <-serveErr:
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down HTTP: %w", err)
		}
		return nil
	}
}
