// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestControlPlaneMutationBlockedByRetainedCredentialOwnership(t *testing.T) {
	tests := []struct {
		name     string
		update   *db.UpdateInProgress
		deleting bool
		want     bool
	}{
		{name: "no owner"},
		{name: "etcd encryption", update: &db.UpdateInProgress{Type: db.OperationTypeEtcdEncryption}, want: true},
		{name: "etcd key rotation", update: &db.UpdateInProgress{Type: db.OperationTypeEtcdKeyRotation}, want: true},
		{name: "certificate renewal", update: &db.UpdateInProgress{Type: db.OperationTypeCertRenewal}, want: true},
		{name: "kubernetes upgrade", update: &db.UpdateInProgress{Type: db.OperationTypeK8sUpgrade}},
		{name: "deletion preempts etcd encryption", update: &db.UpdateInProgress{Type: db.OperationTypeEtcdEncryption}, deleting: true},
		{name: "deletion preempts certificate renewal", update: &db.UpdateInProgress{Type: db.OperationTypeCertRenewal}, deleting: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := db.Cluster{Conditions: db.ClusterConditions{UpdateInProgress: tt.update, Deleting: db.NewCondition(tt.deleting, "")}}
			require.Equal(t, tt.want, db.ControlPlaneMutationBlocked(cluster))
		})
	}
}

func TestControlPlaneNodeRemovalBlockedExemptsOnlyRetiredRotationNodes(t *testing.T) {
	retired, other := uuid.New(), uuid.New()
	rotation := &db.EtcdKeyRotationStatus{
		WorkflowID: "owner", State: db.EtcdKeyRotationStateFailed, Phase: db.EtcdKeyRotationPhaseRewrite,
		RetiredNodes: []db.RetiredEtcdKeyRotationNode{{NodeID: retired, Phase: db.EtcdKeyRotationPhaseRewrite}},
	}
	tests := []struct {
		name     string
		update   *db.UpdateInProgress
		status   *db.EtcdKeyRotationStatus
		node     uuid.UUID
		deleting bool
		want     bool
	}{
		{name: "no owner", node: other},
		{name: "retired node under rotation lock", update: &db.UpdateInProgress{Type: db.OperationTypeEtcdKeyRotation}, status: rotation, node: retired},
		{name: "other node under rotation lock", update: &db.UpdateInProgress{Type: db.OperationTypeEtcdKeyRotation}, status: rotation, node: other, want: true},
		{name: "rotation lock without retirement", update: &db.UpdateInProgress{Type: db.OperationTypeEtcdKeyRotation}, node: retired, want: true},
		{name: "retired record under another owner", update: &db.UpdateInProgress{Type: db.OperationTypeCertRenewal}, status: rotation, node: retired, want: true},
		{name: "deletion preempts the lock", update: &db.UpdateInProgress{Type: db.OperationTypeEtcdKeyRotation}, node: other, deleting: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := db.Cluster{Conditions: db.ClusterConditions{
				UpdateInProgress: tt.update, EtcdKeyRotation: tt.status, Deleting: db.NewCondition(tt.deleting, ""),
			}}
			require.Equal(t, tt.want, db.ControlPlaneNodeRemovalBlocked(cluster, tt.node))
		})
	}
}
