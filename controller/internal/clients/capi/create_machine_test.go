// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"encoding/json"
	"testing"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

func TestApplyNicoMachineInstanceLabels_MergesOntoTemplateSpec(t *testing.T) {
	nodeID := uuid.MustParse("72000000-0000-0000-0000-000000000001")
	clusterID := uuid.MustParse("72000000-0000-0000-0000-000000000002")
	nodePoolID := uuid.MustParse("72000000-0000-0000-0000-000000000003")
	siteID := uuid.MustParse("72000000-0000-0000-0000-000000000004")
	vpcID := uuid.MustParse("72000000-0000-0000-0000-000000000005")

	infraMachine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": nicov1.GroupVersion.String(),
		"kind":       "NicoMachine",
		"spec": map[string]any{
			"labels": map[string]any{
				"example.com/from-template": "keep",
			},
		},
	}}
	node := db.Node{
		ID:   nodeID,
		Nico: &db.NicoNodeProvider{ForgeMachineID: "machine-abc123"},
	}
	cluster := db.Cluster{
		ID: clusterID,
		Nico: &db.NicoClusterProvider{
			OrgID:  "org-1",
			SiteID: siteID,
			VpcID:  vpcID,
		},
	}

	err := applyNicoMachineInstanceLabels(node, cluster, nodePoolID, infraMachine)
	require.NoError(t, err)

	labels, found, err := unstructured.NestedStringMap(infraMachine.Object, "spec", "labels")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "keep", labels["example.com/from-template"])
	assert.Equal(t, nodeID.String(), labels[keys.NodeID.String()])
	assert.Equal(t, clusterID.String(), labels[keys.ClusterID.String()])
	assert.Equal(t, nodePoolID.String(), labels[keys.NodePoolID.String()])
	assert.Empty(t, labels[keys.MachineID.String()])
	assert.Empty(t, labels[keys.NodeSiteID.String()])
	assert.Empty(t, labels[keys.NodeSiteName.String()])
	assert.Empty(t, labels[keys.NodeVPCID.String()])
	assert.Empty(t, labels[keys.NodeVPCName.String()])
}

func TestClient_CreateMachine_Metadata(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	template := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "DockerMachineTemplate",
		"metadata": map[string]interface{}{"name": "workers", "namespace": "nke-system"},
		"spec":     map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"customImage": "kindest/node:test"}}},
	}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template).Build()
	rawTemplate, err := json.Marshal(template.Object)
	require.NoError(t, err)
	nodeSet := db.NodeSet{KubernetesVersion: "1.32.1"}
	nodeSet.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: rawTemplate})
	pool := db.NodePool{MachineAnnotations: map[string]string{"example.com/setup": "gpu,network"}, MachineLabels: map[string]string{"example.com/pool": "gpu"}}
	deps := shared.MachineCreateDeps{
		K8sClient: k8sClient, Namespace: "nke-system", FieldManagerName: "test-controller",
		CreateSpireJoinToken:    func(context.Context, db.Node) (string, string, error) { return "", "", nil },
		NodeAgentBootstrapFiles: func(db.Node) ([]db.CloudInitFile, error) { return nil, nil },
	}
	node := db.Node{ID: uuid.New(), Name: "worker-0"}
	cluster := db.Cluster{Name: "cluster-a"}
	require.NoError(t, New().CreateMachine(ctx, deps, node, nodeSet, pool, cluster))
	machine := &clusterv1.Machine{}
	key := client.ObjectKey{Name: node.Name, Namespace: deps.Namespace}
	require.NoError(t, k8sClient.Get(ctx, key, machine))
	require.Equal(t, "gpu,network", machine.Annotations["example.com/setup"])
	require.Equal(t, "gpu", machine.Labels["example.com/pool"])
	delete(machine.Annotations, "example.com/setup")
	machine.Labels["example.com/pool"] = "manual"
	require.NoError(t, k8sClient.Update(ctx, machine))
	require.NoError(t, New().CreateMachine(ctx, deps, node, nodeSet, pool, cluster))
	require.NoError(t, k8sClient.Get(ctx, key, machine))
	require.Equal(t, "gpu,network", machine.Annotations["example.com/setup"])
	require.Equal(t, "gpu", machine.Labels["example.com/pool"])
	infra := &unstructured.Unstructured{}
	infra.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta1")
	infra.SetKind("DockerMachine")
	require.NoError(t, k8sClient.Get(ctx, key, infra))
	require.NotContains(t, infra.GetAnnotations(), "example.com/setup")
	require.NotContains(t, infra.GetLabels(), "example.com/pool")
}
