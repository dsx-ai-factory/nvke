// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package app provides the shared forge-vms application setup used by both the
// production server and integration tests.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/NVIDIA/nke/controller/forge-vms/internal/bgp"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/cloudinit"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/config"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/consolessh"
	fvmhttp "github.com/NVIDIA/nke/controller/forge-vms/internal/http"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/lifecycle"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/lokilog"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/netutil"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/seed"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/sshexec"
	"github.com/NVIDIA/nke/controller/forge-vms/internal/state"
)

// App holds the running forge-vms components.
type App struct {
	Store          *state.Store
	Engine         *lifecycle.Engine
	Handler        http.Handler
	ConsoleSSH     *consolessh.Server
	ConsoleSSHPort int
	bgpSpeaker     *bgp.Speaker
	targetsWriter  *lokilog.TargetsWriter
	metadataWg     *sync.WaitGroup
	metadataCancel context.CancelFunc
}

// Setup opens the store, hydrates/applies seed data, validates networking for
// the active backend, starts metadata servers and the lifecycle engine, and
// wires the HTTP handler.
// The caller is responsible for calling Close when done.
func Setup(ctx context.Context, cfg config.Config, seedDoc seed.Document, logger *slog.Logger) (*App, error) {
	log := logger.With("component", "setup")
	store, err := state.Open(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open state store: %w", err)
	}

	log.Info("Hydrating seed networking cidrs", "bridgeCIDR", cfg.BridgeCIDR)
	hydratedSeedDoc, err := seed.HydrateNetworkingCIDRs(seedDoc, cfg.BridgeCIDR)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("hydrate seed networking: %w", err)
	}

	log.Info("Applying seed")
	if err := store.ApplySeed(ctx, hydratedSeedDoc); err != nil {
		store.Close()
		return nil, fmt.Errorf("apply seed: %w", err)
	}

	if cfg.EffectiveVMBackend() == config.VMBackendDarwinQEMU {
		log.Info("Validating vpc/subnet cidrs against bridge cidr", "bridgeCIDR", cfg.BridgeCIDR)
		if err := ValidateVPCAndSubnets(ctx, store, cfg.BridgeCIDR); err != nil {
			store.Close()
			return nil, fmt.Errorf("validate vpc/subnet cidrs: %w", err)
		}
	}

	// Start console SSH server for remote console log access.
	var consoleSSHServer *consolessh.Server
	consoleSSHPort := 0
	if cfg.ConsoleSSHPort >= 0 {
		consoleSSHServer = consolessh.New(cfg.WorkingDirectory(), logger)
		addr := ":0"
		if cfg.ConsoleSSHPort > 0 {
			addr = fmt.Sprintf(":%d", cfg.ConsoleSSHPort)
		}
		sshPort, sshErr := consoleSSHServer.Start(ctx, addr)
		if sshErr != nil {
			store.Close()
			return nil, fmt.Errorf("start console SSH server: %w", sshErr)
		}
		consoleSSHPort = sshPort
		log.Info("Console SSH server started", "port", consoleSSHPort)
	}

	// Metadata servers (moved from engine — they are long-running servers,
	// not reconciliation concerns).
	var metadataCancel context.CancelFunc
	var metadataWg *sync.WaitGroup
	if cfg.EffectiveVMBackend() != config.VMBackendMock {
		metadataServer := cloudinit.NewMetadataServer(
			logger.With("component", "cloud-init-metadata"),
			store,
			filepath.Join(filepath.Dir(cfg.DBPath), "instances"),
		)
		metadataCancel, metadataWg = startMetadataServers(ctx, cfg, metadataServer.Handler(), logger)
	}

	// BGP speaker (started at app level alongside metadata servers).
	var bgpSpeaker *bgp.Speaker
	if cfg.BGPEnabled && cfg.EffectiveVMBackend() != config.VMBackendMock {
		gw, gwErr := netutil.GatewayIP(cfg.BridgeCIDR)
		if gwErr != nil {
			store.Close()
			return nil, fmt.Errorf("derive gateway IP for BGP: %w", gwErr)
		}
		bgpSpeaker = bgp.New(bgp.Config{
			Enabled:           true,
			ListenAddr:        gw,
			ListenPort:        cfg.BGPListenPort,
			LocalASN:          cfg.BGPLocalASN,
			PeerASN:           cfg.BGPPeerASN,
			HoldTime:          uint16(cfg.BGPHoldTime),
			KeepaliveInterval: uint16(cfg.BGPKeepalive),
			BridgeSubnet:      cfg.BridgeCIDR,
			BridgeName:        cfg.BridgeName,
			AllowedVIPCIDRs:   cfg.BGPAllowedVIPCIDRs,
		}, logger.With("component", "bgp"))
		go func() {
			if err := bgpSpeaker.Start(ctx); err != nil {
				logger.Error("BGP speaker exited with error", "error", err)
			}
		}()
	}

	engine := lifecycle.New(cfg, store, logger)
	if err := engine.Start(ctx); err != nil {
		store.Close()
		return nil, fmt.Errorf("start lifecycle engine: %w", err)
	}

	// Promtail targets writer (opt-in via FORGE_VMS_PROMTAIL_TARGETS_FILE).
	var tw *lokilog.TargetsWriter
	if cfg.PromtailTargetsFile != "" {
		tw = lokilog.New(
			lokilog.Config{
				TargetsFile:   cfg.PromtailTargetsFile,
				LogPathPrefix: cfg.PromtailLogPathPrefix,
			},
			cfg.WorkingDirectory(), store, logger,
		)
		tw.Start(ctx)
	}

	handler := fvmhttp.NewHandler(store, logger, cfg.LogRequestMethods, consoleSSHPort, cfg.FilesDirectory(), cfg.WorkingDirectory(), sshexec.Config{
		User:     cfg.DiagSSHUser,
		Password: cfg.DiagSSHPassword,
	})

	return &App{
		Store:          store,
		Engine:         engine,
		Handler:        handler,
		ConsoleSSH:     consoleSSHServer,
		ConsoleSSHPort: consoleSSHPort,
		bgpSpeaker:     bgpSpeaker,
		targetsWriter:  tw,
		metadataWg:     metadataWg,
		metadataCancel: metadataCancel,
	}, nil
}

// Close shuts down all components in dependency order.
func (a *App) Close() {
	// 1. Stop Promtail targets writer (queries the store, so must stop before store closes)
	if a.targetsWriter != nil {
		a.targetsWriter.Wait()
	}
	// 2. Stop BGP speaker (closes TCP listener, stops peer goroutines)
	if a.bgpSpeaker != nil {
		a.bgpSpeaker.Stop()
	}
	// 3. Stop metadata servers (they query the store, so must stop before store closes)
	if a.metadataCancel != nil {
		a.metadataCancel()
	}
	if a.metadataWg != nil {
		a.metadataWg.Wait()
	}
	// 3. Wait for engine reconcile workers to drain
	a.Engine.Wait()
	// 4. Stop console SSH
	if a.ConsoleSSH != nil {
		a.ConsoleSSH.Wait()
	}
	// 5. Close store last — everything above may depend on it
	_ = a.Store.Close()
}

// startMetadataServers starts the cloud-init metadata HTTP listeners.
// - 169.254.169.254:7777 — EC2-style metadata (fallback datasource)
// - {bridge_gateway}:7777 — Forge-compatible NoCloud via DNS
func startMetadataServers(ctx context.Context, cfg config.Config, handler http.Handler, logger *slog.Logger) (context.CancelFunc, *sync.WaitGroup) {
	metaCtx, metaCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup

	startHTTPListener := func(addr string) {
		lc := net.ListenConfig{
			Control: func(_, _ string, c syscall.RawConn) error {
				return c.Control(func(fd uintptr) {
					_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
				})
			},
		}
		listener, err := lc.Listen(metaCtx, "tcp", addr)
		if err != nil {
			logger.Warn("Could not start metadata listener", "listenAddr", addr, "error", err)
			return
		}
		server := &http.Server{Handler: handler}
		logger.Info("Cloud-init metadata server started", "listenAddr", addr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-metaCtx.Done()
			server.Close()
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
				logger.Error("metadata listener exited with error", "listenAddr", addr, "error", err)
			}
		}()
	}

	// Primary: link-local Ec2 metadata (fallback datasource)
	startHTTPListener("169.254.169.254:7777")

	// Secondary: bridge gateway for Forge-compatible NoCloud
	if gw, err := netutil.GatewayIP(cfg.BridgeCIDR); err == nil {
		startHTTPListener(net.JoinHostPort(gw, "7777"))
	}

	return metaCancel, &wg
}
