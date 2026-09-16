// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clusters provides cobra subcommands for the nkectl cluster resource.
// All commands here are backed by the CWE REST API.
package clusters

import (
	"github.com/NVIDIA/nke/cli/internal/clients"
)

// CWEClientFn is a factory that returns an authenticated CWE client.
type CWEClientFn func() (*clients.CWEClient, error)
