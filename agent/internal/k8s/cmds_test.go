// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dsx-ai-factory/nke/agent/internal/k8s"
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
			"nke.io/k8s-api-encryption-configured": "true",
		})
		k8s.SetClientsetForTest(fake.NewSimpleClientset(node))

		ok, err := k8s.NodeHasLabel(ctx, "cp1", "nke.io/k8s-api-encryption-configured=true")
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("returns false when label key is absent", func(t *testing.T) {
		node := nodeWithLabels("cp1", map[string]string{})
		k8s.SetClientsetForTest(fake.NewSimpleClientset(node))

		ok, err := k8s.NodeHasLabel(ctx, "cp1", "nke.io/k8s-api-encryption-configured=true")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("returns false when key matches but value differs", func(t *testing.T) {
		node := nodeWithLabels("cp1", map[string]string{
			"nke.io/k8s-api-encryption-configured": "false",
		})
		k8s.SetClientsetForTest(fake.NewSimpleClientset(node))

		ok, err := k8s.NodeHasLabel(ctx, "cp1", "nke.io/k8s-api-encryption-configured=true")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("old bug: full key=value string as map key returns false", func(t *testing.T) {
		// Regression: before the fix, NodeHasLabel looked up node.Labels["key=value"]
		// instead of node.Labels["key"]. Verify the fix handles this correctly.
		node := nodeWithLabels("cp1", map[string]string{
			"nke.io/k8s-api-encryption-configured=true": "true", // wrong key format
		})
		k8s.SetClientsetForTest(fake.NewSimpleClientset(node))

		ok, err := k8s.NodeHasLabel(ctx, "cp1", "nke.io/k8s-api-encryption-configured=true")
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

		err := k8s.LabelNode(ctx, "cp1", "nke.io/k8s-api-encryption-configured=true")
		require.NoError(t, err)

		updated, err := cs.CoreV1().Nodes().Get(ctx, "cp1", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "true", updated.Labels["nke.io/k8s-api-encryption-configured"])
	})
}

func TestRemoveNodeLabel(t *testing.T) {
	ctx := context.Background()

	t.Run("removes label by key", func(t *testing.T) {
		node := nodeWithLabels("cp1", map[string]string{
			"nke.io/k8s-api-encryption-configured": "true",
		})
		cs := fake.NewSimpleClientset(node)
		k8s.SetClientsetForTest(cs)

		err := k8s.RemoveNodeLabel(ctx, "cp1", "nke.io/k8s-api-encryption-configured=true")
		require.NoError(t, err)

		updated, err := cs.CoreV1().Nodes().Get(ctx, "cp1", metav1.GetOptions{})
		require.NoError(t, err)
		_, present := updated.Labels["nke.io/k8s-api-encryption-configured"]
		assert.False(t, present)
	})
}
