// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

func TestNodeMetadataAllowlistReachesTheServices(t *testing.T) {
	t.Parallel()
	keys := []string{"node.kubernetes.io/custom"}

	impl := NewServerImpl(config.Config{NodeMetadataAllowlistedLabelKeys: keys}, slog.New(slog.DiscardHandler), nil, nil, nil)
	assert.Equal(t, keys, impl.clusterService.allowlistedLabelKeys)
	assert.Equal(t, keys, impl.nodePoolService.allowlistedLabelKeys)

	impl = NewServerImpl(config.Config{}, slog.New(slog.DiscardHandler), nil, nil, nil)
	assert.Empty(t, impl.clusterService.allowlistedLabelKeys)
	assert.Empty(t, impl.nodePoolService.allowlistedLabelKeys)
}
