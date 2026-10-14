// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

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

	k := keys.New("", "")
	err := applyNicoMachineInstanceLabels(k, node, cluster, nodePoolID, infraMachine)
	require.NoError(t, err)

	labels, found, err := unstructured.NestedStringMap(infraMachine.Object, "spec", "labels")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "keep", labels["example.com/from-template"])
	assert.Equal(t, nodeID.String(), labels[k.NodeID.String()])
	assert.Equal(t, clusterID.String(), labels[k.ClusterID.String()])
	assert.Equal(t, nodePoolID.String(), labels[k.NodePoolID.String()])
	assert.Empty(t, labels[k.MachineID.String()])
	assert.Empty(t, labels[k.NodeSiteID.String()])
	assert.Empty(t, labels[k.NodeSiteName.String()])
	assert.Empty(t, labels[k.NodeVPCID.String()])
	assert.Empty(t, labels[k.NodeVPCName.String()])
}
