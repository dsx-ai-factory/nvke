// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"github.com/NVIDIA/nke/controller/internal/db"
)

type mockDB struct {
	mock.Mock
	db.DB
}

func (m *mockDB) GetNode(ctx context.Context, id uuid.UUID) (db.Node, error) {
	args := m.Called(ctx, id)
	if args.Get(0) != nil {
		return args.Get(0).(db.Node), args.Error(1)
	}
	return db.Node{}, args.Error(1)
}

func TestChooseNodeToPatch(t *testing.T) {
	tests := []struct {
		name             string
		remainingNodeIDs []uuid.UUID
		setupMock        func(*mockDB, []uuid.UUID)
		wantError        bool
		wantErrorType    string
		verifyResult     func(*testing.T, ChooseNodeToPatchResult)
		verifyError      func(*testing.T, error)
	}{
		{
			name:             "chooses healthy node",
			remainingNodeIDs: []uuid.UUID{uuid.New()},
			setupMock: func(m *mockDB, nodeIDs []uuid.UUID) {
				m.On("GetNode", mock.Anything, nodeIDs[0]).Return(createHealthyNode(nodeIDs[0], "node-1", "10.0.0.1"), nil)
			},
			verifyResult: func(t *testing.T, result ChooseNodeToPatchResult) {
				t.Helper()
				assert.NotEqual(t, uuid.Nil, result.NodeID)
				assert.Equal(t, "node-1", result.NodeName)
				assert.Equal(t, "10.0.0.1", result.NodeIP)
			},
		},
		{
			name:             "chooses first healthy node when multiple nodes available",
			remainingNodeIDs: []uuid.UUID{uuid.New(), uuid.New(), uuid.New()},
			setupMock: func(m *mockDB, nodeIDs []uuid.UUID) {
				// First node is healthy - should be chosen
				m.On("GetNode", mock.Anything, nodeIDs[0]).Return(createHealthyNode(nodeIDs[0], "node-1", "10.0.0.1"), nil)
				// Other nodes should not be queried since we return early
			},
			verifyResult: func(t *testing.T, result ChooseNodeToPatchResult) {
				t.Helper()
				assert.NotEqual(t, uuid.Nil, result.NodeID)
				assert.Equal(t, "node-1", result.NodeName)
			},
		},
		{
			name:             "chooses unhealthy node when no healthy nodes available",
			remainingNodeIDs: []uuid.UUID{uuid.New()},
			setupMock: func(m *mockDB, nodeIDs []uuid.UUID) {
				m.On("GetNode", mock.Anything, nodeIDs[0]).Return(createUnhealthyNode(nodeIDs[0], "node-1", "10.0.0.1"), nil)
			},
			verifyResult: func(t *testing.T, result ChooseNodeToPatchResult) {
				t.Helper()
				assert.NotEqual(t, uuid.Nil, result.NodeID)
				assert.Equal(t, "node-1", result.NodeName)
			},
		},
		{
			name:             "chooses first unhealthy node when multiple unhealthy nodes available",
			remainingNodeIDs: []uuid.UUID{uuid.New(), uuid.New()},
			setupMock: func(m *mockDB, nodeIDs []uuid.UUID) {
				m.On("GetNode", mock.Anything, nodeIDs[0]).Return(createUnhealthyNode(nodeIDs[0], "node-1", "10.0.0.1"), nil)
				m.On("GetNode", mock.Anything, nodeIDs[1]).Return(createUnhealthyNode(nodeIDs[1], "node-2", "10.0.0.2"), nil)
			},
			verifyResult: func(t *testing.T, result ChooseNodeToPatchResult) {
				t.Helper()
				assert.NotEqual(t, uuid.Nil, result.NodeID)
				assert.Equal(t, "node-1", result.NodeName)
			},
		},
		{
			name:             "returns NoPatchableNodesError when all nodes are missing",
			remainingNodeIDs: []uuid.UUID{uuid.New(), uuid.New()},
			setupMock: func(m *mockDB, nodeIDs []uuid.UUID) {
				m.On("GetNode", mock.Anything, nodeIDs[0]).Return(nil, db.ErrNotFound)
				m.On("GetNode", mock.Anything, nodeIDs[1]).Return(nil, db.ErrNotFound)
			},
			wantError:     true,
			wantErrorType: NoPatchableNodesErrorType,
			verifyError: func(t *testing.T, err error) {
				t.Helper()
				var appErr *temporal.ApplicationError
				require.ErrorAs(t, err, &appErr)
				assert.Equal(t, NoPatchableNodesErrorType, appErr.Type())
				assert.True(t, appErr.NonRetryable())

				var details NoPatchableNodesErrorDetails
				err = appErr.Details(&details)
				require.NoError(t, err)
				assert.Len(t, details.MissingNodeIDs, 2)
				assert.Empty(t, details.PendingNodeIDs)
				assert.Empty(t, details.ProvisioningNodeIDs)
				assert.Empty(t, details.DeletingNodeIDs)
			},
		},
		{
			name:             "returns NoPatchableNodesError when all nodes are deleting",
			remainingNodeIDs: []uuid.UUID{uuid.New(), uuid.New()},
			setupMock: func(m *mockDB, nodeIDs []uuid.UUID) {
				m.On("GetNode", mock.Anything, nodeIDs[0]).Return(createDeletingNode(nodeIDs[0]), nil)
				m.On("GetNode", mock.Anything, nodeIDs[1]).Return(createDeletingNode(nodeIDs[1]), nil)
			},
			wantError:     true,
			wantErrorType: NoPatchableNodesErrorType,
			verifyError: func(t *testing.T, err error) {
				t.Helper()
				var appErr *temporal.ApplicationError
				require.ErrorAs(t, err, &appErr)
				assert.Equal(t, NoPatchableNodesErrorType, appErr.Type())

				var details NoPatchableNodesErrorDetails
				err = appErr.Details(&details)
				require.NoError(t, err)
				assert.Empty(t, details.MissingNodeIDs)
				assert.Empty(t, details.PendingNodeIDs)
				assert.Empty(t, details.ProvisioningNodeIDs)
				assert.Len(t, details.DeletingNodeIDs, 2)
			},
		},
		{
			name:             "returns NoPatchableNodesError when all nodes are pending",
			remainingNodeIDs: []uuid.UUID{uuid.New(), uuid.New()},
			setupMock: func(m *mockDB, nodeIDs []uuid.UUID) {
				m.On("GetNode", mock.Anything, nodeIDs[0]).Return(createPendingNode(nodeIDs[0]), nil)
				m.On("GetNode", mock.Anything, nodeIDs[1]).Return(createPendingNode(nodeIDs[1]), nil)
			},
			wantError:     true,
			wantErrorType: NoPatchableNodesErrorType,
			verifyError: func(t *testing.T, err error) {
				t.Helper()
				var appErr *temporal.ApplicationError
				require.ErrorAs(t, err, &appErr)
				assert.Equal(t, NoPatchableNodesErrorType, appErr.Type())

				var details NoPatchableNodesErrorDetails
				err = appErr.Details(&details)
				require.NoError(t, err)
				assert.Empty(t, details.MissingNodeIDs)
				assert.Len(t, details.PendingNodeIDs, 2)
				assert.Empty(t, details.ProvisioningNodeIDs)
				assert.Empty(t, details.DeletingNodeIDs)
			},
		},
		{
			name:             "returns NoPatchableNodesError when all nodes are provisioning",
			remainingNodeIDs: []uuid.UUID{uuid.New(), uuid.New()},
			setupMock: func(m *mockDB, nodeIDs []uuid.UUID) {
				m.On("GetNode", mock.Anything, nodeIDs[0]).Return(createProvisioningNode(nodeIDs[0]), nil)
				m.On("GetNode", mock.Anything, nodeIDs[1]).Return(createProvisioningNode(nodeIDs[1]), nil)
			},
			wantError:     true,
			wantErrorType: NoPatchableNodesErrorType,
			verifyError: func(t *testing.T, err error) {
				t.Helper()
				var appErr *temporal.ApplicationError
				require.ErrorAs(t, err, &appErr)
				assert.Equal(t, NoPatchableNodesErrorType, appErr.Type())

				var details NoPatchableNodesErrorDetails
				err = appErr.Details(&details)
				require.NoError(t, err)
				assert.Empty(t, details.MissingNodeIDs)
				assert.Empty(t, details.PendingNodeIDs)
				assert.Len(t, details.ProvisioningNodeIDs, 2)
				assert.Empty(t, details.DeletingNodeIDs)
			},
		},
		{
			name:             "returns NoPatchableNodesError with mixed ineligible states",
			remainingNodeIDs: []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()},
			setupMock: func(m *mockDB, nodeIDs []uuid.UUID) {
				m.On("GetNode", mock.Anything, nodeIDs[0]).Return(nil, db.ErrNotFound)                     // missing
				m.On("GetNode", mock.Anything, nodeIDs[1]).Return(createDeletingNode(nodeIDs[1]), nil)     // deleting
				m.On("GetNode", mock.Anything, nodeIDs[2]).Return(createPendingNode(nodeIDs[2]), nil)      // pending
				m.On("GetNode", mock.Anything, nodeIDs[3]).Return(createProvisioningNode(nodeIDs[3]), nil) // provisioning
			},
			wantError:     true,
			wantErrorType: NoPatchableNodesErrorType,
			verifyError: func(t *testing.T, err error) {
				t.Helper()
				var appErr *temporal.ApplicationError
				require.ErrorAs(t, err, &appErr)
				assert.Equal(t, NoPatchableNodesErrorType, appErr.Type())

				var details NoPatchableNodesErrorDetails
				err = appErr.Details(&details)
				require.NoError(t, err)
				assert.Len(t, details.MissingNodeIDs, 1)
				assert.Len(t, details.PendingNodeIDs, 1)
				assert.Len(t, details.ProvisioningNodeIDs, 1)
				assert.Len(t, details.DeletingNodeIDs, 1)
			},
		},
		{
			name:             "prioritizes healthy node over unhealthy nodes",
			remainingNodeIDs: []uuid.UUID{uuid.New(), uuid.New()},
			setupMock: func(m *mockDB, nodeIDs []uuid.UUID) {
				// First node is unhealthy
				m.On("GetNode", mock.Anything, nodeIDs[0]).Return(createUnhealthyNode(nodeIDs[0], "unhealthy-node", "10.0.0.1"), nil)
				// Second node is healthy - should be chosen
				m.On("GetNode", mock.Anything, nodeIDs[1]).Return(createHealthyNode(nodeIDs[1], "healthy-node", "10.0.0.2"), nil)
			},
			verifyResult: func(t *testing.T, result ChooseNodeToPatchResult) {
				t.Helper()
				assert.NotEqual(t, uuid.Nil, result.NodeID)
				assert.Equal(t, "healthy-node", result.NodeName)
			},
		},
		{
			name:             "returns error when database query fails",
			remainingNodeIDs: []uuid.UUID{uuid.New()},
			setupMock: func(m *mockDB, nodeIDs []uuid.UUID) {
				m.On("GetNode", mock.Anything, nodeIDs[0]).Return(nil, errors.New("database connection failed"))
			},
			wantError: true,
			verifyError: func(t *testing.T, err error) {
				t.Helper()
				require.Error(t, err)
				assert.Contains(t, err.Error(), "failed to query node from DB")
			},
		},
		{
			name:             "skips ineligible nodes and chooses healthy node",
			remainingNodeIDs: []uuid.UUID{uuid.New(), uuid.New(), uuid.New()},
			setupMock: func(m *mockDB, nodeIDs []uuid.UUID) {
				m.On("GetNode", mock.Anything, nodeIDs[0]).Return(createDeletingNode(nodeIDs[0]), nil)
				m.On("GetNode", mock.Anything, nodeIDs[1]).Return(createPendingNode(nodeIDs[1]), nil)
				m.On("GetNode", mock.Anything, nodeIDs[2]).Return(createHealthyNode(nodeIDs[2], "healthy-node", "10.0.0.3"), nil)
			},
			verifyResult: func(t *testing.T, result ChooseNodeToPatchResult) {
				t.Helper()
				assert.NotEqual(t, uuid.Nil, result.NodeID)
				assert.Equal(t, "healthy-node", result.NodeName)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockDB{}
			if tt.setupMock != nil {
				tt.setupMock(mockDB, tt.remainingNodeIDs)
			}

			activities := &Activities{
				db: mockDB,
			}

			result, err := activities.ChooseNodeToPatch(context.Background(), ChooseNodeToPatchInput{
				RemainingNodeIDs: tt.remainingNodeIDs,
			})

			if tt.wantError {
				require.Error(t, err)
				if tt.verifyError != nil {
					tt.verifyError(t, err)
				}
				if tt.wantErrorType != "" {
					var appErr *temporal.ApplicationError
					if errors.As(err, &appErr) {
						assert.Equal(t, tt.wantErrorType, appErr.Type())
					}
				}
			} else {
				require.NoError(t, err)
				if tt.verifyResult != nil {
					tt.verifyResult(t, result)
				}
			}

			mockDB.AssertExpectations(t)
		})
	}
}

// Helper functions to create test nodes

func createHealthyNode(id uuid.UUID, name, ip string) db.Node {
	now := time.Now()
	return db.Node{
		ID:   id,
		Name: name,
		IP:   ip,
		Conditions: db.NodeConditions{
			Pending: db.Condition{
				Status:             db.ConditionStatusFalse,
				LastTransitionTime: now,
			},
			Provisioning: db.Condition{
				Status:             db.ConditionStatusFalse,
				LastTransitionTime: now,
			},
			Deleting: db.Condition{
				Status:             db.ConditionStatusFalse,
				LastTransitionTime: now,
			},
			KubernetesReady: db.Condition{
				Status:             db.ConditionStatusTrue,
				LastTransitionTime: now,
			},
			KubernetesUnschedulable: db.Condition{
				Status:             db.ConditionStatusFalse,
				LastTransitionTime: now,
			},
			NodeAgentReady: db.Condition{
				Status:             db.ConditionStatusTrue,
				LastTransitionTime: now,
			},
		},
	}
}

func createUnhealthyNode(id uuid.UUID, name, ip string) db.Node {
	now := time.Now()
	return db.Node{
		ID:   id,
		Name: name,
		IP:   ip,
		Conditions: db.NodeConditions{
			Pending: db.Condition{
				Status:             db.ConditionStatusFalse,
				LastTransitionTime: now,
			},
			Provisioning: db.Condition{
				Status:             db.ConditionStatusFalse,
				LastTransitionTime: now,
			},
			Deleting: db.Condition{
				Status:             db.ConditionStatusFalse,
				LastTransitionTime: now,
			},
			KubernetesReady: db.Condition{
				Status:             db.ConditionStatusFalse, // Unhealthy
				LastTransitionTime: now,
			},
			KubernetesUnschedulable: db.Condition{
				Status:             db.ConditionStatusFalse,
				LastTransitionTime: now,
			},
			NodeAgentReady: db.Condition{
				Status:             db.ConditionStatusTrue,
				LastTransitionTime: now,
			},
		},
	}
}

func createDeletingNode(id uuid.UUID) db.Node {
	now := time.Now()
	return db.Node{
		ID:   id,
		Name: "deleting-node",
		IP:   "10.0.0.1",
		Conditions: db.NodeConditions{
			Deleting: db.Condition{
				Status:             db.ConditionStatusTrue,
				LastTransitionTime: now,
			},
		},
	}
}

func createPendingNode(id uuid.UUID) db.Node {
	now := time.Now()
	return db.Node{
		ID:   id,
		Name: "pending-node",
		IP:   "10.0.0.1",
		Conditions: db.NodeConditions{
			Pending: db.Condition{
				Status:             db.ConditionStatusTrue,
				LastTransitionTime: now,
			},
		},
	}
}

func createProvisioningNode(id uuid.UUID) db.Node {
	now := time.Now()
	return db.Node{
		ID:   id,
		Name: "provisioning-node",
		IP:   "10.0.0.1",
		Conditions: db.NodeConditions{
			Provisioning: db.Condition{
				Status:             db.ConditionStatusTrue,
				LastTransitionTime: now,
			},
		},
	}
}
