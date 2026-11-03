// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"github.com/google/uuid"
)

// ControlPlaneScaleDownRanker is implemented by providers that express an
// ordered placement preference, so scale-down sheds the least-preferred node
// first. A provider opts in by implementing ScaleDownRank and being a case in
// activeProvider; one that expresses no preference does not, and scale-down
// falls back to age. The control-plane package learns only the ordering.
//
// Ranking needs both the config preference and a node-side placement identity
// to compare against (Nico reads node.NicoInstanceTypeID()); a provider without
// that identity must not implement this yet. activeProvider returns a value, so
// the method must use a value receiver or the type assertion fails silently —
// the compile-time assertion below guards against that.
type ControlPlaneScaleDownRanker interface {
	// ScaleDownRank returns the node's shed priority: higher is shed sooner. The
	// primary (most-preferred) type ranks 0, each less-preferred type higher, and
	// an off-list or unplaced type highest.
	ScaleDownRank(node Node) int
}

// ScaleDownRanker returns the active provider's scale-down ranker, or nil when
// the provider expresses no placement preference. A nil ranker is a valid
// input to the selector, which then orders purely by age and ID.
func (c ControlPlaneConfigSpec) ScaleDownRanker() ControlPlaneScaleDownRanker {
	if ranker, ok := c.activeProvider().(ControlPlaneScaleDownRanker); ok {
		return ranker
	}
	return nil
}

// Guard the value receivers: activeProvider and CappedControlPlaneConfig assert
// on a value, so a pointer receiver would fail the assertion silently. Both
// Nico guards live in one block so the two provider seams stay paired.
var (
	_ ControlPlaneScaleDownRanker = NicoControlPlaneProvider{}
	_ ControlPlaneRebalanceCapper = NicoControlPlaneProvider{}
)

// ScaleDownRank ranks a node by its instance type's position in the provider's
// ordered preference: the index of the node's type (0 = most preferred, shed
// last), or len(preference) for a type that is off the list or not yet placed
// (shed first). It satisfies ControlPlaneScaleDownRanker.
func (p NicoControlPlaneProvider) ScaleDownRank(node Node) int {
	id := node.NicoInstanceTypeID()
	for i, pref := range p.InstanceTypePreference {
		if pref == id {
			return i
		}
	}
	return len(p.InstanceTypePreference)
}

// ControlPlaneRebalanceCapper is the sibling of ControlPlaneScaleDownRanker: a
// provider implements it so the rebalance step can cap how far a surge node may
// fall back from the primary (most-preferred) type. Where the ranker orders
// nodes by position in an ordered placement preference, the capper truncates
// that same preference to a prefix, so a surge that cannot get its primary type
// falls back only to types strictly better than the node it is replacing, never
// to one no better.
//
// Extensibility: a provider opts into rebalance by implementing BOTH this and
// ControlPlaneScaleDownRanker, and by being a case in CappedControlPlaneConfig
// (the one place, besides activeProvider, that knows the provider union). A
// provider that expresses no ordered preference -- a single placement type, or
// a scheduler whose choice is not captured in the control-plane config snapshot
// -- implements neither, so ScaleDownRanker returns nil and the rebalance step
// is a clean no-op for it. The cap rides the node's pinned control-plane-config
// snapshot: the step pins the surge to a snapshot whose preference is already
// truncated, so no provider provisioning code changes -- the existing in-place
// instance-type walk reads the shorter list and is bounded automatically.
//
// Contract: a provider's ScaleDownRank MUST return the index into the same
// ordered preference cappedToRank truncates. preference[:rank] then drops
// exactly the types at or worse than rank; if the two drift apart the cap is
// meaningless. The compile-time guard below pairs with ScaleDownRanker's.
type ControlPlaneRebalanceCapper interface {
	// cappedToRank returns this provider payload with its ordered instance-type
	// preference truncated to the first rank entries; a rank at or beyond the
	// list length returns it unchanged. The receiver is not mutated. It is
	// unexported so the interface is sealed to the provider types in this
	// package, the same way the whole provider union is internal to db.
	cappedToRank(rank int) any
}

// cappedToRank truncates the Nico instance-type preference to its first rank
// entries, the prefix of types at least as preferred as rank. It pairs with
// ScaleDownRank, which returns a node's index into this same preference, so
// preference[:rank] keeps exactly the types strictly better than rank.
func (p NicoControlPlaneProvider) cappedToRank(rank int) any {
	out := *cloneNicoControlPlaneProvider(&p)
	if rank >= 0 && rank < len(out.InstanceTypePreference) {
		out.InstanceTypePreference = append([]uuid.UUID(nil), out.InstanceTypePreference[:rank]...)
	}
	return out
}

// CappedControlPlaneConfig returns a copy of the spec with the active provider's
// ordered placement preference truncated to the first rank entries, and whether
// the provider supports capping. It is the one place besides activeProvider that
// knows the provider union: add a provider to rebalance by giving its payload a
// cappedToRank method and a case here. A provider that does not implement
// ControlPlaneRebalanceCapper returns (spec, false), and the rebalance step
// treats it as a no-op.
func (c ControlPlaneConfigSpec) CappedControlPlaneConfig(rank int) (ControlPlaneConfigSpec, bool) {
	capper, ok := c.activeProvider().(ControlPlaneRebalanceCapper)
	if !ok {
		return c, false
	}
	out := c
	switch p := capper.cappedToRank(rank).(type) {
	case NicoControlPlaneProvider:
		out.SetNico(p)
	default:
		return c, false
	}
	return out, true
}
