// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/dsx-ai-factory/nvke/agent/internal/k8s"
)

func nodeWithLabels(name string, labels map[string]string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
}

func TestNodeHasLabel(t *testing.T) {
	ctx := context.Background()

	t.Run("returns true when key=value label matches", func(t *testing.T) {
		node := nodeWithLabels("cp1", map[string]string{
			"nvke.nvidia.com/k8s-api-encryption-configured": "true",
		})
		k8s.SetClientsetForTest(fake.NewSimpleClientset(node))

		ok, err := k8s.NodeHasLabel(ctx, "cp1", "nvke.nvidia.com/k8s-api-encryption-configured=true")
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("returns false when label key is absent", func(t *testing.T) {
		node := nodeWithLabels("cp1", map[string]string{})
		k8s.SetClientsetForTest(fake.NewSimpleClientset(node))

		ok, err := k8s.NodeHasLabel(ctx, "cp1", "nvke.nvidia.com/k8s-api-encryption-configured=true")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("returns false when key matches but value differs", func(t *testing.T) {
		node := nodeWithLabels("cp1", map[string]string{
			"nvke.nvidia.com/k8s-api-encryption-configured": "false",
		})
		k8s.SetClientsetForTest(fake.NewSimpleClientset(node))

		ok, err := k8s.NodeHasLabel(ctx, "cp1", "nvke.nvidia.com/k8s-api-encryption-configured=true")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("old bug: full key=value string as map key returns false", func(t *testing.T) {
		// Regression: before the fix, NodeHasLabel looked up node.Labels["key=value"]
		// instead of node.Labels["key"]. Verify the fix handles this correctly.
		node := nodeWithLabels("cp1", map[string]string{
			"nvke.nvidia.com/k8s-api-encryption-configured=true": "true", // wrong key format
		})
		k8s.SetClientsetForTest(fake.NewSimpleClientset(node))

		ok, err := k8s.NodeHasLabel(ctx, "cp1", "nvke.nvidia.com/k8s-api-encryption-configured=true")
		require.NoError(t, err)
		assert.False(t, ok, "should not match label stored with key=value as the map key")
	})
}

func TestLabelNode(t *testing.T) {
	ctx := context.Background()

	t.Run("sets key=value label correctly", func(t *testing.T) {
		node := nodeWithLabels("cp1", map[string]string{})
		cs := fake.NewSimpleClientset(node)
		k8s.SetClientsetForTest(cs)

		err := k8s.LabelNode(ctx, "cp1", "nvke.nvidia.com/k8s-api-encryption-configured=true")
		require.NoError(t, err)

		updated, err := cs.CoreV1().Nodes().Get(ctx, "cp1", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "true", updated.Labels["nvke.nvidia.com/k8s-api-encryption-configured"])
	})
}

func TestRemoveNodeLabel(t *testing.T) {
	ctx := context.Background()

	t.Run("removes label by key", func(t *testing.T) {
		node := nodeWithLabels("cp1", map[string]string{
			"nvke.nvidia.com/k8s-api-encryption-configured": "true",
		})
		cs := fake.NewSimpleClientset(node)
		k8s.SetClientsetForTest(cs)

		err := k8s.RemoveNodeLabel(ctx, "cp1", "nvke.nvidia.com/k8s-api-encryption-configured=true")
		require.NoError(t, err)

		updated, err := cs.CoreV1().Nodes().Get(ctx, "cp1", metav1.GetOptions{})
		require.NoError(t, err)
		_, present := updated.Labels["nvke.nvidia.com/k8s-api-encryption-configured"]
		assert.False(t, present)
	})

	t.Run("retries an update that conflicts with another writer", func(t *testing.T) {
		node := nodeWithLabels("cp1", map[string]string{
			"nvke.nvidia.com/k8s-api-encryption-configured": "true",
		})
		cs := fake.NewSimpleClientset(node)
		conflicted := false
		cs.PrependReactor("update", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
			if conflicted {
				return false, nil, nil
			}
			conflicted = true
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "cp1", errors.New("the object has been modified"))
		})
		k8s.SetClientsetForTest(cs)

		err := k8s.RemoveNodeLabel(ctx, "cp1", "nvke.nvidia.com/k8s-api-encryption-configured=true")
		require.NoError(t, err)

		updated, err := cs.CoreV1().Nodes().Get(ctx, "cp1", metav1.GetOptions{})
		require.NoError(t, err)
		_, present := updated.Labels["nvke.nvidia.com/k8s-api-encryption-configured"]
		assert.False(t, present)
	})
}

func TestNodeHasLabelOrMigrate(t *testing.T) {
	const key = "nvke.nvidia.com/k8s-api-encryption-configured"
	const legacyKey = "nke.io/k8s-api-encryption-configured"
	tests := []struct {
		name     string
		labels   map[string]string
		want     bool
		migrated bool
	}{
		{name: "legacy label migrates", labels: map[string]string{legacyKey: "true"}, want: true, migrated: true},
		{name: "new label already present", labels: map[string]string{key: "true"}, want: true},
		{name: "explicit new false wins", labels: map[string]string{key: "false", legacyKey: "true"}},
		{name: "legacy false is not treated as enabled", labels: map[string]string{legacyKey: "false"}},
		{name: "missing label is not enabled", labels: map[string]string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(nodeWithLabels("cp1", tc.labels))
			k8s.SetClientsetForTest(client)
			t.Cleanup(func() { k8s.SetClientsetForTest(nil) })
			ok, err := k8s.NodeHasLabelOrMigrate(context.Background(), "cp1", key+"=true", legacyKey+"=true")
			require.NoError(t, err)
			assert.Equal(t, tc.want, ok)
			node, err := client.CoreV1().Nodes().Get(context.Background(), "cp1", metav1.GetOptions{})
			require.NoError(t, err)
			if tc.migrated {
				assert.Equal(t, "true", node.Labels[key])
				assert.NotContains(t, node.Labels, legacyKey)
				// Reading it again performs no second update.
				client.ClearActions()
				ok, err = k8s.NodeHasLabelOrMigrate(context.Background(), "cp1", key+"=true", legacyKey+"=true")
				require.NoError(t, err)
				assert.True(t, ok)
				require.Len(t, client.Actions(), 1)
				assert.Equal(t, "get", client.Actions()[0].GetVerb())
			}
		})
	}
}
