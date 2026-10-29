// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenantaccess

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

const testManagementNamespace = "tenant-access"

// TestPublishUsesConfiguredManagementNamespace proves the publish path follows
// config.ManagementNamespace rather than the legacy literal: the CAPI Cluster
// the publish gate reads, and the Secrets it writes, are all in the configured
// namespace, and nothing lands in nke-system.
func TestPublishUsesConfiguredManagementNamespace(t *testing.T) {
	ctx := context.Background()
	cluster := db.Cluster{ID: uuid.New(), Name: "configured"}

	capiCluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: testManagementNamespace,
			UID:       types.UID(cluster.ID.String()),
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(capiCluster).Build()

	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	c := newTestController(
		config.Config{
			SpiffeEndpointSocket: "unix:///run/spire/agent.sock",
			ManagementNamespace:  testManagementNamespace,
		},
		mockDB,
		k8sClient,
		nil,
	)

	require.NoError(t, c.publish(ctx, cluster, []byte(testAdminConf)))

	secret := &corev1.Secret{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Namespace: testManagementNamespace,
		Name:      KubeconfigSecretName(cluster.Name),
	}, secret))

	err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: constants.LegacyManagementNamespace,
		Name:      KubeconfigSecretName(cluster.Name),
	}, &corev1.Secret{})
	assert.Error(t, err, "publishing must not also write the legacy namespace")
}

// TestSecretsNamespaceFallsBackToLegacy covers the other half: a Config with no
// managementNamespace is a site whose nvke-controller-config predates the key,
// and it must keep addressing nke-system.
func TestSecretsNamespaceFallsBackToLegacy(t *testing.T) {
	c := newTestController(config.Config{}, &testutil.MockDB{}, nil, nil)
	assert.Equal(t, constants.LegacyManagementNamespace, c.secretsNamespace())
}
