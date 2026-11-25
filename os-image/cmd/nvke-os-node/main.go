// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// nvke-os-node is the node-side subset of nvke-os: manifest and verify only.
// It links no build-only packages.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/dsx-ai-factory/nvke/os-image/internal/oscli"
)

// version is set at build time via -X main.version=.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	app := oscli.NewApp("nvke-os-node", "check this node against its OS image lock", version,
		oscli.VersionCommand(),
		oscli.ManifestCommand(),
		oscli.VerifyCommand(),
	)
	code := oscli.Run(ctx, app, os.Args, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
