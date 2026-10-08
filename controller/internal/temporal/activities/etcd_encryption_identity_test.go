// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent/agenttest"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

func identityTestDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func withIdentityTestNodes(t *testing.T, a *Activities, clusterID uuid.UUID, hook func(db.Node, string) (string, error)) {
	t.Helper()
	database := &testutil.MockDB{}
	database.On("ListNodes", mock.Anything, mock.MatchedBy(func(filters db.NodeFilters) bool {
		return filters.ClusterID != nil && *filters.ClusterID == clusterID && filters.ControlPlane != nil && *filters.ControlPlane
	})).Return([]db.Node{
		{ID: uuid.New(), Name: "cp-1", IP: "192.0.2.1", Conditions: db.NodeConditions{Joined: db.Condition{Status: db.ConditionStatusTrue}}},
		{ID: uuid.New(), Name: "cp-2", IP: "192.0.2.2", Conditions: db.NodeConditions{Joined: db.Condition{Status: db.ConditionStatusTrue}}},
	}, nil)
	a.db = database
	a.nodeAgentClient = &agenttest.FakeClient{GetEtcdEncryptionKeyIdentityHook: hook}
}

func TestAdoptLegacyRotatedEncryptionIdentity(t *testing.T) {
	for _, test := range []struct {
		name        string
		secondName  string
		unavailable bool
		wantError   string
	}{
		{name: "rotated nodes agree", secondName: "kube-api-encryption-key-7"},
		{name: "different names for equal bytes", secondName: constants.LegacyAPIEncryptionKeyName, wantError: "identities disagree"},
		{name: "key absent on one joined node", wantError: "key is absent"},
		{name: "older or unreachable agent", unavailable: true, wantError: "upgrade older node agents"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, clusterID := context.Background(), uuid.New()
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "nke", Name: constants.EtcdEncryptionKeySecretName(clusterID)}, Data: map[string][]byte{constants.EtcdEncryptionKeyFieldActive: []byte("public-fixture-key")}}
			a := &Activities{cfg: config.Config{PodNamespace: "nke"}, k8sClient: fake.NewClientBuilder().WithObjects(secret).Build()}
			withIdentityTestNodes(t, a, clusterID, func(node db.Node, digest string) (string, error) {
				require.Equal(t, identityTestDigest("public-fixture-key"), digest)
				if node.Name == "cp-1" {
					return "kube-api-encryption-key-7", nil
				}
				if test.unavailable {
					return "", errors.New("unsupported identity RPC")
				}
				return test.secondName, nil
			})
			name, err := a.requireKubeAPIEncryptionKeyName(ctx, clusterID)
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
			} else {
				require.NoError(t, err)
				require.Equal(t, "kube-api-encryption-key-7", name)
				// A second bootstrap uses the persisted identity without rediscovery.
				a.nodeAgentClient = nil
				retried, err := a.requireKubeAPIEncryptionKeyName(ctx, clusterID)
				require.NoError(t, err)
				require.Equal(t, name, retried)
			}
			stored, err := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
			require.NoError(t, err)
			require.Equal(t, "public-fixture-key", string(stored.Data[constants.EtcdEncryptionKeyFieldActive]))
			if test.wantError != "" {
				require.Empty(t, stored.Data[constants.EtcdEncryptionKeyFieldActiveName])
			}
		})
	}
}

func TestRotationAdoptsLegacyPendingNameAndFinalizesIt(t *testing.T) {
	ctx, clusterID := context.Background(), uuid.New()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "nke", Name: constants.EtcdEncryptionKeySecretName(clusterID)}, Data: map[string][]byte{
		constants.EtcdEncryptionKeyFieldActive:  []byte("old-public-fixture-key"),
		constants.EtcdEncryptionKeyFieldPending: []byte("pending-public-fixture-key"),
	}}
	a := &Activities{cfg: config.Config{PodNamespace: "nke"}, k8sClient: fake.NewClientBuilder().WithObjects(secret).Build()}
	withIdentityTestNodes(t, a, clusterID, func(node db.Node, digest string) (string, error) {
		require.Equal(t, identityTestDigest("pending-public-fixture-key"), digest)
		if node.Name == "cp-1" {
			return "kube-api-encryption-key-8", nil
		}
		return "", nil // Interrupted old rollout distributed only to the first node.
	})
	pending, err := a.ensurePendingKubeAPIEncryptionRotationKey(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, "pending-public-fixture-key", pending)
	stored, err := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, "kube-api-encryption-key-8", string(stored.Data[constants.EtcdEncryptionKeyFieldPendingName]))
	// Finalization occurs after the workflow's distribute/promote/rewrite/remove phases.
	require.NoError(t, a.finalizePendingKubeAPIEncryptionKey(ctx, clusterID))
	require.NoError(t, a.finalizePendingKubeAPIEncryptionKey(ctx, clusterID))
	name, err := a.requireKubeAPIEncryptionKeyName(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, "kube-api-encryption-key-8", name)
	stored, err = a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	require.NoError(t, err)
	require.NotContains(t, stored.Data, constants.EtcdEncryptionKeyFieldPendingName)
	require.NotContains(t, stored.Data, constants.EtcdEncryptionKeyFieldPending)
}

func TestLegacyFinalizationDiscoversPendingStorageName(t *testing.T) {
	ctx, clusterID := context.Background(), uuid.New()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "nke", Name: constants.EtcdEncryptionKeySecretName(clusterID)}, Data: map[string][]byte{
		constants.EtcdEncryptionKeyFieldActive: []byte("old-public-fixture-key"), constants.EtcdEncryptionKeyFieldPending: []byte("pending-public-fixture-key"),
	}}
	a := &Activities{cfg: config.Config{PodNamespace: "nke"}, k8sClient: fake.NewClientBuilder().WithObjects(secret).Build()}
	withIdentityTestNodes(t, a, clusterID, func(db.Node, string) (string, error) { return "kube-api-encryption-key-9", nil })
	require.NoError(t, a.finalizePendingKubeAPIEncryptionKey(ctx, clusterID))
	name, err := a.requireKubeAPIEncryptionKeyName(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, "kube-api-encryption-key-9", name)
}

func TestRotationRejectsUnsupportedIdentityBeforeStaging(t *testing.T) {
	ctx, clusterID := context.Background(), uuid.New()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "nke", Name: constants.EtcdEncryptionKeySecretName(clusterID)}, Data: map[string][]byte{constants.EtcdEncryptionKeyFieldActive: []byte("public-fixture-key")}}
	a := &Activities{cfg: config.Config{PodNamespace: "nke"}, k8sClient: fake.NewClientBuilder().WithObjects(secret).Build()}
	withIdentityTestNodes(t, a, clusterID, func(db.Node, string) (string, error) { return "", errors.New("unsupported identity RPC") })
	_, err := a.ensurePendingKubeAPIEncryptionRotationKey(ctx, clusterID)
	require.ErrorContains(t, err, "upgrade older node agents")
	stored, err := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	require.NoError(t, err)
	require.NotContains(t, stored.Data, constants.EtcdEncryptionKeyFieldPending)
}

func TestNewClusterEncryptionIdentityIsDurableWithoutLiveDiscovery(t *testing.T) {
	ctx, clusterID := context.Background(), uuid.New()
	a := &Activities{cfg: config.Config{PodNamespace: "nke"}, k8sClient: fake.NewClientBuilder().Build()}
	key, err := a.ensureKubeAPIEncryptionKey(ctx, clusterID)
	require.NoError(t, err)
	require.NotEmpty(t, key)
	name, err := a.requireKubeAPIEncryptionKeyName(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, constants.LegacyAPIEncryptionKeyName, name)
	retriedKey, err := a.ensureKubeAPIEncryptionKey(ctx, clusterID)
	require.NoError(t, err)
	require.True(t, key == retriedKey, "bootstrap retries must retain the same key") //nolint:testifylint // Do not print random key material on assertion failure.
}
