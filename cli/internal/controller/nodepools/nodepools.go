// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nodepools provides cobra subcommands for the nvkectl node pool
// resource. All commands here are backed by the controller REST API.
package nodepools

import (
	"github.com/dsx-ai-factory/nvke/cli/internal/clients"
)

// ControllerClientFn is a factory that returns an authenticated controller API client.
// Commands receive this rather than a constructed client so that the
// client is only built when a command actually runs (not at startup).
type ControllerClientFn func() (*clients.ControllerClient, error)
