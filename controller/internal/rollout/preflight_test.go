// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rollout_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
)

// TestRegisterClusterPreflight_PanicsOnDuplicate locks in the
// panic-on-dup safety of the RegisterClusterPreflight helper — silent
// overwrite at init() time would route a dispatch lookup to the wrong
// implementation. Mirrors TestRegisterNodeGuard_PanicsOnDuplicate.
func TestRegisterClusterPreflight_PanicsOnDuplicate(t *testing.T) {
	const name = "test-dup-cluster-preflight"
	reg := rollout.NewRegistry()

	fn := func(_ context.Context, _ rollout.Cluster, _ map[string]string) error { return nil }
	reg.RegisterClusterPreflight(name, fn)
	assert.Panics(t, func() { reg.RegisterClusterPreflight(name, fn) },
		"second registration under the same name MUST panic")
}

// TestRegisterNodePreflight_PanicsOnDuplicate is the node-preflight
// mirror of TestRegisterClusterPreflight_PanicsOnDuplicate.
func TestRegisterNodePreflight_PanicsOnDuplicate(t *testing.T) {
	const name = "test-dup-node-preflight"
	reg := rollout.NewRegistry()

	fn := func(_ context.Context, _ rollout.Cluster, _ rollout.Node, _ map[string]string) error { return nil }
	reg.RegisterNodePreflight(name, fn)
	assert.Panics(t, func() { reg.RegisterNodePreflight(name, fn) },
		"second registration under the same name MUST panic")
}

// TestRegisterPreflight_PanicsOnEmptyNameOrNilFunc locks in the
// fail-fast checks: an empty name or a nil function at init() time
// would otherwise surface much later as a confusing dispatch-time
// lookup miss or a nil-call panic. Covers both cluster and node
// variants.
func TestRegisterPreflight_PanicsOnEmptyNameOrNilFunc(t *testing.T) {
	reg := rollout.NewRegistry()
	clusterFn := func(_ context.Context, _ rollout.Cluster, _ map[string]string) error { return nil }
	nodeFn := func(_ context.Context, _ rollout.Cluster, _ rollout.Node, _ map[string]string) error { return nil }

	assert.Panics(t, func() { reg.RegisterClusterPreflight("", clusterFn) },
		"RegisterClusterPreflight must panic on empty name")
	assert.Panics(t, func() { reg.RegisterClusterPreflight("test-nil-fn-cluster-preflight", nil) },
		"RegisterClusterPreflight must panic on nil func")
	assert.Panics(t, func() { reg.RegisterNodePreflight("", nodeFn) },
		"RegisterNodePreflight must panic on empty name")
	assert.Panics(t, func() { reg.RegisterNodePreflight("test-nil-fn-node-preflight", nil) },
		"RegisterNodePreflight must panic on nil func")
}
