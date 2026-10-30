// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"os"

	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
	"github.com/dsx-ai-factory/nvke/management-engine/public"
)

var version = "dev"

func main() {
	if err := public.Run(version, public.Operator(), resource.ClientConfig{Surface: "operator"}); err != nil {
		slog.Error("operator API stopped", "error", err)
		os.Exit(1)
	}
}
