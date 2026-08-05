// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package main

import (
	"context"
	"errors"
	"log/slog"
	nethttp "net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lmittmann/tint"

	"github.com/NVIDIA/nke/controller/forge-vms/internal/app"
	fvmconfig "github.com/NVIDIA/nke/controller/forge-vms/internal/config"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/seed"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "test-server":
			runTestServer()
			return
		}
	}

	var logHandler slog.Handler
	switch fvmconfig.ParseLogFormat() {
	case fvmconfig.LogFormatText:
		logHandler = tint.NewHandler(os.Stdout, &tint.Options{
			Level:      slog.LevelInfo,
			TimeFormat: time.TimeOnly,
			NoColor:    os.Getenv("NO_COLOR") != "",
		})
	default:
		logHandler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})
	}
	logger := slog.New(logHandler)

	cfg, err := fvmconfig.Parse(logger)
	if err != nil {
		logger.Error("failed to parse configuration", "error", err)
		os.Exit(1)
	}

	seedDoc, err := seed.Load(cfg.SeedPath)
	if err != nil {
		logger.Error("failed to load seed file", "path", cfg.SeedPath, "error", err)
		os.Exit(1)
	}

	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	application, err := app.Setup(runCtx, cfg, seedDoc, logger)
	if err != nil {
		logger.Error("failed to start application", "error", err)
		os.Exit(1)
	}
	defer application.Close()

	server := &nethttp.Server{
		Addr:    cfg.ListenAddr,
		Handler: application.Handler,
	}

	go func() {
		<-runCtx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("http shutdown failed", "error", err)
		}
	}()

	logger.Info("starting forge-vms server",
		"listenAddr", cfg.ListenAddr,
		"dbPath", cfg.DBPath,
		"vmBackend", cfg.EffectiveVMBackend(),
		"libvirtURI", cfg.LibvirtURI,
		"bridgeCIDR", cfg.BridgeCIDR,
		"baseImage", cfg.BaseImage,
		"consoleSSHPort", application.ConsoleSSHPort,
	)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
		logger.Error("http server failed", "error", err)
		cancel()
	}

	logger.Info("forge-vms stopped")
}
