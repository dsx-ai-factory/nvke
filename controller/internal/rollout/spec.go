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
	"fmt"
)

// RolloutSpec is the declarative "how": strategy, replica policy,
// rollback policy, and named refs to preflights and guards. Preflight
// and guard refs are resolved by name against the package registries
// at execution time (see preflight.go, guard.go) — the refs carry only
// a name and params so they cross the Temporal activity boundary, which
// cannot serialize function values.
type RolloutSpec struct {
	// Strategy selects the apply ordering. Empty defaults to StrategySerial.
	Strategy RolloutStrategy
	// MinHealthyReplicas bounds how many control-plane replicas may be
	// non-healthy at once during the rollout.
	MinHealthyReplicas MinHealthyReplicasPolicy
	// RollbackPolicy decides whether a node failure cascades a reverse-order
	// rollback. Empty defaults to no cascade.
	RollbackPolicy RollbackPolicy
	// ClusterPreflights run once before any node is touched; any failure
	// aborts the rollout.
	ClusterPreflights []ClusterPreflightRef
	// NodePreflights run per node before ApplyToNode; a failure skips that
	// node (OutcomeSkippedPreflight) without touching it.
	NodePreflights []NodePreflightRef
	// NodeGuards bracket each ApplyToNode call; PostApply runs in reverse.
	NodeGuards []NodeGuardRef
	// ClusterGuards bracket the entire rollout; PostRollout runs in reverse.
	ClusterGuards []ClusterGuardRef
}

// Validate checks structural invariants. Errors surface as
// (Result{}, err) from Execute, not as PhaseFailure.
func (s RolloutSpec) Validate() error {
	if s.Strategy != "" && s.Strategy != StrategySerial {
		return fmt.Errorf("unsupported rollout strategy %q (only %q is implemented)",
			s.Strategy, StrategySerial)
	}
	if s.MinHealthyReplicas.MinHealthy < 0 {
		return fmt.Errorf("minHealthyReplicas must be >= 0, got %d", s.MinHealthyReplicas.MinHealthy)
	}
	switch s.RollbackPolicy {
	case "", RollbackCascadeOnUnrecoverable, RollbackHaltOnAnyFailure:
		// recognized
	default:
		return fmt.Errorf("unsupported rollback policy %q", s.RollbackPolicy)
	}
	return nil
}

type RolloutStrategy string

const (
	// StrategySerial applies one node at a time in
	// Cluster.ControlPlaneNodes order.
	StrategySerial RolloutStrategy = "Serial"
)

// MinHealthyReplicasPolicy bounds control-plane replica availability
// during a rollout: the minimum number of control-plane replicas that
// must stay healthy throughout. Unrelated to etcd quorum.
type MinHealthyReplicasPolicy struct {
	// MinHealthy is the minimum Applied+Unchanged node count required
	// throughout the rollout. 0 = no constraint.
	MinHealthy int
}

type RollbackPolicy string

const (
	// RollbackCascadeOnUnrecoverable cascades only on FailedNotRestored.
	RollbackCascadeOnUnrecoverable RollbackPolicy = "CascadeOnUnrecoverable"
	// RollbackHaltOnAnyFailure cascades on any failure outcome.
	RollbackHaltOnAnyFailure RollbackPolicy = "HaltOnAnyFailure"
)
