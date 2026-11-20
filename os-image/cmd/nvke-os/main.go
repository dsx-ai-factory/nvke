// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// nvke-os builds and checks the node OS image. Build-only verbs are
// registered here and never in nvke-os-node.
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
	app := oscli.NewApp("nvke-os", "build and check the NVKE node OS image", version,
		oscli.VersionCommand(),
		oscli.ManifestCommand(),
		oscli.VerifyCommand(),
	)
	code := oscli.Run(ctx, app, os.Args, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
