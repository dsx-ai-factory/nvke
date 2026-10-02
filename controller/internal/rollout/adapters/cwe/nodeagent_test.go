// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cwe_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/agent"
	"github.com/dsx-ai-factory/nke/controller/internal/config"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout/adapters/cwe"
	"github.com/dsx-ai-factory/nke/controller/internal/testutil"
)

func TestNewNodeResolver(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "cp-0", IP: "10.0.0.11"}
	resolve := cwe.NewNodeResolver([]db.Node{node})

	got, err := resolve(context.Background(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, node.ID, got.ID)

	_, err = resolve(context.Background(), uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, cwe.ErrNodeNotFound)
}

func TestNewDatabaseNodeResolverUsesCurrentInventory(t *testing.T) {
	t.Parallel()

	node := db.Node{ID: uuid.New(), Name: "cp-0", IP: "10.0.0.11"}
	database := &testutil.MockDB{}
	database.On("GetNode", context.Background(), node.ID).Return(node, nil).Once()

	resolve, err := cwe.NewDatabaseNodeResolver(database)
	require.NoError(t, err)
	got, err := resolve(context.Background(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, node.ID, got.ID)
	database.AssertExpectations(t)
}

func TestNewDatabaseNodeResolverRejectsTypedNilDatabase(t *testing.T) {
	t.Parallel()

	var database *testutil.MockDB
	resolve, err := cwe.NewDatabaseNodeResolver(database)
	require.Error(t, err)
	assert.Nil(t, resolve)
	assert.Contains(t, err.Error(), "database must be non-nil")
}

func TestNewNodeResolverDistinguishesNodesWithSameIP(t *testing.T) {
	t.Parallel()

	ip := "10.0.0.11"
	first := db.Node{ID: uuid.New(), Name: "cluster-a-cp-0", IP: ip}
	second := db.Node{ID: uuid.New(), Name: "cluster-b-cp-0", IP: ip}
	resolve := cwe.NewNodeResolver([]db.Node{first, second})

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

	d, err := cwe.NewSPIFFEDialer(newTestTransport(t), nil)
	assert.Nil(t, d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NewSPIFFEDialer")

	d, err = cwe.NewSPIFFEDialer(nil, cwe.NewNodeResolver(nil))
	assert.Nil(t, d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transport must be non-nil")
}

func TestNewSPIFFEDialer_ValidResolver(t *testing.T) {
	t.Parallel()

	d, err := cwe.NewSPIFFEDialer(newTestTransport(t), cwe.NewNodeResolver(nil))
	require.NoError(t, err)
	assert.NotNil(t, d)
}

func TestNewMethodSupportChecker_NilDependenciesFailClosed(t *testing.T) {
	t.Parallel()

	c, err := cwe.NewMethodSupportChecker(newTestTransport(t), nil)
	assert.Nil(t, c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NewMethodSupportChecker")

	c, err = cwe.NewMethodSupportChecker(nil, cwe.NewNodeResolver(nil))
	assert.Nil(t, c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transport must be non-nil")
}

func TestNewMethodSupportChecker_ValidResolver(t *testing.T) {
	t.Parallel()

	c, err := cwe.NewMethodSupportChecker(newTestTransport(t), cwe.NewNodeResolver(nil))
	require.NoError(t, err)
	assert.NotNil(t, c)
}
