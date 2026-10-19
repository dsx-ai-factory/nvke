// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodestatus

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

func nodeWithConditions(conditions ...corev1.NodeCondition) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0"},
		Status:     corev1.NodeStatus{Conditions: conditions},
	}
}

func recycleCondition(conditionType string, status corev1.ConditionStatus, message string) corev1.NodeCondition {
	return corev1.NodeCondition{
		Type:               corev1.NodeConditionType(conditionType),
		Status:             status,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
}

// A node can carry the same condition under both domains: a stale one left by
// an earlier build and the one written now. Status.Conditions has no ordering
// guarantee, so the current domain has to win wherever it sits in the list --
// a legacy False masking a current True would silently drop a recycle request.
func TestApplyKubernetesNodeHealthPrefersTheCurrentDomain(t *testing.T) {
	tests := map[string]struct {
		conditions []corev1.NodeCondition
		want       db.ConditionStatus
		wantMsg    string
	}{
		"current domain only": {
			conditions: []corev1.NodeCondition{
				recycleCondition(keys.NeedsRecycle.String(), corev1.ConditionTrue, "current"),
			},
			want:    db.ConditionStatusTrue,
			wantMsg: "current",
		},
		"legacy domain only": {
			conditions: []corev1.NodeCondition{
				recycleCondition(keys.NeedsRecycle.Legacy(), corev1.ConditionTrue, "legacy"),
			},
			want:    db.ConditionStatusTrue,
			wantMsg: "legacy",
		},
		"legacy listed first does not mask the current one": {
			conditions: []corev1.NodeCondition{
				recycleCondition(keys.NeedsRecycle.Legacy(), corev1.ConditionFalse, "stale legacy"),
				recycleCondition(keys.NeedsRecycle.String(), corev1.ConditionTrue, "current"),
			},
			want:    db.ConditionStatusTrue,
			wantMsg: "current",
		},
		"current listed first wins too": {
			conditions: []corev1.NodeCondition{
				recycleCondition(keys.NeedsRecycle.String(), corev1.ConditionTrue, "current"),
				recycleCondition(keys.NeedsRecycle.Legacy(), corev1.ConditionFalse, "stale legacy"),
			},
			want:    db.ConditionStatusTrue,
			wantMsg: "current",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			node := &db.Node{}
			applyKubernetesNodeHealth(node, nodeWithConditions(params.conditions...))
			require.Equal(t, params.want, node.Conditions.MarkedForRecycling.Status)
			require.Equal(t, params.wantMsg, node.Conditions.MarkedForRecycling.Message)
		})
	}
}

// The same ordering rule applies to NeedsRepair, which shares the code path.
func TestApplyKubernetesNodeHealthPrefersTheCurrentDomainForRepair(t *testing.T) {
	node := &db.Node{}
	applyKubernetesNodeHealth(node, nodeWithConditions(
		recycleCondition(keys.NeedsRepair.Legacy(), corev1.ConditionFalse, "stale legacy"),
		recycleCondition(keys.NeedsRepair.String(), corev1.ConditionTrue, "current"),
	))
	require.Equal(t, db.ConditionStatusTrue, node.Conditions.MarkedForRepair.Status)
	require.Equal(t, "current", node.Conditions.MarkedForRepair.Message)
}

// A node carrying neither form reports Unknown and names the current key, not
// the legacy one. (applyKubernetesNodeHealth only promotes a True, so this is
// checked on the lookup itself.)
func TestFromKubeConditionKeyReportsMissingConditionUnderTheCurrentKey(t *testing.T) {
	got := fromKubeConditionKey(db.Condition{}, nodeWithConditions(
		recycleCondition(string(corev1.NodeReady), corev1.ConditionTrue, "kubelet is posting ready status"),
	), keys.NeedsRecycle)

	require.Equal(t, db.ConditionStatusUnknown, got.Status)
	require.Contains(t, got.Message, keys.NeedsRecycle.String())
	require.NotContains(t, got.Message, keys.NeedsRecycle.Legacy())
}
