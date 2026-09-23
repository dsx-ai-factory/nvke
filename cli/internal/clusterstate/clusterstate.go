// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clusterstate reads a cluster's conditions.
//
// One reading, shared: `nkectl get clusters` prints it in a column and
// `nkectl bootstrap site` waits on it, and the two must not be able to disagree
// about what a cluster is doing.
package clusterstate

import api "github.com/dsx-ai-factory/nke/controller/api"

// Status returns a human-readable state derived from a cluster's conditions.
// Conditions are checked in priority order: terminal and destructive states
// first, then in-progress mutations, then base lifecycle.
func Status(cl *api.Cluster) string {
	c := cl.Conditions
	switch {
	case IsTrue(&c.Deleting):
		return "Deleting"
	case IsTrue(c.UpdateFailed):
		return "UpdateFailed"
	case c.UpdateInProgress != nil:
		return "Updating"
	case IsTrue(c.ControlPlaneUpgrading):
		return "Upgrading"
	case IsTrue(c.CpNodesRecycling):
		return "Recycling"
	case IsTrue(&c.Provisioning):
		return "Provisioning"
	case IsTrue(&c.Provisioned):
		return "Provisioned"
	default:
		return "Unknown"
	}
}

// IsProvisioned reports whether the cluster finished provisioning.
func IsProvisioned(cl *api.Cluster) bool { return IsTrue(&cl.Conditions.Provisioned) }

// IsTrue reports whether an optional condition is present and true.
func IsTrue(condition *api.Condition) bool {
	return condition != nil && condition.Status == api.ConditionStatusTrue
}
