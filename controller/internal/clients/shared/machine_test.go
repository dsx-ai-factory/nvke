// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shared_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

func TestBuildBootstrapSecret(t *testing.T) {
	node := db.Node{Name: "worker-0"}
	cluster := db.Cluster{Name: "cluster-a"}

	secret := shared.BuildBootstrapSecret(node, cluster, "capi-cluster-a", "user-data")

	assert.Equal(t, "worker-0-bootstrap-data", secret.Name)
	assert.Equal(t, "capi-cluster-a", secret.Namespace)
	assert.Equal(t, "cluster-a", secret.Labels["cluster.x-k8s.io/cluster-name"])
	assert.Equal(t, []byte("user-data"), secret.Data["value"])
}

func TestBuildMachine(t *testing.T) {
	nodeID := uuid.MustParse("60000000-0000-0000-0000-000000000001")
	nodeSetID := uuid.MustParse("60000000-0000-0000-0000-000000000002")
	controlPlaneNodeID := uuid.MustParse("60000000-0000-0000-0000-000000000003")
	controlPlaneNodeSetID := uuid.MustParse("60000000-0000-0000-0000-000000000004")
	missingVersionNodeSetID := uuid.MustParse("60000000-0000-0000-0000-000000000005")
	bootstrapSecretName := "worker-0-bootstrap-data"
	infrastructureRef := clusterv1.ContractVersionedObjectReference{
		APIGroup: "infrastructure.cluster.x-k8s.io",
		Kind:     "NicoMachine",
		Name:     "worker-0",
	}

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
			machine, err := shared.BuildMachine(
				params.node,
				params.nodeSet,
				params.nodePool,
				params.cluster,
				"capi-cluster-a",
				params.bootstrapSecret,
				infrastructureRef,
			)
			if params.wantErrContain != "" {
				require.ErrorContains(t, err, params.wantErrContain)
				return
			}
			require.NoError(t, err)

			assert.Equal(t, params.node.Name, machine.Name)
			assert.Equal(t, "capi-cluster-a", machine.Namespace)
			assert.Equal(t, params.cluster.Name, machine.Labels["cluster.x-k8s.io/cluster-name"])
			assert.Equal(t, params.node.ID.String(), machine.Annotations[keys.MachineHostID.String()])
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

func TestApplyMachine_PreservesExistingFailureDomain(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	existing := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "cp-0", Namespace: "nke-system"},
		Spec: clusterv1.MachineSpec{
			ClusterName:   "cluster-a",
			FailureDomain: "fd-a",
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	desired := existing.DeepCopy()
	desired.Spec.FailureDomain = "fd-b"

	require.NoError(t, shared.ApplyMachine(context.Background(), k8sClient, "cloud-workflow-engine", desired))

	actual := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKeyFromObject(existing), actual))
	assert.Equal(t, "fd-a", actual.Spec.FailureDomain)
}

func TestBuildMachine_Metadata(t *testing.T) {
	type parameters struct{ annotations, labels map[string]string }
	for name, params := range map[string]parameters{
		"omitted":    {},
		"configured": {annotations: map[string]string{"example.com/setup": "gpu,network"}, labels: map[string]string{"example.com/pool": "gpu"}},
	} {
		t.Run(name, func(t *testing.T) {
			pool := db.NodePool{MachineAnnotations: params.annotations, MachineLabels: params.labels}
			machine, err := shared.BuildMachine(db.Node{Name: "worker-0"}, db.NodeSet{KubernetesVersion: "1.32.1"}, pool,
				db.Cluster{Name: "cluster-a"}, "nke-system", "bootstrap", clusterv1.ContractVersionedObjectReference{
					APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "DockerMachine", Name: "worker-0",
				})
			require.NoError(t, err)
			for key, value := range params.annotations {
				require.Equal(t, value, machine.Annotations[key])
			}
			for key, value := range params.labels {
				require.Equal(t, value, machine.Labels[key])
			}
			require.Equal(t, "plaintext", machine.Annotations[keys.MachineAgentConnection.String()])
			require.Equal(t, "cluster-a", machine.Labels[clusterv1.ClusterNameLabel])
			require.Equal(t, "cwe-controlled", machine.Annotations[clusterv1.ExcludeNodeDrainingAnnotation])
			machine.Annotations["example.com/only-machine"] = "value"
			machine.Labels["example.com/only-machine"] = "value"
			require.NotContains(t, pool.MachineAnnotations, "example.com/only-machine")
			require.NotContains(t, pool.MachineLabels, "example.com/only-machine")
		})
	}
}

func TestUnit_MachineAnnotations(t *testing.T) {
	id := uuid.MustParse("60000000-0000-0000-0000-000000000001")
	type parameters struct {
		node      db.Node
		transport string
	}
	for name, params := range map[string]parameters{
		"plaintext":       {node: db.Node{ID: id}, transport: "plaintext"},
		"legacy SPIFFE":   {node: db.Node{ID: id, SpiffeEnabled: true}, transport: "spiffe"},
		"JWT SPIFFE":      {node: db.Node{ID: id, SpiffeEnabled: true, NodeAgentTLSJWTEnabled: true}, transport: "spiffe-jwt"},
		"external legacy": {node: db.Node{ID: id, NodeAgentExternalMTLSEnabled: true}, transport: "external-mtls"},
		"external new":    {node: db.Node{ID: id, NodeAgentExternalMTLSEnabled: true, AgentVersion: "v0.3.0"}, transport: "external-mtls"},
		"external JWT":    {node: db.Node{ID: id, NodeAgentExternalMTLSEnabled: true, NodeAgentTLSJWTEnabled: true}, transport: "external-mtls-jwt"},
	} {
		t.Run(name, func(t *testing.T) {
			annotations := shared.MachineAnnotations(params.node)
			require.Equal(t, params.transport, annotations[keys.MachineAgentConnection.String()])
		})
	}
}
