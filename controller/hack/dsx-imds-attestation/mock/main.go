// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
)

func run() error {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	issuer := flag.String("issuer", "https://ds9.dsx.nvid.id", "issuer for host identity JWTs")
	tenant := flag.String("tenant", "operator-1", "tenant encoded in host identities")
	region := flag.String("region", "ds9", "region encoded in host identities")
	partition := flag.String("partition", "nvid.id", "partition encoded in host identities")
	flag.Parse()

	imds, err := newIMDSServer(serverOptions{
		issuer: *issuer, tenant: *tenant, region: *region, partition: *partition,
	})
	if err != nil {
		return fmt.Errorf("create IMDS server: %w", err)
	}
	server := &http.Server{Addr: *listen, Handler: imds.handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("Failed to shut down DSX IMDS mock", "error", err)
		}
	}()

	slog.Info("Starting DSX IMDS mock", "listen", *listen, "issuer", *issuer)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve DSX IMDS mock: %w", err)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("DSX IMDS mock failed", "error", err)
		os.Exit(1)
	}
}
