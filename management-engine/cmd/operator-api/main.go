// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"os"

	"github.com/dsx-ai-factory/nvke/management-engine/pkg/publicapi"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

var version = "dev"

func main() {
	if err := publicapi.Run(version, publicapi.Operator(), resource.ClientConfig{Surface: "operator"}); err != nil {
		slog.Error("operator API stopped", "error", err)
		os.Exit(1)
	}
}
