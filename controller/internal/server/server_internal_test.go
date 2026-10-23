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

	impl := NewServerImpl(config.Config{NodeMetadataAllowlistedLabelKeys: keys}, slog.New(slog.DiscardHandler), nil, nil, nil, nil, nil)
	assert.Equal(t, keys, impl.clusterService.allowlistedLabelKeys)
	assert.Equal(t, keys, impl.nodePoolService.allowlistedLabelKeys)

	impl = NewServerImpl(config.Config{}, slog.New(slog.DiscardHandler), nil, nil, nil, nil, nil)
	assert.Empty(t, impl.clusterService.allowlistedLabelKeys)
	assert.Empty(t, impl.nodePoolService.allowlistedLabelKeys)
}

func TestMachineMetadataAllowlistsReachNodePoolService(t *testing.T) {
	t.Parallel()
	labels := []string{"cluster.x-k8s.io/custom"}
	annotations := []string{"nke.nvidia.com/profile"}
	impl := NewServerImpl(config.Config{MachineMetadataAllowlistedLabelKeys: labels, MachineMetadataAllowlistedAnnotationKeys: annotations}, slog.New(slog.DiscardHandler), nil, nil, nil, nil, nil)
	assert.Equal(t, labels, impl.nodePoolService.machineAllowlistedLabelKeys)
	assert.Equal(t, annotations, impl.nodePoolService.machineAllowlistedAnnotationKeys)
	assert.Equal(t, labels, impl.clusterService.machineAllowlistedLabelKeys)
	assert.Equal(t, annotations, impl.clusterService.machineAllowlistedAnnotationKeys)
	assert.Empty(t, impl.nodePoolService.allowlistedLabelKeys)
	assert.Empty(t, impl.clusterService.allowlistedLabelKeys)
	impl = NewServerImpl(config.Config{}, slog.New(slog.DiscardHandler), nil, nil, nil, nil, nil)
	assert.Empty(t, impl.nodePoolService.machineAllowlistedLabelKeys)
	assert.Empty(t, impl.nodePoolService.machineAllowlistedAnnotationKeys)
}

func TestControlPlaneVIPOverrideReachesServices(t *testing.T) {
	for _, allow := range []bool{false, true} {
		impl := NewServerImpl(config.Config{AllowControlPlaneWithoutVIP: allow}, slog.New(slog.DiscardHandler), nil, nil, nil, nil, nil)
		assert.Equal(t, allow, impl.clusterService.allowControlPlaneWithoutVIP)
		assert.Equal(t, allow, impl.nodeService.allowControlPlaneWithoutVIP)
	}
}
