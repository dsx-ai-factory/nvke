// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package preflights

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout/nodeagent"
)

// RegisterDefaults installs every preflight in this package onto reg
// under its canonical name. Call once at worker startup; duplicates panic.
func RegisterDefaults(reg *rollout.Registry, dialer nodeagent.Dialer) {
	reg.RegisterClusterPreflight(PreflightNameNodeAgentReachable, NewNodeAgentReachableFunc(dialer))
	reg.RegisterClusterPreflight(PreflightNameKubeAPIHealthy, NewKubeAPIHealthyFunc(dialer))
	reg.RegisterNodePreflight(PreflightNameNodeAgentReady, NewNodeAgentReadyFunc(dialer))
	reg.RegisterNodePreflight(PreflightNameNodeAgentVersion, NewNodeAgentVersionFunc(dialer))
}

// DefaultClusterPreflights returns the cluster-scope safety floor the
// workflow factory prepends to every spec. Order: physical-layer →
// apiserver, so each layer can assume the prior one passed.
func DefaultClusterPreflights() []rollout.ClusterPreflightRef {
	return []rollout.ClusterPreflightRef{
		{Name: PreflightNameNodeAgentReachable},
		{Name: PreflightNameKubeAPIHealthy},
	}
}

// DefaultNodePreflights returns the node-scope safety floor; currently
// empty (the agent checks are either cluster-scope or opt-in).
func DefaultNodePreflights() []rollout.NodePreflightRef {
	return nil
}

// PrependDefaults is the SpecMutator the controller worker installs to
// prepend DefaultClusterPreflights + DefaultNodePreflights so the
// floor runs ahead of consumer-supplied entries. Returns a fresh
// spec; the caller's slices are not mutated.
func PrependDefaults(_ rollout.Cluster, spec rollout.RolloutSpec) rollout.RolloutSpec {
	if defCP := DefaultClusterPreflights(); len(defCP) > 0 {
		merged := make([]rollout.ClusterPreflightRef, 0, len(defCP)+len(spec.ClusterPreflights))
		merged = append(merged, defCP...)
		merged = append(merged, spec.ClusterPreflights...)
		spec.ClusterPreflights = merged
	} else {
		// No defaults to prepend: still return a fresh slice so the
		// caller's backing array can't be reached through the result.
		// append onto a nil slice preserves nil when spec is empty.
		spec.ClusterPreflights = append([]rollout.ClusterPreflightRef(nil), spec.ClusterPreflights...)
	}
	if defN := DefaultNodePreflights(); len(defN) > 0 {
		merged := make([]rollout.NodePreflightRef, 0, len(defN)+len(spec.NodePreflights))
		merged = append(merged, defN...)
		merged = append(merged, spec.NodePreflights...)
		spec.NodePreflights = merged
	} else {
		spec.NodePreflights = append([]rollout.NodePreflightRef(nil), spec.NodePreflights...)
	}
	return spec
}
