// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// dsx-identity-exchange-mock is a deliberately small CAPD test double for the
// external contract used by dsx-identity-keys. It validates a real local
// JWT-SVID through the SPIRE Workload API, rewrites its SPIFFE subject into the
// DSX trust-domain form, and publishes the re-signing key through OIDC discovery.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

func run() error {
	listen := flag.String("listen", ":8443", "HTTPS listen address")
	issuer := flag.String("issuer", "", "issuer for exchanged JWTs")
	jwksURL := flag.String("jwks-url", "", "public JWKS URL advertised through discovery")
	region := flag.String("region", "", "region encoded in exchanged SPIFFE trust domains")
	partition := flag.String("partition", "", "partition encoded in exchanged SPIFFE trust domains")
	tlsCert := flag.String("tls-cert", "/run/tls/tls.crt", "TLS certificate file")
	tlsKey := flag.String("tls-key", "/run/tls/tls.key", "TLS private-key file")
	workloadSocket := flag.String("socket", "", "SPIFFE Workload API socket; empty uses SPIFFE_ENDPOINT_SOCKET")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var sourceOptions []workloadapi.JWTSourceOption
	if *workloadSocket != "" {
		sourceOptions = append(sourceOptions, workloadapi.WithClientOptions(workloadapi.WithAddr(*workloadSocket)))
	}
	source, err := workloadapi.NewJWTSource(ctx, sourceOptions...)
	if err != nil {
		return fmt.Errorf("create SPIRE JWT source: %w", err)
	}
	defer source.Close()

	exchange, err := newExchangeServer(*issuer, *jwksURL, *region, *partition, jwtSVIDValidator{bundles: source})
	if err != nil {
		return fmt.Errorf("create exchange server: %w", err)
	}
	server := &http.Server{
		Addr:              *listen,
		Handler:           exchange.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("Failed to shut down test exchange server", "error", err)
		}
	}()

	slog.Info("Starting CAPD DSX identity exchange mock", "listen", *listen, "issuer", *issuer, "region", *region)
	err = server.ListenAndServeTLS(*tlsCert, *tlsKey)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTPS: %w", err)
	}
	<-shutdownDone
	return nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("DSX identity exchange mock failed", "error", err)
		os.Exit(1)
	}
}
