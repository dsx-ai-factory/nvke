// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nodepools provides cobra subcommands for the nkectl node pool
// resource. All commands here are backed by the CWE REST API.
package nodepools

import (
	"github.com/dsx-ai-factory/nke/cli/internal/clients"
)

// CWEClientFn is a factory that returns an authenticated CWE client.
// Commands receive this rather than a constructed client so that the
// client is only built when a command actually runs (not at startup).
type CWEClientFn func() (*clients.CWEClient, error)
