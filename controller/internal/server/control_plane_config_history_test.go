// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestGetClusterControlPlaneConfigHistory(t *testing.T) {
	ctx := context.Background()
	clusterID := uuid.MustParse("550e8400-e29b-41d4-a716-446655440001")

	cfg1 := uuid.MustParse("11111111-0000-0000-0000-000000000001")
	cfg2 := uuid.MustParse("11111111-0000-0000-0000-000000000002")
	cfg3 := uuid.MustParse("11111111-0000-0000-0000-000000000003")
	cfg4 := uuid.MustParse("11111111-0000-0000-0000-000000000004")

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("returns ordered history with diffs and current marker", func(t *testing.T) {
		cluster := db.Cluster{ID: clusterID}
		// cfg2 is the cluster's current desired snapshot.
		cluster.ControlPlaneConfig.ConfigurationID = cfg2

		cfg1Spec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.33.6"}
		cfg1Spec.SetNico(db.NicoControlPlaneProvider{OSProfile: "os-v1", ConfigProfile: "cfg-v1"})
		cfg2Spec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2"}
		cfg2Spec.SetNico(db.NicoControlPlaneProvider{OSProfile: "os-v1", ConfigProfile: "cfg-v1"})
		cfg3Spec := db.ControlPlaneConfigSpec{KubernetesVersion: "1.34.2", TeleportEnabled: true}
		cfg3Spec.SetNico(db.NicoControlPlaneProvider{OSProfile: "os-v2", ConfigProfile: "cfg-v1"})

		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)
		mockDB.On("ListControlPlaneConfigurationsByCluster", mock.Anything, clusterID).Return(
			[]db.ControlPlaneConfiguration{
				{
					ID:        cfg1,
					ClusterID: clusterID,
					CreatedAt: base,
					Config:    cfg1Spec,
				},
				{
					ID:           cfg2,
					ClusterID:    clusterID,
					CreatedAt:    base.Add(time.Hour),
					ChangeReason: "bump kubernetes version",
					CreatedBy:    "operator@example.com",
					Config:       cfg2Spec,
				},
				{
					ID:        cfg3,
					ClusterID: clusterID,
					CreatedAt: base.Add(2 * time.Hour),
					Config:    cfg3Spec,
				},
				{
					// Identical to cfg3: a snapshot with a predecessor but no
					// rollout-relevant change. changedFields must still be [].
					ID:        cfg4,
					ClusterID: clusterID,
					CreatedAt: base.Add(3 * time.Hour),
					Config:    cfg3Spec,
				},
			}, nil,
		)

		svc := clusterService{db: mockDB}
		resp, err := svc.GetClusterControlPlaneConfigHistory(ctx, api.GetClusterControlPlaneConfigHistoryRequestObject{ClusterID: clusterID})
		require.NoError(t, err)

		out, ok := resp.(api.GetClusterControlPlaneConfigHistory200JSONResponse)
		require.True(t, ok, "expected 200 response, got %T", resp)
		require.Len(t, out.Changes, 4)

		// Oldest snapshot: no previous, so no changed fields — but a non-nil
		// slice so it serializes as [] (a required field), never null.
		assert.Equal(t, cfg1, out.Changes[0].ConfigurationID)
		assert.False(t, out.Changes[0].IsCurrent)
		require.NotNil(t, out.Changes[0].ChangedFields)
		assert.Empty(t, out.Changes[0].ChangedFields)

		// Unchanged snapshot with a predecessor: also a non-nil empty slice, not nil.
		assert.Equal(t, cfg4, out.Changes[3].ConfigurationID)
		require.NotNil(t, out.Changes[3].ChangedFields)
		assert.Empty(t, out.Changes[3].ChangedFields)

		// Second snapshot: only the Kubernetes version changed, and it is current.
		assert.Equal(t, cfg2, out.Changes[1].ConfigurationID)
		assert.True(t, out.Changes[1].IsCurrent)
		assert.Equal(t, "bump kubernetes version", out.Changes[1].ChangeReason)
		assert.Equal(t, "operator@example.com", out.Changes[1].CreatedBy)
		require.Len(t, out.Changes[1].ChangedFields, 1)
		assert.Equal(t, "kubernetesVersion", out.Changes[1].ChangedFields[0].Field)
		require.NotNil(t, out.Changes[1].ChangedFields[0].From)
		assert.Equal(t, "1.33.6", *out.Changes[1].ChangedFields[0].From)
		assert.Equal(t, "1.34.2", out.Changes[1].ChangedFields[0].To)

		// Third snapshot: OS profile and teleport toggled; not current.
		assert.Equal(t, cfg3, out.Changes[2].ConfigurationID)
		assert.False(t, out.Changes[2].IsCurrent)
		fields := map[string]string{}
		for _, f := range out.Changes[2].ChangedFields {
			fields[f.Field] = f.To
		}
		assert.Equal(t, "os-v2", fields["osProfile"])
		assert.Equal(t, "true", fields["teleportEnabled"])
		assert.NotContains(t, fields, "kubernetesVersion")

		mockDB.AssertExpectations(t)
	})

	t.Run("unknown cluster returns 404", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{}, db.ErrNotFound)

		svc := clusterService{db: mockDB}
		resp, err := svc.GetClusterControlPlaneConfigHistory(ctx, api.GetClusterControlPlaneConfigHistoryRequestObject{ClusterID: clusterID})
		require.NoError(t, err)
		_, ok := resp.(api.GetClusterControlPlaneConfigHistory404Response)
		assert.True(t, ok, "expected 404 response, got %T", resp)
		mockDB.AssertExpectations(t)
	})

	t.Run("no snapshots returns empty change list", func(t *testing.T) {
		mockDB := &testutil.MockDB{}
		mockDB.On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID}, nil)
		mockDB.On("ListControlPlaneConfigurationsByCluster", mock.Anything, clusterID).Return([]db.ControlPlaneConfiguration{}, nil)

		svc := clusterService{db: mockDB}
		resp, err := svc.GetClusterControlPlaneConfigHistory(ctx, api.GetClusterControlPlaneConfigHistoryRequestObject{ClusterID: clusterID})
		require.NoError(t, err)
		out, ok := resp.(api.GetClusterControlPlaneConfigHistory200JSONResponse)
		require.True(t, ok, "expected 200 response, got %T", resp)
		assert.Empty(t, out.Changes)
		mockDB.AssertExpectations(t)
	})
}
