// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// artifact-diagnostic executes one supplied-artifact check without building its subject.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dsx-ai-factory/nvke/controller/internal/artifactdiagnostic"
)

func main() {
	var o artifactdiagnostic.Options
	flag.StringVar(&o.Kind, "kind", "", "controller-image or chart-package")
	flag.StringVar(&o.Subject, "subject", "", "supplied OCI archive or chart tgz")
	flag.StringVar(&o.SHA256, "sha256", "", "exact archive SHA256")
	flag.StringVar(&o.Version, "version", "", "required controller version")
	flag.StringVar(&o.Output, "output", "", "new private report directory")
	flag.StringVar(&o.Cluster, "cluster", "", "unique iv2 cluster name")
	flag.StringVar(&o.NodeImage, "node-image", "", "pinned Kind node dependency")
	flag.StringVar(&o.PostgresImage, "postgres-image", "", "pinned PostgreSQL dependency")
	flag.StringVar(&o.TemporalImage, "temporal-image", "", "pinned Temporal CLI dependency")
	flag.DurationVar(&o.Timeout, "timeout", 20*time.Minute, "execution budget (maximum one hour)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "flags only")
		os.Exit(2)
	}
	if err := artifactdiagnostic.Run(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, context.DeadlineExceeded) {
			os.Exit(124)
		}
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
}
