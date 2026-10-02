// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestCAPIMachineBuilders_BuildCAPIBootstrapSecret(t *testing.T) {
	createCtx := &capiMachineCreateContext{
		node:      db.Node{Name: "worker-0"},
		cluster:   db.Cluster{Name: "cluster-a"},
		namespace: "capi-cluster-a",
	}

	secret := buildCAPIBootstrapSecret(createCtx, "user-data")

	assert.Equal(t, "worker-0-bootstrap-data", secret.Name)
	assert.Equal(t, "capi-cluster-a", secret.Namespace)
	assert.Equal(t, "cluster-a", secret.Labels["cluster.x-k8s.io/cluster-name"])
	assert.Equal(t, []byte("user-data"), secret.Data["value"])
}

func TestCAPIMachineBuilders_BuildCAPIMachineForNode(t *testing.T) {
	nodeID := uuid.MustParse("60000000-0000-0000-0000-000000000001")
	nodeSetID := uuid.MustParse("60000000-0000-0000-0000-000000000002")
	controlPlaneNodeID := uuid.MustParse("60000000-0000-0000-0000-000000000003")
	controlPlaneNodeSetID := uuid.MustParse("60000000-0000-0000-0000-000000000004")
	missingVersionNodeSetID := uuid.MustParse("60000000-0000-0000-0000-000000000005")
	bootstrapSecretName := "worker-0-bootstrap-data"

	type parameters struct {
		node              db.Node
		nodeSet           db.NodeSet
		nodePool          db.NodePool
		cluster           db.Cluster
		bootstrapSecret   string
		wantVersion       string
		wantFailureDomain string
		wantControlPlane  bool
		wantErrContain    string
	}

	tests := map[string]parameters{
		"uses node set version before cluster version": {
			node: db.Node{
				ID:   nodeID,
				Name: "worker-0",
			},
			nodeSet: db.NodeSet{
				ID:                nodeSetID,
				KubernetesVersion: "1.32.1",
			},
			cluster: db.Cluster{
				Name:              "cluster-a",
				KubernetesVersion: "1.31.9",
			},
			bootstrapSecret: bootstrapSecretName,
			wantVersion:     "v1.32.1",
		},
		"falls back to cluster version": {
			node: db.Node{
				ID:            controlPlaneNodeID,
				Name:          "cp-0",
				FailureDomain: "fd-a",
			},
			nodeSet: db.NodeSet{
				ID: controlPlaneNodeSetID,
			},
			nodePool: db.NodePool{
				ControlPlane: true,
			},
			cluster: db.Cluster{
				Name:              "cluster-a",
				KubernetesVersion: "v1.31.9",
			},
			bootstrapSecret:   "cp-0-bootstrap-data",
			wantVersion:       "v1.31.9",
			wantFailureDomain: "fd-a",
			wantControlPlane:  true,
		},
		"requires kubernetes version": {
			node: db.Node{
				Name: "worker-0",
			},
			nodeSet: db.NodeSet{
				ID: missingVersionNodeSetID,
			},
			cluster: db.Cluster{
				Name: "cluster-a",
			},
			bootstrapSecret: "worker-0-bootstrap-data",
			wantErrContain:  "kubernetes version is empty",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			machine, err := buildCAPIMachineForNode(
				params.node,
				params.nodeSet,
				params.nodePool,
				params.cluster,
				"capi-cluster-a",
				params.bootstrapSecret,
				testInfrastructureRef(),
			)
			if params.wantErrContain != "" {
				require.ErrorContains(t, err, params.wantErrContain)
				return
			}
			require.NoError(t, err)

			assert.Equal(t, params.node.Name, machine.Name)
			assert.Equal(t, "capi-cluster-a", machine.Namespace)
			assert.Equal(t, params.cluster.Name, machine.Labels["cluster.x-k8s.io/cluster-name"])
			assert.Equal(t, params.node.ID.String(), machine.Annotations[constants.MachineAnnotationKeyHostID])
			assert.Equal(t, "cwe-controlled", machine.Annotations[clusterv1.ExcludeNodeDrainingAnnotation])
			assert.Equal(t, "cwe-controlled", machine.Annotations[clusterv1.ExcludeWaitForNodeVolumeDetachAnnotation])
			assert.Equal(t, params.wantVersion, machine.Spec.Version)
			assert.Equal(t, params.wantFailureDomain, machine.Spec.FailureDomain)
			assert.Equal(t, params.bootstrapSecret, *machine.Spec.Bootstrap.DataSecretName)
			assert.Equal(t, "NicoMachine", machine.Spec.InfrastructureRef.Kind)
			if params.wantControlPlane {
				assert.Contains(t, machine.Labels, "cluster.x-k8s.io/control-plane")
			}
		})
	}
}

func TestBuildNicoInstanceLabels(t *testing.T) {
	nodeID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	clusterID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	nodePoolID := uuid.MustParse("99999999-8888-7777-6666-555555555555")

	labels := buildNicoInstanceLabels(
		db.Node{ID: nodeID},
		db.Cluster{ID: clusterID},
		nodePoolID,
	)

	assert.Equal(t, map[string]string{
		constants.LabelKeyNodeID:     nodeID.String(),
		constants.LabelKeyClusterID:  clusterID.String(),
		constants.LabelKeyNodePoolID: nodePoolID.String(),
	}, labels)
}
