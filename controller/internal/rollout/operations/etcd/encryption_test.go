// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package etcd

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/NVIDIA/nke/controller/internal/rollout"
	nkeproto "gitlab-master.nvidia.com/nke/node-agent/pkg/agent/proto"
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

type fakeEncryptionKeyReader struct {
	key           string
	err           error
	calls         int
	rotationCalls int
	clusterID     uuid.UUID
}

func (r *fakeEncryptionKeyReader) ReadEnablementKey(_ context.Context, clusterID uuid.UUID) (string, error) {
	r.calls++
	r.clusterID = clusterID
	return r.key, r.err
}

func (r *fakeEncryptionKeyReader) ReadRotationKey(_ context.Context, clusterID uuid.UUID) (string, error) {
	r.rotationCalls++
	r.clusterID = clusterID
	return r.key, r.err
}

type fakeEncryptionClient struct {
	initializeRequest *nkeproto.InitializeKubernetesAPIEncryptionRequest
	initializeErr     error
	promoteErr        error
	removeErr         error
	promoteCalls      int
	removeCalls       int
	updateRequests    []*nkeproto.UpdateEncryptionConfigRequest
	updateErr         error
}

func (c *fakeEncryptionClient) InitializeKubernetesAPIEncryption(
	_ context.Context,
	req *nkeproto.InitializeKubernetesAPIEncryptionRequest,
	_ ...grpc.CallOption,
) (*nkeproto.InitializeKubernetesAPIEncryptionResponse, error) {
	c.initializeRequest = req
	if c.initializeErr != nil {
		return nil, c.initializeErr
	}
	return &nkeproto.InitializeKubernetesAPIEncryptionResponse{}, nil
}

func (c *fakeEncryptionClient) PromoteKubernetesAPIEncryptionProviders(
	context.Context,
	*nkeproto.PromoteKubernetesAPIEncryptionProvidersRequest,
	...grpc.CallOption,
) (*nkeproto.PromoteKubernetesAPIEncryptionProvidersResponse, error) {
	c.promoteCalls++
	if c.promoteErr != nil {
		return nil, c.promoteErr
	}
	return &nkeproto.PromoteKubernetesAPIEncryptionProvidersResponse{}, nil
}

func (c *fakeEncryptionClient) RemovePlainTextSecondaryEncryptionEntry(
	context.Context,
	*nkeproto.RemovePlainTextSecondaryEncryptionEntryRequest,
	...grpc.CallOption,
) (*nkeproto.RemovePlainTextSecondaryEncryptionEntryResponse, error) {
	c.removeCalls++
	if c.removeErr != nil {
		return nil, c.removeErr
	}
	return &nkeproto.RemovePlainTextSecondaryEncryptionEntryResponse{}, nil
}

func (c *fakeEncryptionClient) UpdateEncryptionConfig(
	_ context.Context,
	req *nkeproto.UpdateEncryptionConfigRequest,
	_ ...grpc.CallOption,
) (*nkeproto.UpdateEncryptionConfigResponse, error) {
	c.updateRequests = append(c.updateRequests, req)
	if c.updateErr != nil {
		return nil, c.updateErr
	}
	return &nkeproto.UpdateEncryptionConfigResponse{}, nil
}

func TestInitializeOperationReadsKeyAtApplyTimeAndKeepsItOutOfPayload(t *testing.T) {
	clusterID := uuid.New()
	node := fakeNode{id: uuid.New(), name: "cp-0", role: rollout.NodeRoleControlPlane, endpoint: "10.0.0.10:50051"}
	cluster := &fakeCluster{id: clusterID, name: "tenant-a", nodes: []rollout.Node{node}}
	reader := &fakeEncryptionKeyReader{}
	client := &fakeEncryptionClient{}
	var dialedNodeID uuid.UUID
	var cleanupCalled bool

	op := NewInitializeOperation(
		WithEncryptionKeyReader(reader),
		WithObjectsToEncrypt([]string{"secrets", "configmaps"}),
	)
	op.nodeAgentClientDialer = func(_ context.Context, dialedNode rollout.Node) (nodeAgentEncryptionClient, func(), error) {
		dialedNodeID = dialedNode.ID()
		return client, func() { cleanupCalled = true }, nil
	}

	payload, err := op.BuildApplyToNodePayload(context.Background(), cluster)
	require.NoError(t, err)
	assert.Contains(t, string(payload), clusterID.String())
	assert.NotContains(t, string(payload), "key-read-inside-activity")

	reader.key = "key-read-inside-activity"
	outcome, err := op.ApplyToNode(context.Background(), node, payload)
	require.NoError(t, err)
	assert.Equal(t, rollout.OutcomeApplied, outcome)
	assert.Equal(t, clusterID, reader.clusterID)
	assert.Equal(t, 1, reader.calls)
	require.NotNil(t, client.initializeRequest)
	assert.Equal(t, "key-read-inside-activity", client.initializeRequest.GetEncryptionKey())
	assert.Equal(t, []string{"secrets", "configmaps"}, client.initializeRequest.GetObjectsToEncrypt())
	assert.Equal(t, node.ID(), dialedNodeID)
	assert.True(t, cleanupCalled)
}

func TestInitializeOperationMissingKeyFailsBeforeDial(t *testing.T) {
	op := NewInitializeOperation(
		WithEncryptionKeyReader(&fakeEncryptionKeyReader{}),
		WithObjectsToEncrypt(DefaultObjectsToEncrypt()),
	)
	op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentEncryptionClient, func(), error) {
		panic("dialer must not be called when the encryption key is missing")
	}
	payload, err := op.BuildApplyToNodePayload(context.Background(), &fakeCluster{id: uuid.New(), name: "tenant-a"})
	require.NoError(t, err)

	outcome, err := op.ApplyToNode(context.Background(), fakeNode{name: "cp-0", role: rollout.NodeRoleControlPlane}, payload)
	require.Error(t, err)
	assert.Equal(t, rollout.OutcomeFailedRestored, outcome)
	assert.ErrorContains(t, err, "key is empty")
}

func TestInitializeOperationValidateRejectsEmptyObjectsToEncrypt(t *testing.T) {
	node := fakeNode{id: uuid.New(), name: "cp-0", role: rollout.NodeRoleControlPlane}
	cluster := &fakeCluster{id: uuid.New(), name: "tenant-a", nodes: []rollout.Node{node}}
	op := NewInitializeOperation(
		WithEncryptionKeyReader(&fakeEncryptionKeyReader{key: "key"}),
	)
	op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentEncryptionClient, func(), error) {
		return &fakeEncryptionClient{}, func() {}, nil
	}

	err := op.Validate(context.Background(), cluster)
	require.Error(t, err)
	assert.ErrorContains(t, err, "objects to encrypt are not configured")
}

func TestDefaultObjectsToEncryptReturnsCopy(t *testing.T) {
	first := DefaultObjectsToEncrypt()
	require.NotEmpty(t, first)
	first[0] = "mutated"

	assert.Equal(t, "secrets", DefaultObjectsToEncrypt()[0])
}

func TestPromoteDialFailureReturnsFailedRestored(t *testing.T) {
	op := NewPromoteProvidersOperation()
	op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentEncryptionClient, func(), error) {
		return nil, nil, errors.New("connection refused")
	}
	payload, err := op.BuildApplyToNodePayload(context.Background(), &fakeCluster{id: uuid.New(), name: "tenant-a"})
	require.NoError(t, err)

	outcome, err := op.ApplyToNode(context.Background(), fakeNode{name: "cp-0", role: rollout.NodeRoleControlPlane}, payload)
	require.Error(t, err)
	assert.Equal(t, rollout.OutcomeFailedRestored, outcome)
	assert.ErrorContains(t, err, "connection refused")
}

func TestPromoteAndRemoveDispatchTheirNodeAgentRPCs(t *testing.T) {
	client := &fakeEncryptionClient{}
	dialer := func(context.Context, rollout.Node) (nodeAgentEncryptionClient, func(), error) {
		return client, func() {}, nil
	}
	cluster := &fakeCluster{id: uuid.New(), name: "tenant-a"}
	node := fakeNode{name: "cp-0", role: rollout.NodeRoleControlPlane}

	for _, op := range []*EncryptionOperation{NewPromoteProvidersOperation(), NewRemovePlainTextSecondaryOperation()} {
		op.nodeAgentClientDialer = dialer
		payload, err := op.BuildApplyToNodePayload(context.Background(), cluster)
		require.NoError(t, err)
		outcome, err := op.ApplyToNode(context.Background(), node, payload)
		require.NoError(t, err)
		assert.Equal(t, rollout.OutcomeApplied, outcome)
	}

	assert.Equal(t, 1, client.promoteCalls)
	assert.Equal(t, 1, client.removeCalls)
}

func TestKeyRotationOperationsReadPendingKeyAndDispatchTheirMutations(t *testing.T) {
	clusterID := uuid.New()
	keyReader := &fakeEncryptionKeyReader{key: "new-pending-key"}
	client := &fakeEncryptionClient{}
	dialer := func(context.Context, rollout.Node) (nodeAgentEncryptionClient, func(), error) {
		return client, func() {}, nil
	}
	cluster := &fakeCluster{id: clusterID, name: "tenant-a"}
	node := fakeNode{name: "cp-0", role: rollout.NodeRoleControlPlane}
	operations := []*EncryptionOperation{
		NewAddSecondaryKeyOperation(WithEncryptionKeyReader(keyReader)),
		NewPromotePrimaryKeyOperation(WithEncryptionKeyReader(keyReader)),
		NewRemoveOldKeysOperation(WithEncryptionKeyReader(keyReader)),
	}

	for _, op := range operations {
		op.nodeAgentClientDialer = dialer
		payload, err := op.BuildApplyToNodePayload(context.Background(), cluster)
		require.NoError(t, err)
		outcome, err := op.ApplyToNode(context.Background(), node, payload)
		require.NoError(t, err)
		assert.Equal(t, rollout.OutcomeApplied, outcome)
	}

	require.Len(t, client.updateRequests, 3)
	assert.Equal(t, "new-pending-key", client.updateRequests[0].GetAddSecondaryKey().GetNewKey())
	assert.Equal(t, "new-pending-key", client.updateRequests[1].GetPromotePrimaryKey().GetNewKey())
	assert.Equal(t, "new-pending-key", client.updateRequests[2].GetRemoveOldKeys().GetPrimaryKey())
	assert.Equal(t, 3, keyReader.rotationCalls)
	assert.Equal(t, 0, keyReader.calls)
	assert.Equal(t, clusterID, keyReader.clusterID)
}

func TestKeyRotationOperationFailureLeavesNodeStateForRetry(t *testing.T) {
	op := NewPromotePrimaryKeyOperation(
		WithEncryptionKeyReader(&fakeEncryptionKeyReader{key: "new-pending-key"}),
	)
	op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentEncryptionClient, func(), error) {
		return &fakeEncryptionClient{updateErr: errors.New("reload not confirmed")}, func() {}, nil
	}
	payload, err := op.BuildApplyToNodePayload(
		context.Background(),
		&fakeCluster{id: uuid.New(), name: "tenant-a"},
	)
	require.NoError(t, err)

	outcome, err := op.ApplyToNode(
		context.Background(),
		fakeNode{name: "cp-0", role: rollout.NodeRoleControlPlane},
		payload,
	)

	require.Error(t, err)
	assert.Equal(t, rollout.OutcomeFailedNotRestored, outcome)
	assert.ErrorContains(t, err, "reload not confirmed")
}
