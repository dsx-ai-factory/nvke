// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout/adapters/controller"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func TestNewNodeResolver(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "cp-0", IP: "10.0.0.11"}
	resolve := controller.NewNodeResolver([]db.Node{node})

	got, err := resolve(context.Background(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, node.ID, got.ID)

	_, err = resolve(context.Background(), uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, controller.ErrNodeNotFound)
}

func TestNewDatabaseNodeResolverUsesCurrentInventory(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "cp-0", IP: "10.0.0.11"}
	database := &testutil.MockDB{}
	database.On("GetNode", context.Background(), node.ID).Return(node, nil).Once()

	resolve, err := controller.NewDatabaseNodeResolver(database)
	require.NoError(t, err)
	got, err := resolve(context.Background(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, node.ID, got.ID)
	database.AssertExpectations(t)
}

func TestNewDatabaseNodeResolverRejectsTypedNilDatabase(t *testing.T) {
	t.Parallel()

	var database *testutil.MockDB
	resolve, err := controller.NewDatabaseNodeResolver(database)
	require.Error(t, err)
	assert.Nil(t, resolve)
	assert.Contains(t, err.Error(), "database must be non-nil")
}

func TestNewNodeResolverDistinguishesNodesWithSameIP(t *testing.T) {
	t.Parallel()

	ip := "10.0.0.11"
	first := db.Node{ID: uuid.New(), Name: "cluster-a-cp-0", IP: ip}
	second := db.Node{ID: uuid.New(), Name: "cluster-b-cp-0", IP: ip}
	resolve := controller.NewNodeResolver([]db.Node{first, second})

	got, err := resolve(context.Background(), second.ID)
	require.NoError(t, err)
	assert.Equal(t, second.ID, got.ID)
}

// newTestTransport builds the transport the adapters dial through. The default
// config enables neither external mTLS nor TLS/JWT, so it needs no certificates
// or issuer to construct.
func newTestTransport(t *testing.T) *agent.Transport {
	t.Helper()

	transport, err := agent.NewTransport(t.Context(), config.Config{NodeAgent: config.NodeAgentConfig{Port: "15005"}}, "example.org", nil)
	require.NoError(t, err)
	return transport
}

// TestNewSPIFFEDialer_NilDependenciesFailClosed verifies the constructor refuses
// a nil transport or resolver so a worker-startup wiring bug surfaces
// immediately rather than at first dispatch.
func TestNewSPIFFEDialer_NilDependenciesFailClosed(t *testing.T) {
	t.Parallel()

	d, err := controller.NewSPIFFEDialer(newTestTransport(t), nil)
	assert.Nil(t, d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NewSPIFFEDialer")

	d, err = controller.NewSPIFFEDialer(nil, controller.NewNodeResolver(nil))
	assert.Nil(t, d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transport must be non-nil")
}

func TestNewSPIFFEDialer_ValidResolver(t *testing.T) {
	t.Parallel()

	d, err := controller.NewSPIFFEDialer(newTestTransport(t), controller.NewNodeResolver(nil))
	require.NoError(t, err)
	assert.NotNil(t, d)
}

func TestNewMethodSupportChecker_NilDependenciesFailClosed(t *testing.T) {
	t.Parallel()

	c, err := controller.NewMethodSupportChecker(newTestTransport(t), nil)
	assert.Nil(t, c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NewMethodSupportChecker")

	c, err = controller.NewMethodSupportChecker(nil, controller.NewNodeResolver(nil))
	assert.Nil(t, c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transport must be non-nil")
}

func TestNewMethodSupportChecker_ValidResolver(t *testing.T) {
	t.Parallel()

	c, err := controller.NewMethodSupportChecker(newTestTransport(t), controller.NewNodeResolver(nil))
	require.NoError(t, err)
	assert.NotNil(t, c)
}
