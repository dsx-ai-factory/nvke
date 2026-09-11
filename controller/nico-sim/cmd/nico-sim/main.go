// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

	"github.com/NVIDIA/nke/controller/nico-sim/internal/app"
	nvmconfig "github.com/NVIDIA/nke/controller/nico-sim/internal/config"
	"github.com/NVIDIA/nke/controller/nico-sim/internal/seed"
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
	switch nvmconfig.ParseLogFormat() {
	case nvmconfig.LogFormatText:
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

	cfg, err := nvmconfig.Parse(logger)
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

	logger.Info("starting nico-sim server",
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

	logger.Info("nico-sim stopped")
}
