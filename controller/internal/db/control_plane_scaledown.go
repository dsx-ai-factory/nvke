// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

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

// Guard the value receiver: a pointer receiver would fail activeProvider's
// type assertion silently.
var _ ControlPlaneScaleDownRanker = NicoControlPlaneProvider{}

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
