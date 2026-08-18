// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rollout

import (
	"context"
)

// ClusterPreflightFunc aborts the rollout on non-nil return; no node is touched.
type ClusterPreflightFunc func(
	ctx context.Context,
	cluster Cluster,
	params map[string]string,
) error

// ClusterPreflightRef names a registered ClusterPreflightFunc to run.
type ClusterPreflightRef struct {
	// Name is looked up in ClusterPreflightRegistry at execution time.
	Name string
	// Params is passed through verbatim to the registered func.
	Params map[string]string
}

// NodePreflightFunc halts the rollout for that node on non-nil return,
// without cascade (the node is never modified).
type NodePreflightFunc func(
	ctx context.Context,
	cluster Cluster,
	node Node,
	params map[string]string,
) error

// NodePreflightRef names a registered NodePreflightFunc to run per node.
type NodePreflightRef struct {
	// Name is looked up in NodePreflightRegistry at execution time.
	Name string
	// Params is passed through verbatim to the registered func.
	Params map[string]string
}

// Preflight registration and lookup live on *Registry (see registry.go).
