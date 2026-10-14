// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodestatus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

// A node carrying only the legacy-domain condition is still recognized when a
// different domain is configured, and the configured form wins when both exist.
func TestApplyKubernetesNodeHealthReadsBothConditionDomains(t *testing.T) {
	t.Parallel()

	custom := keys.New("nvke.example.com", "")

	for _, tc := range []struct {
		name       string
		conditions []corev1.NodeCondition
		marked     bool
	}{
		{
			name: "legacy form only",
			conditions: []corev1.NodeCondition{
				{Type: corev1.NodeConditionType(custom.NeedsRecycle.Legacy()), Status: corev1.ConditionTrue},
			},
			marked: true,
		},
		{
			name: "configured form only",
			conditions: []corev1.NodeCondition{
				{Type: corev1.NodeConditionType(custom.NeedsRecycle.String()), Status: corev1.ConditionTrue},
			},
			marked: true,
		},
		{
			name: "configured form wins over legacy",
			conditions: []corev1.NodeCondition{
				{Type: corev1.NodeConditionType(custom.NeedsRecycle.Legacy()), Status: corev1.ConditionTrue},
				{Type: corev1.NodeConditionType(custom.NeedsRecycle.String()), Status: corev1.ConditionFalse},
			},
			marked: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			controllerNode := &db.Node{}
			kubeNode := &corev1.Node{Status: corev1.NodeStatus{Conditions: tc.conditions}}
			applyKubernetesNodeHealth(controllerNode, kubeNode, custom)

			// Only a True NeedsRecycle marks the node.
			assert.Equal(t, tc.marked, controllerNode.Conditions.MarkedForRecycling.Status == db.ConditionStatusTrue)
		})
	}
}
