// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clusters provides cobra subcommands for the nvkectl cluster resource.
// All commands here are backed by the controller REST API.
package clusters

import (
	"github.com/dsx-ai-factory/nvke/cli/internal/clients"
)

// ControllerClientFn is a factory that returns an authenticated controller API client.
type ControllerClientFn func() (*clients.ControllerClient, error)
