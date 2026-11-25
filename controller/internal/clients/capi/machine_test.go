// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_renderInfraMachine(t *testing.T) {
	t.Run("renders the caller-supplied template for the node", func(t *testing.T) {
		template := json.RawMessage(`{
			"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
			"kind": "DockerMachineTemplate",
			"spec": {
				"template": {
					"spec": {
						"customImage": "kindest/node:v1.33.0"
					}
				}
			}
		}`)
		nodeSet := db.NodeSet{}
		nodeSet.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: template})
		cluster := db.Cluster{ID: uuid.New(), Name: "test-cluster", ProviderType: db.ProviderTypeCAPI}
		node := db.Node{Name: "worker-0"}

		obj, err := New().renderInfraMachine("", "capi-test-cluster", node, nodeSet, db.NodePool{}, cluster)
		require.NoError(t, err)

		assert.Equal(t, "worker-0", obj.GetName())
		assert.Equal(t, "capi-test-cluster", obj.GetNamespace())
		assert.Equal(t, "DockerMachine", obj.GetObjectKind().GroupVersionKind().Kind)
	})

	t.Run("missing provider payload errors", func(t *testing.T) {
		_, err := New().renderInfraMachine("", "ns", db.Node{}, db.NodeSet{}, db.NodePool{}, db.Cluster{})
		require.Error(t, err)
		require.ErrorIs(t, err, db.ErrMissingProviderPayload)
	})

	t.Run("invalid template errors", func(t *testing.T) {
		nodeSet := db.NodeSet{}
		nodeSet.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: json.RawMessage(`{`)})

		_, err := New().renderInfraMachine("", "ns", db.Node{}, nodeSet, db.NodePool{}, db.Cluster{})
		require.Error(t, err)
	})
}

// The selector side of this lives in KubeVirtSpiffeController; without the
// label it writes nothing a Service could select.
func TestUnit_ApplyKubeVirtMachinePodLabels(t *testing.T) {
	t.Parallel()
	nodeID := uuid.MustParse("3f2a9c11-4b7d-4a0e-8c3f-1d2e5a6b7c80")

	t.Run("labels a KubevirtMachine's VM pod with the node ID", func(t *testing.T) {
		t.Parallel()
		infraMachine := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": KubeVirtInfraGroup + "/" + KubeVirtInfraAPIVersion,
			"kind":       KubeVirtMachineKind,
			"spec": map[string]any{"virtualMachineTemplate": map[string]any{
				"spec": map[string]any{"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"keep": "me"}},
				}},
			}},
		}}

		require.NoError(t, applyKubeVirtMachinePodLabels(db.Node{ID: nodeID}, infraMachine))

		labels, found, err := unstructured.NestedStringMap(infraMachine.Object,
			"spec", "virtualMachineTemplate", "spec", "template", "metadata", "labels")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, map[string]string{"keep": "me", NodeIDLabel: nodeID.String()}, labels)
	})

	t.Run("leaves another infrastructure kind alone", func(t *testing.T) {
		t.Parallel()
		infraMachine := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
			"kind":       "DockerMachineTemplate",
		}}

		require.NoError(t, applyKubeVirtMachinePodLabels(db.Node{ID: nodeID}, infraMachine))

		_, found, err := unstructured.NestedStringMap(infraMachine.Object, "spec")
		require.NoError(t, err)
		require.False(t, found)
	})
}
