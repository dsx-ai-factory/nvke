// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestActivities_SeedControlPlaneInstanceType(t *testing.T) {
	t.Parallel()

	preferred := uuid.New()
	second := uuid.New()
	persisted := uuid.New()
	configurationID := uuid.New()
	clusterID := uuid.New()

	type parameters struct {
		node               db.Node
		nodePool           db.NodePool
		preference         []uuid.UUID
		expectDBLookup     bool
		wantInstanceTypeID uuid.UUID
	}

	tests := map[string]parameters{
		"seeds first preference for a type-less control plane node": {
			node:               db.Node{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, ControlPlaneConfigurationID: configurationID},
			nodePool:           db.NodePool{Nico: &db.NicoNodePoolProvider{}},
			preference:         []uuid.UUID{preferred, second},
			expectDBLookup:     true,
			wantInstanceTypeID: preferred,
		},
		"keeps an already-resolved instance type": {
			node:               db.Node{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, ControlPlaneConfigurationID: configurationID},
			nodePool:           db.NodePool{Nico: &db.NicoNodePoolProvider{InstanceTypeID: persisted}},
			expectDBLookup:     false,
			wantInstanceTypeID: persisted,
		},
		"no-op for a non-control-plane node": {
			node:               db.Node{ID: uuid.New()},
			nodePool:           db.NodePool{Nico: &db.NicoNodePoolProvider{}},
			expectDBLookup:     false,
			wantInstanceTypeID: uuid.Nil,
		},
		"stays nil when the pinned preference is empty": {
			node:               db.Node{ID: uuid.New(), ClusterID: clusterID, ControlPlane: true, ControlPlaneConfigurationID: configurationID},
			nodePool:           db.NodePool{Nico: &db.NicoNodePoolProvider{}},
			preference:         nil,
			expectDBLookup:     true,
			wantInstanceTypeID: uuid.Nil,
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			nodePool := params.nodePool

			mockDB := &testutil.MockDB{}
			if params.expectDBLookup {
				mockDB.On("GetControlPlaneConfiguration", mock.Anything, params.node.ControlPlaneConfigurationID).
					Return(db.ControlPlaneConfiguration{
						ID:     configurationID,
						Config: nicoControlPlaneConfigSpec(params.preference),
					}, nil).Once()
			}
			a := &Activities{db: mockDB}

			updatedPool, err := a.seedControlPlaneInstanceType(context.Background(), params.node, nodePool)
			require.NoError(t, err)

			poolNico, err := updatedPool.NicoProvider()
			require.NoError(t, err)
			assert.Equal(t, params.wantInstanceTypeID, poolNico.InstanceTypeID)
			if !params.expectDBLookup {
				mockDB.AssertNotCalled(t, "GetControlPlaneConfiguration", mock.Anything, mock.Anything)
			}
			mockDB.AssertExpectations(t)
		})
	}
}

func nicoControlPlaneConfigSpec(instanceTypePreference []uuid.UUID) db.ControlPlaneConfigSpec {
	return db.ControlPlaneConfigSpec{
		Nico: &db.NicoControlPlaneProvider{
			InstanceTypePreference: append([]uuid.UUID(nil), instanceTypePreference...),
		},
	}
}
