// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"context"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// MockDB is a testify mock for db.DB. Set MockTx (or GetTx) before use so RunInTx invokes the callback with it.
type MockDB struct {
	mock.Mock
	db.DB
	// MockTx is the transaction passed to the RunInTx callback. Prefer setting GetTx in tests to avoid nil receiver issues.
	MockTx *MockTx
	// GetTx, if set, is used by RunInTx to obtain the tx (e.g. func() db.Tx { return mockTx }). Use when MockTx is not reliable.
	GetTx func() db.Tx
}

func (m *MockDB) Ping(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

// RunInTx runs the callback with the tx from GetTx() if set, otherwise MockTx.
// If both are nil or GetTx returns nil, the callback is not invoked.
func (m *MockDB) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	args := m.Called(ctx, fn)
	var tx db.Tx
	if m.GetTx != nil {
		tx = m.GetTx()
	}
	if tx == nil {
		tx = m.MockTx
	}
	if tx != nil {
		if err := fn(ctx, tx); err != nil {
			return err
		}
	}
	return args.Error(0)
}

func (m *MockDB) DeleteNodePool(ctx context.Context, nodePoolID uuid.UUID) error {
	args := m.Called(ctx, nodePoolID)
	return args.Error(0)
}

func (m *MockDB) CreateCluster(ctx context.Context, cluster *db.Cluster) (db.Cluster, error) {
	args := m.Called(ctx, cluster)
	v, ok := args.Get(0).(db.Cluster)
	if !ok {
		return db.Cluster{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) UpdateCluster(ctx context.Context, cluster *db.Cluster) (db.Cluster, error) {
	args := m.Called(ctx, cluster)
	v, ok := args.Get(0).(db.Cluster)
	if !ok {
		return db.Cluster{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) DeleteCluster(ctx context.Context, clusterID uuid.UUID) error {
	args := m.Called(ctx, clusterID)
	return args.Error(0)
}

func (m *MockDB) ListClusters(ctx context.Context, filters db.ClusterFilters) ([]db.Cluster, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.Cluster)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) GetCluster(ctx context.Context, id uuid.UUID) (db.Cluster, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Cluster)
	if !ok {
		return db.Cluster{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) SyncClusterControlPlaneConfigFromCPNodePools(ctx context.Context, clusterID uuid.UUID) error {
	args := m.Called(ctx, clusterID)
	return args.Error(0)
}

func (m *MockDB) CreateControlPlaneConfiguration(ctx context.Context, row *db.ControlPlaneConfiguration) (db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, row)
	v, ok := args.Get(0).(db.ControlPlaneConfiguration)
	if !ok {
		return db.ControlPlaneConfiguration{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) GetControlPlaneConfiguration(ctx context.Context, id uuid.UUID) (db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.ControlPlaneConfiguration)
	if !ok {
		return db.ControlPlaneConfiguration{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListControlPlaneConfigurationsByIDs(ctx context.Context, ids []uuid.UUID) ([]db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, ids)
	v, ok := args.Get(0).([]db.ControlPlaneConfiguration)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListControlPlaneConfigurationsByCluster(ctx context.Context, clusterID uuid.UUID) ([]db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, clusterID)
	v, ok := args.Get(0).([]db.ControlPlaneConfiguration)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListNodePools(ctx context.Context, filters db.NodePoolFilters) ([]db.NodePool, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.NodePool)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) GetNodePool(ctx context.Context, id uuid.UUID) (db.NodePool, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.NodePool)
	if !ok {
		return db.NodePool{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) CreateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	args := m.Called(ctx, nodePool)
	v, ok := args.Get(0).(db.NodePool)
	if !ok {
		return db.NodePool{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) UpdateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	args := m.Called(ctx, nodePool)
	v, ok := args.Get(0).(db.NodePool)
	if !ok {
		return db.NodePool{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) GetNodeSet(ctx context.Context, id uuid.UUID) (db.NodeSet, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.NodeSet)
	if !ok {
		return db.NodeSet{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) CreateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	args := m.Called(ctx, nodeSet)
	v, ok := args.Get(0).(db.NodeSet)
	if !ok {
		return db.NodeSet{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) UpdateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	args := m.Called(ctx, nodeSet)
	v, ok := args.Get(0).(db.NodeSet)
	if !ok {
		return db.NodeSet{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) DeleteNodeSet(ctx context.Context, id uuid.UUID) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockDB) ListNodes(ctx context.Context, filters db.NodeFilters) ([]db.Node, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.Node)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) GetNode(ctx context.Context, id uuid.UUID) (db.Node, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) GetNodeForUpdate(ctx context.Context, id uuid.UUID) (db.Node, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) CreateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	args := m.Called(ctx, node)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) UpdateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	args := m.Called(ctx, node)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) UpdateNodeTeleportRegistered(ctx context.Context, nodeID uuid.UUID, condition db.Condition) error {
	args := m.Called(ctx, nodeID, condition)
	return args.Error(0)
}

func (m *MockDB) DeleteNode(ctx context.Context, id uuid.UUID) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockDB) TryAcquireNodeProvisioningLock(ctx context.Context, nodeID uuid.UUID) (bool, error) {
	args := m.Called(ctx, nodeID)
	return args.Bool(0), args.Error(1)
}

func (m *MockDB) ListNodeSets(ctx context.Context, filters db.NodeSetFilters) ([]db.NodeSet, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.NodeSet)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

// MockTx is a testify mock for db.Tx.
type MockTx struct {
	mock.Mock
	db.Tx
}

func (m *MockTx) CreateCluster(ctx context.Context, cluster *db.Cluster) (db.Cluster, error) {
	args := m.Called(ctx, cluster)
	v, ok := args.Get(0).(db.Cluster)
	if !ok {
		return db.Cluster{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetCluster(ctx context.Context, id uuid.UUID) (db.Cluster, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Cluster)
	if !ok {
		return db.Cluster{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetClusterForUpdate(ctx context.Context, id uuid.UUID) (db.Cluster, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Cluster)
	if !ok {
		return db.Cluster{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) UpdateCluster(ctx context.Context, cluster *db.Cluster) (db.Cluster, error) {
	args := m.Called(ctx, cluster)
	v, ok := args.Get(0).(db.Cluster)
	if !ok {
		return db.Cluster{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) SyncClusterControlPlaneConfigFromCPNodePools(ctx context.Context, clusterID uuid.UUID) error {
	args := m.Called(ctx, clusterID)
	return args.Error(0)
}

func (m *MockTx) CreateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	args := m.Called(ctx, nodePool)
	v, ok := args.Get(0).(db.NodePool)
	if !ok {
		return db.NodePool{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetNodePool(ctx context.Context, id uuid.UUID) (db.NodePool, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.NodePool)
	if !ok {
		return db.NodePool{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) CreateControlPlaneConfiguration(ctx context.Context, row *db.ControlPlaneConfiguration) (db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, row)
	v, ok := args.Get(0).(db.ControlPlaneConfiguration)
	if !ok {
		return db.ControlPlaneConfiguration{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetControlPlaneConfiguration(ctx context.Context, id uuid.UUID) (db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.ControlPlaneConfiguration)
	if !ok {
		return db.ControlPlaneConfiguration{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) ListControlPlaneConfigurationsByIDs(ctx context.Context, ids []uuid.UUID) ([]db.ControlPlaneConfiguration, error) {
	args := m.Called(ctx, ids)
	v, ok := args.Get(0).([]db.ControlPlaneConfiguration)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) ListNodePools(ctx context.Context, filters db.NodePoolFilters) ([]db.NodePool, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.NodePool)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetNodePoolForUpdate(ctx context.Context, id uuid.UUID) (db.NodePool, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.NodePool)
	if !ok {
		return db.NodePool{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) UpdateNodePool(ctx context.Context, nodePool *db.NodePool) (db.NodePool, error) {
	args := m.Called(ctx, nodePool)
	v, ok := args.Get(0).(db.NodePool)
	if !ok {
		return db.NodePool{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) DeleteNodePool(ctx context.Context, id uuid.UUID) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockTx) GetNodeSet(ctx context.Context, id uuid.UUID) (db.NodeSet, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.NodeSet)
	if !ok {
		return db.NodeSet{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) CreateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	args := m.Called(ctx, nodeSet)
	v, ok := args.Get(0).(db.NodeSet)
	if !ok {
		return db.NodeSet{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) ListNodeSets(ctx context.Context, filters db.NodeSetFilters) ([]db.NodeSet, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.NodeSet)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) UpdateNodeSet(ctx context.Context, nodeSet *db.NodeSet) (db.NodeSet, error) {
	args := m.Called(ctx, nodeSet)
	v, ok := args.Get(0).(db.NodeSet)
	if !ok {
		return db.NodeSet{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) DeleteNodeSet(ctx context.Context, id uuid.UUID) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockTx) ListClusterNodeNames(ctx context.Context, clusterID uuid.UUID) ([]string, error) {
	args := m.Called(ctx, clusterID)
	v, ok := args.Get(0).([]string)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) ListNodes(ctx context.Context, filters db.NodeFilters) ([]db.Node, error) {
	args := m.Called(ctx, filters)
	v, ok := args.Get(0).([]db.Node)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) ListNodesEligibleForRecycling(ctx context.Context, nodePoolID uuid.UUID, limit int) ([]db.Node, error) {
	args := m.Called(ctx, nodePoolID, limit)
	v, ok := args.Get(0).([]db.Node)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) CreateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	args := m.Called(ctx, node)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetNode(ctx context.Context, id uuid.UUID) (db.Node, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) GetNodeForUpdate(ctx context.Context, id uuid.UUID) (db.Node, error) {
	args := m.Called(ctx, id)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) UpdateNode(ctx context.Context, node *db.Node) (db.Node, error) {
	args := m.Called(ctx, node)
	v, ok := args.Get(0).(db.Node)
	if !ok {
		return db.Node{}, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) UpdateNodeTeleportRegistered(ctx context.Context, nodeID uuid.UUID, condition db.Condition) error {
	args := m.Called(ctx, nodeID, condition)
	return args.Error(0)
}

func (m *MockTx) DeleteNode(ctx context.Context, id uuid.UUID) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockTx) TryAcquireNodeProvisioningLock(ctx context.Context, nodeID uuid.UUID) (bool, error) {
	args := m.Called(ctx, nodeID)
	return args.Bool(0), args.Error(1)
}

func (m *MockDB) ListNodeConditions(ctx context.Context) ([]db.NodeConditionStatus, error) {
	args := m.Called(ctx)
	v, ok := args.Get(0).([]db.NodeConditionStatus)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListNodePoolMetrics(ctx context.Context) ([]db.NodePoolMetrics, error) {
	args := m.Called(ctx)
	v, ok := args.Get(0).([]db.NodePoolMetrics)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListNodeInfo(ctx context.Context) ([]db.NodeInfo, error) {
	args := m.Called(ctx)
	v, ok := args.Get(0).([]db.NodeInfo)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockDB) ListNodeLifecycleElapsed(ctx context.Context) ([]db.NodeLifecycleElapsed, error) {
	args := m.Called(ctx)
	v, ok := args.Get(0).([]db.NodeLifecycleElapsed)
	if !ok {
		return nil, args.Error(1)
	}
	return v, args.Error(1)
}

func (m *MockTx) CreateWorkflowOutboxEntry(
	ctx context.Context,
	workflowType string,
	workflowID string,
	workflowInput interface{},
) (db.WorkflowOutbox, error) {
	args := m.Called(ctx, workflowType, workflowID, workflowInput)
	v, ok := args.Get(0).(db.WorkflowOutbox)
	if !ok {
		return db.WorkflowOutbox{}, args.Error(1)
	}
	return v, args.Error(1)
}
