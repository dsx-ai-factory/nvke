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

// newNodeAgentTokenSourceCreateMachineFixture builds a CreateMachine fixture over a
// provider-neutral machine template: the token source is not specific to any
// infrastructure provider.
func newNodeAgentTokenSourceCreateMachineFixture(
	t *testing.T, tokenSource db.NodeAgentTokenSource,
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

	nodeSet := db.NodeSet{
		KubernetesVersion: "1.32.1",
		NodeAgent:         &db.NodeAgentSettings{TokenSource: tokenSource},
	}
	nodeSet.SetCapi(db.CAPINodePoolProvider{InfraMachineTemplate: rawTemplate})

	var metadataServiceArgs []bool
	joinTokenCalls := 0
	deps := shared.MachineCreateDeps{
		K8sClient: k8sClient, Namespace: "nke-system", FieldManagerName: "test-controller",
		CreateSpireJoinToken: func(context.Context, db.Node) (string, string, error) {
			joinTokenCalls++
			return "join-token", "/join-token-spire-agent/host-id/x", nil
		},
		NodeAgentBootstrapFiles: func(node db.Node) ([]db.CloudInitFile, error) {
			metadataServiceArgs = append(metadataServiceArgs, node.UsesMetadataServiceToken())
			return nil, nil
		},
		GetTrustDomainBundle: func(context.Context, config.Config) (string, string, error) {
			return "example.org", "bundle", nil
		},
	}
	return k8sClient, deps, nodeSet, &metadataServiceArgs, &joinTokenCalls
}

func TestClient_CreateMachine_MetadataServiceToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k8sClient, deps, nodeSet, metadataServiceArgs, joinTokenCalls := newNodeAgentTokenSourceCreateMachineFixture(t, db.NodeAgentTokenSourceMetadataService)
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
		NodeAgentTokenSource:         db.NodeAgentTokenSourceMetadataService,
	}
	require.NoError(t, New().CreateMachine(ctx, deps, node, nodeSet, db.NodePool{ControlPlane: true}, db.Cluster{Name: "cluster-a"}))

	assert.Equal(t, []bool{true}, *metadataServiceArgs)
	assert.Zero(t, *joinTokenCalls)
	machine := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKey{Name: node.Name, Namespace: deps.Namespace}, machine))
	assert.Empty(t, machine.Annotations[keys.MachineSpireAgentSpiffeIDPath.String()])
}

func TestClient_CreateMachine_TokenExchangeJoinsWithASpireJoinToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, deps, nodeSet, metadataServiceArgs, joinTokenCalls := newNodeAgentTokenSourceCreateMachineFixture(t, "")

	node := db.Node{ID: uuid.New(), Name: "cp-0", SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodJoinToken}
	require.NoError(t, New().CreateMachine(ctx, deps, node, nodeSet, db.NodePool{ControlPlane: true}, db.Cluster{Name: "cluster-a"}))

	assert.Equal(t, []bool{false}, *metadataServiceArgs)
	assert.Equal(t, 1, *joinTokenCalls, "the default token source still mints a join token")
}
