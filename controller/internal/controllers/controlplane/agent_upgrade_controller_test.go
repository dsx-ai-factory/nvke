// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile/dbwrapper"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

type upgradeDatabase struct {
	db.DB
	mu      sync.Mutex
	cluster db.Cluster
	nodes   map[uuid.UUID]db.Node
}

func (d *upgradeDatabase) RunInTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fn(ctx, upgradeTx{d})
}

// The transaction view reads under the lock already held by RunInTx.
// The wrapper reads previous values before writing, so it also uses this view.
type upgradeTx struct{ *upgradeDatabase }

func (tx upgradeTx) GetCluster(ctx context.Context, id uuid.UUID) (db.Cluster, error) {
	return tx.GetClusterForUpdate(ctx, id)
}

func (tx upgradeTx) GetNode(ctx context.Context, id uuid.UUID) (db.Node, error) {
	return tx.GetNodeForUpdate(ctx, id)
}

func (d *upgradeDatabase) GetCluster(ctx context.Context, id uuid.UUID) (db.Cluster, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.GetClusterForUpdate(ctx, id)
}

func (d *upgradeDatabase) GetClusterForUpdate(context.Context, uuid.UUID) (db.Cluster, error) {
	cluster := d.cluster
	if cluster.NodeAgentUpgrade != nil {
		current := *cluster.NodeAgentUpgrade
		cluster.NodeAgentUpgrade = &current
	}
	if cluster.CertificateRenewal != nil {
		current := *cluster.CertificateRenewal
		cluster.CertificateRenewal = &current
	}
	return cluster, nil
}

func (d *upgradeDatabase) UpdateCluster(_ context.Context, cluster *db.Cluster) (db.Cluster, error) {
	d.cluster = *cluster
	return *cluster, nil
}

func (d *upgradeDatabase) ListNodes(context.Context, db.NodeFilters) ([]db.Node, error) {
	nodes := make([]db.Node, 0, len(d.nodes))
	for _, node := range d.nodes {
		nodes = append(nodes, node)
	}
	slices.SortFunc(nodes, func(a, b db.Node) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return nodes, nil
}

func (d *upgradeDatabase) GetNode(ctx context.Context, id uuid.UUID) (db.Node, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.GetNodeForUpdate(ctx, id)
}

func (d *upgradeDatabase) GetNodeForUpdate(_ context.Context, id uuid.UUID) (db.Node, error) {
	node, ok := d.nodes[id]
	if !ok {
		return db.Node{}, db.ErrNotFound
	}
	return node, nil
}

func (d *upgradeDatabase) UpdateNode(_ context.Context, node *db.Node) (db.Node, error) {
	d.nodes[node.ID] = *node
	return *node, nil
}

type convergenceClient struct {
	*agenttest.FakeClient
	fn func(db.Node, string) (string, bool, error)
}

func (c convergenceClient) ConvergeNodeAgent(_ context.Context, node db.Node, target, _ string) (string, bool, error) {
	return c.fn(node, target)
}

func healthyAgentNode() db.Node {
	return db.Node{ID: uuid.New(), ControlPlane: true, AgentVersion: "v0.1.1", Conditions: db.NodeConditions{
		Joined: db.NewCondition(true, ""), Provisioned: db.NewCondition(true, ""), KubernetesReady: db.NewCondition(true, ""), NodeAgentReady: db.NewCondition(true, ""),
	}}
}

func agentUpgradeTestController(nodes ...db.Node) (*Controller, *upgradeDatabase) {
	cluster := db.Cluster{ID: uuid.New(), ControlPlaneConfig: db.ControlPlaneConfig{ControlPlaneConfigSpec: db.ControlPlaneConfigSpec{NodeAgentConfig: db.NodeAgentConfig{Version: "v0.1.2"}}}}
	database := &upgradeDatabase{cluster: cluster, nodes: map[uuid.UUID]db.Node{}}
	for _, node := range nodes {
		database.nodes[node.ID] = node
	}
	return &Controller{cfg: config.Config{NodeAgent: config.NodeAgentConfig{UpdateMethod: "apt"}}, db: dbwrapper.New(database, slog.Default())}, database
}

func TestAgentUpgradeRetriesOneNodeAndAppliesLaterDesiredVersion(t *testing.T) {
	first, second := healthyAgentNode(), healthyAgentNode()
	first.ID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	second.ID = uuid.MustParse("00000000-0000-0000-0000-000000000002")
	controller, database := agentUpgradeTestController(first, second)
	controller.nodeAgentClient = convergenceClient{fn: func(node db.Node, target string) (string, bool, error) {
		require.Equal(t, first.ID, node.ID)
		require.Equal(t, "v0.1.2", target)
		return "v0.1.1", false, errors.New("artifact missing")
	}}
	for range 3 {
		_, err := controller.reconcileNodeAgentUpgrade(t.Context(), database.cluster)
		require.NoError(t, err)
		require.Equal(t, first.ID, database.cluster.NodeAgentUpgrade.NodeID)
		require.Equal(t, "Running", database.cluster.NodeAgentUpgrade.Phase)
		require.Equal(t, "artifact missing", database.cluster.NodeAgentUpgrade.ErrorMessage)
		require.True(t, database.cluster.NodeAgentUpgrade.RetryAt.After(time.Now()))
		database.cluster.NodeAgentUpgrade.RetryAt = time.Time{}
	}
	database.cluster.ControlPlaneConfig.NodeAgentConfig.Version = "v0.1.3"
	controller.nodeAgentClient = convergenceClient{fn: func(node db.Node, target string) (string, bool, error) {
		require.Equal(t, first.ID, node.ID)
		require.Equal(t, "v0.1.2", target, "finish the accepted target without rewriting it")
		return target, true, nil
	}}
	_, err := controller.reconcileNodeAgentUpgrade(t.Context(), database.cluster)
	require.NoError(t, err)
	require.Nil(t, database.cluster.NodeAgentUpgrade)
	require.Equal(t, "v0.1.2", database.nodes[first.ID].AgentVersion)
	controller.nodeAgentClient = convergenceClient{fn: func(node db.Node, target string) (string, bool, error) {
		require.Equal(t, first.ID, node.ID)
		require.Equal(t, "v0.1.3", target)
		return target, true, nil
	}}
	_, err = controller.reconcileNodeAgentUpgrade(t.Context(), database.cluster)
	require.NoError(t, err)
	require.Nil(t, database.cluster.NodeAgentUpgrade)
	require.Equal(t, "v0.1.3", database.nodes[first.ID].AgentVersion)
}

func TestAgentUpgradeNoChangeAndScope(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*db.Node)
	}{
		{"already converged", func(n *db.Node) { n.AgentVersion = "v0.1.2" }},
		{"worker", func(n *db.Node) { n.ControlPlane = false }},
		{"pool managed", func(n *db.Node) { n.NodePoolID = uuid.New() }},
		{"not joined", func(n *db.Node) { n.Conditions.Joined = db.NewCondition(false, "") }},
		{"not provisioned", func(n *db.Node) { n.Conditions.Provisioned = db.NewCondition(false, "") }},
		{"not ready", func(n *db.Node) { n.Conditions.KubernetesReady = db.NewCondition(false, "") }},
		{"deleting", func(n *db.Node) { n.Conditions.Deleting = db.NewCondition(true, "") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := healthyAgentNode()
			test.change(&node)
			controller, database := agentUpgradeTestController(node)
			controller.nodeAgentClient = convergenceClient{fn: func(db.Node, string) (string, bool, error) {
				t.Fatal("installation must not start")
				return "", false, nil
			}}
			_, err := controller.reconcileNodeAgentUpgrade(t.Context(), database.cluster)
			require.NoError(t, err)
			require.Nil(t, database.cluster.NodeAgentUpgrade)
		})
	}
}

func TestDueOrQueuedRenewalDoesNotBlockAgentUpgrade(t *testing.T) {
	node := healthyAgentNode()
	controller, database := agentUpgradeTestController(node)
	past := time.Now().Add(-time.Hour)
	database.cluster.NextCertRenewalDue = &past
	controller.cfg.CertRenewalControllerEnabled = true
	calls := 0
	controller.nodeAgentClient = convergenceClient{fn: func(_ db.Node, target string) (string, bool, error) { calls++; return target, true, nil }}
	_, err := controller.reconcileNodeAgentUpgrade(t.Context(), database.cluster)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
}

func TestDeletionAndReplacementLeaveAcceptedUpdaterAlone(t *testing.T) {
	for _, test := range []struct {
		name    string
		request func(*db.Node)
	}{
		{"requested deletion", func(n *db.Node) { n.Conditions.DeleteRequested = db.NewCondition(true, "") }},
		{"deletion", func(n *db.Node) { n.Conditions.Deleting = db.NewCondition(true, "") }},
		{"repair", func(n *db.Node) { n.Conditions.MarkedForRepair = db.NewCondition(true, "") }},
		{"replacement", func(n *db.Node) { n.Conditions.MarkedForRecycling = db.NewCondition(true, "") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := healthyAgentNode()
			test.request(&node)
			controller, database := agentUpgradeTestController(node)
			selected := db.NodeAgentUpgrade{Source: "background", NodeID: node.ID, TargetVersion: "v0.1.2", Method: "apt", Phase: "Running"}
			database.cluster.NodeAgentUpgrade = &selected
			controller.nodeAgentClient = convergenceClient{fn: func(db.Node, string) (string, bool, error) {
				t.Fatal("deletion must not restart or wait for installation")
				return "", false, nil
			}}
			require.False(t, agentUpgradeBlocksNodeWork(database.cluster, []db.Node{node}))
			_, err := controller.reconcileNodeAgentUpgrade(t.Context(), database.cluster)
			require.NoError(t, err)
			require.Equal(t, selected, *database.cluster.NodeAgentUpgrade, "leave the accepted request alone")
		})
	}
}

func TestAgentRPCDoesNotHoldDeletionTransaction(t *testing.T) {
	node := healthyAgentNode()
	controller, database := agentUpgradeTestController(node)
	controller.nodeAgentClient = convergenceClient{fn: func(_ db.Node, target string) (string, bool, error) {
		finished := make(chan error, 1)
		go func() {
			finished <- database.RunInTx(t.Context(), func(context.Context, db.Tx) error {
				updated := database.nodes[node.ID]
				updated.Conditions.Deleting = db.NewCondition(true, "operator requested deletion")
				database.nodes[node.ID] = updated
				return nil
			})
		}()
		select {
		case err := <-finished:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("the agent RPC holds the deletion transaction")
		}
		return target, false, nil
	}}
	_, err := controller.reconcileNodeAgentUpgrade(t.Context(), database.cluster)
	require.NoError(t, err)
	require.Equal(t, db.ConditionStatusTrue, database.nodes[node.ID].Conditions.Deleting.Status, "status must not overwrite deletion state")
}

func TestAgentUpgradeCannotCompleteOnWrongVersion(t *testing.T) {
	node := healthyAgentNode()
	controller, database := agentUpgradeTestController(node)
	controller.nodeAgentClient = convergenceClient{fn: func(db.Node, string) (string, bool, error) { return "v0.1.1", true, nil }}
	_, err := controller.reconcileNodeAgentUpgrade(t.Context(), database.cluster)
	require.NoError(t, err)
	require.NotNil(t, database.cluster.NodeAgentUpgrade)
	require.Contains(t, database.cluster.NodeAgentUpgrade.ErrorMessage, "expected")
}

func TestAgentUpgradeDiscardsStaleResponseAndRemovedNode(t *testing.T) {
	node := healthyAgentNode()
	controller, database := agentUpgradeTestController(node)
	old := db.NodeAgentUpgrade{Source: "background", NodeID: node.ID, TargetVersion: "v0.1.2", Method: "apt", Phase: "Running"}
	newer := old
	newer.TargetVersion = "v0.1.3"
	database.cluster.NodeAgentUpgrade = &newer
	controller.nodeAgentClient = convergenceClient{fn: func(db.Node, string) (string, bool, error) {
		t.Fatal("stale work must not issue an RPC")
		return "", false, nil
	}}
	require.NoError(t, controller.runNodeAgentUpgrade(t.Context(), database.cluster.ID, old))
	require.NoError(t, controller.saveAgentUpgradeProgress(t.Context(), database.cluster.ID, old, "v0.1.2", true, nil))
	require.Equal(t, newer, *database.cluster.NodeAgentUpgrade)
	delete(database.nodes, node.ID)
	require.NoError(t, controller.runNodeAgentUpgrade(t.Context(), database.cluster.ID, newer))
	require.Nil(t, database.cluster.NodeAgentUpgrade)
}

func TestRunningRenewalBlocksUpgradeAndRecoversAfterCompletion(t *testing.T) {
	for _, phase := range []pb.NodeAgentOperationPhase{pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED, pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED} {
		t.Run(phase.String(), func(t *testing.T) {
			node := healthyAgentNode()
			controller, database := agentUpgradeTestController(node)
			database.cluster.CertificateRenewal = &db.NodeAgentCertificateRenewal{NodeID: node.ID, OperationID: "accepted-renewal"}
			calls := 0
			fake := &agenttest.FakeClient{GetKubernetesCertificateRenewalHook: func(_ db.Node, req *pb.GetKubernetesCertificateRenewalStatusRequest) (*pb.GetKubernetesCertificateRenewalStatusResponse, error) {
				require.Equal(t, "accepted-renewal", req.GetOperationId())
				return &pb.GetKubernetesCertificateRenewalStatusResponse{Operation: &pb.KubernetesCertificateRenewalOperation{Operation: &pb.NodeAgentOperationStatus{Phase: phase}}}, nil
			}}
			controller.nodeAgentClient = convergenceClient{FakeClient: fake, fn: func(_ db.Node, target string) (string, bool, error) { calls++; return target, true, nil }}
			_, err := controller.reconcileNodeAgentUpgrade(t.Context(), database.cluster)
			require.NoError(t, err)
			if phase == pb.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING {
				require.Zero(t, calls)
				require.NotNil(t, database.cluster.CertificateRenewal)
			} else {
				require.Equal(t, 1, calls)
				require.Nil(t, database.cluster.CertificateRenewal)
			}
		})
	}
}

func TestRenewalConnectionFailureDoesNotPermitAgentRestart(t *testing.T) {
	node := healthyAgentNode()
	controller, database := agentUpgradeTestController(node)
	database.cluster.CertificateRenewal = &db.NodeAgentCertificateRenewal{NodeID: node.ID, OperationID: "accepted-renewal"}
	controller.nodeAgentClient = convergenceClient{FakeClient: &agenttest.FakeClient{GetKubernetesCertificateRenewalHook: func(db.Node, *pb.GetKubernetesCertificateRenewalStatusRequest) (*pb.GetKubernetesCertificateRenewalStatusResponse, error) {
		return nil, status.Error(codes.Unavailable, "renewal still owns the node")
	}}, fn: func(db.Node, string) (string, bool, error) {
		t.Fatal("unknown renewal status must not permit installation")
		return "", false, nil
	}}
	_, err := controller.reconcileNodeAgentUpgrade(t.Context(), database.cluster)
	require.Error(t, err)
	require.NotNil(t, database.cluster.CertificateRenewal)
}

func TestAgentUpgradeAndDowngradeUseTheSameReconciliation(t *testing.T) {
	for _, target := range []string{"v0.1.2", "v0.1.0"} {
		t.Run(target, func(t *testing.T) {
			node := healthyAgentNode()
			controller, database := agentUpgradeTestController(node)
			database.cluster.ControlPlaneConfig.NodeAgentConfig.Version = target
			controller.nodeAgentClient = convergenceClient{fn: func(_ db.Node, requested string) (string, bool, error) {
				require.Equal(t, target, requested)
				return requested, true, nil
			}}
			_, err := controller.reconcileNodeAgentUpgrade(t.Context(), database.cluster)
			require.NoError(t, err)
			require.Equal(t, target, database.nodes[node.ID].AgentVersion)
			require.Nil(t, database.cluster.NodeAgentUpgrade)
		})
	}
}

func TestRemovedLifecycleNodeReleasesUpgradeSelection(t *testing.T) {
	node := healthyAgentNode()
	controller, database := agentUpgradeTestController(node)
	database.cluster.NodeAgentUpgrade = &db.NodeAgentUpgrade{NodeID: uuid.New(), Source: db.NodeAgentUpgradeSourceLifecycle, TargetVersion: "v0.1.2", Method: "apt", Phase: "Running"}
	controller.nodeAgentClient = convergenceClient{fn: func(db.Node, string) (string, bool, error) {
		t.Fatal("the removed lifecycle node must receive no installation request")
		return "", false, nil
	}}
	_, err := controller.reconcileNodeAgentUpgrade(t.Context(), database.cluster)
	require.NoError(t, err)
	require.Nil(t, database.cluster.NodeAgentUpgrade)
}
