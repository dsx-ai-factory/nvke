// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package filters

import (
	"github.com/NVIDIA/nke/controller/internal/db"
)

type Cluster struct{}

func (f Cluster) Matches(_ db.Cluster) bool {
	return true
}

type NodePool struct {
	ControlPlane *bool
}

func (f NodePool) Matches(nodePool db.NodePool) bool {
	if f.ControlPlane != nil && nodePool.ControlPlane != *f.ControlPlane {
		return false
	}
	return true
}

type NodeSet struct{}

func (f NodeSet) Matches(_ db.NodeSet) bool {
	return true
}

type Node struct {
	ControlPlane *bool

	// Condition filters use asymmetric matching:
	//   - true: exact match (status = 'True')
	//   - false: not definitely true (includes 'False', 'Unknown', and missing/zero)
	Provisioning *bool
	Deleting     *bool
	Joined       *bool
}

func (f Node) Matches(node db.Node) bool {
	if f.ControlPlane != nil && node.ControlPlane != *f.ControlPlane {
		return false
	}
	if !conditionMatches(node.Conditions.Provisioning, f.Provisioning) {
		return false
	}
	if !conditionMatches(node.Conditions.Deleting, f.Deleting) {
		return false
	}
	if !conditionMatches(node.Conditions.Joined, f.Joined) {
		return false
	}
	return true
}

func conditionMatches(condition db.Condition, want *bool) bool {
	if want == nil {
		return true
	}
	if *want {
		return condition.Status == db.ConditionStatusTrue
	}
	return condition.Status != db.ConditionStatusTrue
}
