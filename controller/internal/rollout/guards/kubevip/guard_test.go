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

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout/nodeagent"
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

func (c *fakeCluster) ID() uuid.UUID                                  { return c.id }
func (c *fakeCluster) Name() string                                   { return c.name }
func (c *fakeCluster) KubernetesVersion() string                      { return "v1.30.0" }
func (c *fakeCluster) ControlPlaneVIP() string                        { return "10.0.0.1:6443" }
func (c *fakeCluster) OIDCProviders() []controllerconfig.OIDCProvider { return nil }
func (c *fakeCluster) ControlPlaneNodes() []rollout.Node              { return c.nodes }
func (c *fakeCluster) Nodes() []rollout.Node                          { return c.nodes }

type fakeKubeVipClient struct {
	nvkeproto.NodeAgentClient

	drainStartErr error
	drainStartReq *nvkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest
	drainStatus   []*nvkeproto.NodeAgentOperationStatus
	drainPolls    int

	restoreStartReq *nvkeproto.StartKubeVipRestoreRequest
	restoreStatus   []*nvkeproto.NodeAgentOperationStatus
	restorePolls    int
}

func (c *fakeKubeVipClient) StartK8SControlPlaneNodeGracefulShutdown(
	_ context.Context,
	req *nvkeproto.StartK8SControlPlaneNodeGracefulShutdownRequest,
	_ ...grpc.CallOption,
) (*nvkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse, error) {
	c.drainStartReq = req
	if c.drainStartErr != nil {
		return nil, c.drainStartErr
	}
	return &nvkeproto.StartK8SControlPlaneNodeGracefulShutdownResponse{
		Operation: running(req.GetOperationId(), "queued"),
	}, nil
}

func (c *fakeKubeVipClient) GetK8SControlPlaneNodeGracefulShutdownStatus(
	context.Context,
	*nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusRequest,
	...grpc.CallOption,
) (*nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse, error) {
	op := succeeded("drain")
	if c.drainPolls < len(c.drainStatus) {
		op = c.drainStatus[c.drainPolls]
	}
	c.drainPolls++
	return &nvkeproto.GetK8SControlPlaneNodeGracefulShutdownStatusResponse{Operation: op}, nil
}

func (c *fakeKubeVipClient) StartKubeVipRestore(
	_ context.Context,
	req *nvkeproto.StartKubeVipRestoreRequest,
	_ ...grpc.CallOption,
) (*nvkeproto.StartKubeVipRestoreResponse, error) {
	if c.restoreStartReq == nil {
		c.restoreStartReq = req
		return &nvkeproto.StartKubeVipRestoreResponse{
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
	return &nvkeproto.StartKubeVipRestoreResponse{Operation: op}, nil
}

func TestKubeVipDrainRestoreGuard_DrainsAndRestores(t *testing.T) {
	node := fakeNode{id: uuid.New(), name: "cp-0", role: rollout.NodeRoleControlPlane, endpoint: "10.0.0.10:50051"}
	cluster := &fakeCluster{id: uuid.New(), name: "tenant-a", nodes: []rollout.Node{node}}
	client := &fakeKubeVipClient{
		drainStatus: []*nvkeproto.NodeAgentOperationStatus{
			succeeded("drain"),
		},
		restoreStatus: []*nvkeproto.NodeAgentOperationStatus{
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
		restoreStatus: []*nvkeproto.NodeAgentOperationStatus{
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
		drainStatus: []*nvkeproto.NodeAgentOperationStatus{
			failed("drain", "waiting_for_vip_socket_quiet_window", "new VIP socket appeared"),
		},
		restoreStatus: []*nvkeproto.NodeAgentOperationStatus{
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

func TestNodeAgentOperationDone_NilStatusFails(t *testing.T) {
	done, err := nodeAgentOperationDone(nil)
	require.Error(t, err)
	assert.False(t, done)
	assert.Contains(t, err.Error(), "nil NodeAgentOperationStatus")
}

func dialerFor(client *fakeKubeVipClient) nodeagent.Dialer {
	return func(context.Context, rollout.Node) (nvkeproto.NodeAgentClient, func(), error) {
		return client, func() {}, nil
	}
}

func running(operationID, step string) *nvkeproto.NodeAgentOperationStatus {
	return &nvkeproto.NodeAgentOperationStatus{
		OperationId: operationID,
		Phase:       nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_RUNNING,
		Step:        step,
	}
}

func succeeded(operationID string) *nvkeproto.NodeAgentOperationStatus {
	return &nvkeproto.NodeAgentOperationStatus{
		OperationId: operationID,
		Phase:       nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_SUCCEEDED,
		Step:        "done",
	}
}

func failed(operationID, step, msg string) *nvkeproto.NodeAgentOperationStatus {
	return &nvkeproto.NodeAgentOperationStatus{
		OperationId:  operationID,
		Phase:        nvkeproto.NodeAgentOperationPhase_NODE_AGENT_OPERATION_PHASE_FAILED,
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
	guard := NewGuard(func(context.Context, rollout.Node) (nvkeproto.NodeAgentClient, func(), error) {
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
