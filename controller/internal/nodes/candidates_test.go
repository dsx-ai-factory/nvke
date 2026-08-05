// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package nodes_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/nodes"
)

func TestPickControlPlaneRunbookCandidates(t *testing.T) {
	healthyA := node("a", db.ConditionStatusTrue, db.ConditionStatusTrue, db.ConditionStatusFalse)
	healthyB := node("b", db.ConditionStatusTrue, db.ConditionStatusTrue, db.ConditionStatusFalse)
	healthyC := node("c", db.ConditionStatusTrue, db.ConditionStatusTrue, db.ConditionStatusFalse)
	unreachable := node("dead", db.ConditionStatusTrue, db.ConditionStatusFalse, db.ConditionStatusFalse)
	unknown := node("unknown", db.ConditionStatusTrue, db.ConditionStatusUnknown, db.ConditionStatusFalse)
	deleting := node("deleting", db.ConditionStatusTrue, db.ConditionStatusTrue, db.ConditionStatusTrue)
	notJoined := node("not-joined", db.ConditionStatusFalse, db.ConditionStatusTrue, db.ConditionStatusFalse)

	type parameters struct {
		input                 []db.Node
		wantNames             []string
		wantHealthyCandidates bool
		wantCandidates        int
	}

	tests := map[string]parameters{
		"all joined and healthy returns all": {
			input:                 []db.Node{healthyA, healthyB, healthyC},
			wantNames:             []string{"a", "b", "c"},
			wantHealthyCandidates: true,
			wantCandidates:        3,
		},
		"skips unreachable joined node": {
			input:                 []db.Node{healthyA, unreachable, healthyB},
			wantNames:             []string{"a", "b"},
			wantHealthyCandidates: true,
			wantCandidates:        2,
		},
		"skips KubernetesReady unknown joined node": {
			input:                 []db.Node{healthyA, unknown, healthyB},
			wantNames:             []string{"a", "b"},
			wantHealthyCandidates: true,
			wantCandidates:        2,
		},
		"skips deleting node even if KubernetesReady true": {
			input:                 []db.Node{healthyA, deleting, healthyB},
			wantNames:             []string{"a", "b"},
			wantHealthyCandidates: true,
			wantCandidates:        2,
		},
		"skips not joined node entirely": {
			input:                 []db.Node{healthyA, notJoined, healthyB},
			wantNames:             []string{"a", "b"},
			wantHealthyCandidates: true,
			wantCandidates:        2,
		},
		"falls back to joined when no node is healthy": {
			input:                 []db.Node{unreachable, unknown},
			wantNames:             []string{"dead", "unknown"},
			wantHealthyCandidates: false,
			wantCandidates:        2,
		},
		"fallback excludes not joined nodes": {
			input:                 []db.Node{unreachable, notJoined},
			wantNames:             []string{"dead"},
			wantHealthyCandidates: false,
			wantCandidates:        1,
		},
		"no joined nodes returns empty": {
			input:                 []db.Node{notJoined},
			wantNames:             []string{},
			wantHealthyCandidates: false,
			wantCandidates:        0,
		},
		"empty input returns empty": {
			input:                 []db.Node{},
			wantNames:             []string{},
			wantHealthyCandidates: false,
			wantCandidates:        0,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			candidates, healthyCandidates := nodes.PickControlPlaneRunbookCandidates(params.input)

			require.Len(t, candidates, params.wantCandidates)
			assert.Equal(t, params.wantHealthyCandidates, healthyCandidates)

			gotNames := make([]string, 0, len(candidates))
			for _, c := range candidates {
				gotNames = append(gotNames, c.Name)
			}
			assert.ElementsMatch(t, params.wantNames, gotNames)
		})
	}
}

func node(name string, joined, kubernetesReady, deleting db.ConditionStatus) db.Node {
	return db.Node{
		ID:   uuid.New(),
		Name: name,
		Conditions: db.NodeConditions{
			Joined:          db.Condition{Status: joined},
			KubernetesReady: db.Condition{Status: kubernetesReady},
			Deleting:        db.Condition{Status: deleting},
		},
	}
}
