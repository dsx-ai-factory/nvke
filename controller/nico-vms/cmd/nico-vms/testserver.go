// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	nethttp "net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/lmittmann/tint"

	"github.com/NVIDIA/nke/controller/nico-vms/internal/app"
	nvmconfig "github.com/NVIDIA/nke/controller/nico-vms/internal/config"
	"github.com/NVIDIA/nke/controller/nico-vms/internal/seed"
)

// runTestServer starts a nico-vms instance configured for integration testing.
//
// It creates an ephemeral database, auto-downloads the base VM image when
// needed, and prints the resolved listen URL to stdout so callers can discover
// the address programmatically:
//
//	NICO_VMS_URL=http://localhost:<port>
//
// All log output goes to stderr. The server shuts down on SIGINT/SIGTERM.
func runTestServer() {
	fs := flag.NewFlagSet("test-server", flag.ExitOnError)
	listenAddr := fs.String("listen-addr", ":0", "address to listen on (use :0 for auto-assigned port)")
	seedPath := fs.String("seed-path", "", "path to seed YAML (empty = nico-vms default)")
	_ = fs.Parse(os.Args[2:])

	var logHandler slog.Handler
	switch nvmconfig.ParseLogFormat() {
	case nvmconfig.LogFormatText:
		logHandler = tint.NewHandler(os.Stderr, &tint.Options{
			Level:      slog.LevelInfo,
			TimeFormat: time.TimeOnly,
			NoColor:    os.Getenv("NO_COLOR") != "",
		})
	default:
		logHandler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})
	}
	logger := slog.New(logHandler)

	tmpDir, err := os.MkdirTemp("", "nico-vms-test-*")
	if err != nil {
		logger.Error("failed to create temp dir", "error", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	if err := ensureTestBaseImage(logger); err != nil {
		logger.Error("failed to ensure base image", "error", err)
		os.Exit(1)
	}

	os.Setenv("NICO_VMS_LISTEN_ADDR", *listenAddr)
	os.Setenv("NICO_VMS_DB_PATH", filepath.Join(tmpDir, "nico-vms.db"))
	if *seedPath != "" {
		os.Setenv("NICO_VMS_SEED_PATH", *seedPath)
	}

	cfg, err := nvmconfig.Parse(logger)
	if err != nil {
		logger.Error("failed to parse config", "error", err)
		os.Exit(1)
	}

	seedDoc, err := seed.Load(cfg.SeedPath)
	if err != nil {
		logger.Error("failed to load seed", "path", cfg.SeedPath, "error", err)
		os.Exit(1)
	}

	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	application, err := app.Setup(runCtx, cfg, seedDoc, logger)
	if err != nil {
		logger.Error("failed to setup app", "error", err)
		os.Exit(1)
	}
	defer application.Close()

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		logger.Error("failed to listen", "addr", cfg.ListenAddr, "error", err)
		os.Exit(1)
	}

	actualAddr := ln.Addr().(*net.TCPAddr)
	serverURL := fmt.Sprintf("http://localhost:%d", actualAddr.Port)

	// Machine-readable URL on stdout; everything else is stderr.
	fmt.Fprintf(os.Stdout, "NICO_VMS_URL=%s\n", serverURL)
	os.Stdout.Sync()

	server := &nethttp.Server{Handler: application.Handler}

	go func() {
		<-runCtx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Info("test-server ready",
		"url", serverURL,
		"vmBackend", cfg.EffectiveVMBackend(),
		"dbPath", cfg.DBPath,
		"seedPath", cfg.SeedPath,
	)

	if err := server.Serve(ln); err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		cancel()
	}

	logger.Info("test-server stopped")
}

// ensureTestBaseImage sets NICO_VMS_BASE_IMAGE if it is not already set,
// checking the conventional cache path and downloading an Ubuntu cloud image
// as a fallback. This is a no-op when NICO_VMS_BASE_IMAGE is already set.
func ensureTestBaseImage(logger *slog.Logger) error {
	if os.Getenv("NICO_VMS_BASE_IMAGE") != "" {
		return nil
	}

	imagePath := filepath.Join("images", "ubuntu-cloud.qcow2")
	if info, err := os.Stat(imagePath); err == nil && info.Size() > 0 {
		abs, _ := filepath.Abs(imagePath)
		logger.Info("reusing cached base image", "path", abs)
		os.Setenv("NICO_VMS_BASE_IMAGE", abs)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		return err
	}

	imageURL := testBaseImageURL()
	logger.Info("downloading base image", "url", imageURL, "dest", imagePath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodGet, imageURL, nil)
	if err != nil {
		return err
	}
	resp, err := nethttp.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != nethttp.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	tmpPath := imagePath + ".download"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpPath, imagePath); err != nil {
		os.Remove(tmpPath)
		return err
	}

	abs, _ := filepath.Abs(imagePath)
	logger.Info("downloaded base image", "path", abs)
	os.Setenv("NICO_VMS_BASE_IMAGE", abs)
	return nil
}

func testBaseImageURL() string {
	if runtime.GOARCH == "arm64" {
		return "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img"
	}
	return "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img"
}
