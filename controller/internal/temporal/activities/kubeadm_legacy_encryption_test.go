// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nodeagent "github.com/dsx-ai-factory/nvke/controller/internal/agent"
	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/kubeadmconfig"
)

const legacyFixtureKey = "legacy-public-fixture-key"

// legacyKeySecret is a key Secret written before storage names were recorded.
func legacyKeySecret(clusterID uuid.UUID) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "nke", Name: constants.EtcdEncryptionKeySecretName(clusterID)},
		Data:       map[string][]byte{constants.EtcdEncryptionKeyFieldActive: []byte(legacyFixtureKey)},
	}
}

func olderAgent(node db.Node) error {
	return fmt.Errorf("node %s %w", node.Name, nodeagent.ErrEncryptionIdentityUnsupported)
}

func TestBootstrapKeyNameForLegacySecret(t *testing.T) {
	for _, test := range []struct {
		name      string
		peer2     func(db.Node) (string, error)
		wantName  string
		wantError string
	}{
		{name: "every peer predates identity discovery", peer2: func(n db.Node) (string, error) { return "", olderAgent(n) }, wantName: constants.LegacyAPIEncryptionKeyName},
		{name: "upgraded peer proves the legacy name", peer2: func(db.Node) (string, error) { return constants.LegacyAPIEncryptionKeyName, nil }, wantName: constants.LegacyAPIEncryptionKeyName},
		{name: "upgraded peer proves a rotated name", peer2: func(db.Node) (string, error) { return "kube-api-encryption-key-3", nil }, wantError: "upgrade older node agents"},
		{name: "unreachable peer still blocks", peer2: func(db.Node) (string, error) { return "", errors.New("unreachable") }, wantError: "check connectivity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, clusterID := context.Background(), uuid.New()
			a := &Activities{cfg: config.Config{PodNamespace: "nke"}, k8sClient: fake.NewClientBuilder().WithObjects(legacyKeySecret(clusterID)).Build()}
			withIdentityTestNodes(t, a, clusterID, func(node db.Node, _ string) (string, error) {
				if node.Name == "cp-1" {
					return "", olderAgent(node)
				}
				return test.peer2(node)
			})
			name, err := a.bootstrapKubeAPIEncryptionKeyName(ctx, clusterID)
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
			} else {
				require.NoError(t, err)
				require.Equal(t, test.wantName, name)
			}
			stored, err := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
			require.NoError(t, err)
			require.NotContains(t, stored.Data, constants.EtcdEncryptionKeyFieldActiveName, "an unproven name is never recorded")
		})
	}
}

func TestControlPlaneJoinSucceedsWithLegacySecretAndOlderPeers(t *testing.T) {
	cluster, joiningNode, joinedNode, nodeSet, nodePool := snapshotJoinRecords()
	cluster.EtcdEncrypted = true
	cluster.OIDCAuthEnabled = false
	database := snapshotJoinDatabase(cluster, joiningNode, joinedNode, nodeSet, nodePool)
	// Identity discovery lists the control plane once more than the plain join.
	database.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{joinedNode}, nil).Once()
	tenantClient := newKubeadmConfigTenantClient(t, snapshotKubeadmClusterConfiguration())
	require.NoError(t, tenantClient.Create(context.Background(), etcdMirrorPod(joinedNode.Name,
		"--quota-backend-bytes="+kubeadmconfig.EtcdQuotaBackendBytes)))
	fakeAgent := &snapshotWiringAgent{FakeClient: &agenttest.FakeClient{
		GetEtcdEncryptionKeyIdentityHook: func(node db.Node, _ string) (string, error) { return "", olderAgent(node) },
	}, joinStatus: true}
	a := snapshotActivities(database, fakeAgent, config.Config{PodNamespace: "nke"}, legacyKeySecret(cluster.ID))
	a.tenantClientProvider = &fakeTenantClientProvider{tenantClient: tenantClient}

	require.NoError(t, executeSnapshotJoin(t, a, joiningNode))

	req := requireSingleJoinRequest(t, fakeAgent)
	require.True(t, req.GetApiEncryptionKey() == legacyFixtureKey, "join must carry the existing key") //nolint:testifylint // Do not print key material.
	require.Equal(t, constants.LegacyAPIEncryptionKeyName, req.GetApiEncryptionKeyName())
	database.AssertExpectations(t)
}

func TestRetriedInitSucceedsWithLegacySecret(t *testing.T) {
	cluster, initialNode, nodeSet, nodePool := snapshotInitRecords()
	cluster.EtcdEncrypted = true
	cluster.OIDCAuthEnabled = false
	database := snapshotInitDatabase(cluster, initialNode, nodeSet, nodePool)
	database.On("ListNodes", mock.Anything, mock.Anything).Return([]db.Node{initialNode}, nil).Once() // not joined yet
	fakeAgent := &snapshotWiringAgent{FakeClient: &agenttest.FakeClient{}, initStatus: true}
	a := snapshotActivities(database, fakeAgent, config.Config{PodNamespace: "nke"}, legacyKeySecret(cluster.ID))

	require.NoError(t, executeSnapshotInit(t, a, KubeadmInitInput{ClusterID: cluster.ID, InitialNodeID: initialNode.ID}))

	req := requireSingleInitRequest(t, fakeAgent)
	require.True(t, req.GetApiEncryptionKey() == legacyFixtureKey, "a retried init reuses the existing key") //nolint:testifylint // Do not print key material.
	require.Equal(t, constants.LegacyAPIEncryptionKeyName, req.GetApiEncryptionKeyName())
	database.AssertExpectations(t)
}

func TestEnsurePendingRotationKeyKeepsIdentityDiagnosticsAndRedactsKubernetesErrors(t *testing.T) {
	for _, test := range []struct {
		name        string
		peer        func(db.Node) (string, error)
		failUpdate  bool
		wantMessage string
	}{
		{name: "older agent", peer: func(n db.Node) (string, error) { return "", olderAgent(n) }, wantMessage: "upgrade older node agents"},
		{name: "conflicting names", peer: func(n db.Node) (string, error) { return "name-" + n.Name, nil }, wantMessage: "identities disagree"},
		{name: "kubernetes update error", peer: func(db.Node) (string, error) { return constants.LegacyAPIEncryptionKeyName, nil }, failUpdate: true, wantMessage: "inspect Kubernetes access and key state"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clusterID := uuid.New()
			builder := fake.NewClientBuilder().WithObjects(legacyKeySecret(clusterID))
			if test.failUpdate {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					Update: func(context.Context, ctrlclient.WithWatch, ctrlclient.Object, ...ctrlclient.UpdateOption) error {
						return errors.New("rejected body " + legacyFixtureKey)
					},
				})
			}
			a := &Activities{cfg: config.Config{PodNamespace: "nke"}, k8sClient: builder.Build()}
			withIdentityTestNodes(t, a, clusterID, func(node db.Node, _ string) (string, error) { return test.peer(node) })

			err := a.EnsurePendingEtcdEncryptionRotationKey(context.Background(), EtcdEncryptionClusterInput{ClusterID: clusterID})

			require.ErrorContains(t, err, test.wantMessage)
			require.NotContains(t, err.Error(), legacyFixtureKey)
		})
	}
}
