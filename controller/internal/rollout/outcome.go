// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rollout

import (
	"fmt"
)

// Outcome is the per-node result from ApplyToNode. Zero value (Unspecified) is
// fail-closed — it triggers cascade rather than silently claiming success.
// FailedRestored halts cleanly; FailedNotRestored cascades (unknown node state).
type Outcome int

const (
	// OutcomeUnspecified is the zero value. Operations must never
	// return it; the executor treats it as unknown and cascades.
	OutcomeUnspecified Outcome = iota
	// OutcomeApplied: change made, node verified healthy.
	OutcomeApplied
	// OutcomeUnchanged: node already in desired state. Counts toward
	// the quorum policy the same as OutcomeApplied.
	OutcomeUnchanged
	// OutcomeFailedRestored: apply failed, but the operation restored the
	// node to a known-good state ITSELF (e.g. reverted its own change).
	// The executor halts without cascade — the cluster is coherent. This
	// is the operation's per-node self-recovery, distinct from the
	// cluster-level RollbackHandler cascade.
	OutcomeFailedRestored
	// OutcomeFailedNotRestored: apply failed AND the operation could not
	// restore the node — its state is unknown. The executor cascades the
	// RollbackHandler over the already-applied nodes in reverse apply order.
	OutcomeFailedNotRestored
	// OutcomeSkippedPreflight: a NodePreflight refused this node;
	// ApplyToNode never invoked. Halts without cascade.
	OutcomeSkippedPreflight
)

// String returns the Outcome name for logs and metrics. Out-of-range renders as Unknown(N).
func (o Outcome) String() string {
	switch o {
	case OutcomeUnspecified:
		return "Unspecified"
	case OutcomeApplied:
		return "Applied"
	case OutcomeUnchanged:
		return "Unchanged"
	case OutcomeFailedRestored:
		return "FailedRestored"
	case OutcomeFailedNotRestored:
		return "FailedNotRestored"
	case OutcomeSkippedPreflight:
		return "SkippedPreflight"
	default:
		return fmt.Sprintf("Unknown(%d)", int(o))
	}
}
