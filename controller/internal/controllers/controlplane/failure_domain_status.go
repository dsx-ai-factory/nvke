// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// reconcileFailureDomainStatus records where control-plane nodes landed and the spread condition,
// writing only on change.
func (r *reconciler) reconcileFailureDomainStatus(ctx context.Context, cluster db.Cluster, cp *controlPlane, fds *failureDomains) error {
	for i := range cp.nodes {
		// A node without an active Machine keeps its last observed domain.
		reported, active := fds.reportedByNodeName[cp.nodes[i].Name]
		if !active || reported == cp.nodes[i].ObservedFailureDomain {
			continue
		}
		if err := r.db.UpdateNodeObservedFailureDomain(ctx, cp.nodes[i].ID, reported); err != nil && !errors.Is(err, db.ErrNotFound) {
			return err
		}
		cp.nodes[i].ObservedFailureDomain = reported
	}

	prev := cmp.Or(cluster.ControlPlaneFailureDomains, &db.ControlPlaneFailureDomains{})
	next := controlPlaneFailureDomainStatus(fds.eligible, cp.nodes)
	spread := prev.Spread
	db.SetCondition(&spread, next.Spread)
	next.Spread = spread
	if slices.Equal(prev.Eligible, next.Eligible) && prev.Spread.Equal(next.Spread) {
		return nil
	}
	return r.db.UpdateClusterControlPlaneFailureDomains(ctx, cluster.ID, &next)
}

// controlPlaneFailureDomainStatus computes the spread condition over the published domains, or the domains nodes report
// when none are published. Deleting nodes are ignored; a domain is degraded when it has provisioned nodes and none is Ready.
func controlPlaneFailureDomainStatus(eligible []string, nodes []db.Node) db.ControlPlaneFailureDomains {
	status := db.ControlPlaneFailureDomains{Eligible: slices.Sorted(slices.Values(eligible))}
	var unplaced []string
	nodeCount, provisioned, ready := map[string]int{}, map[string]int{}, map[string]int{}
	for _, node := range nodes {
		if isMarkedForDeletion(node) {
			continue
		}
		domain := node.EffectiveFailureDomain()
		if domain == "" {
			if isProvisioned(node) {
				unplaced = append(unplaced, node.Name)
			}
			continue
		}
		nodeCount[domain]++
		if isProvisioned(node) {
			provisioned[domain]++
			if isKubernetesReady(node) {
				ready[domain]++
			}
		}
	}

	if len(status.Eligible) == 0 && len(nodeCount) == 0 {
		status.Spread = db.Condition{Status: db.ConditionStatusUnknown, Message: "No failure domains are published or reported."}
		return status
	}

	var shared, degraded []string
	for _, domain := range slices.Sorted(maps.Keys(nodeCount)) {
		if nodeCount[domain] > 1 {
			shared = append(shared, domain)
		}
		if provisioned[domain] > 0 && ready[domain] == 0 {
			degraded = append(degraded, domain)
		}
	}
	slices.Sort(unplaced)
	var problems []string
	if len(unplaced) > 0 {
		problems = append(problems, "no failure domain for "+strings.Join(unplaced, ", "))
	}
	if len(shared) > 0 {
		problems = append(problems, "nodes share "+strings.Join(shared, ", "))
	}
	if len(degraded) > 0 {
		problems = append(problems, "no Ready node in "+strings.Join(degraded, ", "))
	}
	if len(problems) > 0 {
		status.Spread = db.NewCondition(false, strings.Join(problems, "; ")+".")
		return status
	}
	status.Spread = db.NewCondition(true, "Each control-plane node is in its own failure domain.")
	return status
}
