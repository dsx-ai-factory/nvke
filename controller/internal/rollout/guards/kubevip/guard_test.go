// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubevip

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout/nodeagent"
)

type fakeNode struct {
	id       uuid.UUID
	name     string
	role     rollout.NodeRole
	endpoint string
}

func (n fakeNode) ID() uuid.UUID          { return n.id }
func (n fakeNode) Name() string           { return n.name }
func (n fakeNode) Role() rollout.NodeRole { return n.role }
func (n fakeNode) AgentEndpoint() string  { return n.endpoint }

type fakeCluster struct {
	id    uuid.UUID
	name  string
	nodes []rollout.Node
}

func (c *fakeCluster) ID() uuid.UUID                     { return c.id }
func (c *fakeCluster) Name() string                      { return c.name }
func (c *fakeCluster) KubernetesVersion() string         { return "v1.30.0" }
func (c *fakeCluster) ControlPlaneVIP() string           { return "10.0.0.1:6443" }
func (c *fakeCluster) ControlPlaneNodes() []rollout.Node { return c.nodes }
func (c *fakeCluster) Nodes() []rollout.Node             { return c.nodes }

type fakeKubeVipClient struct {
	nkeproto.NodeAgentClient

	drainStartErr error
	drainStartReq *nkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest
	drainStatus   []*nkeproto.NodeAgentOperationStatus
	drainPolls    int

	restoreStartErr error
	restoreStartReq *nkeproto.StartKubeVipRestoreRequest
	restoreStatus   []*nkeproto.NodeAgentOperationStatus
	restorePolls    int
}

func (c *fakeKubeVipClient) StartK8SControlPlaneNodeGracefulShutdown(
	_ context.Context,
	req *nkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest,
	_ ...grpc.CallOption,
) (*nkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse, error) {
	c.drainStartReq = req
	if c.drainStartErr != nil {
		return nil, c.drainStartErr
	}
	return &nkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse{
		Operation: running(req.GetOperationId(), "queued"),
	}, nil
}

func (c *fakeKubeVipClient) GetK8SControlPlaneNodeGracefulShutdownStatus(
	context.Context,
	*nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest,
	...grpc.CallOption,
) (*nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse, error) {
	op := succeeded("drain")
	if c.drainPolls < len(c.drainStatus) {
		op = c.drainStatus[c.drainPolls]
	}
	c.drainPolls++
	return &nkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse{Operation: op}, nil
}

func (c *fakeKubeVipClient) StartKubeVipRestore(
	_ context.Context,
	req *nkeproto.StartKubeVipRestoreRequest,
	_ ...grpc.CallOption,
) (*nkeproto.StartKubeVipRestoreResponse, error) {
	if c.restoreStartErr != nil {
		return nil, c.restoreStartErr
	}
	if c.restoreStartReq == nil {
		c.restoreStartReq = req
		return &nkeproto.StartKubeVipRestoreResponse{
			Operation: running(req.GetOperationId(), "queued"),
		}, nil
	}
	// Subsequent StartKubeVipRestore calls are idempotent status polls.
	operationID := req.GetOperationId()
	op := succeeded(operationID)
	if c.restorePolls < len(c.restoreStatus) {
		op = c.restoreStatus[c.restorePolls]
	}
	c.restorePolls++
	return &nkeproto.StartKubeVipRestoreResponse{Operation: op}, nil
}

func TestKubeVipDrainRestoreGuard_DrainsAndRestores(t *testing.T) {
	node := fakeNode{id: uuid.New(), name: "cp-0", role: rollout.NodeRoleControlPlane, endpoint: "10.0.0.10:50051"}
	cluster := &fakeCluster{id: uuid.New(), name: "tenant-a", nodes: []rollout.Node{node}}
	client := &fakeKubeVipClient{
		drainStatus: []*nkeproto.NodeAgentOperationStatus{
			succeeded("drain"),
		},
		restoreStatus: []*nkeproto.NodeAgentOperationStatus{
			succeeded("restore"),
		},
	}
	guard := NewGuard(dialerFor(client))

	state, err := guard.PreApply(context.Background(), cluster, node)
	require.NoError(t, err)
	require.NotEmpty(t, state.Payload)
	assert.Nil(t, client.drainStartReq, "PreApply must persist state before starting the drain")

	done, err := guard.CheckPreApply(context.Background(), cluster, node, state)
	require.NoError(t, err)
	assert.True(t, done)
	require.NotNil(t, client.drainStartReq)
	assert.False(t, client.drainStartReq.GetStopKubeApiserver(), "kube-vip guard must not stop kube-apiserver")

	err = guard.PostApply(context.Background(), cluster, node, rollout.OutcomeApplied, state)
	require.NoError(t, err)
	require.NotNil(t, client.restoreStartReq)
	assert.NotEmpty(t, client.restoreStartReq.GetOperationId())
	assert.NotEqual(t, client.drainStartReq.GetOperationId(), client.restoreStartReq.GetOperationId())

	done, err = guard.CheckPostApply(context.Background(), cluster, node, rollout.OutcomeApplied, state)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, 1, client.restorePolls)
}

func TestKubeVipDrainRestoreGuard_DirectExecutionsUseDistinctOperationIDs(t *testing.T) {
	node := fakeNode{id: uuid.New(), name: "cp-0", role: rollout.NodeRoleControlPlane, endpoint: "10.0.0.10:50051"}
	cluster := &fakeCluster{id: uuid.New(), name: "tenant-a", nodes: []rollout.Node{node}}
	guard := NewGuard(dialerFor(&fakeKubeVipClient{}))

	first, err := guard.PreApply(context.Background(), cluster, node)
	require.NoError(t, err)
	second, err := guard.PreApply(context.Background(), cluster, node)
	require.NoError(t, err)

	firstPayload, err := stateFromGuardState(first)
	require.NoError(t, err)
	secondPayload, err := stateFromGuardState(second)
	require.NoError(t, err)
	require.NotNil(t, firstPayload)
	require.NotNil(t, secondPayload)
	assert.NotEqual(t, firstPayload.DrainOperationID, secondPayload.DrainOperationID)
	assert.NotEqual(t, firstPayload.RestoreOperationID, secondPayload.RestoreOperationID)
}

func TestKubeVipDrainRestoreGuard_PostApplyRestoresAfterDrainStartError(t *testing.T) {
	node := fakeNode{id: uuid.New(), name: "cp-0", role: rollout.NodeRoleControlPlane, endpoint: "10.0.0.10:50051"}
	cluster := &fakeCluster{id: uuid.New(), name: "tenant-a", nodes: []rollout.Node{node}}
	client := &fakeKubeVipClient{
		drainStartErr: errors.New("rpc error after request dispatch"),
		restoreStatus: []*nkeproto.NodeAgentOperationStatus{
			succeeded("restore"),
		},
	}
	guard := NewGuard(dialerFor(client))

	state, err := guard.PreApply(context.Background(), cluster, node)
	require.NoError(t, err)
	require.NotEmpty(t, state.Payload, "drain state must be durable before the Start RPC")

	done, err := guard.CheckPreApply(context.Background(), cluster, node, state)
	require.Error(t, err)
	assert.False(t, done)
	assert.Contains(t, err.Error(), "rpc error after request dispatch")

	err = guard.PostApply(context.Background(), cluster, node, rollout.OutcomeFailedRestored, state)
	require.NoError(t, err)
	require.NotNil(t, client.restoreStartReq)

	done, err = guard.CheckPostApply(context.Background(), cluster, node, rollout.OutcomeFailedRestored, state)
	require.NoError(t, err)
	assert.True(t, done)
}

func TestKubeVipDrainRestoreGuard_PostApplyRestoresAfterPreApplyCheckFailure(t *testing.T) {
	node := fakeNode{id: uuid.New(), name: "cp-0", role: rollout.NodeRoleControlPlane, endpoint: "10.0.0.10:50051"}
	cluster := &fakeCluster{id: uuid.New(), name: "tenant-a", nodes: []rollout.Node{node}}
	client := &fakeKubeVipClient{
		drainStatus: []*nkeproto.NodeAgentOperationStatus{
			failed("drain", "waiting_for_vip_socket_quiet_window", "new VIP socket appeared"),
		},
		restoreStatus: []*nkeproto.NodeAgentOperationStatus{
			succeeded("restore"),
		},
	}
	guard := NewGuard(dialerFor(client))

	state, err := guard.PreApply(context.Background(), cluster, node)
	require.NoError(t, err)
	require.NotEmpty(t, state.Payload)

	done, err := guard.CheckPreApply(context.Background(), cluster, node, state)
	require.Error(t, err)
	assert.False(t, done)
	assert.Contains(t, err.Error(), "new VIP socket appeared")

	err = guard.PostApply(context.Background(), cluster, node, rollout.OutcomeFailedRestored, state)
	require.NoError(t, err)
	require.NotNil(t, client.restoreStartReq)

	done, err = guard.CheckPostApply(context.Background(), cluster, node, rollout.OutcomeFailedRestored, state)
	require.NoError(t, err)
	assert.True(t, done)
}

// An agent that predates the drain and restore RPCs fails closed, as for the
// other apiserver-restarting rollouts: an undrained node is never restarted.
func TestKubeVipDrainRestoreGuard_UnimplementedAgentFailsClosed(t *testing.T) {
	unimplemented := status.Error(codes.Unimplemented, "unknown method")
	node := fakeNode{id: uuid.New(), name: "cp-0", role: rollout.NodeRoleControlPlane, endpoint: "10.0.0.10:50051"}
	cluster := &fakeCluster{id: uuid.New(), name: "tenant-a", nodes: []rollout.Node{node}}
	client := &fakeKubeVipClient{drainStartErr: unimplemented, restoreStartErr: unimplemented}
	guard := NewGuard(dialerFor(client))

	state, err := guard.PreApply(context.Background(), cluster, node)
	require.NoError(t, err)

	done, err := guard.CheckPreApply(context.Background(), cluster, node, state)
	require.Error(t, err)
	assert.False(t, done)
	require.Error(t, guard.PostApply(context.Background(), cluster, node, rollout.OutcomeApplied, state))
	done, err = guard.CheckPostApply(context.Background(), cluster, node, rollout.OutcomeApplied, state)
	require.Error(t, err)
	assert.False(t, done)
}

func TestNodeAgentOperationDone_NilStatusFails(t *testing.T) {
	done, err := nodeAgentOperationDone(nil)
	require.Error(t, err)
	assert.False(t, done)
	assert.Contains(t, err.Error(), "nil NodeAgentOperationStatus")
}

func dialerFor(client *fakeKubeVipClient) nodeagent.Dialer {
	return func(context.Context, rollout.Node) (nkeproto.NodeAgentClient, func(), error) {
		return client, func() {}, nil
	}
}

func running(operationID, step string) *nkeproto.NodeAgentOperationStatus {
	return &nkeproto.NodeAgentOperationStatus{
		OperationId: operationID,
		Phase:       nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING,
		Step:        step,
	}
}

func succeeded(operationID string) *nkeproto.NodeAgentOperationStatus {
	return &nkeproto.NodeAgentOperationStatus{
		OperationId: operationID,
		Phase:       nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
		Step:        "done",
	}
}

func failed(operationID, step, msg string) *nkeproto.NodeAgentOperationStatus {
	return &nkeproto.NodeAgentOperationStatus{
		OperationId:  operationID,
		Phase:        nkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
		Step:         step,
		ErrorMessage: msg,
	}
}

func TestKubeVipDrainRestoreGuard_DialerRequired(t *testing.T) {
	node := fakeNode{id: uuid.New(), name: "cp-0", role: rollout.NodeRoleControlPlane, endpoint: "10.0.0.10:50051"}
	cluster := &fakeCluster{id: uuid.New(), name: "tenant-a", nodes: []rollout.Node{node}}
	guard := NewGuard(nil)

	state, err := guard.PreApply(context.Background(), cluster, node)
	require.NoError(t, err)
	assert.Equal(t, GuardNameKubeVipDrainRestore, state.GuardName)
	done, err := guard.CheckPreApply(context.Background(), cluster, node, state)
	require.Error(t, err)
	assert.False(t, done)
	assert.Contains(t, err.Error(), "dialer not configured")
}

func TestKubeVipDrainRestoreGuard_DialError(t *testing.T) {
	node := fakeNode{id: uuid.New(), name: "cp-0", role: rollout.NodeRoleControlPlane, endpoint: "10.0.0.10:50051"}
	cluster := &fakeCluster{id: uuid.New(), name: "tenant-a", nodes: []rollout.Node{node}}
	guard := NewGuard(func(context.Context, rollout.Node) (nkeproto.NodeAgentClient, func(), error) {
		return nil, nil, errors.New("connection refused")
	})

	state, err := guard.PreApply(context.Background(), cluster, node)
	require.NoError(t, err)
	assert.Equal(t, GuardNameKubeVipDrainRestore, state.GuardName)
	done, err := guard.CheckPreApply(context.Background(), cluster, node, state)
	require.Error(t, err)
	assert.False(t, done)
	assert.Contains(t, err.Error(), "connection refused")
}
