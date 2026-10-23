// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"testing"

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
