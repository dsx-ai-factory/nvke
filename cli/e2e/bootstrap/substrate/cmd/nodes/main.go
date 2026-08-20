// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command nodes drives the nkectl bootstrap-site E2E fixture: the fake
// pre-provisioned nodes real dispatch will target once bootstrap site stops
// being a stub. Invoked by `make -C cli nodes-up/nodes-down/nodes-status`,
// not run directly.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"github.com/NVIDIA/nke/cli/e2e/bootstrap/substrate"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: nodes <up|down|status>")
		os.Exit(2)
	}

	ctx := context.Background()
	logf := func(format string, args ...any) { slog.Info(fmt.Sprintf(format, args...)) }
	cfg := substrate.DefaultConfig()

	var err error
	switch os.Args[1] {
	case "up":
		_, err = substrate.Start(ctx, cfg, logf)
	case "down":
		err = down(ctx, cfg, logf)
	case "status":
		err = status(ctx)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q; want up, down, or status\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nodes:", err)
		os.Exit(1)
	}
}

func down(ctx context.Context, cfg substrate.Config, logf substrate.Logf) error {
	cfg.Keep = false // "down" always tears down, regardless of NKECTL_E2E_KEEP.
	s, err := substrate.Attach(cfg, logf)
	if err != nil {
		return err
	}
	s.Stop(ctx)
	return nil
}

func status(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label=nkectl-e2e=true",
		"--format", "table {{.Names}}\t{{.Status}}\t{{.Networks}}")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
