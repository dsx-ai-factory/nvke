// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dsx-ai-factory/nvke/agent/pkg/agentupgrade"
)

const defaultOperationRoot = "/var/lib/nke/agent-upgrades"

// version is set at build time via -ldflags "-X main.version=<version>".
var version = "dev"

func main() {
	versionFlag := flag.Bool("version", false, "Print the version and exit")
	operationID := flag.String("operation-id", "", "durable node-agent upgrade operation ID")
	operationRoot := flag.String("operation-root", defaultOperationRoot, "durable node-agent upgrade operation root")
	flag.Parse()
	if *versionFlag {
		_, _ = fmt.Fprintf(os.Stdout, "NKE Agent Updater Version: %s\n", version)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	if err := agentupgrade.ValidateOperationID(*operationID); err != nil {
		slog.Error("invalid node-agent upgrade operation", "operationID", *operationID, "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store := agentupgrade.NewStore(*operationRoot)
	updater := agentupgrade.NewUpdater(store)
	if err := updater.Run(ctx, *operationID); err != nil {
		slog.Error("node-agent updater operation failed", "operationID", *operationID, "err", err)
		os.Exit(1)
	}
	slog.Info("node-agent updater operation completed", "operationID", *operationID)
}
