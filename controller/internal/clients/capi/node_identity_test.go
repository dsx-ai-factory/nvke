// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

func TestClient_ValidateNodeAgentProfile_NodeIdentity(t *testing.T) {
	t.Parallel()

	metadataServicePool := db.NodePool{Capi: &db.CAPINodePoolProvider{NodeIdentity: db.NodeIdentityMetadataService}}
	spireAgentPool := db.NodePool{Capi: &db.CAPINodePoolProvider{}}

	tests := map[string]struct {
		profile  db.Node
		nodePool db.NodePool
		wantErr  error
	}{
		"metadata service without external mTLS": {
			profile:  db.Node{SpiffeEnabled: true, NodeAgentTLSJWTEnabled: true},
			nodePool: metadataServicePool,
			wantErr:  db.ErrMetadataServiceIdentityRequiresTLSJWT,
		},
		"metadata service without TLS/JWT": {
			profile:  db.Node{SpiffeEnabled: true, NodeAgentExternalMTLSEnabled: true},
			nodePool: metadataServicePool,
			wantErr:  db.ErrMetadataServiceIdentityRequiresTLSJWT,
		},
		"metadata service with TLS/JWT and external mTLS": {
			profile:  db.Node{SpiffeEnabled: true, NodeAgentExternalMTLSEnabled: true, NodeAgentTLSJWTEnabled: true},
			nodePool: metadataServicePool,
		},
		"SPIRE agent without external mTLS": {
			profile:  db.Node{SpiffeEnabled: true},
			nodePool: spireAgentPool,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := New().ValidateNodeAgentProfile(tt.profile, tt.nodePool)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

// newNodeIdentityCreateMachineFixture builds a CreateMachine fixture over a
// provider-neutral machine template: node identity is not specific to any
// infrastructure provider.
func newNodeIdentityCreateMachineFixture(
	t *testing.T, identity db.NodeIdentity,
) (client.Client, shared.MachineCreateDeps, db.NodeSet, *[]bool, *int) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	template := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1", "kind": "DockerMachineTemplate",
		"metadata": map[string]any{"name": "control-plane", "namespace": "nke-system"},
		"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{}}},
	}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(template).Build()
	rawTemplate, err := json.Marshal(template.Object)
	require.NoError(t, err)

	nodeSet := db.NodeSet{KubernetesVersion: "1.32.1"}
	nodeSet.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: rawTemplate, NodeIdentity: identity})

	var metadataServiceArgs []bool
	joinTokenCalls := 0
	deps := shared.MachineCreateDeps{
		K8sClient: k8sClient, Namespace: "nke-system", FieldManagerName: "test-controller",
		CreateSpireJoinToken: func(context.Context, db.Node) (string, string, error) {
			joinTokenCalls++
			return "join-token", "/join-token-spire-agent/host-id/x", nil
		},
		NodeAgentBootstrapFiles: func(node db.Node) ([]db.CloudInitFile, error) {
			metadataServiceArgs = append(metadataServiceArgs, node.UsesMetadataServiceIdentity())
			return nil, nil
		},
		GetTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
			return "example.org", "bundle", nil
		},
	}
	return k8sClient, deps, nodeSet, &metadataServiceArgs, &joinTokenCalls
}

func TestClient_CreateMachine_MetadataServiceIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k8sClient, deps, nodeSet, metadataServiceArgs, joinTokenCalls := newNodeIdentityCreateMachineFixture(t, db.NodeIdentityMetadataService)
	deps.GetTrustDomainBundle = func(context.Context, config.Config) (string, string, error) {
		return "", "", errors.New("a node without a SPIRE agent needs no trust bundle")
	}

	node := db.Node{
		ID:                           uuid.New(),
		Name:                         "cp-0",
		SpiffeEnabled:                true,
		SpireAttestationMethod:       db.SpireAttestationMethodJoinToken,
		NodeAgentExternalMTLSEnabled: true,
		NodeAgentTLSJWTEnabled:       true,
		NodeIdentity:                 db.NodeIdentityMetadataService,
	}
	require.NoError(t, New().CreateMachine(ctx, deps, node, nodeSet, db.NodePool{ControlPlane: true}, db.Cluster{Name: "cluster-a"}))

	assert.Equal(t, []bool{true}, *metadataServiceArgs)
	assert.Zero(t, *joinTokenCalls)
	machine := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: node.Name, Namespace: deps.Namespace}, machine))
	assert.Empty(t, machine.Annotations[keys.MachineSpireAgentSpiffeIDPath.String()])
}

func TestClient_CreateMachine_SpireAgentIdentityJoinsWithAToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, deps, nodeSet, metadataServiceArgs, joinTokenCalls := newNodeIdentityCreateMachineFixture(t, "")

	node := db.Node{ID: uuid.New(), Name: "cp-0", SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodJoinToken}
	require.NoError(t, New().CreateMachine(ctx, deps, node, nodeSet, db.NodePool{ControlPlane: true}, db.Cluster{Name: "cluster-a"}))

	assert.Equal(t, []bool{false}, *metadataServiceArgs)
	assert.Equal(t, 1, *joinTokenCalls, "the default identity still mints a join token")
}
