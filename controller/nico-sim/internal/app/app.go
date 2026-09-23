// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package app provides the shared nico-sim application setup used by both the
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
	"time"

	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/bgp"
	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/cloudinit"
	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/config"
	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/consolessh"
	containerbackend "github.com/dsx-ai-factory/nke/controller/nico-sim/internal/container"
	nvmhttp "github.com/dsx-ai-factory/nke/controller/nico-sim/internal/http"
	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/lifecycle"
	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/lokilog"
	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/netutil"
	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/seed"
	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/sshexec"
	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/state"
)

// App holds the running nico-sim components.
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
	// When containers are disabled, use a service that returns unavailable for
	// every container operation.
	containers := nvmhttp.ContainerService(nvmhttp.DisabledContainers{})
	if cfg.ContainerEnabled {
		containerSite, siteErr := store.GetContainerSiteConfig(ctx)
		if siteErr != nil {
			store.Close()
			return nil, fmt.Errorf("resolve container site configuration: %w", siteErr)
		}
		service, serviceErr := containerbackend.New(containerbackend.Config{
			OrgID: containerSite.OrgID, TenantID: containerSite.TenantID, SiteID: containerSite.SiteID,
			VPCID: containerSite.VPCID, SubnetID: containerSite.SubnetID, SSHKeyGroupID: containerSite.SSHKeyGroupID,
			InstanceTypeID: containerSite.InstanceTypeID, NodeNetwork: cfg.ContainerNodeNetwork,
			NodeImage: cfg.ContainerNodeImage, DockerBinary: cfg.ContainerDockerBinary,
			SpireAttestationMethod: cfg.ContainerSpireAttestationMethod,
		}, logger)
		if serviceErr != nil {
			store.Close()
			return nil, fmt.Errorf("initialize container backend: %w", serviceErr)
		}
		if healthErr := service.Health(ctx); healthErr != nil {
			if cfg.ContainerRequireHealth {
				store.Close()
				return nil, fmt.Errorf("container backend health: %w", healthErr)
			}
			log.Warn("Container backend is unavailable; container operations will return 503", "error", healthErr)
		}
		containers = service
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

	// BGP speaker. Skipped in libvirt-network mode: it installs host routes
	// and needs the nico-vms bridge + iptables redirect.
	var bgpSpeaker *bgp.Speaker
	if cfg.BGPEnabled && !cfg.UseLibvirtNetwork() && cfg.EffectiveVMBackend() != config.VMBackendMock {
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

	// Metadata servers. Started after engine.Start() so that libvirt-network
	// mode's EnsureSetup has already queried the gateway (MetadataAddr needs it).
	var metadataCancel context.CancelFunc
	var metadataWg *sync.WaitGroup
	if cfg.EffectiveVMBackend() != config.VMBackendMock {
		metadataServer := cloudinit.NewMetadataServer(
			logger.With("component", "cloud-init-metadata"),
			store,
			filepath.Join(filepath.Dir(cfg.DBPath), "instances"),
		)
		var metaErr error
		metadataCancel, metadataWg, metaErr = startMetadataServers(ctx, engine.MetadataAddrs(), metadataServer.Handler(), logger)
		if metaErr != nil {
			return nil, fmt.Errorf("start metadata servers: %w", metaErr)
		}
	}

	// Promtail targets writer (opt-in via NICO_VMS_PROMTAIL_TARGETS_FILE).
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

	handler := nvmhttp.NewHandler(store, containers, logger, cfg.LogRequestMethods, consoleSSHPort, cfg.FilesDirectory(), cfg.WorkingDirectory(), sshexec.Config{
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
	// 3.5. Clean up network-mode-specific resources (e.g. libvirt DNS host entry)
	teardownCtx, teardownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer teardownCancel()
	a.Engine.Teardown(teardownCtx)
	// 4. Stop console SSH
	if a.ConsoleSSH != nil {
		a.ConsoleSSH.Wait()
	}
	// 5. Close store last — everything above may depend on it
	_ = a.Store.Close()
}

// startMetadataServers starts the cloud-init metadata HTTP listeners on the
// addresses the active NetworkManager reports via MetadataAddrs() (e.g.
// 169.254.169.254:7777 + {bridge_gw}:7777 in bridge mode, or {libvirt_gw}:80 in
// libvirt-network mode). At least one address must bind: if every one fails,
// cloud-init has no reachable datasource and the VMs would never receive their
// seed, so this returns an error and Setup fails fast rather than coming up
// silently without a metadata server.
func startMetadataServers(ctx context.Context, addrs []string, handler http.Handler, logger *slog.Logger) (context.CancelFunc, *sync.WaitGroup, error) {
	metaCtx, metaCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup

	bound := 0
	for _, addr := range addrs {
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
			continue
		}
		bound++
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

	if len(addrs) > 0 && bound == 0 {
		metaCancel()
		wg.Wait()
		return nil, nil, fmt.Errorf("no cloud-init metadata listener could bind (tried %v)", addrs)
	}

	return metaCancel, &wg, nil
}
