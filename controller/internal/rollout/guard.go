// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package rollout

import (
	"context"
)

// GuardState carries guard state between Pre* and Post* calls.
// Pre* MUST return a non-empty GuardName; use StatelessGuardState(g.Name()) for stateless guards.
type GuardState struct {
	GuardName string // must be non-empty; set to g.Name() by the framework
	Payload   []byte // guard's private state; nil for stateless guards
}

// StatelessGuardState returns a GuardState with only GuardName set.
func StatelessGuardState(name string) GuardState {
	return GuardState{GuardName: name}
}

// NodeGuard symmetrically brackets each ApplyToNode call.
// State MUST flow via GuardState — the executor re-instantiates guards between Pre and Post.
type NodeGuard interface {
	Name() string

	// PreApply runs before ApplyToNode. PostApply fires for every guard regardless of error.
	PreApply(ctx context.Context, cluster Cluster, node Node) (GuardState, error)

	// PostApply runs after ApplyToNode in reverse order, always.
	// Failure on Applied/Unchanged/FailedRestored escalates Outcome to FailedNotRestored.
	PostApply(ctx context.Context, cluster Cluster, node Node, outcome Outcome, state GuardState) error
}

// NodeGuardPreApplyChecker is an optional NodeGuard capability for guards
// whose PreApply starts asynchronous external work. The executor calls
// PreApply as a short activity, then drives CheckPreApply as short status
// activities separated by workflow.Sleep. This keeps polling in deterministic
// workflow orchestration instead of hiding start+poll inside one activity,
// where a worker crash would lose polling progress and durability would exist
// only at the activity boundary.
type NodeGuardPreApplyChecker interface {
	CheckPreApply(ctx context.Context, cluster Cluster, node Node, state GuardState) (done bool, err error)
}

// NodeGuardPostApplyChecker is the PostApply sibling of
// NodeGuardPreApplyChecker. It lets cleanup guards start asynchronous restore
// work in PostApply, then let the workflow durably poll completion with
// workflow timers and short status activities.
type NodeGuardPostApplyChecker interface {
	CheckPostApply(ctx context.Context, cluster Cluster, node Node, outcome Outcome, state GuardState) (done bool, err error)
}

// NodeGuardFactory produces a fresh NodeGuard per Execute call.
type NodeGuardFactory func(params map[string]string) (NodeGuard, error)

// NodeGuardRef names a registered NodeGuardFactory to instantiate and
// bracket each node's apply. Resolved by name at execution time so it
// can cross the Temporal activity boundary.
type NodeGuardRef struct {
	// Name is resolved against the Registry at execution time.
	Name string
	// Params is passed through verbatim to the registered factory.
	Params map[string]string
}

// ClusterGuard symmetrically brackets the entire rollout. State MUST flow via GuardState.
type ClusterGuard interface {
	Name() string

	// PreRollout runs before BuildApplyToNodePayload. PostRollout fires for every guard regardless of error.
	PreRollout(ctx context.Context, cluster Cluster) (GuardState, error)

	// PostRollout runs after the final node result in reverse order. Errors are swallowed.
	PostRollout(ctx context.Context, cluster Cluster, result Result, state GuardState) error
}

// ClusterGuardFactory produces a fresh ClusterGuard per Execute call.
type ClusterGuardFactory func(params map[string]string) (ClusterGuard, error)

// ClusterGuardRef names a registered ClusterGuardFactory to instantiate
// and bracket the entire rollout. Resolved by name at execution time so
// it can cross the Temporal activity boundary.
type ClusterGuardRef struct {
	// Name is resolved against the Registry at execution time.
	Name string
	// Params is passed through verbatim to the registered factory.
	Params map[string]string
}

// Guard registration and resolution live on *Registry (see registry.go).
