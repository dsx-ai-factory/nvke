// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/agent"
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
		if digest == identityTestDigest("old-public-fixture-key") {
			return constants.LegacyAPIEncryptionKeyName, nil
		}
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
	require.NoError(t, a.finalizePendingKubeAPIEncryptionKey(ctx, clusterID, pendingKeyOwnedRotation))
	require.NoError(t, a.finalizePendingKubeAPIEncryptionKey(ctx, clusterID, pendingKeyOwnedRotation))
	name, err := a.requireKubeAPIEncryptionKeyName(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, "kube-api-encryption-key-8", name)
	stored, err = a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	require.NoError(t, err)
	require.NotContains(t, stored.Data, constants.EtcdEncryptionKeyFieldPendingName)
	require.NotContains(t, stored.Data, constants.EtcdEncryptionKeyFieldPending)
}

func TestRotationReusingPendingKeyRecordsActiveName(t *testing.T) {
	for _, test := range []struct {
		name             string
		storedActiveName string
		activeOn         map[string]bool
		wantActiveName   string
		wantError        string
	}{
		{name: "active name missing is recorded", wantActiveName: "kube-api-encryption-key-7"},
		{name: "matching active name is kept", storedActiveName: "kube-api-encryption-key-7", wantActiveName: "kube-api-encryption-key-7"},
		{name: "disagreeing active name is refused", storedActiveName: constants.LegacyAPIEncryptionKeyName, wantError: "active encryption identity disagrees"},
		{name: "active key absent on one node is recorded from the other", activeOn: map[string]bool{"cp-2": true}, wantActiveName: "kube-api-encryption-key-7"},
		{name: "active key absent on every node leaves active name empty", activeOn: map[string]bool{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, clusterID := context.Background(), uuid.New()
			data := map[string][]byte{
				constants.EtcdEncryptionKeyFieldActive:      []byte("old-public-fixture-key"),
				constants.EtcdEncryptionKeyFieldPending:     []byte("pending-public-fixture-key"),
				constants.EtcdEncryptionKeyFieldPendingName: []byte("kube-api-encryption-key-8"),
			}
			if test.storedActiveName != "" {
				data[constants.EtcdEncryptionKeyFieldActiveName] = []byte(test.storedActiveName)
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "nke", Name: constants.EtcdEncryptionKeySecretName(clusterID)}, Data: data}
			a := &Activities{cfg: config.Config{PodNamespace: "nke"}, k8sClient: fake.NewClientBuilder().WithObjects(secret).Build()}
			withIdentityTestNodes(t, a, clusterID, func(node db.Node, digest string) (string, error) {
				if digest == identityTestDigest("old-public-fixture-key") {
					if test.activeOn != nil && !test.activeOn[node.Name] {
						return "", nil
					}
					return "kube-api-encryption-key-7", nil
				}
				return "kube-api-encryption-key-8", nil
			})
			pending, err := a.ensurePendingKubeAPIEncryptionRotationKey(ctx, clusterID)
			stored, getErr := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
			require.NoError(t, getErr)
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
				require.Equal(t, test.storedActiveName, string(stored.Data[constants.EtcdEncryptionKeyFieldActiveName]))
				return
			}
			require.NoError(t, err)
			require.Equal(t, "pending-public-fixture-key", pending)
			require.Equal(t, test.wantActiveName, string(stored.Data[constants.EtcdEncryptionKeyFieldActiveName]))
			require.Equal(t, "kube-api-encryption-key-8", string(stored.Data[constants.EtcdEncryptionKeyFieldPendingName]))
		})
	}
}

func TestLegacyFinalizationDiscoversPendingStorageName(t *testing.T) {
	ctx, clusterID := context.Background(), uuid.New()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "nke", Name: constants.EtcdEncryptionKeySecretName(clusterID)}, Data: map[string][]byte{
		constants.EtcdEncryptionKeyFieldActive: []byte("old-public-fixture-key"), constants.EtcdEncryptionKeyFieldPending: []byte("pending-public-fixture-key"),
	}}
	a := &Activities{cfg: config.Config{PodNamespace: "nke"}, k8sClient: fake.NewClientBuilder().WithObjects(secret).Build()}
	withIdentityTestNodes(t, a, clusterID, func(db.Node, string) (string, error) { return "kube-api-encryption-key-9", nil })
	require.NoError(t, a.finalizePendingKubeAPIEncryptionKey(ctx, clusterID, pendingKeyOwnedRotation))
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

// TestEnablementFinalizesUnnamedPendingKeyWithoutIdentityDiscovery models an
// enablement that reuses a pending key staged before names were recorded, on
// agents admitted by the enablement minimum that predate the identity RPC.
func TestEnablementFinalizesUnnamedPendingKeyWithoutIdentityDiscovery(t *testing.T) {
	ctx, clusterID := context.Background(), uuid.New()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "nke", Name: constants.EtcdEncryptionKeySecretName(clusterID)}, Data: map[string][]byte{
		constants.EtcdEncryptionKeyFieldPending: []byte("pending-public-fixture-key"),
	}}
	a := &Activities{cfg: config.Config{PodNamespace: "nke"}, k8sClient: fake.NewClientBuilder().WithObjects(secret).Build()}
	identityCalls := 0
	withIdentityTestNodes(t, a, clusterID, func(node db.Node, _ string) (string, error) {
		identityCalls++
		return "", fmt.Errorf("node %s %w", node.Name, agent.ErrEncryptionIdentityUnsupported)
	})
	a.db.(*testutil.MockDB).On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID}, nil)
	require.NoError(t, a.EnsurePendingEtcdEncryptionKey(ctx, EtcdEncryptionClusterInput{ClusterID: clusterID}))
	require.NoError(t, a.FinalizePendingEtcdEncryptionKey(ctx, EtcdEncryptionClusterInput{ClusterID: clusterID}))
	require.Zero(t, identityCalls)
	stored, err := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, "pending-public-fixture-key", string(stored.Data[constants.EtcdEncryptionKeyFieldActive]))
	require.Equal(t, constants.LegacyAPIEncryptionKeyName, string(stored.Data[constants.EtcdEncryptionKeyFieldActiveName]))
	require.NotContains(t, stored.Data, constants.EtcdEncryptionKeyFieldPending)
}

// TestLegacyRotationFinalizeDefersStorageNameDiscovery covers rotation histories
// recorded before ownership, which finalize through the enablement activity
// after EtcdEncrypted is set. Finalization records no name; the next reader
// discovers the auto-derived name from upgraded agents.
func TestLegacyRotationFinalizeDefersStorageNameDiscovery(t *testing.T) {
	ctx, clusterID := context.Background(), uuid.New()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "nke", Name: constants.EtcdEncryptionKeySecretName(clusterID)}, Data: map[string][]byte{
		constants.EtcdEncryptionKeyFieldActive: []byte("old-public-fixture-key"), constants.EtcdEncryptionKeyFieldPending: []byte("pending-public-fixture-key"),
	}}
	a := &Activities{cfg: config.Config{PodNamespace: "nke"}, k8sClient: fake.NewClientBuilder().WithObjects(secret).Build()}
	identityCalls := 0
	withIdentityTestNodes(t, a, clusterID, func(db.Node, string) (string, error) {
		identityCalls++
		return "kube-api-encryption-key-9", nil
	})
	a.db.(*testutil.MockDB).On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, EtcdEncrypted: true}, nil)
	require.NoError(t, a.FinalizePendingEtcdEncryptionKey(ctx, EtcdEncryptionClusterInput{ClusterID: clusterID}))
	require.Zero(t, identityCalls)
	name, err := a.requireKubeAPIEncryptionKeyName(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, "kube-api-encryption-key-9", name)
}

// TestLegacyRotationFinalizesUnnamedPendingKeyWithoutIdentityDiscovery models a
// rotation started by an earlier controller that finalizes on agents without
// the identity RPC. It must record the original state: the pending bytes become
// active and no storage name is guessed; a stale active name is dropped.
func TestLegacyRotationFinalizesUnnamedPendingKeyWithoutIdentityDiscovery(t *testing.T) {
	ctx, clusterID := context.Background(), uuid.New()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "nke", Name: constants.EtcdEncryptionKeySecretName(clusterID)}, Data: map[string][]byte{
		constants.EtcdEncryptionKeyFieldActive:     []byte("old-public-fixture-key"),
		constants.EtcdEncryptionKeyFieldActiveName: []byte(constants.LegacyAPIEncryptionKeyName),
		constants.EtcdEncryptionKeyFieldPending:    []byte("pending-public-fixture-key"),
	}}
	a := &Activities{cfg: config.Config{PodNamespace: "nke"}, k8sClient: fake.NewClientBuilder().WithObjects(secret).Build()}
	identityCalls := 0
	withIdentityTestNodes(t, a, clusterID, func(node db.Node, _ string) (string, error) {
		identityCalls++
		return "", fmt.Errorf("node %s %w", node.Name, agent.ErrEncryptionIdentityUnsupported)
	})
	a.db.(*testutil.MockDB).On("GetCluster", mock.Anything, clusterID).Return(db.Cluster{ID: clusterID, EtcdEncrypted: true}, nil)
	require.NoError(t, a.FinalizePendingEtcdEncryptionKey(ctx, EtcdEncryptionClusterInput{ClusterID: clusterID}))
	require.NoError(t, a.FinalizePendingEtcdEncryptionKey(ctx, EtcdEncryptionClusterInput{ClusterID: clusterID}))
	require.Zero(t, identityCalls)
	stored, err := a.getKubeAPIEncryptionKeySecret(ctx, clusterID)
	require.NoError(t, err)
	require.Equal(t, "pending-public-fixture-key", string(stored.Data[constants.EtcdEncryptionKeyFieldActive]))
	require.NotContains(t, stored.Data, constants.EtcdEncryptionKeyFieldActiveName)
	require.NotContains(t, stored.Data, constants.EtcdEncryptionKeyFieldPending)
	require.NotContains(t, stored.Data, constants.EtcdEncryptionKeyFieldPendingName)
}
