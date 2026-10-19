// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package etcd

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

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
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

type fakeEncryptionKeyReader struct {
	key           string
	err           error
	calls         int
	rotationCalls int
	clusterID     uuid.UUID
	name          string
}

func (r *fakeEncryptionKeyReader) ReadEnablementKey(_ context.Context, clusterID uuid.UUID) (EncryptionKey, error) {
	r.calls++
	r.clusterID = clusterID
	return EncryptionKey{Key: r.key, Name: r.name}, r.err
}

func (r *fakeEncryptionKeyReader) ReadRotationKey(_ context.Context, clusterID uuid.UUID) (EncryptionKey, error) {
	r.rotationCalls++
	r.clusterID = clusterID
	return EncryptionKey{Key: r.key, Name: r.name}, r.err
}

type fakeEncryptionClient struct {
	initializeRequest *nvkeproto.InitializeKubernetesAPIEncryptionRequest
	initializeErr     error
	promoteErr        error
	removeErr         error
	promoteCalls      int
	removeCalls       int
	updateRequests    []*nvkeproto.UpdateEncryptionConfigRequest
	updateErr         error
	identityName      string
	identityErr       error
	identityCalls     int
}

func (c *fakeEncryptionClient) InitializeKubernetesAPIEncryption(
	_ context.Context,
	req *nvkeproto.InitializeKubernetesAPIEncryptionRequest,
	_ ...grpc.CallOption,
) (*nvkeproto.InitializeKubernetesAPIEncryptionResponse, error) {
	c.initializeRequest = req
	if c.initializeErr != nil {
		return nil, c.initializeErr
	}
	return &nvkeproto.InitializeKubernetesAPIEncryptionResponse{}, nil
}

func (c *fakeEncryptionClient) PromoteKubernetesAPIEncryptionProviders(
	context.Context,
	*nvkeproto.PromoteKubernetesAPIEncryptionProvidersRequest,
	...grpc.CallOption,
) (*nvkeproto.PromoteKubernetesAPIEncryptionProvidersResponse, error) {
	c.promoteCalls++
	if c.promoteErr != nil {
		return nil, c.promoteErr
	}
	return &nvkeproto.PromoteKubernetesAPIEncryptionProvidersResponse{}, nil
}

func (c *fakeEncryptionClient) RemovePlainTextSecondaryEncryptionEntry(
	context.Context,
	*nvkeproto.RemovePlainTextSecondaryEncryptionEntryRequest,
	...grpc.CallOption,
) (*nvkeproto.RemovePlainTextSecondaryEncryptionEntryResponse, error) {
	c.removeCalls++
	if c.removeErr != nil {
		return nil, c.removeErr
	}
	return &nvkeproto.RemovePlainTextSecondaryEncryptionEntryResponse{}, nil
}

func (c *fakeEncryptionClient) UpdateEncryptionConfig(
	_ context.Context,
	req *nvkeproto.UpdateEncryptionConfigRequest,
	_ ...grpc.CallOption,
) (*nvkeproto.UpdateEncryptionConfigResponse, error) {
	c.updateRequests = append(c.updateRequests, req)
	if c.updateErr != nil {
		return nil, c.updateErr
	}
	return &nvkeproto.UpdateEncryptionConfigResponse{}, nil
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
		WithObjectsToEncrypt([]string{secrets, configmaps}),
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
	assert.Equal(t, []string{secrets, configmaps}, client.initializeRequest.GetObjectsToEncrypt())
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

	assert.Equal(t, secrets, DefaultObjectsToEncrypt()[0])
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
	keyReader := &fakeEncryptionKeyReader{key: "new-pending-key", name: "kube-api-encryption-key-fixture"}
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
	assert.Equal(t, keyReader.name, client.updateRequests[0].GetAddSecondaryKey().GetKeyName())
	assert.Equal(t, keyReader.name, client.updateRequests[1].GetPromotePrimaryKey().GetKeyName())
	assert.Equal(t, keyReader.name, client.updateRequests[2].GetRemoveOldKeys().GetKeyName())
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
	require.ErrorContains(t, err, "inspect node encryption health")
	assert.NotContains(t, err.Error(), "reload not confirmed")
}

// TestRotationErrorsNeverSerializeKeyMaterial simulates unsafe upstream error
// text and protects both activity failure and rollout outcome boundaries.
func TestRotationErrorsNeverSerializeKeyMaterial(t *testing.T) {
	for _, name := range []string{"key lookup", "agent update"} {
		t.Run(name, func(t *testing.T) {
			reader := &fakeEncryptionKeyReader{key: "KEY_MATERIAL_SENTINEL"}
			client := &fakeEncryptionClient{}
			if name == "key lookup" {
				reader.err = errors.New("rejected Secret body KEY_MATERIAL_SENTINEL")
			} else {
				client.updateErr = errors.New("rejected request KEY_MATERIAL_SENTINEL")
			}
			op := NewAddSecondaryKeyOperation(WithEncryptionKeyReader(reader))
			op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentEncryptionClient, func(), error) {
				return client, func() {}, nil
			}
			payload, err := op.BuildApplyToNodePayload(t.Context(), &fakeCluster{id: uuid.New()})
			require.NoError(t, err)
			_, err = op.ApplyToNode(t.Context(), fakeNode{role: rollout.NodeRoleControlPlane}, payload)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "KEY_MATERIAL_SENTINEL")
			require.NotContains(t, string(payload), "KEY_MATERIAL_SENTINEL")
		})
	}
}

func (c *fakeEncryptionClient) GetEtcdEncryptionKeyIdentity(context.Context, *nvkeproto.GetEtcdEncryptionKeyIdentityRequest, ...grpc.CallOption) (*nvkeproto.GetEtcdEncryptionKeyIdentityResponse, error) {
	c.identityCalls++
	return &nvkeproto.GetEtcdEncryptionKeyIdentityResponse{KeyName: c.identityName}, c.identityErr
}

func TestRotationIdentityChecksBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name         string
		storedName   string
		observedName string
		err          error
		wantError    bool
	}{
		{name: "supported named key", storedName: "key-7", observedName: "key-7"},
		{name: "named key not yet distributed", storedName: "key-7"},
		{name: "unnamed legacy key skips identity", observedName: "key-7"},
		{name: "mismatched names rejected", storedName: "key-7", observedName: "key-8", wantError: true},
		{name: "older agent rejected", storedName: "key-7", err: errors.New("unsupported identity RPC"), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &fakeEncryptionKeyReader{key: "public-fixture-key", name: test.storedName}
			client := &fakeEncryptionClient{identityName: test.observedName, identityErr: test.err}
			op := NewAddSecondaryKeyOperation(WithEncryptionKeyReader(reader))
			op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentEncryptionClient, func(), error) {
				return client, nil, nil
			}
			payload, err := op.BuildApplyToNodePayload(context.Background(), &fakeCluster{id: uuid.New()})
			require.NoError(t, err)
			_, err = op.ApplyToNode(context.Background(), fakeNode{name: "cp-1", role: rollout.NodeRoleControlPlane}, payload)
			if test.wantError {
				require.Error(t, err)
				require.Empty(t, client.updateRequests)
			} else {
				require.NoError(t, err)
				require.Len(t, client.updateRequests, 1)
				require.Equal(t, test.storedName, client.updateRequests[0].GetAddSecondaryKey().GetKeyName())
			}
			if test.storedName == "" {
				require.Zero(t, client.identityCalls)
			} else {
				require.Equal(t, 1, client.identityCalls)
			}
		})
	}
}

// TestLegacyRotationPhasesUseOriginalProtocolOnAgentsWithoutIdentity models a
// rotation started by an earlier controller: its pending key has no recorded
// name and the agents predate the identity RPC. Every phase must send the
// original unnamed request and never call the identity RPC.
func TestLegacyRotationPhasesUseOriginalProtocolOnAgentsWithoutIdentity(t *testing.T) {
	reader := &fakeEncryptionKeyReader{key: "legacy-pending-key"}
	client := &fakeEncryptionClient{identityErr: status.Error(codes.Unimplemented, "unknown method GetEtcdEncryptionKeyIdentity")}
	cluster := &fakeCluster{id: uuid.New(), name: "tenant-a"}
	for _, op := range []*EncryptionOperation{
		NewAddSecondaryKeyOperation(WithEncryptionKeyReader(reader)),
		NewPromotePrimaryKeyOperation(WithEncryptionKeyReader(reader)),
		NewRemoveOldKeysOperation(WithEncryptionKeyReader(reader)),
	} {
		op.nodeAgentClientDialer = func(context.Context, rollout.Node) (nodeAgentEncryptionClient, func(), error) {
			return client, nil, nil
		}
		payload, err := op.BuildApplyToNodePayload(t.Context(), cluster)
		require.NoError(t, err)
		outcome, err := op.ApplyToNode(t.Context(), fakeNode{name: "cp-0", role: rollout.NodeRoleControlPlane}, payload)
		require.NoError(t, err, op.Name())
		require.Equal(t, rollout.OutcomeApplied, outcome)
	}
	require.Zero(t, client.identityCalls)
	require.Len(t, client.updateRequests, 3)
	require.Equal(t, &nvkeproto.AddSecondaryKeyOperation{NewKey: "legacy-pending-key"}, client.updateRequests[0].GetAddSecondaryKey())
	require.Equal(t, &nvkeproto.PromotePrimaryKeyOperation{NewKey: "legacy-pending-key"}, client.updateRequests[1].GetPromotePrimaryKey())
	require.Equal(t, &nvkeproto.RemoveOldKeysOperation{PrimaryKey: "legacy-pending-key"}, client.updateRequests[2].GetRemoveOldKeys())
}
